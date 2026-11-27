package pool

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"buddy2api-go/internal/billing"
	"buddy2api-go/internal/config"
)

func newTestRouter(ttl time.Duration) *Router {
	return NewRouter(0, func() time.Duration { return ttl })
}

// TestRouterIdleFirstSpread 4 个全空闲号 + 4 个不同会话 → 各占一个号。
// 这是「不同对话尽量分散」的核心契约。
func TestRouterIdleFirstSpread(t *testing.T) {
	r := newTestRouter(3 * time.Hour)
	avail := []string{"acc1", "acc2", "acc3", "acc4"}
	used := map[string]string{}
	for _, sess := range []string{"s1", "s2", "s3", "s4"} {
		uid, hit := r.Resolve(sess, "glm", avail)
		if hit {
			t.Errorf("%s 首次请求不应算粘性命中", sess)
		}
		if prev, ok := used[uid]; ok {
			t.Errorf("%s 与 %s 撞到同一个号 %s（idle-first 应各占一个）", sess, prev, uid)
		}
		used[uid] = sess
	}
	if len(used) != 4 {
		t.Fatalf("应占用 4 个不同账号，实际 %d", len(used))
	}
}

// TestRouterReuseSticky 同一会话重复请求 → 同一账号 + 命中粘性。
func TestRouterReuseSticky(t *testing.T) {
	r := newTestRouter(3 * time.Hour)
	avail := []string{"acc1", "acc2"}
	first, _ := r.Resolve("s1", "glm", avail)
	for i := 0; i < 5; i++ {
		uid, hit := r.Resolve("s1", "glm", avail)
		if !hit {
			t.Fatal("第二次起应命中粘性")
		}
		if uid != first {
			t.Fatalf("粘性失效：%s → %s", first, uid)
		}
	}
	if hit, miss := r.Stats(); hit != 5 || miss != 1 {
		t.Errorf("命中/未命中 = %d/%d，期望 5/1", hit, miss)
	}
}

// TestRouterModelIsolation 同一会话换模型可以走不同号（D4：键带 model）。
// 关键场景：glm 被冷却后，该会话在 deepseek 上的粘性不应被一起打散。
func TestRouterModelIsolation(t *testing.T) {
	r := newTestRouter(3 * time.Hour)
	avail := []string{"acc1", "acc2", "acc3"}
	glmUID, _ := r.Resolve("s1", "glm", avail)
	dsUID, _ := r.Resolve("s1", "deepseek", avail)
	// 两者可以相同也可以不同（hash 决定），但必须是两条独立绑定
	if g, ok := r.BoundUID("s1", "glm"); !ok || g != glmUID {
		t.Error("glm 绑定应独立存在")
	}
	if d, ok := r.BoundUID("s1", "deepseek"); !ok || d != dsUID {
		t.Error("deepseek 绑定应独立存在")
	}
	// glm 的绑定失效不影响 deepseek
	r.UnbindIf("s1", "glm", glmUID)
	if _, ok := r.BoundUID("s1", "glm"); ok {
		t.Error("glm 绑定应已解除")
	}
	if d, ok := r.BoundUID("s1", "deepseek"); !ok || d != dsUID {
		t.Error("deepseek 绑定不应受 glm 解绑影响")
	}
}

// TestRouterTTLExpiry TTL 到期后重新分配，且旧条目不再占 idle 名额。
func TestRouterTTLExpiry(t *testing.T) {
	r := newTestRouter(30 * time.Millisecond)
	avail := []string{"acc1", "acc2"}
	first, _ := r.Resolve("s1", "glm", avail)
	time.Sleep(50 * time.Millisecond)
	uid, hit := r.Resolve("s1", "glm", avail)
	if hit {
		t.Error("TTL 过期后不应算命中")
	}
	// 过期条目被摘除后，重新分配可能落回同一个号（hash 决定），
	// 但**必须**重新走一次分配（miss 计数 +1），且绑定仍然有效
	if _, ok := r.BoundUID("s1", "glm"); !ok {
		t.Error("重新分配后应有绑定")
	}
	_ = first
	_ = uid
	if _, miss := r.Stats(); miss != 2 {
		t.Errorf("miss 应为 2（初次 + 过期后重分配），实际 %d", miss)
	}
}

// TestRouterTTLSliding 每次命中都续期：持续访问的会话永不过期。
func TestRouterTTLSliding(t *testing.T) {
	r := newTestRouter(60 * time.Millisecond)
	avail := []string{"acc1"}
	first, _ := r.Resolve("s1", "glm", avail)
	for i := 0; i < 5; i++ {
		time.Sleep(20 * time.Millisecond)
		if uid, hit := r.Resolve("s1", "glm", avail); !hit || uid != first {
			t.Fatalf("第 %d 次滑动续期失败（hit=%v uid=%s）", i, hit, uid)
		}
	}
}

