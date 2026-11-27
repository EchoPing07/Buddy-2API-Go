package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"buddy2api-go/internal/auth"
	"buddy2api-go/internal/config"
	"buddy2api-go/internal/pool"
	"buddy2api-go/internal/scheduler"
	"buddy2api-go/internal/store"
	"buddy2api-go/internal/taskops"
	"buddy2api-go/internal/upstream"
)

type multiTestAccount struct {
	uid          string
	domain       string
	nickname     string
	remark       string
	enterpriseID string
	refreshToken string
}

// newMultiAccountFixture 创建多账号测试环境（含真实 pool.Manager 与真实 *scheduler.Scheduler）。
// 第一个账号写入 token.json（主账号），其余全部写入 accounts/<id>.json。
func newMultiAccountFixture(t *testing.T, region string, accts ...multiTestAccount) (*Handler, *store.Store, *scheduler.Scheduler) {
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
	if region != "" && region != "cn" {
		if err := cfg.Update(func(c *config.Config) error { c.Region = region; return nil }); err != nil {
			t.Fatal(err)
		}
	}

	book := auth.NewAccountBook(dir)
	for i, a := range accts {
		dom := a.domain
		if dom == "" {
			dom = "www.codebuddy.cn"
		}
		nn := a.nickname
		if nn == "" {
			nn = "昵称-" + a.uid
		}
		tok := &auth.Token{
			UID:          a.uid,
			AccessToken:  "at-" + a.uid,
			RefreshToken: a.refreshToken,
			TokenType:    "Bearer",
			Domain:       dom,
			Nickname:     nn,
			Remark:       a.remark,
			EnterpriseID: a.enterpriseID,
			ExpiresAt:    time.Now().Add(2 * time.Hour).Unix(),
		}
		if err := book.Save(tok); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			ts, err := auth.NewTokenStorePath(filepath.Join(dir, "token.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := ts.Save(tok); err != nil {
				t.Fatal(err)
			}
		}
	}

	toks, err := auth.NewTokenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	client := upstream.New(toks, func() string { return cfg.Get().Region }, 30)
	// 不传 client 给 pool.New，避免后台异步未探测 goroutine 抢先打测试服务器
	poolMgr := pool.New(cfg, st, toks, nil)
	t.Cleanup(func() { poolMgr.Close() })

	sched := scheduler.New(cfg, client, st, nil)
	sched.SetPool(poolMgr)
	sched.Reconfigure()
	t.Cleanup(func() { sched.Stop() })

	h := New(cfg, st, toks, client, nil, nil, sched, nil)
	h.SetPool(poolMgr)
	sched.SetTaskSweepHook(h.RunTaskSweep)
	sched.SetTaskQueueHook(h.RunTaskQueueOnce)
	sched.SetTaskSweepForHook(h.RunTaskSweepFor)
	sched.SetTaskQueueForHook(h.RunTaskQueueFor)
	return h, st, sched
}

func TestMultiAccountRoutesRegistered(t *testing.T) {
	h, _, _ := newMultiAccountFixture(t, "cn", multiTestAccount{uid: "acc1"})
	r := chi.NewRouter()
	r.Route("/admin", h.Routes())
	got := map[string]bool{}
	if err := chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		got[method+" "+route] = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"PUT /admin/pool/strategy",
		"GET /admin/checkin/summary",
		"POST /admin/checkin/claim_all",
	} {
		if !got[want] {
			t.Errorf("路由缺少 %q", want)
		}
	}
}

