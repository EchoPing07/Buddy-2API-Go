// Package taskops 成长任务动作编排：扫描待办 / 接受任务 / 执行行为链 / 回读进度 /
// 自动领奖 / 周期补领清扫。
//
// 为什么单独成包：本项目的 admin（HTTP 面板）与 scheduler（定时）都需要这套编排，
// 放在任何一侧都会造成反向依赖。本包只依赖 upstream 与 config，两边都能直接调用。
//
// 三类入口：
//
//	Scan      只读扫描：列出「未完成且可自动化」的待办（含小程序口径合并）
//	Auto/AutoAll  执行行为链 → 有界轮询回读（异步计分）→ 达标即领奖
//	Sweep     周期幂等补领：只 accept 未接受的、只 claim 已达标未领的，**不跑行为链**
//
// Sweep 的存在理由（参考 issue #174 的教训）：`Auto` 只覆盖「有自动动作」的任务，
// 不在动作白名单里的任务即使达标也永远没人领奖；「一键完成」对已达标任务又直接跳过
// 不领。少了这条链路，积分会永远卡在半路。
package taskops

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"buddy2api-go/internal/upstream"
)

// 节流参数（对齐上游实测节奏，防风控）。
//
// 包级变量的原因：这些是全局生效的风控节奏，不是某个 Runner 的属性。
// **并发约束**：仅由 ThrottleTuning 在程序启动/测试准备阶段改写，
// 运行期只读。测试不可 t.Parallel()（会与 ThrottleTuning 形成数据竞争）；
// 生产路径从不调用 ThrottleTuning。
var (
	// reportGap 连续行为上报间隔（实测 1.05s 口径）。
	reportGap = 1050 * time.Millisecond
	// acceptBatchGap 批量接受的批间节流。
	acceptBatchGap = 1050 * time.Millisecond
	// mpActionGap 小程序任务 accept/上报/领奖之间的间隔。
	mpActionGap = 2 * time.Second
	// mpChatEventGap 小程序对话事件间隔。上游对 Sequential_Tasks_3「5 次有效对话」有
	// 反作弊校验：数秒级连发的事件会先被计入进度、随后整体回滚（claim 返回
	// 400 task not completed）；45s 间隔逐条上报才稳定存活。
	mpChatEventGap = 45 * time.Second
	// expertSummonGap 专家召唤链间隔（真实使用节奏）。
	expertSummonGap = 6 * time.Second
	// acceptBatchSize 单次 accept 的 task_codes 上限（保守分片）。
	acceptBatchSize = 20
)

// Throttle 动作节流参数快照（当前生效值）。
type Throttle struct {
	Report      time.Duration // 行为上报/项间间隔
	AcceptBatch time.Duration // 批量接受批间间隔
	MPAction    time.Duration // 小程序任务各阶段间隔
	MPChatEvent time.Duration // 小程序对话事件间隔（反作弊要求数十秒）
}

// ThrottleTuning 调整节流参数，返回恢复函数。参数为 0 表示「保持原值」，
// 显式关闭需传 -1（测试常用）。
//
// 生产不需要调用（默认值即实测节奏）；存在意义是跨包测试不必真等 45 秒一条，
// 以及上游风控口径变化时可在不改代码的前提下调整。
func ThrottleTuning(report, acceptBatch, mpAction, mpChatEvent time.Duration) (restore func()) {
	prev := Throttle{Report: reportGap, AcceptBatch: acceptBatchGap,
		MPAction: mpActionGap, MPChatEvent: mpChatEventGap}
	set := func(dst *time.Duration, v time.Duration) {
		switch {
		case v == 0:
			// 保持原值
		case v < 0:
			*dst = 0
		default:
			*dst = v
		}
	}
	set(&reportGap, report)
	set(&acceptBatchGap, acceptBatch)
	set(&mpActionGap, mpAction)
	set(&mpChatEventGap, mpChatEvent)
	return func() {
		reportGap, acceptBatchGap = prev.Report, prev.AcceptBatch
		mpActionGap, mpChatEventGap = prev.MPAction, prev.MPChatEvent
	}
}

// CurrentThrottle 当前节流参数（测试与诊断用）。
func CurrentThrottle() Throttle {
	return Throttle{Report: reportGap, AcceptBatch: acceptBatchGap,
		MPAction: mpActionGap, MPChatEvent: mpChatEventGap}
}

