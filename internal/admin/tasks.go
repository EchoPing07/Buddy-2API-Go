// tasks.go 成长任务端点：任务列表 / 接受 / 领奖 / 一键完成 / 全账号扫描 / 执行队列 /
// 补领清扫 / 补偿状态（均支持 ?uid= 多账号）。
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
	"strings"
	"time"

	"buddy2api-go/internal/auth"
	"buddy2api-go/internal/config"
	"buddy2api-go/internal/pool"
	"buddy2api-go/internal/scheduler"
	"buddy2api-go/internal/taskops"
	"buddy2api-go/internal/upstream"
)

// 任务队列 / 清扫的最近一轮结果落库表与基础键名。
const (
	taskCacheTable   = scheduler.GrowthCacheTable
	taskKeyLastSweep = scheduler.TaskKeyLastSweep
	taskKeyLastQueue = scheduler.TaskKeyLastQueue
)

// maxAcceptCodes 单次手动 accept 的任务码上限。
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

func (h *Handler) recordSweep(st taskops.SweepStats) {
	uid := ""
	if t := h.toks.Get(); t != nil {
		uid = t.UID
	}
	h.recordSweepFor(uid, st)
}

func (h *Handler) recordSweepFor(uid string, st taskops.SweepStats) {
	raw, err := json.Marshal(st)
	if err != nil {
		return
	}
	if err := h.st.SetCache(taskCacheTable, scheduler.TaskLastSweepKey(uid), string(raw)); err != nil {
		slog.Warn("任务清扫结果落库失败", "uid", uid, "error", err)
	}
}

func (h *Handler) recordQueueFor(uid string, summary map[string]any) {
	raw, err := json.Marshal(summary)
	if err != nil {
		return
	}
	if err := h.st.SetCache(taskCacheTable, scheduler.TaskLastQueueKey(uid), string(raw)); err != nil {
		slog.Warn("任务队列结果落库失败", "uid", uid, "error", err)
	}
}

// runner 返回主账号任务执行器（在 New() 里创建一次，与 client 同源）。
func (h *Handler) runner() *taskops.Runner {
	if h.tasks == nil {
		return taskops.NewRunner(h.client)
	}
	return h.tasks
}

// runnerFor 返回指定账号的任务执行器：
//   - 主账号复用 h.runner()（与 scheduler 的夜猫子任务共享同一把锁）；
//   - 非主账号从 uidRunners 缓存取或新建 taskops.NewRunner(h.client.WithTokenStore(ts))，按 ts 指针保鲜。
func (h *Handler) runnerFor(uid string, ts *auth.TokenStore) *taskops.Runner {
	uid = strings.TrimSpace(uid)
	active := h.toks.Get()
	if uid == "" || (active != nil && active.UID == uid) || ts == nil {
		return h.runner()
	}
	h.uidRunnersMu.Lock()
	defer h.uidRunnersMu.Unlock()
	if h.uidRunners == nil {
		h.uidRunners = make(map[string]uidRunnerEntry)
	}
	if ent, ok := h.uidRunners[uid]; ok && ent.ts == ts && ent.runner != nil {
		return ent.runner
	}
	r := taskops.NewRunner(h.client.WithTokenStore(ts))
	h.uidRunners[uid] = uidRunnerEntry{ts: ts, runner: r}
	return r
}

// taskGateMessage 把门控 reason 映射为人话提示（兼容 taskops 与池状态新增的 credential_disabled）。
func taskGateMessage(reason string) string {
	if reason == "credential_disabled" {
		return "该账号凭证已失效，请重新登录后再试"
	}
	return taskops.GateMessage(reason)
}

// gateFail 输出门控失败响应（HTTP 200 + ok:false，让前端统一按 reason 展示人话）。
func gateFail(w http.ResponseWriter, reason string) {
	jsonWrite(w, http.StatusOK, map[string]any{
		"ok": false, "reason": reason, "detail": taskGateMessage(reason),
	})
}

// resolveTaskTarget 按 ?uid= 解析任务/补偿端点的目标账号、上游 client 与 runner。
//   - rawUID==""：主账号；
//   - rawUID!=""：校验属于号池（否则 httpStatus=404）、CN 匹配、非凭证失效及 runner.Gate。
func (h *Handler) resolveTaskTarget(rawUID string) (effectiveUID string, cli *upstream.Client, r *taskops.Runner, httpStatus int, gateReason string) {
	rawUID = strings.TrimSpace(rawUID)
	cfg := h.cfg.Get()
	cnDomain := config.EndpointOf("cn").Domain

	if rawUID == "" {
		if t := h.toks.Get(); t != nil {
			effectiveUID = t.UID
		}
		r = h.runner()
		return effectiveUID, h.client, r, 0, r.Gate(cfg.Region)
	}

	targetUID, cli, tok, found := h.resolveUIDClient(rawUID)
	if !found {
		return "", nil, nil, http.StatusNotFound, "账号不存在"
	}
	if cfg.Region != "cn" || !regionMatches(tok.Domain, cnDomain) {
		return targetUID, nil, nil, 0, "region_global"
	}
	if h.pool != nil {
		if st, ok := h.pool.AccountStateOf(targetUID); ok && st.AutoDisabled == pool.AutoCredential {
			return targetUID, nil, nil, 0, "credential_disabled"
		}
	}
	active := h.toks.Get()
	if active != nil && active.UID == targetUID {
		r = h.runner()
		return targetUID, h.client, r, 0, r.Gate(cfg.Region)
	}
	var ts *auth.TokenStore
	if h.pool != nil {
		ts, _ = h.pool.TokenStore(targetUID)
	}
	r = h.runnerFor(targetUID, ts)
	return targetUID, cli, r, 0, r.Gate(cfg.Region)
}

