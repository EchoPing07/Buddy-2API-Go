// Package admin 管理后台 API：登录会话、账号、keys、余额、签到、日志、设置。
package admin

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Session 基于 HMAC-SHA256 签名 cookie 的会话（无服务端状态，标准库实现）。
type Session struct {
	key  []byte
	name string
	ttl  time.Duration
}

// NewSession 创建会话管理器；HMAC 密钥持久化在 data/session_key（重启不掉线）。
func NewSession(dataDir string) (*Session, error) {
	s := &Session{name: "b2a_session", ttl: 24 * time.Hour}
	path := filepath.Join(dataDir, "session_key")
	if raw, err := os.ReadFile(path); err == nil && len(raw) == 32 {
		s.key = raw
		return s, nil
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, key, 0o600); err != nil {
		return nil, err
	}
	s.key = key
	return s, nil
}

// Issue 下发 HttpOnly 会话 cookie。
func (s *Session) Issue(w http.ResponseWriter) {
	expiry := time.Now().Add(s.ttl).Unix()
	nonce := randHex(8)
	payload := fmt.Sprintf("%d|%s", expiry, nonce)
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(payload))
	value := base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + hex.EncodeToString(mac.Sum(nil))
	http.SetCookie(w, &http.Cookie{
		Name:     s.name,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(s.ttl),
	})
}

// Clear 清除会话 cookie。
func (s *Session) Clear(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.name,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// Valid 校验请求携带的会话 cookie。
func (s *Session) Valid(r *http.Request) bool {
	ck, err := r.Cookie(s.name)
	if err != nil || ck.Value == "" {
		return false
	}
	parts := strings.SplitN(ck.Value, ".", 2)
	if len(parts) != 2 {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, s.key)
	mac.Write(payload)
	if !hmac.Equal(mac.Sum(nil), decodeHex(parts[1])) {
		return false
	}
	fields := strings.SplitN(string(payload), "|", 2)
	if len(fields) != 2 {
		return false
	}
	expiry, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil || time.Now().Unix() > expiry {
		return false
	}
	return true
}

func decodeHex(s string) []byte {
	b, _ := hex.DecodeString(s)
	return b
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
