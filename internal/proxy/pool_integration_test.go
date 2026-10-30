package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"buddy2api-go/internal/apikey"
	"buddy2api-go/internal/auth"
	"buddy2api-go/internal/config"
	"buddy2api-go/internal/pool"
	"buddy2api-go/internal/store"
	"buddy2api-go/internal/upstream"
)

// 生产日志里的真实错误体（见 docs/POOL-LOADBALANCE.md §3）。
const (
	sseOK = `data: {"id":"c1","model":"glm","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"c1","model":"glm","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12,"credit":0.01}}` + "\n\n" +
		"data: [DONE]\n\n"
	errBody6004  = `{"code":6004,"msg":"您的使用量已超出频率限制，将在 2026-09-03 22:27:42 UTC+8 重置，您也可以切换其他模型继续使用。","requestId":"r1"}`
	errBody14018 = `{"error":{"data":{"code":14018,"msg":"额度已用尽，请访问以下链接，购买加量包以获取更多额度：https://www.codebuddy.cn/profile/usage ","requestId":"r2"}}}`
)

// fakeChat 假 chat 上游：按 Authorization 里的账号区分行为。
//
// 行为用「每个账号一个脚本」表达：script 是响应动作的序列，用完后重复最后一个。
// 这样单测能精确表达「第一次 6004、之后 200」这种场景。
type fakeChat struct {
	mu       sync.Mutex
	hits     map[string]int
	scripts  map[string][]func(w http.ResponseWriter)
	default_ func(w http.ResponseWriter)
}

func newFakeChat() *fakeChat {
	return &fakeChat{hits: map[string]int{}, scripts: map[string][]func(http.ResponseWriter){}}
}

// plan 为该账号设定响应序列。
func (f *fakeChat) plan(uid string, steps ...func(w http.ResponseWriter)) {
	f.mu.Lock()
	f.scripts[uid] = steps
	f.mu.Unlock()
}

func respOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, sseOK)
}

func respErr(status int, body string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}
}

// respSSEWithErrorFrame 200 开流后，流里带一个错误帧（流中途报错）。
func respSSEWithErrorFrame(frame string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `data: {"id":"c1","model":"glm","choices":[{"index":0,"delta":{"content":"部分输出"},"finish_reason":null}]}`+"\n\n")
		fmt.Fprint(w, "data: "+frame+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}
}

func (f *fakeChat) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	uid := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer at-")
	f.mu.Lock()
	f.hits[uid]++
	n := f.hits[uid] - 1
	steps := f.scripts[uid]
	f.mu.Unlock()

	switch {
	case len(steps) == 0:
		if f.default_ != nil {
			f.default_(w)
			return
		}
		respOK(w)
	case n >= len(steps):
		steps[len(steps)-1](w)
	default:
		steps[n](w)
	}
}

// poolFixture 号池测试环境。
type poolFixture struct {
	h    *Handler
	m    *pool.Manager
	st   *store.Store
	cfg  *config.Manager
	up   *fakeChat
	srv  *httptest.Server
	uids []string
}

func newPoolFixture(t *testing.T, uids ...string) *poolFixture {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	enablePool(t, cfg)
	book := auth.NewAccountBook(dir)
	for i, uid := range uids {
		tok := &auth.Token{UID: uid, AccessToken: "at-" + uid, Domain: "www.codebuddy.cn",
			ExpiresAt: time.Now().Add(time.Hour).Unix()}
		if err := book.Save(tok); err != nil {
			t.Fatal(err)
		}
		if i == 0 { // 第一个也是「当前活动账号」（token.json）
			ts, err := auth.NewTokenStorePath(filepath.Join(dir, "token.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := ts.Save(tok); err != nil {
				t.Fatal(err)
			}
		}
	}
	toks, err := auth.NewTokenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	up := newFakeChat()
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)

	client := upstream.New(toks, func() string { return "cn" }, 60)
	client.ChatBaseOverride = srv.URL
	client.AuthBaseOverride = srv.URL

	m := pool.New(cfg, st, toks, client)
	t.Cleanup(func() { m.Close() })
	h := New(client, &upstream.ModelCache{}, apikey.New(st), st, cfg, m)
	return &poolFixture{h: h, m: m, st: st, cfg: cfg, up: up, srv: srv, uids: uids}
}

