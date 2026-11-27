// growth.go 多账号成长任务链编排：活跃上报 → 连登自检 → 领养 → 礼包/补签 → 兑换 → 抽奖 → 旅行巡检。
//
// 幂等语义：按账号 UID 隔离内存态 growthStates[uid] 与 SQLite growth_cache 键
// （report_day:<uid> / adopt_day:<uid> / reward_day:<uid> / last_run:<uid>），
// 每账号独立互斥锁（growthLocks[uid]），不同账号手动触发互不阻塞。
package scheduler

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"buddy2api-go/internal/auth"
	"buddy2api-go/internal/config"
	"buddy2api-go/internal/pool"
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
	growthReportCountMax = config.GrowthReportCountMax
	// growthLotteryMaxDraws 单轮抽奖上限（异常余额保护：避免一次跑上千次写请求）。
	growthLotteryMaxDraws = 50
)

// growthReportGap 同账号内 N 条上报间隔：5 连发模拟同一会话多轮对话，
// 秒发易触发风控，故 1.5s 一条。测试可置 0。
var growthReportGap = 1500 * time.Millisecond

// growthCatchupDelay 启动补跑延迟：留出 HTTP 服务启动时间
// （测试可拉长，避免 Reconfigure 排出的真实 timer 在测试结束后触发）。
var growthCatchupDelay = 30 * time.Second

// growthCacheTable growth 相关缓存的表名与 key 内部别名。
const (
	growthCacheTable   = GrowthCacheTable
	growthKeyReportDay = GrowthKeyReportDay
	growthKeyAdoptDay  = GrowthKeyAdoptDay
	growthKeyRewardDay = GrowthKeyRewardDay
	growthKeyLastRun   = GrowthKeyLastRun
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
	GetCompensationStatus() (upstream.CompensationStatus, error)
}

// ── 结果类型 ──────────────────────────────────────────────────────────────────

// GrowthStep 单步结果。Status ∈ "ok" | "skip" | "warn" | "fail"。
type GrowthStep struct {
	Key    string `json:"key"` // report | streak | adopt | makeup | redeem | lottery | bonus | travel
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"` // 人话摘要（前端直接展示）
}

