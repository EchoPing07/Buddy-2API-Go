// tasks.go growth 域「任务」接口：列表查询 / 接受 / 领取奖励（双端口径）。
//
// 端点（cn 域，见 growth.go 的 growthCall 传输层）：
//
//	GET  /v2/activity/growth/tasks               任务列表（默认口径）
//	GET  /v2/activity/growth/tasks               + X-Client-Platform: miniprogram → 小程序口径
//	POST /v2/activity/growth/tasks/accept        {"task_codes":[...]}（报名，不产生进度）
//	POST /activity/growth/tasks/<code>/claim     领奖（**Web 域** https://www.workbuddy.cn）
//
// 语义要点（实测口径）：
//   - accept 是「报名」，进度只由服务端行为事件点亮（chat_request_send / 桌面行为链 /
//     mp 埋点），因此 accept 可幂等重放。
//   - 领奖端点三套形态并存，必须按任务口径选对，否则恒 400 "task not completed"：
//     ① 默认任务 → Web 域 /activity/growth/tasks/<code>/claim（无 /v2，task_code 在路径）
//     ② 小程序任务 → chat 域 /activity/growth/tasks/<code>/claim + mp 头（400 时降级 ①）
//   - claim 幂等（重复领 already_claimed → 0 分 0 能），宁可多试一轮也不漏领。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// growth 域任务路径。
const (
	tasksListPath   = "/v2/activity/growth/tasks"
	tasksAcceptPath = "/v2/activity/growth/tasks/accept"
)

// mpPlatform 小程序口径端标记。小程序限定任务（Sequential_Tasks_* / school_season）
// 的列表下发、accept、claim 全链路要求该头；缺头 accept 返回 task not found。
const mpPlatform = "miniprogram"

// mpTaskCodes 小程序口径任务的**权威判定表**：与「这次是从哪份列表读到的」无关，
// 是任务自身的服务端属性（accept / claim 一律要求 mp 头）。
//
// 为什么必须按码判定而不能只看 Task.MP：mp 列表是默认列表的超集，合并去重时默认口径
// 优先，于是同时出现在两份列表里的 mp 任务会带着 MP=false 进入合并结果 —— 领奖走 Web 域
// 恒 400 "task not found"，且每轮清扫都会重试一次、永远领不到。
// 新任务出现时在此登记（taskops 的动作表 MP 字段须与此表一致，有测试守护）。
var mpTaskCodes = map[string]bool{
	"school_season":      true, // 校园日：mp chat + activityId
	"Sequential_Tasks_1": true, // 小程序首对话
	"Sequential_Tasks_2": true, // 小程序选专家对话
	"Sequential_Tasks_3": true, // 小程序 5 次对话
	"Sequential_Tasks_4": true,
	"Sequential_Tasks_5": true,
	"Sequential_Tasks_6": true,
	"Sequential_Tasks_7": true,
}

// IsMPTaskCode 任务码是否属于小程序口径（权威判定）。
func IsMPTaskCode(code string) bool { return mpTaskCodes[code] }

// Task 单个任务的对外视图（字段名与前端对齐，上游多余字段不透出）。
type Task struct {
	TaskCode     string `json:"task_code"`
	Title        string `json:"title,omitempty"`
	Description  string `json:"description,omitempty"` // 操作指引（含跳转说明）
	TaskDesc     string `json:"task_desc,omitempty"`   // 达成条件简述
	Credit       int64  `json:"credit,omitempty"`      // 奖励积分（上游 reward_credit）
	Energy       int64  `json:"energy,omitempty"`      // 奖励能量（上游 reward_energy）
	HasReward    bool   `json:"has_reward,omitempty"`
	RewardBuddy  bool   `json:"reward_buddy,omitempty"`
	TaskType     string `json:"task_type,omitempty"` // single（一次性）/ 累计型
	Tag          string `json:"tag,omitempty"`       // 端标记（PC 等）
	JumpURL      string `json:"jump_url,omitempty"`  // 客户端跳转协议（workbuddy://...）
	Locked       bool   `json:"locked,omitempty"`    // 上游标记未解锁
	Target       int64  `json:"target"`              // 目标次数（恒输出：0 是有效进度值）
	Current      int64  `json:"current"`             // 当前进度（恒输出：0 是有效进度值）
	AcceptStatus string `json:"accept_status,omitempty"`
	Status       string `json:"status,omitempty"`    // 上游任务状态（complete 等）
	Claimable    bool   `json:"claimable,omitempty"` // 进度达标且未领取（本地推算）
	Claimed      bool   `json:"claimed,omitempty"`   // 已领取（accept_status == claimed）
	MP           bool   `json:"mp,omitempty"`        // 是否「仅由 mp 列表下发」（观察值；口径判定请用 NeedsMP）
}