// Runner 任务动作执行器。
//
// 本项目是单活动账号架构（多账号靠档案切换，见 internal/auth/accounts.go），
// 故互斥只需一把全局锁：同一时刻只允许一轮任务动作在跑（HTTP 手动触发 / 定时队列 /
// 补领清扫共用），避免同账号并发动作互相干扰（各动作幂等但含真实对话，重跑浪费配额）。
type Runner struct {
	c      *upstream.Client
	mu     sync.Mutex
	nowFn  func() time.Time
	logger *slog.Logger
	ctx    context.Context // 当前轮的取消信号（AutoContext / AutoAllContext 设置）
}

// NewRunner 创建执行器。
func NewRunner(c *upstream.Client) *Runner {
	return &Runner{c: c, nowFn: time.Now, logger: slog.Default()}
}

// SetNow 替换时钟（单测用）。
func (r *Runner) SetNow(fn func() time.Time) {
	if fn != nil {
		r.nowFn = fn
	}
}

// sleep 可取消的等待：ctx 取消时立即返回 false（面板断开 / 进程退出不必走完节流）。
func (r *Runner) sleep(d time.Duration) bool {
	if d <= 0 {
		return true
	}
	ctx := r.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// ErrBusy 已有一轮任务动作在执行。
var ErrBusy = errors.New("已有一轮任务动作在执行，请稍候")

// Gate 前置门控：region 必须为 cn（成长体系仅国内版），且必须已登录。
// 返回非空字符串表示不可执行（正常态，调用方据此给出人话原因，不发起任何上游调用）。
func (r *Runner) Gate(region string) string {
	t := r.c.TokenSummary()
	if t == nil || t.AccessToken == "" {
		return "not_logged_in"
	}
	if region != "cn" {
		return "region_global"
	}
	// 企业版账号没有个人成长体系：上游对 growth 域一律 403
	// "growth system is only available for personal users"，claim-gift 也是
	// 400「企业账号不支持该操作」。判定后不发起任何上游调用。
	if strings.TrimSpace(t.EnterpriseID) != "" {
		return "enterprise"
	}
	return ""
}

// GateMessage 把 Gate 的原因码翻成人话（前端直接展示）。
func GateMessage(reason string) string {
	switch reason {
	case "not_logged_in":
		return "尚未登录，请先在「账号」页扫码登录"
	case "region_global":
		return "成长任务仅国内版（cn）账号可用"
	case "enterprise":
		return "该账号为企业版，无个人成长体系（上游不支持）"
	case "busy":
		return ErrBusy.Error()
	}
	return reason
}

// ── 扫描 ─────────────────────────────────────────────────────────────────────

// Scan 扫描全部待办（只读）：未领取且未锁定、且有自动动作的任务。
// 达标记（target>0 && current>=target）同样入列——执行时会被识别为「已完成」并补领奖，
// 这是唯一能把历史达标奖励领掉的入口。
func (r *Runner) Scan() ([]upstream.Task, error) {
	tasks, err := r.c.ListTasksMerged()
	if err != nil {
		return nil, err
	}
	out := make([]upstream.Task, 0, len(tasks))
	for _, t := range tasks {
		if t.Claimed || t.Locked {
			continue
		}
		if ActionFor(t.TaskCode) == nil {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

// ── 自动完成 ─────────────────────────────────────────────────────────────────

// StepResult 单任务执行结果（前端表格直接渲染）。
type StepResult struct {
	TaskCode       string `json:"task_code"`
	Title          string `json:"title,omitempty"`
	Status         string `json:"status"` // done | skipped | error
	Message        string `json:"message,omitempty"`
	Attempt        bool   `json:"attempt,omitempty"` // 尝试型（判据未完全验证）
	ProgressBefore string `json:"progress_before,omitempty"`
	ProgressAfter  string `json:"progress_after,omitempty"`
	Claimable      bool   `json:"claimable,omitempty"`
	Claimed        bool   `json:"claimed,omitempty"`
	ClaimError     string `json:"claim_error,omitempty"`
	Credit         int64  `json:"credit,omitempty"`
	Energy         int64  `json:"energy,omitempty"`
}

// Auto 执行单个任务（无可取消语义，供 cron / 单测调用）。
func (r *Runner) Auto(code string) (StepResult, error) {
	return r.AutoContext(context.Background(), code)
}

// AutoContext 执行单个任务：accept 阶段 → 动作 → 有界轮询回读 → 达标即领奖。
// ctx 取消后不再发起新的上游调用，已发出的请求各自超时返回。
func (r *Runner) AutoContext(ctx context.Context, code string) (StepResult, error) {
	act := ActionFor(code)
	if act == nil {
		return StepResult{}, fmt.Errorf("任务 %s 没有可自动化的动作（需在官方客户端内交互）", code)
	}
	if !r.mu.TryLock() {
		return StepResult{}, ErrBusy
	}
	defer r.mu.Unlock()
	r.ctx = ctx
	defer func() { r.ctx = nil }()

	before, err := r.taskByCode(code, act.MP)
	if err != nil {
		return StepResult{}, err
	}
	if before == nil {
		return StepResult{TaskCode: code, Status: "skipped", Message: "当前账号没有下发此任务"}, nil
	}
	res := StepResult{TaskCode: code, Title: before.Title, Attempt: act.Attempt,
		ProgressBefore: before.Progress()}
	// 口径以任务自身为准（码白名单 ∪ 仅 mp 下发），动作表的 MP 只作兜底。
	mp := before.NeedsMP() || act.MP
	if before.Claimed {
		res.Status, res.Message = "skipped", "该任务奖励已领取"
		return res, nil
	}
	if before.Locked {
		// 上游未开放（Sequential 族每日零点解锁下一环）：跑动作只会白耗真实对话配额。
		res.Status, res.Message = "skipped", "任务尚未解锁（上游 locked），等解锁后自动补"
		return res, nil
	}
	if before.Done() {
		// 已达标未领：直接补领（不重跑行为链，省配额）
		res.Status, res.Message = "done", "进度已达标"
		r.tryClaim(code, mp, &res)
		return res, nil
	}
	r.acceptPending(code, mp)

	msg, runErr := act.Run(r)
	res.Message = msg
	if runErr != nil {
		res.Status = "error"
		res.Message = runErr.Error()
		return res, nil
	}
	after, _ := r.waitProgress(code, mp)
	if after != nil {
		res.ProgressAfter = after.Progress()
		res.Claimable = after.Claimable
	}
	if after != nil && after.Claimable {
		r.tryClaim(code, mp, &res)
	}
	res.Status = stepStatus(runErr)
	return res, nil
}

// AutoAll 依次执行全部有动作的任务（单项失败不阻断后续）。
func (r *Runner) AutoAll() ([]StepResult, error) {
	return r.AutoAllContext(context.Background())
}

// AutoAllContext 同 AutoAll，但可被 ctx 取消（关机 / 面板超时后放弃）。
// 先做一轮批量 accept（默认口径与小程序口径分开提交），让状态机规范。
func (r *Runner) AutoAllContext(ctx context.Context) ([]StepResult, error) {
	if !r.mu.TryLock() {
		return nil, ErrBusy
	}
	defer r.mu.Unlock()
	r.ctx = ctx
	defer func() { r.ctx = nil }()

	var out []StepResult
	if msg, err := r.acceptAllPending(); err != nil {
		out = append(out, StepResult{TaskCode: "(批量接受)", Status: "error", Message: err.Error()})
	} else if msg != "" {
		out = append(out, StepResult{TaskCode: "(批量接受)", Status: "done", Message: msg})
	}

	// 预先取一份任务快照：25 个动作各自 taskByCode 会打 25+ 次列表请求。
	// 快照取成功后，非 mp 任务「查不到」就是「本账号没有下发」，不必再逐个回查；
	// mp 任务仍允许回查（mp 列表可能拉取失败，合并结果里就没有它们）。
	snapshot, snapErr := r.c.ListTasksMerged()
	if snapErr != nil {
		snapshot = nil // 取不到就退回逐个查询（宁可多几次请求也别漏跑）
	}
	for _, act := range actions {
		if ctx.Err() != nil {
			out = append(out, StepResult{TaskCode: "(已取消)", Status: "skipped",
				Message: "执行被取消，剩余任务未执行"})
			return out, nil
		}
		res := r.runOneSnap(act, snapshot)
		out = append(out, res)
		r.logger.Info("taskops: 任务动作",
			"code", act.Code, "status", res.Status,
			"progress", res.ProgressBefore+"→"+res.ProgressAfter,
			"claimed", res.Claimed, "credit", res.Credit)
		// 跳过项没有任何上游动作，不必等节流（否则「全部已领取」也要白等 25×1.05s）。
		if res.Status != "skipped" && !r.sleep(reportGap) {
			out = append(out, StepResult{TaskCode: "(已取消)", Status: "skipped",
				Message: "执行被取消，剩余任务未执行"})
			return out, nil
		}
	}
	return out, nil
}

// runOneSnap 用预取快照执行单个动作。
//
// 快照拉取成功时，「查不到」即「本账号没有下发这个任务」，直接 skip 不再回查——
// 否则 25 个动作每个都要多打一次列表请求。mp 任务例外：mp 列表可能整份拉取失败
// （ListTasksMerged 会静默降级为仅默认列表），此时回查一次才能区分「没下发」与「没拉到」。
func (r *Runner) runOneSnap(act *Action, snapshot []upstream.Task) StepResult {
	if snapshot != nil {
		if t := upstream.FindTask(snapshot, act.Code); t != nil {
			return r.runOneWith(act, t)
		}
		if !act.MP && !upstream.IsMPTaskCode(act.Code) {
			return StepResult{TaskCode: act.Code, Attempt: act.Attempt,
				Status: "skipped", Message: "当前账号没有下发此任务"}
		}
	}
	return r.runOneLocked(act)
}

// runOneLocked 执行单个动作（调用方需持锁）。
func (r *Runner) runOneLocked(act *Action) StepResult {
	before, err := r.taskByCode(act.Code, act.MP)
	if err != nil {
		return StepResult{TaskCode: act.Code, Attempt: act.Attempt,
			Status: "error", Message: "查询失败: " + err.Error()}
	}
	if before == nil {
		return StepResult{TaskCode: act.Code, Attempt: act.Attempt,
			Status: "skipped", Message: "当前账号没有下发此任务"}
	}
	return r.runOneWith(act, before)
}

// runOneWith 在已知任务快照的前提下执行单个动作（调用方需持锁）。
func (r *Runner) runOneWith(act *Action, before *upstream.Task) StepResult {
	res := StepResult{TaskCode: act.Code, Attempt: act.Attempt}
	res.Title, res.ProgressBefore = before.Title, before.Progress()
	mp := before.NeedsMP() || act.MP
	if before.Claimed {
		res.Status, res.Message = "skipped", "该任务奖励已领取"
		return res
	}
	if before.Locked {
		// 与 Auto 同口径：locked 任务不跑动作链。此前这里漏了检查，AutoAll
		// 会对 Sequential 链条的锁定环节反复发上报与真实对话，白耗配额。
		res.Status, res.Message = "skipped", "任务尚未解锁（上游 locked），等解锁后自动补"
		return res
	}
	if before.Done() {
		res.Status, res.Message = "done", "进度已达标"
		r.tryClaim(act.Code, mp, &res)
		return res
	}
	msg, runErr := act.Run(r)
	res.Message = msg
	if runErr != nil {
		res.Status, res.Message = "error", runErr.Error()
		return res
	}
	after, _ := r.waitProgress(act.Code, mp)
	if after != nil {
		res.ProgressAfter, res.Claimable = after.Progress(), after.Claimable
	}
	if after != nil && after.Claimable {
		r.tryClaim(act.Code, mp, &res)
	}
	res.Status = stepStatus(runErr)
	return res
}

// stepStatus 由动作返回的 error 推出步骤状态。
//
// 动作契约（actions.go 的 Run）：返回非 nil error = 动作没完成；返回 nil = 已完成。
// 曾经的写法是内部失败时返回 (msg, nil)，于是这里无条件写 done，前端把失败弹成绿色
// 「已完成」。现在动作一律用 error 汇报失败（见 actions.go），这里如实映射。
func stepStatus(err error) string {
	if err != nil {
		return "error"
	}
	return "done"
}

// tryClaim 领奖并回填结果。领奖失败不掩盖主流程：status 仍为 done，只附 claim_error。
// mp 传 Task.NeedsMP()（码白名单 ∪ 仅 mp 下发），不能传 Task.MP。
func (r *Runner) tryClaim(code string, mp bool, res *StepResult) {
	credit, energy, err := r.c.ClaimRewardFor(code, mp)
	if err != nil {
		res.ClaimError = err.Error()
		if res.Message != "" {
			res.Message += "；达标但领奖失败，可稍后重试"
		}
		return
	}
	res.Claimed = true
	res.Credit, res.Energy = credit, energy
	if res.Message == "" {
		res.Message = "已完成"
	}
	if credit > 0 || energy > 0 {
		res.Message += fmt.Sprintf("；已自动领奖 +%d 分 +%d 能", credit, energy)
	} else {
		res.Message += "；奖励此前已领取"
	}
}

// taskByCode 拉取单个任务（mp 码自动走 mp 口径）。
func (r *Runner) taskByCode(code string, mp bool) (*upstream.Task, error) {
	if mp {
		tasks, err := r.c.ListTasksMP()
		if err != nil {
			return nil, err
		}
		return upstream.FindTask(tasks, code), nil
	}
	tasks, err := r.c.ListTasks()
	if err != nil {
		return nil, err
	}
	if t := upstream.FindTask(tasks, code); t != nil {
		return t, nil
	}
	// 默认列表查不到时回落 mp 列表（避免把「仅在 mp 下发」误判为「无此任务」）
	if mpTasks, mpErr := r.c.ListTasksMP(); mpErr == nil {
		return upstream.FindTask(mpTasks, code), nil
	}
	return nil, nil
}

// waitProgress 有界轮询回读（上游计分异步：上报后进度要数秒才刷新）。
func (r *Runner) waitProgress(code string, mp bool) (*upstream.Task, error) {
	return r.c.TaskByCodeWaiting(code, mp)
}

// acceptPending 接受单个任务（若尚未接受）。失败静默：accept 只是报名，行为事件才是判据。
func (r *Runner) acceptPending(code string, mp bool) {
	t, err := r.taskByCode(code, mp)
	if err != nil || t == nil {
		return
	}
	if t.Locked || t.Claimed || t.AcceptStatus == "accepted" || t.AcceptStatus == "completed" {
		return
	}
	if mp {
		_ = r.c.AcceptTasksMP([]string{code})
	} else {
		_ = r.c.AcceptTasks([]string{code})
	}
	r.sleep(reportGap)
}

// acceptAllPending 批量接受默认口径 + 小程序口径的未接受任务（分开提交，端标记不同）。
func (r *Runner) acceptAllPending() (string, error) {
	tasks, err := r.c.ListTasks()
	if err != nil {
		return "", err
	}
	var codes []string
	for _, t := range tasks {
		if t.Claimed || t.Locked || t.AcceptStatus == "accepted" || t.AcceptStatus == "completed" {
			continue
		}
		codes = append(codes, t.TaskCode)
	}
	accepted, failed := r.acceptInBatches(codes, false)

	// 小程序口径单独提交（混提交会让 mp 任务报 task not found）。
	// 这里只用「mp 列表里有、默认列表没有」来发现新任务，口径本身仍按 NeedsMP 判定：
	// 之前把默认列表的全部码建成 mpOnly 白名单，导致「两份列表都有」的 mp 任务
	// 永远不会走 mp 头 accept，状态机长期停在未接受。
	seenInDefault := make(map[string]bool, len(tasks))
	for _, t := range tasks {
		seenInDefault[t.TaskCode] = true
	}
	if mpTasks, mpErr := r.c.ListTasksMP(); mpErr == nil {
		var mpCodes []string
		for _, t := range mpTasks {
			if t.Claimed || t.Locked || !t.NeedsMP() ||
				t.AcceptStatus == "accepted" || t.AcceptStatus == "completed" {
				continue
			}
			// 默认列表已把它列为「已接受/已完成」的不重复提交
			if d := upstream.FindTask(tasks, t.TaskCode); d != nil &&
				(d.AcceptStatus == "accepted" || d.AcceptStatus == "completed") {
				continue
			}
			mpCodes = append(mpCodes, t.TaskCode)
		}
		_ = seenInDefault
		a, f := r.acceptInBatches(mpCodes, true)
		accepted += a
		failed += f
	}
	if accepted == 0 && failed == 0 {
		return "", nil
	}
	msg := fmt.Sprintf("已接受 %d 个任务", accepted)
	if failed > 0 {
		msg += fmt.Sprintf("（%d 个被上游拒绝）", failed)
	}
	return msg, nil
}

// acceptInBatches 分批接受，返回 (成功数, 失败数)。
func (r *Runner) acceptInBatches(codes []string, mp bool) (ok, failed int) {
	for i := 0; i < len(codes); i += acceptBatchSize {
		end := i + acceptBatchSize
		if end > len(codes) {
			end = len(codes)
		}
		batch := codes[i:end]
		var err error
		if mp {
			err = r.c.AcceptTasksMP(batch)
		} else {
			err = r.c.AcceptTasks(batch)
		}
		if err != nil {
			failed += len(batch)
			r.logger.Warn("taskops: 批量接受失败", "mp", mp, "codes", batch, "error", err)
			continue
		}
		ok += len(batch)
		if !r.sleep(acceptBatchGap) {
			return ok, failed
		}
	}
	return ok, failed
}

// ── 周期补领清扫 ─────────────────────────────────────────────────────────────

// SweepStats 一轮补领清扫的汇总。
type SweepStats struct {
	Accepted int    `json:"accepted"` // 本轮接受成功的任务数
	Claimed  int    `json:"claimed"`  // 本轮领奖成功的任务数
	Credits  int64  `json:"credits"`  // 本轮到账积分
	Energy   int64  `json:"energy"`   // 本轮到账能量
	Errors   int    `json:"errors"`   // 本轮出错（不中断）的次数
	Ran      bool   `json:"ran"`
	Reason   string `json:"reason,omitempty"`
	At       int64  `json:"at,omitempty"`
}

// Sweep 一轮幂等补领清扫：只 accept 未接受的、只 claim 已达标未领的。
//
// 刻意**不跑任何行为链**（不真实对话、不上报事件）——这是它敢分钟级周期运行的前提。
// 判据只用 progress 口径（Claimable = 未领 && target>0 && current>=target），
// 不用 accept_status == "completed"：后者在回读时会短暂转 completed 后整体回滚。
func (r *Runner) Sweep(region string) SweepStats {
	if reason := r.Gate(region); reason != "" {
		return SweepStats{Reason: reason}
	}
	if !r.mu.TryLock() {
		return SweepStats{Reason: "busy"}
	}
	defer r.mu.Unlock()

	st := SweepStats{Ran: true, At: r.nowFn().Unix()}
	tasks, err := r.c.ListTasksMerged()
	if err != nil {
		st.Errors++
		st.Reason = "list: " + err.Error()
		return st
	}

	// 阶段一：接受未接受的任务（默认 / mp 分开提交）。
	var codes, mpCodes []string
	for _, t := range tasks {
		if t.Claimed || t.Locked || t.AcceptStatus == "accepted" || t.AcceptStatus == "completed" {
			continue
		}
		if t.NeedsMP() {
			mpCodes = append(mpCodes, t.TaskCode)
		} else {
			codes = append(codes, t.TaskCode)
		}
	}
	a1, f1 := r.acceptInBatches(codes, false)
	a2, f2 := r.acceptInBatches(mpCodes, true)
	st.Accepted, st.Errors = a1+a2, f1+f2

	// 阶段二：领取已达标未领的奖励（claim 幂等，单项失败不中断）。
	for _, t := range tasks {
		if !t.Claimable {
			continue
		}
		credit, energy, err := r.c.ClaimRewardFor(t.TaskCode, t.NeedsMP())
		if err != nil {
			st.Errors++
			r.logger.Warn("taskops: 补领失败", "code", t.TaskCode, "error", err)
			continue
		}
		st.Claimed++
		st.Credits += credit
		st.Energy += energy
		r.logger.Info("taskops: 补领成功", "code", t.TaskCode, "credit", credit, "energy", energy)
	}
	if st.Claimed > 0 || st.Accepted > 0 || st.Errors > 0 {
		r.logger.Info("taskops: 补领清扫完成",
			"accepted", st.Accepted, "claimed", st.Claimed,
			"credit", st.Credits, "energy", st.Energy, "errors", st.Errors)
	}
	return st
}