func enablePool(t *testing.T, cfg *config.Manager) {
	t.Helper()
	if err := cfg.Update(func(c *config.Config) error {
		c.PoolEnabled = true
		c.PoolSessionTTL = 180
		c.PoolMaxRetry = 2
		c.PoolCoolMaxMin = 360
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// chat 发一次非流式 chat 请求。
func (f *poolFixture) chat(t *testing.T, sessKey, model string) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, model)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	if sessKey != "" {
		req.Header.Set("session_id", sessKey)
	}
	rr := httptest.NewRecorder()
	f.h.Chat(rr, req)
	return rr
}

// pin 把会话预绑定到指定账号。
//
// 分配本身是 hash 决定的，测试要断言的往往是「某个号坏了会怎样」，
// 不钉住账号就不知道第一次会落到谁身上（两个号时 50% 概率白跑）。
func (f *poolFixture) pin(headerValue, model, uid string) {
	// 与 pool.SessionKey 的返回值对齐：请求头来源的键带 "h:" 前缀。
	// 直接用裸串会绑到一条永不命中的键上（这个坑值得留在注释里）。
	f.m.Bind("h:"+headerValue, model, uid)
}

// lastLog 取最后一条日志（异步 worker 写入，需等）。
func (f *poolFixture) lastLog(t *testing.T) store.LogEntry {
	t.Helper()
	for i := 0; i < 200; i++ {
		logs, _, err := f.st.QueryLogs(store.LogFilter{Page: 1, PageSize: 1})
		if err != nil {
			t.Fatal(err)
		}
		if len(logs) > 0 {
			return logs[0]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("等不到日志落库")
	return store.LogEntry{}
}

// TestPoolDisabledZeroChange 开关关闭：走单账号路径，account_uid 为空。
func TestPoolDisabledZeroChange(t *testing.T) {
	f := newPoolFixture(t, "acc1", "acc2")
	if err := f.cfg.Update(func(c *config.Config) error { c.PoolEnabled = false; return nil }); err != nil {
		t.Fatal(err)
	}
	rr := f.chat(t, "s1", "glm")
	if rr.Code != http.StatusOK {
		t.Fatalf("应成功，得到 %d: %s", rr.Code, rr.Body.String())
	}
	if got := f.lastLog(t).AccountUID; got != "" {
		t.Errorf("号池关闭时 account_uid 应为空，得到 %q", got)
	}
	if f.up.hits["acc1"] != 1 || f.up.hits["acc2"] != 0 {
		t.Errorf("应只请求当前活动账号 acc1，实际 %v", f.up.hits)
	}
}

// TestPoolStickySameSession 同一 session key 两次请求走同一账号。
func TestPoolStickySameSession(t *testing.T) {
	f := newPoolFixture(t, "acc1", "acc2", "acc3", "acc4")
	if rr := f.chat(t, "sess-A", "glm"); rr.Code != http.StatusOK {
		t.Fatalf("首次请求失败 %d", rr.Code)
	}
	uid1 := f.lastLog(t).AccountUID
	if uid1 == "" {
		t.Fatal("号池开启时 account_uid 必须有值")
	}
	if rr := f.chat(t, "sess-A", "glm"); rr.Code != http.StatusOK {
		t.Fatalf("第二次请求失败 %d", rr.Code)
	}
	if uid2 := f.lastLog(t).AccountUID; uid1 != uid2 {
		t.Errorf("同一会话应粘同一账号：%q → %q", uid1, uid2)
	}
	if hit, _ := f.m.StickyStats(); hit < 1 {
		t.Errorf("第二次应命中粘性，hit=%d", hit)
	}
}

// TestPoolDifferentSessionsSpread 4 个不同会话应各占一个账号。
func TestPoolDifferentSessionsSpread(t *testing.T) {
	f := newPoolFixture(t, "acc1", "acc2", "acc3", "acc4")
	seen := map[string]bool{}
	for _, sess := range []string{"A", "B", "C", "D"} {
		if rr := f.chat(t, sess, "glm"); rr.Code != http.StatusOK {
			t.Fatalf("%s 请求失败 %d", sess, rr.Code)
		}
		uid := f.lastLog(t).AccountUID
		if seen[uid] {
			t.Errorf("%s 与其他会话撞号 %q（idle-first 应各占一个）", sess, uid)
		}
		seen[uid] = true
	}
	if len(seen) != 4 {
		t.Errorf("应占用 4 个不同账号，实际 %d：%v", len(seen), seen)
	}
}

// TestPoolRetryOn6004 acc1 回 6004 → 换号成功；acc1 只有 glm 被冷却。
func TestPoolRetryOn6004(t *testing.T) {
	f := newPoolFixture(t, "acc1", "acc2")
	f.up.plan("acc1", respErr(http.StatusTooManyRequests, errBody6004))
	f.pin("sess", "glm", "acc1")

	rr := f.chat(t, "sess", "glm")
	if rr.Code != http.StatusOK {
		t.Fatalf("应换号成功，得到 %d: %s", rr.Code, rr.Body.String())
	}
	if uid := f.lastLog(t).AccountUID; uid == "acc1" {
		t.Error("应在 acc2 上成功")
	}
	// acc1 的 glm 冷却，其它模型仍可用
	if avail := f.m.Available("glm"); len(avail) != 1 || avail[0] != "acc2" {
		t.Errorf("glm 上应只剩 acc2 可用，实际 %v", avail)
	}
	if avail := f.m.Available("deepseek"); len(avail) != 2 {
		t.Errorf("其它模型上 acc1 应仍可用，实际 %v", avail)
	}
	if st, ok := f.m.AccountStateOf("acc1"); !ok || !st.Available() {
		t.Error("6004 不应禁用整个账号")
	}
}

// TestPoolRetryStopsAtMaxRetry 全部账号都回 6004 → 最多尝试 1+maxRetry 次后透传。
func TestPoolRetryStopsAtMaxRetry(t *testing.T) {
	f := newPoolFixture(t, "acc1", "acc2", "acc3", "acc4")
	for _, uid := range f.uids {
		f.up.plan(uid, respErr(http.StatusTooManyRequests, errBody6004))
	}
	rr := f.chat(t, "sess", "glm")
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("应透传 429（不能改成 503），得到 %d", rr.Code)
	}
	total := 0
	for _, n := range f.up.hits {
		total += n
	}
	// maxRetry=2 → 最多 3 次尝试，且每次换不同号
	if total > 3 {
		t.Errorf("网关内重试上限 2 → 最多 3 次上游调用，实际 %d", total)
	}
	// 每个尝试过的号都应被标记（D2）
	cooled := 0
	for _, uid := range f.uids {
		if st, ok := f.m.AccountStateOf(uid); ok && st.ErrorCount > 0 {
			cooled++
		}
	}
	if cooled != total {
		t.Errorf("%d 次尝试应对应 %d 个被标记的账号，实际 %d", total, total, cooled)
	}
}

// TestPoolQuotaDisablesAfterTwo acc1 连续 14018 → 禁用；请求仍能被 acc2 接住。
func TestPoolQuotaDisablesAfterTwo(t *testing.T) {
	f := newPoolFixture(t, "acc1", "acc2")
	f.up.plan("acc1", respErr(http.StatusTooManyRequests, errBody14018))
	f.pin("s1", "glm", "acc1")
	f.pin("s2", "glm", "acc1")

	// 第 1 次：acc1 额度耗尽（连击 1）→ 换号到 acc2 成功
	if rr := f.chat(t, "s1", "glm"); rr.Code != http.StatusOK {
		t.Fatalf("应换号成功，得到 %d: %s", rr.Code, rr.Body.String())
	}
	st, ok := f.m.AccountStateOf("acc1")
	if !ok {
		t.Fatal("应有 acc1 状态")
	}
	if !st.Available() {
		t.Error("第 1 次 14018 不应禁用")
	}
	// 第 2 次：再撞一次 → 禁用
	if rr := f.chat(t, "s2", "glm"); rr.Code != http.StatusOK {
		t.Fatalf("应仍能成功，得到 %d", rr.Code)
	}
	st, _ = f.m.AccountStateOf("acc1")
	if st.Available() {
		t.Error("连续 2 次 14018 应禁用账号")
	}
	if st.AutoDisabled != pool.AutoQuota {
		t.Errorf("auto_disabled = %q，期望 %q", st.AutoDisabled, pool.AutoQuota)
	}
}

func respOpenAISuccess() func(http.ResponseWriter) { return respOK }

// TestPoolNoPenaltyOn5xx 5xx 不重试、不惩罚账号（库里 73 条 503 全是网络抖动）。
func TestPoolNoPenaltyOn5xx(t *testing.T) {
	f := newPoolFixture(t, "acc1", "acc2")
	for _, uid := range f.uids {
		f.up.plan(uid, respErr(http.StatusServiceUnavailable, `{"msg":"upstream down"}`))
	}
	rr := f.chat(t, "sess", "glm")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("应透传 503，得到 %d", rr.Code)
	}
	total := 0
	for _, n := range f.up.hits {
		total += n
	}
	if total != 1 {
		t.Errorf("5xx 不应重试，实际 %d 次上游调用", total)
	}
	for _, uid := range f.uids {
		st, _ := f.m.AccountStateOf(uid)
		if !st.Available() {
			t.Errorf("%s 不应因 5xx 被禁用", uid)
		}
	}
	if avail := f.m.Available("glm"); len(avail) != 2 {
		t.Errorf("5xx 不应冷却模型，可用 %v", avail)
	}
}

// TestPoolSSEErrorFrameCoolsButNoRetry 流内错误帧：不重试（字节已发出），但记冷却。
func TestPoolSSEErrorFrameCoolsButNoRetry(t *testing.T) {
	f := newPoolFixture(t, "acc1", "acc2")
	f.up.plan("acc1", respSSEWithErrorFrame(`{"error":{"message":"您的使用量已超出频率限制","code":"6004"}}`))
	f.pin("sess", "glm", "acc1")

	// 必须是流式：非流式走 aggregate，错误帧被吃掉不输出（这是既有行为）；
	// 「原样透传」只对流式有意义。
	rr := f.chatStream(t, "sess", "glm")
	if rr.Code != http.StatusOK {
		t.Fatalf("开流后应保持 200，得到 %d", rr.Code)
	}
	// 流内错误帧被原样透传
	if !strings.Contains(rr.Body.String(), "6004") {
		t.Error("错误帧应原样透传")
	}
	// 但 acc1 的 glm 应被冷却
	if avail := f.m.Available("glm"); len(avail) != 1 || avail[0] == "acc1" {
		t.Errorf("流内错误帧应记冷却，glm 可用 %v", avail)
	}
	// 且没有重试（只该有 1 次上游调用）
	if f.up.hits["acc1"] != 1 {
		t.Errorf("流内报错不应重试，实际 %d 次", f.up.hits["acc1"])
	}
}

// TestPoolExhausted503 全池禁用 → 网关自造 503 + pool_exhausted。
func TestPoolExhausted503(t *testing.T) {
	f := newPoolFixture(t, "acc1", "acc2")
	for _, uid := range f.uids {
		if err := f.m.SetManualDisabled(uid, true); err != nil {
			t.Fatal(err)
		}
	}
	rr := f.chat(t, "sess", "glm")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("全池不可用应回 503，得到 %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "pool_exhausted") {
		t.Errorf("应带 pool_exhausted 标识：%s", rr.Body.String())
	}
	total := 0
	for _, n := range f.up.hits {
		total += n
	}
	if total != 0 {
		t.Errorf("无可用号时不应打上游，实际 %d 次", total)
	}
}

// TestPoolCredentialErrorPassesThrough401 401 一次即禁用该号并换号。
func TestPoolCredentialErrorPassesThrough401(t *testing.T) {
	f := newPoolFixture(t, "acc1", "acc2")
	f.up.plan("acc1", respErr(http.StatusUnauthorized, `{"code":401,"msg":"unauthorized"}`))
	f.pin("sess", "glm", "acc1")

	rr := f.chat(t, "sess", "glm")
	if rr.Code != http.StatusOK {
		t.Fatalf("应换号成功，得到 %d: %s", rr.Code, rr.Body.String())
	}
	for _, uid := range f.uids {
		st, _ := f.m.AccountStateOf(uid)
		if uid == "acc1" && st.AutoDisabled != pool.AutoCredential {
			t.Errorf("acc1 应被标为凭证失效，实际 %q", st.AutoDisabled)
		}
		if uid == "acc2" && !st.Available() {
			t.Error("acc2 不应受影响")
		}
	}
}

// TestPoolErrorMessageUnpacksNested14018 14018 的嵌套包络要能解出可读文案。
func TestPoolErrorMessageUnpacksNested14018(t *testing.T) {
	msg := upstreamErrorMessage([]byte(errBody14018), 429)
	if !strings.Contains(msg, "14018") {
		t.Errorf("应解出 code=14018：%s", msg)
	}
	if !strings.Contains(msg, "额度已用尽") {
		t.Errorf("应解出 msg 正文：%s", msg)
	}
	if strings.Contains(msg, "error\":{\"data") {
		t.Errorf("不应回落到原始 JSON：%s", msg)
	}
}

// chatStream 发一次流式 chat 请求。
func (f *poolFixture) chatStream(t *testing.T, sessKey, model string) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{"model":%q,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, model)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	if sessKey != "" {
		req.Header.Set("session_id", sessKey)
	}
	rr := httptest.NewRecorder()
	f.h.Chat(rr, req)
	return rr
}

