package scheduler

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/robfig/cron/v3"

	"buddy2api-go/internal/auth"
	"buddy2api-go/internal/config"
	"buddy2api-go/internal/store"
	"buddy2api-go/internal/upstream"
)

// ── fake 成长客户端（脚本化响应 + 调用记录）────────────────────────────────────

type fakeGrowth struct {
	token *auth.Token // nil = 未登录

	reportCalls int // ReportChatActivity 调用次数
	reportErrOn int // 第 N 条上报失败（1 起；0 = 不失败）

	streakSeq []*upstream.GrowthStreakInfo // 依次返回的快照（耗尽后用最后一个）
	streakErr error
	streakN   int

	heatmap  []upstream.HeatmapCell
	makeupN  int      // UseMakeupCard 调用次数
	makeupTo []string // 调用参数记录
	makeupOK bool     // makeup 是否成功

	redeemTiers  []string // GrowthRedeem 调用的 tier 记录
	redeemResult *upstream.GrowthRedeemResult
	redeemErr    error

	chances    int
	chancesErr error
	drawN      int
	drawResult *upstream.GrowthLotteryDrawResult
	drawErr    error

	buddy        *upstream.Buddy
	buddyErr     error
	agreementN   int
	firstN       int
	firstErr     error
	firstErrOnce bool // firstErr 只在第一次 BuddyFirst 返回（模拟领养成功后状态变化）

	travelState *upstream.TravelState
	travelErr   error
	departN     int
	departErr   error
	claimIDs    []int64
	claimReward int64
	claimErr    error

	giftCredit int64
	compCredit int64
}

func (f *fakeGrowth) TokenSummary() *auth.Token { return f.token }

func (f *fakeGrowth) ReportChatActivity(cid, rid string) error {
	f.reportCalls++
	if f.reportErrOn != 0 && f.reportCalls >= f.reportErrOn {
		return errors.New("report rejected")
	}
	return nil
}

func (f *fakeGrowth) GrowthStreakInfo() (*upstream.GrowthStreakInfo, error) {
	if f.streakErr != nil {
		return nil, f.streakErr
	}
	if f.streakN < len(f.streakSeq)-1 {
		f.streakN++
		return f.streakSeq[f.streakN-1], nil
	}
	f.streakN++
	if len(f.streakSeq) == 0 {
		return &upstream.GrowthStreakInfo{}, nil
	}
	return f.streakSeq[len(f.streakSeq)-1], nil
}

func (f *fakeGrowth) GrowthHeatmap() ([]upstream.HeatmapCell, error) { return f.heatmap, nil }

func (f *fakeGrowth) UseMakeupCard(date string) error {
	f.makeupN++
	f.makeupTo = append(f.makeupTo, date)
	if !f.makeupOK {
		return &upstream.GrowthError{Status: 200, Code: 1001, Msg: "no card"}
	}
	return nil
}

func (f *fakeGrowth) GrowthRedeem(tier, token string) (*upstream.GrowthRedeemResult, error) {
	f.redeemTiers = append(f.redeemTiers, tier)
	if f.redeemErr != nil {
		return nil, f.redeemErr
	}
	if f.redeemResult != nil {
		return f.redeemResult, nil
	}
	return &upstream.GrowthRedeemResult{CreditGranted: 80, EnergyGranted: 5, CardsGranted: 1, ChancesGranted: 1}, nil
}

func (f *fakeGrowth) GrowthLotteryChances() (int, error) {
	if f.chancesErr != nil {
		return 0, f.chancesErr
	}
	return f.chances, nil
}

func (f *fakeGrowth) GrowthLotteryDraw(token string) (*upstream.GrowthLotteryDrawResult, error) {
	f.drawN++
	if f.drawErr != nil {
		return nil, f.drawErr
	}
	if f.drawResult != nil {
		return f.drawResult, nil
	}
	return &upstream.GrowthLotteryDrawResult{PrizeName: "6积分", PrizeType: "credit", CreditAmount: 6}, nil
}

func (f *fakeGrowth) BuddyInfo() (*upstream.Buddy, error) {
	if f.buddyErr != nil {
		return nil, f.buddyErr
	}
	return f.buddy, nil
}

func (f *fakeGrowth) BuddyAgreement() error {
	f.agreementN++
	return nil
}

func (f *fakeGrowth) BuddyFirst() error {
	f.firstN++
	if f.firstErr != nil {
		if f.firstErrOnce && f.firstN > 1 {
			f.buddy = &upstream.Buddy{ID: 1, Name: "喵喵"} // 重试成功后有猫
			return nil
		}
		return f.firstErr
	}
	f.buddy = &upstream.Buddy{ID: 1, Name: "喵喵"} // 领养成功后有猫（供后续 BuddyInfo 读到）
	return nil
}

func (f *fakeGrowth) TravelStatus() (*upstream.TravelState, error) {
	if f.travelErr != nil {
		return nil, f.travelErr
	}
	if f.travelState != nil {
		return f.travelState, nil
	}
	return &upstream.TravelState{State: "idle"}, nil
}

func (f *fakeGrowth) TravelDepart(loc int) error {
	f.departN++
	return f.departErr
}

func (f *fakeGrowth) TravelClaim(recordID int64) (int64, error) {
	f.claimIDs = append(f.claimIDs, recordID)
	if f.claimErr != nil {
		return 0, f.claimErr
	}
	return f.claimReward, nil
}

func (f *fakeGrowth) ClaimGift() (int64, error) { return f.giftCredit, nil }

