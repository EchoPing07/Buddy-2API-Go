package pool

import (
	"strings"
	"testing"
)

func hdr(m map[string]string) func(string) string {
	return func(k string) string { return m[strings.ToLower(k)] }
}

// TestSessionKeyHeaderPriority 请求头优先级（pi 的三种 compat 形态都能取到）。
func TestSessionKeyHeaderPriority(t *testing.T) {
	cases := []struct {
		name string
		h    map[string]string
		want string
	}{
		{"x-session-affinity 优先", map[string]string{"x-session-affinity": "a", "session_id": "b"}, "h:a"},
		{"session_id", map[string]string{"session_id": "b"}, "h:b"},
		{"x-client-request-id", map[string]string{"x-client-request-id": "c"}, "h:c"},
		{"x-session-id", map[string]string{"x-session-id": "d"}, "h:d"},
		{"x-opencode-session", map[string]string{"x-opencode-session": "e"}, "h:e"},
		// 空白值不算
		{"空白回落到下一层", map[string]string{"x-session-affinity": "  ", "session_id": "b"}, "h:b"},
	}
	for _, c := range cases {
		if got := SessionKey(hdr(c.h), nil, 1); got != c.want {
			t.Errorf("%s: got %q，期望 %q", c.name, got, c.want)
		}
	}
}

// TestSessionKeyFromBody prompt_cache_key / metadata / 顶层 conversation。
func TestSessionKeyFromBody(t *testing.T) {
	// pi 在 PI_CACHE_RETENTION=long 时发 prompt_cache_key = sessionId
	body := map[string]any{"prompt_cache_key": "pc-1"}
	if got := SessionKey(hdr(nil), body, 1); got != "c:pc-1" {
		t.Errorf("prompt_cache_key: got %q", got)
	}

	body = map[string]any{"metadata": map[string]any{"conversation_id": "conv-1"}}
	if got := SessionKey(hdr(nil), body, 1); got != "m:conv-1" {
		t.Errorf("metadata.conversation_id: got %q", got)
	}

	body = map[string]any{"metadata": map[string]any{"conversationId": "conv-2"}}
	if got := SessionKey(hdr(nil), body, 1); got != "m:conv-2" {
		t.Errorf("metadata.conversationId: got %q", got)
	}

	body = map[string]any{"conversation_id": "conv-3"}
	if got := SessionKey(hdr(nil), body, 1); got != "b:conv-3" {
		t.Errorf("顶层 conversation_id: got %q", got)
	}

	// 请求头优先于 body
	body = map[string]any{"prompt_cache_key": "pc-1"}
	if got := SessionKey(hdr(map[string]string{"session_id": "hdr"}), body, 1); got != "h:hdr" {
		t.Errorf("头应优先于 body: got %q", got)
	}
}

// TestSessionKeyDerivedFromFirstUser 派生键：取**首条** user，不取最后一条。
// 取最后一条的话用户每说一句就换一个会话，粘性形同虚设。
func TestSessionKeyDerivedFromFirstUser(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": "you are helpful"},
			map[string]any{"role": "user", "content": "第一句"},
			map[string]any{"role": "assistant", "content": "回复"},
			map[string]any{"role": "user", "content": "第二句"},
		},
	}
	k1 := SessionKey(nil, body, 1)
	if !strings.HasPrefix(k1, "d:") {
		t.Fatalf("应走派生键，得到 %q", k1)
	}
	// 追加更多轮次，键必须不变
	body["messages"] = append(body["messages"].([]any),
		map[string]any{"role": "assistant", "content": "再回复"},
		map[string]any{"role": "user", "content": "第三句"},
	)
	if k2 := SessionKey(nil, body, 1); k2 != k1 {
		t.Errorf("多轮对话后派生键应不变：%q → %q", k1, k2)
	}
}

// TestSessionKeyDerivedIgnoresSystem system prompt 不参与派生（Cline 会往里面塞时间戳）。
func TestSessionKeyDerivedIgnoresSystem(t *testing.T) {
	mk := func(sys string) string {
		return SessionKey(nil, map[string]any{
			"messages": []any{
				map[string]any{"role": "system", "content": sys},
				map[string]any{"role": "user", "content": "同一个开场"},
			},
		}, 1)
	}
	if a, b := mk("system v1"), mk("system v2 时间 12:00:01"); a != b {
		t.Errorf("system prompt 变化不应影响派生键：%q vs %q", a, b)
	}
}

// TestSessionKeyApiKeyScoping 必须掺 api_key_id：否则两个用户同样开场白会共用一个号。
func TestSessionKeyApiKeyScoping(t *testing.T) {
	body := map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "帮我写个排序"}},
	}
	a := SessionKey(nil, body, 1)
	b := SessionKey(nil, body, 2)
	if a == b {
		t.Error("不同 api_key 的相同开场白必须派生出不同键")
	}
}

// TestSessionKeyArrayContent pi 的 content 是数组形态 [{"type":"text","text":"..."}]，
// 不处理数组会让 pi 裸连场景的派生键恒为空。
func TestSessionKeyArrayContent(t *testing.T) {
	body := map[string]any{
		"messages": []any{map[string]any{
			"role":    "user",
			"content": []any{map[string]any{"type": "text", "text": "数组形态的开场"}},
		}},
	}
	k := SessionKey(nil, body, 1)
	if !strings.HasPrefix(k, "d:") {
		t.Fatalf("数组 content 应能派生键，得到 %q", k)
	}
	// 与字符串形态应派生同一个键（语义相同）
	strBody := map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "数组形态的开场"}},
	}
	if k2 := SessionKey(nil, strBody, 1); k2 != k {
		t.Errorf("数组与字符串形态应派生同一键：%q vs %q", k, k2)
	}
}

// TestSessionKeyArrayContentSkipsNonText 多模态 parts 里的图片不能参与文本拼接。
func TestSessionKeyArrayContentSkipsNonText(t *testing.T) {
	mk := func(imgURL string) string {
		return SessionKey(nil, map[string]any{
			"messages": []any{map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": "看这张图"},
					map[string]any{"type": "image_url", "image_url": map[string]any{"url": imgURL}},
				},
			}},
		}, 1)
	}
	if a, b := mk("http://x/1.png"), mk("http://x/2.png"); a != b {
		t.Errorf("换图片不应改变派生键：%q vs %q", a, b)
	}
}

// TestSessionKeyEmpty 全空 → 空串（调用方走纯分发，不伪造粘性）。
func TestSessionKeyEmpty(t *testing.T) {
	cases := []map[string]any{
		nil,
		{},
		{"messages": []any{}},
		{"messages": []any{map[string]any{"role": "assistant", "content": "只有助手消息"}}},
		{"messages": []any{map[string]any{"role": "user", "content": ""}}},
	}
	for i, body := range cases {
		if got := SessionKey(hdr(nil), body, 1); got != "" {
			t.Errorf("case %d 应为空键，得到 %q", i, got)
		}
	}
}
