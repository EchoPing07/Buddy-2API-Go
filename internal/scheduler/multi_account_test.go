package scheduler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"buddy2api-go/internal/auth"
	"buddy2api-go/internal/config"
	"buddy2api-go/internal/pool"
	"buddy2api-go/internal/store"
	"buddy2api-go/internal/taskops"
	"buddy2api-go/internal/upstream"
)

// multiAccountFixture 搭一套真实 pool.Manager + upstream.Client + httptest 上游的多账号环境。
// 每个账号的 AccessToken 为 "tok-<uid>"，上游据此识别发起请求的账号。
type multiAccountFixture struct {
	s      *Scheduler
	p      *pool.Manager
	st     *store.Store
	cfg    *config.Manager
	client *upstream.Client
	srv    *httptest.Server

	mu            sync.Mutex
	checkinClaims map[string]int
	reportCalls   map[string]int
	travelDeparts map[string]int
	travelClaims  map[string]int
	travelStates  map[string]string // uid -> "arrived" | "idle" | "traveling"
}

type seedAccount struct {
	uid            string
	domain         string // "" 默认 CN 域
	manualDisabled bool
	autoDisabled   string
}

func newMultiAccountFixture(t *testing.T, accounts []seedAccount) *multiAccountFixture {
	t.Helper()
	origReportGap := growthReportGap
	origMultiGap := multiAccountGap
	origCatchupDelay := growthCatchupDelay
	growthReportGap = 0
	multiAccountGap = 0
	growthCatchupDelay = time.Hour
	t.Cleanup(func() {
		growthReportGap = origReportGap
		multiAccountGap = origMultiGap
		growthCatchupDelay = origCatchupDelay
	})

	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "multi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Update(func(c *config.Config) error {
		c.Region = "cn"
		c.CheckinMode = "fixed"
		c.CheckinCron = "0 0 9 * * *"
		c.GrowthReportCount = 2
		c.GrowthReportJitter = 0
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	fx := &multiAccountFixture{
		st:            st,
		cfg:           cfg,
		checkinClaims: make(map[string]int),
		reportCalls:   make(map[string]int),
		travelDeparts: make(map[string]int),
		travelClaims:  make(map[string]int),
		travelStates:  make(map[string]string),
	}

	fx.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid := uidFromReq(r)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v2/billing/meter/checkin-activity-status":
			fx.mu.Lock()
			checked := fx.checkinClaims[uid] > 0
			fx.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{"active": true, "today_checked_in": checked},
			})
		case r.URL.Path == "/v2/billing/meter/daily-checkin":
			fx.mu.Lock()
			fx.checkinClaims[uid]++
			fx.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{"credit": 50, "today_checked_in": true},
			})
		case r.URL.Path == "/v2/report":
			fx.mu.Lock()
			fx.reportCalls[uid]++
			fx.mu.Unlock()
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
		case r.URL.Path == "/activity/growth/streak":
			_, _ = w.Write([]byte(`{"code":0,"data":{"streak":{"days":3},"makeup_cards":{"balance":0},"redemption":{"tiers":[]}}}`))
		case r.URL.Path == "/activity/growth/heatmap":
			_, _ = w.Write([]byte(`{"code":0,"data":{"cells":[]}}`))
		case r.URL.Path == "/activity/growth/lottery/chances" || r.URL.Path == "/activity/growth/lottery/summary":
			_, _ = w.Write([]byte(`{"code":0,"data":{"balance":0,"chances":0}}`))
		case r.URL.Path == "/activity/growth/buddy/info":
			_, _ = w.Write([]byte(`{"code":0,"data":{"buddy":{"id":1,"name":"喵喵"}}}`))
		case r.URL.Path == "/activity/growth/buddy/travel/status":
			fx.mu.Lock()
			state := fx.travelStates[uid]
			fx.mu.Unlock()
			if state == "" {
				state = "idle"
			}
			recID := int64(0)
			if state == "arrived" {
				recID = 88
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{"state": state, "record_id": recID, "reward_credit": 30},
			})
		case r.URL.Path == "/activity/growth/buddy/travel/depart":
			fx.mu.Lock()
			fx.travelDeparts[uid]++
			fx.mu.Unlock()
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
		case r.URL.Path == "/activity/growth/buddy/travel/claim":
			fx.mu.Lock()
			fx.travelClaims[uid]++
			fx.mu.Unlock()
			_, _ = w.Write([]byte(`{"code":0,"data":{"reward_credit":30}}`))
		case strings.HasSuffix(r.URL.Path, "/claim-gift"):
			_, _ = w.Write([]byte(`{"code":0,"data":{"credit":0}}`))
		case strings.Contains(r.URL.Path, "compensation"):
			_, _ = w.Write([]byte(`{"code":0,"data":{"active":false}}`))
		default:
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
		}
	}))
	t.Cleanup(fx.srv.Close)

	book := auth.NewAccountBook(dir)
	toks, err := auth.NewTokenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	cnDomain := config.EndpointOf("cn").Domain
	exp := time.Now().Add(24 * time.Hour).Unix()

	for i, a := range accounts {
		dom := a.domain
		if dom == "" {
			dom = cnDomain
		}
		tok := &auth.Token{
			UID:          a.uid,
			Nickname:     "nick-" + a.uid,
			AccessToken:  "tok-" + a.uid,
			RefreshToken: "ref-" + a.uid,
			TokenType:    "Bearer",
			Domain:       dom,
			ExpiresAt:    exp,
		}
		if err := book.Save(tok); err != nil {
			t.Fatalf("保存账号 %s 失败: %v", a.uid, err)
		}
		if i == 0 {
			if err := toks.Save(tok); err != nil {
				t.Fatal(err)
			}
		}
		if a.manualDisabled || a.autoDisabled != "" {
			if err := st.SavePoolAccount(&store.PoolAccountRow{
				UID:            a.uid,
				ManualDisabled: a.manualDisabled,
				AutoDisabled:   a.autoDisabled,
			}); err != nil {
				t.Fatal(err)
			}
		}
	}

	client := upstream.New(toks, func() string { return cfg.Get().Region }, 30)
	client.BillingBaseOverride = fx.srv.URL
	client.GrowthBaseOverride = fx.srv.URL
	client.WebBaseOverride = fx.srv.URL
	client.MPBaseOverride = fx.srv.URL
	client.AuthBaseOverride = fx.srv.URL
	fx.client = client

	p := pool.New(cfg, st, toks, client)
	t.Cleanup(func() { p.Close() })
	fx.p = p

	s := New(cfg, client, st, nil)
	t.Cleanup(s.Stop)
	s.SetPool(p)
	s.Reconfigure()
	fx.s = s
	return fx
}