func TestResourcesSingleAndAllWithCacheAndForce(t *testing.T) {
	h, st, _ := newMultiAccountFixture(t, "cn",
		multiTestAccount{uid: "acc1", remark: "主号"},
		multiTestAccount{uid: "acc2", remark: "临期号"},
		multiTestAccount{uid: "acc-global", domain: "www.codebuddy.ai", remark: "国际号"},
	)

	now := time.Now()
	expSoon := now.Add(3 * 24 * time.Hour).Format("2006-01-02 15:04:05")
	expLater := now.Add(20 * 24 * time.Hour).Format("2006-01-02 15:04:05")

	var mu sync.Mutex
	hits := map[string]int{}
	var failAcc2 bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHdr := r.Header.Get("Authorization")
		mu.Lock()
		hits[authHdr]++
		shouldFail := failAcc2 && authHdr == "Bearer at-acc2"
		mu.Unlock()

		if shouldFail {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"code":502,"msg":"upstream down"}`))
			return
		}
		switch authHdr {
		case "Bearer at-acc1":
			_, _ = w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{
				"TotalDosage":200,
				"Accounts":[
					{"PackageName":"主号无到期包","CapacityRemainPrecise":"100","CapacitySizePrecise":"100","ExpiredTime":""},
					{"PackageName":"主号20天包","CapacityRemainPrecise":"100","CapacitySizePrecise":"100","ExpiredTime":"` + expLater + `"}
				]
			}}}}`))
		case "Bearer at-acc2":
			_, _ = w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{
				"TotalDosage":50,
				"Accounts":[
					{"PackageName":"临期3天包","CapacityRemainPrecise":"50","CapacitySizePrecise":"100","ExpiredTime":"` + expSoon + `"}
				]
			}}}}`))
		default:
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	defer srv.Close()
	h.client.BillingBaseOverride = srv.URL

	// 1) 未知 uid → 404
	rr404 := httptest.NewRecorder()
	h.resources(rr404, httptest.NewRequest(http.MethodGet, "/admin/resources?uid=ghost", nil))
	if rr404.Code != http.StatusNotFound {
		t.Fatalf("未知 uid 应 404，得到 %d: %s", rr404.Code, rr404.Body.String())
	}

	// 2) 单账号查询 ?uid=acc2 → mode=single，写 v4:acc2 缓存，同步更新号池额度与最早到期
	_ = st.SetCache("resource_cache", "default_v3", `{"stale":true}`)
	rrSingle := httptest.NewRecorder()
	h.resources(rrSingle, httptest.NewRequest(http.MethodGet, "/admin/resources?uid=acc2", nil))
	if rrSingle.Code != http.StatusOK {
		t.Fatalf("单账号查询失败 %d: %s", rrSingle.Code, rrSingle.Body.String())
	}
	var singleResp struct {
		Mode   string         `json:"mode"`
		Cached bool           `json:"cached"`
		UID    string         `json:"uid"`
		Label  string         `json:"label"`
		Data   map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rrSingle.Body.Bytes(), &singleResp); err != nil {
		t.Fatal(err)
	}
	if singleResp.Mode != "single" || singleResp.Cached || singleResp.UID != "acc2" || singleResp.Label != "临期号" {
		t.Errorf("single 响应字段异常: %+v", singleResp)
	}
	if _, _, ok := st.GetCache("resource_cache", "default_v3", 0); ok {
		t.Error("写入 v4 缓存后应清理旧版 default_v3")
	}
	if _, _, ok := st.GetCache("resource_cache", resourceCacheKey("acc2"), 0); !ok {
		t.Error("应写入 v4:acc2 缓存")
	}
	if stPool, ok := h.pool.AccountStateOf("acc2"); !ok || stPool.Credits != 50 || stPool.EarliestExpireAt <= 0 {
		t.Errorf("单账号拉取应同步更新 pool quota summary: ok=%v st=%+v", ok, stPool)
	}
	// pkg 应包含 expire_at
	rawAccs, _ := singleResp.Data["accounts"].([]any)
	if len(rawAccs) != 1 || rawAccs[0].(map[string]any)["expire_at"].(float64) <= 0 {
		t.Errorf("单账号额度包应含 expire_at: %+v", rawAccs)
	}

	// 3) 全部聚合 ?uid=all：acc2 已缓存不重打上游，acc1 未缓存打一次，国际号被过滤
	rrAll := httptest.NewRecorder()
	h.resources(rrAll, httptest.NewRequest(http.MethodGet, "/admin/resources?uid=all", nil))
	if rrAll.Code != http.StatusOK {
		t.Fatalf("all 模式失败 %d: %s", rrAll.Code, rrAll.Body.String())
	}
	var allResp struct {
		Mode             string                   `json:"mode"`
		TotalDosage      float64                  `json:"total_dosage"`
		Expiring7d       float64                  `json:"expiring_7d"`
		EarliestExpireAt int64                    `json:"earliest_expire_at"`
		AccountsSummary  []resourceAccountSummary `json:"accounts_summary"`
		Accounts         []mergedResourceAccount  `json:"accounts"`
	}
	if err := json.Unmarshal(rrAll.Body.Bytes(), &allResp); err != nil {
		t.Fatal(err)
	}
	if allResp.Mode != "all" || allResp.TotalDosage != 250 || allResp.Expiring7d != 50 || allResp.EarliestExpireAt <= 0 {
		t.Errorf("all 聚合数值异常: %+v", allResp)
	}
	if len(allResp.AccountsSummary) != 2 {
		t.Fatalf("应仅包含 2 个 CN 账号（过滤国际号），实际 %+v", allResp.AccountsSummary)
	}
	if len(allResp.Accounts) != 3 {
		t.Fatalf("应合并 3 个额度包，实际 %d", len(allResp.Accounts))
	}
	// 排序校验：3 天到期在前，20 天其次，无到期（expire_at=0）最后
	if allResp.Accounts[0].AccountUID != "acc2" || allResp.Accounts[0].AccountLabel != "临期号" ||
		allResp.Accounts[2].ExpireAt != 0 {
		t.Errorf("合并排序异常（应按 expire_at 升序且无到期在后）: %+v", allResp.Accounts)
	}
	mu.Lock()
	if hits["Bearer at-acc2"] != 1 || hits["Bearer at-acc1"] != 1 || hits["Bearer at-acc-global"] != 0 {
		t.Errorf("缓存/区域过滤命中次数异常: %+v", hits)
	}
	mu.Unlock()

	// 4) force=1 + 单账号故障容忍：acc2 失败填 error，acc1 仍正常汇总
	mu.Lock()
	failAcc2 = true
	mu.Unlock()
	rrForce := httptest.NewRecorder()
	h.resources(rrForce, httptest.NewRequest(http.MethodGet, "/admin/resources?uid=all&force=1", nil))
	if rrForce.Code != http.StatusOK {
		t.Fatalf("部分账号失败时 all 仍应 200 容错，得到 %d", rrForce.Code)
	}
	var forceResp struct {
		TotalDosage     float64                  `json:"total_dosage"`
		AccountsSummary []resourceAccountSummary `json:"accounts_summary"`
	}
	if err := json.Unmarshal(rrForce.Body.Bytes(), &forceResp); err != nil {
		t.Fatal(err)
	}
	if forceResp.TotalDosage != 200 {
		t.Errorf("acc2 失败时总计应只计 acc1 的 200，得到 %v", forceResp.TotalDosage)
	}
	var sawErr bool
	for _, s := range forceResp.AccountsSummary {
		if s.UID == "acc2" && s.Error != "" {
			sawErr = true
		}
	}
	if !sawErr {
		t.Errorf("acc2 summary 应记录 error: %+v", forceResp.AccountsSummary)
	}
	mu.Lock()
	if hits["Bearer at-acc1"] != 2 || hits["Bearer at-acc2"] != 2 {
		t.Errorf("force=1 应强制重打两号，hits=%+v", hits)
	}
	mu.Unlock()
}

func TestCheckinSummaryAndClaimAll(t *testing.T) {
	h, st, _ := newMultiAccountFixture(t, "cn",
		multiTestAccount{uid: "acc1", remark: "已签主号"},
		multiTestAccount{uid: "acc2", remark: "禁用但待签"},
		multiTestAccount{uid: "acc-cred", remark: "凭证失效"},
		multiTestAccount{uid: "acc-global", domain: "www.codebuddy.ai", remark: "国际号"},
	)
	if err := h.pool.SetManualDisabled("acc2", true); err != nil {
		t.Fatal(err)
	}
	// acc-cred 标为凭证失效（应从 EligibleAccountUIDs 剔除）
	h.pool.NoteError("acc-cred", "gpt-4o", upstream.ErrKindCredential, []byte(`{"error":"unauthorized"}`))

	// 为 acc2 预置今日随机签到时刻
	rt := time.Now().Format(time.RFC3339)
	_ = st.SetCache(scheduler.CheckinCacheTable, scheduler.CheckinRandomTargetKey("acc2"), rt)

	var mu sync.Mutex
	checked := map[string]bool{"acc1": true, "acc2": false}
	claimCalls := map[string]int{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer at-")
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/v2/billing/meter/checkin-activity-status":
			isChecked := checked[uid]
			credit := 0.0
			streak := 2
			if isChecked {
				credit = 15
				streak = 3
			}
			_, _ = fmt.Fprintf(w, `{"code":0,"data":{"active":true,"today_checked_in":%v,"today_credit":%.0f,"streak_days":%d}}`,
				isChecked, credit, streak)
		case "/v2/billing/meter/daily-checkin":
			claimCalls[uid]++
			checked[uid] = true
			_, _ = w.Write([]byte(`{"code":0,"data":{"credit":20,"today_checked_in":true}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	h.client.BillingBaseOverride = srv.URL

	// 1) status?uid=unknown → 404
	rr404 := httptest.NewRecorder()
	h.checkinStatus(rr404, httptest.NewRequest(http.MethodGet, "/admin/checkin/status?uid=nope", nil))
	if rr404.Code != http.StatusNotFound {
		t.Fatalf("未知 uid 应 404，得到 %d", rr404.Code)
	}

	// 2) status?uid=acc2 → 读取 random_target:acc2 且写入 status:acc2
	rrSt := httptest.NewRecorder()
	h.checkinStatus(rrSt, httptest.NewRequest(http.MethodGet, "/admin/checkin/status?uid=acc2", nil))
	if rrSt.Code != http.StatusOK {
		t.Fatalf("checkinStatus?uid=acc2 失败 %d: %s", rrSt.Code, rrSt.Body.String())
	}
	if !strings.Contains(rrSt.Body.String(), `"random_target":"`+time.Now().Format("15:04")+`"`) {
		t.Errorf("应附带 acc2 的 random_target: %s", rrSt.Body.String())
	}
	if _, _, ok := st.GetCache(scheduler.CheckinCacheTable, scheduler.CheckinStatusKey("acc2"), 0); !ok {
		t.Error("应写入 status:acc2 缓存")
	}

	// 3) summary：包含 acc1 + 手动禁用的 acc2；排除 acc-cred 和 acc-global
	rrSum := httptest.NewRecorder()
	h.checkinSummary(rrSum, httptest.NewRequest(http.MethodGet, "/admin/checkin/summary", nil))
	if rrSum.Code != http.StatusOK {
		t.Fatalf("summary 失败 %d: %s", rrSum.Code, rrSum.Body.String())
	}
	var sumResp checkinSummaryResp
	if err := json.Unmarshal(rrSum.Body.Bytes(), &sumResp); err != nil {
		t.Fatal(err)
	}
	if sumResp.TotalCount != 2 || sumResp.CheckedCount != 1 {
		t.Fatalf("summary 计数异常（应 1/2，含手动禁用 acc2，排除失效/国际号）: %+v", sumResp)
	}

	// 4) claim_all：跳过 acc1，仅为 acc2 签到，返回汇总 2/2
	rrAll := httptest.NewRecorder()
	h.checkinClaimAll(rrAll, httptest.NewRequest(http.MethodPost, "/admin/checkin/claim_all", nil))
	if rrAll.Code != http.StatusOK {
		t.Fatalf("claim_all 失败 %d: %s", rrAll.Code, rrAll.Body.String())
	}
	var allResp struct {
		OK      bool               `json:"ok"`
		Claimed int                `json:"claimed"`
		Skipped int                `json:"skipped"`
		Failed  int                `json:"failed"`
		Summary checkinSummaryResp `json:"summary"`
	}
	if err := json.Unmarshal(rrAll.Body.Bytes(), &allResp); err != nil {
		t.Fatal(err)
	}
	if !allResp.OK || allResp.Claimed != 1 || allResp.Skipped != 1 || allResp.Failed != 0 {
		t.Errorf("claim_all 计数异常: %+v", allResp)
	}
	if allResp.Summary.CheckedCount != 2 || allResp.Summary.TotalCount != 2 {
		t.Errorf("claim_all 后 summary 应更新为 2/2: %+v", allResp.Summary)
	}
	mu.Lock()
	if claimCalls["acc1"] != 0 || claimCalls["acc2"] != 1 {
		t.Errorf("claim 调用次数异常: %+v", claimCalls)
	}
	mu.Unlock()
}