// NeedsMP 该任务的 accept / claim 是否必须走小程序口径（mp 头 + mp 领奖端点）。
//
// 两个信号取并集：任务码白名单（权威）+ 「仅 mp 列表下发」的观察值（兜住白名单尚未
// 登记的新任务）。领奖分派必须用它，不能用 Task.MP —— 见 mpTaskCodes 的说明。
func (t Task) NeedsMP() bool { return t.MP || IsMPTaskCode(t.TaskCode) }

// Done 报告任务是否已达标（含已领取）。
func (t Task) Done() bool { return t.Claimed || (t.Target > 0 && t.Current >= t.Target) }

// Progress 可读进度（"2/5" / "claimed"）。
func (t Task) Progress() string {
	if t.Target > 0 {
		return fmt.Sprintf("%d/%d", t.Current, t.Target)
	}
	if t.Claimed {
		return "claimed"
	}
	return t.AcceptStatus
}

// ListTasks 拉取默认口径任务列表。
// 响应形如 data.tasks[]，progress 可能是 {current,target} 对象或平铺字段，两者都尝试。
func (c *Client) ListTasks() ([]Task, error) {
	data, err := c.growthCall(http.MethodGet, tasksListPath, nil)
	if err != nil {
		return nil, err
	}
	return parseGrowthTasks(data, false)
}

// ListTasksMP 拉取小程序口径任务列表（X-Client-Platform: miniprogram）。
// 实测 mp 列表是默认口径的**超集**（含常规任务 + mp 专属任务），合并时按 task_code 去重。
func (c *Client) ListTasksMP() ([]Task, error) {
	data, err := c.growthCallMP(http.MethodGet, tasksListPath, nil)
	if err != nil {
		return nil, err
	}
	return parseGrowthTasks(data, true)
}

// ListTasksMerged 合并双口径任务列表（默认优先，mp 补齐；按 task_code 去重）。
// mp 列表失败静默（活动结束 / 无 mp 任务时零影响），默认列表失败才报错。
func (c *Client) ListTasksMerged() ([]Task, error) {
	tasks, err := c.ListTasks()
	if err != nil {
		return nil, err
	}
	mpTasks, mpErr := c.ListTasksMP()
	if mpErr != nil {
		return tasks, nil
	}
	seen := make(map[string]bool, len(tasks))
	for _, t := range tasks {
		seen[t.TaskCode] = true
	}
	for _, t := range mpTasks {
		if !seen[t.TaskCode] {
			tasks = append(tasks, t)
			seen[t.TaskCode] = true
		}
	}
	return tasks, nil
}

// AcceptTasks 接受任务（报名；幂等）。
func (c *Client) AcceptTasks(taskCodes []string) error {
	if len(taskCodes) == 0 {
		return nil
	}
	_, err := c.growthCall(http.MethodPost, tasksAcceptPath, map[string]any{"task_codes": taskCodes})
	return err
}

// AcceptTasksMP 接受小程序限定任务（必须带 mp 头）。
func (c *Client) AcceptTasksMP(taskCodes []string) error {
	if len(taskCodes) == 0 {
		return nil
	}
	_, err := c.growthCallMP(http.MethodPost, tasksAcceptPath, map[string]any{"task_codes": taskCodes})
	return err
}