// GrowthChainResult 成长链单次执行的逐步结果（自动/手动共用，落 last_run:<uid> 缓存 + 返回给前端）。
type GrowthChainResult struct {
	UID    string       `json:"uid,omitempty"`    // 执行目标账号 UID
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
		slog.Warn("growth", "uid", uidTail(r.UID), "step", key, "status", status, "detail", detail)
	} else {
		slog.Info("growth", "uid", uidTail(r.UID), "step", key, "status", status, "detail", detail)
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

// ── 日内状态（内存 + SQLite 双层，按 UID 隔离）──────────────────────────────────

// growthDay 一日内的成长任务状态。day 为 CST 自然日（上游口径），跨日重置。
type growthDay struct {
	day        string // "2006-01-02" CST
	reportDone bool   // 今日 N 条上报已发满
	adoptTried bool   // 今日已尝试领养且门槛未达（防同日多趟轰炸）
	rewardDone bool   // 今日奖励链已处理一轮
}

// growthLock 返回指定 UID 的成长链互斥锁（不同 UID 互不阻塞）。
func (s *Scheduler) growthLock(uid string) *sync.Mutex {
	s.growthLocksMu.Lock()
	defer s.growthLocksMu.Unlock()
	if s.growthLocks == nil {
		s.growthLocks = make(map[string]*sync.Mutex)
	}
	mu := s.growthLocks[uid]
	if mu == nil {
		mu = &sync.Mutex{}
		s.growthLocks[uid] = mu
	}
	return mu
}

// growthStateOf 返回指定 UID 的日内状态指针（调用方需持该 UID 的 growthLock）。
func (s *Scheduler) growthStateOf(uid string) *growthDay {
	s.growthLocksMu.Lock()
	defer s.growthLocksMu.Unlock()
	if s.growthStates == nil {
		s.growthStates = make(map[string]*growthDay)
	}
	st := s.growthStates[uid]
	if st == nil {
		st = &growthDay{}
		s.growthStates[uid] = st
	}
	return st
}

// rollGrowthDayLocked 跨 CST 日时重置该账号内存状态；内存 day 为空（进程刚启动）或
// 跨日时从 cache 镜像回填当日标志（服务重启后日标志不丢，双保险）。
// 调用方需持 s.growthLock(uid)。
func (s *Scheduler) rollGrowthDayLocked(uid string, now time.Time) *growthDay {
	day := upstream.GrowthTodayDate(now)
	st := s.growthStateOf(uid)
	if st.day == day {
		return st
	}
	*st = growthDay{day: day}
	if payload, _, ok := s.st.GetCache(growthCacheTable, GrowthReportDayKey(uid), growthDayTTL); ok && payload == day {
		st.reportDone = true
	}
	if payload, _, ok := s.st.GetCache(growthCacheTable, GrowthAdoptDayKey(uid), growthDayTTL); ok && payload == day {
		st.adoptTried = true
	}
	if payload, _, ok := s.st.GetCache(growthCacheTable, GrowthRewardDayKey(uid), growthDayTTL); ok && payload == day {
		st.rewardDone = true
	}
	return st
}

// growthPreconditionFor 成长类操作前置检查：region=cn、账号已登录且非海外域名。
// 返回非空 reason 表示不可执行（正常态，不发起任何上游调用）。
func (s *Scheduler) growthPreconditionFor(uid string) (schedAccount, string) {
	if s.cfg.Get().Region != "cn" {
		return schedAccount{}, "region_global"
	}
	acct, reason := s.resolveAccount(uid)
	if reason != "" || acct.growth == nil {
		if reason == "" {
			reason = "not_logged_in"
		}
		return schedAccount{}, reason
	}
	t := acct.growth.TokenSummary()
	if t == nil {
		return schedAccount{}, "not_logged_in"
	}
	if t.Domain != "" && t.Domain != config.EndpointOf("cn").Domain {
		return schedAccount{}, "region_global"
	}
	if acct.uid == "" {
		acct.uid = t.UID
	}
	return acct, ""
}

// ── 成长任务链（核心编排）──────────────────────────────────────────────────────

// RunGrowthChain 为当前活动账号执行一次完整成长链（向后兼容入口）。
func (s *Scheduler) RunGrowthChain() GrowthChainResult { return s.RunGrowthChainFor("") }

// RunGrowthChainFor 为指定账号执行一次完整成长链（cron / 手动触发 / 启动补跑共用）。
// 顺序：上报 → 连登自检 → 领养 → 礼包 → 补签+兑换+抽奖 → 旅行巡检。
// 任一步失败不阻断后续步（单步记 fail）。同 UID 互斥：已有链在跑 → 直接返回 busy（不等待）。
func (s *Scheduler) RunGrowthChainFor(uid string) GrowthChainResult {
	acct, reason := s.growthPreconditionFor(uid)
	if reason != "" {
		return GrowthChainResult{UID: uid, Ran: false, Reason: reason}
	}
	return s.runGrowthChainForAccount(acct)
}

func (s *Scheduler) runGrowthChainForAccount(acct schedAccount) GrowthChainResult {
	mu := s.growthLock(acct.uid)
	if !mu.TryLock() {
		return GrowthChainResult{UID: acct.uid, Ran: false, Reason: "busy"}
	}
	defer mu.Unlock()

	now := time.Now()
	st := s.rollGrowthDayLocked(acct.uid, now)
	day := upstream.GrowthTodayDate(now)
	res := GrowthChainResult{UID: acct.uid, Day: day, At: now.Unix(), Ran: true}
	defer func() {
		if raw, err := json.Marshal(res); err == nil {
			_ = s.st.SetCache(growthCacheTable, GrowthLastRunKey(acct.uid), string(raw))
		}
		if st.reportDone {
			_ = s.st.SetCache(growthCacheTable, GrowthReportDayKey(acct.uid), day)
		}
	}()

	cfg := s.cfg.Get()
	count := config.RollReportCount(cfg.GrowthReportCount, cfg.GrowthReportJitter, rand.Int())
	if count <= 0 || count > growthReportCountMax {
		count = 5 // Normalize 已钳制，双保险
	}
	if jitter := cfg.GrowthReportJitter; jitter > 0 {
		slog.Info("growth 上报条数波动", "uid", uidTail(acct.uid), "base", cfg.GrowthReportCount, "jitter", jitter, "actual", count)
	}
	_, reported := s.stepReports(acct, st, &res, count, false)
	var info *upstream.GrowthStreakInfo
	s.stepStreakCheck(acct, &res, reported, &info)
	justAdopted := s.stepAdopt(acct, st, &res, reported) // 上报刚发满 → force=true 豁免今日早前标记
	s.stepBonus(acct, &res)
	s.stepRewardChain(acct, st, &res, &info)
	s.stepTravel(acct, st, &res, true, justAdopted)

	slog.Info("growth 链完成", "uid", uidTail(acct.uid), "day", res.Day, "steps", len(res.Steps))
	return res
}

// RunActivityReports 为当前活动账号执行手动活跃上报（向后兼容入口）。
func (s *Scheduler) RunActivityReports(count int) ActivityReportResult {
	return s.RunActivityReportsFor("", count)
}

// RunActivityReportsFor 为指定账号执行手动活跃上报：不查 reportDone（用户显式意图），发满置 report_day:<uid>。
// count<=0 时取配置条数并按 jitter 波动一次；显式 count 尊重用户指定，不波动。
// 结果钳 1..10；同 UID 互斥（与自动链防并发秒发）。
func (s *Scheduler) RunActivityReportsFor(uid string, count int) ActivityReportResult {
	acct, reason := s.growthPreconditionFor(uid)
	if reason != "" {
		return ActivityReportResult{Ran: false, Reason: reason}
	}
	if count <= 0 {
		cfg := s.cfg.Get()
		count = config.RollReportCount(cfg.GrowthReportCount, cfg.GrowthReportJitter, rand.Int())
	}
	if count > growthReportCountMax {
		count = growthReportCountMax
	}
	mu := s.growthLock(acct.uid)
	if !mu.TryLock() {
		return ActivityReportResult{Ran: false, Reason: "busy"}
	}
	defer mu.Unlock()

	now := time.Now()
	st := s.rollGrowthDayLocked(acct.uid, now)
	res := GrowthChainResult{UID: acct.uid, Day: upstream.GrowthTodayDate(now), Ran: true}
	sent, _ := s.stepReports(acct, st, &res, count, true)
	detail := ""
	if stp, ok := res.findStep("report"); ok {
		detail = stp.Detail
	}
	return ActivityReportResult{Ran: true, Sent: sent, Total: count, Detail: detail}
}

// RunTravelTick 为当前活动账号执行独立旅行巡检（向后兼容入口）。
func (s *Scheduler) RunTravelTick() TravelTickResult { return s.RunTravelTickFor("") }

// RunTravelTickFor 为指定账号执行独立旅行巡检（cron / 手动按钮）：单动作状态机（arrived 先领奖，
// idle 才派出）。不做领养——领养归成长链与手动按钮，避免与链内 adopt 防抖交叉，无猫仅记 skip。
func (s *Scheduler) RunTravelTickFor(uid string) TravelTickResult {
	acct, reason := s.growthPreconditionFor(uid)
	if reason != "" {
		return TravelTickResult{Ran: false, Reason: reason}
	}
	return s.runTravelTickForAccount(acct)
}

func (s *Scheduler) runTravelTickForAccount(acct schedAccount) TravelTickResult {
	res := &GrowthChainResult{UID: acct.uid, Day: upstream.GrowthTodayDate(time.Now()), Ran: true}
	outcome := s.stepTravel(acct, nil, res, false, false)
	return TravelTickResult{Ran: true, Status: outcome.status, Action: outcome.action, Detail: outcome.detail}
}

// RunGrowthAdopt 为当前活动账号执行手动领养（向后兼容入口）。
func (s *Scheduler) RunGrowthAdopt() GrowthAdoptResult { return s.RunGrowthAdoptFor("") }

// RunGrowthAdoptFor 为指定账号执行手动领养（force 语义：豁免今日 adopt_day 防抖）。
// 门槛未达时置 adopt_day:<uid>（当日 cron 链不再重试）。
func (s *Scheduler) RunGrowthAdoptFor(uid string) GrowthAdoptResult {
	acct, reason := s.growthPreconditionFor(uid)
	if reason != "" {
		return GrowthAdoptResult{Ran: false, Reason: reason}
	}
	mu := s.growthLock(acct.uid)
	if !mu.TryLock() {
		return GrowthAdoptResult{Ran: false, Reason: "busy"}
	}
	defer mu.Unlock()

	now := time.Now()
	st := s.rollGrowthDayLocked(acct.uid, now)
	res := &GrowthChainResult{UID: acct.uid, Day: upstream.GrowthTodayDate(now), Ran: true}
	s.stepAdopt(acct, st, res, true)
	out := GrowthAdoptResult{Ran: true}
	if stp, ok := res.findStep("adopt"); ok {
		out.Status, out.Detail = stp.Status, stp.Detail
	}
	return out
}

// RunGrowthRedeem 为当前活动账号执行手动兑换（向后兼容入口）。
func (s *Scheduler) RunGrowthRedeem(tier string) GrowthRedeemActionResult {
	return s.RunGrowthRedeemFor("", tier)
}

// RunGrowthRedeemFor 为指定账号执行手动兑换指定档位。仅成功时置 reward_day:<uid>（skip/fail 不置，
// 用户可换档重试）；正常态（409 已领/403 天数不足）识别为 skip。
func (s *Scheduler) RunGrowthRedeemFor(uid string, tier string) GrowthRedeemActionResult {
	acct, reason := s.growthPreconditionFor(uid)
	if reason != "" {
		return GrowthRedeemActionResult{Ran: false, Reason: reason}
	}
	mu := s.growthLock(acct.uid)
	if !mu.TryLock() {
		return GrowthRedeemActionResult{Ran: false, Reason: "busy"}
	}
	defer mu.Unlock()

	now := time.Now()
	st := s.rollGrowthDayLocked(acct.uid, now)
	day := upstream.GrowthTodayDate(now)
	out := GrowthRedeemActionResult{Ran: true}
	r, err := acct.growth.GrowthRedeem(tier, "")
	switch {
	case err == nil:
		st.rewardDone = true
		_ = s.st.SetCache(growthCacheTable, GrowthRewardDayKey(acct.uid), day)
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
// 返回已发条数与是否发满；发满置 reportDone + cache。调用方需持该账号 growthLock。
func (s *Scheduler) stepReports(acct schedAccount, st *growthDay, res *GrowthChainResult, count int, ignoreDone bool) (sent int, full bool) {
	if !ignoreDone && st.reportDone {
		res.add("report", "skip", "今日已完成")
		return 0, false
	}
	cid := fmt.Sprintf("b2a-%d", time.Now().UnixMilli())
	for i := 1; i <= count; i++ {
		rid := fmt.Sprintf("%s-r%d", cid, i)
		if err := acct.growth.ReportChatActivity(cid, rid); err != nil {
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
	st.reportDone = true
	_ = s.st.SetCache(growthCacheTable, GrowthReportDayKey(acct.uid), res.Day)
	// 上报补满可能解锁 chat_5 门槛：清掉今日领养防抖，让后续领养尝试
	// （链内 force 或手动按钮）值得再试；若门槛仍未达，下次失败会重新置位。
	if st.adoptTried {
		st.adoptTried = false
		_ = s.st.DeleteCacheKey(growthCacheTable, GrowthAdoptDayKey(acct.uid))
	}
	res.add("report", "ok", fmt.Sprintf("已上报 %d 条（同会话 %s）", count, cid))
	return sent, true
}

// stepStreakCheck 连登回读自检（只读 oracle）：上报发满才执行。
// days==0 → warn（上报 200 但静默丢弃）；回读失败 → warn 不重试。
// 成功时把快照存入 pInfo 供奖励链复用（免二次 GET）。
func (s *Scheduler) stepStreakCheck(acct schedAccount, res *GrowthChainResult, reported bool, pInfo **upstream.GrowthStreakInfo) {
	if !reported {
		res.add("streak", "skip", "本轮上报未发满，跳过连登自检")
		return
	}
	info, err := acct.growth.GrowthStreakInfo()
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
// 「刚领养，本轮不派出」。调用方需持该账号 growthLock。
func (s *Scheduler) stepAdopt(acct schedAccount, st *growthDay, res *GrowthChainResult, force bool) bool {
	if !force && st != nil && st.adoptTried {
		res.add("adopt", "skip", "今日已尝试领养（门槛未达），明日自动重试")
		return false
	}
	buddy, err := acct.growth.BuddyInfo()
	if err != nil {
		res.add("adopt", "fail", "查询猫档案失败: "+err.Error())
		return false
	}
	if buddy != nil {
		res.add("adopt", "skip", "已有猫 "+buddy.Name)
		return false
	}
	if err := acct.growth.BuddyAgreement(); err != nil { // 幂等
		res.add("adopt", "fail", "同意协议失败: "+err.Error())
		return false
	}
	err = acct.growth.BuddyFirst()
	switch {
	case err == nil:
		res.add("adopt", "ok", "领养成功（+300 积分）")
		return true
	case upstream.IsBuddyTaskIncomplete(err):
		// 门槛未达（chat_5）：当日不重试轰炸，置日标志；明日链上报后自动重试
		if st != nil {
			st.adoptTried = true
		}
		_ = s.st.SetCache(growthCacheTable, GrowthAdoptDayKey(acct.uid), res.Day)
		res.add("adopt", "skip", "对话门槛未达（chat_5），明日上报后自动重试")
		return false
	default:
		res.add("adopt", "fail", "领养失败: "+err.Error())
		return false
	}
}

// stepBonus 礼包 + 补偿领取（幂等，有则领）。
func (s *Scheduler) stepBonus(acct schedAccount, res *GrowthChainResult) {
	var detail []string
	if credit, err := acct.growth.ClaimGift(); err == nil && credit > 0 {
		detail = append(detail, fmt.Sprintf("礼包 +%d 积分", credit))
	}
	s.claimCompensationQuiet(acct, res, &detail)
	if len(detail) > 0 {
		res.add("bonus", "ok", strings.Join(detail, " / "))
	}
}

// claimCompensationQuiet 补偿领取的可观测化版本：先查状态，待领才领。
func (s *Scheduler) claimCompensationQuiet(acct schedAccount, res *GrowthChainResult, detail *[]string) {
	st, err := acct.growth.GetCompensationStatus()
	if err != nil {
		res.add("compensation", "warn", "补偿状态查询失败: "+err.Error())
		return
	}
	if !st.Pending() {
		return // 无活动 / 已领 / 零额度：正常态
	}
	credit, cerr := acct.growth.ClaimCompensation()
	if cerr != nil {
		msg := "补偿待领但领取失败（截止 " + orDefaultStr(st.EndTime, "未知") + "）: " + cerr.Error()
		res.add("compensation", "fail", msg)
		return
	}
	if credit > 0 {
		*detail = append(*detail, fmt.Sprintf("补偿 +%d 积分", credit))
	} else {
		*detail = append(*detail, "补偿已领取")
	}
}

// orDefaultStr 空字符串回落默认值。
func orDefaultStr(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

// stepRewardChain 补签 + 兑换 + 抽奖（每日一轮）。调用方需持该账号 growthLock。
func (s *Scheduler) stepRewardChain(acct schedAccount, st *growthDay, res *GrowthChainResult, pInfo **upstream.GrowthStreakInfo) {
	if st != nil && st.rewardDone {
		res.add("redeem", "skip", "今日已处理奖励链")
		return
	}
	if *pInfo == nil {
		info, err := acct.growth.GrowthStreakInfo()
		if err != nil {
			res.add("redeem", "warn", "连登状态读取失败: "+err.Error())
			return
		}
		*pInfo = info
	}
	if s.makeupYesterday(acct, res, *pInfo) {
		// 补签成功 → 重读 streak（天数可能恢复到新档位）
		if info2, err := acct.growth.GrowthStreakInfo(); err == nil {
			*pInfo = info2
		}
	}
	tiers := (*pInfo).EligibleTiers()
	if len(tiers) == 0 {
		res.add("redeem", "skip", "无可领档位（未达标或已领）")
		if st != nil {
			st.rewardDone = true
		}
		_ = s.st.SetCache(growthCacheTable, GrowthRewardDayKey(acct.uid), res.Day)
		return
	}
	var granted []string
	for _, tier := range tiers {
		r, err := acct.growth.GrowthRedeem(tier, "")
		switch {
		case err == nil:
			granted = append(granted, fmt.Sprintf("%s(+%dc +%de 卡×%d 抽奖×%d)",
				tier, r.CreditGranted, r.EnergyGranted, r.CardsGranted, r.ChancesGranted))
		case upstream.IsRedeemAlreadyClaimed(err) || upstream.IsRedeemNotEnoughDays(err):
			continue // 该档已领 / 天数不足：正常态
		default:
			res.add("redeem", "fail", "兑换 "+tier+" 失败: "+err.Error())
		}
	}
	if len(granted) > 0 {
		res.add("redeem", "ok", "兑换 "+strings.Join(granted, " / "))
	} else {
		res.add("redeem", "skip", "已解锁档位已全部领取")
	}
	if st != nil {
		st.rewardDone = true
	}
	_ = s.st.SetCache(growthCacheTable, GrowthRewardDayKey(acct.uid), res.Day)
	s.stepLottery(acct, res)
}

// makeupYesterday 昨日漏签且有补签卡时自动补签（保住连登连续天数）。
func (s *Scheduler) makeupYesterday(acct schedAccount, res *GrowthChainResult, info *upstream.GrowthStreakInfo) bool {
	cells, err := acct.growth.GrowthHeatmap()
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
	if err := acct.growth.UseMakeupCard(yesterday); err != nil {
		res.add("makeup", "warn", "补签 "+yesterday+" 失败（次日再判）: "+err.Error())
		return false
	}
	res.add("makeup", "ok", fmt.Sprintf("补签 %s（连登保住）", yesterday))
	return true
}

// stepLottery 抽奖（自动链：把 chances 抽完）。
func (s *Scheduler) stepLottery(acct schedAccount, res *GrowthChainResult) {
	chances, err := acct.growth.GrowthLotteryChances()
	if err != nil {
		res.add("lottery", "warn", "抽奖次数查询失败: "+err.Error())
		return
	}
	if chances <= 0 {
		res.add("lottery", "skip", "无抽奖次数")
		return
	}
	if chances > growthLotteryMaxDraws {
		chances = growthLotteryMaxDraws // 上限保护：避免异常余额导致一次跑上千次写请求
	}
	var wins []string
	var creditTotal int64
	for i := 0; i < chances; i++ {
		draw, err := acct.growth.GrowthLotteryDraw("")
		switch {
		case err == nil:
			switch draw.PrizeType {
			case "credit":
				creditTotal += int64(draw.CreditAmount)
				wins = append(wins, fmt.Sprintf("%s(+%d)", draw.PrizeName, draw.CreditAmount))
			case "physical":
				wins = append(wins, draw.PrizeName+"(实物，请在官方渠道填写地址)")
			default:
				wins = append(wins, draw.PrizeName)
			}
		case upstream.IsLotteryNoChance(err) || upstream.IsLotteryDisabled(err):
			// 次数用尽 / 活动未开启：结束本轮（已抽到的结果照常汇报）
			i = chances
		default:
			res.add("lottery", "fail", "抽奖失败: "+err.Error())
			i = chances
		}
	}
	if len(wins) == 0 {
		res.add("lottery", "skip", "无抽奖次数或未开启")
		return
	}
	detail := fmt.Sprintf("抽奖 %d 次：%s", len(wins), strings.Join(wins, " / "))
	if creditTotal > 0 {
		detail += fmt.Sprintf("（共 +%d 积分）", creditTotal)
	}
	res.add("lottery", "ok", detail)
}

// travelOutcome 旅行单趟结果（内部传递用）。
type travelOutcome struct {
	status string // ok | skip | warn | fail
	action string // claim | depart | skip
	detail string
}

// stepTravel 旅行巡检（单动作状态机，不轮询不等待）。
func (s *Scheduler) stepTravel(acct schedAccount, st *growthDay, res *GrowthChainResult, fromChain, justAdopted bool) travelOutcome {
	buddy, err := acct.growth.BuddyInfo()
	if err != nil {
		res.add("travel", "fail", "查询猫档案失败: "+err.Error())
		return travelOutcome{status: "fail", action: "skip", detail: "查询猫档案失败"}
	}
	if buddy == nil {
		if fromChain {
			s.stepAdopt(acct, st, res, false)
			stp, _ := res.findStep("adopt")
			return travelOutcome{status: stp.Status, action: "skip", detail: stp.Detail}
		}
		res.add("travel", "skip", "尚未领养猫咪（领养在成长链/手动触发）")
		return travelOutcome{status: "skip", action: "skip", detail: "尚未领养猫咪"}
	}
	ts, err := acct.growth.TravelStatus()
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
		reward, err := acct.growth.TravelClaim(ts.RecordID)
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
		if err := acct.growth.TravelDepart(travelLocationID); err != nil {
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

// ── cron 入口（多账号串行遍历）──────────────────────────────────────────────────

func (s *Scheduler) tickGrowthChain() {
	if s.cfg.Get().Region != "cn" {
		return
	}
	accts := s.eligibleAccounts()
	for i, acct := range accts {
		if acct.growth == nil {
			continue
		}
		s.runGrowthChainForAccount(acct)
		if i < len(accts)-1 && multiAccountGap > 0 {
			time.Sleep(multiAccountGap)
		}
	}
}

func (s *Scheduler) tickTravel() {
	if s.cfg.Get().Region != "cn" {
		return
	}
	accts := s.eligibleAccounts()
	for i, acct := range accts {
		if acct.growth == nil {
			continue
		}
		s.runTravelTickForAccount(acct)
		if i < len(accts)-1 && multiAccountGap > 0 {
			time.Sleep(multiAccountGap)
		}
	}
}

// scheduleGrowthCatchupLocked 排一次启动补跑（调用方需持 s.mu）：当日已过上报 cron
// 首个触发时刻、未过深夜窗口、任意合规账号上报未完成 → 30s 后补跑一次。
func (s *Scheduler) scheduleGrowthCatchupLocked(cfg config.Config) {
	if !s.growthCatchupDueWithPool(cfg, time.Now(), s.pool) {
		return
	}
	if s.growthCatchupTimer != nil {
		s.growthCatchupTimer.Stop()
	}
	s.growthCatchupTimer = time.AfterFunc(growthCatchupDelay, s.growthCatchupFire)
	slog.Info("当日存在成长上报未完成账号，稍后补跑一次", "at", time.Now().Format("15:04"))
}

// growthCatchupFire 补跑回调（AfterFunc 独立 goroutine，不持 s.mu）：
// 复查开关后，为所有今日尚未完成 report_day:<uid> 的合规 CN 账号逐一补跑。
func (s *Scheduler) growthCatchupFire() {
	cfg := s.cfg.Get()
	if !cfg.AutoGrowth || cfg.Region != "cn" {
		return // 期间已被关闭或切到 global
	}
	day := upstream.GrowthTodayDate(time.Now())
	accts := s.eligibleAccounts()
	var pending []schedAccount
	for _, acct := range accts {
		if acct.growth == nil {
			continue
		}
		if payload, _, ok := s.st.GetCache(growthCacheTable, GrowthReportDayKey(acct.uid), growthDayTTL); ok && payload == day {
			continue
		}
		pending = append(pending, acct)
	}
	for i, acct := range pending {
		res := s.runGrowthChainForAccount(acct)
		if !res.Ran {
			slog.Info("成长补跑跳过", "uid", uidTail(acct.uid), "reason", res.Reason)
		}
		if i < len(pending)-1 && multiAccountGap > 0 {
			time.Sleep(multiAccountGap)
		}
	}
}

// growthCatchupDue 补跑条件判定（供单测调用）。
func (s *Scheduler) growthCatchupDue(cfg config.Config, now time.Time) bool {
	return s.growthCatchupDueWithPool(cfg, now, s.getPool())
}

// growthCatchupDueWithPool 补跑条件判定：region=cn、已过当日上报时点、未超 22:00、
// 且至少有一个合规账号尚未完成今日 report_day:<uid>。
func (s *Scheduler) growthCatchupDueWithPool(cfg config.Config, now time.Time, p *pool.Manager) bool {
	if cfg.Region != "cn" {
		return false
	}
	mainAt := fixedMainAt("成长上报", cfg.GrowthReportCron, now)
	if mainAt.IsZero() || now.Before(mainAt) {
		return false
	}
	if now.Hour()*60+now.Minute() > growthCatchupWindowEndMin {
		return false // 深夜不打扰（>22:00）
	}
	accts := s.eligibleAccountsWithPool(p)
	if len(accts) == 0 {
		return false
	}
	day := upstream.GrowthTodayDate(now)
	for _, acct := range accts {
		if payload, _, ok := s.st.GetCache(growthCacheTable, GrowthReportDayKey(acct.uid), growthDayTTL); !ok || payload != day {
			return true
		}
	}
	return false
}

// ── 编译期断言 ─────────────────────────────────────────────────────────────────

var _ growthClient = (*upstream.Client)(nil)
