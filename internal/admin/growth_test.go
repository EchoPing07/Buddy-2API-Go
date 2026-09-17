package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"buddy2api-go/internal/auth"
	"buddy2api-go/internal/config"
	"buddy2api-go/internal/scheduler"
	"buddy2api-go/internal/store"
	"buddy2api-go/internal/upstream"
)

// ── fake scheduler（SchedulerControl 空实现 + 调用记录）───────────────────────

type fakeSched struct {
	reconfigureN int
	chainN       int
	travelN      int
	reportN      int
	adoptN       int
	redeemN      int
	lastTier     string
}

func (f *fakeSched) Reconfigure() { f.reconfigureN++ }
func (f *fakeSched) RunGrowthChain() scheduler.GrowthChainResult {
	f.chainN++
	return scheduler.GrowthChainResult{Ran: true, Day: "2026-01-01"}
}
func (f *fakeSched) RunTravelTick() scheduler.TravelTickResult {
	f.travelN++
	return scheduler.TravelTickResult{Ran: true, Action: "skip"}
}
func (f *fakeSched) RunActivityReports(count int) scheduler.ActivityReportResult {
	f.reportN++
	return scheduler.ActivityReportResult{Ran: true, Sent: count, Total: count}
}
func (f *fakeSched) RunGrowthAdopt() scheduler.GrowthAdoptResult {
	f.adoptN++
	return scheduler.GrowthAdoptResult{Ran: true, Status: "ok", Detail: "领养成功"}
}
func (f *fakeSched) RunGrowthRedeem(tier string) scheduler.GrowthRedeemActionResult {
	f.redeemN++
	f.lastTier = tier
	return scheduler.GrowthRedeemActionResult{Ran: true, Status: "ok",
		Granted: &upstream.GrowthRedeemResult{CreditGranted: 80}}
}

// ── fake 上游（growthAPI 注入用）────────────────────────────────────────────

type fakeGrowthAPI struct {
	info        *upstream.GrowthStreakInfo
	infoErr     error
	infoN       int // GrowthStreakInfo 调用计数（验证缓存命中未触达上游）
	cells       []upstream.HeatmapCell
	cellsErr    error
	buddy       *upstream.Buddy
	buddyErr    error
	ts          *upstream.TravelState
	chances     int
	chancesSeq  []int // 依次返回（抽后回查场景），耗尽后保持最后一个
	chancesErr  error
	draw        *upstream.GrowthLotteryDrawResult
	drawErr     error
	drawN       int
	makeupErr   error
	makeupDates []string
	gift        int64
	giftErr     error
	comp        int64
	compErr     error
}

func (f *fakeGrowthAPI) GrowthStreakInfo() (*upstream.GrowthStreakInfo, error) {
	f.infoN++
	return f.info, f.infoErr
}
func (f *fakeGrowthAPI) GrowthHeatmap() ([]upstream.HeatmapCell, error) {
	return f.cells, f.cellsErr
}
func (f *fakeGrowthAPI) BuddyInfo() (*upstream.Buddy, error) {
	return f.buddy, f.buddyErr
}
func (f *fakeGrowthAPI) TravelStatus() (*upstream.TravelState, error) {
	return f.ts, nil
}
func (f *fakeGrowthAPI) GrowthLotteryChances() (int, error) {
	if f.chancesErr != nil {
		return 0, f.chancesErr
	}
	if len(f.chancesSeq) == 0 {
		return f.chances, nil
	}
	v := f.chancesSeq[0]
	if len(f.chancesSeq) > 1 {
		f.chancesSeq = f.chancesSeq[1:]
	}
	return v, nil
}
func (f *fakeGrowthAPI) GrowthLotteryDraw(clientToken string) (*upstream.GrowthLotteryDrawResult, error) {
	f.drawN++
	if f.drawErr != nil {
		return nil, f.drawErr
	}
	if f.draw != nil {
		return f.draw, nil
	}
	return &upstream.GrowthLotteryDrawResult{PrizeName: "6积分", PrizeType: "credit", CreditAmount: 6}, nil
}
func (f *fakeGrowthAPI) UseMakeupCard(targetDate string) error {
	f.makeupDates = append(f.makeupDates, targetDate)
	return f.makeupErr
}
func (f *fakeGrowthAPI) ClaimGift() (int64, error)         { return f.gift, f.giftErr }
func (f *fakeGrowthAPI) ClaimCompensation() (int64, error) { return f.comp, f.compErr }

