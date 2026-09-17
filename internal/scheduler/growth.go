// growth.go 成长任务链编排：活跃上报 → 连登自检 → 领养 → 礼包/补签 → 兑换 → 抽奖 → 旅行巡检。
//
// 移植自 references/workbuddy2api 的 runActivity/claimGrowthRewards/travel 状态机，
// 单账号化简化：单次调用 + 账号内上报间隔 1.5s（防风控保留）；当日防抖从参考项目的
// 进程内内存 map 升级为 growthDay 内存态 + SQLite growth_cache 镜像（跨重启持久）。
//
// 幂等语义：上报按日标志 report_day；领养 adopt_day 防同日轰炸；兑换 reward_day +
// 服务端 409 兜底，且领取类写失败当日不重试（次日自然重置）。
package scheduler

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"buddy2api-go/internal/auth"
	"buddy2api-go/internal/config"
	"buddy2api-go/internal/upstream"
)

const (
	// growthDayTTL 日标志缓存有效期：跨一日重启后 catch-up 判据与 overview 仍可读。
	growthDayTTL = 48 * 3600
	// travelLocationID 派出地点固定 4：4 个地点收益/时长区间完全相同，无最优解。
	travelLocationID = 4
	// growthCatchupWindowEndMin 补跑窗口下限（22:00）：深夜不打扰。
	growthCatchupWindowEndMin = 22 * 60
	// growthReportCountMax 每日上报条数上限（防风控）。
	growthReportCountMax = 10
)

// growthReportGap 同账号内 N 条上报间隔：5 连发模拟同一会话多轮对话，
// 秒发易触发风控，故 1.5s 一条。测试可置 0。
var growthReportGap = 1500 * time.Millisecond

// growthCatchupDelay 启动补跑延迟：留出 HTTP 服务启动时间
// （测试可拉长，避免 Reconfigure 排出的真实 timer 在测试结束后触发）。
var growthCatchupDelay = 30 * time.Second

// growthCacheTable growth 相关缓存的表名与 key 常量。
// 日标志（report/adopt/reward_day）的值为 CST 日期串，便于跨日直接比对。
const (
	growthCacheTable   = "growth_cache"
	growthKeyReportDay = "report_day"
	growthKeyAdoptDay  = "adopt_day"
	growthKeyRewardDay = "reward_day"
	growthKeyLastRun   = "last_run"
)

// growthClient 成长链依赖的上游接口（*upstream.Client 实现；测试可注入 fake）。
type growthClient interface {
	TokenSummary() *auth.Token
	ReportChatActivity(conversationID, requestID string) error
	GrowthStreakInfo() (*upstream.GrowthStreakInfo, error)
	GrowthHeatmap() ([]upstream.HeatmapCell, error)
	UseMakeupCard(targetDate string) error
	GrowthRedeem(tier, clientToken string) (*upstream.GrowthRedeemResult, error)
	GrowthLotteryChances() (int, error)
	GrowthLotteryDraw(clientToken string) (*upstream.GrowthLotteryDrawResult, error)
	BuddyInfo() (*upstream.Buddy, error)
	BuddyAgreement() error
	BuddyFirst() error
	TravelStatus() (*upstream.TravelState, error)
	TravelDepart(locationID int) error
	TravelClaim(recordID int64) (int64, error)
	ClaimGift() (int64, error)
	ClaimCompensation() (int64, error)
}

// ── 结果类型 ──────────────────────────────────────────────────────────────────

// GrowthStep 单步结果。Status ∈ "ok" | "skip" | "warn" | "fail"。
type GrowthStep struct {
	Key    string `json:"key"` // report | streak | adopt | makeup | redeem | lottery | bonus | travel
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"` // 人话摘要（前端直接展示）
}