// TestRouterUnusableRebind 绑定的号变得不可用（被禁用/该模型冷却）时必须重新分配，
// 而不是继续返回它——否则会话会被钉在限流号上反复失败。
//
// 注意责任划分：可用性由**调用方**判定并体现在传入的 avail 里
// （Router 不回调调用方，见 Router.Resolve 的锁顺序注释）。
func TestRouterUnusableRebind(t *testing.T) {
	r := newTestRouter(3 * time.Hour)
	all := []string{"acc1", "acc2"}

	first, _ := r.Resolve("s1", "glm", all)
	// 模拟 first 变坏：调用方的 avail 里不再包含它
	var avail []string
	for _, u := range all {
		if u != first {
			avail = append(avail, u)
		}
	}
	uid, hit := r.Resolve("s1", "glm", avail)
	if hit {
		t.Error("绑定的号不可用时应重新分配，不算命中")
	}
	if uid == first {
		t.Errorf("不应重新选中不可用的号 %s", first)
	}
	if uid == "" {
		t.Error("应分配到另一个可用号")
	}
}

// TestRouterEmptyKeyNotPersisted 无会话键时不写表（否则每次都是一条永不复用的垃圾条目）。
func TestRouterEmptyKeyNotPersisted(t *testing.T) {
	r := newTestRouter(3 * time.Hour)
	for i := 0; i < 10; i++ {
		if uid, hit := r.Resolve("", "glm", []string{"acc1", "acc2"}); uid == "" || hit {
			t.Fatal("无会话键应返回一个号且不算命中")
		}
	}
	if n := r.Count(); n != 0 {
		t.Errorf("无会话键不应写绑定表，实际 %d 条", n)
	}
}

// TestRouterUnbindIfRespectsOwner 只解绑「失败的正是本会话绑定的号」。
// 否则会话 A 绑在 acc1，而 acc2 在别的会话撞了限流，会误删 A 的绑定。
func TestRouterUnbindIfRespectsOwner(t *testing.T) {
	r := newTestRouter(3 * time.Hour)
	r.Bind("s1", "glm", "acc1")
	r.UnbindIf("s1", "glm", "acc2") // 别的号失败
	if uid, ok := r.BoundUID("s1", "glm"); !ok || uid != "acc1" {
		t.Error("acc2 的失败不应解绑 s1 的 acc1 绑定")
	}
	r.UnbindIf("s1", "glm", "acc1")
	if _, ok := r.BoundUID("s1", "glm"); ok {
		t.Error("acc1 失败应解绑 s1")
	}
}

// TestRouterConcurrentSameKey 同一会话并发首请求只允许分配一个号（-race 检查数据竞争）。
func TestRouterConcurrentSameKey(t *testing.T) {
	r := newTestRouter(3 * time.Hour)
	avail := []string{"acc1", "acc2", "acc3", "acc4"}
	const n = 32
	var wg sync.WaitGroup
	results := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], _ = r.Resolve("same-session", "glm", avail)
		}(i)
	}
	wg.Wait()
	first := results[0]
	for i, uid := range results {
		if uid != first {
			t.Fatalf("并发分配不一致：第 %d 个得到 %s，期望 %s", i, uid, first)
		}
	}
	if n := r.Count(); n != 1 {
		t.Errorf("只应有 1 条绑定，实际 %d", n)
	}
}

// TestRouterEvictLRU 超出容量时淘汰最久未活跃的条目。
func TestRouterEvictLRU(t *testing.T) {
	r := NewRouter(10, func() time.Duration { return 3 * time.Hour })
	// 先建 10 条（达到容量），并让 s0 成为最旧的
	for i := 0; i < 10; i++ {
		r.Bind(fmt.Sprintf("s%d", i), "glm", "acc1")
		time.Sleep(time.Millisecond)
	}
	// 刷新 s1..s9，使 s0 明确最旧
	for i := 1; i < 10; i++ {
		time.Sleep(time.Millisecond)
		r.Bind(fmt.Sprintf("s%d", i), "glm", "acc1")
	}
	// 再加一条触发淘汰
	r.Bind("new", "glm", "acc1")
	if n := r.Count(); n > 11 {
		t.Errorf("容量 10 时不应无限增长，实际 %d", n)
	}
	if _, ok := r.BoundUID("s0", "glm"); ok {
		t.Error("最久未活跃的 s0 应被淘汰")
	}
	if _, ok := r.BoundUID("new", "glm"); !ok {
		t.Error("新条目不应被淘汰")
	}
}

