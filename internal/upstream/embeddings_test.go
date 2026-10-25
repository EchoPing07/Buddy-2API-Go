package upstream

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"buddy2api-go/internal/auth"
)

// embeddingsTestClient 构造指向 httptest 假上游的客户端（带 refresh_token，可测 401 刷新链）。
func embeddingsTestClient(t *testing.T, base string) *Client {
	t.Helper()
	dir, err := os.MkdirTemp("", "embeddings-test-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	toks, err := auth.NewTokenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := toks.Save(&auth.Token{
		AccessToken:  "test-access-token",
		RefreshToken: "test-refresh-token",
		TokenType:    "Bearer",
		Domain:       "www.codebuddy.cn",
		UID:          "test-uid",
		ExpiresAt:    time.Now().Add(1 * time.Hour).Unix(),
	}); err != nil {
		t.Fatal(err)
	}
	orig := embeddingsBase
	embeddingsBase = func(*Client) string { return base }
	// 刷新链（RefreshFn）也须指向假上游，否则 401 重试测试会打到真实端点
	authOrig := authBase
	authBase = func(*Client) string { return base }
	t.Cleanup(func() { embeddingsBase, authBase = orig, authOrig })
	return New(toks, func() string { return "cn" }, 60)
}

// okEmbeddingsBody 构造上游成功响应（OpenAI 形状，无 code/msg 包络）。
func okEmbeddingsBody(model string, dims ...int) []byte {
	type item struct {
		Embedding []float64 `json:"embedding"`
		Object    string    `json:"object"`
	}
	data := make([]item, len(dims))
	for i, d := range dims {
		emb := make([]float64, d)
		for j := range emb {
			emb[j] = float64(i) // 每条向量可辨识：全 (条序号)
		}
		data[i] = item{Embedding: emb, Object: "embedding"}
	}
	total := 0
	for _, d := range dims {
		total += d
	}
	raw, _ := json.Marshal(map[string]any{
		"object": "list",
		"model":  model,
		"data":   data,
		"usage":  map[string]int{"prompt_tokens": total, "total_tokens": total},
	})
	return raw
}

// TestDoEmbeddingsSuccess 请求形状（路径/头/体）与响应解析。
func TestDoEmbeddingsSuccess(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("请求体非法 JSON: %v", err)
		}
		w.Write(okEmbeddingsBody("bge-m3", 4, 4))
	}))
	defer srv.Close()
	c := embeddingsTestClient(t, srv.URL)

	res, err := c.DoEmbeddings(context.Background(), "bge-m3", []string{"你好", "hello"})
	if err != nil {
		t.Fatal("DoEmbeddings 失败:", err)
	}
	if gotPath != "/v2/embeddings" {
		t.Errorf("请求路径 = %q，期望 /v2/embeddings", gotPath)
	}
	if gotAuth != "Bearer test-access-token" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotBody["model"] != "bge-m3" {
		t.Errorf("请求体 model = %v", gotBody["model"])
	}
	if ins, ok := gotBody["input"].([]any); !ok || len(ins) != 2 {
		t.Errorf("请求体 input = %v，期望两条", gotBody["input"])
	}
	if res.Model != "bge-m3" || len(res.Data) != 2 || len(res.Data[0].Embedding) != 4 {
		t.Errorf("结果形状异常: model=%q data=%d 条", res.Model, len(res.Data))
	}
	if res.Usage.TotalTokens != 8 {
		t.Errorf("usage.total_tokens = %d，期望 8", res.Usage.TotalTokens)
	}
}

