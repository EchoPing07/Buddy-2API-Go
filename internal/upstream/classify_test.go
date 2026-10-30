package upstream

import (
	"testing"
	"time"
)

// 以下错误体全部来自本项目生产日志（data/buddy2api.db）的真实原文。
const (
	realBody6004  = `{"code":6004,"msg":"您的使用量已超出频率限制，将在 2026-09-03 22:27:42 UTC+8 重置，您也可以切换其他模型继续使用。","requestId":"r1"}`
	realBody14018 = `{"error":{"data":{"code":14018,"msg":"额度已用尽，请访问以下链接，购买加量包以获取更多额度：https://www.codebuddy.cn/profile/usage ","requestId":"r2"}}}`
	realBody11128 = `{"code":11128,"msg":"Illegal API invocation from an unapproved channel","requestId":"r3"}`
)

func TestBizCode(t *testing.T) {
	cases := []struct {
		name string
		body string
		code int
		msg  string // 只校验非空性/包含关系，见断言
	}{
		{"顶层包络", realBody6004, 6004, "频率限制"},
		{"嵌套 error.data", realBody14018, 14018, "额度已用尽"},
		{"顶层 11128", realBody11128, 11128, "unapproved channel"},
		{"data 层", `{"data":{"code":42,"msg":"m"}}`, 42, "m"},
		{"error 直挂", `{"error":{"code":7,"msg":"e"}}`, 7, "e"},
		{"字符串 code", `{"error":{"message":"x","code":"6004"}}`, 6004, ""},
		{"非 JSON", `<html>502</html>`, 0, ""},
		{"空体", ``, 0, ""},
		{"无 code", `{"msg":"只有消息"}`, 0, "只有消息"},
	}
	for _, c := range cases {
		code, msg := BizCode([]byte(c.body))
		if code != c.code {
			t.Errorf("%s: code = %d，期望 %d", c.name, code, c.code)
		}
		if c.msg != "" && !contains(msg, c.msg) {
			t.Errorf("%s: msg = %q，期望包含 %q", c.name, msg, c.msg)
		}
	}
}

// TestBizCodeNestedJSONCode string 形态的 code 出现在 SSE 错误帧里（实测 pi 侧的 6004 帧）。
func TestBizCodeStringCodeInFrame(t *testing.T) {
	frame := `{"error":{"message":"您的使用量已超出频率限制","code":"6004","requestId":"r"}}`
	code, msg := BizCode([]byte(frame))
	if code != 6004 {
		t.Fatalf("code = %d，期望 6004", code)
	}
	if !contains(msg, "频率限制") {
		t.Errorf("msg = %q，期望包含频率限制", msg)
	}
}

// TestBizCodeDeepNestingGuard 自嵌套 JSON 不能无限递归。
func TestBizCodeDeepNestingGuard(t *testing.T) {
	body := `{"data":{"data":{"data":{"data":{"data":{"code":99,"msg":"deep"}}}}}}`
	code, _ := BizCode([]byte(body))
	if code != 0 {
		t.Errorf("超过深度上限应返回 0，得到 %d", code)
	}
}

func TestClassifyChatError(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   ChatErrKind
	}{
		// ── 账号级终态 ──
		{"14018 嵌套", 429, realBody14018, ErrKindQuotaExhausted},
		{"402 HTTP", 402, `{}`, ErrKindQuotaExhausted},
		{"402 body code", 429, `{"code":402,"msg":"payment"}`, ErrKindQuotaExhausted},
		{"401", 401, ``, ErrKindCredential},
		// 14018 与 401 同时出现时，额度优先（用户看到的原因更准确）
		{"401 但 body 是 14018", 401, realBody14018, ErrKindQuotaExhausted},

		// ── 模型级限额 ──
		{"6004 顶层", 429, realBody6004, ErrKindModelRate},
		{"裸 429", 429, `{"msg":"rate limited"}`, ErrKindModelRate},
		{"429 空体", 429, ``, ErrKindModelRate},

		// ── 不惩罚 ──
		{"11128 渠道", 400, realBody11128, ErrKindChannel},
		{"11102 无此模型", 400, `{"code":11102,"msg":"model not found"}`, ErrKindModelBlocked},
		{"5xx", 503, ``, ErrKindServer},
		{"502 带 body", 502, `<html>bad gateway</html>`, ErrKindServer},
		{"其它 400", 400, `{"code":1,"msg":"bad"}`, ErrKindBadParams},
		{"404", 404, `{}`, ErrKindBadParams},

		// ── 无错误 ──
		{"200 正常体", 200, `{"id":"x","choices":[]}`, ErrKindNone},
		{"200 空体", 200, ``, ErrKindNone},
	}
	for _, c := range cases {
		if got := ClassifyChatError(c.status, []byte(c.body)); got != c.want {
			t.Errorf("%s: ClassifyChatError(%d) = %v，期望 %v", c.name, c.status, got, c.want)
		}
	}
}

