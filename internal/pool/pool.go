// Package pool 号池：把 /v1/chat/completions 分发到全部已存账号，
// 同一会话在 TTL 内粘同一账号，并按「账号级禁用 / (账号,模型) 级冷却」做故障规避。
//
// 设计约束（见 docs/POOL-LOADBALANCE.md）：
//   - 只影响 chat 路径。签到 / 成长任务 / 余额面板仍走 token.json 的当前活动账号；
//   - 实验开关关闭时零行为变化：Manager 不构造（proxy 侧字段为 nil），不读写任何表；
//   - 账号级（ManualDisabled / AutoDisabled）与模型级（modelCool）是**两个正交维度**，
//     不合并成一个 disabled 字段——手动关掉的号探测成功也不该自动打开；
//   - 冷却 / 粘性表纯内存（重启最多再撞一次），禁用状态落 SQLite（重启不能复活坏号）。
package pool

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"buddy2api-go/internal/auth"
	"buddy2api-go/internal/config"
	"buddy2api-go/internal/store"
	"buddy2api-go/internal/upstream"
)

// 自动禁用原因（落在 store.PoolAccountRow.AutoDisabled 里）。
const (
	AutoQuota      = "quota"      // 额度已用尽（14018 / 402），等探测
	AutoCredential = "credential" // 凭证失效（401 刷不动），需要重新登录
)

const (
	// defaultModelCool 上游没给重置时间时的有界退避。
	defaultModelCool = 10 * time.Minute
	// minModelCool 上游给的时间已过期（时钟偏差）时的最小冻结时长：
	// 防止「立刻解冻 → 立刻又撞」的死循环。
	minModelCool = 5 * time.Minute
	// persistQueueSize 状态落库队列容量。满了就丢弃并留痕——
	// 这是观测数据，宁可丢也不能在请求路径上阻塞。
	persistQueueSize = 256
)

// ErrNoAccount 池内无可用的该账号。
var ErrNoAccount = errors.New("号池内无此账号")

// Manager 号池管理器。
//
// nil 安全性：proxy 侧允许持 nil Manager 且所有调用点都先判 h.pool != nil，
// 但本类型的方法除 Enabled() 外都**不**为 nil 设计——调用方必须先判 nil。
type Manager struct {
	cfg    *config.Manager
	st     *store.Store
	book   *auth.AccountBook
	client *upstream.Client

	mu       sync.RWMutex
	accounts map[string]*AccountState                // uid → 状态
	members  []string                                // 池成员 uid（稳定顺序）
	toks     map[string]*auth.TokenStore             // uid → 独立 TokenStore（各自 refreshMu）
	cools    map[string]map[string]store.PoolCoolRow // uid → model → 冷却
	router   *Router

	persistCh   chan persistOp
	persistDone chan struct{}
	persistOnce sync.Once
	closeOnce   sync.Once

	// seqMu 保护 seq / applied：记录「每个 uid 已同步生效到的序号」，
	// worker 落库前比对，落后就丢弃（说明期间有更新的同步写已经覆盖了它）。
	seqMu   sync.Mutex
	seq     int64
	applied map[string]int64

	// probeLimit 手动探测限频：uid → 上次探测时刻
	probeLimit sync.Map
}

// persistOp 异步落库操作。载荷用具体结构而不是闭包：
// 闭包捕获的可能是锁内的状态指针，异步执行时会读到已被后续修改的值。
//
// seq 是全局单调序号，用于**丢弃过期写入**。没有它会出现两类真实故障：
//  1. NoteError 入队一条 manual_disabled=0 的旧行 → 用户随后点「禁用」同步写 1
//     → worker 再把旧的 0 写回去，DB 里禁用丢失，重启后坏号复活；
//  2. NoteError 入队 → DeleteAccount 同步删 → worker 又 UPSERT 回来，
//     账号已删但池状态行还在，重新导入同 uid 会立即被跳过。
type persistOp struct {
	seq     int64
	account *store.PoolAccountRow
	cool    *store.PoolCoolRow
	delCool bool
	delAcct bool
}