func newGrowthTestHandler(t *testing.T, region string, loggedIn bool, gf *fakeGrowthAPI) (*Handler, *store.Store, *fakeSched) {
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
	toks, err := auth.NewTokenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if loggedIn {
		if err := toks.Save(&auth.Token{UID: "u", AccessToken: "t", ExpiresAt: time.Now().Add(time.Hour).Unix()}); err != nil {
			t.Fatal(err)
		}
	}
	client := upstream.New(toks, func() string { return region }, 60)
	fs := &fakeSched{}
	h := &Handler{cfg: cfg, st: st, toks: toks, client: client, growth: gf, sched: fs}
	return h, st, fs
}

// ── overview 聚合纯函数 ───────────────────────────────────────────────────────────

// TestBuildGrowthOverview 覆盖：tier 状态计算、热力格昨日/今日切片、
// degraded 聚合、无猫时 travel 不算降级。
func TestBuildGrowthOverview(t *testing.T) {
	now := time.Date(2026, 1, 16, 12, 0, 0, 0, time.UTC)
	info := &upstream.GrowthStreakInfo{}
	info.Streak.Days = 7
	info.MakeupCards.Balance, info.MakeupCards.Max = 1, 3
	info.Redemption.Tier7dStatus = "claimed"
	info.Redemption.Tier14dStatus = "available"
	info.Redemption.Tier28dStatus = "locked"
	info.Redemption.RemainingDays = 7
	info.Redemption.Tiers = []upstream.GrowthTierSpec{
		{Tier: "7d", Days: 7, Credit: 80}, {Tier: "14d", Days: 14, Credit: 150}, {Tier: "28d", Days: 28, Credit: 400},
	}
	cells := []upstream.HeatmapCell{
		{Date: "2026-01-14", Score: 1},
		{Date: "2026-01-15", Score: 0}, // 昨日（CST）漏签
	}
	buddy := &upstream.Buddy{ID: 1, Name: "喵喵"}
	ts := &upstream.TravelState{State: "arrived", RecordID: 12, RewardCredit: 30}
	chances := 2
	lastRun := &scheduler.GrowthChainResult{Day: "2026-01-16", Ran: true}

	ov := buildGrowthOverview(now, 5, info, cells, true, buddy, true, ts, &chances, true, false, false, lastRun)

	if !ov.Available || ov.Streak == nil || ov.Streak.Days != 7 {
		t.Fatalf("streak 聚合异常: %+v", ov.Streak)
	}
	if ov.Streak.RemainingDays != 7 || ov.Streak.MakeupCards.Balance != 1 {
		t.Errorf("streak 细节异常: %+v", ov.Streak)
	}
	// tier 状态：7d claimed / 14d locked（14>7 天数不足）/ 28d locked
	want := []string{"claimed", "locked", "locked"}
	for i, s := range want {
		if ov.Streak.Tiers[i].Status != s {
			t.Errorf("tier %s 状态=%s，期望 %s（days=7 时 14d/28d 均为天数不足 locked）",
				ov.Streak.Tiers[i].Tier, ov.Streak.Tiers[i].Status, s)
		}
	}
	// days 达标时 available：days=20 → 14d available
	info2 := &upstream.GrowthStreakInfo{}
	info2.Streak.Days = 20
	info2.Redemption.Tiers = info.Redemption.Tiers
	ov2 := buildGrowthOverview(now, 5, info2, nil, false, nil, true, nil, &chances, false, false, false, nil)
	if ov2.Streak.Tiers[1].Status != "available" || ov2.Streak.Tiers[2].Status != "locked" {
		t.Errorf("days=20 时 14d 应 available、28d 应 locked，得到 %s/%s",
			ov2.Streak.Tiers[1].Status, ov2.Streak.Tiers[2].Status)
	}

	// 热力格：昨日（CST 2026-01-15）漏签命中；今日（01-16）无格 present=false
	if ov.Heatmap == nil || ov.Heatmap.Yesterday == nil || !ov.Heatmap.Yesterday.Present || ov.Heatmap.Yesterday.Score != 0 {
		t.Errorf("昨日热力格异常: %+v", ov.Heatmap)
	}
	if ov.Heatmap.Today == nil || ov.Heatmap.Today.Present {
		t.Errorf("今日无格应 present=false: %+v", ov.Heatmap.Today)
	}

	if !ov.Buddy.Has || ov.Buddy.Name != "喵喵" || ov.Travel == nil || ov.Travel.State != "arrived" {
		t.Errorf("buddy/travel 聚合异常: %+v %+v", ov.Buddy, ov.Travel)
	}
	if ov.Lottery == nil || ov.Lottery.Chances != 2 {
		t.Errorf("lottery 聚合异常: %+v", ov.Lottery)
	}
	if len(ov.Degraded) != 0 {
		t.Errorf("全量数据不应有 degraded: %v", ov.Degraded)
	}
	if !ov.Today.ReportDone || ov.Today.AdoptTried || ov.Today.ReportCount != 5 {
		t.Errorf("today 标志异常: %+v", ov.Today)
	}
	if ov.LastRun == nil || ov.LastRun.Day != "2026-01-16" {
		t.Errorf("last_run 异常: %+v", ov.LastRun)
	}

	// degraded：streak/heatmap/lottery 失败计入；无猫时 travel 不算降级
	var nilChances *int
	ov3 := buildGrowthOverview(now, 5, nil, nil, false, nil, true, nil, nilChances, false, false, false, nil)
	if ov3.Streak != nil || ov3.Heatmap != nil || ov3.Lottery != nil {
		t.Error("失败切片应为 nil")
	}
	for _, d := range []string{"streak", "heatmap", "lottery"} {
		found := false
		for _, got := range ov3.Degraded {
			if got == d {
				found = true
			}
		}
		if !found {
			t.Errorf("degraded 应含 %q，得到 %v", d, ov3.Degraded)
		}
	}
	for _, got := range ov3.Degraded {
		if got == "travel" {
			t.Error("无猫时 travel 不应计入 degraded")
		}
	}
	// 有猫但 travel 查询失败 → degraded 含 travel
	ov4 := buildGrowthOverview(now, 5, nil, nil, false, buddy, true, nil, nilChances, false, false, false, nil)
	travelDegraded := false
	for _, got := range ov4.Degraded {
		if got == "travel" {
			travelDegraded = true
		}
	}
	if !travelDegraded {
		t.Errorf("有猫且 travel 失败应计入 degraded: %v", ov4.Degraded)
	}
	// buddy 查询失败（buddyOK=false）→ degraded 含 buddy 且 has=false（≠真无猫，
	// 前端不得因此展示领养空态诱导写操作）
	ov5 := buildGrowthOverview(now, 5, nil, nil, false, nil, false, nil, nilChances, false, false, false, nil)
	buddyDegraded := false
	for _, got := range ov5.Degraded {
		if got == "buddy" {
			buddyDegraded = true
		}
	}
	if !buddyDegraded || ov5.Buddy.Has {
		t.Errorf("buddy 查询失败应 degraded 且 has=false，得到 degraded=%v has=%v", ov5.Degraded, ov5.Buddy.Has)
	}
}