// TestRouterDropUID 账号被禁用后摘掉全部绑定。
func TestRouterDropUID(t *testing.T) {
	r := newTestRouter(3 * time.Hour)
	r.Bind("s1", "glm", "acc1")
	r.Bind("s2", "glm", "acc1")
	r.Bind("s3", "glm", "acc2")
	if n := r.DropUID("acc1"); n != 2 {
		t.Errorf("应摘掉 2 条，实际 %d", n)
	}
	if _, ok := r.BoundUID("s3", "glm"); !ok {
		t.Error("acc2 的绑定不应受影响")
	}
}

// TestRouterDroppedUIDFreesIdleSlot 关键回归：被禁用的号不能继续占 idle-first 名额。
// 没有 DropUID 的话，4 个号里禁 1 个后，idle 集合永远少一个空位，第 4 个会话被迫共享。
func TestRouterDroppedUIDFreesIdleSlot(t *testing.T) {
	r := newTestRouter(3 * time.Hour)

	// acc1 被禁用前先占住 s0
	r.Bind("s0", "glm", "acc1")
	r.DropUID("acc1")

	used := map[string]bool{}
	for _, sess := range []string{"s1", "s2", "s3"} {
		uid, _ := r.Resolve(sess, "glm", []string{"acc2", "acc3", "acc4"})
		if used[uid] {
			t.Errorf("%s 撞号 %s：禁用号的残留绑定占住了 idle 名额", sess, uid)
		}
		used[uid] = true
	}
	if len(used) != 3 {
		t.Errorf("3 个会话应占 3 个不同号，实际 %d", len(used))
	}
}

func TestHashIndexStable(t *testing.T) {
	// 同样输入必须得到同样输出（否则无法复现分配）
	for i := 0; i < 5; i++ {
		if a, b := hashIndex("k", 7), hashIndex("k", 7); a != b {
			t.Fatal("hashIndex 不稳定")
		}
	}
	// n <= 1 时恒为 0
	for _, n := range []int{-1, 0, 1} {
		if got := hashIndex("anything", n); got != 0 {
			t.Errorf("hashIndex(_, %d) = %d，期望 0", n, got)
		}
	}
	// 结果必须在 [0, n) 内
	for n := 2; n <= 50; n++ {
		for i := 0; i < 100; i++ {
			if got := hashIndex(fmt.Sprint("key", i), n); got < 0 || got >= n {
				t.Fatalf("hashIndex 越界：%d / %d", got, n)
			}
		}
	}
}

// TestRouterHashSpread 打散性：4 个号上的落点应大致均匀（不要求精确）。
func TestRouterHashSpread(t *testing.T) {
	const total = 400
	counts := map[int]int{}
	for i := 0; i < total; i++ {
		counts[hashIndex(fmt.Sprint("session-", i), 4)]++
	}
	for slot := 0; slot < 4; slot++ {
		// 均匀期望 100，允许 50%~150%
		if counts[slot] < 50 || counts[slot] > 150 {
			t.Errorf("槽位 %d 分到 %d 个，分布明显不均", slot, counts[slot])
		}
	}
}

func TestRouterExpiredCleanupAndCounts(t *testing.T) {
	r := newTestRouter(30 * time.Millisecond)
	r.Bind("s1", "glm", "acc1")
	r.Bind("s2", "glm", "acc1")
	r.Bind("s3", "deepseek", "acc2")
	time.Sleep(50 * time.Millisecond)

	if n := r.Count(); n != 0 {
		t.Errorf("过期条目不应计入 Count()，实际 %d", n)
	}
	if byUID := r.CountByUID(); len(byUID) != 0 {
		t.Errorf("过期条目不应计入 CountByUID()，实际 %v", byUID)
	}
	if byModel := r.CountByUIDModel("glm"); len(byModel) != 0 {
		t.Errorf("过期条目不应计入 CountByUIDModel()，实际 %v", byModel)
	}
	if _, ok := r.BoundUID("s1", "glm"); ok {
		t.Error("BoundUID 不应返回已过期绑定")
	}

	// 慢路径分配扫描应主动删除僵尸条目
	r.Resolve("fresh-s", "glm", []string{"acc1", "acc2"})
	r.mu.RLock()
	rawLen := len(r.entries)
	r.mu.RUnlock()
	if rawLen != 1 {
		t.Errorf("慢路径后应只剩 1 条活跃条目（僵尸已清理），实际 map 长度 %d", rawLen)
	}
}