// GrowthChainResult 成长链单次执行的逐步结果（自动/手动共用，落 last_run 缓存 + 返回给前端）。
type GrowthChainResult struct {
	Day    string       `json:"day"`              // CST 日期
	At     int64        `json:"at,omitempty"`     // 执行时刻（unix 秒，前端「上次执行」展示）
	Ran    bool         `json:"ran"`              // false = 本次跳过（未登录/global/互斥占用）
	Reason string       `json:"reason,omitempty"` // 跳过原因: not_logged_in | region_global | busy
	Steps  []GrowthStep `json:"steps"`            // 按执行顺序记录
}

// add 追加一步并按 status 分级打日志（运维 grep 友好：grep '"growth"'）。
func (r *GrowthChainResult) add(key, status, detail string) {
	r.Steps = append(r.Steps, GrowthStep{Key: key, Status: status, Detail: detail})
	if status == "fail" || status == "warn" {
		slog.Warn("growth", "step", key, "status", status, "detail", detail)
	} else {
		slog.Info("growth", "step", key, "status", status, "detail", detail)
	}
}

// findStep 取指定 key 的步骤（无则零值）。
func (r *GrowthChainResult) findStep(key string) (GrowthStep, bool) {
	for _, s := range r.Steps {
		if s.Key == key {
			return s, true
		}
	}
	return GrowthStep{}, false
}

// TravelTickResult 单趟旅行巡检结果。
type TravelTickResult struct {
	Ran    bool   `json:"ran"`
	Reason string `json:"reason,omitempty"` // not_logged_in | region_global
	Status string `json:"status,omitempty"` // ok | skip | warn | fail（前端 toast 分级）
	Action string `json:"action,omitempty"` // claim | depart | skip
	Detail string `json:"detail,omitempty"`
}

// ActivityReportResult 手动活跃上报结果。
type ActivityReportResult struct {
	Ran    bool   `json:"ran"`
	Reason string `json:"reason,omitempty"`
	Sent   int    `json:"sent"`
	Total  int    `json:"total"`
	Detail string `json:"detail,omitempty"`
}

// GrowthAdoptResult 手动领养结果。
type GrowthAdoptResult struct {
	Ran    bool   `json:"ran"`
	Reason string `json:"reason,omitempty"`
	Status string `json:"status"` // ok | skip | fail
	Detail string `json:"detail,omitempty"`
}

// GrowthRedeemActionResult 手动兑换结果。
type GrowthRedeemActionResult struct {
	Ran     bool                         `json:"ran"`
	Reason  string                       `json:"reason,omitempty"`
	Status  string                       `json:"status"` // ok | skip | fail
	Detail  string                       `json:"detail,omitempty"`
	Granted *upstream.GrowthRedeemResult `json:"granted,omitempty"`
}

// ── 日内状态（内存 + SQLite 双层）──────────────────────────────────────────────

// growthDay 一日内的成长任务状态。day 为 CST 自然日（上游口径），跨日重置。
type growthDay struct {
	day        string // "2006-01-02" CST
	reportDone bool   // 今日 N 条上报已发满
	adoptTried bool   // 今日已尝试领养且门槛未达（防同日多趟轰炸）
	rewardDone bool   // 今日奖励链已处理一轮
}

// rollGrowthDayLocked 跨 CST 日时重置内存状态；内存 day 为空（进程刚启动）或
// 跨日时从 cache 镜像回填当日标志（服务重启后日标志不丢，双保险）。
// 调用方需持 growthMu。
func (s *Scheduler) rollGrowthDayLocked(now time.Time) {
	day := upstream.GrowthTodayDate(now)
	if s.growthState.day == day {
		return
	}
	s.growthState = growthDay{day: day}
	if payload, _, ok := s.st.GetCache(growthCacheTable, growthKeyReportDay, growthDayTTL); ok && payload == day {
		s.growthState.reportDone = true
	}
	if payload, _, ok := s.st.GetCache(growthCacheTable, growthKeyAdoptDay, growthDayTTL); ok && payload == day {
		s.growthState.adoptTried = true
	}
	if payload, _, ok := s.st.GetCache(growthCacheTable, growthKeyRewardDay, growthDayTTL); ok && payload == day {
		s.growthState.rewardDone = true
	}
}