// ── overview 端点：不可用态（region/global、未登录）─────────────────────────

func TestGrowthOverviewUnavailable(t *testing.T) {
	// region=global：不发起任何上游调用（client 真实但无网络路径可走——只走到前置检查）
	h, _, _ := newGrowthTestHandler(t, "global", true, &fakeGrowthAPI{})
	rec := httptest.NewRecorder()
	h.growthOverview(rec, httptest.NewRequest(http.MethodGet, "/admin/growth/overview", nil))
	body := rec.Body.String()
	if !strings.Contains(body, `"available":false`) || !strings.Contains(body, "region_global") {
		t.Errorf("global 应返回 available:false region_global，得到 %s", body)
	}
	// 未登录
	h2, _, _ := newGrowthTestHandler(t, "cn", false, &fakeGrowthAPI{})
	rec2 := httptest.NewRecorder()
	h2.growthOverview(rec2, httptest.NewRequest(http.MethodGet, "/admin/growth/overview", nil))
	body2 := rec2.Body.String()
	if !strings.Contains(body2, "not_logged_in") {
		t.Errorf("未登录应返回 not_logged_in，得到 %s", body2)
	}
}

// ── 动作端点参数校验 ─────────────────────────────────────────────────────────

func TestGrowthRedeemTierValidation(t *testing.T) {
	h, _, fs := newGrowthTestHandler(t, "cn", true, &fakeGrowthAPI{})
	req := httptest.NewRequest(http.MethodPost, "/admin/growth/redeem", strings.NewReader(`{"tier":"99d"}`))
	rec := httptest.NewRecorder()
	h.growthRedeem(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("非法 tier 应 400，得到 %d body=%s", rec.Code, rec.Body.String())
	}
	if fs.redeemN != 0 {
		t.Error("非法 tier 不应触达 scheduler")
	}
	// 合法 tier 透传
	req2 := httptest.NewRequest(http.MethodPost, "/admin/growth/redeem", strings.NewReader(`{"tier":"14d"}`))
	rec2 := httptest.NewRecorder()
	h.growthRedeem(rec2, req2)
	if rec2.Code != http.StatusOK || fs.redeemN != 1 || fs.lastTier != "14d" {
		t.Errorf("合法 tier 应透传 scheduler，code=%d redeemN=%d tier=%q body=%s",
			rec2.Code, fs.redeemN, fs.lastTier, rec2.Body.String())
	}
}