// New 构造号池管理器。
//
// 构造期会加载两张池状态表并枚全池成员（读 data/accounts/*.json 与 data/token.json）。
// 因此**只在开关开启时才应调用**：关闭时 proxy 持 nil，不产生任何文件与 DB 读取。
func New(cfg *config.Manager, st *store.Store, toks *auth.TokenStore, client *upstream.Client) *Manager {
	m := &Manager{
		cfg:         cfg,
		st:          st,
		book:        auth.NewAccountBook(toks.DataDir()),
		client:      client,
		accounts:    make(map[string]*AccountState, 8),
		toks:        make(map[string]*auth.TokenStore, 8),
		cools:       make(map[string]map[string]store.PoolCoolRow, 8),
		persistCh:   make(chan persistOp, persistQueueSize),
		persistDone: make(chan struct{}),
		applied:     make(map[string]int64, 8),
	}
	// TTL 从配置热读：改 TTL 不需要重建 Router，也不需要清表。
	m.router = NewRouter(0, func() time.Duration {
		return time.Duration(cfg.Get().PoolSessionTTL) * time.Minute
	})
	m.Load()
	m.startPersister()
	return m
}

// Enabled 号池总开关（每次从配置热读，UI 改完立刻生效）。
func (m *Manager) Enabled() bool {
	if m == nil {
		return false
	}
	return m.cfg.Get().PoolEnabled
}

// MaxRetry 网关内换号重试上限（不带本次尝试）。
func (m *Manager) MaxRetry() int {
	if m == nil {
		return 0
	}
	n := m.cfg.Get().PoolMaxRetry
	if n < 0 {
		return 0
	}
	if n > 5 {
		return 5
	}
	return n
}

// ── 加载 / 成员枚举 ──

// Load 重扫账号档案、池状态与冷却表（启动时与开启开关时调用）。
func (m *Manager) Load() {
	cools, err := m.st.LoadPoolCools(time.Now().Unix())
	if err != nil {
		slog.Error("加载号池冷却表失败", "error", err)
	}
	states, err := m.st.LoadPoolAccounts()
	if err != nil {
		slog.Error("加载号池账号状态失败", "error", err)
	}

	m.mu.Lock()
	m.cools = make(map[string]map[string]store.PoolCoolRow, len(cools))
	for _, c := range cools {
		if m.cools[c.UID] == nil {
			m.cools[c.UID] = make(map[string]store.PoolCoolRow, 2)
		}
		m.cools[c.UID][c.Model] = c
	}
	saved := make(map[string]store.PoolAccountRow, len(states))
	for _, s := range states {
		saved[s.UID] = s
	}
	m.mu.Unlock()

	m.reloadMembers(saved)
	slog.Info("号池已加载", "members", len(m.Members()),
		"cools", len(cools), "ttl_min", m.cfg.Get().PoolSessionTTL)
}

// reloadMembers 重扫池成员并合并已落库的状态。
//
// 池成员 = data/accounts/*.json 的 UID ∪ data/token.json 的 UID，按 UID 去重。
// **必须并上 token.json**：activateAccount 只在切换时才 archiveAccount，
// 用户从没切过号时，当前账号可能只存在于 token.json、不在 accounts/ 目录里。
// 同一 UID 两份都在时以 token.json 优先（它才是会被 EnsureValid 刷新的那份，
// 档案可能停在旧 token 上）。
func (m *Manager) reloadMembers(saved map[string]store.PoolAccountRow) {
	type candidate struct {
		uid     string
		path    string
		fromTok bool // 来自 token.json（优先）
	}
	byUID := make(map[string]candidate, 8)

	metas, err := m.book.List()
	if err != nil {
		slog.Warn("枚举账号档案失败", "error", err)
	}
	accountsDir := filepath.Join(m.book.DataDir(), "accounts")
	for _, meta := range metas {
		byUID[meta.UID] = candidate{uid: meta.UID, path: filepath.Join(accountsDir, meta.ID+".json")}
	}
	// token.json：当前活动账号
	tokPath := filepath.Join(m.book.DataDir(), "token.json")
	if active, err := auth.NewTokenStorePath(tokPath); err == nil {
		if t := active.Get(); t != nil && t.UID != "" {
			byUID[t.UID] = candidate{uid: t.UID, path: tokPath, fromTok: true}
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	old := m.accounts
	m.accounts = make(map[string]*AccountState, len(byUID))
	m.toks = make(map[string]*auth.TokenStore, len(byUID))
	m.members = m.members[:0]
	for uid, c := range byUID {
		ts, err := auth.NewTokenStorePath(c.path)
		if err != nil {
			slog.Warn("加载账号凭证失败", "uid", shortUID(uid), "path", c.path, "error", err)
			continue
		}
		// 复用已有状态里的观测计数，避免每次 Load 都把统计清零
		st := &AccountState{UID: uid}
		if prev, ok := old[uid]; ok {
			st.SuccessCount, st.ErrorCount, st.LastUsedAt = prev.SuccessCount, prev.ErrorCount, prev.LastUsedAt
		}
		if row, ok := saved[uid]; ok {
			applyRow(st, row)
		}
		m.accounts[uid] = st
		m.toks[uid] = ts
		m.members = append(m.members, uid)
	}
	sort.Strings(m.members) // 稳定顺序：Available() 的顺序直接决定分发的确定性
}

// ReloadMembers 重新枚举池成员（管理页开关切换 / 导入新账号后调用）。
func (m *Manager) ReloadMembers() {
	states, err := m.st.LoadPoolAccounts()
	if err != nil {
		slog.Error("重载号池账号状态失败", "error", err)
	}
	saved := make(map[string]store.PoolAccountRow, len(states))
	for _, s := range states {
		saved[s.UID] = s
	}
	m.reloadMembers(saved)
}

// Members 池成员 uid（有序）。
func (m *Manager) Members() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, len(m.members))
	copy(out, m.members)
	return out
}

