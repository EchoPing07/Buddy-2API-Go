package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
		c.PoolEnabled = true
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
	// 当前活动账号（acc1）另有一份更新的余额缓存：应当被采纳
	if err := h.st.SetCache("resource_cache", resourceCacheKey, `{"total_dosage":999}`); err != nil {
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