func (f *fakeGrowth) ClaimCompensation() (int64, error) { return f.compCredit, nil }

// ── 测试基建 ─────────────────────────────────────────────────────────────────

func newTestScheduler(t *testing.T, region string, fake *fakeGrowth) (*Scheduler, *store.Store, *config.Manager) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if region != "cn" {
		if err := cfg.Update(func(c *config.Config) error { c.Region = region; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if fake.token == nil {
		fake.token = &auth.Token{UID: "test-uid", AccessToken: "t"} // 默认已登录
	}
	s := &Scheduler{
		c:      cron.New(cron.WithParser(secondsParser), cron.WithChain()),
		cfg:    cfg,
		client: nil,
		st:     st,
		growth: fake,
	}
	return s, st, cfg
}

func stepOf(res *GrowthChainResult, key string) *GrowthStep {
	for i := range res.Steps {
		if res.Steps[i].Key == key {
			return &res.Steps[i]
		}
	}
	return nil
}

func mustDay(t *testing.T) string {
	t.Helper()
	return upstream.GrowthTodayDate(time.Now())
}

// TestGrowthChainReportFailChain 上报第 3 条失败：report fail、streak 跳过、
// 后续步照跑（bonus/redeem/travel 仍执行）。
func TestGrowthChainReportFailChain(t *testing.T) {
	origGap := growthReportGap
	growthReportGap = 0
	t.Cleanup(func() { growthReportGap = origGap })

	fake := &fakeGrowth{reportErrOn: 3, chances: 1, buddy: &upstream.Buddy{ID: 1, Name: "喵喵"}}
	s, st, _ := newTestScheduler(t, "cn", fake)
	res := s.RunGrowthChain()

	if !res.Ran {
		t.Fatalf("链应执行: %+v", res)
	}
	if stp := stepOf(&res, "report"); stp == nil || stp.Status != "fail" || stp.Detail == "" {
		t.Errorf("report 步应 fail 且带详情，得到 %+v", stp)
	}
	if fake.reportCalls != 3 {
		t.Errorf("第 3 条失败后应停止上报，调用 %d 次", fake.reportCalls)
	}
	if stp := stepOf(&res, "streak"); stp == nil || stp.Status != "skip" {
		t.Errorf("上报未发满 streak 应跳过，得到 %+v", stp)
	}
	// 后续步照跑
	if stp := stepOf(&res, "redeem"); stp == nil {
		t.Error("奖励链应照跑")
	}
	if stp := stepOf(&res, "travel"); stp == nil {
		t.Error("旅行步应照跑")
	}
	// 失败不置 report_day
	if payload, _, ok := st.GetCache(growthCacheTable, growthKeyReportDay, growthDayTTL); ok && payload == mustDay(t) {
		t.Error("上报未发满不应置 report_day")
	}
}

// TestGrowthChainStreakZeroWarn 上报发满但 days=0 → streak warn（静默丢弃信号）。
func TestGrowthChainStreakZeroWarn(t *testing.T) {
	origGap := growthReportGap
	growthReportGap = 0
	t.Cleanup(func() { growthReportGap = origGap })

	info := &upstream.GrowthStreakInfo{}
	info.Streak.Days = 0
	fake := &fakeGrowth{streakSeq: []*upstream.GrowthStreakInfo{info}}
	s, _, _ := newTestScheduler(t, "cn", fake)
	res := s.RunGrowthChain()

	if stp := stepOf(&res, "report"); stp == nil || stp.Status != "ok" {
		t.Errorf("report 应 ok，得到 %+v", stp)
	}
	if stp := stepOf(&res, "streak"); stp == nil || stp.Status != "warn" {
		t.Errorf("days=0 应 warn，得到 %+v", stp)
	}
}

// TestGrowthChainAdoptThresholdAndDailyDedup 门槛未达 → adopt_day 置位；
// 同日再跑链 → 领养防抖生效（不再调 BuddyFirst）；跨日重置后重试成功。
func TestGrowthChainAdoptThresholdAndDailyDedup(t *testing.T) {
	origGap := growthReportGap
	growthReportGap = 0
	t.Cleanup(func() { growthReportGap = origGap })

	fake := &fakeGrowth{
		firstErr:     &upstream.GrowthError{Status: 400, Msg: "first_buddy task not completed yet"},
		firstErrOnce: true, // 第二次调用（跨日后）成功
	}
	s, st, _ := newTestScheduler(t, "cn", fake)

	// 第 1 趟：门槛未达 → skip + adopt_day 置位
	res := s.RunGrowthChain()
	if stp := stepOf(&res, "adopt"); stp == nil || stp.Status != "skip" {
		t.Errorf("门槛未答应 skip，得到 %+v", stp)
	}
	if fake.firstN != 1 {
		t.Fatalf("BuddyFirst 应调用 1 次，得到 %d", fake.firstN)
	}
	if payload, _, ok := st.GetCache(growthCacheTable, growthKeyAdoptDay, growthDayTTL); !ok || payload != mustDay(t) {
		t.Errorf("adopt_day 应置今日，得到 ok=%v payload=%q", ok, payload)
	}

	// 第 2 趟（同日）：防抖生效，不再调 BuddyFirst
	res = s.RunGrowthChain()
	if stp := stepOf(&res, "adopt"); stp == nil || stp.Status != "skip" {
		t.Errorf("同日应防抖 skip，得到 %+v", stp)
	}
	if fake.firstN != 1 {
		t.Errorf("同日不应再调 BuddyFirst，得到 %d 次", fake.firstN)
	}

	// 模拟真正跨 CST 日：清掉今日日标志缓存 + 内存 day 重置 → 全新一天
	// （fake 的 firstErrOnce 让第二次 BuddyFirst 成功）
	_ = st.DeleteCacheKey(growthCacheTable, growthKeyReportDay)
	_ = st.DeleteCacheKey(growthCacheTable, growthKeyAdoptDay)
	_ = st.DeleteCacheKey(growthCacheTable, growthKeyRewardDay)
	s.growthMu.Lock()
	s.growthState = growthDay{}
	s.growthMu.Unlock()
	res = s.RunGrowthChain()
	if stp := stepOf(&res, "adopt"); stp == nil || stp.Status != "ok" {
		t.Errorf("跨日重试应领养成功，得到 %+v", stp)
	}
}

// TestGrowthChainMakeupRefillTier 补签成功 → 重读 streak 挑档：首读 days=6（不够 7d），
// 补签后 days=7 → 兑换 7d。
func TestGrowthChainMakeupRefillTier(t *testing.T) {
	origGap := growthReportGap
	growthReportGap = 0
	t.Cleanup(func() { growthReportGap = origGap })

	info1 := &upstream.GrowthStreakInfo{}
	info1.Streak.Days = 6
	info1.MakeupCards.Balance = 1
	info1.Redemption.Tiers = []upstream.GrowthTierSpec{{Tier: "7d", Days: 7, Credit: 80}}
	info2 := &upstream.GrowthStreakInfo{}
	info2.Streak.Days = 7
	info2.MakeupCards.Balance = 0
	info2.Redemption.Tiers = []upstream.GrowthTierSpec{{Tier: "7d", Days: 7, Credit: 80}}

	yesterday := upstream.GrowthYesterdayDate(time.Now())
	fake := &fakeGrowth{
		streakSeq: []*upstream.GrowthStreakInfo{info1, info2},
		heatmap:   []upstream.HeatmapCell{{Date: yesterday, Score: 0}}, // 昨日漏签
		makeupOK:  true,
		chances:   2,
	}
	s, _, _ := newTestScheduler(t, "cn", fake)
	res := s.RunGrowthChain()

	if stp := stepOf(&res, "makeup"); stp == nil || stp.Status != "ok" {
		t.Errorf("补签应 ok，得到 %+v", stp)
	}
	if fake.makeupN != 1 || fake.makeupTo[0] != yesterday {
		t.Errorf("应补昨日 %s，得到 %v", yesterday, fake.makeupTo)
	}
	if fake.streakN != 2 {
		t.Errorf("补签成功应重读 streak（共 2 次读取），得到 %d", fake.streakN)
	}
	if len(fake.redeemTiers) != 1 || fake.redeemTiers[0] != "7d" {
		t.Errorf("应兑换 7d，得到 %v", fake.redeemTiers)
	}
	if stp := stepOf(&res, "redeem"); stp == nil || stp.Status != "ok" {
		t.Errorf("redeem 应 ok，得到 %+v", stp)
	}
	if stp := stepOf(&res, "lottery"); stp == nil || stp.Status != "ok" {
		t.Errorf("兑换送 chances 后应抽奖，得到 %+v", stp)
	}
	if fake.drawN != 1 {
		t.Errorf("自动链应只抽 1 次，得到 %d", fake.drawN)
	}
}

// TestGrowthChainRewardDailyOnce 奖励链每日一轮：第 2 趟同日 skip。
func TestGrowthChainRewardDailyOnce(t *testing.T) {
	origGap := growthReportGap
	growthReportGap = 0
	t.Cleanup(func() { growthReportGap = origGap })

	info := &upstream.GrowthStreakInfo{}
	info.Streak.Days = 7
	info.Redemption.Tiers = []upstream.GrowthTierSpec{{Tier: "7d", Days: 7}}
	fake := &fakeGrowth{streakSeq: []*upstream.GrowthStreakInfo{info}}
	s, st, _ := newTestScheduler(t, "cn", fake)

	s.RunGrowthChain()
	if payload, _, ok := st.GetCache(growthCacheTable, growthKeyRewardDay, growthDayTTL); !ok || payload != mustDay(t) {
		t.Errorf("reward_day 应置今日，得到 ok=%v payload=%q", ok, payload)
	}
	res := s.RunGrowthChain()
	if stp := stepOf(&res, "redeem"); stp == nil || stp.Status != "skip" {
		t.Errorf("同日第二趟应 skip 奖励链，得到 %+v", stp)
	}
	if len(fake.redeemTiers) != 1 {
		t.Errorf("同日不应重复兑换，得到 %v", fake.redeemTiers)
	}
}

// TestGrowthChainBusyMutex 互斥占用 → busy（不等待）。
func TestGrowthChainBusyMutex(t *testing.T) {
	fake := &fakeGrowth{}
	s, _, _ := newTestScheduler(t, "cn", fake)
	s.growthMu.Lock()
	res := s.RunGrowthChain()
	s.growthMu.Unlock()
	if res.Ran || res.Reason != "busy" {
		t.Errorf("互斥占用应返回 busy，得到 %+v", res)
	}
	if fake.reportCalls != 0 {
		t.Error("busy 时不应发任何上报")
	}
}

// TestGrowthChainRegionGlobal region=global：跳过且零上游调用。
func TestGrowthChainRegionGlobal(t *testing.T) {
	fake := &fakeGrowth{}
	s, _, _ := newTestScheduler(t, "global", fake)
	res := s.RunGrowthChain()
	if res.Ran || res.Reason != "region_global" {
		t.Errorf("global 应跳过，得到 %+v", res)
	}
	if fake.reportCalls+fake.streakN+fake.firstN+fake.departN+fake.drawN > 0 {
		t.Error("global 不应发起任何上游调用")
	}
	// 未登录同理（newTestScheduler 会补默认登录，构造后手动清空）
	fake2 := &fakeGrowth{}
	s2, _, _ := newTestScheduler(t, "cn", fake2)
	fake2.token = nil
	res = s2.RunGrowthChain()
	if res.Ran || res.Reason != "not_logged_in" {
		t.Errorf("未登录应跳过，得到 %+v", res)
	}
}

// TestGrowthChainCrossDayReset 跨 CST 日状态重置：昨日 done 标志今日清零。
func TestGrowthChainCrossDayReset(t *testing.T) {
	origGap := growthReportGap
	growthReportGap = 0
	t.Cleanup(func() { growthReportGap = origGap })

	fake := &fakeGrowth{}
	s, _, _ := newTestScheduler(t, "cn", fake)
	// 人为置昨日状态：全部 done
	s.growthMu.Lock()
	s.growthState = growthDay{day: "2000-01-01", reportDone: true, adoptTried: true, rewardDone: true}
	s.growthMu.Unlock()

	res := s.RunGrowthChain()
	if s.growthState.day != mustDay(t) {
		t.Errorf("跨日后应重置为今日，得到 %q", s.growthState.day)
	}
	if stp := stepOf(&res, "report"); stp == nil || stp.Status != "ok" {
		t.Errorf("跨日后上报应重新执行，得到 %+v", stp)
	}
	if stp := stepOf(&res, "redeem"); stp == nil || stp.Status != "skip" {
		t.Errorf("跨日后奖励链应重新执行，得到 %+v", stp)
	}
}

// TestGrowthChainRestartRestore 重启回填：cache 已有今日 report_day → 内存态回填后 skip。
func TestGrowthChainRestartRestore(t *testing.T) {
	origGap := growthReportGap
	growthReportGap = 0
	t.Cleanup(func() { growthReportGap = origGap })

	fake := &fakeGrowth{}
	s, st, _ := newTestScheduler(t, "cn", fake)
	// 模拟重启前的缓存镜像：今日上报已完成
	if err := st.SetCache(growthCacheTable, growthKeyReportDay, mustDay(t)); err != nil {
		t.Fatal(err)
	}
	// 内存 day 为空（新进程）
	res := s.RunGrowthChain()
	if stp := stepOf(&res, "report"); stp == nil || stp.Status != "skip" {
		t.Errorf("重启后应从 cache 回填 reportDone 并 skip，得到 %+v", stp)
	}
	if !s.growthState.reportDone {
		t.Error("cache 镜像应回填内存状态")
	}
}

// TestTravelStateMachine 旅行状态机各分支。
func TestTravelStateMachine(t *testing.T) {
	mk := func(state string, limit bool, record int64, reward int64) *fakeGrowth {
		return &fakeGrowth{
			buddy:       &upstream.Buddy{ID: 1, Name: "喵喵"},
			travelState: &upstream.TravelState{State: state, DailyLimitReached: limit, RecordID: record, RewardCredit: reward},
		}
	}
	runChain := func(f *fakeGrowth) (GrowthChainResult, *fakeGrowth) {
		origGap := growthReportGap
		growthReportGap = 0
		t.Cleanup(func() { growthReportGap = origGap })
		s, _, _ := newTestScheduler(t, "cn", f)
		return s.RunGrowthChain(), f
	}

	// arrived + record → claim
	f := mk("arrived", false, 12, 30)
	f.claimReward = 30
	res, f := runChain(f)
	if stp := stepOf(&res, "travel"); stp == nil || stp.Status != "ok" || stp.Detail != "领取旅行奖励 +30 积分" {
		t.Errorf("arrived 应领奖，得到 %+v", stp)
	}
	if len(f.claimIDs) != 1 || f.claimIDs[0] != 12 {
		t.Errorf("claim 应带 record_id=12，得到 %v", f.claimIDs)
	}
	if f.departN != 0 {
		t.Error("arrived 时不应派出")
	}

	// arrived 无 record_id → warn
	f2 := mk("arrived", false, 0, 30)
	res2, _ := runChain(f2)
	if stp := stepOf(&res2, "travel"); stp == nil || stp.Status != "warn" {
		t.Errorf("arrived 无 record 应 warn，得到 %+v", stp)
	}

	// idle + 今日已派出 → skip
	f3 := mk("idle", true, 0, 0)
	res3, _ := runChain(f3)
	if stp := stepOf(&res3, "travel"); stp == nil || stp.Status != "skip" || stp.Detail != "今日已派出过" {
		t.Errorf("daily limit 应 skip，得到 %+v", stp)
	}

	// idle + 可派出 → depart（地点固定 4）
	f4 := mk("idle", false, 0, 0)
	res4, f4 := runChain(f4)
	if stp := stepOf(&res4, "travel"); stp == nil || stp.Status != "ok" {
		t.Errorf("idle 应派出，得到 %+v", stp)
	}
	if f4.departN != 1 {
		t.Error("应调用 TravelDepart")
	}

	// traveling → skip（不派不领）
	f5 := mk("traveling", false, 7, 30)
	res5, f5 := runChain(f5)
	if stp := stepOf(&res5, "travel"); stp == nil || stp.Status != "skip" {
		t.Errorf("traveling 应 skip，得到 %+v", stp)
	}
	if f5.departN+f5.drawN != 0 || len(f5.claimIDs) != 0 {
		t.Error("traveling 不应有任何动作")
	}
}

// TestTravelChainJustAdoptedNoDepart 链内刚领养成功 → 本轮不派出（下轮巡检派出）。
func TestTravelChainJustAdoptedNoDepart(t *testing.T) {
	origGap := growthReportGap
	growthReportGap = 0
	t.Cleanup(func() { growthReportGap = origGap })

	// 无猫 + 领养成功（first 无错）+ travel idle
	fake := &fakeGrowth{travelState: &upstream.TravelState{State: "idle"}}
	s, _, _ := newTestScheduler(t, "cn", fake)
	res := s.RunGrowthChain()

	if stp := stepOf(&res, "adopt"); stp == nil || stp.Status != "ok" {
		t.Errorf("领养应成功，得到 %+v", stp)
	}
	if stp := stepOf(&res, "travel"); stp == nil || stp.Status != "skip" || stp.Detail != "已领养，下轮巡检派出" {
		t.Errorf("链内刚领养不应派出，得到 %+v", stp)
	}
	if fake.departN != 0 {
		t.Error("刚领养不应 TravelDepart")
	}
}

// TestTravelTickNoAdopt 独立巡检（非链内）发现无猫：仅记 skip，不领养。
func TestTravelTickNoAdopt(t *testing.T) {
	fake := &fakeGrowth{} // 无猫
	s, _, _ := newTestScheduler(t, "cn", fake)
	res := s.RunTravelTick()
	if !res.Ran || res.Action != "skip" {
		t.Errorf("无猫巡检应 skip，得到 %+v", res)
	}
	if fake.firstN != 0 || fake.agreementN != 0 {
		t.Error("独立巡检不应尝试领养")
	}
}

// TestRunTravelTickStates RunTravelTick 各状态动作映射。
func TestRunTravelTickStates(t *testing.T) {
	// arrived → claim
	f := &fakeGrowth{
		buddy:       &upstream.Buddy{ID: 1, Name: "喵喵"},
		travelState: &upstream.TravelState{State: "arrived", RecordID: 9, RewardCredit: 30},
		claimReward: 30,
	}
	s, _, _ := newTestScheduler(t, "cn", f)
	if res := s.RunTravelTick(); res.Action != "claim" || !res.Ran {
		t.Errorf("arrived 应 claim，得到 %+v", res)
	}
	// idle → depart
	f2 := &fakeGrowth{buddy: &upstream.Buddy{ID: 1}, travelState: &upstream.TravelState{State: "idle"}}
	s2, _, _ := newTestScheduler(t, "cn", f2)
	if res := s2.RunTravelTick(); res.Action != "depart" {
		t.Errorf("idle 应 depart，得到 %+v", res)
	}
	// global → 不执行
	f3 := &fakeGrowth{buddy: &upstream.Buddy{ID: 1}}
	s3, _, _ := newTestScheduler(t, "global", f3)
	if res := s3.RunTravelTick(); res.Ran || res.Reason != "region_global" {
		t.Errorf("global 应跳过，得到 %+v", res)
	}
}

// TestRunActivityReportsManual 手动上报：不查 reportDone、发满置 report_day、busy 互斥。
func TestRunActivityReportsManual(t *testing.T) {
	origGap := growthReportGap
	growthReportGap = 0
	t.Cleanup(func() { growthReportGap = origGap })

	fake := &fakeGrowth{}
	s, st, cfg := newTestScheduler(t, "cn", fake)
	// 配置条数 3
	if err := cfg.Update(func(c *config.Config) error { c.GrowthReportCount = 3; return nil }); err != nil {
		t.Fatal(err)
	}
	// 自动链先跑一遍（今日已上报）
	s.RunGrowthChain()
	if !s.growthState.reportDone {
		t.Fatal("前置失败：自动链应置 reportDone")
	}
	// 手动上报：不受日标志阻塞
	res := s.RunActivityReports(0) // count=0 → 取配置 3
	if !res.Ran || res.Sent != 3 || res.Total != 3 {
		t.Errorf("手动上报应发 3 条，得到 %+v", res)
	}
	if payload, _, ok := st.GetCache(growthCacheTable, growthKeyReportDay, growthDayTTL); !ok || payload != mustDay(t) {
		t.Error("手动上报发满应置 report_day")
	}
	// 显式 count 钳制：0 以下取配置，>10 钳 10
	if res := s.RunActivityReports(99); res.Total != 10 {
		t.Errorf("count=99 应钳 10，得到 %d", res.Total)
	}
	// busy
	s.growthMu.Lock()
	res2 := s.RunActivityReports(2)
	s.growthMu.Unlock()
	if res2.Ran || res2.Reason != "busy" {
		t.Errorf("互斥时应 busy，得到 %+v", res2)
	}
}

// TestReportCountJitter 波动数生效：count<=0 时实际发送条数应在 base±jitter 内浮动，
// 且多次执行会出现不同值（证明真的在随机，而非固定取 base）。
func TestReportCountJitter(t *testing.T) {
	origGap := growthReportGap
	growthReportGap = 0
	t.Cleanup(func() { growthReportGap = origGap })

	s, _, cfg := newTestScheduler(t, "cn", &fakeGrowth{})
	if err := cfg.Update(func(c *config.Config) error {
		c.GrowthReportCount = 8
		c.GrowthReportJitter = 2
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	seen := map[int]int{}
	for i := 0; i < 60; i++ {
		s.growthState.reportDone = false // 每轮重置日标志，模拟不同日期
		res := s.RunActivityReports(0)
		if !res.Ran {
			t.Fatalf("第 %d 轮未执行: %+v", i, res)
		}
		if res.Sent != res.Total {
			t.Errorf("sent(%d) 应等于 total(%d)", res.Sent, res.Total)
		}
		if res.Total < 6 || res.Total > 10 {
			t.Fatalf("应在 8±2 即 [6,10] 内，得到 %d", res.Total)
		}
		seen[res.Total]++
	}
	if len(seen) < 2 {
		t.Errorf("波动应产出多个不同条数，实际分布: %v", seen)
	}

	// jitter=0 → 固定 base，多轮恒等
	if err := cfg.Update(func(c *config.Config) error { c.GrowthReportJitter = 0; return nil }); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		s.growthState.reportDone = false
		if got := s.RunActivityReports(0).Total; got != 8 {
			t.Fatalf("jitter=0 应恒为 8，得到 %d", got)
		}
	}

	// 显式 count 不受波动影响
	s.growthState.reportDone = false
	if got := s.RunActivityReports(3).Total; got != 3 {
		t.Errorf("显式 count 应尊重 3，得到 %d", got)
	}
}

// TestRunGrowthRedeemManual 手动兑换：成功置 reward_day；已领 skip 不置。
func TestRunGrowthRedeemManual(t *testing.T) {
	fake := &fakeGrowth{}
	s, st, _ := newTestScheduler(t, "cn", fake)

	out := s.RunGrowthRedeem("7d")
	if !out.Ran || out.Status != "ok" || out.Granted == nil {
		t.Errorf("手动兑换应成功，得到 %+v", out)
	}
	if payload, _, ok := st.GetCache(growthCacheTable, growthKeyRewardDay, growthDayTTL); !ok || payload != mustDay(t) {
		t.Error("兑换成功应置 reward_day")
	}
	if !s.growthState.rewardDone {
		t.Error("兑换成功应置内存 rewardDone")
	}

	// 已领（409 duplicate）→ skip，不覆盖 detail
	fake2 := &fakeGrowth{redeemErr: &upstream.GrowthError{Status: 409, Msg: "duplicate"}}
	s2, st2, _ := newTestScheduler(t, "cn", fake2)
	out2 := s2.RunGrowthRedeem("14d")
	if out2.Status != "skip" || out2.Detail != "本月该档已领取" {
		t.Errorf("409 应 skip，得到 %+v", out2)
	}
	if payload, _, ok := st2.GetCache(growthCacheTable, growthKeyRewardDay, growthDayTTL); ok && payload == mustDay(t) {
		t.Error("skip 不应置 reward_day")
	}
	// 无档位天数不足（403）
	fake3 := &fakeGrowth{redeemErr: &upstream.GrowthError{Status: 403, Msg: "连续登录天数不足"}}
	s3, _, _ := newTestScheduler(t, "cn", fake3)
	if out3 := s3.RunGrowthRedeem("28d"); out3.Status != "skip" || out3.Detail != "连续登录天数不足" {
		t.Errorf("403 应 skip，得到 %+v", out3)
	}
}

// TestRunGrowthAdoptManual 手动领养：已有猫 → skip；门槛未达 → 置 adopt_day。
func TestRunGrowthAdoptManual(t *testing.T) {
	fake := &fakeGrowth{buddy: &upstream.Buddy{ID: 1, Name: "喵喵"}}
	s, _, _ := newTestScheduler(t, "cn", fake)
	out := s.RunGrowthAdopt()
	if out.Status != "skip" || fake.firstN != 0 {
		t.Errorf("已有猫应 skip 且不调 First，得到 %+v firstN=%d", out, fake.firstN)
	}

	fake2 := &fakeGrowth{firstErr: &upstream.GrowthError{Status: 400, Msg: "first_buddy task not completed yet"}}
	s2, st2, _ := newTestScheduler(t, "cn", fake2)
	out2 := s2.RunGrowthAdopt()
	if out2.Status != "skip" {
		t.Errorf("门槛未达应 skip，得到 %+v", out2)
	}
	if payload, _, ok := st2.GetCache(growthCacheTable, growthKeyAdoptDay, growthDayTTL); !ok || payload != mustDay(t) {
		t.Error("手动领养门槛未达应置 adopt_day")
	}
	// force 语义：同日手动领养豁免防抖再试（手动按钮不受 adoptTried 限制）
	if out3 := s2.RunGrowthAdopt(); out3.Status != "skip" {
		t.Errorf("第二次手动领养仍应 skip（门槛未达），得到 %+v", out3)
	}
	if fake2.firstN != 2 {
		t.Errorf("手动 force 领养应豁免防抖（共 2 次 First），得到 %d", fake2.firstN)
	}
}

// TestGrowthCatchupDueMatrix 补跑条件矩阵：region/登录/时点/窗口/日标志。
func TestGrowthCatchupDueMatrix(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfgMgr, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}

	// 已登录与否由 fake token 控制（生产为 client 本体）
	fake := &fakeGrowth{} // token=nil → 未登录
	s := &Scheduler{cfg: cfgMgr, client: nil, st: st, growth: fake}

	cfg := cfgMgr.Get()
	at := func(h, m int) time.Time { return time.Date(2026, 8, 22, h, m, 0, 0, time.Local) } // 周六

	// 未登录 → 不补
	if s.growthCatchupDue(cfg, at(12, 0)) {
		t.Error("未登录不应补跑")
	}
	fake.token = &auth.Token{UID: "u", AccessToken: "t"}
	// 未到时点（08:00 < 10:00）→ 不补
	if s.growthCatchupDue(cfg, at(8, 0)) {
		t.Error("未到上报时点不应补跑")
	}
	// 已过时点且未上报 → 补
	if !s.growthCatchupDue(cfg, at(12, 0)) {
		t.Error("已过时点且未上报应补跑")
	}
	// 22:00 边界（含）→ 补
	if !s.growthCatchupDue(cfg, at(22, 0)) {
		t.Error("22:00 应仍在补跑窗口")
	}
	// 22:01 → 不补
	if s.growthCatchupDue(cfg, at(22, 1)) {
		t.Error("超过 22:00 不应补跑")
	}
	// 今日上报已完成 → 不补
	day := upstream.GrowthTodayDate(at(12, 0))
	if err := st.SetCache(growthCacheTable, growthKeyReportDay, day); err != nil {
		t.Fatal(err)
	}
	if s.growthCatchupDue(cfg, at(12, 0)) {
		t.Error("今日上报完成不应补跑")
	}
	_ = st.DeleteCacheKey(growthCacheTable, growthKeyReportDay)
	// 当日无 cron 触发（仅周一，今天是周六）→ 不补
	cfgNo := cfg
	cfgNo.GrowthReportCron = "0 0 10 * * 1"
	if s.growthCatchupDue(cfgNo, at(12, 0)) {
		t.Error("cron 当日无触发不应补跑")
	}
	// region=global → 不补
	cfgG := cfg
	cfgG.Region = "global"
	if s.growthCatchupDue(cfgG, at(12, 0)) {
		t.Error("global 不应补跑")
	}
}

// TestGrowthReconfigure 开关装配：AutoGrowth 开 → 注册两个 cron；关 → 摘除。
func TestGrowthReconfigure(t *testing.T) {
	// 拉长补跑延迟，避免 Reconfigure 排出的真实 30s timer 在测试结束后触发
	origDelay := growthCatchupDelay
	growthCatchupDelay = time.Hour
	t.Cleanup(func() { growthCatchupDelay = origDelay })

	fake := &fakeGrowth{}
	s, _, cfg := newTestScheduler(t, "cn", fake)
	s.Reconfigure()
	if s.growthReportID != 0 || s.growthTravelID != 0 {
		t.Fatal("默认 AutoGrowth=false 不应注册 growth cron")
	}
	_ = cfg.Update(func(c *config.Config) error { c.AutoGrowth = true; return nil })
	s.Reconfigure()
	if s.growthReportID == 0 || s.growthTravelID == 0 {
		t.Error("开启 AutoGrowth 应注册 growth cron")
	}
	_ = cfg.Update(func(c *config.Config) error { c.AutoGrowth = false; return nil })
	s.Reconfigure()
	if s.growthReportID != 0 || s.growthTravelID != 0 {
		t.Error("关闭 AutoGrowth 应摘除 growth cron")
	}
	// 非法 cron：注册失败但不 panic，entry 不挂
	_ = cfg.Update(func(c *config.Config) error { c.AutoGrowth = true; c.GrowthReportCron = "not-a-cron"; return nil })
	s.Reconfigure()
	if s.growthReportID != 0 {
		t.Error("非法 cron 不应注册")
	}
	// 签到 cron 在 AutoCheckin=false + AutoGrowth=true 时也互不影响
	_ = cfg.Update(func(c *config.Config) error {
		c.AutoGrowth = true
		c.GrowthReportCron = "0 0 10 * * *"
		c.AutoCheckin = true
		return nil
	})
	s.Reconfigure()
	if s.growthReportID == 0 || s.tickID == 0 {
		t.Error("两个开关应独立装配")
	}
}

// TestGrowthChainLotterySkipStates 抽奖正常态分类：无次数/未开启 → skip；真错误 → fail。
// （链内 lottery 步仅在 redeem 后执行，故造一个可领 7d 档。）
func TestGrowthChainLotterySkipStates(t *testing.T) {
	origGap := growthReportGap
	growthReportGap = 0
	t.Cleanup(func() { growthReportGap = origGap })

	mk := func(drawErr error) *fakeGrowth {
		info := &upstream.GrowthStreakInfo{}
		info.Streak.Days = 7
		info.Redemption.Tiers = []upstream.GrowthTierSpec{{Tier: "7d", Days: 7, Credit: 80}}
		return &fakeGrowth{streakSeq: []*upstream.GrowthStreakInfo{info}, chances: 2, drawErr: drawErr}
	}
	// 400 insufficient → skip
	s, _, _ := newTestScheduler(t, "cn", mk(&upstream.GrowthError{Status: 400, Msg: "insufficient lottery chance balance"}))
	res := s.RunGrowthChain()
	if stp := stepOf(&res, "lottery"); stp == nil || stp.Status != "skip" {
		t.Errorf("无次数应 skip，得到 %+v", stp)
	}
	// 400 disabled → skip
	s2, _, _ := newTestScheduler(t, "cn", mk(&upstream.GrowthError{Status: 400, Msg: "lottery disabled"}))
	res2 := s2.RunGrowthChain()
	if stp := stepOf(&res2, "lottery"); stp == nil || stp.Status != "skip" {
		t.Errorf("未开启应 skip，得到 %+v", stp)
	}
	// 真错误 → fail
	s3, _, _ := newTestScheduler(t, "cn", mk(errors.New("network refused")))
	res3 := s3.RunGrowthChain()
	if stp := stepOf(&res3, "lottery"); stp == nil || stp.Status != "fail" {
		t.Errorf("真错误应 fail，得到 %+v", stp)
	}
}

// TestRunTravelTickFailures 巡检动作失败：claim/depart 上游错误 → Status fail（前端 toast 分级）。
func TestRunTravelTickFailures(t *testing.T) {
	// claim 失败
	f := &fakeGrowth{
		buddy:       &upstream.Buddy{ID: 1},
		travelState: &upstream.TravelState{State: "arrived", RecordID: 5, RewardCredit: 30},
		claimErr:    errors.New("upstream 500"),
	}
	s, _, _ := newTestScheduler(t, "cn", f)
	if res := s.RunTravelTick(); res.Status != "fail" || res.Action != "claim" {
		t.Errorf("claim 失败应 fail，得到 %+v", res)
	}
	// depart 失败
	f2 := &fakeGrowth{
		buddy:       &upstream.Buddy{ID: 1},
		travelState: &upstream.TravelState{State: "idle"},
		departErr:   errors.New("already departed"),
	}
	s2, _, _ := newTestScheduler(t, "cn", f2)
	if res := s2.RunTravelTick(); res.Status != "fail" || res.Action != "depart" {
		t.Errorf("depart 失败应 fail，得到 %+v", res)
	}
}

// TestRunGrowthRedeemFail 手动兑换真错误 → fail 且不置 reward_day。
func TestRunGrowthRedeemFail(t *testing.T) {
	fake := &fakeGrowth{redeemErr: errors.New("network refused")}
	s, st, _ := newTestScheduler(t, "cn", fake)
	out := s.RunGrowthRedeem("7d")
	if out.Status != "fail" {
		t.Errorf("网络错误应 fail，得到 %+v", out)
	}
	if payload, _, ok := st.GetCache(growthCacheTable, growthKeyRewardDay, growthDayTTL); ok && payload == mustDay(t) {
		t.Error("fail 不应置 reward_day")
	}
}

// TestStepReportsClearsAdoptDebounce 上报补满清领养防抖：今日早上领养失败（门槛
// 未达）后，上报把 chat_5 补满 → 防抖清除，链内/手动领养可再试并成功。
func TestStepReportsClearsAdoptDebounce(t *testing.T) {
	origGap := growthReportGap
	growthReportGap = 0
	t.Cleanup(func() { growthReportGap = origGap })

	fake := &fakeGrowth{} // 无猫，领养成功
	s, st, _ := newTestScheduler(t, "cn", fake)
	// 模拟今早手动领养失败后的防抖状态
	s.growthMu.Lock()
	s.growthState = growthDay{day: mustDay(t), adoptTried: true}
	s.growthMu.Unlock()
	if err := st.SetCache(growthCacheTable, growthKeyAdoptDay, mustDay(t)); err != nil {
		t.Fatal(err)
	}

	res := s.RunGrowthChain() // 上报发满 → 清防抖 → force=true 领养成功
	if s.growthState.adoptTried {
		t.Error("上报补满应清领养防抖")
	}
	if _, _, ok := st.GetCache(growthCacheTable, growthKeyAdoptDay, growthDayTTL); ok {
		t.Error("adopt_day 缓存应随之删除")
	}
	if stp := stepOf(&res, "adopt"); stp == nil || stp.Status != "ok" {
		t.Errorf("防抖清除后领养应成功，得到 %+v", stp)
	}

	// 手动上报路径同样清防抖
	s.growthMu.Lock()
	s.growthState.adoptTried = true
	s.growthMu.Unlock()
	if out := s.RunActivityReports(2); out.Sent != 2 {
		t.Fatalf("手动上报应成功: %+v", out)
	}
	if s.growthState.adoptTried {
		t.Error("手动上报补满应清领养防抖")
	}
}

// TestGrowthCatchupFire 补跑回调复查：开关关闭或今日上报已完成 → 不执行链。
func TestGrowthCatchupFire(t *testing.T) {
	origGap := growthReportGap
	growthReportGap = 0
	t.Cleanup(func() { growthReportGap = origGap })

	fake := &fakeGrowth{}
	s, _, cfg := newTestScheduler(t, "cn", fake)
	// 开关关闭 → 不跑
	s.growthCatchupFire()
	if fake.reportCalls != 0 {
		t.Error("AutoGrowth 关闭时补跑不应执行")
	}
	// 开启且今日未上报 → 跑（fake 上报全成功，链内置 report_day）
	if err := cfg.Update(func(c *config.Config) error { c.AutoGrowth = true; return nil }); err != nil {
		t.Fatal(err)
	}
	s.growthCatchupFire()
	if fake.reportCalls == 0 {
		t.Error("开启且今日未上报应补跑")
	}
	// 上一趟已置 report_day → 复查后不再跑
	n := fake.reportCalls
	s.growthCatchupFire()
	if fake.reportCalls != n {
		t.Error("今日上报已完成时补跑回调不应再执行链")
	}
}
