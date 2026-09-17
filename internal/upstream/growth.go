// growth.go growth 域「成长任务体系」：对话活跃上报、连登状态、补签卡、奖励兑换、
// 抽奖、猫猫旅行、新手礼包/活动补偿。
//
// 移植自 references/workbuddy2api（多账号网关版），单账号化简化：
//   - 全部走 c.ep().Upstream 单主机（cn = copilot.tencent.com），请求头沿用现有
//     billingHeaders（签到/余额已验证该头组对该主机有效），不引入第二主机
//   - Region 门控由调用方负责（scheduler/admin 前置判定 region=="cn"），本层不判断
//
// 端点契约（上游统一 {code,msg,data} 信封，code!=0 即业务错误）：
//
//	POST /v2/report                            活跃上报（body 为单元素数组）
//	GET  /activity/growth/streak               连登天数 + 补签卡 + 兑换状态（一响应三用途）
//	GET  /activity/growth/heatmap              活跃地图热力格
//	POST /activity/growth/makeup-cards/use     补签（target_date，CST 自然日）
//	POST /activity/growth/redeem               连登里程碑兑换（7d/14d/28d）
//	GET  /activity/growth/lottery/chances      抽奖次数余额
//	POST /activity/growth/lottery/draw         抽奖一次
//	GET  /activity/growth/buddy/info           猫档案（buddy null = 无猫）
//	POST /activity/growth/buddy/agreement      同意协议（幂等）
//	POST /activity/growth/buddy/first          领养（+300 积分 +8 能量）
//	GET  /activity/growth/buddy/travel/status  旅行状态（idle/traveling/arrived）
//	POST /activity/growth/buddy/travel/depart  派出旅行
//	POST /activity/growth/buddy/travel/claim   领取到站奖励（必带 record_id）
//	POST /billing/meter/claim-gift             新手礼包（每号一次，路径回退链）
//	POST /billing/meter/claim-compensation     活动补偿（有则领，路径回退链）
package upstream

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ── 传输 helper 与错误类型 ─────────────────────────────────────────────────────

// GrowthError growth 域上游错误：保留 HTTP 状态与业务码，供正常态分类器判定。
// 网络/解析层错误不包装为 *GrowthError（Status=0），一律视为真错误。
type GrowthError struct {
	Status int // HTTP 状态码
	Code   int // 业务 code（HTTP 200 时非 0 = 业务错误）
	Msg    string
	Body   string // 截断响应体（≤300 字节），排障用
}

func (e *GrowthError) Error() string {
	if e.Status != 0 && e.Status != http.StatusOK {
		return fmt.Sprintf("growth HTTP %d: %s", e.Status, e.Msg)
	}
	return fmt.Sprintf("growth code=%d: %s", e.Code, e.Msg)
}

// growthBase growth 域请求主机。包级变量便于单测以 httptest 替换（生产恒为当前端点）。
var growthBase = func(c *Client) string { return c.ep().Upstream }

// growthCall growth 域统一传输：任意 method/body，token 有效性保证 + 信封解析。
// 语义与 billing() 对齐：先 EnsureValid（需要时自动刷新）再请求；
// HTTP 非 2xx 或业务 code != 0 → *GrowthError；2xx 但响应体不是 JSON 信封
// （反代错误页等）→ 普通 error，不包装 GrowthError（正常态分类器一律判 false）。
func (c *Client) growthCall(method, path string, body any) (json.RawMessage, error) {
	t, err := c.token()
	if err != nil {
		return nil, err
	}
	// growth 前也确保 token 有效（同 billing()：best-effort，失败仍尝试请求）
	_ = c.toks.EnsureValid(c.RefreshFn)
	t = c.toks.Get()
	if t == nil { // EnsureValid 与 Get 之间可能被并发登出清空
		return nil, ErrNoToken
	}
	old := *t
	old.Domain = orDefault(t.Domain, c.ep().Domain)

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
	req.Header = billingHeaders(&old)
	resp, err := c.short.Do(req) // 30s 超时，单次 RPC 足够
	if err != nil {
		return nil, err
	}
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	resp.Body.Close()
	if rerr != nil {
		return nil, rerr
	}
	var env envelope
	parseErr := json.Unmarshal(raw, &env)
	trunc := truncStr(string(raw), 300)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &GrowthError{Status: resp.StatusCode, Msg: env.Msg, Body: trunc}
	}
	if parseErr != nil {
		// 2xx 但非 JSON：不能当成功，否则写操作会被静默记为假成功（如 BuddyFirst
		// 后误报「领养成功」）；宁可 fail，次日重试（服务端幂等）。
		return nil, fmt.Errorf("growth 响应体非 JSON（HTTP %d）: %s", resp.StatusCode, trunc)
	}
	if env.Code != 0 {
		return nil, &GrowthError{Status: resp.StatusCode, Code: env.Code, Msg: env.Msg, Body: trunc}
	}
	if len(env.Data) == 0 {
		return json.RawMessage("null"), nil // 信封缺 data：归一化为 null，调用方 Unmarshal 得零值
	}
	return env.Data, nil
}

