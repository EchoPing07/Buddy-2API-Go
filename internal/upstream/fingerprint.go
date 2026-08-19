// Package upstream 封装 CodeBuddy 上游客户端：chat 转发、token 刷新、
// OAuth 设备流、billing、签到、/v3/config 模型列表，以及 CLI 指纹头。
//
// 指纹头逐字移植自 tests/fingerprint.go（实测验证），规范见 DEVELOPMENT.md §7。
package upstream

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strings"

	"buddy2api-go/internal/auth"
)

const (
	ideVersion              = "2.109.2"
	stainlessPackageVersion = "5.10.1"
	nodeRuntimeVersion      = "v22.13.1"
)

func userAgent() string {
	if v := strings.TrimSpace(os.Getenv("CB_GATEWAY_USER_AGENT")); v != "" {
		return v
	}
	return fmt.Sprintf("CLI/%s CodeBuddy/%s", ideVersion, ideVersion)
}

func stainlessOS() string {
	if v := strings.TrimSpace(os.Getenv("CB_GATEWAY_STAINLESS_OS")); v != "" {
		return v
	}
	switch runtime.GOOS {
	case "windows":
		return "Windows"
	case "darwin":
		return "macOS"
	default:
		return "Linux"
	}
}

// originFor 按 domain 推导 Origin/Referer（与 tests originFor() 一致，实测可用）：
// 含 workbuddy → 国际 Origin，否则一律中国 Origin（国际端点亦发 cn Origin）。
func originFor(domain string) string {
	if strings.Contains(strings.ToLower(domain), "workbuddy") {
		return "https://www.workbuddy.ai"
	}
	return "https://www.codebuddy.cn"
}

// randHex 生成 n 字节的十六进制字符串。
func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// uuidHex 生成无连字符的 uuid hex（32 字符）。
func uuidHex() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant
	return hex.EncodeToString(b)
}

// uuidStd 生成标准 uuid 字符串。
func uuidStd() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func domainHeader(domain string) (string, string) {
	if strings.TrimSpace(domain) != "" {
		return "X-Domain", domain
	}
	return "X-No-Department-Info", "1"
}

func accountHeader(value, header, noHeader string) (string, string) {
	if strings.TrimSpace(value) != "" {
		return header, value
	}
	return noHeader, "1"
}

