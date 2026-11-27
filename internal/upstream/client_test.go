package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"buddy2api-go/internal/auth"
)

func newTestTokenStore(t *testing.T, uid, access, refresh string) *auth.TokenStore {
	t.Helper()
	ts, err := auth.NewTokenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.Save(&auth.Token{
		AccessToken:  access,
		RefreshToken: refresh,
		TokenType:    "Bearer",
		Domain:       "www.codebuddy.cn",
		UID:          uid,
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
	}); err != nil {
		t.Fatal(err)
	}
	return ts
}

// TestWithTokenStore 验证 WithTokenStore 保留全部连接/覆盖字段且隔离 toks，nil 返回原实例。
func TestWithTokenStore(t *testing.T) {
	tsA := newTestTokenStore(t, "uid-A", "tok-A", "ref-A")
	tsB := newTestTokenStore(t, "uid-B", "tok-B", "ref-B")

	base := New(tsA, func() string { return "global" }, 45)
	base.GrowthBaseOverride = "http://growth"
	base.WebBaseOverride = "http://web"
	base.MPBaseOverride = "http://mp"
	base.AuthBaseOverride = "http://auth"
	base.ChatBaseOverride = "http://chat"
	base.BillingBaseOverride = "http://billing"

	// nil 返回自身
	if got := base.WithTokenStore(nil); got != base {
		t.Fatalf("WithTokenStore(nil) 应返回原指针")
	}

	cloned := base.WithTokenStore(tsB)
	if cloned == base {
		t.Fatal("WithTokenStore(non-nil) 应返回新浅拷贝")
	}
	if cloned.toks != tsB || base.toks != tsA {
		t.Fatalf("toks 未正确切换或污染原实例: base=%v cloned=%v", base.TokenSummary().UID, cloned.TokenSummary().UID)
	}
	if cloned.long != base.long || cloned.short != base.short {
		t.Error("应共享 long / short http.Client")
	}
	if cloned.regionProvider() != "global" {
		t.Errorf("regionProvider 未保留: %q", cloned.regionProvider())
	}
	if cloned.GrowthBaseOverride != base.GrowthBaseOverride ||
		cloned.WebBaseOverride != base.WebBaseOverride ||
		cloned.MPBaseOverride != base.MPBaseOverride ||
		cloned.AuthBaseOverride != base.AuthBaseOverride ||
		cloned.ChatBaseOverride != base.ChatBaseOverride ||
		cloned.BillingBaseOverride != base.BillingBaseOverride {
		t.Errorf("*BaseOverride 字段未完整保留: %+v", cloned)
	}

	// 端到端验证 cloned 发出的 billing 请求使用 tsB 的 token 而非 tsA
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte(`{"code":0,"msg":"ok","data":{"Accounts":[]}}`))
	}))
	defer srv.Close()

	base.BillingBaseOverride = srv.URL
	cB := base.WithTokenStore(tsB)
	if _, err := cB.Resources(); err != nil {
		t.Fatalf("Resources 失败: %v", err)
	}
	if gotAuth != "Bearer tok-B" {
		t.Errorf("cloned 应携带 tsB 凭证，得到 Authorization=%q", gotAuth)
	}
}

// TestDoChatWithSession 验证会话键注入、按账号隔离、X-Conversation-ID 稳定性以及 401 合成响应。
func TestDoChatWithSession(t *testing.T) {
	type captured struct {
		auth     string
		convID   string
		cacheKey string
	}
	var got captured
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/plugin/auth/token/refresh" {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"code":401,"msg":"refresh failed"}`))
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var obj map[string]any
		_ = json.Unmarshal(raw, &obj)
		got = captured{
			auth:     r.Header.Get("Authorization"),
			convID:   r.Header.Get("X-Conversation-ID"),
			cacheKey: obj["prompt_cache_key"].(string),
		}
		if r.Header.Get("Authorization") == "Bearer expired-tok" {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"code":401,"msg":"unauthorized"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	tsA := newTestTokenStore(t, "uidA12345678", "tok-A", "ref-A")
	tsB := newTestTokenStore(t, "uidB87654321", "tok-B", "ref-B")
	c := New(tsA, func() string { return "cn" }, 10)
	c.ChatBaseOverride = srv.URL
	c.AuthBaseOverride = srv.URL

	// 1) nil TokenStore 返回 ErrNoToken
	if _, err := c.DoChatWithSession(context.Background(), nil, []byte(`{"model":"auto"}`), "s1"); err != ErrNoToken {
		t.Fatalf("nil ts 期望 ErrNoToken，得到 %v", err)
	}

	// 2) 同 sessionKey 在 A 号上稳定命中相同 prompt_cache_key 与 X-Conversation-ID
	body := []byte(`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`)
	resp, err := c.DoChatWithSession(context.Background(), tsA, body, "sess-100")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	capA1 := got

	resp, err = c.DoChatWithSession(context.Background(), tsA, body, "sess-100")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	capA2 := got

	wantKeyA := buildCacheKey("uidA12345678", "sess-100")
	if capA1.auth != "Bearer tok-A" || capA1.cacheKey != wantKeyA {
		t.Errorf("A 号请求异常: %+v, want cacheKey=%q", capA1, wantKeyA)
	}
	if capA1.cacheKey != capA2.cacheKey || capA1.convID != capA2.convID || capA1.convID == "" {
		t.Errorf("同账号同会话缓存键/会话头不稳定: %+v vs %+v", capA1, capA2)
	}

	// 3) 换号到 B 号（同 sessionKey）→ 不同 prompt_cache_key 与不同 X-Conversation-ID
	resp, err = c.DoChatWithSession(context.Background(), tsB, body, "sess-100")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	capB := got
	if capB.cacheKey == capA1.cacheKey || capB.convID == capA1.convID {
		t.Errorf("故障转移换号后应隔离 cacheKey 与 X-Conversation-ID: A=%+v B=%+v", capA1, capB)
	}

	// 4) 号池路径下即使 sessionKey 为空且客户端带 prompt_cache_key，也必须按 uid 隔离
	clientKeyBody := []byte(`{"model":"auto","prompt_cache_key":"client-raw-key"}`)
	resp, err = c.DoChatWith(context.Background(), tsA, clientKeyBody)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	pooledA := got
	resp, err = c.DoChatWith(context.Background(), tsB, clientKeyBody)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	pooledB := got
	if pooledA.cacheKey != buildCacheKey("uidA12345678", "client-raw-key") ||
		pooledB.cacheKey != buildCacheKey("uidB87654321", "client-raw-key") ||
		pooledA.cacheKey == pooledB.cacheKey {
		t.Errorf("号池路径未对客户端 prompt_cache_key 做 uid 隔离: A=%q B=%q", pooledA.cacheKey, pooledB.cacheKey)
	}

	// 5) 401 且刷新失败 → 返回合成 401 响应（而非 error）
	tsDead := newTestTokenStore(t, "uid-dead", "expired-tok", "bad-ref")
	resp, err = c.DoChatWithSession(context.Background(), tsDead, body, "sess-dead")
	if err != nil {
		t.Fatalf("号池路径 401 刷不动应返回合成 401 响应而非 error，得到 %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("期望合成 401，得到 %d", resp.StatusCode)
	}
}