// ClaimReward 领取单个任务奖励（Web 域口径：www.workbuddy.cn）。
// 返回本次到账的 (credit, energy)；已领过时上游 already_claimed → (0,0,nil)。
func (c *Client) ClaimReward(taskCode string) (credit, energy int64, err error) {
	data, err := c.doGrowthEnvelope(func() (*http.Request, error) {
		req, berr := c.webRequest(http.MethodPost,
			"/activity/growth/tasks/"+url.PathEscape(taskCode)+"/claim", nil)
		if berr != nil {
			return nil, berr
		}
		req.Header.Set("x-client-platform", "web")
		req.Header.Set("Referer", webBase(c)+"/profile/growth-center")
		return req, nil
	})
	if err != nil {
		return 0, 0, err
	}
	return parseClaimReward(data)
}

// ClaimRewardMP 领取小程序任务奖励：chat 域 + mp 头；chat 域 400 时降级 Web 域端点。
func (c *Client) ClaimRewardMP(taskCode string) (credit, energy int64, err error) {
	data, err := c.doGrowthEnvelope(func() (*http.Request, error) {
		req, berr := c.growthRequest(http.MethodPost,
			"/activity/growth/tasks/"+url.PathEscape(taskCode)+"/claim", nil)
		if berr != nil {
			return nil, berr
		}
		req.Header.Set("X-Client-Platform", mpPlatform)
		return req, nil
	})
	if err != nil {
		var ge *GrowthError
		if errors.As(err, &ge) && ge.Status == http.StatusBadRequest {
			return c.ClaimReward(taskCode) // chat 域对该路径 400（部分租户形态）→ Web 域降级
		}
		return 0, 0, err
	}
	return parseClaimReward(data)
}

// ClaimRewardFor 按任务是否 mp 口径分派领奖端点。
// mp 应为 Task.NeedsMP()（码白名单 ∪ 仅 mp 下发），不要直接传 Task.MP。
func (c *Client) ClaimRewardFor(taskCode string, mp bool) (int64, int64, error) {
	if mp {
		return c.ClaimRewardMP(taskCode)
	}
	return c.ClaimReward(taskCode)
}

// ClaimTask 领取指定任务奖励（自动判定口径，调用方无需关心 mp）。
func (c *Client) ClaimTask(t Task) (int64, int64, error) {
	return c.ClaimRewardFor(t.TaskCode, t.NeedsMP())
}

// parseClaimReward 解析领奖响应 data：{"already_claimed":bool,"credit":n,"energy":n}。
func parseClaimReward(data json.RawMessage) (credit, energy int64, err error) {
	var resp struct {
		AlreadyClaimed bool  `json:"already_claimed"`
		Credit         int64 `json:"credit"`
		Energy         int64 `json:"energy"`
	}
	if len(data) == 0 || string(data) == "null" {
		return 0, 0, nil
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, 0, err
	}
	if resp.AlreadyClaimed {
		return 0, 0, nil // 幂等：重复领取不算错误，但无新增奖励
	}
	return resp.Credit, resp.Energy, nil
}