// Available 该模型上当前可用的 uid（稳定顺序，不随机——随机性由 Router 提供，
// 两层都随机会互相抵消，反而让分配变得不可预测）。
func (m *Manager) Available(model string) []string {
	now := time.Now().Unix()
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.members))
	for _, uid := range m.members {
		if m.usableLocked(uid, model, now) {
			out = append(out, uid)
		}
	}
	return out
}

// usableLocked 可用性判定（调用方需持读锁或写锁）。
func (m *Manager) usableLocked(uid, model string, now int64) bool {
	st := m.accounts[uid]
	if st == nil || !st.Available() {
		return false
	}
	if c, ok := m.cools[uid][model]; ok && c.Until > now {
		return false
	}
	return true
}

// usable 可用性判定（自取锁，给 Router 的回调用）。
func (m *Manager) usable(uid, model string) bool {
	now := time.Now().Unix()
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.usableLocked(uid, model, now)
}

// TokenStore 取该账号的凭证存储（各账号独立 refreshMu）。
func (m *Manager) TokenStore(uid string) (*auth.TokenStore, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ts, ok := m.toks[uid]
	if !ok {
		return nil, ErrNoAccount
	}
	return ts, nil
}

// AccountStateOf 返回账号状态快照（管理页用）。
func (m *Manager) AccountStateOf(uid string) (AccountState, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	st, ok := m.accounts[uid]
	if !ok {
		return AccountState{}, false
	}
	return *st, true
}

// ── 会话路由 ──

// Resolve 选号：粘性命中优先，其次 idle-first 分配；tried 里的号会被跳过。
//
// tried 是本次请求已经试过的号：换号重试时不能重复选同一个号
// （否则「撞限流 → 重试 → 又选回它」直接空转）。
//
// 注意 tried 必须同时约束**粘性命中**：会话已绑定 acc1、而 acc1 本次刚失败时，
// 只把 tried 从候选集合里删掉是不够的——Router 的快路径会直接把 acc1 又返回回来。
// 所以把「是否在候选集合内」挤进 usable 回调，让快路径也能识别。
func (m *Manager) Resolve(sessionKey, model string, tried map[string]bool) (string, bool) {
	avail := m.Available(model)
	if len(avail) == 0 {
		return "", false
	}
	allowed := make(map[string]bool, len(avail))
	for _, u := range avail {
		if tried[u] {
			continue
		}
		allowed[u] = true
	}
	if len(allowed) == 0 {
		return "", false
	}
	candidates := make([]string, 0, len(allowed))
	for _, u := range avail {
		if allowed[u] {
			candidates = append(candidates, u)
		}
	}
	// 传给 Router 的必须是「此刻确实可用且未试过」的集合：
	// Router 不会再回调回来校验（那会造成锁顺序反转，见 Router.Resolve 注释）。
	uid, _ := m.router.Resolve(sessionKey, model, candidates)
	return uid, uid != ""
}

// Bind 请求成功后把会话绑到最终生效的账号。
func (m *Manager) Bind(sessionKey, model, uid string) {
	m.router.Bind(sessionKey, model, uid)
}

// UnbindIf 仅当会话绑定的是 uid 时解绑。
func (m *Manager) UnbindIf(sessionKey, model, uid string) {
	m.router.UnbindIf(sessionKey, model, uid)
}

// SessionCount 活跃会话绑定数。
func (m *Manager) SessionCount() int { return m.router.Count() }

// CountByUID 每个账号被多少会话绑定。
func (m *Manager) CountByUID() map[string]int { return m.router.CountByUID() }

// StickyStats 粘性命中 / 需分配累计数。
func (m *Manager) StickyStats() (hit, miss int64) { return m.router.Stats() }

// ResetSessions 清空全部会话绑定。
func (m *Manager) ResetSessions() { m.router.Reset() }