// TestPoolSmallPoolPreservesUpstreamError 账号数 ≤ 1+maxRetry 时必须透传真实上游错误。
//
// 回归：早期「候选耗尽」直接回网关自造 503 + pool_exhausted，把真实的 429 吞掉了。
// 1–3 个号是常见配置，且「全号限流」正是最典型的故障场景——客户端（pi）靠状态码
// 决定要不要重试，改码会改变它的行为，用户也看不到上游原文（含 requestId）。
func TestPoolSmallPoolPreservesUpstreamError(t *testing.T) {
	for _, n := range []int{1, 2} {
		uids := make([]string, n)
		for i := range uids {
			uids[i] = fmt.Sprintf("acc%d", i+1)
		}
		f := newPoolFixture(t, uids...)
		for _, uid := range f.uids {
			f.up.plan(uid, respErr(http.StatusTooManyRequests, errBody6004))
		}
		rr := f.chat(t, "sess", "glm")
		if rr.Code != http.StatusTooManyRequests {
			t.Errorf("%d 个账号全 429：应透传 429，实际 %d（body=%s）", n, rr.Code, rr.Body.String())
		}
		if strings.Contains(rr.Body.String(), "pool_exhausted") {
			t.Errorf("%d 个账号：不应改写成 pool_exhausted", n)
		}
		if !strings.Contains(rr.Body.String(), "6004") {
			t.Errorf("%d 个账号：应保留上游 code，实际 %s", n, rr.Body.String())
		}
	}
}

