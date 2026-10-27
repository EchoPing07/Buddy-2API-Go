// tasks.go 成长任务端点：任务列表 / 接受 / 领奖 / 一键完成 / 全账号扫描 / 执行队列 /
// 补领清扫 / 补偿状态。
//
// 编排逻辑在 internal/taskops（admin 与 scheduler 共用），本文件只做 HTTP 适配：
// 门控 → 调用 → 结果透出。所有写操作失败都返回人话原因，不透上游长响应。
package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"buddy2api-go/internal/taskops"
	"buddy2api-go/internal/upstream"
)

// 任务队列 / 清扫的最近一轮结果落库键（账号口径，换账号由 ResetAccountState 一并清理）。
const (
	taskCacheTable   = "growth_cache"
	taskKeyLastSweep = "task_last_sweep"
	taskKeyLastQueue = "task_last_queue"
)

// maxAcceptCodes 单次手动 accept 的任务码上限。
// 缺省分支只提交列表里真实存在的任务（自然有界）；显式分支是用户/脚本直接给的，
// 没有上限时一个请求可以塞几千个码，各触发一次上游 accept 并长期占住全局锁。
const maxAcceptCodes = 200

// cachedJSON 读落库的 JSON 片段（缺失 / 过期 / 损坏都返回 nil，前端按空态处理）。
func (h *Handler) cachedJSON(key string) json.RawMessage {
	raw, _, ok := h.st.GetCache(taskCacheTable, key, 7*24*3600)
	if !ok || raw == "" {
		return nil
	}
	if !json.Valid([]byte(raw)) {
		return nil
	}
	return json.RawMessage(raw)
}

// recordSweep 把一轮清扫结果落库（面板可展示「上次清扫在几点、扫到什么」）。
// 落库失败只记日志：统计是辅助信息，不能影响清扫本身。
func (h *Handler) recordSweep(st taskops.SweepStats) {
	raw, err := json.Marshal(st)
	if err != nil {
		return
	}
	if err := h.st.SetCache(taskCacheTable, taskKeyLastSweep, string(raw)); err != nil {
		slog.Warn("任务清扫结果落库失败", "error", err)
	}
}

// recordQueue 把一轮队列结果落库。
func (h *Handler) recordQueue(summary map[string]any) {
	raw, err := json.Marshal(summary)
	if err != nil {
		return
	}
	if err := h.st.SetCache(taskCacheTable, taskKeyLastQueue, string(raw)); err != nil {
		slog.Warn("任务队列结果落库失败", "error", err)
	}
}

// runner 返回任务执行器（在 New() 里创建一次，与 client 同源）。
// 刻意不做惰性初始化：handler 会被多个 HTTP goroutine 并发调用，惰性赋值是数据竞争
// （即使只是读多写少，第一次并发请求就可能两个 goroutine 同时写）。
func (h *Handler) runner() *taskops.Runner {
	if h.tasks == nil {
		return taskops.NewRunner(h.client) // 仅测试里直接构造 Handler 才会走到
	}
	return h.tasks
}

// taskGate 任务侧前置门控：返回非空 reason 表示不可执行（不发起任何上游调用）。
func (h *Handler) taskGate() string {
	return h.runner().Gate(h.cfg.Get().Region)
}

// gateFail 输出门控失败响应（HTTP 200 + ok:false，让前端统一按 reason 展示人话）。
func gateFail(w http.ResponseWriter, reason string) {
	jsonWrite(w, http.StatusOK, map[string]any{
		"ok": false, "reason": reason, "detail": taskops.GateMessage(reason),
	})
}

// ── 任务列表 / 接受 / 领奖 ────────────────────────────────────────────────────

// tasksList GET /admin/tasks — 全量任务（默认 + 小程序口径合并），含本地推算的可领状态。
func (h *Handler) tasksList(w http.ResponseWriter, r *http.Request) {
	if reason := h.taskGate(); reason != "" {
		gateFail(w, reason)
		return
	}
	tasks, err := h.client.ListTasksMerged()
	if err != nil {
		jsonErr(w, http.StatusBadGateway, "拉取任务列表失败: "+err.Error())
		return
	}
	// 附加「是否支持一键完成」，前端据此决定按钮可用性。
	type taskView struct {
		upstream.Task
		Auto bool `json:"auto"`
	}
	views := make([]taskView, 0, len(tasks))
	pending := 0
	for _, t := range tasks {
		auto := taskops.ActionFor(t.TaskCode) != nil
		if auto && !t.Claimed && !t.Locked {
			pending++
		}
		views = append(views, taskView{Task: t, Auto: auto})
	}
	jsonWrite(w, http.StatusOK, map[string]any{
		"ok": true, "tasks": views, "pending": pending,
		"at": time.Now().Unix(),
		// 上一轮清扫 / 队列的落库结果：面板展示「上次扫了什么」，
		// 也让「一键完成超时后台继续」的结果在刷新后有处可查。
		"last_sweep": h.cachedJSON(taskKeyLastSweep),
		"last_queue": h.cachedJSON(taskKeyLastQueue),
	})
}

