package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"buddy2api-go/internal/auth"
	"buddy2api-go/internal/config"
	"buddy2api-go/internal/pool"
	"buddy2api-go/internal/store"
	"buddy2api-go/internal/upstream"
)

// newPoolPanelFixture 造一个带 N 个已存账号的号池 + 对应 Handler。
//
// 返回的 cfg 已把号池开关打开，token.json 是第一个账号（= 当前活动账号）。
func newPoolPanelFixture(t *testing.T, uids ...string) (*Handler, *store.Store) {
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
	if err := cfg.Update(func(c *config.Config) error {
		c.PoolSessionTTL = 180
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	book := auth.NewAccountBook(dir)
	for i, uid := range uids {
		tok := &auth.Token{
			UID: uid, AccessToken: "at-" + uid, Domain: "www.codebuddy.cn",
			ExpiresAt: time.Now().Add(time.Hour).Unix(), Nickname: "账号" + uid,
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
	client := upstream.New(toks, func() string { return "cn" }, 30)
	m := pool.New(cfg, st, toks, client)
	t.Cleanup(func() { m.Close() })

	h := &Handler{cfg: cfg, st: st, toks: toks, client: client,
		growth: &fakeGrowthAPI{}, sched: &fakeSched{}, pool: m}
	return h, st
}

// TestPoolViewCreditsForEveryAccount 面板必须给**每个**号显示额度，不能只有当前活动账号。
//
// 回归：poolView 的 creditsOf 只认「当前活动账号」的余额缓存（缓存键是单账号设计的
// 固定字串），其余号一律回 0 → 号池面板上只有当前这个号有额度，其它全是 —。
// 修法：探测时把额度存进池状态（并落库），Snapshot 按时间戳与注入值取新。
func TestPoolViewCreditsForEveryAccount(t *testing.T) {
	h, _ := newPoolPanelFixture(t, "acc1", "acc2", "acc3")

	// 模拟「已经探测过」：每个号都有自己的额度（绕开真实上游）
	for uid, credits := range map[string]float64{"acc1": 100, "acc2": 250.5, "acc3": 0} {
		if _, ok := h.pool.AccountStateOf(uid); !ok {
			t.Fatalf("号池里没有 %s", uid)
		}
		h.pool.SetCredits(uid, credits, time.Now().Add(-time.Hour).Unix())
	}
	// 当前活动账号（acc1）另有一份更新的 per-uid 余额缓存：应当被采纳
	if err := h.st.SetCache("resource_cache", resourceCacheKey("acc1"), `{"total_dosage":999}`); err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	h.poolView(rr, httptest.NewRequest(http.MethodGet, "/admin/pool", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	var view pool.PoolView
	if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
		t.Fatalf("解析响应失败: %v\n%s", err, rr.Body.String())
	}
	if len(view.Accounts) != 3 {
		t.Fatalf("应返回 3 个账号，实际 %d", len(view.Accounts))
	}

	got := map[string]pool.AccountView{}
	for _, a := range view.Accounts {
		got[a.UID] = a
	}
	// acc1：缓存更新 → 用缓存值
	if got["acc1"].Credits != 999 {
		t.Errorf("acc1（当前活动账号）额度应取更新的余额缓存 999，实际 %v", got["acc1"].Credits)
	}
	// acc2：非活动账号，只能靠自己的探测值
	if got["acc2"].Credits != 250.5 || got["acc2"].CreditsAt <= 0 {
		t.Errorf("acc2（非活动账号）额度应来自自己的探测值 250.5，实际 %v/%d "+
			"（回归：早期这里只有活动账号有值）", got["acc2"].Credits, got["acc2"].CreditsAt)
	}
	// acc3：探测确认 0 额度 → 必须显示 0（而不是 —）
	if got["acc3"].CreditsAt <= 0 {
		t.Errorf("acc3 应记录探测时刻（否则前端会把「额度 0」误显示成「未知」），实际 %d",
			got["acc3"].CreditsAt)
	}
	if got["acc3"].Credits != 0 {
		t.Errorf("acc3 额度应为 0，实际 %v", got["acc3"].Credits)
	}
}

// TestPoolViewActiveFlagMarked 当前活动账号要在视图里标出来（UI 靠它显示「当前」徽标）。
func TestPoolViewActiveFlagMarked(t *testing.T) {
	h, _ := newPoolPanelFixture(t, "acc1", "acc2")
	rr := httptest.NewRecorder()
	h.poolView(rr, httptest.NewRequest(http.MethodGet, "/admin/pool", nil))
	var view pool.PoolView
	if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	actives := 0
	for _, a := range view.Accounts {
		if a.Active {
			actives++
			if a.UID != "acc1" {
				t.Errorf("active 标在了 %s，应为 acc1（token.json 里的账号）", a.UID)
			}
		}
	}
	if actives != 1 {
		t.Errorf("应恰好标出 1 个活动账号，实际 %d", actives)
	}
}

// TestPoolStrategyEndpoint PUT /admin/pool/strategy 校验策略合法性、持久化、env 锁及 GET /admin/pool 同步。
func TestPoolStrategyEndpoint(t *testing.T) {
	h, _ := newPoolPanelFixture(t, "acc1")

	// 非法策略 → 400
	rrBad := httptest.NewRecorder()
	h.poolSetStrategy(rrBad, httptest.NewRequest(http.MethodPut, "/admin/pool/strategy", strings.NewReader(`{"strategy":"random_bad"}`)))
	if rrBad.Code != http.StatusBadRequest {
		t.Fatalf("非法策略应 400，得到 %d: %s", rrBad.Code, rrBad.Body.String())
	}

	// 合法策略 → 200 并持久化
	for _, s := range []string{"balanced", "credits_first", "expire_first"} {
		rr := httptest.NewRecorder()
		h.poolSetStrategy(rr, httptest.NewRequest(http.MethodPut, "/admin/pool/strategy", strings.NewReader(`{"strategy":"`+s+`"}`)))
		if rr.Code != http.StatusOK {
			t.Fatalf("切换策略 %s 失败 %d: %s", s, rr.Code, rr.Body.String())
		}
		var resp map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if resp["ok"] != true || resp["strategy"] != s {
			t.Errorf("响应异常: %+v", resp)
		}

		// GET /admin/pool 应实时返回该策略
		rrView := httptest.NewRecorder()
		h.poolView(rrView, httptest.NewRequest(http.MethodGet, "/admin/pool", nil))
		var view pool.PoolView
		if err := json.Unmarshal(rrView.Body.Bytes(), &view); err != nil {
			t.Fatal(err)
		}
		if view.Strategy != s {
			t.Errorf("GET /admin/pool strategy=%q，期望 %q", view.Strategy, s)
		}
	}
}

// TestPoolStrategyEnvLocked BUDDY2API_POOL_STRATEGY 锁定时 PUT 不得覆盖 env 值。
func TestPoolStrategyEnvLocked(t *testing.T) {
	t.Setenv("BUDDY2API_POOL_STRATEGY", "credits_first")
	h, _ := newPoolPanelFixture(t, "acc1")

	rr := httptest.NewRecorder()
	h.poolSetStrategy(rr, httptest.NewRequest(http.MethodPut, "/admin/pool/strategy", strings.NewReader(`{"strategy":"balanced"}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"strategy":"credits_first"`) {
		t.Errorf("env 锁定时应保持 credits_first，得到 %s", rr.Body.String())
	}
}

// TestPoolProbeResourceHookWritesCache 验证 SetPool 挂载的 ResourceHook 在 Probe 后写入 v4:<uid> 缓存并清理旧键。
func TestPoolProbeResourceHookWritesCache(t *testing.T) {
	h, st := newPoolPanelFixture(t, "acc1")
	h.SetPool(h.pool)

	exp := time.Now().Add(48 * time.Hour).Format("2006-01-02 15:04:05")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{
			"TotalDosage":120,
			"Accounts":[
				{"PackageName":"临期包","CapacityRemainPrecise":"120","CapacitySizePrecise":"200","CycleCapacityRemainPrecise":"120","CycleCapacitySizePrecise":"200","ExpiredTime":"` + exp + `"}
			]
		}}}}`))
	}))
	defer srv.Close()
	h.client.BillingBaseOverride = srv.URL

	// 写入旧版残留键，探测后应被清理
	_ = st.SetCache("resource_cache", "default_v3", `{"stale":true}`)

	rr := httptest.NewRecorder()
	h.poolProbe(rr, withURLParam(httptest.NewRequest(http.MethodPost, "/admin/pool/accounts/acc1/probe", nil), "uid", "acc1"))
	if rr.Code != http.StatusOK {
		t.Fatalf("probe 失败 %d: %s", rr.Code, rr.Body.String())
	}
	payload, _, ok := st.GetCache("resource_cache", resourceCacheKey("acc1"), 0)
	if !ok || !strings.Contains(payload, `"total_dosage":120`) || !strings.Contains(payload, `"expire_at"`) {
		t.Fatalf("Probe 后未写入 v4:acc1 缓存: ok=%v payload=%s", ok, payload)
	}
	if _, _, legacyOK := st.GetCache("resource_cache", "default_v3", 0); legacyOK {
		t.Error("Probe 写缓存时应一并清理旧版 default_v3 键")
	}
}