func TestRouterLeastBound(t *testing.T) {
	r := newTestRouter(3 * time.Hour)
	r.Bind("s1", "glm", "acc1")
	r.Bind("s2", "glm", "acc1")
	r.Bind("s3", "glm", "acc2")

	// acc3 在 glm 上 0 绑定 < acc2(1) < acc1(2) → 必选 acc3
	uid, hit := r.Resolve("s4", "glm", []string{"acc1", "acc2", "acc3"})
	if hit || uid != "acc3" {
		t.Errorf("应优先分配最空闲的 acc3，实际 uid=%s hit=%v", uid, hit)
	}
	// 同一个会话对 deepseek 选号：三号在 deepseek 上均 0 绑定，但总绑定 acc1=2, acc2=1, acc3=1 → 避开 acc1
	uid2, _ := r.Resolve("s5", "deepseek", []string{"acc1", "acc2", "acc3"})
	if uid2 == "acc1" {
		t.Errorf("deepseek 均 0 绑定时应按总绑定少者选，不应选总绑定最多的 acc1")
	}
}

func TestRouterDropUIDModel(t *testing.T) {
	r := newTestRouter(3 * time.Hour)
	r.Bind("s1", "glm", "acc1")
	r.Bind("s2", "glm", "acc1")
	r.Bind("s1", "deepseek", "acc1")
	r.Bind("s3", "glm", "acc2")

	if dropped := r.DropUIDModel("acc1", "glm"); dropped != 2 {
		t.Errorf("应只摘掉 (acc1, glm) 2 条，实际 %d", dropped)
	}
	if _, ok := r.BoundUID("s1", "deepseek"); !ok {
		t.Error("acc1 的 deepseek 绑定不应被误删")
	}
	if _, ok := r.BoundUID("s3", "glm"); !ok {
		t.Error("acc2 的 glm 绑定不应被误删")
	}
}

func TestRouterTouchThrottled(t *testing.T) {
	r := newTestRouter(10 * time.Minute) // 阈值 1 分钟
	r.Bind("s1", "glm", "acc1")

	recent := time.Now().Add(-10 * time.Second)
	r.mu.Lock()
	r.entries[sessionKey("s1", "glm")] = bindEntry{UID: "acc1", LastActive: recent}
	r.mu.Unlock()

	uid, hit := r.Resolve("s1", "glm", []string{"acc1"})
	if !hit || uid != "acc1" {
		t.Fatalf("应粘性命中 acc1，实际 %s hit=%v", uid, hit)
	}
	r.mu.RLock()
	afterFast := r.entries[sessionKey("s1", "glm")].LastActive
	r.mu.RUnlock()
	if !afterFast.Equal(recent) {
		t.Errorf("1 分钟内粘性命中应节流不刷新 LastActive：before=%v after=%v", recent, afterFast)
	}

	stale := time.Now().Add(-2 * time.Minute)
	r.mu.Lock()
	r.entries[sessionKey("s1", "glm")] = bindEntry{UID: "acc1", LastActive: stale}
	r.mu.Unlock()

	_, hit = r.Resolve("s1", "glm", []string{"acc1"})
	if !hit {
		t.Fatal("仍未过期，应命中")
	}
	r.mu.RLock()
	afterRefresh := r.entries[sessionKey("s1", "glm")].LastActive
	r.mu.RUnlock()
	if !afterRefresh.After(stale.Add(time.Minute)) {
		t.Errorf("超过 1 分钟后应刷新 LastActive，实际 %v", afterRefresh)
	}
}