// growthPrecondition 成长类操作前置检查：region=cn 且已登录。
// 返回非空 reason 表示不可执行（正常态，不发起任何上游调用）。
func (s *Scheduler) growthPrecondition() string {
	if s.cfg.Get().Region != "cn" {
		return "region_global"
	}
	if s.growth.TokenSummary() == nil {
		return "not_logged_in"
	}
	return ""
}

// ── 成长任务链（核心编排）──────────────────────────────────────────────────────

// RunGrowthChain 执行一次完整成长链（cron / 手动触发 / 启动补跑共用）。
// 顺序：上报 → 连登自检 → 领养 → 礼包 → 补签+兑换+抽奖 → 旅行巡检。
// 任一步失败不阻断后续步（单步记 fail）。growthMu 互斥：已有链在跑 →
// 直接返回 {ran:false, reason:"busy"}（不等待）。
func (s *Scheduler) RunGrowthChain() GrowthChainResult {
	if reason := s.growthPrecondition(); reason != "" {
		return GrowthChainResult{Ran: false, Reason: reason}
	}
	if !s.growthMu.TryLock() {
		return GrowthChainResult{Ran: false, Reason: "busy"}
	}
	defer s.growthMu.Unlock()

	now := time.Now()
	s.rollGrowthDayLocked(now)
	day := upstream.GrowthTodayDate(now)
	res := GrowthChainResult{Day: day, At: now.Unix(), Ran: true}
	defer func() {
		if raw, err := json.Marshal(res); err == nil {
			_ = s.st.SetCache(growthCacheTable, growthKeyLastRun, string(raw))
		}
		if s.growthState.reportDone {
			_ = s.st.SetCache(growthCacheTable, growthKeyReportDay, day)
		}
	}()

	count := s.cfg.Get().GrowthReportCount
	if count <= 0 || count > growthReportCountMax {
		count = 5 // Normalize 已钳制，双保险
	}
	_, reported := s.stepReports(&res, count, false)
	var info *upstream.GrowthStreakInfo
	s.stepStreakCheck(&res, reported, &info)
	justAdopted := s.stepAdopt(&res, reported) // 上报刚发满 → force=true 豁免今日早前标记
	s.stepBonus(&res)
	s.stepRewardChain(&res, &info)
	s.stepTravel(&res, true, justAdopted)

	slog.Info("growth 链完成", "day", res.Day, "steps", len(res.Steps))
	return res
}

// RunActivityReports 手动活跃上报：不查 reportDone（用户显式意图），发满置 report_day。
// count 钳 1..10；growthMu 互斥（与自动链防并发秒发）。
func (s *Scheduler) RunActivityReports(count int) ActivityReportResult {
	if reason := s.growthPrecondition(); reason != "" {
		return ActivityReportResult{Ran: false, Reason: reason}
	}
	if count <= 0 {
		count = s.cfg.Get().GrowthReportCount
	}
	if count > growthReportCountMax {
		count = growthReportCountMax
	}
	if !s.growthMu.TryLock() {
		return ActivityReportResult{Ran: false, Reason: "busy"}
	}
	defer s.growthMu.Unlock()

	now := time.Now()
	s.rollGrowthDayLocked(now)
	res := GrowthChainResult{Day: upstream.GrowthTodayDate(now), Ran: true}
	sent, _ := s.stepReports(&res, count, true)
	detail := ""
	if st, ok := res.findStep("report"); ok {
		detail = st.Detail
	}
	return ActivityReportResult{Ran: true, Sent: sent, Total: count, Detail: detail}
}

// RunTravelTick 独立旅行巡检（cron / 手动按钮）：单动作状态机（arrived 先领奖，
// idle 才派出）。不做领养——领养归成长链与手动按钮，避免与链内 adopt 防抖交叉，
// 无猫仅记 skip。不持 growthMu：单趟只读本地状态、上游幂等，并发时服务端兜底。
func (s *Scheduler) RunTravelTick() TravelTickResult {
	if reason := s.growthPrecondition(); reason != "" {
		return TravelTickResult{Ran: false, Reason: reason}
	}
	res := &GrowthChainResult{Day: upstream.GrowthTodayDate(time.Now()), Ran: true}
	outcome := s.stepTravel(res, false, false)
	return TravelTickResult{Ran: true, Status: outcome.status, Action: outcome.action, Detail: outcome.detail}
}

