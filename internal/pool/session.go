package pool

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// 会话键请求头优先级。
//
// pi 1.4.0 在 models.json 里写 compat.sendSessionAffinityHeaders=true +
// sessionAffinityFormat="openai" 时会**同时**发前三个，值完全相同（都是它的
// sessionId，UUIDv7），所以取哪个都一致；保留多个只是为了兼容别的客户端。
//
// Go 的 textproto 会把头名规范化为 Xxx-Yyy（session_id → Session_id），
// 但 Header.Get 内部做同样的规范化，所以这里照小写写即可取到。
var sessionHeaders = []string{
	"x-session-affinity",  // pi compat / Fireworks / 部分中转
	"session_id",          // pi compat "openai" 格式
	"x-client-request-id", // pi / Codex CLI
	"x-session-id",        // OpenRouter 格式
	"x-opencode-session",  // opencode provider
}

// maxKeyLen 会话键长度上限。超过则取 sha256 截断。
//
// 键直接来自请求头/请求体，长度不受控（HTTP 头默认可到 1MB）。Router 用它做
// map 键与 TTL 条目，10k 条 × 1MB ≈ 10GB 常驻内存；每次分配还要 FNV 扫全键。
// 128 字节足够容纳任何正常的 sessionId / UUID / conversation UUID。
const maxKeyLen = 128

// normalizeKey 把过长键压成定长哈希，保留前缀以便排障时看出键的来源。
func normalizeKey(prefix, v string) string {
	if len(v) <= maxKeyLen {
		return prefix + v
	}
	sum := sha256.Sum256([]byte(v))
	return prefix + "sha256:" + hex.EncodeToString(sum[:])
}

// SessionKey 从请求里提取会话键（首个非空即用，绝不失败）。
//
// 分层（前 6 层显式、第 7 层派生、第 8 层放弃）：
//  1. 请求头（见 sessionHeaders）
//  2. body.prompt_cache_key —— OpenAI 官方字段，pi 需 PI_CACHE_RETENTION=long 才发
//  3. body.metadata.conversation_id / conversationId
//  4. body.conversation_id / conversationId / conversation（顶层）
//  5. 派生：sha256(api_key_id | 首条 user 文本)  ← 必须掺 api_key_id
//  6. 全空 → 返回 ""（调用方走纯分发，不伪造粘性）
//
// body 是已反序列化的请求体（proxy 层为了 model/stream 已经解过一次，不重复解析）。
// header 传 nil 时只用 body 与派生。apiKeyID 用于隔离不同调用方。
func SessionKey(header func(string) string, body map[string]any, apiKeyID int64) string {
	if header != nil {
		for _, h := range sessionHeaders {
			if v := strings.TrimSpace(header(h)); v != "" {
				return normalizeKey("h:", v)
			}
		}
	}
	if body != nil {
		if v, ok := body["prompt_cache_key"].(string); ok && strings.TrimSpace(v) != "" {
			return normalizeKey("c:", strings.TrimSpace(v))
		}
		for _, nested := range []string{"metadata", "metadata_json"} {
			m, ok := body[nested].(map[string]any)
			if !ok {
				continue
			}
			for _, k := range []string{"conversation_id", "conversationId"} {
				if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
					return normalizeKey("m:", strings.TrimSpace(v))
				}
			}
		}
		for _, k := range []string{"conversation_id", "conversationId", "conversation"} {
			if v, ok := body[k].(string); ok && strings.TrimSpace(v) != "" {
				return normalizeKey("b:", strings.TrimSpace(v))
			}
		}
		// 派生兜底
		if txt := firstUserText(body); txt != "" {
			return "d:" + deriveKey(apiKeyID, txt)
		}
	}
	return ""
}

// deriveKey 派生键：sha256(api_key_id | \x00 | 首条 user 文本)。
//
// 必须掺 api_key_id：否则两个调用方首条都发「帮我写个排序」会撞同一个键，
// 从而被永久绑到同一个账号上，池化失效。
//
// 只取首条 user 的文本、不取 system：system prompt 里可能含时间戳等易变内容
// （Cline 就是往 system 里塞当前时间，每请求都变），纳进去会让派生键恒变。
func deriveKey(apiKeyID int64, firstUser string) string {
	h := sha256.New()
	h.Write([]byte(itoa(apiKeyID)))
	h.Write([]byte{0})
	h.Write([]byte(firstUser))
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// firstUserText 取**首条** role=user 消息的文本。
//
// 取首条而非最后一条是关键：最后一条是「当前轮」，用户每说一句就变，
// 用它当键等于每轮换一个会话。（部分网关用最后一条是为了派生上游的聚合 ID，
// 语义相反，不要抄混。）
//
// content 两种形态都要处理：字符串，以及 pi 实测的数组
// [{"type":"text","text":"..."}]。
func firstUserText(body map[string]any) string {
	msgs, ok := body["messages"].([]any)
	if !ok {
		return ""
	}
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := mm["role"].(string); role != "user" {
			continue
		}
		if txt := contentText(mm["content"]); txt != "" {
			return txt
		}
	}
	return ""
}

// contentText 取消息 content 的文本。
//
// 字符串形态直接返回；数组形态（多模态 parts）拼接各 part 的 text。
// 数组里可能同时有 image_url 等非文本 part，必须按 type 过滤——
// 不过滤会把整个 part 的 JSON 形状当成文本，导致同一会话不同图片时派生出不同键。
func contentText(v any) string {
	switch c := v.(type) {
	case string:
		return strings.TrimSpace(c)
	case []any:
		var sb strings.Builder
		for _, part := range c {
			p, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := p["type"].(string); t != "" && t != "text" {
				continue
			}
			if s, ok := p["text"].(string); ok && s != "" {
				sb.WriteString(s)
			}
		}
		return strings.TrimSpace(sb.String())
	case map[string]any:
		// 少数客户端把 content 包成 {"type":"text","text":"..."}
		if s, ok := c["text"].(string); ok {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// itoa 避免为一次整数转字符串引入 strconv（此处调用频率极低，自己写更短）。
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