// tasksAccept POST /admin/tasks/accept — 接受未接受的任务（{task_codes?:[...]}）。
// 缺省 = 全部未接受的任务（默认与小程序口径分开提交）。
func (h *Handler) tasksAccept(w http.ResponseWriter, r *http.Request) {
	if reason := h.taskGate(); reason != "" {
		gateFail(w, reason)
		return
	}
	var body struct {
		TaskCodes []string `json:"task_codes"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body) // 空体 = 全部
	if len(body.TaskCodes) > 0 {
		if len(body.TaskCodes) > maxAcceptCodes {
			jsonErr(w, http.StatusBadRequest,
				fmt.Sprintf("task_codes 过多（上限 %d）", maxAcceptCodes))
			return
		}
		var plain, mpCodes []string
		for _, c := range body.TaskCodes {
			if upstream.IsMPTaskCode(c) {
				mpCodes = append(mpCodes, c)
			} else {
				plain = append(plain, c)
			}
		}
		var firstErr error
		if err := h.client.AcceptTasks(plain); err != nil {
			firstErr = err
		}
		if err := h.client.AcceptTasksMP(mpCodes); err != nil && firstErr == nil {
			firstErr = err
		}
		if firstErr != nil {
			jsonErr(w, http.StatusBadGateway, "接受任务失败: "+firstErr.Error())
			return
		}
		jsonWrite(w, http.StatusOK, map[string]any{"ok": true, "accepted": len(body.TaskCodes)})
		return
	}
	tasks, err := h.client.ListTasksMerged()
	if err != nil {
		jsonErr(w, http.StatusBadGateway, "拉取任务列表失败: "+err.Error())
		return
	}
	var plain, mpCodes []string
	for _, t := range tasks {
		if t.Claimed || t.Locked || t.AcceptStatus == "accepted" || t.AcceptStatus == "completed" {
			continue
		}
		if t.NeedsMP() {
			mpCodes = append(mpCodes, t.TaskCode)
		} else {
			plain = append(plain, t.TaskCode)
		}
	}
	accepted := 0
	var failed []string
	for i := 0; i < len(plain); i += 20 {
		end := min(i+20, len(plain))
		if err := h.client.AcceptTasks(plain[i:end]); err != nil {
			failed = append(failed, plain[i:end]...)
			continue
		}
		accepted += end - i
	}
	for i := 0; i < len(mpCodes); i += 20 {
		end := min(i+20, len(mpCodes))
		if err := h.client.AcceptTasksMP(mpCodes[i:end]); err != nil {
			failed = append(failed, mpCodes[i:end]...)
			continue
		}
		accepted += end - i
	}
	slog.Info("admin: 批量接受任务", "accepted", accepted, "failed", len(failed))
	jsonWrite(w, http.StatusOK, map[string]any{"ok": true, "accepted": accepted, "failed": failed})
}

// tasksClaim POST /admin/tasks/claim — 领取单个任务奖励 {task_code}（幂等）。
func (h *Handler) tasksClaim(w http.ResponseWriter, r *http.Request) {
	if reason := h.taskGate(); reason != "" {
		gateFail(w, reason)
		return
	}
	var body struct {
		TaskCode string `json:"task_code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.TaskCode == "" {
		jsonErr(w, http.StatusBadRequest, "task_code 不能为空")
		return
	}
	// 口径按任务码白名单判定（upstream.mpTaskCodes）：动作表只覆盖可自动化任务，
	// 用它推断会让「没有动作的小程序任务」被当成默认口径领奖，恒 400。
	credit, energy, err := h.client.ClaimRewardFor(body.TaskCode, upstream.IsMPTaskCode(body.TaskCode))
	if err != nil {
		jsonErr(w, http.StatusBadGateway, "领取失败: "+err.Error())
		return
	}
	if credit == 0 && energy == 0 {
		jsonWrite(w, http.StatusOK, map[string]any{
			"ok": true, "already_claimed": true, "detail": "该奖励此前已领取",
		})
		return
	}
	slog.Info("admin: 领取任务奖励", "code", body.TaskCode, "credit", credit, "energy", energy)
	jsonWrite(w, http.StatusOK, map[string]any{
		"ok": true, "credit": credit, "energy": energy,
		"detail": "已领取",
	})
}