// TestPoolNullErrorFrameDoesNotSwallowReal 正常帧里的 "error":null 不能吞掉后续真错误帧的冷却。
// 很多 OpenAI 兼容上游在正常 chunk 里写 "error":null。
func TestPoolNullErrorFrameDoesNotSwallowReal(t *testing.T) {
	f := newPoolFixture(t, "acc1", "acc2")
	stream := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// 先发一个带 error:null 的正常帧
		fmt.Fprint(w, `data: {"id":"c","model":"glm","error":null,"choices":[{"index":0,"delta":{"content":"a"},"finish_reason":null}]}`+"\n\n")
		// 再发真正的错误帧
		fmt.Fprint(w, `data: {"error":{"message":"rate","code":"6004"}}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}
	f.up.plan("acc1", stream)
	f.pin("sess", "glm", "acc1")

	f.chatStream(t, "sess", "glm")
	if avail := f.m.Available("glm"); len(avail) != 1 || avail[0] == "acc1" {
		t.Errorf("真错误帧应记冷却（acc1 应从 glm 可用集合里消失），实际 %v", avail)
	}
}

// TestPoolCooldownUsesRouteModel 冷却必须记在**请求声明的 model** 上，不是上游回报的别名。
//
// 回归：流式路径会把 logEntry.Model 覆盖成上游 chunk 里的 model（可能是 "glm-5.3"
// 这种别名）。若冷却记在别名上，而选号查询用请求 model，冷却对路由完全不可见——
// 被限流的号会被反复分配。
func TestPoolCooldownUsesRouteModel(t *testing.T) {
	f := newPoolFixture(t, "acc1", "acc2")
	stream := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// 上游把 model 回报成别名 glm-5.3
		fmt.Fprint(w, `data: {"id":"c","model":"glm-5.3","choices":[{"index":0,"delta":{"content":"a"},"finish_reason":null}]}`+"\n\n")
		fmt.Fprint(w, `data: {"error":{"message":"rate","code":"6004"}}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}
	f.up.plan("acc1", stream)
	f.pin("sess", "glm", "acc1")

	f.chatStream(t, "sess", "glm")
	// 用**请求 model**（glm）查询必须能看到冷却
	if avail := f.m.Available("glm"); len(avail) != 1 || avail[0] == "acc1" {
		t.Errorf("冷却应记在请求 model 上（glm），实际可用集合 %v", avail)
	}
}
