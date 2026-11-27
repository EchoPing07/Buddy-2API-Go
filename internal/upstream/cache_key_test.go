package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

func extractCacheKey(t *testing.T, raw []byte) string {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("JSON 解析失败: %v", err)
	}
	v, _ := obj["prompt_cache_key"].(string)
	return v
}

// TestInjectPromptCacheKeyBackwardCompat 验证既有 InjectPromptCacheKey 保持向后兼容：
// 客户端已传 prompt_cache_key 时原样保留；未传时按 conversation / uid 派生。
func TestInjectPromptCacheKeyBackwardCompat(t *testing.T) {
	// 已有 prompt_cache_key 原样保留
	in := []byte(`{"model":"auto","prompt_cache_key":"client-custom-key"}`)
	out := InjectPromptCacheKey(in, "uid123456789")
	if got := extractCacheKey(t, out); got != "client-custom-key" {
		t.Errorf("已有 prompt_cache_key 应原样保留，得到 %q", got)
	}

	// 缺 prompt_cache_key 时派生
	in2 := []byte(`{"model":"auto","conversation_id":"conv-1"}`)
	out2 := InjectPromptCacheKey(in2, "uid123456789")
	if got, want := extractCacheKey(t, out2), buildCacheKey("uid123456789", "conv-1"); got != want {
		t.Errorf("派生键 = %q，期望 %q", got, want)
	}

	// 空/非法 body 原样返回
	if got := InjectPromptCacheKey(nil, "u"); got != nil {
		t.Errorf("nil body 应原样返回，得到 %v", got)
	}
	if got := string(InjectPromptCacheKey([]byte("not-json"), "u")); got != "not-json" {
		t.Errorf("非法 JSON 应原样返回，得到 %q", got)
	}
}

// TestInjectPromptCacheKeyWithSession 验证会话优先级、账号隔离与故障转移场景。
func TestInjectPromptCacheKeyWithSession(t *testing.T) {
	// 1) 显式 sessionKey 优先于 body.prompt_cache_key 和 conversation_id
	in := []byte(`{"model":"auto","prompt_cache_key":"client-key","conversation_id":"conv-1"}`)
	out := InjectPromptCacheKeyWithSession(in, "uidA12345678", "  sess-42  ")
	want := buildCacheKey("uidA12345678", "sess-42")
	if got := extractCacheKey(t, out); got != want {
		t.Errorf("显式 sessionKey: got %q, want %q", got, want)
	}

	// 2) 同会话不同账号（故障转移换号）必须得到不同键，避免跨账号复用缓存
	outB := InjectPromptCacheKeyWithSession(in, "uidB87654321", "sess-42")
	keyA, keyB := extractCacheKey(t, out), extractCacheKey(t, outB)
	if keyA == keyB {
		t.Fatalf("不同账号不应生成相同 cache key: %q", keyA)
	}
	if !strings.HasPrefix(keyA, "b2a-uidA1234-") || !strings.HasPrefix(keyB, "b2a-uidB8765-") {
		t.Errorf("uid8 前缀缺失: keyA=%q keyB=%q", keyA, keyB)
	}

	// 3) sessionKey 为空时回退到 body.prompt_cache_key，仍经 uid 包装隔离
	outFallbackA := InjectPromptCacheKeyWithSession(in, "uidA12345678", "   ")
	outFallbackB := InjectPromptCacheKeyWithSession(in, "uidB87654321", "")
	if got, want := extractCacheKey(t, outFallbackA), buildCacheKey("uidA12345678", "client-key"); got != want {
		t.Errorf("回退 body.prompt_cache_key 失败: got %q, want %q", got, want)
	}
	if extractCacheKey(t, outFallbackA) == extractCacheKey(t, outFallbackB) {
		t.Errorf("客户端相同 prompt_cache_key 在不同账号下必须隔离")
	}

	// 4) 无 sessionKey 且无 body.prompt_cache_key → 回退 conversationOf(obj)
	inConv := []byte(`{"model":"auto","metadata":{"conversation_id":"meta-conv-9"}}`)
	outConv := InjectPromptCacheKeyWithSession(inConv, "uidA12345678", "")
	if got, want := extractCacheKey(t, outConv), buildCacheKey("uidA12345678", "meta-conv-9"); got != want {
		t.Errorf("回退 metadata.conversation_id 失败: got %q, want %q", got, want)
	}

	// 5) 全无会话标识 → 回退空串，仍按 uid 稳定派生
	inEmpty := []byte(`{"model":"auto"}`)
	outEmpty := InjectPromptCacheKeyWithSession(inEmpty, "uidA12345678", "")
	if got, want := extractCacheKey(t, outEmpty), buildCacheKey("uidA12345678", ""); got != want {
		t.Errorf("回退空 seg 失败: got %q, want %q", got, want)
	}

	// 6) 空/非法 body 原样返回
	if got := InjectPromptCacheKeyWithSession(nil, "u", "s"); got != nil {
		t.Errorf("nil body 应原样返回")
	}
	if got := string(InjectPromptCacheKeyWithSession([]byte("bad"), "u", "s")); got != "bad" {
		t.Errorf("非法 JSON 应原样返回，得到 %q", got)
	}
}