// TestClassifySSEErrorFrame HTTP 200 + body 里带错误帧（流中途报错）。
func TestClassifySSEErrorFrame(t *testing.T) {
	frame := `{"error":{"message":"您的使用量已超出频率限制","code":"6004"}}`
	if got := ClassifyChatError(200, []byte(frame)); got != ErrKindModelRate {
		t.Errorf("流内 6004 错误帧 = %v，期望 ErrKindModelRate", got)
	}
	quotaFrame := `{"error":{"data":{"code":14018,"msg":"额度已用尽"}}}`
	if got := ClassifyChatError(200, []byte(quotaFrame)); got != ErrKindQuotaExhausted {
		t.Errorf("流内 14018 错误帧 = %v，期望 ErrKindQuotaExhausted", got)
	}
}

func TestParseRateReset(t *testing.T) {
	// 上游文案为 UTC+8 墙钟，必须按 UTC+8 解释（与进程时区无关）
	want := time.Date(2026, 9, 3, 22, 27, 42, 0, softRateResetLoc)
	if got, ok := ParseRateReset([]byte(realBody6004)); !ok || !got.Equal(want) {
		t.Fatalf("ParseRateReset = (%v, %v)，期望 (%v, true)", got, ok, want)
	}
	// 带不带 " UTC+8" 后缀都应解析出同一时刻
	noSuffix := `{"code":6004,"msg":"将在 2026-09-03 22:27:42 重置"}`
	if got, ok := ParseRateReset([]byte(noSuffix)); !ok || !got.Equal(want) {
		t.Errorf("无后缀 = (%v, %v)，期望 (%v, true)", got, ok, want)
	}
	// 14018 没有重置时间，必须返回 false（否则会臆造一个时间）
	if _, ok := ParseRateReset([]byte(realBody14018)); ok {
		t.Error("14018 不应解析出重置时间")
	}
	// 非法/缺失时间
	for _, body := range []string{
		`{"code":6004,"msg":"已超出频率限制"}`,
		`{"code":6004,"msg":"将在 明天 重置"}`,
		``,
		`not json`,
	} {
		if _, ok := ParseRateReset([]byte(body)); ok {
			t.Errorf("%q 不应解析出重置时间", body)
		}
	}
}

// TestChatErrKindRetryable 锁定「哪些类允许网关内换号重试」。
// 5xx / transport 不重试是刻意的：上游可能已计费，交给调用方决定。
func TestChatErrKindRetryable(t *testing.T) {
	yes := []ChatErrKind{ErrKindModelRate, ErrKindQuotaExhausted, ErrKindCredential}
	for _, k := range yes {
		if !k.Retryable() {
			t.Errorf("%v 应可重试", k)
		}
		if !k.Punishes() {
			t.Errorf("%v 应惩罚账号", k)
		}
	}
	no := []ChatErrKind{ErrKindNone, ErrKindServer, ErrKindTransport, ErrKindChannel,
		ErrKindModelBlocked, ErrKindBadParams, ErrKindPassThrough}
	for _, k := range no {
		if k.Retryable() {
			t.Errorf("%v 不应可重试", k)
		}
		if k.Punishes() {
			t.Errorf("%v 不应惩罚账号", k)
		}
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