// ── 状态迁移 ──

// NoteSuccess 记录一次成功：清该模型的冷却、重置额度连击计数。
//
// 只清**当前模型**，不清别的模型：在 glm 上成功不能证明 glm 之外也恢复了。
func (m *Manager) NoteSuccess(uid, model string) {
	now := time.Now().Unix()
	m.mu.Lock()
	st := m.accounts[uid]
	if st != nil {
		st.SuccessCount++
		st.LastUsedAt = now
		st.QuotaFailStreak = 0 // 能成功说明额度还在，之前的连击作废
	}
	hadCool := false
	if set, ok := m.cools[uid]; ok {
		if _, ok := set[model]; ok {
			delete(set, model)
			hadCool = true
		}
		if len(set) == 0 {
			delete(m.cools, uid)
		}
	}
	var row *store.PoolAccountRow
	// 只有真正需要落盘的变化才入队：
	//  - 清了冷却（上面 hadCool）
	//  - 账号之前被自动禁用了、而这次成功没走到探测恢复（保守起见同步一次）
	// 否则每次成功的 chat 都会产生一条「内容完全没变」的 UPSERT，
	// 在 MaxOpenConns(1) 上与日志 worker 抢连接，还会把队列塞满挤掉真正的禁用写。
	if st != nil && st.AutoDisabled != "" {
		row = m.rowOfLocked(st)
	}
	m.mu.Unlock()

	if hadCool {
		m.persist(persistOp{delCool: true, cool: &store.PoolCoolRow{UID: uid, Model: model}})
		slog.Info("模型冷却已解除", "uid", shortUID(uid), "model", model)
	}
	if row != nil {
		m.persist(persistOp{account: row})
	}
}

// NoteError 记录一次错误并按分类推进状态。
//
// 幂等约定：同一请求的重试链会重复上报同一条错误（pi 自己还会再重试 3 次，
// 一次失败最多产生 12 个错误响应）。因此**写冷却/禁用必须幂等**：
// 重复写同一个 (uid,model) 冷却只刷新 until、只把 hits 加一（按请求计），
// 不会把 QuotaFailStreak 推到失控值。
func (m *Manager) NoteError(uid, model string, kind upstream.ChatErrKind, body []byte) {
	if !kind.Punishes() {
		// 5xx / 网络 / 渠道 / 参数问题都记在这里：只累加观测计数，不动状态。
		// 实测库里 73 条 503 全是 DNS 与 http2 超时，按失败禁用会让一次断网把全池打成禁用。
		m.mu.Lock()
		if st := m.accounts[uid]; st != nil {
			st.ErrorCount++
		}
		m.mu.Unlock()
		return
	}

	now := time.Now()
	nowUnix := now.Unix()
	// 在进锁**之前**取配置：cfg.Get() 要拿 config 的 RWMutex，而 config.Update
	// 持写锁时会做 3 个系统调用 + 写盘（MkdirAll/Marshal/WriteFile）。若在 Manager
	// 写锁内等它，一次「改设置」就能把整个 chat 入口的 Available() 全挡住。
	// 顺带避免 312 字节的 Config 值拷贝发生在锁内。
	coolMax := time.Duration(m.cfg.Get().PoolCoolMaxMin) * time.Minute

	m.mu.Lock()
	st := m.accounts[uid]
	if st == nil {
		m.mu.Unlock()
		return
	}
	st.ErrorCount++
	st.LastUsedAt = nowUnix

	var persistCool *store.PoolCoolRow
	var persistAcct *store.PoolAccountRow
	disabledNow := ""

	switch kind {
	case upstream.ErrKindModelRate:
		resetAt, hasReset := upstream.ParseRateReset(body)
		until := computeCoolUntil(now, resetAt, hasReset, coolMax)
		code := "429"
		if c, _ := upstream.BizCode(body); c != 0 {
			code = itoa(int64(c))
		}
		_, _ = upstream.BizCode(body)
		reason := truncRunes(errorReason(body), 200)
		set := m.cools[uid]
		if set == nil {
			set = make(map[string]store.PoolCoolRow, 2)
			m.cools[uid] = set
		}
		// hits 按「请求」而非「错误响应」累加：同 (uid, model) 已有冷却时不再 ++，
		// 否则网关 2 次重试 × pi 3 次重试会让一次真实限流显示成 12 次。
		hits := 1
		if prev, ok := set[model]; ok && prev.Until > nowUnix {
			hits = prev.Hits
		}
		row := store.PoolCoolRow{
			UID: uid, Model: model, Until: until.Unix(),
			ResetAt: resetAtUnix(resetAt, hasReset), Code: code, Reason: reason, Hits: hits,
		}
		set[model] = row
		persistCool = &row
		slog.Info("模型已冷却", "uid", shortUID(uid), "model", model, "code", code,
			"until", time.Unix(row.Until, 0).Format(time.RFC3339),
			"reset_at", row.ResetAt, "reason", reason)

	case upstream.ErrKindQuotaExhausted:
		// 连续 2 次才禁用：防上游边界时刻的偶发误报。
		st.QuotaFailStreak++
		if st.QuotaFailStreak >= 2 && st.AutoDisabled == "" {
			st.AutoDisabled = AutoQuota
			st.AutoDisabledAt = nowUnix
			st.AutoDisabledMsg = truncRunes(errorReason(body), 200)
			disabledNow = AutoQuota
		}

	case upstream.ErrKindCredential:
		if st.AutoDisabled == "" {
			st.AutoDisabled = AutoCredential
			st.AutoDisabledAt = nowUnix
			st.AutoDisabledMsg = "凭证已失效且刷新失败"
			disabledNow = AutoCredential
		}
	}
	persistAcct = m.rowOfLocked(st)
	m.mu.Unlock()

	if persistCool != nil {
		m.persist(persistOp{cool: persistCool})
	}
	if persistAcct != nil {
		m.persist(persistOp{account: persistAcct})
	}
	if disabledNow != "" {
		// 掉该账号的全部会话绑定：否则被禁用的号会继续作为「已占用」占着
		// idle-first 的名额，把空闲号挡在门外。
		n := m.router.DropUID(uid)
		slog.Warn("账号已自动禁用", "uid", shortUID(uid), "reason", disabledNow,
			"msg", persistAcct.AutoDisabledMsg, "dropped_sessions", n)
	}
}