// ── 一键完成 ─────────────────────────────────────────────────────────────────

// tasksAuto POST /admin/tasks/auto — 一键完成单个任务 {task_code}。
func (h *Handler) tasksAuto(w http.ResponseWriter, r *http.Request) {
	if reason := h.taskGate(); reason != "" {
		gateFail(w, reason)
		return
	}
	var body struct {
		TaskCode string `json:"task_code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.TaskCode == "" {
		jsonErr(w, http.StatusBadRequest, "task_code 不能为空")
		return
	}
	if taskops.ActionFor(body.TaskCode) == nil {
		jsonErr(w, http.StatusNotImplemented,
			"该任务需要客户端内交互（无对应接口），无法自动完成；请按任务说明在官方客户端操作")
		return
	}
	res, err := h.runner().AutoContext(r.Context(), body.TaskCode)
	if err != nil {
		if err == taskops.ErrBusy {
			gateFail(w, "busy")
			return
		}
		jsonErr(w, http.StatusBadGateway, "执行失败: "+err.Error())
		return
	}
	slog.Info("admin: 单任务一键完成", "code", body.TaskCode, "status", res.Status,
		"progress", res.ProgressBefore+"→"+res.ProgressAfter, "claimed", res.Claimed)
	jsonWrite(w, http.StatusOK, map[string]any{"ok": true, "result": res})
}

// tasksAutoAll POST /admin/tasks/auto_all — 一键完成全部可自动化任务。
// 动作多项串联（个别含真实对话），HTTP 侧给 5 分钟兜底：超时后台继续，
// 期间重复点击由 runner 的互斥挡成 busy。
func (h *Handler) tasksAutoAll(w http.ResponseWriter, r *http.Request) {
	if reason := h.taskGate(); reason != "" {
		gateFail(w, reason)
		return
	}
	type outcome struct {
		results []taskops.StepResult
		err     error
	}
	// 刻意为后台执行用独立的 context.Background()（而非 r.Context()）：
	// 一键完成会真实领奖，用户关掉页面不该让半途的任务链中断。
	// 进程退出由 scheduler.Stop 之外的自然超时兜底；单任务粒度带 ctx 的是 /tasks/auto。
	ch := make(chan outcome, 1)
	go func() {
		results, err := h.runner().AutoAllContext(context.Background())
		ch <- outcome{results: results, err: err}
	}()
	select {
	case out := <-ch:
		if out.err != nil {
			if out.err == taskops.ErrBusy {
				gateFail(w, "busy")
				return
			}
			jsonErr(w, http.StatusBadGateway, out.err.Error())
			return
		}
		claimed, credit, energy := 0, int64(0), int64(0)
		for _, res := range out.results {
			if res.Claimed {
				claimed++
				credit += res.Credit
				energy += res.Energy
			}
		}
		slog.Info("admin: 一键完成全部任务", "steps", len(out.results),
			"claimed", claimed, "credit", credit, "energy", energy)
		jsonWrite(w, http.StatusOK, map[string]any{
			"ok": true, "results": out.results, "claimed": claimed,
			"credit": credit, "energy": energy,
		})
	case <-time.After(5 * time.Minute):
		jsonErr(w, http.StatusGatewayTimeout, "执行超时（任务仍在后台继续，可稍后刷新查看进度）")
	}
}

// ── 扫描 / 队列 / 清扫 ───────────────────────────────────────────────────────

// tasksScan GET /admin/tasks/scan — 只读扫描：未完成且可自动化的待办。
func (h *Handler) tasksScan(w http.ResponseWriter, r *http.Request) {
	if reason := h.taskGate(); reason != "" {
		gateFail(w, reason)
		return
	}
	pending, err := h.runner().Scan()
	if err != nil {
		jsonErr(w, http.StatusBadGateway, "扫描失败: "+err.Error())
		return
	}
	jsonWrite(w, http.StatusOK, map[string]any{
		"ok": true, "pending": pending, "count": len(pending), "at": time.Now().Unix(),
	})
}

// tasksSweep POST /admin/tasks/sweep — 立即执行一轮补领清扫（只 accept/claim）。
func (h *Handler) tasksSweep(w http.ResponseWriter, r *http.Request) {
	if reason := h.taskGate(); reason != "" {
		gateFail(w, reason)
		return
	}
	st := h.runner().Sweep(h.cfg.Get().Region)
	h.recordSweep(st)
	jsonWrite(w, http.StatusOK, map[string]any{"ok": true, "stats": st})
}

// ── 补偿 ─────────────────────────────────────────────────────────────────────

// compensation GET /admin/compensation — 补偿活动状态（只读）。
// active=false 是「当前无补偿活动」的正常返回，不是错误。
func (h *Handler) compensation(w http.ResponseWriter, r *http.Request) {
	if reason := h.taskGate(); reason != "" {
		gateFail(w, reason)
		return
	}
	st, err := h.client.GetCompensationStatus()
	if err != nil {
		jsonErr(w, http.StatusBadGateway, "查询补偿状态失败: "+err.Error())
		return
	}
	jsonWrite(w, http.StatusOK, map[string]any{
		"ok": true, "status": st, "pending": st.Pending(),
	})
}

// compensationClaim POST /admin/compensation/claim — 领取补偿（先查后领，幂等）。
func (h *Handler) compensationClaim(w http.ResponseWriter, r *http.Request) {
	if reason := h.taskGate(); reason != "" {
		gateFail(w, reason)
		return
	}
	st, err := h.client.GetCompensationStatus()
	if err != nil {
		jsonErr(w, http.StatusBadGateway, "查询补偿状态失败: "+err.Error())
		return
	}
	if !st.Pending() {
		detail := "当前无待领补偿"
		switch {
		case !st.Active:
			detail = "当前没有生效中的补偿活动"
		case st.Claimed:
			detail = "本次补偿已领取"
		case st.CreditNum <= 0:
			detail = "补偿活动存在但额度为 0"
		}
		jsonWrite(w, http.StatusOK, map[string]any{"ok": true, "claimed": false, "detail": detail, "status": st})
		return
	}
	credit, err := h.client.ClaimCompensation()
	if err != nil {
		jsonErr(w, http.StatusBadGateway, "领取补偿失败: "+err.Error())
		return
	}
	slog.Info("admin: 领取补偿", "credit", credit, "end_time", st.EndTime)
	jsonWrite(w, http.StatusOK, map[string]any{
		"ok": true, "claimed": true, "credit": credit,
		"detail": "已领取补偿 +" + itoa64(credit) + " 积分",
	})
}

// min Go 1.21+ 已内建 min（int 版），此处不重复定义。

// tasksActions GET /admin/tasks/actions — 支持一键完成的任务清单（前端展示用，无上游调用）。
func (h *Handler) tasksActions(w http.ResponseWriter, r *http.Request) {
	jsonWrite(w, http.StatusOK, map[string]any{"ok": true, "actions": taskops.Actions()})
}

// ── 供 scheduler 挂载的回调 ───────────────────────────────────────────────────

// RunTaskSweep 执行一轮补领清扫（scheduler 的分钟级 ticker 调用）。
func (h *Handler) RunTaskSweep() taskops.SweepStats {
	if reason := h.taskGate(); reason != "" {
		return taskops.SweepStats{Reason: reason}
	}
	st := h.runner().Sweep(h.cfg.Get().Region)
	h.recordSweep(st)
	return st
}

// RunTaskQueueOnce 任务队列一轮：扫描待办 → 逐个执行动作（scheduler 每日 cron 调用）。
// 异步执行（队列本身可能耗时数分钟），已在跑时安全跳过。
func (h *Handler) RunTaskQueueOnce() {
	if reason := h.taskGate(); reason != "" {
		return
	}
	runner := h.runner()
	pending, err := runner.Scan()
	if err != nil {
		slog.Warn("admin: 任务队列扫描失败", "error", err)
		return
	}
	if len(pending) == 0 {
		slog.Info("admin: 任务队列无待办，跳过")
		return
	}
	go func() {
		slog.Info("admin: 任务队列启动", "count", len(pending))
		results, err := runner.AutoAllContext(context.Background())
		if err != nil {
			slog.Warn("admin: 任务队列执行失败", "error", err)
			return
		}
		claimed, credit := 0, int64(0)
		for _, res := range results {
			if res.Claimed {
				claimed++
				credit += res.Credit
			}
		}
		h.recordQueue(map[string]any{
			"at": time.Now().Unix(), "steps": len(results),
			"claimed": claimed, "credit": credit,
		})
		slog.Info("admin: 任务队列结束", "steps", len(results), "claimed", claimed, "credit", credit)
	}()
}
