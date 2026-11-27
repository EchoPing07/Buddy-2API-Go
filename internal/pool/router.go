package pool

import (
	"hash/fnv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"buddy2api-go/internal/billing"
	"buddy2api-go/internal/config"
)

// CandidateInfo 路由选号时的候选账号元数据快照。
//
// 由调用方（Manager）在持自己锁时一次性收集，避免 Router 回调 Manager 导致锁顺序反转。
type CandidateInfo struct {
	UID              string
	Credits          float64
	CreditsAt        int64
	EarliestExpireAt int64
	Inflight         int64
}

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

// entryModel 从绑定键提取模型名。
func entryModel(ek string) string {
	if i := strings.LastIndexByte(ek, 0); i >= 0 {
		return ek[i+1:]
	}
	return ""
}

// Resolve 返回该会话应使用的账号（默认 balanced 策略）；ok=false 表示池内无可用账号。
//
// **本方法绝不回调调用方**。锁顺序强制单向 Manager → Router。
func (r *Router) Resolve(key, model string, avail []string) (uid string, hit bool) {
	if len(avail) == 0 {
		return "", false
	}
	cands := make([]CandidateInfo, len(avail))
	for i, u := range avail {
		cands[i] = CandidateInfo{UID: u}
	}
	return r.ResolveWithStrategy(key, model, cands, config.PoolStrategyBalanced)
}

// ResolveWithStrategy 按指定策略选号：
//   - 有会话键且存在未过期绑定且仍在候选集内 → 粘性命中（三种策略共用，保住前缀缓存）
//   - 否则按 strategy（expire_first / balanced / credits_first）+ 在途并发 + 活跃绑定数选号
//
// 锁顺序强制单向 Manager → Router，本方法绝不回调调用方。
func (r *Router) ResolveWithStrategy(key, model string, candidates []CandidateInfo, strategy string) (uid string, hit bool) {
	if len(candidates) == 0 {
		return "", false
	}
	candSet := make(map[string]struct{}, len(candidates))
	for _, c := range candidates {
		candSet[c.UID] = struct{}{}
	}
	k := sessionKey(key, model)

	// 无会话键：纯分发，不写表（否则每次都是一条永不复用的垃圾条目），
	// 但仍遵循所选策略与当前活跃绑定负载。
	if k == "" {
		modelByUID, totalByUID := r.activeCounts(model)
		uid = r.selectCandidate("", candidates, strategy, modelByUID, totalByUID, time.Now().Unix())
		return uid, false
	}

	// ── 快路径：命中且绑定的号仍在本次可用集合里 ──
	r.mu.RLock()
	e, ok := r.entries[k]
	r.mu.RUnlock()
	if ok && r.fresh(e) {
		if _, in := candSet[e.UID]; in {
			r.touchIfNeeded(k, e.LastActive)
			r.hit.Add(1)
			return e.UID, true
		}
	}

	// ── 慢路径 ──
	r.mu.Lock()
	defer r.mu.Unlock()
	// 重查：等锁期间可能已被同会话的另一个 goroutine 分配好
	if e, ok := r.entries[k]; ok && r.fresh(e) {
		if _, in := candSet[e.UID]; in {
			if r.shouldTouch(e.LastActive) {
				r.entries[k] = bindEntry{UID: e.UID, LastActive: time.Now()}
			}
			r.hit.Add(1)
			return e.UID, true
		}
	}
	// 绑定存在但已不可用（号被禁用 / 该模型冷却 / TTL 过期）：先摘掉再重分配。
	delete(r.entries, k)

	// 扫描活跃绑定并顺手清理所有已过期僵尸条目，避免继续占着绑定计数名额。
	modelByUID := make(map[string]int, len(candidates))
	totalByUID := make(map[string]int, len(candidates))
	for ek, ev := range r.entries {
		if !r.fresh(ev) {
			delete(r.entries, ek)
			continue
		}
		totalByUID[ev.UID]++
		if entryModel(ek) == model {
			modelByUID[ev.UID]++
		}
	}

	uid = r.selectCandidate(k, candidates, strategy, modelByUID, totalByUID, time.Now().Unix())
	r.entries[k] = bindEntry{UID: uid, LastActive: time.Now()}
	r.miss.Add(1)
	r.evictLocked()
	return uid, false
}