// parseGrowthTasks 解析 growth 任务列表 data.tasks[]（默认与 mp 口径共用）。
func parseGrowthTasks(data json.RawMessage, mp bool) ([]Task, error) {
	var resp struct {
		Tasks []struct {
			TaskCode     string          `json:"task_code"`
			Title        string          `json:"title"`
			Description  string          `json:"description"`
			TaskDesc     string          `json:"task_desc"`
			RewardCredit int64           `json:"reward_credit"`
			RewardEnergy int64           `json:"reward_energy"`
			HasReward    bool            `json:"has_reward"`
			RewardBuddy  bool            `json:"reward_buddy"`
			TaskType     string          `json:"task_type"`
			Tag          string          `json:"tag"`
			JumpURL      string          `json:"jump_url"`
			Locked       bool            `json:"locked"`
			AcceptStatus string          `json:"accept_status"`
			Status       string          `json:"status"`
			Target       int64           `json:"target"`
			Current      int64           `json:"current"`
			Progress     json.RawMessage `json:"progress"`
		} `json:"tasks"`
	}
	if len(data) == 0 || string(data) == "null" {
		return nil, nil
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	out := make([]Task, 0, len(resp.Tasks))
	for _, t := range resp.Tasks {
		if strings.TrimSpace(t.TaskCode) == "" {
			continue
		}
		cur, tgt := t.Current, t.Target
		// progress 可能是 {current,target} 对象（实测口径），逐字段覆盖平铺值。
		//
		// 必须按「键是否存在」合并而不是「两个值一起整体覆盖」：服务端只想表达进度时
		// 会下发 {"current":3}（target 沿用平铺字段），整体覆盖会把 target 抹成 0，
		// 于是 Claimable 恒为 false、奖励永远领不到。同理 {"target":9} 不该把 current 抹掉。
		if len(t.Progress) > 0 && string(t.Progress) != "null" {
			var pr struct {
				Current *int64 `json:"current"`
				Target  *int64 `json:"target"`
			}
			if json.Unmarshal(t.Progress, &pr) == nil {
				if pr.Current != nil {
					cur = *pr.Current
				}
				if pr.Target != nil {
					tgt = *pr.Target
				}
			}
		}
		claimed := t.AcceptStatus == "claimed"
		out = append(out, Task{
			TaskCode: t.TaskCode, Title: t.Title, Description: t.Description, TaskDesc: t.TaskDesc,
			Credit: t.RewardCredit, Energy: t.RewardEnergy,
			HasReward: t.HasReward, RewardBuddy: t.RewardBuddy,
			TaskType: t.TaskType, Tag: t.Tag, JumpURL: t.JumpURL, Locked: t.Locked,
			Target: tgt, Current: cur,
			AcceptStatus: t.AcceptStatus, Status: t.Status,
			// Claimable 排除锁定任务：Sequential 链条常见「进度达标但上游未解锁」，
			// 对它 claim 只会 400 并每轮清扫重试一次。领奖判据见 Sweep / Auto。
			Claimable: !claimed && !t.Locked && tgt > 0 && cur >= tgt,
			Claimed:   claimed,
			MP:        mp,
		})
	}
	return out, nil
}

// FindTask 在列表中定位单个任务（未找到返回 nil）。
func FindTask(tasks []Task, code string) *Task {
	for i := range tasks {
		if tasks[i].TaskCode == code {
			return &tasks[i]
		}
	}
	return nil
}

// ClaimPollAttempts / claimPollGap 达标回读的有界轮询参数。
// 上游计分是**异步**的：行为事件上报后进度要数秒才刷新（实测 Model_chat 对话完成后
// 立即回读仍是 0/1，约 5~8 秒后才变 1/1）。一次性回读会误判「未达标」而跳过自动领奖。
var (
	claimPollAttempts = 6
	claimPollGap      = 2 * time.Second
)

// TaskPollTuning 调整达标回读的轮询节奏，返回恢复函数。
//
// **并发约束**：改写包级变量，只允许在启动/测试准备阶段调用；调用方测试不可
// t.Parallel()（会与其它用例的数据竞争）。生产路径从不调用。
//
// 生产环境不需要调用它（默认 6 次 × 2s ≈ 12 秒已覆盖实测的 5~8 秒计分延迟）；
// 存在的意义是让跨包端到端测试不必等满 12 秒，以及上游计分变慢时可临时加大预算。
func TaskPollTuning(gap time.Duration, attempts int) (restore func()) {
	prevGap, prevAttempts := claimPollGap, claimPollAttempts
	if gap > 0 {
		claimPollGap = gap
	}
	if attempts > 0 {
		claimPollAttempts = attempts
	}
	return func() { claimPollGap, claimPollAttempts = prevGap, prevAttempts }
}

// TaskByCodeWaiting 回读指定任务，若未达标则在有界预算内轮询等待（异步计分）。
// 已达标记（Claimable / Done）立即返回；预算耗尽返回最后一次结果（可能仍未达标）。
func (c *Client) TaskByCodeWaiting(code string, mp bool) (*Task, error) {
	return c.TaskByCodeWaitingContext(context.Background(), code, mp)
}

// TaskByCodeWaitingContext 同 TaskByCodeWaiting，但等待期间可被 ctx 取消
// （面板请求断开 / 进程退出时不必把 12 秒预算走完）。
func (c *Client) TaskByCodeWaitingContext(ctx context.Context, code string, mp bool) (*Task, error) {
	var last *Task
	for i := 0; i < claimPollAttempts; i++ {
		var tasks []Task
		var err error
		if mp {
			tasks, err = c.ListTasksMP()
		} else {
			tasks, err = c.ListTasks()
		}
		if err != nil {
			if last != nil {
				return last, nil // 已有一份读数：返回它，不让瞬时错误抹掉进度
			}
			return nil, err
		}
		last = FindTask(tasks, code)
		if last != nil && last.Done() {
			return last, nil
		}
		if i < claimPollAttempts-1 {
			select {
			case <-ctx.Done():
				return last, nil // 已有一份读数就交回它
			case <-time.After(claimPollGap):
			}
		}
	}
	return last, nil
}

// ── growth 域请求构造（供 tasks/desktop/mp 复用）────────────────────────────────

// growthRequest 构造指向 chat 域（growthBase）的请求（带 billingHeaders）。
// 每次调用都重新读取当前 token：这是 doAuthed 能在 401 后「重建请求带新 token」的前提。
// 调用方若需追加额外头（如 X-Client-Platform），改的是 build 出来的那个请求，不影响重试。
func (c *Client) growthRequest(method, path string, body any) (*http.Request, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, growthBase(c)+path, rdr)
	if err != nil {
		return nil, err
	}
	t := c.toks.Get()
	if t == nil {
		return nil, ErrNoToken
	}
	old := *t
	old.Domain = orDefault(t.Domain, c.ep().Domain)
	req.Header = billingHeaders(&old)
	return req, nil
}