func uidFromReq(r *http.Request) string {
	if authz := r.Header.Get("Authorization"); strings.HasPrefix(authz, "Bearer tok-") {
		return strings.TrimPrefix(authz, "Bearer tok-")
	}
	if u := r.Header.Get("X-User-Id"); u != "" {
		return u
	}
	return ""
}

// TestScopedCacheKeyHelpers 验证所有导出的按 UID 隔离缓存键辅助函数。
func TestScopedCacheKeyHelpers(t *testing.T) {
	if got := ScopedCacheKey("status", ""); got != "status" {
		t.Errorf("空 uid 应原样返回 base，得到 %q", got)
	}
	if got := ScopedCacheKey("status", "  "); got != "status" {
		t.Errorf("空白 uid 应原样返回 base，得到 %q", got)
	}
	if got := ScopedCacheKey("status", "u1"); got != "status:u1" {
		t.Errorf("非空 uid 应拼接冒号，得到 %q", got)
	}

	cases := map[string]string{
		CheckinStatusKey("u1"):       "status:u1",
		CheckinRandomTargetKey("u1"): "random_target:u1",
		GrowthOverviewKey("u1"):      "overview:u1",
		GrowthReportDayKey("u1"):     "report_day:u1",
		GrowthAdoptDayKey("u1"):      "adopt_day:u1",
		GrowthRewardDayKey("u1"):     "reward_day:u1",
		GrowthLastRunKey("u1"):       "last_run:u1",
		TaskLastSweepKey("u1"):       "task_last_sweep:u1",
		TaskLastQueueKey("u1"):       "task_last_queue:u1",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("期望 %q，得到 %q", want, got)
		}
	}
}