// RunGrowthAdopt 手动领养（force 语义：豁免今日 adopt_day 防抖）。
// 门槛未达时置 adopt_day（当日 cron 链不再重试）。
func (s *Scheduler) RunGrowthAdopt() GrowthAdoptResult {
	if reason := s.growthPrecondition(); reason != "" {
		return GrowthAdoptResult{Ran: false, Reason: reason}
	}
	if !s.growthMu.TryLock() {
		return GrowthAdoptResult{Ran: false, Reason: "busy"}
	}
	defer s.growthMu.Unlock()

	now := time.Now()
	s.rollGrowthDayLocked(now)
	res := &GrowthChainResult{Day: upstream.GrowthTodayDate(now), Ran: true}
	s.stepAdopt(res, true)
	out := GrowthAdoptResult{Ran: true}
	if st, ok := res.findStep("adopt"); ok {
		out.Status, out.Detail = st.Status, st.Detail
	}
	return out
}

// RunGrowthRedeem 手动兑换指定档位。仅成功时置 reward_day（skip/fail 不置，
// 用户可换档重试）；正常态（409 已领/403 天数不足）识别为 skip。
func (s *Scheduler) RunGrowthRedeem(tier string) GrowthRedeemActionResult {
	if reason := s.growthPrecondition(); reason != "" {
		return GrowthRedeemActionResult{Ran: false, Reason: reason}
	}
	if !s.growthMu.TryLock() {
		return GrowthRedeemActionResult{Ran: false, Reason: "busy"}
	}
	defer s.growthMu.Unlock()

	now := time.Now()
	s.rollGrowthDayLocked(now)
	day := upstream.GrowthTodayDate(now)
	out := GrowthRedeemActionResult{Ran: true}
	r, err := s.growth.GrowthRedeem(tier, "")
	switch {
	case err == nil:
		s.growthState.rewardDone = true
		_ = s.st.SetCache(growthCacheTable, growthKeyRewardDay, day)
		out.Status, out.Granted = "ok", r
		out.Detail = fmt.Sprintf("兑换 %s：+%d 积分 +%d 能量 +%d 补签卡 +%d 抽奖",
			tier, r.CreditGranted, r.EnergyGranted, r.CardsGranted, r.ChancesGranted)
	case upstream.IsRedeemAlreadyClaimed(err):
		out.Status, out.Detail = "skip", "本月该档已领取"
	case upstream.IsRedeemNotEnoughDays(err):
		out.Status, out.Detail = "skip", "连续登录天数不足"
	default:
		out.Status, out.Detail = "fail", "兑换失败: "+err.Error()
	}
	return out
}

// ── 链内各步 ──────────────────────────────────────────────────────────────────

// stepReports 活跃上报：N 条共用同一 conversationId（模拟同会话多轮），
// 各条独立 requestId，间隔 growthReportGap 防风控。
// ignoreDone=false 时尊重日内 reportDone 标志（自动链）；true 为手动意图。
// 返回已发条数与是否发满；发满置 reportDone + cache。调用方需持 growthMu。
func (s *Scheduler) stepReports(res *GrowthChainResult, count int, ignoreDone bool) (sent int, full bool) {
	if !ignoreDone && s.growthState.reportDone {
		res.add("report", "skip", "今日已完成")
		return 0, false
	}
	cid := fmt.Sprintf("b2a-%d", time.Now().UnixMilli())
	for i := 1; i <= count; i++ {
		rid := fmt.Sprintf("%s-r%d", cid, i)
		if err := s.growth.ReportChatActivity(cid, rid); err != nil {
			// 不发满则 streak 自检无意义，直接 break；链继续走后续步
			// （奖励链用旧状态判断，不依赖今日上报）
			res.add("report", "fail", fmt.Sprintf("第 %d/%d 条失败: %v", i, count, err))
			return sent, false
		}
		sent++
		if i < count {
			time.Sleep(growthReportGap) // 1.5s 防风控
		}
	}
	s.growthState.reportDone = true
	_ = s.st.SetCache(growthCacheTable, growthKeyReportDay, res.Day)
	// 上报补满可能解锁 chat_5 门槛：清掉今日领养防抖，让后续领养尝试
	// （链内 force 或手动按钮）值得再试；若门槛仍未达，下次失败会重新置位。
	if s.growthState.adoptTried {
		s.growthState.adoptTried = false
		_ = s.st.DeleteCacheKey(growthCacheTable, growthKeyAdoptDay)
	}
	res.add("report", "ok", fmt.Sprintf("已上报 %d 条（同会话 %s）", count, cid))
	return sent, true
}

