// Package apikey 管理 OpenAI 端点 API Key：创建、启停、常量时间比对鉴权。
package apikey

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"

	"buddy2api-go/internal/store"
)

// KeyInfo 鉴权通过后注入 context 的 key 信息。
type KeyInfo struct {
	ID   int64
	Name string
}

type ctxKey struct{}

// FromContext 取出当前请求的 key 信息。
func FromContext(ctx context.Context) *KeyInfo {
	v, _ := ctx.Value(ctxKey{}).(*KeyInfo)
	return v
}

// Manager key 管理器。
type Manager struct{ st *store.Store }

// New 创建管理器。
func New(st *store.Store) *Manager { return &Manager{st: st} }

// GenerateKey 生成随机 key：sk- + 24 字节 hex。
func GenerateKey() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "sk-" + hex.EncodeToString(b), nil
}

// Create 创建 key（custom 为空则随机生成）。
func (m *Manager) Create(name, custom string) (*store.APIKey, error) {
	plain := strings.TrimSpace(custom)
	if plain == "" {
		var err error
		if plain, err = GenerateKey(); err != nil {
			return nil, err
		}
	}
	if len(plain) < 8 {
		return nil, fmt.Errorf("自定义 key 至少 8 个字符")
	}
	k := &store.APIKey{
		KeyPrefix: plain[:min(8, len(plain))],
		KeyPlain:  plain,
		Name:      strings.TrimSpace(name),
	}
	if err := m.st.CreateKey(k); err != nil {
		return nil, err
	}
	return k, nil
}

// List 列表。
func (m *Manager) List() ([]store.APIKey, error) { return m.st.ListKeys() }

// Update 更新。
func (m *Manager) Update(id int64, name, status *string) error {
	return m.st.UpdateKey(id, name, status)
}

// Delete 删除。
func (m *Manager) Delete(id int64) error { return m.st.DeleteKey(id) }

// extractKey 从请求头提取 key：Authorization: Bearer <key> 或 X-API-Key: <key>。
func extractKey(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("X-API-Key")); v != "" {
		return v
	}
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	return ""
}

// openaiError OpenAI 风格错误响应。
func openaiError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"error":{"message":%q,"type":"%s","code":%q}}`, msg, "invalid_request_error", code)
}

// Authenticate 鉴权中间件：明文常量时间比对 + 状态校验。
func (m *Manager) Authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plain := extractKey(r)
		if plain == "" {
			openaiError(w, http.StatusUnauthorized, "invalid_api_key", "缺少 API Key（Authorization: Bearer <key> 或 X-API-Key）")
			return
		}
		keys, err := m.st.ListKeys()
		if err != nil {
			openaiError(w, http.StatusInternalServerError, "internal_error", "读取 key 列表失败")
			return
		}
		var matched *store.APIKey
		for i := range keys {
			// 常量时间比对，防时序侧信道
			if subtle.ConstantTimeCompare([]byte(plain), []byte(keys[i].KeyPlain)) == 1 {
				matched = &keys[i]
				break
			}
		}
		if matched == nil {
			openaiError(w, http.StatusUnauthorized, "invalid_api_key", "API Key 无效")
			return
		}
		if matched.Status != "active" {
			openaiError(w, http.StatusForbidden, "key_disabled", "API Key 已停用")
			return
		}
		info := &KeyInfo{ID: matched.ID, Name: matched.Name}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, info)))
	})
}

// RecordUsage 记录一次使用（请求完成时调用）。
func (m *Manager) RecordUsage(keyID int64, tokens int64) {
	_ = m.st.IncrementKeyUsage(keyID, tokens)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