func TestGrowthAndTasksUIDRoutingAndHooks(t *testing.T) {
	restoreThrottle := taskops.ThrottleTuning(-1, -1, -1, -1)
	t.Cleanup(restoreThrottle)
	restorePoll := upstream.TaskPollTuning(time.Millisecond, 2)
	t.Cleanup(restorePoll)

	h, st, _ := newMultiAccountFixture(t, "cn",
		multiTestAccount{uid: "acc1", remark: "一号"},
		multiTestAccount{uid: "acc2", remark: "二号"},
	)

	var mu sync.Mutex
	seenAuth := map[string][]string{}
	claimedByUID := map[string]int{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer at-")
		mu.Lock()
		seenAuth[r.URL.Path] = append(seenAuth[r.URL.Path], uid)
		mu.Unlock()

		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/activity/growth/streak"):
			days := 3
			if uid == "acc2" {
				days = 14
			}
			_, _ = fmt.Fprintf(w, `{"code":0,"data":{"streak":{"days":%d},"makeup_cards":{"balance":1,"max":3},"redemption":{"remaining_days":10,"tiers":[]}}}`, days)
		case strings.HasSuffix(p, "/activity/growth/heatmap"):
			_, _ = w.Write([]byte(`{"code":0,"data":{"cells":[]}}`))
		case strings.HasSuffix(p, "/activity/growth/buddy/info"):
			_, _ = w.Write([]byte(`{"code":0,"data":null}`))
		case strings.HasSuffix(p, "/activity/growth/lottery/chances"):
			_, _ = w.Write([]byte(`{"code":0,"data":{"balance":1}}`))
		case strings.HasSuffix(p, "/activity/growth/lottery/draw"):
			_, _ = w.Write([]byte(`{"code":0,"data":{"prize_name":"6积分","prize_type":"credit","credit_amount":6}}`))
		case strings.HasSuffix(p, "/tasks") && r.Header.Get("X-Client-Platform") == "miniprogram":
			_, _ = w.Write([]byte(`{"code":0,"data":{"tasks":[]}}`))
		case strings.HasSuffix(p, "/tasks"):
			_, _ = w.Write([]byte(`{"code":0,"data":{"tasks":[{"task_code":"automation_1","title":"定时","reward_credit":100,"target":1,"current":1}]}}`))
		case strings.HasSuffix(p, "/claim"):
			mu.Lock()
			claimedByUID[uid]++
			mu.Unlock()
			_, _ = w.Write([]byte(`{"code":0,"data":{"credit":100,"energy":5}}`))
		case strings.HasSuffix(p, "/billing/meter/compensation-status"):
			_, _ = w.Write([]byte(`{"code":0,"data":{"active":false}}`))
		default:
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
		}
	}))
	defer srv.Close()
	h.client.GrowthBaseOverride = srv.URL
	h.client.WebBaseOverride = srv.URL
	h.client.MPBaseOverride = srv.URL

	// 1) 未知 uid → 404（覆盖 growth + tasks + compensation）
	for _, fn := range []http.HandlerFunc{
		h.growthOverview, h.growthRun, h.growthLottery, h.tasksList, h.tasksSweep, h.compensation,
	} {
		rr := httptest.NewRecorder()
		fn(rr, httptest.NewRequest(http.MethodGet, "/x?uid=missing", nil))
		if rr.Code != http.StatusNotFound {
			t.Errorf("未知 uid 应 404，得到 %d: %s", rr.Code, rr.Body.String())
		}
	}

	// 2) growthOverview?uid=acc2 走 acc2 凭证，并隔离缓存 overview:acc2
	_ = st.SetCache(growthCacheTable, scheduler.GrowthOverviewKey("acc1"), `{"available":true,"streak":{"days":1}}`)
	rrOv := httptest.NewRecorder()
	h.growthOverview(rrOv, httptest.NewRequest(http.MethodGet, "/admin/growth/overview?uid=acc2", nil))
	if rrOv.Code != http.StatusOK || !strings.Contains(rrOv.Body.String(), `"days":14`) {
		t.Fatalf("overview?uid=acc2 应取 acc2 数据 (days=14)，得到 %d: %s", rrOv.Code, rrOv.Body.String())
	}
	if p1, _, ok := st.GetCache(growthCacheTable, scheduler.GrowthOverviewKey("acc1"), 0); !ok || !strings.Contains(p1, `"days":1`) {
		t.Error("查询 acc2 不得覆盖 acc1 缓存")
	}

	// 3) growthLottery?uid=acc2 走 acc2 凭证
	rrLot := httptest.NewRecorder()
	h.growthLottery(rrLot, httptest.NewRequest(http.MethodPost, "/admin/growth/lottery?uid=acc2", nil))
	if rrLot.Code != http.StatusOK || !strings.Contains(rrLot.Body.String(), `"status":"ok"`) {
		t.Fatalf("lottery?uid=acc2 失败: %d %s", rrLot.Code, rrLot.Body.String())
	}

	// 4) tasksSweep?uid=acc2 与 RunTaskSweepFor("acc2") 使用 acc2 凭证且只写 task_last_sweep:acc2
	rrSw := httptest.NewRecorder()
	h.tasksSweep(rrSw, httptest.NewRequest(http.MethodPost, "/admin/tasks/sweep?uid=acc2", nil))
	if rrSw.Code != http.StatusOK || !strings.Contains(rrSw.Body.String(), `"claimed":1`) {
		t.Fatalf("tasksSweep?uid=acc2 失败: %d %s", rrSw.Code, rrSw.Body.String())
	}
	if _, _, ok := st.GetCache(taskCacheTable, scheduler.TaskLastSweepKey("acc2"), 0); !ok {
		t.Error("sweep?uid=acc2 应写入 task_last_sweep:acc2")
	}
	if _, _, ok := st.GetCache(taskCacheTable, scheduler.TaskLastSweepKey("acc1"), 0); ok {
		t.Error("sweep?uid=acc2 不得写入 task_last_sweep:acc1")
	}

	// RunTaskSweepFor("acc2") 直接调用
	sw2 := h.RunTaskSweepFor("acc2")
	if !sw2.Ran || sw2.Claimed != 1 {
		t.Errorf("RunTaskSweepFor(acc2) 异常: %+v", sw2)
	}
	mu.Lock()
	if claimedByUID["acc2"] != 2 || claimedByUID["acc1"] != 0 {
		t.Errorf("claim 应全部落在 acc2，得到 %+v", claimedByUID)
	}
	mu.Unlock()

	// 5) compensation?uid=acc2 走 acc2 凭证
	rrComp := httptest.NewRecorder()
	h.compensation(rrComp, httptest.NewRequest(http.MethodGet, "/admin/compensation?uid=acc2", nil))
	if rrComp.Code != http.StatusOK || !strings.Contains(rrComp.Body.String(), `"ok":true`) {
		t.Fatalf("compensation?uid=acc2 失败: %d %s", rrComp.Code, rrComp.Body.String())
	}
}

