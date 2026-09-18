package admin

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestSessionCookieSecure 校验会话 cookie 的 Secure 标志随请求链路是否为 HTTPS 变化。
//
// 两个方向均须成立：明文直连不得带 Secure（否则浏览器丢弃 cookie、无法登录），
// HTTPS（直连 TLS 或反代声明）必须带 Secure。
func TestSessionCookieSecure(t *testing.T) {
	s := &Session{name: "b2a_session", key: make([]byte, 32)}

	cases := []struct {
		name         string
		tls          bool
		forwardProto string
		wantSecure   bool
	}{
		{"明文直连", false, "", false},
		{"直连 TLS", true, "", true},
		{"反代声明 https", false, "https", true},
		{"反代声明大写 HTTPS", false, "HTTPS", true},
		{"反代声明 https + 空白", false, "  https  ", true},
		{"反代链：客户端伪造 http，反代追加 https", false, "http, https", true},
		{"反代链：客户端伪造 https，反代追加 http（TLS 终结在别处）", false, "https, http", false},
		{"反代声明 http（TLS 终结在别处）", false, "http", false},
		{"反代声明垃圾值", false, "banana", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/admin/login", nil)
			if c.forwardProto != "" {
				req.Header.Set("X-Forwarded-Proto", c.forwardProto)
			}
			if c.tls {
				req.TLS = &tls.ConnectionState{}
			}
			s.Issue(rec, req)

			cookies := rec.Result().Cookies()
			if len(cookies) == 0 {
				t.Fatal("未下发 cookie")
			}
			ck := cookies[0]
			if ck.Secure != c.wantSecure {
				t.Errorf("Secure = %v，期望 %v", ck.Secure, c.wantSecure)
			}
			// 任意链路下 HttpOnly 与 SameSite 均不得丢失
			if !ck.HttpOnly {
				t.Error("HttpOnly 丢失")
			}
			if ck.SameSite != http.SameSiteLaxMode {
				t.Errorf("SameSite = %v，期望 Lax", ck.SameSite)
			}
			if ck.Value == "" {
				t.Error("cookie 值为空")
			}
		})
	}
}

// TestSessionClearSecureMatchesIssue 校验清除 cookie 的 Secure 与下发时一致；否则 HTTPS 下无法清除会话。
func TestSessionClearSecureMatchesIssue(t *testing.T) {
	s := &Session{name: "b2a_session", key: make([]byte, 32)}
	for _, c := range []struct {
		name         string
		tls          bool
		forwardProto string
		wantSecure   bool
	}{
		{"明文", false, "", false},
		{"反代 https", false, "https", true},
		{"直连 TLS", true, "", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/admin/logout", nil)
			if c.forwardProto != "" {
				req.Header.Set("X-Forwarded-Proto", c.forwardProto)
			}
			if c.tls {
				req.TLS = &tls.ConnectionState{}
			}
			rec := httptest.NewRecorder()
			s.Clear(rec, req)
			cookies := rec.Result().Cookies()
			if len(cookies) == 0 {
				t.Fatal("未下发 cookie")
			}
			if cookies[0].Secure != c.wantSecure {
				t.Errorf("Clear Secure = %v，期望 %v", cookies[0].Secure, c.wantSecure)
			}
			if cookies[0].MaxAge >= 0 {
				t.Errorf("Clear 应设置 MaxAge<0，实际 %d", cookies[0].MaxAge)
			}
		})
	}
}

// TestIsHTTPSNilRequest 校验 isHTTPS 对 nil 请求不 panic。
func TestIsHTTPSNilRequest(t *testing.T) {
	if isHTTPS(nil) {
		t.Error("nil 请求应为 false")
	}
}