// stepStreakCheck 连登回读自检（只读 oracle）：上报发满才执行。
// days==0 → warn（上报 200 但静默丢弃）；回读失败 → warn 不重试。
// 成功时把快照存入 pInfo 供奖励链复用（免二次 GET）。
func (s *Scheduler) stepStreakCheck(res *GrowthChainResult, reported bool, pInfo **upstream.GrowthStreakInfo) {
	if !reported {
		res.add("streak", "skip", "本轮上报未发满，跳过连登自检")
		return
	}
	info, err := s.growth.GrowthStreakInfo()
	if err != nil {
		res.add("streak", "warn", "连登回读失败: "+err.Error())
		return
	}
	*pInfo = info
	if info.Streak.Days == 0 {
		res.add("streak", "warn", "⚠ 上报成功但 streak.days=0（疑似静默丢弃）")
		return
	}
	res.add("streak", "ok", fmt.Sprintf("连登 %d 天", info.Streak.Days))
}

// stepAdopt 领养：force=false 尊重今日 adoptTried 防抖；force=true 豁免
// （上报刚把 chat_5 补满 / 手动按钮）。返回本次是否领养成功，供旅行步判断
// 「刚领养，本轮不派出」。调用方需持 growthMu。
func (s *Scheduler) stepAdopt(res *GrowthChainResult, force bool) bool {
	if !force && s.growthState.adoptTried {
		res.add("adopt", "skip", "今日已尝试领养（门槛未达），明日自动重试")
		return false
	}
	buddy, err := s.growth.BuddyInfo()
	if err != nil {
		res.add("adopt", "fail", "查询猫档案失败: "+err.Error())
		return false
	}
	if buddy != nil {
		res.add("adopt", "skip", "已有猫 "+buddy.Name)
		return false
	}
	if err := s.growth.BuddyAgreement(); err != nil { // 幂等
		res.add("adopt", "fail", "同意协议失败: "+err.Error())
		return false
	}
	err = s.growth.BuddyFirst()
	switch {
	case err == nil:
		res.add("adopt", "ok", "领养成功（+300 积分）")
		return true
	case upstream.IsBuddyTaskIncomplete(err):
		// 门槛未达（chat_5）：当日不重试轰炸，置日标志；明日链上报后自动重试
		s.growthState.adoptTried = true
		_ = s.st.SetCache(growthCacheTable, growthKeyAdoptDay, res.Day)
		res.add("adopt", "skip", "对话门槛未达（chat_5），明日上报后自动重试")
		return false
	default:
		res.add("adopt", "fail", "领养失败: "+err.Error())
		return false
	}
}

// stepBonus 礼包/补偿领取（幂等，有则领）：两者都静默尝试，只有实际到账才记步
// （业务错误是常态——绝大多数号早已领过，无法与真错误区分）。
func (s *Scheduler) stepBonus(res *GrowthChainResult) {
	detail := ""
	if credit, err := s.growth.ClaimGift(); err == nil && credit > 0 {
		detail = fmt.Sprintf("礼包 +%d 积分", credit)
	}
	if credit, err := s.growth.ClaimCompensation(); err == nil && credit > 0 {
		if detail != "" {
			detail += " / "
		}
		detail += fmt.Sprintf("补偿 +%d 积分", credit)
	}
	if detail != "" {
		res.add("bonus", "ok", detail)
	}
}