// TestDoEmbeddingsModelNotFound 上游 500 + code=11351 → EmbeddingsError 且 ModelNotFound。
func TestDoEmbeddingsModelNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"code":11351,"msg":"Generate embedding failed with error: 11351:embedding model [nes-gf] not found","requestId":"x"}`))
	}))
	defer srv.Close()
	c := embeddingsTestClient(t, srv.URL)

	_, err := c.DoEmbeddings(context.Background(), "nes-gf", []string{"hi"})
	if err == nil {
		t.Fatal("期望失败，得到 nil")
	}
	ue, ok := err.(*EmbeddingsError)
	if !ok {
		t.Fatalf("错误类型 = %T，期望 *EmbeddingsError", err)
	}
	if ue.Code != 11351 || !ue.ModelNotFound() {
		t.Errorf("code=%d ModelNotFound=%v，期望 11351/true", ue.Code, ue.ModelNotFound())
	}
	if !strings.Contains(ue.Error(), "not found") {
		t.Errorf("错误文案缺上游信息: %q", ue.Error())
	}
}

// TestDoEmbeddingsAuthRefresh 401 → 刷新（refresh 端点）→ 携新 token 重试成功。
func TestDoEmbeddingsAuthRefresh(t *testing.T) {
	var mu sync.Mutex
	embedCalls := 0
	refreshCalls := 0
	var retryAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/plugin/auth/token/refresh":
			mu.Lock()
			refreshCalls++
			mu.Unlock()
			if r.Header.Get("X-Refresh-Token") != "test-refresh-token" {
				t.Errorf("刷新请求缺 X-Refresh-Token: %q", r.Header.Get("X-Refresh-Token"))
			}
			w.Write([]byte(`{"code":0,"msg":"ok","data":{"accessToken":"new-access-token","refreshToken":"new-refresh-token","expiresIn":3600}}`))
		case "/v2/embeddings":
			mu.Lock()
			embedCalls++
			mu.Unlock()
			if embedCalls == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte(`{"code":401,"msg":"unauthorized"}`))
				return
			}
			retryAuth = r.Header.Get("Authorization")
			w.Write(okEmbeddingsBody("bge-m3", 2))
		default:
			t.Errorf("意外请求路径: %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	c := embeddingsTestClient(t, srv.URL)

	res, err := c.DoEmbeddings(context.Background(), "bge-m3", []string{"hi"})
	if err != nil {
		t.Fatal("刷新重试后仍失败:", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if embedCalls != 2 || refreshCalls != 1 {
		t.Errorf("embeddings 调用 %d 次 / 刷新 %d 次，期望 2/1", embedCalls, refreshCalls)
	}
	if retryAuth != "Bearer new-access-token" {
		t.Errorf("重试 Authorization = %q，期望新 token", retryAuth)
	}
	if len(res.Data) != 1 {
		t.Errorf("结果 data = %d 条", len(res.Data))
	}
}

// TestDoEmbeddingsCountMismatch 向量条数与输入不符 → 拒绝（防错位配对）。
func TestDoEmbeddingsCountMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(okEmbeddingsBody("bge-m3", 2, 2, 2)) // 3 条对 2 条输入
	}))
	defer srv.Close()
	c := embeddingsTestClient(t, srv.URL)

	_, err := c.DoEmbeddings(context.Background(), "bge-m3", []string{"a", "b"})
	if err == nil || !strings.Contains(err.Error(), "不符") {
		t.Fatalf("期望条数不符错误，得到 %v", err)
	}
}

// TestParseEmbeddingsResponse 200 但业务错误码（防御 chat 系包络形态）。
func TestParseEmbeddingsResponseBusinessError(t *testing.T) {
	_, err := parseEmbeddingsResponse(http.StatusOK, []byte(`{"code":11217,"msg":"限流"}`), 1)
	ue, ok := err.(*EmbeddingsError)
	if !ok {
		t.Fatalf("错误类型 = %T，期望 *EmbeddingsError", err)
	}
	if ue.Code != 11217 || ue.Msg != "限流" {
		t.Errorf("code=%d msg=%q", ue.Code, ue.Msg)
	}
}

// TestEmbeddingsErrorModelNotFoundBoundary code=11351 但文案非 not found 不算模型缺失。
func TestEmbeddingsErrorModelNotFoundBoundary(t *testing.T) {
	e := &EmbeddingsError{Code: 11351, Msg: "backend busy"}
	if e.ModelNotFound() {
		t.Error("非 not found 文案不应判为模型缺失")
	}
	if (&EmbeddingsError{Code: 1, Msg: "x not found"}).ModelNotFound() {
		t.Error("code 非 11351 不应判为模型缺失")
	}
}