// ── 活跃上报 ──────────────────────────────────────────────────────────────────

// chatRequestEvent 客户端 chat_request_send 事件完整形状（36 字段，照抄参考项目
// report.go；勿精简，防上游加严校验）。userId 必填（= token UID）：缺失时
// 服务端返回 200 但静默丢弃，连登进度不动。
type chatRequestEvent struct {
	EventCode             string `json:"eventCode"`
	Timestamp             int64  `json:"timestamp"`
	ReportDelay           int    `json:"reportDelay"`
	Mode                  string `json:"mode"`
	ConversationID        string `json:"conversationId"`
	RequestID             string `json:"requestId"`
	InputLength           int    `json:"inputLength"`
	RequestModelID        string `json:"requestModelId"`
	RequestModelName      string `json:"requestModelName"`
	IsPlan                bool   `json:"isPlan"`
	IsAutoExecuteTerminal bool   `json:"isAutoExecuteTerminal"`
	IsAutoModify          bool   `json:"isAutoModify"`
	CodebaseEnable        bool   `json:"codebaseEnable"`
	MaxToken              int    `json:"maxToken"`
	MaxSteps              int    `json:"maxSteps"`
	Temperature           int    `json:"temperature"`
	MaxRetries            int    `json:"maxRetries"`
	MentionContexts       []any  `json:"mentionContexts"`
	KnowledgeID           []any  `json:"knowledgeId"`
	KnowledgeName         []any  `json:"knowledgeName"`
	CodebaseID            string `json:"codebaseId"`
	MentionContextCount   int    `json:"mentionContextCount"`
	Command               string `json:"command"`
	ExpertID              string `json:"expertId"`
	RecommendID           string `json:"recommendId"`
	SkillID               string `json:"skillId"`
	SkillCount            int    `json:"skillCount"`
	TotalCount            int    `json:"totalCount"`
	FileURI               string `json:"fileUri"`
	PresentAt             int64  `json:"presentAt"`
	TraceID               string `json:"traceId"`
	RootRequestID         string `json:"rootRequestId"`
	ParentConversationID  string `json:"parentConversationId"`
	AgentName             string `json:"agentName"`
	AgentType             string `json:"agentType"`
	UserID                string `json:"userId"` // ★ 必填 = token UID
}

// ReportChatActivity 发送一条对话活跃上报（chat_request_send）。
// conversationID 由调用方生成（"b2a-<UnixMilli>"），服务端不校验与真实会话一致性；
// requestID 为本轮请求独立标识（多轮同会话上报时各条不同），空时回落 conversationID。
// 一条上报同时点亮 growth 连登 + 解锁领养前置任务（chat_5 需 5 次对话）。
func (c *Client) ReportChatActivity(conversationID, requestID string) error {
	t, err := c.token()
	if err != nil {
		return err
	}
	if requestID == "" {
		requestID = conversationID
	}
	now := time.Now().UnixMilli()
	ev := chatRequestEvent{
		EventCode:             "chat_request_send",
		Timestamp:             now,
		ReportDelay:           0,
		Mode:                  "craft",
		ConversationID:        conversationID,
		RequestID:             requestID,
		InputLength:           12,
		RequestModelID:        "deepseek-v4-flash",
		RequestModelName:      "DeepSeek V4 Flash",
		IsPlan:                false,
		IsAutoExecuteTerminal: false,
		IsAutoModify:          false,
		CodebaseEnable:        false,
		MaxToken:              0,
		MaxSteps:              0,
		Temperature:           0,
		MaxRetries:            0,
		MentionContexts:       []any{},
		KnowledgeID:           []any{},
		KnowledgeName:         []any{},
		CodebaseID:            "",
		MentionContextCount:   0,
		Command:               "",
		ExpertID:              "",
		RecommendID:           "",
		SkillID:               "",
		SkillCount:            0,
		TotalCount:            0,
		FileURI:               "",
		PresentAt:             now,
		TraceID:               "",
		RootRequestID:         conversationID,
		ParentConversationID:  conversationID,
		AgentName:             "default",
		AgentType:             "conversation",
		UserID:                t.UID,
	}
	raw, err := json.Marshal([]chatRequestEvent{ev}) // 注意：数组单元素
	if err != nil {
		return err
	}
	_, err = c.growthCall(http.MethodPost, "/v2/report", json.RawMessage(raw))
	return err
}