// stepRewardChain 补签 + 兑换 + 抽奖（每日一轮）：
// 补签保连登（成功则重读天数挑档）→ 挑最高达标档兑换 → 抽奖一次。
// 处理过一轮即置 rewardDone，无论成败（领取类写失败当日不重试轰炸，
// 次日自然重置 / 服务端 409 兜底）。调用方需持 growthMu。
func (s *Scheduler) stepRewardChain(res *GrowthChainResult, pInfo **upstream.GrowthStreakInfo) {
	if s.growthState.rewardDone {
		res.add("redeem", "skip", "今日已处理奖励链")
		return
	}
	if *pInfo == nil {
		info, err := s.growth.GrowthStreakInfo()
		if err != nil {
			res.add("redeem", "warn", "连登状态读取失败: "+err.Error())
			return
		}
		*pInfo = info
	}
	if s.makeupYesterday(res, *pInfo) {
		// 补签成功 → 重读 streak（天数可能恢复到新档位）
		if info2, err := s.growth.GrowthStreakInfo(); err == nil {
			*pInfo = info2
		}
	}
	tier := (*pInfo).EligibleTier()
	if tier == "" {
		// 无可领档位：正常态（很多天没到 7d），不 warn；本轮不抽奖（chances 来自兑换）
		res.add("redeem", "skip", "无可领档位（未达标或已领）")
		s.growthState.rewardDone = true
		_ = s.st.SetCache(growthCacheTable, growthKeyRewardDay, res.Day)
		return
	}
	r, err := s.growth.GrowthRedeem(tier, "")
	switch {
	case err == nil:
		res.add("redeem", "ok", fmt.Sprintf("兑换 %s：+%d 积分 +%d 能量 +%d 补签卡 +%d 抽奖",
			tier, r.CreditGranted, r.EnergyGranted, r.CardsGranted, r.ChancesGranted))
	case upstream.IsRedeemAlreadyClaimed(err) || upstream.IsRedeemNotEnoughDays(err):
		res.add("redeem", "skip", "该档已领取或天数不足")
	default:
		res.add("redeem", "fail", "兑换失败: "+err.Error())
	}
	s.growthState.rewardDone = true
	_ = s.st.SetCache(growthCacheTable, growthKeyRewardDay, res.Day)
	s.stepLottery(res)
}

// makeupYesterday 昨日漏签且有补签卡时自动补签（保住连登连续天数）。
// 判据链：heatmap 昨日格 score==0 → makeup_cards.balance>0 → use。
// 无卡/无漏签/无该日格/查询失败均静默返回 false（不记步，不影响主流程）。
func (s *Scheduler) makeupYesterday(res *GrowthChainResult, info *upstream.GrowthStreakInfo) bool {
	cells, err := s.growth.GrowthHeatmap()
	if err != nil {
		return false // 只读判据失败：静默（次日再判，无写风险）
	}
	yesterday := upstream.GrowthYesterdayDate(time.Now())
	score, ok := upstream.HeatmapDayScore(cells, yesterday)
	if !ok || score != 0 {
		return false // 昨日有分或无判据：无需补签
	}
	if info.MakeupCards.Balance <= 0 {
		return false // 无卡
	}
	if err := s.growth.UseMakeupCard(yesterday); err != nil {
		res.add("makeup", "warn", "补签 "+yesterday+" 失败（次日再判）: "+err.Error())
		return false
	}
	res.add("makeup", "ok", fmt.Sprintf("补签 %s（连登保住）", yesterday))
	return true
}

