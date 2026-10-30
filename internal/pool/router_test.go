package pool

import (
	"fmt"
	"sync"
	"testing"
	"time"
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
