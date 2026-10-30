package pool

import (
	"hash/fnv"
	"sync"
	"sync/atomic"
	"time"
)

// bindEntry 一条会话绑定。
//
// LastActive 用于滑动续期（每次命中都往后推 TTL）与 LRU 淘汰。
type bindEntry struct {
	UID        string
	LastActive time.Time
}

// Router 会话粘性路由表（纯内存）。
//
// 键是「会话键 + \x00 + 模型」而不是只用会话键：6004 是**模型级**限额，
// 不带模型维度时，「glm 撞了限额」会把同一会话在 deepseek 上的粘性一起打散，
// 而且那个被限的号会对这个会话永久闲置（绑定还在，但 resolve 永远不可用）。
//
// 并发模型：快路径 RLock 读、慢路径 Lock 后**重查**。重查是必须的——同一会话的
// 两个并发首请求若都走分配，会各绑一个号，后写的覆盖先写的，白浪费一次分配。
type Router struct {
	mu      sync.RWMutex
	entries map[string]bindEntry
	maxSize int
	ttl     func() time.Duration // 由 Manager 注入（读热改配置）；nil = 不判过期

	hit  atomic.Int64 // 粘性命中（观测用，不落盘）
	miss atomic.Int64 // 需要新分配的请求数
	rr   atomic.Int64 // 无会话键时的轮转游标
}

// NewRouter 创建路由表。
//
// maxSize <= 0 用默认 10000 条：10k ×（约 100 字节键 + 40 字节值）≈ 1.4MB，
// 对单实例网关是可接受的上限。ttl 为 nil 表示不做 TTL 过期判定。
func NewRouter(maxSize int, ttl func() time.Duration) *Router {
	if maxSize <= 0 {
		maxSize = 10000
	}
	return &Router{entries: make(map[string]bindEntry, 64), maxSize: maxSize, ttl: ttl}
}

// sessionKey 拼接绑定键。分隔符用 \x00：会话键来自请求头/请求体，
// 可能含任意可见字符，只有 NUL 不会出现在里面，因此不会与模型名混淆。
func sessionKey(key, model string) string {
	if key == "" {
		return ""
	}
	return key + "\x00" + model
}

// Resolve 返回该会话应使用的账号；ok=false 表示池内无可用账号。
//
// 分配策略是 **idle-first 双段**：先只在「未被任何活跃绑定占用的号」里挑，
// 挑空了才回落到全池。效果是 4 个账号 4 个不同对话时必然各占一个号，
// 第 5 个对话才开始共享——这正是「不同对话尽量分散到不同账号」要的行为。
//
// avail 必须是**调用方已确认可用**的集合（账号未禁用、该模型未冷却、不在本次 tried 里）。
//
// **本方法绝不回调调用方**。早期版本接受 `usable func(uid) bool` 回调来做二次校验，
// 结果形成 Router.mu.Lock → Manager.mu.RLock 的反向依赖，与
// Snapshot 的 Manager.mu.RLock → Router.mu.RLock 构成 ABBA 死锁
// （`TestLockOrderInversion` 会复现）。现在锁顺序被强制为**单向：Manager → Router**，
// Router 只读自己受锁保护的 entries，不看池状态。
func (r *Router) Resolve(key, model string, avail []string) (uid string, hit bool) {
	if len(avail) == 0 {
		return "", false
	}
	usable := func(u string) bool {
		for _, v := range avail {
			if v == u {
				return true
			}
		}
		return false
	}
	k := sessionKey(key, model)
	// 无会话键：纯分发，不写表（否则每次都是一条永不复用的垃圾条目）。
	//
	// 用 roundRobin 而非 hash(model)：只 hash model 会让**所有**无会话键的请求
	// 恒定落到同一个号上（实测 100/100），池化在这个分支上完全失效，
	// 而且那个号会被单点压垮。轮转让它们均匀铺开。
	if k == "" {
		n := int(r.rr.Add(1))
		return avail[n%len(avail)], false
	}

	// ── 快路径：命中且绑定的号仍在本次可用集合里 ──
	r.mu.RLock()
	e, ok := r.entries[k]
	r.mu.RUnlock()
	if ok && r.fresh(e) && usable(e.UID) {
		r.touch(k)
		r.hit.Add(1)
		return e.UID, true
	}

	// ── 慢路径 ──
	r.mu.Lock()
	defer r.mu.Unlock()
	// 重查：等锁期间可能已被同会话的另一个 goroutine 分配好
	if e, ok := r.entries[k]; ok && r.fresh(e) && usable(e.UID) {
		r.entries[k] = bindEntry{UID: e.UID, LastActive: time.Now()}
		r.hit.Add(1)
		return e.UID, true
	}
	// 绑定存在但已不可用（号被禁用 / 该模型冷却 / TTL 过期）：先摘掉再重分配。
	// 不摘的话它会一直占着 idle-first 的「已占用」名额，把空闲号挡在门外。
	delete(r.entries, k)

	bound := make(map[string]bool, len(r.entries))
	for _, v := range r.entries {
		bound[v.UID] = true
	}
	idle := make([]string, 0, len(avail))
	for _, u := range avail {
		if !bound[u] {
			idle = append(idle, u)
		}
	}
	pick := idle
	if len(pick) == 0 {
		pick = avail
	}
	uid = pick[hashIndex(k, len(pick))]
	r.entries[k] = bindEntry{UID: uid, LastActive: time.Now()}
	r.miss.Add(1)
	r.evictLocked()
	return uid, false
}