func TestGrowthMakeupDateValidation(t *testing.T) {
	h, _, _ := newGrowthTestHandler(t, "cn", true, &fakeGrowthAPI{})
	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/admin/growth/makeup", strings.NewReader(body))
		rec := httptest.NewRecorder()
		h.growthMakeup(rec, req)
		return rec
	}
	today := upstream.GrowthTodayDate(time.Now())
	cases := []struct {
		name string
		body string
	}{
		{"未来日期", `{"date":"2099-01-01"}`},
		{"今日", `{"date":"` + today + `"}`},
		{"格式非法", `{"date":"2026/01/15"}`},
		{"非法日期值", `{"date":"2026-13-45"}`},
		{"超长串", `{"date":"2026-01-15 00:00:00"}`},
	}
	for _, c := range cases {
		rec := post(c.body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: 应 400，得到 %d body=%s", c.name, rec.Code, rec.Body.String())
		}
	}
	// 未登录 → ok:false not_logged_in（HTTP 200）
	h2, _, _ := newGrowthTestHandler(t, "cn", false, &fakeGrowthAPI{})
	req := httptest.NewRequest(http.MethodPost, "/admin/growth/makeup", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	h2.growthMakeup(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "not_logged_in") {
		t.Errorf("未登录应 ok:false not_logged_in，得到 %d %s", rec.Code, rec.Body.String())
	}
}

// ── 动作后清 overview 缓存 ───────────────────────────────────────────────────

func TestGrowthActionInvalidatesOverview(t *testing.T) {
	h, st, fs := newGrowthTestHandler(t, "cn", true, &fakeGrowthAPI{})
	if err := st.SetCache(growthCacheTable, growthKeyOverview, `{"stale":true}`); err != nil {
		t.Fatal(err)
	}
	// 手动执行链 → 缓存被清
	req := httptest.NewRequest(http.MethodPost, "/admin/growth/run", nil)
	rec := httptest.NewRecorder()
	h.growthRun(rec, req)
	if rec.Code != http.StatusOK || fs.chainN != 1 {
		t.Fatalf("run 应透传 scheduler: code=%d chainN=%d", rec.Code, fs.chainN)
	}
	if payload, _, ok := st.GetCache(growthCacheTable, growthKeyOverview, growthOverviewTTL); ok && payload != "" {
		t.Error("动作后 overview 缓存应被清除")
	}
}

