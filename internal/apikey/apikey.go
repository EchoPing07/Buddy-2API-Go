// Package apikey 管理 OpenAI 端点 API Key：创建、启停、常量时间比对鉴权。
package apikey

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"

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
//
// cache 在内存中持有全量 key 副本，鉴权路径无需每请求查库；
// Create/Update/Delete 后 reload 刷新缓存。usageCh 把用量更新异步落库，
// 避免请求 defer 上的同步写库阻塞热路径（原实现每请求 SELECT 全表 + 同步 UPDATE）。
type Manager struct {
	st      *store.Store
	mu      sync.RWMutex
	cache   []store.APIKey
	usageCh chan usageUpdate
}

type usageUpdate struct {
	keyID  int64
	tokens int64
}

// New 创建管理器并加载缓存、启动用量更新 worker。
func New(st *store.Store) *Manager {
	m := &Manager{st: st, usageCh: make(chan usageUpdate, 256)}
	if err := m.reload(); err != nil {
		slog.Error("加载 API Key 缓存失败", "error", err)
	}
	go m.usageWorker()
	return m
}

// reload 从库重新加载全量 key 到缓存。
func (m *Manager) reload() error {
	keys, err := m.st.ListKeys()
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.cache = keys
	m.mu.Unlock()
	return nil
}

func (m *Manager) usageWorker() {
	for u := range m.usageCh {
		if err := m.st.IncrementKeyUsage(u.keyID, u.tokens); err != nil {
			slog.Error("更新 key 用量失败", "error", err)
		}
	}
}

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
	if err := m.reload(); err != nil {
		slog.Warn("刷新 key 缓存失败", "error", err)
	}
	return k, nil
}

// List 列表。
func (m *Manager) List() ([]store.APIKey, error) { return m.st.ListKeys() }

// Update 更新。
func (m *Manager) Update(id int64, name, status *string) error {
	if err := m.st.UpdateKey(id, name, status); err != nil {
		return err
	}
	if err := m.reload(); err != nil {
		slog.Warn("刷新 key 缓存失败", "error", err)
	}
	return nil
}

// Delete 删除。
func (m *Manager) Delete(id int64) error {
	if err := m.st.DeleteKey(id); err != nil {
		return err
	}
	if err := m.reload(); err != nil {
		slog.Warn("刷新 key 缓存失败", "error", err)
	}
	return nil
}

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
		m.mu.RLock()
		keys := m.cache
		m.mu.RUnlock()
		var matched store.APIKey
		found := false
		for i := range keys {
			// 常量时间比对，防时序侧信道；不 break，使总耗时与命中位置无关
			if subtle.ConstantTimeCompare([]byte(plain), []byte(keys[i].KeyPlain)) == 1 {
				matched = keys[i]
				found = true
			}
		}
		if !found {
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

// RecordUsage 异步记录一次使用（请求完成时调用）；队列满则丢弃，不阻塞热路径。
func (m *Manager) RecordUsage(keyID int64, tokens int64) {
	select {
	case m.usageCh <- usageUpdate{keyID, tokens}:
	default:
		slog.Warn("用量更新队列已满，丢弃一次更新")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
