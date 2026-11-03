// cache_key.go 出站注入 prompt_cache_key（上游前缀缓存命中键）。
//
// 逆向实测：同一段 8k token 前缀，不带该键 prompt_cache_hit_tokens=0、credit≈0.34；
// 带键时 prompt_cache_hit_tokens≈7808、credit≈0.02（约 17× 费用差）。
//
// 此前网关既不透传也不注入这个字段（passthroughBodyKeys 里没有它），
// 所以上游的前缀缓存命中率恒为 0——这也是 usage 里缓存字段恒为 0 的根因。
//
// 安全约束：键必须**按账号隔离**。跨账号复用同一 cache key 会命中别人账号的
// 前缀缓存，等于泄露对方上下文。故键里带 uid8 隔离段。
package upstream

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// InjectPromptCacheKey 在出站 body 上补齐 prompt_cache_key。
//
// 优先级：
//  1. body 已带非空 prompt_cache_key → 原值保留（客户端自知复用哪个键）
//  2. body 带 conversation_id / conversationId / metadata 里同名字段 → 用它做会话段
//  3. 都没有 → 只用 uid 派生（同账号跨会话共用一个键；上游仍按前缀内容分片，
//     不会串内容，只是新会话命中率低一些，属可接受的退化）
//
// uid 为空时用 "-"（仍注入键，保证多租隔离至少不退化成完全不注入）。
// body 不可解析 / 为空时原样返回（坏 body 不二次错误化）。
func InjectPromptCacheKey(body []byte, uid string) []byte {
	if len(body) == 0 {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	if v, ok := obj["prompt_cache_key"].(string); ok && strings.TrimSpace(v) != "" {
		return body
	}
	obj["prompt_cache_key"] = buildCacheKey(uid, conversationOf(obj))
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// conversationOf 从请求体里找会话标识（找不到返回空串）。
func conversationOf(obj map[string]any) string {
	if m, ok := obj["metadata"].(map[string]any); ok {
		for _, k := range []string{"conversation_id", "conversationId"} {
			if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
	}
	for _, k := range []string{"conversation_id", "conversationId", "conversation"} {
		if v, ok := obj[k].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// buildCacheKey 生成 `b2a-<uid8>-<hex16>` 形式的稳定键。
//
// uid8 提供账号隔离段；hex16 = sha256(uid|conversation)[:16] 提供会话段
// （同账号同会话稳定，不同会话不同；会话源为空时仍由 uid 单独哈希）。
func buildCacheKey(uid, conversation string) string {
	uid8 := uid
	if len(uid8) > 8 {
		uid8 = uid8[:8]
	}
	if uid8 == "" {
		uid8 = "-"
	}
	sum := sha256.Sum256([]byte(uid + "|" + conversation))
	return "b2a-" + uid8 + "-" + hex.EncodeToString(sum[:8])
}

// ConversationIDFromBody 由出站 body 的 prompt_cache_key 派生稳定的 X-Conversation-ID。
//
// 上游的前缀缓存按会话维度生效：每次换 X-Conversation-ID 会让缓存命中率恒为 0。
// 实测同一段 2415 token 前缀：随机 conv → hit=0/credit≈0.04；
// 固定 conv → hit=2368/credit≈0.01（约 4× 降本）。
//
// 返回标准 uuid 形状（上游既把它当不透明串，也当 uuid 用，保持形状最安全）：
// 取 sha256(prompt_cache_key) 前 16 字节，按 RFC 4122 v4 置位。
// body 无 prompt_cache_key 时返回 ""，调用方回退到旧的随机行为。
func ConversationIDFromBody(body []byte) string {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return ""
	}
	key, _ := obj["prompt_cache_key"].(string)
	if strings.TrimSpace(key) == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("conv|" + key))
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
