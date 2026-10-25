package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"buddy2api-go/internal/apikey"
	"buddy2api-go/internal/auth"
	"buddy2api-go/internal/config"
	"buddy2api-go/internal/store"
	"buddy2api-go/internal/upstream"
)

func TestNormalizeEmbeddingsInput(t *testing.T) {
	// 单字符串 → 单条
	got, err := normalizeEmbeddingsInput("hello")
	if err != nil || len(got) != 1 || got[0] != "hello" {
		t.Fatalf("单字符串: got=%v err=%v", got, err)
	}
	// 字符串数组（含中文/空串，均为合法条目）
	got, err = normalizeEmbeddingsInput([]any{"你好", "", "world"})
	if err != nil || len(got) != 3 || got[0] != "你好" || got[1] != "" {
		t.Fatalf("字符串数组: got=%v err=%v", got, err)
	}
	// token id 数组（OpenAI 允许、上游不支持）→ 明确报错
	if _, err := normalizeEmbeddingsInput([]any{[]any{1, 2, 3}}); err == nil {
		t.Error("token id 数组应报错")
	}
	// 非字符串标量 / 对象 / nil
	for _, v := range []any{42, 3.14, true, nil, map[string]any{"text": "x"}} {
		if _, err := normalizeEmbeddingsInput(v); err == nil {
			t.Errorf("%v 应报错", v)
		}
	}
}

// TestOpenAIEmbeddingsResponseShape 响应体契约：data[i] 带 object/index/embedding，
// 顶层为 object/data/model/usage，usage 为 prompt_tokens/total_tokens。
func TestOpenAIEmbeddingsResponseShape(t *testing.T) {
	res := &upstream.EmbeddingsResult{
		Model: "bge-m3",
		Data:  []upstream.EmbeddingItem{{Embedding: []float64{0.1, -0.2}}, {Embedding: []float64{0.3}}},
		Usage: upstream.EmbeddingUsage{PromptTokens: 7, TotalTokens: 7},
	}
	resp := openAIEmbeddingsResponse{
		Object: "list",
		Data:   make([]openAIEmbeddingItem, len(res.Data)),
		Model:  orDefault(res.Model, "bge-m3"),
		Usage:  res.Usage,
	}
	for i, item := range res.Data {
		resp.Data[i] = openAIEmbeddingItem{Object: "embedding", Index: i, Embedding: item.Embedding}
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m["object"] != "list" || m["model"] != "bge-m3" {
		t.Errorf("顶层字段异常: %s", raw)
	}
	data := m["data"].([]any)
	if len(data) != 2 {
		t.Fatalf("data = %d 条", len(data))
	}
	first := data[0].(map[string]any)
	if first["object"] != "embedding" || first["index"] != float64(0) {
		t.Errorf("首条缺 object/index: %v", first)
	}
	if emb := first["embedding"].([]any); len(emb) != 2 {
		t.Errorf("embedding 维度 = %d", len(emb))
	}
	usage := m["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(7) || usage["total_tokens"] != float64(7) {
		t.Errorf("usage 异常: %v", usage)
	}
}

// ── 实验开关门控（默认关闭；开启由管理后台「实验」页控制） ───────────────────

// newEmbeddingsHandler 构造完整 Handler：ModelCache 用零值（List() 恒空，不触网），
// 嵌入请求在开关关闭时不会到达上游，可安全直调处理器。
func newEmbeddingsHandler(t *testing.T) (*Handler, *config.Manager) {
	t.Helper()
	dir := t.TempDir()
	toks, err := auth.NewTokenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	client := upstream.New(toks, func() string { return "cn" }, 60)
	h := New(client, &upstream.ModelCache{}, apikey.New(st), st, cfg)
	return h, cfg
}

// TestEmbeddingsDisabledByDefault 开关默认关闭：请求被 403 拒绝，不读请求体、不触上游。
func TestEmbeddingsDisabledByDefault(t *testing.T) {
	h, cfg := newEmbeddingsHandler(t)
	if cfg.Get().EmbeddingsEnabled {
		t.Fatal("前提失败：默认应关闭")
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(`{"model":"bge-m3","input":["hi"]}`))
	rr := httptest.NewRecorder()
	h.Embeddings(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("关闭时应 403，得到 %d: %s", rr.Code, rr.Body.String())
	}
	var e map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	errObj := e["error"].(map[string]any)
	if errObj["code"] != "embeddings_disabled" {
		t.Errorf("错误码 = %v，期望 embeddings_disabled", errObj["code"])
	}
}

// TestModelsListConditionalEmbedding /v1/models 仅在开关开启时附带嵌入模型。
func TestModelsListConditionalEmbedding(t *testing.T) {
	h, cfg := newEmbeddingsHandler(t)
	getIDs := func() []string {
		rr := httptest.NewRecorder()
		h.Models(rr, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
		var m struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		return func() []string {
			ids := make([]string, len(m.Data))
			for i, d := range m.Data {
				ids[i] = d.ID
			}
			return ids
		}()
	}

	if ids := getIDs(); len(ids) != 0 {
		t.Errorf("关闭时零 chat 模型 + 嵌入模型不应出现，得到 %v", ids)
	}
	if err := cfg.Update(func(c *config.Config) error { c.EmbeddingsEnabled = true; return nil }); err != nil {
		t.Fatal(err)
	}
	ids := getIDs()
	if len(ids) != 1 || ids[0] != upstream.EmbeddingsModel {
		t.Errorf("开启后应仅含 %q，得到 %v", upstream.EmbeddingsModel, ids)
	}
}