// doGrowthEnvelope 发送并按 growth 信封解析（doAuthed 版本：token 过期 / 401 自动刷新重试）。
// build 每次调用都会重新读取 token，故 401 重试携带的是刷新后的新凭证。
func (c *Client) doGrowthEnvelope(build func() (*http.Request, error)) (json.RawMessage, error) {
	status, raw, err := c.doAuthed(c.short, build)
	if err != nil {
		return nil, err
	}
	return parseGrowthEnvelope(status, raw)
}

// parseGrowthEnvelope 把 (HTTP 状态, 原始体) 解析为 growth 信封语义。
func parseGrowthEnvelope(status int, raw []byte) (json.RawMessage, error) {
	var env envelope
	parseErr := json.Unmarshal(raw, &env)
	trunc := truncStr(string(raw), 300)
	if status < 200 || status >= 300 {
		return nil, &GrowthError{Status: status, Msg: env.Msg, Body: trunc}
	}
	if parseErr != nil {
		return nil, fmt.Errorf("growth 响应体非 JSON（HTTP %d）: %s", status, trunc)
	}
	if env.Code != 0 {
		return nil, &GrowthError{Status: status, Code: env.Code, Msg: env.Msg, Body: trunc}
	}
	if len(env.Data) == 0 {
		return json.RawMessage("null"), nil
	}
	return env.Data, nil
}

// growthCallMP 发 chat 域请求并叠加小程序端标记。
func (c *Client) growthCallMP(method, path string, body any) (json.RawMessage, error) {
	return c.doGrowthEnvelope(func() (*http.Request, error) {
		req, err := c.growthRequest(method, path, body)
		if err != nil {
			return nil, err
		}
		req.Header.Set("X-Client-Platform", mpPlatform)
		return req, nil
	})
}