// stepLottery 抽奖一次（自动链保守消耗：每日至多 1 次）。
// chances 先查后抽；无次数/未开启是正常态 skip；中实物仅展示奖名（不代填地址）。
func (s *Scheduler) stepLottery(res *GrowthChainResult) {
	chances, err := s.growth.GrowthLotteryChances()
	if err != nil {
		res.add("lottery", "warn", "抽奖次数查询失败: "+err.Error())
		return
	}
	if chances <= 0 {
		res.add("lottery", "skip", "无抽奖次数")
		return
	}
	draw, err := s.growth.GrowthLotteryDraw("")
	switch {
	case err == nil:
		detail := "抽中 " + draw.PrizeName
		switch draw.PrizeType {
		case "credit":
			detail += fmt.Sprintf("（+%d 积分）", draw.CreditAmount)
		case "physical":
			detail += "（实物，请在官方渠道填写地址）"
		}
		res.add("lottery", "ok", detail)
	case upstream.IsLotteryNoChance(err) || upstream.IsLotteryDisabled(err):
		res.add("lottery", "skip", "无抽奖次数或未开启")
	default:
		res.add("lottery", "fail", "抽奖失败: "+err.Error())
	}
}

// travelOutcome 旅行单趟结果（内部传递用）。
type travelOutcome struct {
	status string // ok | skip | warn | fail（同步记入 GrowthStep 与 TravelTickResult，前端 toast 分级）
	action string // claim | depart | skip
	detail string
}

// stepTravel 旅行巡检（单动作状态机，不轮询不等待）：
// arrived 先领奖（必带 record_id）→ idle 且未达日限才派出 → traveling 仅展示。
// fromChain=true 为链内调用：无猫时先尝试领养（force=false），刚领养本轮不派出
// （「领养立即派出」未经验证，刻意留到下轮巡检）；fromChain=false（独立巡检）
// 无猫仅记 skip——领养归成长链/手动按钮，避免与 adopt 防抖交叉。
func (s *Scheduler) stepTravel(res *GrowthChainResult, fromChain, justAdopted bool) travelOutcome {
	buddy, err := s.growth.BuddyInfo()
	if err != nil {
		res.add("travel", "fail", "查询猫档案失败: "+err.Error())
		return travelOutcome{status: "fail", action: "skip", detail: "查询猫档案失败"}
	}
	if buddy == nil {
		if fromChain {
			s.stepAdopt(res, false)
			st, _ := res.findStep("adopt")
			return travelOutcome{status: st.Status, action: "skip", detail: st.Detail}
		}
		res.add("travel", "skip", "尚未领养猫咪（领养在成长链/手动触发）")
		return travelOutcome{status: "skip", action: "skip", detail: "尚未领养猫咪"}
	}
	ts, err := s.growth.TravelStatus()
	if err != nil {
		res.add("travel", "fail", "旅行状态查询失败: "+err.Error())
		return travelOutcome{status: "fail", action: "skip", detail: "旅行状态查询失败"}
	}
	switch ts.State {
	case "arrived":
		if ts.RecordID == 0 {
			res.add("travel", "warn", "已到站但缺少 record_id，无法领奖")
			return travelOutcome{status: "warn", action: "skip", detail: "已到站但缺少 record_id"}
		}
		reward, err := s.growth.TravelClaim(ts.RecordID)
		if err != nil {
			res.add("travel", "fail", fmt.Sprintf("领取旅行奖励失败: %v", err))
			return travelOutcome{status: "fail", action: "claim", detail: "领取旅行奖励失败"}
		}
		detail := fmt.Sprintf("领取旅行奖励 +%d 积分", reward)
		res.add("travel", "ok", detail)
		return travelOutcome{status: "ok", action: "claim", detail: detail}
	case "idle":
		if ts.DailyLimitReached {
			res.add("travel", "skip", "今日已派出过")
			return travelOutcome{status: "skip", action: "skip", detail: "今日已派出过"}
		}
		if fromChain && justAdopted {
			res.add("travel", "skip", "已领养，下轮巡检派出")
			return travelOutcome{status: "skip", action: "skip", detail: "已领养，下轮巡检派出"}
		}
		if err := s.growth.TravelDepart(travelLocationID); err != nil {
			res.add("travel", "fail", fmt.Sprintf("派出失败: %v", err))
			return travelOutcome{status: "fail", action: "depart", detail: "派出失败"}
		}
		detail := fmt.Sprintf("派出旅行（地点 %d）", travelLocationID)
		res.add("travel", "ok", detail)
		return travelOutcome{status: "ok", action: "depart", detail: detail}
	case "traveling":
		detail := fmt.Sprintf("旅行中（到站后可领奖，记录 %d）", ts.RecordID)
		res.add("travel", "skip", detail)
		return travelOutcome{status: "skip", action: "skip", detail: "旅行中（到站后可领奖）"}
	default:
		res.add("travel", "warn", "未知状态 "+ts.State)
		return travelOutcome{status: "warn", action: "skip", detail: "未知状态 " + ts.State}
	}
}