func TestAccountLifecyclePoolReloadAndScopedCacheIsolation(t *testing.T) {
	h, st, _ := newMultiAccountFixture(t, "cn",
		multiTestAccount{uid: "acc1", remark: "一号", enterpriseID: "ent1", refreshToken: "rt1"},
		multiTestAccount{uid: "acc2", remark: "二号", refreshToken: "rt2"},
	)

	// 为 acc2 写入各类 scoped 缓存与池额度
	h.pool.SetCredits("acc2", 88.5, time.Now().Unix())
	_ = st.SetCache("resource_cache", resourceCacheKey("acc2"), `{"total_dosage":88.5}`)
	_ = st.SetCache(scheduler.CheckinCacheTable, scheduler.CheckinStatusKey("acc2"), `{"data":{"today_checked_in":true}}`)
	_ = st.SetCache(scheduler.CheckinCacheTable, scheduler.CheckinRandomTargetKey("acc2"), time.Now().Format(time.RFC3339))
	_ = st.SetCache(scheduler.GrowthCacheTable, scheduler.GrowthOverviewKey("acc2"), `{"available":true}`)
	_ = st.SetCache(scheduler.GrowthCacheTable, scheduler.GrowthReportDayKey("acc2"), "2026-01-01")
	_ = st.SetCache(scheduler.GrowthCacheTable, scheduler.TaskLastSweepKey("acc2"), `{"ran":true}`)
	_ = st.SetCache(scheduler.GrowthCacheTable, scheduler.TaskLastQueueKey("acc2"), `{"steps":1}`)

	// 1) /admin/accounts 无需 PoolEnabled 即可返回池状态与企业/刷新/额度字段
	rrList := httptest.NewRecorder()
	h.listAccounts(rrList, httptest.NewRequest(http.MethodGet, "/admin/accounts", nil))
	if rrList.Code != http.StatusOK {
		t.Fatalf("listAccounts 失败 %d", rrList.Code)
	}
	var listResp struct {
		Items []accountItem `json:"items"`
	}
	if err := json.Unmarshal(rrList.Body.Bytes(), &listResp); err != nil {
		t.Fatal(err)
	}
	byUID := map[string]accountItem{}
	for _, it := range listResp.Items {
		byUID[it.UID] = it
	}
	if !byUID["acc1"].InPool || !byUID["acc1"].PoolAvailable || byUID["acc1"].EnterpriseID != "ent1" || !byUID["acc1"].HasRefresh {
		t.Errorf("acc1 /admin/accounts 字段异常: %+v", byUID["acc1"])
	}
	if !byUID["acc2"].InPool || byUID["acc2"].Credits != 88.5 {
		t.Errorf("acc2 /admin/accounts 应透出池内额度: %+v", byUID["acc2"])
	}

	// 2) 导入新账号（仅导入）应 ReloadMembers，立即出现在号池成员中
	impBody := `{"access_token":"` + testJWT(t, "acc3", "三号") + `"}`
	rrImp := httptest.NewRecorder()
	h.accountImport(rrImp, httptest.NewRequest(http.MethodPost, "/admin/account/import", strings.NewReader(impBody)))
	if rrImp.Code != http.StatusOK {
		t.Fatalf("导入失败 %d: %s", rrImp.Code, rrImp.Body.String())
	}
	if _, ok := h.pool.AccountStateOf("acc3"); !ok {
		t.Errorf("导入后号池应 ReloadMembers 包含 acc3，当前成员: %v", h.pool.Members())
	}

	// 3) 切换主账号到 acc3 不得清除 acc2 的 scoped 缓存
	rrAct := httptest.NewRecorder()
	h.activateAccount(rrAct, withURLParam(httptest.NewRequest(http.MethodPost, "/admin/accounts/acc3/activate", nil), "uid", "acc3"))
	if rrAct.Code != http.StatusOK {
		t.Fatalf("activate acc3 失败 %d: %s", rrAct.Code, rrAct.Body.String())
	}
	for _, pair := range [][2]string{
		{"resource_cache", resourceCacheKey("acc2")},
		{scheduler.CheckinCacheTable, scheduler.CheckinStatusKey("acc2")},
		{scheduler.GrowthCacheTable, scheduler.GrowthOverviewKey("acc2")},
		{scheduler.GrowthCacheTable, scheduler.GrowthReportDayKey("acc2")},
		{scheduler.GrowthCacheTable, scheduler.TaskLastSweepKey("acc2")},
	} {
		if _, _, ok := st.GetCache(pair[0], pair[1], 0); !ok {
			t.Errorf("切换主账号不应清除其他账号缓存 %s/%s", pair[0], pair[1])
		}
	}

	// 4) 删除非主账号 acc2 应清除其全部 scoped 缓存与池状态
	rrDel := httptest.NewRecorder()
	h.deleteAccountProfile(rrDel, withURLParam(httptest.NewRequest(http.MethodDelete, "/admin/accounts/acc2", nil), "uid", "acc2"))
	if rrDel.Code != http.StatusOK {
		t.Fatalf("delete acc2 失败 %d: %s", rrDel.Code, rrDel.Body.String())
	}
	for _, pair := range [][2]string{
		{"resource_cache", resourceCacheKey("acc2")},
		{scheduler.CheckinCacheTable, scheduler.CheckinStatusKey("acc2")},
		{scheduler.CheckinCacheTable, scheduler.CheckinRandomTargetKey("acc2")},
		{scheduler.GrowthCacheTable, scheduler.GrowthOverviewKey("acc2")},
		{scheduler.GrowthCacheTable, scheduler.GrowthReportDayKey("acc2")},
		{scheduler.GrowthCacheTable, scheduler.TaskLastSweepKey("acc2")},
		{scheduler.GrowthCacheTable, scheduler.TaskLastQueueKey("acc2")},
	} {
		if _, _, ok := st.GetCache(pair[0], pair[1], 0); ok {
			t.Errorf("删除账号 acc2 后缓存 %s/%s 未清除", pair[0], pair[1])
		}
	}
	if _, ok := h.pool.AccountStateOf("acc2"); ok {
		t.Error("删除账号 acc2 后号池应移除其成员状态")
	}
	// 注意避免 unused var 警告
	_ = atomic.LoadInt32
}