// TestMultiAccountAutoCheckin 验证多账号自动签到：
//   - 2 个正常 CN 账号 + 1 个手动禁用 CN 账号 全部按错峰时刻完成签到并写入各自 checkin_cache；
//   - 1 个凭证失效（AutoCredential）CN 账号 与 1 个海外域账号 被跳过。
func TestMultiAccountAutoCheckin(t *testing.T) {
	globalDom := config.EndpointOf("global").Domain
	fx := newMultiAccountFixture(t, []seedAccount{
		{uid: "cn-1"},
		{uid: "cn-2"},
		{uid: "cn-3-manual", manualDisabled: true},
		{uid: "cn-4-cred", autoDisabled: pool.AutoCredential},
		{uid: "gl-5", domain: globalDom},
	})

	// EligibleAccountUIDs 应恰好包含 3 个 CN 账号（含手动禁用）
	gotUIDs := fx.s.EligibleAccountUIDs()
	wantUIDs := []string{"cn-1", "cn-2", "cn-3-manual"}
	if !reflect.DeepEqual(gotUIDs, wantUIDs) {
		t.Fatalf("EligibleAccountUIDs = %v，期望 %v", gotUIDs, wantUIDs)
	}

	// fixed 09:00：idx=0 在 09:00 触发，idx=1 在 09:01，idx=2 在 09:02
	fx.s.tickCheckinAt(at(9, 0))
	fx.mu.Lock()
	c1At900 := fx.checkinClaims["cn-1"]
	c2At900 := fx.checkinClaims["cn-2"]
	c3At900 := fx.checkinClaims["cn-3-manual"]
	fx.mu.Unlock()
	if c1At900 != 1 || c2At900 != 0 || c3At900 != 0 {
		t.Fatalf("09:00 应仅错峰触发 idx=0(cn-1)，得到 cn-1=%d cn-2=%d cn-3=%d", c1At900, c2At900, c3At900)
	}

	fx.s.tickCheckinAt(at(9, 1))
	fx.s.tickCheckinAt(at(9, 2))
	// 再 tick 一次：均已 secured，不得重复领取
	fx.s.tickCheckinAt(at(9, 3))

	fx.mu.Lock()
	defer fx.mu.Unlock()
	for _, uid := range wantUIDs {
		if fx.checkinClaims[uid] != 1 {
			t.Errorf("账号 %s 应签到恰好 1 次，得到 %d", uid, fx.checkinClaims[uid])
		}
		if payload, _, ok := fx.st.GetCache(CheckinCacheTable, CheckinStatusKey(uid), 0); !ok || payload == "" {
			t.Errorf("账号 %s 应写入 %s 缓存", uid, CheckinStatusKey(uid))
		}
	}
	for _, skipped := range []string{"cn-4-cred", "gl-5"} {
		if fx.checkinClaims[skipped] != 0 {
			t.Errorf("账号 %s 应跳过签到，实际调用 %d 次", skipped, fx.checkinClaims[skipped])
		}
	}
}