// commonHeaders 所有 API 共享的通用指纹头（含 B3 追踪头）。
func commonHeaders(domain string) http.Header {
	origin := originFor(domain)
	requestID := uuidHex()
	spanID := randHex(8)
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json, text/plain, */*")
	h.Set("X-Requested-With", "XMLHttpRequest")
	h.Set("Origin", origin)
	h.Set("Referer", origin+"/")
	h.Set("X-Product", "SaaS")
	h.Set("User-Agent", userAgent())
	h.Set("X-Request-ID", requestID)
	h.Set("X-B3-TraceId", requestID)
	h.Set("X-B3-SpanId", spanID)
	h.Set("X-B3-Sampled", "1")
	h.Set("b3", fmt.Sprintf("%s-%s-1-", requestID, spanID))
	dk, dv := domainHeader(domain)
	h.Set(dk, dv)
	return h
}

// chatHeaders chat 请求头：通用 + 账号 + IDE/CLI + SDK。
// 红线：绝不带 X-Refresh-Token。
func chatHeaders(t *auth.Token) http.Header {
	h := commonHeaders(t.Domain)

	k, v := accountHeader(t.AccessToken, "Authorization", "X-No-Authorization")
	if k == "Authorization" {
		h.Set(k, "Bearer "+v)
	} else {
		h.Set(k, v)
	}

	k, v = accountHeader(t.UID, "X-User-Id", "X-No-User-Id")
	h.Set(k, v)
	k, v = accountHeader(t.EnterpriseID, "X-Enterprise-Id", "X-No-Enterprise-Id")
	h.Set(k, v)
	if t.EnterpriseID != "" {
		h.Set("X-Tenant-Id", t.EnterpriseID)
	}

	h.Set("X-IDE-Type", "CLI")
	h.Set("X-IDE-Name", "CLI")
	h.Set("X-IDE-Version", ideVersion)
	h.Set("x-codebuddy-request", "1")
	h.Set("X-Agent-Intent", "craft")
	h.Set("X-Conversation-ID", uuidStd())
	h.Set("X-Conversation-Request-ID", uuidHex())
	h.Set("X-Conversation-Message-ID", uuidHex())
	h.Set("x-stainless-arch", "x64")
	h.Set("x-stainless-lang", "js")
	h.Set("x-stainless-os", stainlessOS())
	h.Set("x-stainless-package-version", stainlessPackageVersion)
	h.Set("x-stainless-retry-count", "0")
	h.Set("x-stainless-runtime", "node")
	h.Set("x-stainless-runtime-version", nodeRuntimeVersion)
	return h
}

// billingHeaders 精简指纹：通用子集 + 账号头，无 IDE/SDK/Conversation。
func billingHeaders(t *auth.Token) http.Header {
	h := http.Header{}
	h.Set("Accept", "application/json")
	h.Set("Content-Type", "application/json")
	h.Set("X-Product", "SaaS")
	h.Set("User-Agent", userAgent())
	dk, dv := domainHeader(t.Domain)
	h.Set(dk, dv)

	k, v := accountHeader(t.AccessToken, "Authorization", "X-No-Authorization")
	if k == "Authorization" {
		h.Set(k, "Bearer "+v)
	} else {
		h.Set(k, v)
	}
	k, v = accountHeader(t.UID, "X-User-Id", "X-No-User-Id")
	h.Set(k, v)
	k, v = accountHeader(t.EnterpriseID, "X-Enterprise-Id", "X-No-Enterprise-Id")
	h.Set(k, v)
	if t.EnterpriseID != "" {
		h.Set("X-Tenant-Id", t.EnterpriseID)
	}
	return h
}

// refreshHeaders token 刷新头。X-Refresh-Token 只出现在这里。
func refreshHeaders(t *auth.Token) http.Header {
	h := commonHeaders(t.Domain)
	h.Set("Accept", "application/json")
	h.Set("Cache-Control", "no-cache")
	h.Set("Pragma", "no-cache")
	if t.AccessToken != "" {
		h.Set("Authorization", "Bearer "+t.AccessToken)
	}
	h.Set("X-Refresh-Token", t.RefreshToken)
	h.Set("X-Auth-Refresh-Source", "plugin")
	k, v := accountHeader(t.UID, "X-User-Id", "X-No-User-Id")
	h.Set(k, v)
	k, v = accountHeader(t.EnterpriseID, "X-Enterprise-Id", "X-No-Enterprise-Id")
	h.Set(k, v)
	return h
}

// oauthHeaders OAuth 启动/轮询头：无账号，X-No-* 标记（值 true，实测可用）。
func oauthHeaders() http.Header {
	h := http.Header{}
	h.Set("Accept", "application/json, text/plain, */*")
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-cache")
	h.Set("Pragma", "no-cache")
	h.Set("X-Requested-With", "XMLHttpRequest")
	h.Set("X-Domain", "www.codebuddy.ai") // 固定值，中国端点同样如此（实测可登录）
	h.Set("X-No-Authorization", "true")
	h.Set("X-No-User-Id", "true")
	h.Set("X-No-Enterprise-Id", "true")
	h.Set("X-No-Department-Info", "true")
	h.Set("User-Agent", userAgent())
	h.Set("X-Product", "SaaS")
	h.Set("X-Request-ID", uuidHex())
	h.Set("X-B3-TraceId", uuidHex())
	h.Set("X-B3-SpanId", randHex(8))
	h.Set("X-B3-Sampled", "1")
	return h
}

// configHeaders /v3/config 用 VSCode 指纹（实测可用）。
func configHeaders(t *auth.Token) http.Header {
	h := http.Header{}
	h.Set("Accept", "application/json, text/plain, */*")
	h.Set("X-Requested-With", "XMLHttpRequest")
	h.Set("Authorization", "Bearer "+t.AccessToken)
	h.Set("X-User-Id", t.UID)
	h.Set("X-Domain", t.Domain)
	h.Set("X-Product", "SaaS")
	h.Set("X-IDE-Type", "VSCode")
	h.Set("X-IDE-Name", "VSCode")
	h.Set("X-IDE-Version", "1.119.0")
	h.Set("X-Product-Version", "4.9.29177644")
	h.Set("X-Request-Trace-Id", uuidStd())
	h.Set("X-Env-ID", "production")
	h.Set("User-Agent", "VSCode/1.119.0 CodeBuddy/4.9.29177644")
	return h
}