// activeCounts 在读锁下统计指定模型的绑定数与总绑定数（跳过过期条目）。
func (r *Router) activeCounts(model string) (modelByUID, totalByUID map[string]int) {
	modelByUID = make(map[string]int, 8)
	totalByUID = make(map[string]int, 8)
	r.mu.RLock()
	defer r.mu.RUnlock()
	for ek, ev := range r.entries {
		if !r.fresh(ev) {
			continue
		}
		totalByUID[ev.UID]++
		if entryModel(ek) == model {
			modelByUID[ev.UID]++
		}
	}
	return modelByUID, totalByUID
}

func (r *Router) selectCandidate(k string, candidates []CandidateInfo, strategy string, modelByUID, totalByUID map[string]int, now int64) string {
	switch strategy {
	case config.PoolStrategyBalanced:
		return r.pickBalanced(k, candidates, modelByUID, totalByUID)
	case config.PoolStrategyCreditsFirst:
		return r.pickCreditsFirst(k, candidates, modelByUID, totalByUID)
	default: // config.PoolStrategyExpireFirst
		return r.pickExpireFirst(k, candidates, modelByUID, totalByUID, now)
	}
}

// pickExpireFirst 临期优先（按北京时间自然日分桶）：
//   - Tier 1：存在未来到期时间（EarliestExpireAt > now）的候选，按 CST 自然日取最早一天桶；
//   - Tier 2（仅当 Tier 1 为空）：其余全部候选（永久包 / 未探测 / 已过期）；
//   - 桶内按在途并发 → (uid, model) 活跃绑定 → 总活跃绑定 → hash/rr 均衡分摊。
func (r *Router) pickExpireFirst(k string, candidates []CandidateInfo, modelByUID, totalByUID map[string]int, now int64) string {
	var minDay string
	for _, c := range candidates {
		if c.EarliestExpireAt <= now {
			continue
		}
		day := time.Unix(c.EarliestExpireAt, 0).In(billing.TZ).Format("2006-01-02")
		if minDay == "" || day < minDay {
			minDay = day
		}
	}
	var bucket []CandidateInfo
	if minDay != "" {
		bucket = make([]CandidateInfo, 0, len(candidates))
		for _, c := range candidates {
			if c.EarliestExpireAt > now &&
				time.Unix(c.EarliestExpireAt, 0).In(billing.TZ).Format("2006-01-02") == minDay {
				bucket = append(bucket, c)
			}
		}
	} else {
		bucket = candidates
	}
	return r.pickBalanced(k, bucket, modelByUID, totalByUID)
}

// pickBalanced 均衡分摊：min Inflight → min (uid, model) 活跃绑定 → min 总活跃绑定 → hash / rr。
func (r *Router) pickBalanced(k string, candidates []CandidateInfo, modelByUID, totalByUID map[string]int) string {
	pool := filterMinInflight(candidates)
	return r.pickByBindingsAndHash(k, pool, modelByUID, totalByUID)
}

// pickCreditsFirst 高余额优先：
//   - 先按 min Inflight 过滤（避免同毫秒并发全砸到同一个号）；
//   - 再按额度梯队过滤：已知正额度 (CreditsAt>0 && Credits>0) > 未知 (CreditsAt==0) > 已知零额度 (Credits<=0)；
//   - 正额度梯队内取最高额度及其 1.0 额度误差范围内的并列候选；
//   - 最后按 min (uid, model) 绑定 → min 总绑定 → hash / rr 决胜。
func (r *Router) pickCreditsFirst(k string, candidates []CandidateInfo, modelByUID, totalByUID map[string]int) string {
	pool := filterMinInflight(candidates)

	tierOf := func(c CandidateInfo) int {
		if c.CreditsAt > 0 && c.Credits > 0 {
			return 2
		}
		if c.CreditsAt == 0 {
			return 1
		}
		return 0
	}
	bestTier := -1
	for _, c := range pool {
		if t := tierOf(c); t > bestTier {
			bestTier = t
		}
	}
	tiered := make([]CandidateInfo, 0, len(pool))
	for _, c := range pool {
		if tierOf(c) == bestTier {
			tiered = append(tiered, c)
		}
	}

	if bestTier == 2 && len(tiered) > 1 {
		maxCred := tiered[0].Credits
		for _, c := range tiered[1:] {
			if c.Credits > maxCred {
				maxCred = c.Credits
			}
		}
		top := make([]CandidateInfo, 0, len(tiered))
		for _, c := range tiered {
			if maxCred-c.Credits <= 1.0+1e-9 {
				top = append(top, c)
			}
		}
		tiered = top
	}
	return r.pickByBindingsAndHash(k, tiered, modelByUID, totalByUID)
}