// 手动上报 count 缺省取配置、超界钳 10。
func TestGrowthReportCountClamp(t *testing.T) {
	h, _, fs := newGrowthTestHandler(t, "cn", true, &fakeGrowthAPI{})
	// 缺省 → 配置值 5
	req := httptest.NewRequest(http.MethodPost, "/admin/growth/report", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	h.growthReport(rec, req)
	if fs.reportN != 1 {
		t.Fatal("应触达 scheduler")
	}
	if !strings.Contains(rec.Body.String(), `"total":5`) {
		t.Errorf("缺省 count 应取配置 5，得到 %s", rec.Body.String())
	}
	// 超界 → 钳 10
	req2 := httptest.NewRequest(http.MethodPost, "/admin/growth/report", strings.NewReader(`{"count":99}`))
	rec2 := httptest.NewRecorder()
	h.growthReport(rec2, req2)
	if !strings.Contains(rec2.Body.String(), `"total":10`) {
		t.Errorf("count=99 应钳 10，得到 %s", rec2.Body.String())
	}
}

// ── overview 缓存与聚合行为（fake 上游注入，无网络）──────────────────────────

// TestGrowthOverviewCacheHit 60s 内命中缓存：cached:true 直接回放缓存内容，不发上游。
func TestGrowthOverviewCacheHit(t *testing.T) {
	gf := &fakeGrowthAPI{} // 若被调用则说明缓存未命中（无网络路径下会 panic/超时前先断言）
	h, st, _ := newGrowthTestHandler(t, "cn", true, gf)
	if err := st.SetCache(growthCacheTable, growthKeyOverview, `{"available":true,"updated_at":123,"streak":{"days":5}}`); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/admin/growth/overview", nil)
	rec := httptest.NewRecorder()
	h.growthOverview(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, `"cached":true`) || !strings.Contains(body, `"days":5`) {
		t.Errorf("缓存命中应回放缓存内容（cached:true），得到 %s", body)
	}
	if gf.infoN != 0 {
		t.Errorf("缓存命中不应触达上游，GrowthStreakInfo 调用 %d 次", gf.infoN)
	}
}

// TestGrowthOverviewForceBypass force=1 绕过缓存重新聚合，且 buddy 查询失败
// 计入 degraded（查询失败 ≠ 无猫）。
func TestGrowthOverviewForceBypass(t *testing.T) {
	info := &upstream.GrowthStreakInfo{}
	info.Streak.Days = 5
	gf := &fakeGrowthAPI{info: info, buddyErr: errors.New("upstream 500"), chances: 2}
	h, st, _ := newGrowthTestHandler(t, "cn", true, gf)
	if err := st.SetCache(growthCacheTable, growthKeyOverview, `{"available":true,"streak":{"days":999}}`); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/admin/growth/overview?force=1", nil)
	rec := httptest.NewRecorder()
	h.growthOverview(rec, req)
	body := rec.Body.String()
	if strings.Contains(body, "999") {
		t.Errorf("force=1 应绕过缓存重新聚合，得到 %s", body)
	}
	if !strings.Contains(body, `"days":5`) {
		t.Errorf("应使用 fake 上游数据，得到 %s", body)
	}
	if !strings.Contains(body, `"degraded":["buddy"]`) {
		t.Errorf("buddy 查询失败应计入 degraded，得到 %s", body)
	}
	if !strings.Contains(body, `"has":false`) {
		t.Errorf("buddy 失败时 has 应为 false，得到 %s", body)
	}
}

// ── lottery / makeup / bonus handler ────────────────────────────────────────

// TestGrowthLotteryHandler 手动抽奖：无次数 skip / 成功 ok+prize+chances_left 回查 /
// 上游判无次数 skip / 真错误 fail / 次数查询失败 fail。
func TestGrowthLotteryHandler(t *testing.T) {
	post := func(gf *fakeGrowthAPI) map[string]any {
		h, _, _ := newGrowthTestHandler(t, "cn", true, gf)
		req := httptest.NewRequest(http.MethodPost, "/admin/growth/lottery", nil)
		rec := httptest.NewRecorder()
		h.growthLottery(rec, req)
		var m map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			t.Fatalf("响应非 JSON: %s", rec.Body.String())
		}
		return m
	}
	// 无次数 → skip 且不 draw
	gf := &fakeGrowthAPI{chances: 0}
	m := post(gf)
	if m["status"] != "skip" || gf.drawN != 0 {
		t.Errorf("无次数应 skip 且不抽，得到 %v drawN=%d", m, gf.drawN)
	}
	// 成功 → ok + prize + chances_left 回查（2 → 抽后 1）
	gf2 := &fakeGrowthAPI{
		chancesSeq: []int{2, 1},
		draw:       &upstream.GrowthLotteryDrawResult{PrizeName: "6积分", PrizeType: "credit", CreditAmount: 6},
	}
	m2 := post(gf2)
	if m2["status"] != "ok" || m2["chances_left"] != float64(1) {
		t.Errorf("成功应回查 chances_left=1，得到 %v", m2)
	}
	if p, ok := m2["prize"].(map[string]any); !ok || p["prize_name"] != "6积分" {
		t.Errorf("prize 异常: %v", m2["prize"])
	}
	// 上游判无次数（400 insufficient）→ skip
	gf3 := &fakeGrowthAPI{chances: 1, drawErr: &upstream.GrowthError{Status: 400, Msg: "insufficient lottery chance balance"}}
	if m3 := post(gf3); m3["status"] != "skip" {
		t.Errorf("上游判无次数应 skip，得到 %v", m3)
	}
	// 真错误 → fail
	gf4 := &fakeGrowthAPI{chances: 1, drawErr: errors.New("network refused")}
	if m4 := post(gf4); m4["status"] != "fail" {
		t.Errorf("真错误应 fail，得到 %v", m4)
	}
	// 次数查询失败 → fail
	gf5 := &fakeGrowthAPI{chancesErr: errors.New("boom")}
	if m5 := post(gf5); m5["status"] != "fail" {
		t.Errorf("次数查询失败应 fail，得到 %v", m5)
	}
}