// SetManualDisabled 手动禁用 / 启用。
//
// 启用时**同时清空自动域与连击计数**：用户显式点「启用」就是一个「我知道它好了，让我试」
// 的强信号，比我们的自动探测更权威。
func (m *Manager) SetManualDisabled(uid string, disabled bool) error {
	now := time.Now().Unix()
	m.mu.Lock()
	st := m.accounts[uid]
	if st == nil {
		m.mu.Unlock()
		return ErrNoAccount
	}
	st.ManualDisabled = disabled
	st.ManualDisabledAt = now
	if !disabled {
		st.AutoDisabled = ""
		st.AutoDisabledAt = 0
		st.AutoDisabledMsg = ""
		st.QuotaFailStreak = 0
	}
	row := m.rowOfLocked(st)
	m.mu.Unlock()

	// 手动操作不在热路径上：同步落库，并用 markApplied 让队列里的旧写入失效
	// （否则一个更早入队的 NoteError 行会把刚点的「禁用」覆盖掉）。
	m.syncWriteAccount(uid, row)
	if disabled {
		m.router.DropUID(uid)
	}
	slog.Info("账号手动状态已更新", "uid", shortUID(uid), "disabled", disabled)
	return nil
}

// ClearModelCool 手动清除某模型的冷却（「我知道好了，让我试试」）。
func (m *Manager) ClearModelCool(uid, model string) error {
	m.mu.Lock()
	if set, ok := m.cools[uid]; ok {
		delete(set, model)
		if len(set) == 0 {
			delete(m.cools, uid)
		}
	}
	m.mu.Unlock()
	if err := m.st.DeletePoolCool(uid, model); err != nil {
		return err
	}
	slog.Info("模型冷却已手动清除", "uid", shortUID(uid), "model", model)
	return nil
}

// DeleteAccount 清理某账号的全部池状态（删除账号档案时调用）。
func (m *Manager) DeleteAccount(uid string) {
	m.mu.Lock()
	delete(m.accounts, uid)
	delete(m.toks, uid)
	delete(m.cools, uid)
	keep := m.members[:0]
	for _, u := range m.members {
		if u != uid {
			keep = append(keep, u)
		}
	}
	m.members = keep
	m.mu.Unlock()

	m.router.DropUID(uid)
	m.syncDeleteAccount(uid)
}

// ── Snapshot（管理 API） ──