// ── cron 入口 ──────────────────────────────────────────────────────────────────

func (s *Scheduler) tickGrowthChain() { s.RunGrowthChain() }
func (s *Scheduler) tickTravel()      { s.RunTravelTick() }

// scheduleGrowthCatchup 排一次启动补跑（调用方需持 s.mu）：当日已过上报 cron
// 首个触发时刻、未过深夜窗口、上报未完成 → 30s 后补跑一次链。单账号场景
// 服务器常在白天重启，错过时点不该白丢一天连登。已在排程中的 timer 直接
// 重置（多次保存设置不叠加）；timer 由 s.mu 保护，Stop 时取消。
// 判定只读一条 SQLite，同步执行不影响调度锁持有时长。
func (s *Scheduler) scheduleGrowthCatchup(cfg config.Config) {
	if !s.growthCatchupDue(cfg, time.Now()) {
		return
	}
	if s.growthCatchupTimer != nil {
		s.growthCatchupTimer.Stop()
	}
	s.growthCatchupTimer = time.AfterFunc(growthCatchupDelay, s.growthCatchupFire)
	slog.Info("当日成长上报未完成，稍后补跑一次", "at", time.Now().Format("15:04"))
}

// growthCatchupFire 补跑回调（AfterFunc 独立 goroutine，不持 s.mu）：
// 触发时复查开关与日标志——排程后的 30s 内上报可能已由 cron 链或手动上报完成，
// 复查可避免整条链的冗余上游调用（bonus 是写操作）。链内自带 growthMu 互斥
// 与日内幂等，即使复查后仍与链并发也安全。
func (s *Scheduler) growthCatchupFire() {
	if !s.cfg.Get().AutoGrowth {
		return // 期间已被关闭
	}
	day := upstream.GrowthTodayDate(time.Now())
	if payload, _, ok := s.st.GetCache(growthCacheTable, growthKeyReportDay, growthDayTTL); ok && payload == day {
		return // 今日上报已完成（排程后由 cron/手动链完成）
	}
	res := s.RunGrowthChain()
	if !res.Ran {
		slog.Info("成长补跑跳过", "reason", res.Reason)
	}
}

// growthCatchupDue 补跑条件判定（纯逻辑，便于单测矩阵）：
// region=cn 且已登录；已过当日上报 cron 触发时刻（本地时区口径，与 cron 触发一致，
// 上游 CST 只影响日标志口径）；当日无触发（如仅工作日 cron）不补；超过 22:00 不补；
// 今日上报未完成（cache report_day != 今日 CST）。
func (s *Scheduler) growthCatchupDue(cfg config.Config, now time.Time) bool {
	if cfg.Region != "cn" || s.growth.TokenSummary() == nil {
		return false
	}
	mainAt := fixedMainAt("成长上报", cfg.GrowthReportCron, now)
	if mainAt.IsZero() || now.Before(mainAt) {
		return false
	}
	if now.Hour()*60+now.Minute() > growthCatchupWindowEndMin {
		return false // 深夜不打扰（>22:00）
	}
	day := upstream.GrowthTodayDate(now)
	if payload, _, ok := s.st.GetCache(growthCacheTable, growthKeyReportDay, growthDayTTL); ok && payload == day {
		return false // 今日上报已完成
	}
	return true
}

// ── 编译期断言 ─────────────────────────────────────────────────────────────────

var _ growthClient = (*upstream.Client)(nil)
