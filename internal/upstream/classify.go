package upstream

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// ChatErrKind 上游 chat 错误的分类结果。
//
// 分类的唯一目的是决定「惩罚谁、罚多久、要不要在网关内换号重试」。
// 判据一律来自实测的错误体（见 docs/POOL-LOADBALANCE.md §3），不猜：
//   - 6004 是顶层 {code,msg}，文案自带「将在 … UTC+8 重置」→ 模型级限额；
//   - 14018 是嵌套 {error:{data:{code,msg}}}，无重置时间 → 账号级额度耗尽；
//   - 11128 是渠道指纹问题，换号也一样 400 → 不能罚号；
//   - 5xx / transport 是网络抖动（实测库里 73 条 503 全是 DNS / http2 超时）→ 不能罚号。
type ChatErrKind int

const (
	// ErrKindNone 无错误（HTTP 200 且响应体里也没有错误帧）。
	ErrKindNone ChatErrKind = iota
	// ErrKindModelRate 模型级限流（6004 / 裸 429）→ 只冷却 (账号, 模型)，账号本身仍可用。
	ErrKindModelRate
	// ErrKindQuotaExhausted 额度耗尽（14018 / 402）→ 账号级自动禁用，等探测恢复。
	ErrKindQuotaExhausted
	// ErrKindCredential 凭证失效（401 且刷不动）→ 账号级自动禁用，等重新登录/探测恢复。
	ErrKindCredential
	// ErrKindModelBlocked 该后端无此模型（11102）。本版不惩罚（池内模型的权限差异意义不大）。
	ErrKindModelBlocked
	// ErrKindChannel 渠道未获批（11128）→ 渠道/指纹问题，换号无用，不惩罚。
	ErrKindChannel
	// ErrKindBadParams 其它 4xx → 请求本身的问题，不惩罚。
	ErrKindBadParams
	// ErrKindServer 5xx → 网络/上游抖动，不惩罚。
	ErrKindServer
	// ErrKindTransport 请求未拿到任何 HTTP 响应（DNS / 连接 / 超时 / http2 中断）。
	ErrKindTransport
	// ErrKindPassThrough 其它情况 → 原样透传。
	ErrKindPassThrough
)

func (k ChatErrKind) String() string {
	switch k {
	case ErrKindNone:
		return "none"
	case ErrKindModelRate:
		return "model_rate"
	case ErrKindQuotaExhausted:
		return "quota_exhausted"
	case ErrKindCredential:
		return "credential"
	case ErrKindModelBlocked:
		return "model_blocked"
	case ErrKindChannel:
		return "channel"
	case ErrKindBadParams:
		return "bad_params"
	case ErrKindServer:
		return "server"
	case ErrKindTransport:
		return "transport"
	default:
		return "pass_through"
	}
}

// Retryable 该错误是否允许在网关内换号重试。
//
// 只对「上游明确拒绝且未开工」的类别为 true：429（限流/额度）与 401（凭证）都在上游
// 处理请求之前就打回，重试不会产生双份计费。5xx 不给重试是刻意的：上游可能已经计费，
// 重试有重复扣费风险，交给调用方按自己的退避策略决定（pi 对 429/5xx 都会重试）。
func (k ChatErrKind) Retryable() bool {
	switch k {
	case ErrKindModelRate, ErrKindQuotaExhausted, ErrKindCredential:
		return true
	}
	return false
}

// PunishesAccount 该错误是否应记在账号头上（决定要不要落冷却/禁用）。
// 模型级只罚模型，账号级罚账号，其余一律不罚。
func (k ChatErrKind) Punishes() bool {
	switch k {
	case ErrKindModelRate, ErrKindQuotaExhausted, ErrKindCredential:
		return true
	}
	return false
}

// 上游业务错误码（实测出现的全部取值）。
const (
	CodeModelRate      = 6004  // 模型级频率限制
	CodeQuotaExhausted = 14018 // 额度已用尽
	CodeModelBlocked   = 11102 // 该后端无此模型
	CodeChannel        = 11128 // 渠道未获批（Illegal API invocation from an unapproved channel）
	CodeBilling        = 402   // HTTP 402 也是额度耗尽
)

// softRateResetLoc 上游 6004 文案里的时间恒为 UTC+8，与进程本地时区无关。
var softRateResetLoc = time.FixedZone("UTC+8", 8*60*60)

// softRateTimeRE 从 6004 的 msg 里抠出墙钟。只认完整日期时间，
// 文案里其余中文（含变量）不参与匹配。
var softRateTimeRE = regexp.MustCompile(`\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}`)

// softRateTimeLayout 与软限速文案里的时间格式一致（秒级，无时区后缀）。
const softRateTimeLayout = "2006-01-02 15:04:05"

// flexCode 兼容两种 code 形态：数字（14018）与字符串（"6004"）。
// SSE 错误帧里是字符串，普通错误体里是数字，不兼容会漏判。
type flexCode struct {
	Val int
	Set bool
}

