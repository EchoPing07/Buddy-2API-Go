// Package auth 管理 data/token.json：读写、JWT 解析、过期判定与单飞刷新。
package auth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ErrNoToken 表示尚未登录（token.json 不存在）。
var ErrNoToken = errors.New("尚未登录，请先在管理后台扫码登录")

// Token 账号凭证（OAuth 模式）。
type Token struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresAt    int64  `json:"expires_at"` // unix 秒
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterprise_id"` // 可空（个人账号）
	Domain       string `json:"domain"`
	Nickname     string `json:"nickname,omitempty"`
	SavedAt      int64  `json:"saved_at"`
}

// iss 格式: https://<domain>/auth/realms/sso-<enterprise_id>
// 中国端点 realm 为 copilot（无 sso- 前缀），enterprise_id 为空。
var (
	issDomainRE     = regexp.MustCompile(`^https?://([^/]+)`)
	issEnterpriseRE = regexp.MustCompile(`/sso-([^/]+)$`)
)

// ParseJWT 不验签，仅解码 payload 取 sub / iss 域 / enterprise_id / nickname / exp。
func ParseJWT(token string) (sub, domain, enterpriseID, nickname string, exp int64, err error) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return "", "", "", "", 0, fmt.Errorf("不是合法 JWT（段数不足）")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", "", "", "", 0, fmt.Errorf("解码 JWT payload: %w", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", "", "", "", 0, fmt.Errorf("解析 JWT claims: %w", err)
	}
	sub, _ = claims["sub"].(string)
	iss, _ := claims["iss"].(string)
	if m := issDomainRE.FindStringSubmatch(iss); len(m) == 2 {
		domain = m[1]
	}
	if m := issEnterpriseRE.FindStringSubmatch(iss); len(m) == 2 {
		enterpriseID = m[1]
	}
	nickname, _ = claims["nickname"].(string)
	if e, ok := claims["exp"].(float64); ok {
		exp = int64(e)
	}
	return sub, domain, enterpriseID, nickname, exp, nil
}

// EnrichFromJWT 用 JWT claims 填充 Token 的 uid/eid/domain/nickname/exp。
func (t *Token) EnrichFromJWT() {
	if sub, dom, eid, nick, exp, err := ParseJWT(t.AccessToken); err == nil {
		if t.UID == "" {
			t.UID = sub
		}
		if t.EnterpriseID == "" {
			t.EnterpriseID = eid
		}
		if t.Domain == "" {
			t.Domain = dom
		}
		if t.Nickname == "" {
			t.Nickname = nick
		}
		if t.ExpiresAt == 0 && exp > 0 {
			t.ExpiresAt = exp
		}
	}
}

// IsExpired 提前 60s 判定过期。
func (t *Token) IsExpired() bool {
	if t == nil || t.ExpiresAt == 0 {
		return false
	}
	return time.Now().Unix() >= t.ExpiresAt-60
}

// ExpiresInHuman 可读剩余时间。
func (t *Token) ExpiresInHuman() string {
	if t == nil || t.ExpiresAt == 0 {
		return "未知"
	}
	d := time.Until(time.Unix(t.ExpiresAt, 0))
	if d < 0 {
		return "已过期"
	}
	days := int(d.Hours()) / 24
	if days > 0 {
		return fmt.Sprintf("%d 天", days)
	}
	return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
}

// TokenStore 线程安全的 token.json 读写 + 单飞刷新。
//
// mu 保护 t（读写锁，读取走 RLock 不阻塞并发读）；refreshMu 串行化刷新，
// 配合「进入后重检」实现单飞，且刷新 HTTP 在 mu 锁外执行，
// 避免刷新期间阻塞所有 Get（原实现刷新 HTTP 全程持互斥锁，高并发下 = 全站卡顿）。
type TokenStore struct {
	mu        sync.RWMutex
	path      string
	t         *Token
	refreshMu sync.Mutex
}

// NewTokenStore 创建并尝试加载 data/token.json。
func NewTokenStore(dataDir string) (*TokenStore, error) {
	s := &TokenStore{path: filepath.Join(dataDir, "token.json")}
	if err := s.Load(); err != nil {
		return nil, err
	}
	return s, nil
}

// Load 从磁盘加载（启动时调用）。
func (s *TokenStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // 未登录属正常状态
		}
		return err
	}
	var t Token
	if err := json.Unmarshal(raw, &t); err != nil {
		return fmt.Errorf("解析 token.json: %w", err)
	}
	// 补提 nickname（旧文件可能没存）
	if t.Nickname == "" && t.AccessToken != "" {
		if _, _, _, nick, _, err := ParseJWT(t.AccessToken); err == nil {
			t.Nickname = nick
		}
	}
	s.t = &t
	return nil
}

// Get 返回 token 副本；未登录返回 nil。读锁不阻塞并发读，也不阻塞于刷新。
func (s *TokenStore) Get() *Token {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.t == nil {
		return nil
	}
	cp := *s.t
	return &cp
}

// Save 保存 token 到内存 + 磁盘（0600）。
func (s *TokenStore) Save(t *Token) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked(t)
}

func (s *TokenStore) saveLocked(t *Token) error {
	t.SavedAt = time.Now().Unix()
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(s.path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	if err := os.WriteFile(s.path, data, 0o600); err != nil {
		return err
	}
	s.t = t
	return nil
}

// Clear 删除凭证（登出账号）。
func (s *TokenStore) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.t = nil
	err := os.Remove(s.path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// EnsureValid 单飞刷新过期 token。刷新 HTTP 在 token 锁外执行，不阻塞并发 Get。
func (s *TokenStore) EnsureValid(refreshFn func(t *Token) (*Token, error)) error {
	s.mu.RLock()
	t := s.t
	s.mu.RUnlock()
	if t == nil {
		return ErrNoToken
	}
	if !t.IsExpired() {
		return nil
	}
	return s.refresh(refreshFn, false)
}

// ForceRefresh 强制刷新（手动触发 / 401 重试）。
func (s *TokenStore) ForceRefresh(refreshFn func(t *Token) (*Token, error)) error {
	s.mu.RLock()
	t := s.t
	s.mu.RUnlock()
	if t == nil {
		return ErrNoToken
	}
	return s.refresh(refreshFn, true)
}

// refresh 单飞刷新：refreshMu 串行化，进入后重检 token 状态（可能已被另一
// goroutine 刷新或被登出清空）；force=true 时跳过过期判断总是刷新。
// refreshFn 在 token 锁外执行，期间 Get 可继续返回（可能为旧的）token。
// 落盘前以写锁原子校验 s.t 仍为刷新起点，避免覆盖并发登录/登出产生的新凭证。
func (s *TokenStore) refresh(refreshFn func(t *Token) (*Token, error), force bool) error {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	s.mu.RLock()
	t := s.t
	s.mu.RUnlock()
	if t == nil {
		return ErrNoToken
	}
	if !force && !t.IsExpired() {
		return nil // 另一 goroutine 已刷新
	}
	tcopy := *t
	nt, err := refreshFn(&tcopy)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.t != t {
		return nil // 期间 token 已被替换（如重新登录），放弃本次刷新结果
	}
	return s.saveLocked(nt)
}