func filterMinInflight(candidates []CandidateInfo) []CandidateInfo {
	if len(candidates) <= 1 {
		return candidates
	}
	minInf := candidates[0].Inflight
	for _, c := range candidates[1:] {
		if c.Inflight < minInf {
			minInf = c.Inflight
		}
	}
	out := make([]CandidateInfo, 0, len(candidates))
	for _, c := range candidates {
		if c.Inflight == minInf {
			out = append(out, c)
		}
	}
	return out
}

func filterMinInt(candidates []CandidateInfo, score func(CandidateInfo) int) []CandidateInfo {
	if len(candidates) <= 1 {
		return candidates
	}
	minVal := score(candidates[0])
	for _, c := range candidates[1:] {
		if v := score(c); v < minVal {
			minVal = v
		}
	}
	out := make([]CandidateInfo, 0, len(candidates))
	for _, c := range candidates {
		if score(c) == minVal {
			out = append(out, c)
		}
	}
	return out
}

func (r *Router) pickByBindingsAndHash(k string, candidates []CandidateInfo, modelByUID, totalByUID map[string]int) string {
	pick := filterMinInt(candidates, func(c CandidateInfo) int { return modelByUID[c.UID] })
	pick = filterMinInt(pick, func(c CandidateInfo) int { return totalByUID[c.UID] })
	if k == "" {
		idx := int(uint64(r.rr.Add(1)-1) % uint64(len(pick)))
		return pick[idx].UID
	}
	return pick[hashIndex(k, len(pick))].UID
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

// BoundUID 返回会话当前绑定的账号（无绑定或已过期返回 false）。
func (r *Router) BoundUID(key, model string) (string, bool) {
	k := sessionKey(key, model)
	if k == "" {
		return "", false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[k]
	if !ok || !r.fresh(e) {
		return "", false
	}
	return e.UID, true
}

// Count 当前活跃绑定数（忽略已过期条目）。
func (r *Router) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := 0
	for _, v := range r.entries {
		if r.fresh(v) {
			n++
		}
	}
	return n
}

// CountByUID 统计每个账号被多少个活跃会话绑定（管理页「会话」列，忽略过期条目）。
func (r *Router) CountByUID() map[string]int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]int, 8)
	for _, v := range r.entries {
		if r.fresh(v) {
			out[v.UID]++
		}
	}
	return out
}

// CountByUIDModel 统计指定模型上每个账号的活跃会话绑定数（忽略过期条目）。
func (r *Router) CountByUIDModel(model string) map[string]int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]int, 8)
	for k, v := range r.entries {
		if r.fresh(v) && entryModel(k) == model {
			out[v.UID]++
		}
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
// 没有这一步，被禁用的号会继续作为「已占用」占着名额，
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

// DropUIDModel 摘掉该账号在指定模型上的会话绑定（模型限流/短冷却时调用）。
func (r *Router) DropUIDModel(uid, model string) int {
	suffix := "\x00" + model
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for k, v := range r.entries {
		if v.UID == uid && strings.HasSuffix(k, suffix) {
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

// touchThreshold 返回滑动续期节流阈值：默认 1 分钟；当 TTL < 4 分钟时退化为 ttl/4，
// 既消除高并发粘性命中的写锁争用，又保证短 TTL 测试滑动续期正常生效。
func (r *Router) touchThreshold() time.Duration {
	if r.ttl != nil {
		if ttl := r.ttl(); ttl > 0 && ttl < 4*time.Minute {
			return ttl / 4
		}
	}
	return time.Minute
}

func (r *Router) shouldTouch(lastActive time.Time) bool {
	return time.Since(lastActive) > r.touchThreshold()
}

func (r *Router) touchIfNeeded(k string, lastActive time.Time) {
	if r.shouldTouch(lastActive) {
		r.touch(k)
	}
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