func (c *flexCode) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" {
		return nil
	}
	s = strings.Trim(s, `"`)
	if s == "" {
		return nil
	}
	n := 0
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return nil // 非数字 code：当作没有，不阻断整解析
	}
	c.Val, c.Set = n, true
	return nil
}

// bizEnvelope 上游错误包络。两种形态并存（顶层 {code,msg} 与嵌套 {error:{data:{code,msg}}}），
// 故 error / data 都保留为原始 JSON，由 BizCode 递归下探。
//
// Message 是 SSE 错误帧的字段名（OpenAI 风格 {error:{message,code}}），
// 与 RPC 包络的 Msg 并存：不兼容两种写法会让流内错误帧拿不到可读文案。
type bizEnvelope struct {
	Code    flexCode        `json:"code"`
	Msg     string          `json:"msg"`
	Message string          `json:"message"`
	Error   json.RawMessage `json:"error"`
	Data    json.RawMessage `json:"data"`
}

// BizCode 从响应体解出业务 code 与可读 msg；解不出返回 (0, "")。
//
// 逐层下探的顺序是 error → data → error.data → error.data.data，
// 覆盖实测的三种包络：{code,msg}、{error:{data:{code,msg}}}、{data:{code,msg}}。
// 找不到含 code 的层时，仍返回沿途最深一层的 msg（用于前端展示）。
func BizCode(raw []byte) (int, string) {
	return bizCodeDepth(raw, 0)
}

// bizCodeDepth 限制递归深度，防御上游返回自嵌套 JSON（无限下探）。
func bizCodeDepth(raw []byte, depth int) (int, string) {
	if depth > 4 || len(raw) == 0 {
		return 0, ""
	}
	var env bizEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return 0, ""
	}
	bestMsg := strings.TrimSpace(env.Msg)
	if bestMsg == "" {
		bestMsg = strings.TrimSpace(env.Message)
	}
	if env.Code.Set {
		return env.Code.Val, bestMsg
	}
	for _, next := range []json.RawMessage{env.Error, env.Data} {
		if len(next) == 0 {
			continue
		}
		if code, msg := bizCodeDepth(next, depth+1); code != 0 {
			return code, msg
		} else if msg != "" && bestMsg == "" {
			bestMsg = msg
		}
	}
	return 0, bestMsg
}

// ClassifyChatError 按 HTTP 状态码 + 响应体分类。
//
// 判定顺序即优先级，每层的相对位置都有语义依据（见 docs/POOL-LOADBALANCE.md §4.2）：
//  1. 402 / 14018 在 401 之前 —— 两者都是账号级终态，先判额度可让用户看到更准确的原因，
//     避免「额度耗尽 + token 过期」的号在 credential / quota 之间来回翻转；
//  2. 6004 在 429 之前 —— 6004 是 429 的子集，必须先按业务码分流成模型级；
//  3. 11128 / 11102 在 429 之前 —— 它们可能带任意状态码，但都不能罚号。
//
// status 传 200 时用于解析 SSE 中途错误帧（HTTP 已经 200，错误只在 body 里）。
func ClassifyChatError(status int, body []byte) ChatErrKind {
	code, _ := BizCode(body)

	// 账号级终态优先：额度耗尽。
	if status == CodeBilling || code == CodeBilling || code == CodeQuotaExhausted {
		return ErrKindQuotaExhausted
	}
	// 凭证失效（401）：DoChat 已尝试过强刷，走到这里说明刷不动。
	if status == 401 {
		return ErrKindCredential
	}
	// 模型级限额：业务码优先于裸 429。
	if code == CodeModelRate {
		return ErrKindModelRate
	}
	// 渠道/指纹问题，换号无用。
	if code == CodeChannel {
		return ErrKindChannel
	}
	// 该后端无此模型。
	if code == CodeModelBlocked {
		return ErrKindModelBlocked
	}
	// 裸 429：保守按模型级处理（对账号更宽松，判错代价只是多试几个模型）。
	if status == 429 {
		return ErrKindModelRate
	}
	if status >= 500 {
		return ErrKindServer
	}
	if status >= 400 {
		return ErrKindBadParams
	}
	if status == 200 {
		return ErrKindNone
	}
	return ErrKindPassThrough
}

// ParseRateReset 从 6004 的 msg 里解出上游权威的重置墙钟。
//
// 支持带与不带 " UTC+8" 后缀两种写法（时间本身一律按 UTC+8 解释）。
// 解析不出返回 (零值, false) —— 调用方必须退回有界退避，绝不臆造时间。
func ParseRateReset(body []byte) (time.Time, bool) {
	_, msg := BizCode(body)
	if msg == "" {
		return time.Time{}, false
	}
	ts := softRateTimeRE.FindString(msg)
	if ts == "" {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation(softRateTimeLayout, ts, softRateResetLoc)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}