// ── 任务列表 / 接受 / 领奖 ────────────────────────────────────────────────────

// tasksList GET /admin/tasks — 全量任务（默认 + 小程序口径合并），含本地推算的可领状态。
func (h *Handler) tasksList(w http.ResponseWriter, r *http.Request) {
	rawUID := strings.TrimSpace(r.URL.Query().Get("uid"))
	effectiveUID, cli, _, status, reason := h.resolveTaskTarget(rawUID)
	if status != 0 {
		jsonErr(w, status, reason)
		return
	}
	if reason != "" {
		gateFail(w, reason)
		return
	}
	tasks, err := cli.ListTasksMerged()
	if err != nil {
		jsonErr(w, http.StatusBadGateway, "拉取任务列表失败: "+err.Error())
		return
	}
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
	lastSweep := h.cachedJSON(scheduler.TaskLastSweepKey(effectiveUID))
	if lastSweep == nil && rawUID == "" {
		lastSweep = h.cachedJSON(taskKeyLastSweep)
	}
	lastQueue := h.cachedJSON(scheduler.TaskLastQueueKey(effectiveUID))
	if lastQueue == nil && rawUID == "" {
		lastQueue = h.cachedJSON(taskKeyLastQueue)
	}
	jsonWrite(w, http.StatusOK, map[string]any{
		"ok": true, "tasks": views, "pending": pending,
		"at":         time.Now().Unix(),
		"last_sweep": lastSweep,
		"last_queue": lastQueue,
	})
}