// Bind 显式把会话绑到指定账号（请求最终成功后调用）。
//
// 作用：多轮对话在失败轮里换了号时，让后续轮次**稳定收敛**到持续成功的那个号，
// 而不是下一轮又跳回原号（原号此时通常已被冷却排除）。
func (r *Router) Bind(key, model, uid string) {
	k := sessionKey(key, model)
	if k == "" || uid == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries[k] = bindEntry{UID: uid, LastActive: time.Now()}
	r.evictLocked()
}

// UnbindIf 仅当会话当前绑定的是 uid 时才解除。
//
// 必须先比对：会话 A 绑在 acc1 上，而 acc2 在别的会话撞了限流，
// 无条件解绑会把 A 的绑定误删。
func (r *Router) UnbindIf(key, model, uid string) {
	k := sessionKey(key, model)
	if k == "" {
		return
	}
	r.mu.Lock()
	if e, ok := r.entries[k]; ok && e.UID == uid {
		delete(r.entries, k)
	}
	r.mu.Unlock()
}

// BoundUID 返回会话当前绑定的账号（无绑定返回 false）。
func (r *Router) BoundUID(key, model string) (string, bool) {
	k := sessionKey(key, model)
	if k == "" {
		return "", false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[k]
	return e.UID, ok
}

// Count 当前活跃绑定数。
func (r *Router) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.entries)
}

// CountByUID 统计每个账号被多少个会话绑定（管理页「会话」列）。
func (r *Router) CountByUID() map[string]int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]int, 8)
	for _, v := range r.entries {
		out[v.UID]++
	}
	return out
}

// Reset 清空全部绑定（管理页「让所有会话重新分配」）。
func (r *Router) Reset() {
	r.mu.Lock()
	r.entries = make(map[string]bindEntry, 64)
	r.mu.Unlock()
}

// DropUID 摘掉所有绑定到 uid 的会话（账号被判不可用时调用）。
//
// 没有这一步，被禁用的号会继续作为「已占用」占着 idle-first 的名额，
// 同时那些会话每次请求都要走一遍「绑定不可用 → 删除 → 重分配」的慢路径。
func (r *Router) DropUID(uid string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for k, v := range r.entries {
		if v.UID == uid {
			delete(r.entries, k)
			n++
		}
	}
	return n
}

// Stats 返回粘性命中 / 需分配 累计数（观测用）。
// 命中率 = hit / (hit+miss)，是判断 TTL 该调大还是调小的唯一依据。
func (r *Router) Stats() (hit, miss int64) {
	return r.hit.Load(), r.miss.Load()
}

// fresh 判断绑定是否仍在 TTL 内。
// TTL 存在 Router.ttl 而不是每条目里：它是可热改配置，
// 存进条目会让旧条目一直沿用建条目时的旧值。
func (r *Router) fresh(e bindEntry) bool {
	if r.ttl == nil {
		return true
	}
	ttl := r.ttl()
	if ttl <= 0 {
		return true
	}
	return time.Since(e.LastActive) <= ttl
}

// touch 滑动续期。
func (r *Router) touch(k string) {
	r.mu.Lock()
	if e, ok := r.entries[k]; ok {
		e.LastActive = time.Now()
		r.entries[k] = e
	}
	r.mu.Unlock()
}

// evictLocked 容量超限时淘汰最久未活跃的 10%（调用方需持写锁）。
//
// O(n) 部分选择排序：10000 条量级下微秒级，且只在**新分配**时触发
// （粘性命中走不到这里），比维护双向链表简单且没有链表与 map 不一致的风险。
func (r *Router) evictLocked() {
	if len(r.entries) <= r.maxSize {
		return
	}
	target := len(r.entries) - r.maxSize + r.maxSize/10 + 1
	type kv struct {
		k string
		t time.Time
	}
	all := make([]kv, 0, len(r.entries))
	for k, v := range r.entries {
		all = append(all, kv{k, v.LastActive})
	}
	for i := 0; i < target && i < len(all); i++ {
		min := i
		for j := i + 1; j < len(all); j++ {
			if all[j].t.Before(all[min].t) {
				min = j
			}
		}
		all[i], all[min] = all[min], all[i]
		delete(r.entries, all[i].k)
	}
}

// hashIndex 用 FNV-1a 把键映射到 [0, n)。
//
// 只用来「在候选集合里打散」，不做「键 → 固定账号」的映射，
// 因此池成员增减时不需要重排整张表（比一致性哈希环简单且够用）。
func hashIndex(key string, n int) int {
	if n <= 1 {
		return 0
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return int(h.Sum32() % uint32(n))
}