// AccountView 管理页用的账号视图（不含凭证）。
type AccountView struct {
	UID          string `json:"uid"`
	Nickname     string `json:"nickname"`
	Domain       string `json:"domain"`
	ExpiresAt    int64  `json:"expires_at"`
	ExpiresHuman string `json:"expires_human"`
	Active       bool   `json:"active"`
	RegionMatch  bool   `json:"region_match"`

	ManualDisabled  bool   `json:"manual_disabled"`
	AutoDisabled    string `json:"auto_disabled"`
	AutoDisabledAt  int64  `json:"auto_disabled_at"`
	AutoDisabledMsg string `json:"auto_disabled_msg"`
	QuotaFailStreak int    `json:"quota_fail_streak"`
	Available       bool   `json:"available"`

	Credits      float64 `json:"credits"`
	LastProbeAt  int64   `json:"last_probe_at"`
	LastProbeOK  bool    `json:"last_probe_ok"`
	LastProbeMsg string  `json:"last_probe_msg"`

	// ModelCooldowns 该账号当前被冷却的模型（6004 等模型级限额）。
	ModelCooldowns []CoolView `json:"model_cooldowns"`
	PartialBlocked bool       `json:"partial_blocked"` // 有冷却但账号本身可用

	BoundSessions int   `json:"bound_sessions"`
	SuccessCount  int64 `json:"success_count"`
	ErrorCount    int64 `json:"error_count"`
	LastUsedAt    int64 `json:"last_used_at"`
}

// CoolView 模型冷却视图。同时透出 until 与 reset_at：
// 前者是截断后的实际截止，后者是上游权威墙钟，两者不等时才知道上游给的时间超了我们的上限。
type CoolView struct {
	Model   string `json:"model"`
	Code    string `json:"code"`
	Until   int64  `json:"until"`
	ResetAt int64  `json:"reset_at"`
	Reason  string `json:"reason"`
	Hits    int    `json:"hits"`
}

// PoolView GET /admin/pool 的响应体。
type PoolView struct {
	Enabled      bool          `json:"enabled"`
	EnabledAt    int64         `json:"enabled_at"`
	SessionCount int           `json:"session_count"`
	MemberCount  int           `json:"member_count"`
	AvailableNow int           `json:"available_now"` // 至少有一个模型可用的账号数
	StickyHit    int64         `json:"sticky_hit_total"`
	StickyMiss   int64         `json:"sticky_miss_total"`
	Accounts     []AccountView `json:"accounts"`
}

// Snapshot 构造管理页视图。creditsOf 由调用方注入（余额需要额外上游调用，
// pool 包不自己做，避免探测路径与面板路径耦合）。
func (m *Manager) Snapshot(creditsOf func(uid string) float64) PoolView {
	cfg := m.cfg.Get()
	now := time.Now().Unix()
	// Router 的取数全部在 m.mu 之外：持 Manager 锁时绝不进 Router 的锁，
	// 这是「锁顺序单向 Manager → Router」的落实点（详见 Router.Resolve 的死锁说明）。
	byUID := m.router.CountByUID()
	sessCount := m.router.Count()
	hit, miss := m.router.Stats()

	// 第一段：持锁只做内存拷贝。**绝不在锁内调 creditsOf**——
	// 它是 admin 注入的一次 SQLite 查询，而 SQLite 是单连接（MaxOpenConns=1）。
	// 持读锁做 DB IO 会被 RWMutex 的写者优先规则放大：一次面板请求 + 一次
	// NoteError 排队，就能让之后所有 chat 的 Available() 一直等下去
	// （实测可阻塞 300ms+，上限是 busy_timeout 的 5s）。
	var creditsUIDs []string
	m.mu.RLock()
	views := make([]AccountView, 0, len(m.members))
	availableNow := 0
	for _, uid := range m.members {
		st := m.accounts[uid]
		if st == nil {
			continue
		}
		v := AccountView{
			UID: uid, ManualDisabled: st.ManualDisabled,
			AutoDisabled: st.AutoDisabled, AutoDisabledAt: st.AutoDisabledAt,
			AutoDisabledMsg: st.AutoDisabledMsg, QuotaFailStreak: st.QuotaFailStreak,
			LastProbeAt: st.LastProbeAt, LastProbeOK: st.LastProbeOK,
			LastProbeMsg: st.LastProbeMsg, BoundSessions: byUID[uid],
			SuccessCount: st.SuccessCount, ErrorCount: st.ErrorCount, LastUsedAt: st.LastUsedAt,
		}
		cools := make([]CoolView, 0, len(m.cools[uid]))
		for _, c := range m.cools[uid] {
			if c.Until <= now {
				continue
			}
			cools = append(cools, CoolView{
				Model: c.Model, Code: c.Code, Until: c.Until,
				ResetAt: c.ResetAt, Reason: c.Reason, Hits: c.Hits,
			})
		}
		sort.Slice(cools, func(i, j int) bool { return cools[i].Model < cools[j].Model })
		v.ModelCooldowns = cools
		v.PartialBlocked = len(cools) > 0 && st.Available()
		// 「可用」只回答整号维度：模型集合是上游动态下发的，这里不枚举全部模型，
		// 部分限额由 UI 看 model_cooldowns 自行展示。
		v.Available = st.Available()
		if v.Available {
			availableNow++
		}
		if ts := m.toks[uid]; ts != nil {
			if t := ts.Get(); t != nil {
				v.Nickname, v.Domain, v.ExpiresAt = t.Nickname, t.Domain, t.ExpiresAt
				v.ExpiresHuman = t.ExpiresInHuman()
				v.RegionMatch = t.Domain == config.EndpointOf(cfg.Region).Domain
			}
		}
		views = append(views, v)
		creditsUIDs = append(creditsUIDs, uid)
	}
	memberCount := len(m.members)
	m.mu.RUnlock()

	// 第二段：出了锁再查余额（可能是一次 DB 读）。
	if creditsOf != nil {
		for i := range views {
			views[i].Credits = creditsOf(creditsUIDs[i])
		}
	}
	return PoolView{
		Enabled: cfg.PoolEnabled, EnabledAt: cfg.PoolEnabledAt,
		SessionCount: sessCount, MemberCount: memberCount,
		AvailableNow: availableNow, StickyHit: hit, StickyMiss: miss,
		Accounts: views,
	}
}