// TestEncodedAccountIDAndRawUIDCompat 验证含特殊字符（. / @ / :）的 UID：
// /admin/accounts 同时返回 uid（档案 ID h_...）与 raw_uid（原始 UID），
// 且 ?uid= 与 ?account_uid= 均兼容接收 raw_uid 或档案 ID。
func TestEncodedAccountIDAndRawUIDCompat(t *testing.T) {
	rawUID := "user.1@example.com"
	encID := auth.AccountID(rawUID)
	if encID == rawUID {
		t.Fatalf("前置条件异常：%q 应编码为不同 ID", rawUID)
	}
	h, st, _ := newMultiAccountFixture(t, "cn",
		multiTestAccount{uid: "acc1", remark: "一号"},
		multiTestAccount{uid: rawUID, remark: "特殊UID"},
	)

	// 1) /admin/accounts 透出 raw_uid
	rrList := httptest.NewRecorder()
	h.listAccounts(rrList, httptest.NewRequest(http.MethodGet, "/admin/accounts", nil))
	if rrList.Code != http.StatusOK {
		t.Fatalf("listAccounts 失败 %d", rrList.Code)
	}
	var listResp struct {
		Items []accountItem `json:"items"`
	}
	if err := json.Unmarshal(rrList.Body.Bytes(), &listResp); err != nil {
		t.Fatal(err)
	}
	var found *accountItem
	for i := range listResp.Items {
		if listResp.Items[i].RawUID == rawUID {
			found = &listResp.Items[i]
			break
		}
	}
	if found == nil || found.UID != encID {
		t.Fatalf("listAccounts 应包含 uid=%q raw_uid=%q，实际 %+v", encID, rawUID, listResp.Items)
	}

	// 2) /admin/resources 无论传 raw_uid 还是档案 ID 均能命中并返回真实 uid
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"TotalDosage":42,"Accounts":[]}}}}`))
	}))
	defer srv.Close()
	h.client.BillingBaseOverride = srv.URL

	for _, qUID := range []string{rawUID, encID} {
		rr := httptest.NewRecorder()
		h.resources(rr, httptest.NewRequest(http.MethodGet, "/admin/resources?uid="+qUID, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("resources?uid=%s 应 200，得到 %d: %s", qUID, rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), `"uid":"`+rawUID+`"`) {
			t.Errorf("resources?uid=%s 响应 uid 应为原始 UID %q: %s", qUID, rawUID, rr.Body.String())
		}
	}

	// 3) /admin/logs?account_uid= 传 raw_uid 或档案 ID 均能命中日志
	if err := st.InsertLog(&store.LogEntry{
		Model: "glm-5.3", StatusCode: 200, AccountUID: rawUID, CreatedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatal(err)
	}
	for _, qUID := range []string{rawUID, encID} {
		rr := httptest.NewRecorder()
		h.logs(rr, httptest.NewRequest(http.MethodGet, "/admin/logs?account_uid="+qUID, nil))
		var resp struct {
			Total int `json:"total"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil || resp.Total != 1 {
			t.Errorf("logs?account_uid=%s 应返回 1 条日志，得到 %d / %s", qUID, resp.Total, rr.Body.String())
		}
	}
}