// TestGrowthMakeupHandler 手动补签：默认昨日成功 / 业务错误 skip / 网络与 5xx fail。
func TestGrowthMakeupHandler(t *testing.T) {
	post := func(gf *fakeGrowthAPI, body string) map[string]any {
		h, _, _ := newGrowthTestHandler(t, "cn", true, gf)
		req := httptest.NewRequest(http.MethodPost, "/admin/growth/makeup", strings.NewReader(body))
		rec := httptest.NewRecorder()
		h.growthMakeup(rec, req)
		var m map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			t.Fatalf("响应非 JSON: %s", rec.Body.String())
		}
		return m
	}
	// 默认昨日，成功
	gf := &fakeGrowthAPI{}
	m := post(gf, `{}`)
	if m["status"] != "ok" {
		t.Errorf("补签应 ok，得到 %v", m)
	}
	want := upstream.GrowthYesterdayDate(time.Now())
	if m["date"] != want || len(gf.makeupDates) != 1 || gf.makeupDates[0] != want {
		t.Errorf("默认应补昨日 %s，得到 date=%v 调用=%v", want, m["date"], gf.makeupDates)
	}
	// 业务错误（无卡，HTTP 200 + code!=0）→ skip 展示上游文案
	gf2 := &fakeGrowthAPI{makeupErr: &upstream.GrowthError{Status: 200, Code: 1001, Msg: "无补签卡"}}
	if m2 := post(gf2, `{}`); m2["status"] != "skip" {
		t.Errorf("业务错误应 skip，得到 %v", m2)
	}
	// 4xx（如「已补过」）→ skip
	gf3 := &fakeGrowthAPI{makeupErr: &upstream.GrowthError{Status: 409, Msg: "already made up"}}
	if m3 := post(gf3, `{}`); m3["status"] != "skip" {
		t.Errorf("4xx 业务错误应 skip，得到 %v", m3)
	}
	// 网络错误 → fail
	gf4 := &fakeGrowthAPI{makeupErr: errors.New("network refused")}
	if m4 := post(gf4, `{}`); m4["status"] != "fail" {
		t.Errorf("网络错误应 fail，得到 %v", m4)
	}
	// 5xx → fail
	gf5 := &fakeGrowthAPI{makeupErr: &upstream.GrowthError{Status: 500, Msg: "boom"}}
	if m5 := post(gf5, `{}`); m5["status"] != "fail" {
		t.Errorf("5xx 应 fail，得到 %v", m5)
	}
}

// TestGrowthBonusHandler 手动礼包+补偿：双领取、部分静默、全静默。
func TestGrowthBonusHandler(t *testing.T) {
	post := func(gf *fakeGrowthAPI) map[string]any {
		h, _, _ := newGrowthTestHandler(t, "cn", true, gf)
		req := httptest.NewRequest(http.MethodPost, "/admin/growth/bonus", nil)
		rec := httptest.NewRecorder()
		h.growthBonus(rec, req)
		var m map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			t.Fatalf("响应非 JSON: %s", rec.Body.String())
		}
		return m
	}
	m := post(&fakeGrowthAPI{gift: 50, comp: 30})
	if m["gift"] != float64(50) || m["compensation"] != float64(30) {
		t.Errorf("双领取应展示，得到 %v", m)
	}
	if d, _ := m["detail"].(string); !strings.Contains(d, "50") || !strings.Contains(d, "30") {
		t.Errorf("detail 应含两项数值: %v", m["detail"])
	}
	m2 := post(&fakeGrowthAPI{giftErr: errors.New("already claimed"), comp: 10})
	if m2["gift"] != float64(0) || m2["compensation"] != float64(10) {
		t.Errorf("礼包失败应静默计 0，得到 %v", m2)
	}
	m3 := post(&fakeGrowthAPI{giftErr: errors.New("x"), compErr: errors.New("y")})
	if m3["detail"] != "暂无可领取（早已领过或无补偿）" {
		t.Errorf("全静默文案异常: %v", m3["detail"])
	}
}