func TestRouterStrategyExpireFirst(t *testing.T) {
	r := newTestRouter(3 * time.Hour)
	now := time.Now().In(billing.TZ)
	todayLate := time.Date(now.Year(), now.Month(), now.Day(), 23, 0, 0, 0, billing.TZ).Add(24 * time.Hour)
	day1Morning := todayLate
	day1Evening := todayLate.Add(30 * time.Minute)
	day2 := todayLate.Add(48 * time.Hour)

	cands := []CandidateInfo{
		{UID: "perm", Credits: 500, CreditsAt: 100, EarliestExpireAt: 0},
		{UID: "stale", Credits: 500, CreditsAt: 100, EarliestExpireAt: time.Now().Add(-time.Hour).Unix()},
		{UID: "day2", Credits: 500, CreditsAt: 100, EarliestExpireAt: day2.Unix()},
		{UID: "day1-a", Credits: 50, CreditsAt: 100, EarliestExpireAt: day1Morning.Unix()},
		{UID: "day1-b", Credits: 50, CreditsAt: 100, EarliestExpireAt: day1Evening.Unix()},
	}

	// 同日桶内两个号应分别被两个新会话选中（桶内负载均衡）
	got1, _ := r.ResolveWithStrategy("s1", "glm", cands, config.PoolStrategyExpireFirst)
	got2, _ := r.ResolveWithStrategy("s2", "glm", cands, config.PoolStrategyExpireFirst)
	if (got1 != "day1-a" && got1 != "day1-b") || (got2 != "day1-a" && got2 != "day1-b") || got1 == got2 {
		t.Errorf("临期优先应在最早 CST 自然日桶内分摊 [day1-a, day1-b]，实际 %q, %q", got1, got2)
	}

	// day1 不可用时，溢出到 day2
	cands2 := cands[:3] // perm, stale, day2
	got3, _ := r.ResolveWithStrategy("s3", "glm", cands2, config.PoolStrategyExpireFirst)
	if got3 != "day2" {
		t.Errorf("day1 耗尽后应选次近临期 day2，实际 %q", got3)
	}

	// Tier 1 为空时，回落到 Tier 2（perm / stale）
	cands3 := cands[:2] // perm, stale
	got4, _ := r.ResolveWithStrategy("s4", "glm", cands3, config.PoolStrategyExpireFirst)
	if got4 != "perm" && got4 != "stale" {
		t.Errorf("Tier 1 为空时应回落 Tier 2，实际 %q", got4)
	}

	// 即使策略切换或新增更临期号，已绑定的粘性会话依然保住命中
	uidSticky, hit := r.ResolveWithStrategy("s3", "glm", cands, config.PoolStrategyExpireFirst)
	if !hit || uidSticky != "day2" {
		t.Errorf("已有绑定应在候选内保持粘性（保前缀缓存），实际 uid=%q hit=%v", uidSticky, hit)
	}
}

func TestRouterStrategyCreditsFirst(t *testing.T) {
	r := newTestRouter(3 * time.Hour)

	// 1. 已知正余额 > 未知 (CreditsAt==0) > 已知零余额
	cands := []CandidateInfo{
		{UID: "zero", Credits: 0, CreditsAt: 100},
		{UID: "unknown", Credits: 0, CreditsAt: 0},
		{UID: "low", Credits: 20, CreditsAt: 100},
		{UID: "high", Credits: 100, CreditsAt: 100},
	}
	if uid, _ := r.ResolveWithStrategy("s1", "glm", cands, config.PoolStrategyCreditsFirst); uid != "high" {
		t.Errorf("应选最高已知正余额 high，实际 %q", uid)
	}

	// 无正余额时，未知优先于已知零余额
	candsNoPos := cands[:2]
	if uid, _ := r.ResolveWithStrategy("s2", "glm", candsNoPos, config.PoolStrategyCreditsFirst); uid != "unknown" {
		t.Errorf("未知余额应优先于已知零余额，实际 %q", uid)
	}

	// 2. 1.0 额度误差内按活跃绑定打散
	r2 := newTestRouter(3 * time.Hour)
	tied := []CandidateInfo{
		{UID: "a", Credits: 100.5, CreditsAt: 100},
		{UID: "b", Credits: 100.0, CreditsAt: 100},
	}
	u1, _ := r2.ResolveWithStrategy("x1", "glm", tied, config.PoolStrategyCreditsFirst)
	u2, _ := r2.ResolveWithStrategy("x2", "glm", tied, config.PoolStrategyCreditsFirst)
	if u1 == u2 {
		t.Errorf("1.0 额度误差内两个新会话应分散到不同账号，实际均选中 %q", u1)
	}
}

func TestRouterInflightTieBreaking(t *testing.T) {
	for _, strat := range []string{config.PoolStrategyExpireFirst, config.PoolStrategyBalanced, config.PoolStrategyCreditsFirst} {
		t.Run(strat, func(t *testing.T) {
			r := newTestRouter(3 * time.Hour)
			exp := time.Now().Add(24 * time.Hour).Unix()
			cands := []CandidateInfo{
				{UID: "busy", Credits: 500, CreditsAt: 100, EarliestExpireAt: exp, Inflight: 2},
				{UID: "idle", Credits: 100, CreditsAt: 100, EarliestExpireAt: exp, Inflight: 0},
			}
			// 空 sessionKey 也应遵循 min Inflight + strategy
			if uid, hit := r.ResolveWithStrategy("", "glm", cands, strat); hit || uid != "idle" {
				t.Errorf("%s 应优先低在途 idle，实际 uid=%q hit=%v", strat, uid, hit)
			}
			if n := r.Count(); n != 0 {
				t.Errorf("空 sessionKey 不应写入绑定表，实际 %d", n)
			}
		})
	}
}