// ── 落库 ──

// persist 把状态变更投递到异步落库队列。
//
// 绝不在请求路径上同步写 SQLite：SQLite 是单写者（MaxOpenConns=1），
// 一个慢写会卡住整个网关的日志写入与请求。
func (m *Manager) persist(op persistOp) {
	if m == nil {
		return
	}
	op.seq = m.nextSeq()
	// 直接向已关闭的 channel 发送会 panic；shutdown 期间静默丢弃是可接受的
	// （状态已在内存里，且进程即将退出）。
	defer func() { _ = recover() }()
	select {
	case m.persistCh <- op:
	default:
		// 队列满：优先保「关键状态」（禁用 / 删除），观测类可以丢。
		// 丢掉 AutoDisabled 会让重启后坏号复活，所以这里不能一律丢。
		if op.account != nil && (op.account.AutoDisabled != "" || op.account.ManualDisabled) {
			slog.Error("号池落库队列已满，关键状态丢弃（重启后可能复活坏号）",
				"uid", shortUID(op.account.UID), "manual", op.account.ManualDisabled,
				"auto", op.account.AutoDisabled)
		} else {
			slog.Warn("号池落库队列已满，丢弃一条")
		}
	}
}

// nextSeq 分配全局单调序号。
func (m *Manager) nextSeq() int64 {
	m.seqMu.Lock()
	defer m.seqMu.Unlock()
	m.seq++
	return m.seq
}

// markApplied 记录某个 uid 的状态已被同步写入 DB 到 seq。
// worker 落库前比对：op.seq < applied[uid] 说明期间有更新的同步写生效，本 op 过期。
func (m *Manager) markApplied(uid string, seq int64) {
	m.seqMu.Lock()
	if seq > m.applied[uid] {
		m.applied[uid] = seq
	}
	m.seqMu.Unlock()
}

// isStale 判断某个 op 是否已被更新的同步写超越。
func (m *Manager) isStale(uid string, seq int64) bool {
	m.seqMu.Lock()
	defer m.seqMu.Unlock()
	return seq < m.applied[uid]
}

// syncWriteAccount 同步写入账号状态并标记序号（用户操作 / 关键状态用）。
func (m *Manager) syncWriteAccount(uid string, row *store.PoolAccountRow) {
	seq := m.nextSeq()
	m.markApplied(uid, seq)
	if err := m.st.SavePoolAccount(row); err != nil {
		slog.Error("保存号池账号状态失败", "uid", shortUID(uid), "error", err)
	}
}

// syncDeleteAccount 同步删除账号状态并标记序号（同上）。
func (m *Manager) syncDeleteAccount(uid string) {
	seq := m.nextSeq()
	m.markApplied(uid, seq)
	if err := m.st.DeletePoolAccount(uid); err != nil {
		slog.Error("清理号池账号状态失败", "uid", shortUID(uid), "error", err)
	}
}

