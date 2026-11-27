package proxy

import (
	"encoding/json"
	"fmt"
	"io"
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
type capturedReq struct {
	UID            string
	ConversationID string
	PromptCacheKey string
}

type fakeChat struct {
	mu       sync.Mutex
	hits     map[string]int
	reqs     []capturedReq
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
	rawBody, _ := io.ReadAll(r.Body)
	var bodyObj map[string]any
	_ = json.Unmarshal(rawBody, &bodyObj)
	cacheKey, _ := bodyObj["prompt_cache_key"].(string)

	f.mu.Lock()
	f.hits[uid]++
	n := f.hits[uid] - 1
	f.reqs = append(f.reqs, capturedReq{
		UID:            uid,
		ConversationID: r.Header.Get("X-Conversation-ID"),
		PromptCacheKey: cacheKey,
	})
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

// lastLog 取**本次请求**写的那条日志（异步 worker 写入，需等）。
//
// afterID 是本请求之前库里最大的日志 id，取自发起请求前：日志 id 自增，
// 只有 id > afterID 的才是本次请求产生的。**不能只等「库里有任意一条日志」**——
// worker 是异步的，上一次请求的行往往还在库里，于是上一次的 account_uid 会被
// 当成本次的结果返回，让「不同会话各占一个号」这类断言随机失败（CI 上必现）。
func (f *poolFixture) lastLog(t *testing.T, afterID int64) store.LogEntry {
	t.Helper()
	for i := 0; i < 200; i++ {
		logs, _, err := f.st.QueryLogs(store.LogFilter{Page: 1, PageSize: 1})
		if err != nil {
			t.Fatal(err)
		}
		if len(logs) > 0 && logs[0].ID > afterID {
			return logs[0]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("等不到本次请求的日志落库")
	return store.LogEntry{}
}

// maxLogID 当前库里最大的日志 id（无日志时为 0）。发起请求前取一次，
// 作为 lastLog 的「本次请求」分界线。
func (f *poolFixture) maxLogID(t *testing.T) int64 {
	t.Helper()
	logs, _, err := f.st.QueryLogs(store.LogFilter{Page: 1, PageSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) == 0 {
		return 0
	}
	return logs[0].ID
}

// TestPoolNilUsesSingleAccount h.pool 为 nil 时走单账号路径，account_uid 为空；
// 当 h.pool 非 nil 时即使配置中 PoolEnabled=false 也常驻启用多账号调度。
func TestPoolNilUsesSingleAccount(t *testing.T) {
	f := newPoolFixture(t, "acc1", "acc2")
	// 1. h.pool 非 nil 且 PoolEnabled=false：仍走号池（原生常驻）
	if err := f.cfg.Update(func(c *config.Config) error { c.PoolEnabled = false; return nil }); err != nil {
		t.Fatal(err)
	}
	f.pin("s-on", "glm", "acc2")
	before := f.maxLogID(t)
	if rr := f.chat(t, "s-on", "glm"); rr.Code != http.StatusOK {
		t.Fatalf("应成功，得到 %d: %s", rr.Code, rr.Body.String())
	}
	if got := f.lastLog(t, before).AccountUID; got != "acc2" {
		t.Errorf("原生常驻号池时应按粘性命中 acc2，得到 %q", got)
	}

	// 2. h.pool == nil（隔离单测模式）：退回单账号路径，account_uid 为空
	f.h.pool = nil
	before = f.maxLogID(t)
	rr := f.chat(t, "s1", "glm")
	if rr.Code != http.StatusOK {
		t.Fatalf("应成功，得到 %d: %s", rr.Code, rr.Body.String())
	}
	if got := f.lastLog(t, before).AccountUID; got != "" {
		t.Errorf("pool 为 nil 时 account_uid 应为空，得到 %q", got)
	}
	if f.up.hits["acc1"] != 1 || f.up.hits["acc2"] != 1 {
		t.Errorf("pool 为 nil 时应只再请求当前活动账号 acc1，实际 %v", f.up.hits)
	}
}

// TestPoolStickySameSession 同一 session key 两次请求走同一账号。
func TestPoolStickySameSession(t *testing.T) {
	f := newPoolFixture(t, "acc1", "acc2", "acc3", "acc4")
	before := f.maxLogID(t)
	if rr := f.chat(t, "sess-A", "glm"); rr.Code != http.StatusOK {
		t.Fatalf("首次请求失败 %d", rr.Code)
	}
	uid1 := f.lastLog(t, before).AccountUID
	if uid1 == "" {
		t.Fatal("号池开启时 account_uid 必须有值")
	}
	before = f.maxLogID(t)
	if rr := f.chat(t, "sess-A", "glm"); rr.Code != http.StatusOK {
		t.Fatalf("第二次请求失败 %d", rr.Code)
	}
	if uid2 := f.lastLog(t, before).AccountUID; uid1 != uid2 {
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
		before := f.maxLogID(t)
		if rr := f.chat(t, sess, "glm"); rr.Code != http.StatusOK {
			t.Fatalf("%s 请求失败 %d", sess, rr.Code)
		}
		uid := f.lastLog(t, before).AccountUID
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

	before := f.maxLogID(t)
	rr := f.chat(t, "sess", "glm")
	if rr.Code != http.StatusOK {
		t.Fatalf("应换号成功，得到 %d: %s", rr.Code, rr.Body.String())
	}
	if uid := f.lastLog(t, before).AccountUID; uid == "acc1" {
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

// TestPoolQuotaDisablesAfterTwo acc1 首次 14018 进入 60s 短冷却并摘掉该模型绑定，
// 同一模型下毫秒请求直接落到 acc2；再次在另一模型撞 14018 达 streak=2 后整号禁用。
func TestPoolQuotaDisablesAfterTwo(t *testing.T) {
	f := newPoolFixture(t, "acc1", "acc2")
	f.up.plan("acc1", respErr(http.StatusTooManyRequests, errBody14018))
	f.pin("s1", "glm", "acc1")
	f.pin("s2", "glm", "acc1")
	f.pin("s3", "deepseek", "acc1")

	// 第 1 次：acc1 glm 额度耗尽（连击 1）→ 换号到 acc2 成功，并对 (acc1, glm) 加 60s 短冷却
	if rr := f.chat(t, "s1", "glm"); rr.Code != http.StatusOK {
		t.Fatalf("应换号成功，得到 %d: %s", rr.Code, rr.Body.String())
	}
	st, ok := f.m.AccountStateOf("acc1")
	if !ok || !st.Available() || st.QuotaFailStreak != 1 {
		t.Fatalf("第 1 次 14018 不应整号禁用（avail=%v streak=%d）", st.Available(), st.QuotaFailStreak)
	}
	// 同一模型下毫秒内再次请求：应避开处于 60s 短冷却的 acc1，不增加 acc1 命中次数
	hitsBefore := f.up.hits["acc1"]
	if rr := f.chat(t, "s2", "glm"); rr.Code != http.StatusOK {
		t.Fatalf("应直接命中 acc2 成功，得到 %d", rr.Code)
	}
	if f.up.hits["acc1"] != hitsBefore {
		t.Errorf("首次 14018 后 (acc1, glm) 应进入 60s 短冷却，不应再被撞到（hits %d -> %d）", hitsBefore, f.up.hits["acc1"])
	}

	// 第 2 次撞 14018（另一模型 deepseek）→ 达到 streak=2 整号禁用
	if rr := f.chat(t, "s3", "deepseek"); rr.Code != http.StatusOK {
		t.Fatalf("应仍能换号成功，得到 %d", rr.Code)
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

// TestPoolSSEInline14018ReachesStreakTwoAndDisables 验证 HTTP 200 流内 14018 不会在开流时被
// NoteSuccess 清零连击计数，连续两次流内 14018 后整号禁用，且首次会解绑并加 60s 短冷却。
func TestPoolSSEInline14018ReachesStreakTwoAndDisables(t *testing.T) {
	f := newPoolFixture(t, "acc1", "acc2")
	f.up.plan("acc1", respSSEWithErrorFrame(errBody14018))
	f.pin("s1", "glm", "acc1")
	f.pin("s2", "deepseek", "acc1")

	if rr := f.chatStream(t, "s1", "glm"); rr.Code != http.StatusOK {
		t.Fatalf("开流应 200，得到 %d", rr.Code)
	}
	st, _ := f.m.AccountStateOf("acc1")
	if st.QuotaFailStreak != 1 || st.AutoDisabled != "" {
		t.Fatalf("首次流内 14018 后 streak 应保持 1（不被 NoteSuccess 清零），实际 %+v", st)
	}
	if avail := f.m.Available("glm"); len(avail) != 1 || avail[0] != "acc2" {
		t.Errorf("首次流内 14018 后 glm 应短冷却避开 acc1，实际 %v", avail)
	}

	if rr := f.chatStream(t, "s2", "deepseek"); rr.Code != http.StatusOK {
		t.Fatalf("第二次开流应 200，得到 %d", rr.Code)
	}
	st, _ = f.m.AccountStateOf("acc1")
	if st.QuotaFailStreak != 2 || st.AutoDisabled != pool.AutoQuota {
		t.Errorf("连续两次流内 14018 应禁用账号为 quota，实际 %+v", st)
	}

	// 非流式 aggregate 遇流内 14018 同样不调用 NoteSuccess/Bind
	f2 := newPoolFixture(t, "acc1", "acc2")
	f2.up.plan("acc1", respSSEWithErrorFrame(errBody14018))
	f2.pin("s-agg", "glm", "acc1")
	if rr := f2.chat(t, "s-agg", "glm"); rr.Code != http.StatusOK {
		t.Fatalf("aggregate 返回 %d", rr.Code)
	}
	stAgg, _ := f2.m.AccountStateOf("acc1")
	if stAgg.QuotaFailStreak != 1 {
		t.Errorf("非流式流内 14018 后 streak 应为 1，实际 %d", stAgg.QuotaFailStreak)
	}
}

// TestPoolInflightTrackedAndReleased 验证在途请求计数在流式请求期间准确置 1、结束后准确归 0。
func TestPoolInflightTrackedAndReleased(t *testing.T) {
	f := newPoolFixture(t, "acc1", "acc2")
	f.pin("s-inf", "glm", "acc1")

	started := make(chan struct{})
	release := make(chan struct{})
	f.up.plan("acc1", func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `data: {"id":"c1","model":"glm","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`+"\n\n")
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		close(started)
		<-release
		fmt.Fprint(w, "data: [DONE]\n\n")
	})

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- f.chatStream(t, "s-inf", "glm")
	}()

	select {
	case <-started:
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("等待上游流开始超时")
	}
	if inf := f.m.Inflight("acc1"); inf != 1 {
		t.Errorf("请求进行中 acc1 inflight 应为 1，实际 %d", inf)
	}
	close(release)
	rr := <-done
	if rr.Code != http.StatusOK {
		t.Fatalf("流式请求失败: %d", rr.Code)
	}
	if inf := f.m.Inflight("acc1"); inf != 0 {
		t.Errorf("流结束后 acc1 inflight 应归 0，实际 %d", inf)
	}
}

// TestPoolSessionCacheKeyPropagation 验证 sessKey 传入 DoChatWithSession 后生成按账号隔离的
// prompt_cache_key 与稳定 X-Conversation-ID，换号重试时不跨号共享缓存键。
func TestPoolSessionCacheKeyPropagation(t *testing.T) {
	f := newPoolFixture(t, "acc1", "acc2")
	f.up.plan("acc1", respErr(http.StatusTooManyRequests, errBody6004))
	f.pin("sess-cache", "glm", "acc1")

	// 第 1 次请求：acc1 6004 → 重试 acc2 成功
	if rr := f.chat(t, "sess-cache", "glm"); rr.Code != http.StatusOK {
		t.Fatalf("应换号成功，得到 %d", rr.Code)
	}
	// 第 2 次请求：同会话应粘在 acc2 且使用完全相同的 prompt_cache_key / X-Conversation-ID
	if rr := f.chat(t, "sess-cache", "glm"); rr.Code != http.StatusOK {
		t.Fatalf("第二次请求失败: %d", rr.Code)
	}

	f.up.mu.Lock()
	reqs := append([]capturedReq(nil), f.up.reqs...)
	f.up.mu.Unlock()
	if len(reqs) != 3 {
		t.Fatalf("应有 3 次上游请求（acc1 失败 + acc2 成功 + acc2 复用），实际 %d: %+v", len(reqs), reqs)
	}
	rAcc1, rAcc2First, rAcc2Second := reqs[0], reqs[1], reqs[2]
	if rAcc1.PromptCacheKey == "" || rAcc1.ConversationID == "" {
		t.Fatalf("出站请求缺少 prompt_cache_key 或 X-Conversation-ID: %+v", rAcc1)
	}
	if rAcc1.PromptCacheKey == rAcc2First.PromptCacheKey || rAcc1.ConversationID == rAcc2First.ConversationID {
		t.Errorf("换号重试跨账号不得复用同一缓存键/会话头: acc1=%+v acc2=%+v", rAcc1, rAcc2First)
	}
	if rAcc2First.PromptCacheKey != rAcc2Second.PromptCacheKey || rAcc2First.ConversationID != rAcc2Second.ConversationID {
		t.Errorf("同账号同会话续轮必须复用完全一致的缓存键/会话头: %+v vs %+v", rAcc2First, rAcc2Second)
	}
}