// ── 连登状态（一次 GET 三种用途）───────────────────────────────────────────────

// GrowthTierSpec 连登奖励档位配置（streak.redemption_status.tiers 数组元素）。
type GrowthTierSpec struct {
	Tier    string `json:"tier"`    // "7d"|"14d"|"28d"
	Days    int    `json:"days"`    // 达标连登天数
	Credit  int    `json:"credit"`  // 送积分
	Energy  int    `json:"energy"`  // 送能量
	Cards   int    `json:"cards"`   // 送补签卡
	Chances int    `json:"chances"` // 送抽奖次数
}

// GrowthStreakInfo streak 端点完整快照：连登天数 + 补签卡余额 + 兑换状态，
// 一次 GET 读完，供 overview 聚合与三用途（自检/补签/挑档）复用。
type GrowthStreakInfo struct {
	Streak struct {
		Days int `json:"days"`
	} `json:"streak"`
	MakeupCards struct {
		Balance int `json:"balance"`
		Max     int `json:"max"`
	} `json:"makeup_cards"`
	Redemption struct {
		Tier7dStatus  string           `json:"tier_7d_status"`
		Tier14dStatus string           `json:"tier_14d_status"`
		Tier28dStatus string           `json:"tier_28d_status"`
		Tiers         []GrowthTierSpec `json:"tiers"`
		RemainingDays int              `json:"remaining_days"`
	} `json:"redemption_status"`
}

// Claimed 报告指定档位本月是否已领（status=="claimed"）。
func (g *GrowthStreakInfo) Claimed(tier string) bool {
	switch tier {
	case "7d":
		return g.Redemption.Tier7dStatus == "claimed"
	case "14d":
		return g.Redemption.Tier14dStatus == "claimed"
	case "28d":
		return g.Redemption.Tier28dStatus == "claimed"
	}
	return false
}

// EligibleTier 挑「天数达标且未领」的最高档；"" = 无可领档（正常态）。
// 按 days 取最大者，而非命中的数组末位，乱序响应下同样正确。
func (g *GrowthStreakInfo) EligibleTier() string {
	days := g.Streak.Days
	best := ""
	bestDays := 0
	for _, sp := range g.Redemption.Tiers {
		if days >= sp.Days && sp.Days > bestDays && !g.Claimed(sp.Tier) {
			best, bestDays = sp.Tier, sp.Days
		}
	}
	return best
}