func (m *Manager) startPersister() {
	m.persistOnce.Do(func() {
		go func() {
			defer close(m.persistDone)
			for op := range m.persistCh {
				switch {
				case op.delAcct && op.account != nil:
					// 已被后续同步写/删除超越就不做（否则会把删掉的号 UPSERT 回来）
					if m.isStale(op.account.UID, op.seq) {
						continue
					}
					m.markApplied(op.account.UID, op.seq)
					if err := m.st.DeletePoolAccount(op.account.UID); err != nil {
						slog.Error("号池状态删除失败", "error", err)
					}
				case op.account != nil:
					// 过期写入丢弃：期间已有更新的（通常是用户的同步操作）生效，
					// 这里再写会把「用户刚点的禁用」覆盖回旧值。
					if m.isStale(op.account.UID, op.seq) {
						continue
					}
					m.markApplied(op.account.UID, op.seq)
					if err := m.st.SavePoolAccount(op.account); err != nil {
						slog.Error("号池账号状态落库失败", "uid", shortUID(op.account.UID), "error", err)
					}
				}
				if op.cool != nil {
					if op.delCool {
						if err := m.st.DeletePoolCool(op.cool.UID, op.cool.Model); err != nil {
							slog.Error("号池冷却删除失败", "error", err)
						}
					} else if err := m.st.SavePoolCool(op.cool); err != nil {
						slog.Error("号池冷却落库失败", "error", err)
					}
				}
			}
		}()
	})
}

// Close 停止落库 worker（进程退出时调用）。
//
// 关闭 channel 后**等队列刷完**再返回：不等的话，调用方紧接着关数据库，
// 排队中的写入会报「database is closed」，而它们携带的正是禁用状态——
// 丢了这些写，重启后坏号会复活。
//
// 用 sync.Once 保证只关一次（double Close 会 panic: close of closed channel）。
func (m *Manager) Close() {
	if m == nil {
		return
	}
	m.closeOnce.Do(func() {
		close(m.persistCh)
		<-m.persistDone
	})
}

// ── 内部工具 ──

// rowOfLocked 状态 → 落库行（调用方需持锁）。
func (m *Manager) rowOfLocked(st *AccountState) *store.PoolAccountRow {
	return &store.PoolAccountRow{
		UID: st.UID, ManualDisabled: st.ManualDisabled, ManualDisabledAt: st.ManualDisabledAt,
		AutoDisabled: st.AutoDisabled, AutoDisabledAt: st.AutoDisabledAt,
		AutoDisabledMsg: st.AutoDisabledMsg, QuotaFailStreak: st.QuotaFailStreak,
		LastProbeAt: st.LastProbeAt, LastProbeOK: st.LastProbeOK, LastProbeMsg: st.LastProbeMsg,
	}
}

// applyRow 落库行 → 内存状态。
func applyRow(st *AccountState, row store.PoolAccountRow) {
	st.ManualDisabled = row.ManualDisabled
	st.ManualDisabledAt = row.ManualDisabledAt
	st.AutoDisabled = row.AutoDisabled
	st.AutoDisabledAt = row.AutoDisabledAt
	st.AutoDisabledMsg = row.AutoDisabledMsg
	st.QuotaFailStreak = row.QuotaFailStreak
	st.LastProbeAt = row.LastProbeAt
	st.LastProbeOK = row.LastProbeOK
	st.LastProbeMsg = row.LastProbeMsg
}

// computeCoolUntil 计算实际生效的冷却截止。
//
//   - 上游给了重置时间：取 min(resetAt, now+max)，max 为配置的截断上限（默认 6h）；
//   - 上游没给（14018 之外的裸 429）：有界退避 defaultModelCool；
//   - 上游给的时间已过期（时钟偏差 / 陈旧文案）：now+minModelCool，防止
//     「立刻解冻 → 立刻又撞」的死循环。
func computeCoolUntil(now time.Time, resetAt time.Time, hasReset bool, max time.Duration) time.Time {
	if max <= 0 {
		max = 6 * time.Hour
	}
	if !hasReset || resetAt.IsZero() {
		return now.Add(defaultModelCool)
	}
	until := resetAt
	if cap := now.Add(max); until.After(cap) {
		until = cap
	}
	if !until.After(now) {
		until = now.Add(minModelCool)
	}
	return until
}

func resetAtUnix(t time.Time, ok bool) int64 {
	if !ok || t.IsZero() {
		return 0
	}
	return t.Unix()
}

// errorReason 取上游错误体的可读 reason（截断前的原文）。
func errorReason(body []byte) string {
	if _, msg := upstream.BizCode(body); msg != "" {
		return msg
	}
	return strings.TrimSpace(string(body))
}

func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

// shortUID 日志里只打 uid 后缀：uid 是账号标识，完整打进日志没有额外价值。
func shortUID(uid string) string {
	if len(uid) <= 8 {
		return uid
	}
	return uid[len(uid)-8:]
}

// ensure 目录存在（供测试与首次部署使用）。
var _ = os.MkdirAll