// TestMultiAccountCheckinRandomTargetsIsolated random 模式下每个账号独立持久化 random_target:<uid>。
func TestMultiAccountCheckinRandomTargetsIsolated(t *testing.T) {
	fx := newMultiAccountFixture(t, []seedAccount{
		{uid: "r-1"},
		{uid: "r-2"},
	})
	if err := fx.cfg.Update(func(c *config.Config) error {
		c.CheckinMode = "random"
		c.CheckinRandomStart = "09:00"
		c.CheckinRandomEnd = "18:00"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	fx.s.Reconfigure()
	fx.s.tickCheckinAt(at(8, 0)) // 窗口前，仅初始化各自 random_target

	for _, uid := range []string{"r-1", "r-2"} {
		payload, _, ok := fx.st.GetCache(CheckinCacheTable, CheckinRandomTargetKey(uid), randomTargetTTL)
		if !ok || payload == "" {
			t.Fatalf("账号 %s 应生成独立 %s 缓存", uid, CheckinRandomTargetKey(uid))
		}
	}
}

// TestClaimCheckinFor 验证导出方法 ClaimCheckinFor 支持按 uid 手动签到并防重复。
func TestClaimCheckinFor(t *testing.T) {
	fx := newMultiAccountFixture(t, []seedAccount{
		{uid: "u-a"},
		{uid: "u-b"},
	})
	credit, already, err := fx.s.ClaimCheckinFor("u-b")
	if err != nil || already || credit != 50 {
		t.Fatalf("首次为 u-b 签到应成功 +50，得到 credit=%v already=%v err=%v", credit, already, err)
	}
	// 第二次应识别为已签
	_, already2, err2 := fx.s.ClaimCheckinFor("u-b")
	if err2 != nil || !already2 {
		t.Fatalf("二次签到应返回 alreadyChecked=true，得到 already=%v err=%v", already2, err2)
	}
	// u-a 不受影响
	fx.mu.Lock()
	aClaims := fx.checkinClaims["u-a"]
	fx.mu.Unlock()
	if aClaims != 0 {
		t.Errorf("u-a 不应被 u-b 的签到影响，得到 %d", aClaims)
	}
}

// TestMultiAccountAutoGrowthChainTravelAndCatchup 验证多账号自动成长全链路：
//   - tickGrowthChain 覆盖正常 + 手动禁用 CN 账号，跳过凭证失效与海外账号，且按 UID 隔离缓存；
//   - tickTravel 按账号状态分别 claim / depart；
//   - growthCatchupDue / growthCatchupFire 仅补跑今日未上报的账号，不重跑已完成账号。
func TestMultiAccountAutoGrowthChainTravelAndCatchup(t *testing.T) {
	globalDom := config.EndpointOf("global").Domain
	fx := newMultiAccountFixture(t, []seedAccount{
		{uid: "g-1"},
		{uid: "g-2"},
		{uid: "g-3-manual", manualDisabled: true},
		{uid: "g-4-cred", autoDisabled: pool.AutoCredential},
		{uid: "g-5-global", domain: globalDom},
	})
	fx.travelStates["g-1"] = "arrived"
	fx.travelStates["g-2"] = "idle"
	fx.travelStates["g-3-manual"] = "idle"

	// 1) tickGrowthChain 遍历全部合规账号
	fx.s.tickGrowthChain()

	day := mustDay(t)
	fx.mu.Lock()
	for _, uid := range []string{"g-1", "g-2", "g-3-manual"} {
		if fx.reportCalls[uid] != 2 {
			t.Errorf("账号 %s 应上报 2 条，得到 %d", uid, fx.reportCalls[uid])
		}
		if payload, _, ok := fx.st.GetCache(GrowthCacheTable, GrowthReportDayKey(uid), growthDayTTL); !ok || payload != day {
			t.Errorf("账号 %s 应写入 %s=%s，得到 ok=%v payload=%q", uid, GrowthReportDayKey(uid), day, ok, payload)
		}
		if payload, _, ok := fx.st.GetCache(GrowthCacheTable, GrowthLastRunKey(uid), 0); !ok || payload == "" {
			t.Errorf("账号 %s 应写入 %s", uid, GrowthLastRunKey(uid))
		}
	}
	for _, skipped := range []string{"g-4-cred", "g-5-global"} {
		if fx.reportCalls[skipped] != 0 {
			t.Errorf("账号 %s 应跳过成长链，实际调用 %d 次", skipped, fx.reportCalls[skipped])
		}
	}
	if fx.travelClaims["g-1"] != 1 || fx.travelDeparts["g-2"] != 1 || fx.travelDeparts["g-3-manual"] != 1 {
		t.Errorf("链内旅行动作异常: claims=%v departs=%v", fx.travelClaims, fx.travelDeparts)
	}
	fx.mu.Unlock()

	// 2) tickTravel 独立巡检同样遍历全部合规账号
	fx.s.tickTravel()
	fx.mu.Lock()
	if fx.travelClaims["g-1"] != 2 || fx.travelDeparts["g-2"] != 2 || fx.travelDeparts["g-3-manual"] != 2 {
		t.Errorf("独立旅行巡检应覆盖全部合规账号: claims=%v departs=%v", fx.travelClaims, fx.travelDeparts)
	}
	if fx.travelDeparts["g-4-cred"] != 0 || fx.travelDeparts["g-5-global"] != 0 {
		t.Errorf("不合规账号不应触发旅行巡检: departs=%v", fx.travelDeparts)
	}
	fx.mu.Unlock()

	// 3) catchup：全部已完成时 growthCatchupDue=false；清掉 g-2 的 report_day 后应为 true，
	//    且 growthCatchupFire 只补跑 g-2，不重跑 g-1/g-3-manual。
	if err := fx.cfg.Update(func(c *config.Config) error { c.AutoGrowth = true; return nil }); err != nil {
		t.Fatal(err)
	}
	nowAfterCron := time.Date(time.Now().Year(), time.Now().Month(), time.Now().Day(), 12, 0, 0, 0, time.Local)
	if fx.s.growthCatchupDue(fx.cfg.Get(), nowAfterCron) {
		t.Fatal("所有账号已完成今日上报时不应触发补跑")
	}
	_ = fx.st.DeleteCacheKey(GrowthCacheTable, GrowthReportDayKey("g-2"))
	mu := fx.s.growthLock("g-2")
	mu.Lock()
	fx.s.growthStateOf("g-2").reportDone = false
	mu.Unlock()

	if !fx.s.growthCatchupDue(fx.cfg.Get(), nowAfterCron) {
		t.Fatal("存在未完成今日上报的账号(g-2)时应判定需要补跑")
	}
	fx.s.growthCatchupFire()
	fx.mu.Lock()
	defer fx.mu.Unlock()
	if fx.reportCalls["g-2"] != 4 {
		t.Errorf("补跑应为 g-2 补发 2 条（累计 4），得到 %d", fx.reportCalls["g-2"])
	}
	if fx.reportCalls["g-1"] != 2 || fx.reportCalls["g-3-manual"] != 2 {
		t.Errorf("已完成账号不应被补跑重发: g-1=%d g-3=%d", fx.reportCalls["g-1"], fx.reportCalls["g-3-manual"])
	}
}

// TestRunGrowthChainForIsolation 验证按 UID 手动触发的完全隔离：
//   - RunGrowthChainFor("iso-b") 只作用于 iso-b，不影响 iso-a；
//   - iso-a 锁被占用时仅 iso-a 返回 busy，iso-b 仍可并发执行。
func TestRunGrowthChainForIsolation(t *testing.T) {
	fx := newMultiAccountFixture(t, []seedAccount{
		{uid: "iso-a"},
		{uid: "iso-b"},
	})

	// 锁住 iso-a：iso-a 返回 busy，但 iso-b 正常执行！
	muA := fx.s.growthLock("iso-a")
	muA.Lock()
	defer muA.Unlock()

	resA := fx.s.RunGrowthChainFor("iso-a")
	if resA.Ran || resA.Reason != "busy" {
		t.Fatalf("iso-a 持锁期间应返回 busy，得到 %+v", resA)
	}
	resB := fx.s.RunGrowthChainFor("iso-b")
	if !resB.Ran || resB.UID != "iso-b" {
		t.Fatalf("iso-a 持锁不应阻塞 iso-b，得到 %+v", resB)
	}

	fx.mu.Lock()
	defer fx.mu.Unlock()
	if fx.reportCalls["iso-a"] != 0 || fx.reportCalls["iso-b"] != 2 {
		t.Errorf("应只上报 iso-b：iso-a=%d iso-b=%d", fx.reportCalls["iso-a"], fx.reportCalls["iso-b"])
	}
	if _, _, ok := fx.st.GetCache(GrowthCacheTable, GrowthReportDayKey("iso-a"), growthDayTTL); ok {
		t.Error("iso-a 不应有 report_day 缓存")
	}
	if payload, _, ok := fx.st.GetCache(GrowthCacheTable, GrowthReportDayKey("iso-b"), growthDayTTL); !ok || payload != mustDay(t) {
		t.Errorf("iso-b 应写入独立 report_day 缓存，得到 ok=%v payload=%q", ok, payload)
	}
}

// TestMultiAccountTaskForHooks 验证 SetTaskQueueForHook / SetTaskSweepForHook 串行遍历全部合规账号并汇总。
func TestMultiAccountTaskForHooks(t *testing.T) {
	fx := newMultiAccountFixture(t, []seedAccount{
		{uid: "t-1"},
		{uid: "t-2", manualDisabled: true},
		{uid: "t-3-cred", autoDisabled: pool.AutoCredential},
	})

	var queueSeen []string
	fx.s.SetTaskQueueForHook(func(uid string) {
		queueSeen = append(queueSeen, uid)
	})
	fx.s.tickTaskQueue()
	sort.Strings(queueSeen)
	if !reflect.DeepEqual(queueSeen, []string{"t-1", "t-2"}) {
		t.Errorf("TaskQueueForHook 应遍历 [t-1 t-2]，得到 %v", queueSeen)
	}

	var sweepSeen []string
	fx.s.SetTaskSweepForHook(func(uid string) taskops.SweepStats {
		sweepSeen = append(sweepSeen, uid)
		return taskops.SweepStats{Ran: true, Accepted: 1, Claimed: 2, Credits: 100, Energy: 10}
	})
	agg := fx.s.RunTaskSweepOnce()
	sort.Strings(sweepSeen)
	if !reflect.DeepEqual(sweepSeen, []string{"t-1", "t-2"}) {
		t.Errorf("TaskSweepForHook 应遍历 [t-1 t-2]，得到 %v", sweepSeen)
	}
	if !agg.Ran || agg.Accepted != 2 || agg.Claimed != 4 || agg.Credits != 200 || agg.Energy != 20 {
		t.Errorf("补领清扫汇总异常: %+v", agg)
	}
}