// tasksAccept POST /admin/tasks/accept — 接受未接受的任务（{task_codes?:[...]}）。
func (h *Handler) tasksAccept(w http.ResponseWriter, r *http.Request) {
	_, cli, _, status, reason := h.resolveTaskTarget(r.URL.Query().Get("uid"))
	if status != 0 {
		jsonErr(w, status, reason)
		return
	}
	if reason != "" {
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
		if err := cli.AcceptTasks(plain); err != nil {
			firstErr = err
		}
		if err := cli.AcceptTasksMP(mpCodes); err != nil && firstErr == nil {
			firstErr = err
		}
		if firstErr != nil {
			jsonErr(w, http.StatusBadGateway, "接受任务失败: "+firstErr.Error())
			return
		}
		jsonWrite(w, http.StatusOK, map[string]any{"ok": true, "accepted": len(body.TaskCodes)})
		return
	}
	tasks, err := cli.ListTasksMerged()
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
		if err := cli.AcceptTasks(plain[i:end]); err != nil {
			failed = append(failed, plain[i:end]...)
			continue
		}
		accepted += end - i
	}
	for i := 0; i < len(mpCodes); i += 20 {
		end := min(i+20, len(mpCodes))
		if err := cli.AcceptTasksMP(mpCodes[i:end]); err != nil {
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
	_, cli, _, status, reason := h.resolveTaskTarget(r.URL.Query().Get("uid"))
	if status != 0 {
		jsonErr(w, status, reason)
		return
	}
	if reason != "" {
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
	credit, energy, err := cli.ClaimRewardFor(body.TaskCode, upstream.IsMPTaskCode(body.TaskCode))
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
	_, _, runner, status, reason := h.resolveTaskTarget(r.URL.Query().Get("uid"))
	if status != 0 {
		jsonErr(w, status, reason)
		return
	}
	if reason != "" {
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
	res, err := runner.AutoContext(r.Context(), body.TaskCode)
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
func (h *Handler) tasksAutoAll(w http.ResponseWriter, r *http.Request) {
	_, _, runner, status, reason := h.resolveTaskTarget(r.URL.Query().Get("uid"))
	if status != 0 {
		jsonErr(w, status, reason)
		return
	}
	if reason != "" {
		gateFail(w, reason)
		return
	}
	type outcome struct {
		results []taskops.StepResult
		err     error
	}
	ch := make(chan outcome, 1)
	go func() {
		results, err := runner.AutoAllContext(context.Background())
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
	_, _, runner, status, reason := h.resolveTaskTarget(r.URL.Query().Get("uid"))
	if status != 0 {
		jsonErr(w, status, reason)
		return
	}
	if reason != "" {
		gateFail(w, reason)
		return
	}
	pending, err := runner.Scan()
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
	effectiveUID, _, runner, status, reason := h.resolveTaskTarget(r.URL.Query().Get("uid"))
	if status != 0 {
		jsonErr(w, status, reason)
		return
	}
	if reason != "" {
		gateFail(w, reason)
		return
	}
	st := runner.Sweep(h.cfg.Get().Region)
	h.recordSweepFor(effectiveUID, st)
	jsonWrite(w, http.StatusOK, map[string]any{"ok": true, "stats": st})
}

// ── 补偿 ─────────────────────────────────────────────────────────────────────

// compensation GET /admin/compensation — 补偿活动状态（只读）。
func (h *Handler) compensation(w http.ResponseWriter, r *http.Request) {
	_, cli, _, status, reason := h.resolveTaskTarget(r.URL.Query().Get("uid"))
	if status != 0 {
		jsonErr(w, status, reason)
		return
	}
	if reason != "" {
		gateFail(w, reason)
		return
	}
	st, err := cli.GetCompensationStatus()
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
	_, cli, _, status, reason := h.resolveTaskTarget(r.URL.Query().Get("uid"))
	if status != 0 {
		jsonErr(w, status, reason)
		return
	}
	if reason != "" {
		gateFail(w, reason)
		return
	}
	st, err := cli.GetCompensationStatus()
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
	credit, err := cli.ClaimCompensation()
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

// tasksActions GET /admin/tasks/actions — 支持一键完成的任务清单（前端展示用，无上游调用）。
func (h *Handler) tasksActions(w http.ResponseWriter, r *http.Request) {
	jsonWrite(w, http.StatusOK, map[string]any{"ok": true, "actions": taskops.Actions()})
}

// ── 供 scheduler 挂载的回调 ───────────────────────────────────────────────────

// RunTaskSweep 执行主账号的一轮补领清扫（兼容旧单账号 hook）。
func (h *Handler) RunTaskSweep() taskops.SweepStats {
	uid := ""
	if t := h.toks.Get(); t != nil {
		uid = t.UID
	}
	return h.runTaskSweepWith(uid, h.runner())
}

// RunTaskSweepFor 按账号 UID 执行一轮补领清扫并写入 per-uid 缓存（供 scheduler 多账号遍历）。
func (h *Handler) RunTaskSweepFor(uid string) taskops.SweepStats {
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return h.RunTaskSweep()
	}
	effectiveUID, _, runner, status, reason := h.resolveTaskTarget(uid)
	if status != 0 {
		return taskops.SweepStats{Reason: "not_logged_in"}
	}
	if reason != "" {
		return taskops.SweepStats{Reason: reason}
	}
	return h.runTaskSweepWith(effectiveUID, runner)
}

func (h *Handler) runTaskSweepWith(uid string, runner *taskops.Runner) taskops.SweepStats {
	cfg := h.cfg.Get()
	if reason := runner.Gate(cfg.Region); reason != "" {
		return taskops.SweepStats{Reason: reason}
	}
	st := runner.Sweep(cfg.Region)
	h.recordSweepFor(uid, st)
	return st
}

// RunTaskQueueOnce 执行主账号的一轮任务队列（兼容旧单账号 hook）。
func (h *Handler) RunTaskQueueOnce() {
	uid := ""
	if t := h.toks.Get(); t != nil {
		uid = t.UID
	}
	h.runTaskQueueWith(uid, h.runner())
}

// RunTaskQueueFor 按账号 UID 执行一轮任务队列并写入 per-uid 缓存（供 scheduler 多账号遍历）。
func (h *Handler) RunTaskQueueFor(uid string) {
	uid = strings.TrimSpace(uid)
	if uid == "" {
		h.RunTaskQueueOnce()
		return
	}
	effectiveUID, _, runner, status, reason := h.resolveTaskTarget(uid)
	if status != 0 || reason != "" {
		return
	}
	h.runTaskQueueWith(effectiveUID, runner)
}

func (h *Handler) runTaskQueueWith(uid string, runner *taskops.Runner) {
	if reason := runner.Gate(h.cfg.Get().Region); reason != "" {
		return
	}
	pending, err := runner.Scan()
	if err != nil {
		slog.Warn("admin: 任务队列扫描失败", "uid", uid, "error", err)
		return
	}
	if len(pending) == 0 {
		slog.Info("admin: 任务队列无待办，跳过", "uid", uid)
		return
	}
	go func() {
		slog.Info("admin: 任务队列启动", "uid", uid, "count", len(pending))
		results, err := runner.AutoAllContext(context.Background())
		if err != nil {
			slog.Warn("admin: 任务队列执行失败", "uid", uid, "error", err)
			return
		}
		claimed, credit := 0, int64(0)
		for _, res := range results {
			if res.Claimed {
				claimed++
				credit += res.Credit
			}
		}
		h.recordQueueFor(uid, map[string]any{
			"at": time.Now().Unix(), "steps": len(results),
			"claimed": claimed, "credit": credit,
		})
		slog.Info("admin: 任务队列结束", "uid", uid, "steps", len(results), "claimed", claimed, "credit", credit)
	}()
}