// GrowthStreakInfo 读取连登完整快照（GET /activity/growth/streak）。
func (c *Client) GrowthStreakInfo() (*GrowthStreakInfo, error) {
	data, err := c.growthCall(http.MethodGet, "/activity/growth/streak", nil)
	if err != nil {
		return nil, err
	}
	var info GrowthStreakInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// ── 热力格与补签 ───────────────────────────────────────────────────────────────

// cstShanghai 上游自然日口径：CST（UTC+8 固定偏移，不依赖 tzdata）。
// 与签到状态机的本地时区不同：growth 的「日」由上游 CST 自然日定义，必须单独用 CST。
var cstShanghai = time.FixedZone("CST", 8*60*60)

// CSTShanghai 返回上游自然日口径时区（供 admin 日期校验等外部使用，不重复定义）。
func CSTShanghai() *time.Location { return cstShanghai }

// GrowthYesterdayDate 昨日 CST 自然日。补签判据固定盯昨日（今日尚未结算）。
func GrowthYesterdayDate(now time.Time) string {
	return now.AddDate(0, 0, -1).In(cstShanghai).Format("2006-01-02")
}

// GrowthTodayDate 今日 CST 自然日（日内日标志口径）。
func GrowthTodayDate(now time.Time) string {
	return now.In(cstShanghai).Format("2006-01-02")
}

// HeatmapCell 活跃地图热力格（一日一格）。
type HeatmapCell struct {
	Date  string `json:"date"`  // "2006-01-02"（比较取前 10 位）
	Score int    `json:"score"` // 0 = 漏签
}

// GrowthHeatmap 读取活跃地图热力格列表（GET /activity/growth/heatmap → data.cells）。
func (c *Client) GrowthHeatmap() ([]HeatmapCell, error) {
	data, err := c.growthCall(http.MethodGet, "/activity/growth/heatmap", nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Cells []HeatmapCell `json:"cells"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	return resp.Cells, nil
}

// HeatmapDayScore 返回 cells 中 date（2006-01-02）当日的 score；
// 无该日格（活跃地图未覆盖）返回 (0,false)——调用方据 ok=false 判「无判据，不动」。
func HeatmapDayScore(cells []HeatmapCell, date string) (int, bool) {
	for _, c := range cells {
		if len(c.Date) >= 10 && c.Date[:10] == date {
			return c.Score, true
		}
	}
	return 0, false
}

// UseMakeupCard 对指定日期使用补签卡（target_date 格式 2006-01-02，CST 自然日）。
// 无卡 / 该日无漏签 / 已补过 → *GrowthError，由调用方静默跳过。
func (c *Client) UseMakeupCard(targetDate string) error {
	_, err := c.growthCall(http.MethodPost, "/activity/growth/makeup-cards/use",
		map[string]any{"target_date": targetDate})
	return err
}

// ── 兑换与抽奖 ─────────────────────────────────────────────────────────────────

// GrowthRedeemResult 兑换回执（POST /activity/growth/redeem 成功态 data）。
type GrowthRedeemResult struct {
	CardsGranted   int `json:"cards_granted"`
	CardsOverflow  int `json:"cards_overflow"`
	CreditGranted  int `json:"credit_granted"`
	EnergyGranted  int `json:"energy_granted"`
	ChancesGranted int `json:"chances_granted"`
}

// GrowthRedeem 兑换指定档位连登奖励。clientToken 为幂等键，空时自动生成
// （"redeem-<tier>-<32hex>"）；每次必须新生成，复用会被上游幂等去重吞掉。
func (c *Client) GrowthRedeem(tier, clientToken string) (*GrowthRedeemResult, error) {
	if clientToken == "" {
		clientToken = "redeem-" + tier + "-" + uuidHex()
	}
	data, err := c.growthCall(http.MethodPost, "/activity/growth/redeem",
		map[string]any{"tier": tier, "client_token": clientToken})
	if err != nil {
		return nil, err
	}
	var res GrowthRedeemResult
	if len(data) > 0 {
		_ = json.Unmarshal(data, &res) // 回执字段缺失不视为失败：调用方按 0 记日志
	}
	return &res, nil
}

// GrowthLotteryChances 查询抽奖次数余额（GET /activity/growth/lottery/chances → data.balance）。
// 0 = 无次数（连登奖励未送或已抽完），正常态、无副作用。
func (c *Client) GrowthLotteryChances() (int, error) {
	data, err := c.growthCall(http.MethodGet, "/activity/growth/lottery/chances", nil)
	if err != nil {
		return 0, err
	}
	var resp struct {
		Balance int `json:"balance"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, err
	}
	return resp.Balance, nil
}

// GrowthLotteryDrawResult 单次抽奖结果。prize_type credit=积分 / physical=实物
// （实物需人工在官方渠道填地址，本网关不代填）。
type GrowthLotteryDrawResult struct {
	PrizeCode    string `json:"prize_code"`
	PrizeName    string `json:"prize_name"`
	PrizeType    string `json:"prize_type"`
	CreditAmount int    `json:"credit_amount"`
}

// GrowthLotteryDraw 抽一次奖。clientToken 为幂等键，空时自动生成（"draw-<32hex>"）。
// 无抽奖次数（400 insufficient）与抽奖未开启（400 lottery disabled）属正常态，
// 见 IsLotteryNoChance / IsLotteryDisabled，调用方据此静默跳过。
func (c *Client) GrowthLotteryDraw(clientToken string) (*GrowthLotteryDrawResult, error) {
	if clientToken == "" {
		clientToken = "draw-" + uuidHex()
	}
	data, err := c.growthCall(http.MethodPost, "/activity/growth/lottery/draw",
		map[string]any{"client_token": clientToken})
	if err != nil {
		return nil, err
	}
	var res GrowthLotteryDrawResult
	if len(data) > 0 {
		_ = json.Unmarshal(data, &res) // 奖品字段缺失不视为失败
	}
	return &res, nil
}

// ── 猫猫档案与旅行 ─────────────────────────────────────────────────────────────

// Buddy 账号当前猫档案。
type Buddy struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// TravelState 猫猫旅行状态。
type TravelState struct {
	State             string `json:"state"`               // idle | traveling | arrived
	DailyLimitReached bool   `json:"daily_limit_reached"` // 今日已派出过（CST 00:00 重置）
	RecordID          int64  `json:"record_id"`           // 在途/到站记录 id，claim 必带
	RewardCredit      int64  `json:"reward_credit"`       // 到站可领奖励积分
}

// BuddyInfo 查询当前猫档案；返回 (nil, nil) 表示无猫（buddy 为 null/缺失/空对象）。
func (c *Client) BuddyInfo() (*Buddy, error) {
	data, err := c.growthCall(http.MethodGet, "/activity/growth/buddy/info", nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Buddy json.RawMessage `json:"buddy"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(string(resp.Buddy))
	if trimmed == "" || trimmed == "null" || trimmed == "{}" {
		return nil, nil
	}
	var b Buddy
	if err := json.Unmarshal(resp.Buddy, &b); err != nil {
		return nil, err
	}
	return &b, nil
}

// BuddyAgreement 同意协议（幂等，可重复调用）。
func (c *Client) BuddyAgreement() error {
	_, err := c.growthCall(http.MethodPost, "/activity/growth/buddy/agreement",
		map[string]any{"agree": true})
	return err
}

// BuddyFirst 领养第一只猫（+300 积分 +8 能量）。门槛未达标（chat_5 对话量不足）
// 返回 HTTP 400（见 IsBuddyTaskIncomplete），属预期行为，调用方当日不重试。
func (c *Client) BuddyFirst() error {
	_, err := c.growthCall(http.MethodPost, "/activity/growth/buddy/first", map[string]any{})
	return err
}

// TravelStatus 查询猫猫旅行状态。
func (c *Client) TravelStatus() (*TravelState, error) {
	data, err := c.growthCall(http.MethodGet, "/activity/growth/buddy/travel/status", nil)
	if err != nil {
		return nil, err
	}
	var st TravelState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// TravelDepart 派出猫旅行；locationID 实测 1~4，4 个地点收益/时长完全相同，固定 4。
func (c *Client) TravelDepart(locationID int) error {
	_, err := c.growthCall(http.MethodPost, "/activity/growth/buddy/travel/depart",
		map[string]any{"location_id": locationID})
	return err
}

// TravelClaim 领取到站奖励（必须带 record_id），返回 reward_credit。
func (c *Client) TravelClaim(recordID int64) (int64, error) {
	data, err := c.growthCall(http.MethodPost, "/activity/growth/buddy/travel/claim",
		map[string]any{"record_id": recordID})
	if err != nil {
		return 0, err
	}
	var resp struct {
		RewardCredit int64 `json:"reward_credit"`
	}
	if len(data) > 0 {
		_ = json.Unmarshal(data, &resp) // 奖励字段缺失不视为失败：调用方按 0 记日志
	}
	return resp.RewardCredit, nil
}

// ── 礼包 / 补偿（路径回退链）──────────────────────────────────────────────────

// claimBonusPaths 参考项目实测于 www.codebuddy.cn/billing/meter/claim-*（无 /v2）；
// copilot.tencent.com 上前缀未知，双路径尝试：首路径 404 → 尝试下一路径。
var claimBonusPaths = []string{
	"/billing/meter/claim-gift",    // 参考项目口径（优先）
	"/v2/billing/meter/claim-gift", // 对齐本项目 checkin 路径族
}

// ClaimGift 领取新手礼包（每号一次，重复领业务错误由调用方静默）。返回到账 credit。
func (c *Client) ClaimGift() (int64, error) {
	return c.claimBonus(pathsReplaceSuffix(claimBonusPaths, "claim-gift"))
}

// ClaimCompensation 领取活动补偿（有则领，无则业务错误由调用方静默）。返回到账 credit。
func (c *Client) ClaimCompensation() (int64, error) {
	return c.claimBonus(pathsReplaceSuffix(claimBonusPaths, "claim-compensation"))
}

// pathsReplaceSuffix 把回退链模板的尾段换成指定端点名（保持「无 /v2 优先、有 /v2 兜底」结构）。
func pathsReplaceSuffix(paths []string, endpoint string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		if idx := strings.LastIndex(p, "/"); idx >= 0 {
			out[i] = p[:idx+1] + endpoint
		} else {
			out[i] = p
		}
	}
	return out
}

// claimBonus 领取类公共实现：逐路径 POST {} → data.credit。
// 404 → 尝试下一路径（主机上路径前缀未知）；业务错误（已领过等）原样返回由调用方静默。
func (c *Client) claimBonus(paths []string) (int64, error) {
	var lastErr error
	for _, p := range paths {
		data, err := c.growthCall(http.MethodPost, p, map[string]any{})
		if err != nil {
			var ge *GrowthError
			if errors.As(err, &ge) && ge.Status == http.StatusNotFound {
				lastErr = err
				continue // 路径不存在 → 试下一前缀
			}
			return 0, err
		}
		var resp struct {
			Credit int64 `json:"credit"`
		}
		_ = json.Unmarshal(data, &resp)
		return resp.Credit, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("growth claim: 无可用路径")
	}
	return 0, lastErr
}

// ── 错误分类器（正常态识别）────────────────────────────────────────────────────

// isGrowthErrWith 判定 err 是 *GrowthError、Status 命中且 msg 含任一 marker
// （大小写不敏感）。网络/解析层错误返回 false，不误判为正常态。
func isGrowthErrWith(err error, status int, markers ...string) bool {
	if err == nil {
		return false
	}
	var ge *GrowthError
	if !errors.As(err, &ge) || ge.Status != status {
		return false
	}
	lower := strings.ToLower(ge.Msg)
	for _, m := range markers {
		if strings.Contains(lower, strings.ToLower(m)) {
			return true
		}
	}
	return false
}

// IsRedeemAlreadyClaimed 报告是否「本月已领取」幂等态（HTTP 409 + duplicate/已领取）。
func IsRedeemAlreadyClaimed(err error) bool {
	return isGrowthErrWith(err, http.StatusConflict, "duplicate", "已领取")
}

// IsRedeemNotEnoughDays 报告是否「连续登录天数不足」（HTTP 403）。正常态。
func IsRedeemNotEnoughDays(err error) bool {
	return isGrowthErrWith(err, http.StatusForbidden, "连续登录天数不足")
}

// IsLotteryNoChance 报告是否「无抽奖次数」（HTTP 400 + insufficient balance）。正常态。
func IsLotteryNoChance(err error) bool {
	return isGrowthErrWith(err, http.StatusBadRequest, "insufficient lottery chance balance")
}

// IsLotteryDisabled 报告是否「抽奖未开启」（HTTP 400 + lottery disabled）。正常态。
func IsLotteryDisabled(err error) bool {
	return isGrowthErrWith(err, http.StatusBadRequest, "lottery disabled")
}

// IsBuddyTaskIncomplete 判定「领养门槛未达标」（HTTP 400 + first_buddy 关键词）。
// 该错误当日不应重试，避免对上游重试轰炸。
func IsBuddyTaskIncomplete(err error) bool {
	return isGrowthErrWith(err, http.StatusBadRequest, "first_buddy task not completed yet")
}
