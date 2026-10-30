package pool

import (
	"path/filepath"
	"testing"
	"time"

	"buddy2api-go/internal/auth"
	"buddy2api-go/internal/config"
	"buddy2api-go/internal/store"
	"buddy2api-go/internal/upstream"
)

// newTestManager 构造一个带 N 个已存账号的号池。
//
// 每份档案写进 data/accounts/<uid>.json，并额外写一份 token.json，
// 与真实布局一致（池成员 = accounts/* ∪ token.json）。
func newTestManager(t *testing.T, uids ...string) (*Manager, *store.Store, *config.Manager) {
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
	if err := cfg.Update(func(c *config.Config) error {
		c.PoolEnabled = true
		c.PoolSessionTTL = 180
		c.PoolMaxRetry = 2
		c.PoolCoolMaxMin = 360
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	book := auth.NewAccountBook(dir)
	for i, uid := range uids {
		tok := &auth.Token{
			UID:         uid,
			AccessToken: "at-" + uid,
			Domain:      "www.codebuddy.cn",
			ExpiresAt:   time.Now().Add(time.Hour).Unix(),
			Nickname:    "账号" + uid,
		}
		if err := book.Save(tok); err != nil {
			t.Fatal(err)
		}
		// 第一个账号也放一份 token.json（模拟「当前活动账号」）
		if i == 0 {
			ts, err := auth.NewTokenStorePath(filepath.Join(dir, "token.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := ts.Save(tok); err != nil {
				t.Fatal(err)
			}
		}
	}
	m := New(cfg, st, mustTokenStore(t, dir), nil)
	t.Cleanup(func() { m.Close() })
	return m, st, cfg
}

func mustTokenStore(t *testing.T, dir string) *auth.TokenStore {
	t.Helper()
	ts, err := auth.NewTokenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

func body6004(resetAt string) []byte {
	return []byte(`{"code":6004,"msg":"您的使用量已超出频率限制，将在 ` + resetAt + ` UTC+8 重置，您也可以切换其他模型继续使用。"}`)
}

func body14018() []byte {
	return []byte(`{"error":{"data":{"code":14018,"msg":"额度已用尽，请访问以下链接，购买加量包以获取更多额度：https://www.codebuddy.cn/profile/usage ","requestId":"r"}}}`)
}

// TestMembersIncludesTokenJSON 池成员必须并上 token.json：
// 用户从没切过号时，当前账号只存在于 token.json，不在 accounts/ 目录里。
func TestMembersIncludesTokenJSON(t *testing.T) {
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
	// 只写 token.json，不写任何档案
	ts, err := auth.NewTokenStorePath(filepath.Join(dir, "token.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.Save(&auth.Token{UID: "only-active", AccessToken: "t", ExpiresAt: time.Now().Add(time.Hour).Unix()}); err != nil {
		t.Fatal(err)
	}
	m := New(cfg, st, ts, nil)
	t.Cleanup(func() { m.Close() })

	if got := m.Members(); len(got) != 1 || got[0] != "only-active" {
		t.Errorf("池成员 = %v，期望 [only-active]", got)
	}
}

// TestMembersDedupByUID 同一 uid 档案与 token.json 都在时去重成一条。
func TestMembersDedupByUID(t *testing.T) {
	m, _, _ := newTestManager(t, "acc1", "acc2")
	if got := m.Members(); len(got) != 2 {
		t.Errorf("成员 = %v，期望 2 条（acc1 去重）", got)
	}
}

// TestNoteError6004CoolsOnlyModel 6004 只冷却该模型，账号本身仍可用其它模型。
func TestNoteError6004CoolsOnlyModel(t *testing.T) {
	m, _, _ := newTestManager(t, "acc1", "acc2")

	m.NoteError("acc1", "glm-5.3", upstream.ErrKindModelRate, body6004("2030-01-01 10:00:00"))

	// glm 上 acc1 不可用
	if avail := m.Available("glm-5.3"); containsStr(avail, "acc1") {
		t.Errorf("acc1 应已在 glm-5.3 上冷却，可用集合 %v", avail)
	}
	// 其它模型上 acc1 仍可用
	if avail := m.Available("deepseek-v4.1-flash"); !containsStr(avail, "acc1") {
		t.Errorf("acc1 在其它模型上应仍可用，可用集合 %v", avail)
	}
	// 账号本身没被禁用
	st, _ := m.AccountStateOf("acc1")
	if !st.Available() {
		t.Error("6004 不应禁用整个账号")
	}
}

// TestNoteError6004ParseResetAt 冷却截止取上游文案的 UTC+8 墙钟（受上限截断）。
func TestNoteError6004ParseResetAt(t *testing.T) {
	m, _, _ := newTestManager(t, "acc1")
	before := time.Now()
	m.NoteError("acc1", "glm", upstream.ErrKindModelRate, body6004("2030-01-01 10:00:00"))

	m.mu.RLock()
	cool, ok := m.cools["acc1"]["glm"]
	m.mu.RUnlock()
	if !ok {
		t.Fatal("应写入冷却")
	}
	if cool.ResetAt == 0 {
		t.Error("reset_at 应记录上游权威墙钟")
	}
	// 上游给了 2030 年，远超默认 6h 上限，until 应被截断到 now+6h
	maxUntil := before.Add(6*time.Hour + time.Minute).Unix()
	if cool.Until > maxUntil {
		t.Errorf("until 应被截断到 ~6h，实际 %s（上限 %s）",
			time.Unix(cool.Until, 0), time.Unix(maxUntil, 0))
	}
	if cool.Code != "6004" {
		t.Errorf("code = %q，期望 6004", cool.Code)
	}
	if cool.Reason == "" {
		t.Error("reason 应保留上游原文")
	}
}

// TestNoteError6004NoResetTime 上游没给重置时间 → 有界退避，不臆造时间。
func TestNoteError6004NoResetTime(t *testing.T) {
	m, _, _ := newTestManager(t, "acc1")
	before := time.Now()
	m.NoteError("acc1", "glm", upstream.ErrKindModelRate, []byte(`{"code":6004,"msg":"已超出频率限制"}`))

	m.mu.RLock()
	cool := m.cools["acc1"]["glm"]
	m.mu.RUnlock()
	if cool.ResetAt != 0 {
		t.Errorf("无文案时 reset_at 应为 0，实际 %d", cool.ResetAt)
	}
	expect := before.Add(defaultModelCool).Unix()
	if cool.Until < expect-2 || cool.Until > expect+2 {
		t.Errorf("until 应约为 now+%v，实际 %s", defaultModelCool, time.Unix(cool.Until, 0))
	}
}

// TestNoteError14018NeedsTwoStreak 额度耗尽需连续 2 次才禁用（防上游边界误报）。
func TestNoteError14018NeedsTwoStreak(t *testing.T) {
	m, _, _ := newTestManager(t, "acc1", "acc2")

	m.NoteError("acc1", "glm", upstream.ErrKindQuotaExhausted, body14018())
	st, _ := m.AccountStateOf("acc1")
	if !st.Available() {
		t.Fatal("第 1 次不应禁用")
	}
	if st.QuotaFailStreak != 1 {
		t.Errorf("连击计数 = %d，期望 1", st.QuotaFailStreak)
	}

	m.NoteError("acc1", "glm", upstream.ErrKindQuotaExhausted, body14018())
	st, _ = m.AccountStateOf("acc1")
	if st.Available() {
		t.Fatal("连续 2 次应禁用账号")
	}
	if st.AutoDisabled != AutoQuota {
		t.Errorf("auto_disabled = %q，期望 %q", st.AutoDisabled, AutoQuota)
	}
}

// TestNoteError401ImmediatelyDisables 401 一次即禁用（凭证失效没有「偶发」这一说）。
func TestNoteError401ImmediatelyDisables(t *testing.T) {
	m, _, _ := newTestManager(t, "acc1")
	m.NoteError("acc1", "glm", upstream.ErrKindCredential, nil)
	st, _ := m.AccountStateOf("acc1")
	if st.Available() {
		t.Fatal("401 应立即禁用")
	}
	if st.AutoDisabled != AutoCredential {
		t.Errorf("auto_disabled = %q，期望 %q", st.AutoDisabled, AutoCredential)
	}
}

// TestNoteErrorDoesNotPunishNonAccountErrors 5xx / 网络 / 渠道 / 参数都不惩罚。
// 库里 73 条 503 全是 DNS 与 http2 超时，按失败禁用会让一次断网把全池打成禁用。
func TestNoteErrorDoesNotPunishNonAccountErrors(t *testing.T) {
	kinds := []upstream.ChatErrKind{
		upstream.ErrKindServer, upstream.ErrKindTransport, upstream.ErrKindChannel,
		upstream.ErrKindModelBlocked, upstream.ErrKindBadParams, upstream.ErrKindPassThrough,
	}
	for _, kind := range kinds {
		m, _, _ := newTestManager(t, "acc1")
		for i := 0; i < 5; i++ {
			m.NoteError("acc1", "glm", kind, []byte(`{}`))
		}
		st, _ := m.AccountStateOf("acc1")
		if !st.Available() {
			t.Errorf("%v 不应禁用账号", kind)
		}
		if len(m.Available("glm")) != 1 {
			t.Errorf("%v 不应冷却模型", kind)
		}
		if st.ErrorCount != 5 {
			t.Errorf("%v 应记观测计数，实际 %d", kind, st.ErrorCount)
		}
	}
}

// TestNoteErrorHitsIdempotent 同一 (uid,model) 已有冷却时不重复累加 hits。
// 一次真实限流会被上报多次（网关重试 2 次 × pi 重试 3 次），累加会让统计失真。
func TestNoteErrorHitsIdempotent(t *testing.T) {
	m, _, _ := newTestManager(t, "acc1")
	for i := 0; i < 5; i++ {
		m.NoteError("acc1", "glm", upstream.ErrKindModelRate, body6004("2030-01-01 10:00:00"))
	}
	m.mu.RLock()
	hits := m.cools["acc1"]["glm"].Hits
	m.mu.RUnlock()
	if hits != 1 {
		t.Errorf("重复上报同一限流时 hits 应保持 1，实际 %d", hits)
	}
}

// TestNoteSuccessClearsOnlyCurrentModel 成功只清当前模型的冷却。
func TestNoteSuccessClearsOnlyCurrentModel(t *testing.T) {
	m, _, _ := newTestManager(t, "acc1", "acc2")
	m.NoteError("acc1", "glm", upstream.ErrKindModelRate, body6004("2030-01-01 10:00:00"))
	m.NoteError("acc1", "deepseek", upstream.ErrKindModelRate, body6004("2030-01-01 10:00:00"))

	m.NoteSuccess("acc1", "glm")

	if avail := m.Available("glm"); !containsStr(avail, "acc1") {
		t.Error("glm 冷却应被解除")
	}
	if avail := m.Available("deepseek"); containsStr(avail, "acc1") {
		t.Error("deepseek 冷却不应被 glm 的成功解除")
	}
}

// TestNoteSuccessResetsStreak 成功会把额度连击计数归零（证明额度还在）。
func TestNoteSuccessResetsStreak(t *testing.T) {
	m, _, _ := newTestManager(t, "acc1")
	m.NoteError("acc1", "glm", upstream.ErrKindQuotaExhausted, body14018())
	m.NoteSuccess("acc1", "glm")
	st, _ := m.AccountStateOf("acc1")
	if st.QuotaFailStreak != 0 {
		t.Errorf("成功后连击计数应归零，实际 %d", st.QuotaFailStreak)
	}
}

// TestManualDisabledSurvivesButEnableResetsAuto 手动域与自动域正交。
func TestManualDisabledSurvivesButEnableResetsAuto(t *testing.T) {
	m, _, _ := newTestManager(t, "acc1")

	// 手动禁用 → 不可用
	if err := m.SetManualDisabled("acc1", true); err != nil {
		t.Fatal(err)
	}
	if len(m.Available("glm")) != 0 {
		t.Error("手动禁用后不应可用")
	}
	// 自动禁用也记上（模拟探测确认无额度）
	m.mu.Lock()
	m.accounts["acc1"].AutoDisabled = AutoQuota
	m.mu.Unlock()

	// 手动启用：同时清掉自动域与连击计数（用户显式操作 = 比自动探测更权威）
	if err := m.SetManualDisabled("acc1", false); err != nil {
		t.Fatal(err)
	}
	st, _ := m.AccountStateOf("acc1")
	if st.ManualDisabled {
		t.Error("手动启用后 ManualDisabled 应为 false")
	}
	if st.AutoDisabled != "" {
		t.Errorf("手动启用应清空自动域，实际 %q", st.AutoDisabled)
	}
	if len(m.Available("glm")) != 1 {
		t.Error("启用后应恢复可用")
	}
}

// TestEagerDropSessionsOnAutoDisable 账号被自动禁用时立即摘掉其会话绑定。
// 不摘的话它会继续作为「已占用」占着 idle-first 名额。
func TestEagerDropSessionsOnAutoDisable(t *testing.T) {
	m, _, _ := newTestManager(t, "acc1", "acc2")
	m.Bind("s1", "glm", "acc1")
	if m.SessionCount() != 1 {
		t.Fatal("应有一条绑定")
	}
	m.NoteError("acc1", "glm", upstream.ErrKindCredential, nil)
	if m.SessionCount() != 0 {
		t.Errorf("账号被禁用后其会话绑定应被摘掉，实际剩 %d 条", m.SessionCount())
	}
}

// waitPersisted 等某 uid 的账号状态真的落到 DB（落库是异步队列，NoteError / 
// probe 都不保证返回时已写库）。用于「重启」类断言。
func waitPersisted(t *testing.T, st *store.Store, uid string) {
	t.Helper()
	for i := 0; i < 200; i++ {
		rows, err := st.LoadPoolAccounts()
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if r.UID == uid && (r.AutoDisabled != "" || r.ManualDisabled) {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s 的禁用状态等不到落库", uid)
}

// TestStatePersistsAcrossRestart 禁用状态必须落 SQLite（重启不能复活坏号）。
func TestStatePersistsAcrossRestart(t *testing.T) {
	m, st, cfg := newTestManager(t, "acc1", "acc2")
	if err := m.SetManualDisabled("acc2", true); err != nil {
		t.Fatal(err)
	}
	m.NoteError("acc1", "glm", upstream.ErrKindCredential, nil)

	// 落库是异步的（NoteError 只投递到队列），必须先等到写进 DB 再「重启」，
	// 否则 New() 读到的是空状态——断言失败而其实是测试自己抢跑了。
	waitPersisted(t, st, "acc1")

	// 模拟重启：同一 DB + 同一档案目录重新构造
	m2 := New(cfg, st, m.toks["acc1"], nil)
	t.Cleanup(func() { m2.Close() })

	if st2, ok := m2.AccountStateOf("acc2"); !ok || !st2.ManualDisabled {
		t.Errorf("手动禁用应持久化，得到 %+v", st2)
	}
	if st1, ok := m2.AccountStateOf("acc1"); !ok || st1.AutoDisabled != AutoCredential {
		t.Errorf("自动禁用应持久化，得到 %+v", st1)
	}
}

// TestDeleteAccountClearsState 删除账号要连池状态一起清，
// 否则重新导入同一个号会继承旧的「额度耗尽」标记而立即被跳过。
func TestDeleteAccountClearsState(t *testing.T) {
	m, st, _ := newTestManager(t, "acc1", "acc2")
	if err := m.SetManualDisabled("acc2", true); err != nil {
		t.Fatal(err)
	}
	m.DeleteAccount("acc2")
	if _, ok := m.AccountStateOf("acc2"); ok {
		t.Error("内存状态应被清理")
	}
	rows, err := st.LoadPoolAccounts()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.UID == "acc2" {
			t.Error("DB 状态应被清理")
		}
	}
}

// TestResolveSkipsTried 换号重试时不能重复选同一个号。
func TestResolveSkipsTried(t *testing.T) {
	m, _, _ := newTestManager(t, "acc1", "acc2")
	tried := map[string]bool{}
	first, ok := m.Resolve("s1", "glm", tried)
	if !ok {
		t.Fatal("应能选到号")
	}
	tried[first] = true
	second, ok := m.Resolve("s1", "glm", tried)
	if !ok {
		t.Fatal("应有第二个号可选")
	}
	if second == first {
		t.Errorf("不应重复选中已试过的 %s", first)
	}
	tried[second] = true
	if _, ok := m.Resolve("s1", "glm", tried); ok {
		t.Error("全部试过后应返回不可用")
	}
}

// TestResolveNoCandidates 全池禁用时返回 false（由 proxy 转成 pool_exhausted 503）。
func TestResolveNoCandidates(t *testing.T) {
	m, _, _ := newTestManager(t, "acc1")
	if err := m.SetManualDisabled("acc1", true); err != nil {
		t.Fatal(err)
	}
	if uid, ok := m.Resolve("s1", "glm", nil); ok {
		t.Errorf("全池不可用时应返回 false，得到 %q", uid)
	}
}

// TestClearModelCool 手动清除冷却后立刻可用。
func TestClearModelCool(t *testing.T) {
	m, _, _ := newTestManager(t, "acc1")
	m.NoteError("acc1", "glm", upstream.ErrKindModelRate, body6004("2030-01-01 10:00:00"))
	if len(m.Available("glm")) != 0 {
		t.Fatal("应先冷却")
	}
	if err := m.ClearModelCool("acc1", "glm"); err != nil {
		t.Fatal(err)
	}
	if len(m.Available("glm")) != 1 {
		t.Error("清除冷却后应恢复可用")
	}
}

// TestComputeCoolUntil 冷却截止的三种分支。
func TestComputeCoolUntil(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	max := 6 * time.Hour

	// 1. 上游给了未来时间，且在截断上限内 → 用它
	reset := now.Add(2 * time.Hour)
	if got := computeCoolUntil(now, reset, true, max); !got.Equal(reset) {
		t.Errorf("应直接用上游时间，得到 %v", got)
	}
	// 2. 上游给的时间超过上限 → 截断
	reset = now.Add(100 * time.Hour)
	if got := computeCoolUntil(now, reset, true, max); !got.Equal(now.Add(max)) {
		t.Errorf("应截断到 now+max，得到 %v", got)
	}
	// 3. 上游给的时间已过期 → now+minModelCool（防立刻解冻又立刻撞）
	reset = now.Add(-time.Hour)
	if got := computeCoolUntil(now, reset, true, max); !got.Equal(now.Add(minModelCool)) {
		t.Errorf("已过期时间应退回 now+minModelCool，得到 %v", got)
	}
	// 4. 上游没给时间 → 有界退避
	if got := computeCoolUntil(now, time.Time{}, false, max); !got.Equal(now.Add(defaultModelCool)) {
		t.Errorf("无时间应有界退避，得到 %v", got)
	}
	// 5. max<=0 → 用默认 6h
	reset = now.Add(100 * time.Hour)
	if got := computeCoolUntil(now, reset, true, 0); !got.Equal(now.Add(6 * time.Hour)) {
		t.Errorf("max=0 应回落 6h，得到 %v", got)
	}
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
