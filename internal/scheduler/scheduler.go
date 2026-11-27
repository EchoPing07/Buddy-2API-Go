// Package scheduler 定时任务：多账号自动签到状态机 + 成长任务链 + 猫猫旅行 + 夜猫子 +
// 任务队列 + 周期补领清扫 + 模型列表刷新 + 日志清理 + 号池探测。
package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"buddy2api-go/internal/auth"
	"buddy2api-go/internal/config"
	"buddy2api-go/internal/pool"
	"buddy2api-go/internal/store"
	"buddy2api-go/internal/taskops"
	"buddy2api-go/internal/upstream"
)

const (
	// checkinTickSpec 每分钟驱动一次签到状态机（随机时刻为分钟粒度，秒级无意义）。
	checkinTickSpec = "0 * * * * *"
	// checkinRetryDelay 一次尝试失败后的重试间隔；每个阶段只重试一次，再失败当天放弃。
	checkinRetryDelay = 5 * time.Minute
	// checkinFallbackMin 末班兜底时刻（当日 23:50），失败 23:55 重试一次后认栽。
	checkinFallbackMin = 23*60 + 50
	// checkinFixedStaggerClampMin fixed 模式下多账号顺延上限（当日 23:45，避免跨过末班兜底）。
	checkinFixedStaggerClampMin = 23*60 + 45
	// modelsRefreshSpec 每小时 01 分刷新一次模型列表（避开整点的签到/成长等任务密集期；
	// 失败由 ModelCache 自行保留旧表或回落内置表，此处仅记录日志）。
	modelsRefreshSpec = "0 1 * * * *"
	// randomTargetTTL 随机主时刻缓存有效期（跨日后失效重摇）。
	randomTargetTTL = 48 * 3600
)

// 缓存表名与基础键名常量（导出供 admin 侧同口径复用）。
const (
	CheckinCacheTable      = "checkin_cache"
	GrowthCacheTable       = "growth_cache"
	CheckinKeyStatus       = "status"
	CheckinKeyRandomTarget = "random_target"
	GrowthKeyOverview      = "overview"
	GrowthKeyReportDay     = "report_day"
	GrowthKeyAdoptDay      = "adopt_day"
	GrowthKeyRewardDay     = "reward_day"
	GrowthKeyLastRun       = "last_run"
	TaskKeyLastSweep       = "task_last_sweep"
	TaskKeyLastQueue       = "task_last_queue"
)

// ScopedCacheKey 构造按账号 UID 隔离的缓存键：uid 非空返回 "base:uid"，空串返回 base。
func ScopedCacheKey(base, uid string) string {
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return base
	}
	return base + ":" + uid
}

// CheckinStatusKey 签到状态缓存键。
func CheckinStatusKey(uid string) string { return ScopedCacheKey(CheckinKeyStatus, uid) }

// CheckinRandomTargetKey 当日随机签到主时刻缓存键。
func CheckinRandomTargetKey(uid string) string { return ScopedCacheKey(CheckinKeyRandomTarget, uid) }

// GrowthOverviewKey 成长概览缓存键。
func GrowthOverviewKey(uid string) string { return ScopedCacheKey(GrowthKeyOverview, uid) }

// GrowthReportDayKey 成长活跃上报日标志缓存键。
func GrowthReportDayKey(uid string) string { return ScopedCacheKey(GrowthKeyReportDay, uid) }

// GrowthAdoptDayKey 猫咪领养防抖日标志缓存键。
func GrowthAdoptDayKey(uid string) string { return ScopedCacheKey(GrowthKeyAdoptDay, uid) }

// GrowthRewardDayKey 成长奖励链日标志缓存键。
func GrowthRewardDayKey(uid string) string { return ScopedCacheKey(GrowthKeyRewardDay, uid) }

// GrowthLastRunKey 成长链上次执行结果缓存键。
func GrowthLastRunKey(uid string) string { return ScopedCacheKey(GrowthKeyLastRun, uid) }

// TaskLastSweepKey 任务补领清扫上次结果缓存键。
func TaskLastSweepKey(uid string) string { return ScopedCacheKey(TaskKeyLastSweep, uid) }

// TaskLastQueueKey 任务队列上次结果缓存键。
func TaskLastQueueKey(uid string) string { return ScopedCacheKey(TaskKeyLastQueue, uid) }

// multiAccountGap 多账号串行执行时的账号间隔，避免同时打上游触发风控。测试可置 0。
var multiAccountGap = 5 * time.Second

// secondsParser 6 段含秒 cron 解析器（与 admin.validCron 一致），用于推算 fixed 模式当日触发时刻。
var secondsParser = cron.NewParser(
	cron.Second | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor,
)

// TaskQueueHook 任务队列执行回调（旧单账号接口，由 admin 侧注入）。
// nil 时到点跳过（测试构造 Scheduler 时通常不注入）。
type TaskQueueHook func()

// TaskSweepHook 补领清扫回调（旧单账号接口，返回本轮汇总，供日志）。
type TaskSweepHook func() taskops.SweepStats

// schedAccount 调度器面向单个账号的执行上下文。
type schedAccount struct {
	uid    string
	ts     *auth.TokenStore
	client *upstream.Client
	growth growthClient
}

// Scheduler cron 调度器。
type Scheduler struct {
	mu     sync.Mutex
	c      *cron.Cron
	cfg    *config.Manager
	client *upstream.Client
	st     *store.Store
	models *upstream.ModelCache // 可为 nil（测试构造 Scheduler 时未注入则跳过模型刷新）
	tickID cron.EntryID
	cks    map[string]*checkinDay // 按 uid 隔离的签到日内状态（mu 保护，跨日自动重置）

	// 成长任务：growth 为默认账号的链编排上游接口（生产为 client 本体，测试可注入 fake）。
	// 多账号场景下按 uid 分别持有独立互斥锁与日内状态，不同账号互不阻塞。
	growthLocksMu      sync.Mutex
	growthLocks        map[string]*sync.Mutex
	growthStates       map[string]*growthDay
	growth             growthClient
	growthReportID     cron.EntryID  // 上报+奖励链 cron entry
	growthTravelID     cron.EntryID  // 旅行巡检 cron entry
	growthQueueID      cron.EntryID  // 任务队列 cron entry
	blackcatID         cron.EntryID  // 夜猫子 cron entry
	poolProbeID        cron.EntryID  // 号池探测 cron entry
	pool               *pool.Manager // 号池（nil = 单账号测试模式；非 nil 时探测始终开启）
	growthCatchupTimer *time.Timer   // 启动补跑 timer（s.mu 保护；Stop 时取消，重入重置不叠加）

	// 任务侧钩子（admin 在构造后注入；启动期写、运行期读，用 mutex 保护）
	hookMu           sync.Mutex
	taskQueueHook    TaskQueueHook
	taskSweepHook    TaskSweepHook
	taskQueueForHook func(uid string)
	taskSweepForHook func(uid string) taskops.SweepStats

	// 补领清扫独立 ticker（分钟级，与 cron 表达式无关）。
	sweepCtx    context.Context
	sweepCancel context.CancelFunc
	sweepDone   chan struct{}

	// runner 任务动作执行器（New 中创建一次，服务当前主账号）。**必须与 admin 共用同一个实例**：
	// Runner 的互斥锁是实例字段，各建各的会让夜猫子 cron 与面板「一键完成全部」
	// 同时跑两轮真实对话（各动作幂等，但含真实对话，重跑白耗配额）。
	runner *taskops.Runner
}

// New 创建调度器并启动日志清理任务（每天 03:00）与模型列表刷新（每小时 01 分）。
func New(cfg *config.Manager, client *upstream.Client, st *store.Store, models *upstream.ModelCache) *Scheduler {
	s := &Scheduler{
		c:            cron.New(cron.WithParser(secondsParser), cron.WithChain()),
		cfg:          cfg,
		client:       client,
		st:           st,
		models:       models,
		cks:          make(map[string]*checkinDay),
		growthLocks:  make(map[string]*sync.Mutex),
		growthStates: make(map[string]*growthDay),
		growth:       client,
	}
	s.runner = taskops.NewRunner(client)
	if _, err := s.c.AddFunc("0 0 3 * * *", s.cleanupLogs); err != nil {
		slog.Error("注册日志清理任务失败", "error", err)
	}
	if _, err := s.c.AddFunc(modelsRefreshSpec, s.refreshModels); err != nil {
		slog.Error("注册模型列表刷新任务失败", "error", err)
	}
	s.Reconfigure()
	s.c.Start()
	return s
}

// Runner 返回任务执行器（admin 应把它注入 Handler，共用同一把互斥锁）。
func (s *Scheduler) Runner() *taskops.Runner { return s.runner }

// SetPool 注入号池（非 nil 时号池探测与多账号调度均自动生效）。
//
// 必须在 Start 之后由调用方补一次 Reconfigure，使号池探测 cron 挂上。
func (s *Scheduler) SetPool(p *pool.Manager) {
	s.mu.Lock()
	s.pool = p
	s.mu.Unlock()
}

// getPool 线程安全地读取当前号池实例。
func (s *Scheduler) getPool() *pool.Manager {
	s.mu.Lock()
	p := s.pool
	s.mu.Unlock()
	return p
}

// SetRunner 用外部创建的执行器替换内置实例（admin 先构造 Handler 再注入 scheduler 时用）。
// 传 nil 不生效——内置实例始终存在，避免 tickBlackcat 拿到 nil。
func (s *Scheduler) SetRunner(r *taskops.Runner) {
	if r != nil {
		s.runner = r
	}
}

// SetTaskQueueHook 挂载旧版单账号任务队列回调（仅在未设置 TaskQueueForHook 时使用）。
func (s *Scheduler) SetTaskQueueHook(fn TaskQueueHook) {
	s.hookMu.Lock()
	s.taskQueueHook = fn
	s.hookMu.Unlock()
}

// SetTaskSweepHook 挂载旧版单账号补领清扫回调（仅在未设置 TaskSweepForHook 时使用）。
func (s *Scheduler) SetTaskSweepHook(fn TaskSweepHook) {
	s.hookMu.Lock()
	s.taskSweepHook = fn
	s.hookMu.Unlock()
}

// SetTaskQueueForHook 挂载按账号 UID 执行的任务队列回调。
func (s *Scheduler) SetTaskQueueForHook(fn func(uid string)) {
	s.hookMu.Lock()
	s.taskQueueForHook = fn
	s.hookMu.Unlock()
}

// SetTaskSweepForHook 挂载按账号 UID 执行的补领清扫回调。
func (s *Scheduler) SetTaskSweepForHook(fn func(uid string) taskops.SweepStats) {
	s.hookMu.Lock()
	s.taskSweepForHook = fn
	s.hookMu.Unlock()
}

// ── 多账号目标枚举 ──

// eligibleAccounts 枚举当前可参与定时签到/成长的全部账号。
func (s *Scheduler) eligibleAccounts() []schedAccount {
	return s.eligibleAccountsWithPool(s.getPool())
}

// eligibleAccountsWithPool 使用指定 pool 枚举目标账号（供已持有 s.mu 的路径复用，避免重入锁）。
//
// 规则：
//   - 有号池时遍历 p.Members()：跳过无 token、AutoCredential、域名与当前 region 不匹配的账号；
//   - 手动禁用（ManualDisabled）与额度用尽（AutoQuota）均不跳过——这些号仍应继续签到与成长；
//   - 无号池时（单测/单账号模式）退化为当前活动账号 s.client / s.growth。
func (s *Scheduler) eligibleAccountsWithPool(p *pool.Manager) []schedAccount {
	cfg := s.cfg.Get()
	wantDomain := config.EndpointOf(cfg.Region).Domain
	if p != nil {
		members := p.Members()
		out := make([]schedAccount, 0, len(members))
		for _, uid := range members {
			ts, err := p.TokenStore(uid)
			if err != nil || ts == nil {
				continue
			}
			t := ts.Get()
			if t == nil {
				continue
			}
			if st, ok := p.AccountStateOf(uid); ok && st.AutoDisabled == pool.AutoCredential {
				continue
			}
			if t.Domain != "" && t.Domain != wantDomain {
				continue
			}
			var c *upstream.Client
			var g growthClient
			if s.client != nil {
				c = s.client.WithTokenStore(ts)
				g = c
			}
			out = append(out, schedAccount{uid: uid, ts: ts, client: c, growth: g})
		}
		return out
	}

	g := s.growth
	if g == nil && s.client != nil {
		g = s.client
	}
	var t *auth.Token
	if g != nil {
		t = g.TokenSummary()
	} else if s.client != nil {
		t = s.client.TokenSummary()
	}
	if t == nil {
		return nil
	}
	if t.Domain != "" && t.Domain != wantDomain {
		return nil
	}
	return []schedAccount{{uid: t.UID, client: s.client, growth: g}}
}

// EligibleAccountUIDs 返回当前满足定时签到/成长条件的全部账号 UID（有序）。
func (s *Scheduler) EligibleAccountUIDs() []string {
	accts := s.eligibleAccounts()
	out := make([]string, 0, len(accts))
	for _, a := range accts {
		if a.uid != "" {
			out = append(out, a.uid)
		}
	}
	return out
}

// resolveAccount 解析显式单账号手动执行目标。
// uid == "" 使用当前活动账号（s.client / s.growth）；uid != "" 优先从 pool 查找对应 TokenStore。
func (s *Scheduler) resolveAccount(uid string) (schedAccount, string) {
	uid = strings.TrimSpace(uid)
	if uid == "" {
		g := s.growth
		if g == nil && s.client != nil {
			g = s.client
		}
		if g == nil {
			return schedAccount{}, "not_logged_in"
		}
		t := g.TokenSummary()
		if t == nil {
			return schedAccount{}, "not_logged_in"
		}
		return schedAccount{uid: t.UID, client: s.client, growth: g}, ""
	}

	if p := s.getPool(); p != nil {
		if ts, err := p.TokenStore(uid); err == nil && ts != nil {
			if ts.Get() == nil {
				return schedAccount{}, "not_logged_in"
			}
			var c *upstream.Client
			var g growthClient
			if s.client != nil {
				c = s.client.WithTokenStore(ts)
				g = c
			}
			return schedAccount{uid: uid, ts: ts, client: c, growth: g}, ""
		}
	}

	// 回落检查：未挂 pool 或 pool 尚未 reload 时，若当前活动账号正好匹配该 uid 则直接复用。
	g := s.growth
	if g == nil && s.client != nil {
		g = s.client
	}
	if g != nil {
		if t := g.TokenSummary(); t != nil && t.UID == uid {
			return schedAccount{uid: uid, client: s.client, growth: g}, ""
		}
	}
	return schedAccount{}, "not_logged_in"
}

// ── 装配 ──

// Reconfigure 按配置重装配定时任务：号池探测（pool 非 nil 即常驻）+ 签到（auto_checkin）
// + 成长任务（auto_growth）+ 补领清扫（task_sweep_enabled）。日内签到状态下次 tick 重建。
func (s *Scheduler) Reconfigure() {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 先摘旧 entry（签到 + 成长四类 + 夜猫子 + 号池探测），再按配置重挂
	old := []cron.EntryID{s.tickID, s.growthReportID, s.growthTravelID, s.growthQueueID, s.blackcatID, s.poolProbeID}
	for _, id := range old {
		if id != 0 {
			s.c.Remove(id)
		}
	}
	s.tickID, s.growthReportID, s.growthTravelID = 0, 0, 0
	s.growthQueueID, s.blackcatID, s.poolProbeID = 0, 0, 0
	s.cks = make(map[string]*checkinDay)

	cfg := s.cfg.Get()
	// 号池探测：只要注入了 pool 就始终挂载（不再受已废弃的 PoolEnabled 门控）。
	if s.pool != nil {
		if id, err := s.c.AddFunc(cfg.PoolProbeCron, s.tickPoolProbe); err != nil {
			slog.Error("注册号池探测失败", "error", err, "cron", cfg.PoolProbeCron)
		} else {
			s.poolProbeID = id
			slog.Info("号池探测已开启", "cron", cfg.PoolProbeCron)
		}
	}
	if cfg.AutoCheckin {
		id, err := s.c.AddFunc(checkinTickSpec, s.tickCheckin)
		if err != nil {
			slog.Error("注册签到任务失败", "error", err)
		} else {
			s.tickID = id
			mode := cfg.CheckinMode
			if mode == "random" {
				mode += " " + cfg.CheckinRandomStart + "~" + cfg.CheckinRandomEnd
			}
			slog.Info("自动签到已开启", "mode", mode, "fallback", cfg.CheckinFallback)
		}
	}
	if cfg.AutoGrowth {
		if id, err := s.c.AddFunc(cfg.GrowthReportCron, s.tickGrowthChain); err != nil {
			slog.Error("注册成长任务链失败", "error", err, "cron", cfg.GrowthReportCron)
		} else {
			s.growthReportID = id
		}
		if id, err := s.c.AddFunc(cfg.GrowthTravelCron, s.tickTravel); err != nil {
			slog.Error("注册旅行巡检失败", "error", err, "cron", cfg.GrowthTravelCron)
		} else {
			s.growthTravelID = id
		}
		// 任务队列：每日扫描待办并逐个执行。Sequential 族每日零点解锁一环，
		// 没有这条排程就只能手动点「执行全部待办」推进。
		if id, err := s.c.AddFunc(cfg.GrowthQueueCron, s.tickTaskQueue); err != nil {
			slog.Error("注册任务队列失败", "error", err, "cron", cfg.GrowthQueueCron)
		} else {
			s.growthQueueID = id
		}
		// 夜猫子：black_cat 只在夜间窗口计数，须在窗口内补足。
		if id, err := s.c.AddFunc(cfg.BlackcatCron, s.tickBlackcat); err != nil {
			slog.Error("注册夜猫子任务失败", "error", err, "cron", cfg.BlackcatCron)
		} else {
			s.blackcatID = id
		}
		slog.Info("成长任务已开启", "report_cron", cfg.GrowthReportCron,
			"travel_cron", cfg.GrowthTravelCron, "queue_cron", cfg.GrowthQueueCron,
			"blackcat_cron", cfg.BlackcatCron, "count", cfg.GrowthReportCount)
		// 启动补跑：当日已过上报时点且存在未上报的合规账号 → 30s 后补跑一次。
		// timer 由 s.mu 保护并在 Stop 取消。
		s.scheduleGrowthCatchupLocked(cfg)
	}
	interval := time.Duration(0)
	if cfg.TaskSweepEnabled && cfg.AutoGrowth {
		interval = time.Duration(cfg.TaskSweepMinutes) * time.Minute
	}
	s.startTaskSweepLocked(interval)
}

// startTaskSweepLocked 启停补领清扫 ticker（调用方需持 s.mu）。
// interval<=0 → 停止。重复调用会先停旧的再起新的，不会叠加。
func (s *Scheduler) startTaskSweepLocked(interval time.Duration) {
	if s.sweepCancel != nil {
		s.sweepCancel()
		s.sweepCancel = nil
		if s.sweepDone != nil {
			<-s.sweepDone
			s.sweepDone = nil
		}
	}
	if interval <= 0 {
		slog.Info("任务补领清扫已禁用")
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.sweepCtx, s.sweepCancel, s.sweepDone = ctx, cancel, done
	slog.Info("任务补领清扫已启动", "interval", interval,
		"说明", "分钟级 accept/claim，只补领已达标未领的奖励，不跑任何行为链")
	go func() {
		defer close(done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.runSweepWithContext(ctx)
			}
		}
	}()
}

// runSweepWithContext 一轮清扫（带取消）。ctx 只用于「关机时不再发起新请求」；
// Sweep 的单次 HTTP 已有 short 客户端 30s 超时兜底，不必在请求层接 ctx。
func (s *Scheduler) runSweepWithContext(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	_ = s.runTaskSweepOnce(ctx)
}

// RunTaskSweepOnce 执行一轮补领清扫（ticker 与手动按钮共用）。
// 若注入了 TaskSweepForHook 则串行遍历全部合规 CN 账号并汇总统计；否则回落旧单账号钩子。
func (s *Scheduler) RunTaskSweepOnce() taskops.SweepStats {
	return s.runTaskSweepOnce(context.Background())
}

func (s *Scheduler) runTaskSweepOnce(ctx context.Context) taskops.SweepStats {
	s.hookMu.Lock()
	forFn := s.taskSweepForHook
	legacyFn := s.taskSweepHook
	s.hookMu.Unlock()

	if forFn != nil {
		if s.cfg.Get().Region != "cn" {
			return taskops.SweepStats{Reason: "region_global"}
		}
		accts := s.eligibleAccounts()
		if len(accts) == 0 {
			return taskops.SweepStats{Reason: "not_logged_in"}
		}
		agg := taskops.SweepStats{At: time.Now().Unix()}
		for i, acct := range accts {
			if ctx.Err() != nil {
				break
			}
			st := forFn(acct.uid)
			if st.Ran {
				agg.Ran = true
			}
			agg.Accepted += st.Accepted
			agg.Claimed += st.Claimed
			agg.Credits += st.Credits
			agg.Energy += st.Energy
			agg.Errors += st.Errors
			if i < len(accts)-1 && !sleepContext(ctx, multiAccountGap) {
				break
			}
		}
		return agg
	}
	if legacyFn == nil {
		return taskops.SweepStats{Reason: "no_hook"}
	}
	return legacyFn()
}

// tickTaskQueue 任务队列 cron 入口。
// 若注入了 TaskQueueForHook 则串行遍历全部合规 CN 账号；否则回落旧单账号钩子。
func (s *Scheduler) tickTaskQueue() {
	s.hookMu.Lock()
	forFn := s.taskQueueForHook
	legacyFn := s.taskQueueHook
	s.hookMu.Unlock()

	if forFn != nil {
		if s.cfg.Get().Region != "cn" {
			return
		}
		accts := s.eligibleAccounts()
		for i, acct := range accts {
			forFn(acct.uid)
			if i < len(accts)-1 && multiAccountGap > 0 {
				time.Sleep(multiAccountGap)
			}
		}
		return
	}
	if legacyFn != nil {
		legacyFn()
	}
}

// tickBlackcat 夜猫子 cron 入口：窗口内遍历全部合规 CN 账号逐个完成 black_cat。
func (s *Scheduler) tickBlackcat() {
	cfg := s.cfg.Get()
	if cfg.Region != "cn" {
		return
	}
	if !upstream.InNightWindow(time.Now()) {
		slog.Info("blackcat: 当前不在 23:00–08:00 计数窗口，跳过")
		return
	}
	accts := s.eligibleAccounts()
	if len(accts) == 0 {
		return
	}
	activeUID := ""
	if s.client != nil {
		if t := s.client.TokenSummary(); t != nil {
			activeUID = t.UID
		}
	}
	p := s.getPool()
	for i, acct := range accts {
		var runner *taskops.Runner
		if (p == nil || (acct.uid != "" && acct.uid == activeUID)) && s.runner != nil {
			runner = s.runner
		} else if acct.client != nil {
			runner = taskops.NewRunner(acct.client)
		}
		if runner == nil {
			continue
		}
		if reason := runner.Gate(cfg.Region); reason != "" {
			continue
		}
		res, err := runner.Auto("black_cat")
		switch {
		case err != nil:
			slog.Warn("blackcat: 执行失败", "uid", uidTail(acct.uid), "error", err)
		case res.Claimed:
			slog.Info("blackcat: 完成并领奖", "uid", uidTail(acct.uid), "progress", res.ProgressAfter,
				"credit", res.Credit, "energy", res.Energy)
		default:
			slog.Info("blackcat: 已处理", "uid", uidTail(acct.uid), "status", res.Status, "message", res.Message,
				"progress", res.ProgressBefore+"→"+res.ProgressAfter)
		}
		if i < len(accts)-1 && multiAccountGap > 0 {
			time.Sleep(multiAccountGap)
		}
	}
}

// Stop 停止调度器（含取消未触发的成长补跑 timer 与补领清扫 ticker）。
func (s *Scheduler) Stop() {
	s.mu.Lock()
	if s.growthCatchupTimer != nil {
		s.growthCatchupTimer.Stop()
		s.growthCatchupTimer = nil
	}
	if s.sweepCancel != nil {
		s.sweepCancel()
		s.sweepCancel = nil
	}
	done := s.sweepDone
	s.sweepDone = nil
	s.mu.Unlock()
	if done != nil {
		<-done // 已在跑的那一轮会自行返回（单请求 30s 超时兜底）
	}
	<-s.c.Stop().Done()
}

// ResetAccountState 切换活动账号后的状态清理。
//
// 现已全面按 UID 隔离内存态与 SQLite 缓存，换号不再清空其它账号的日内状态与缓存；
// 此处仅清理历史残留的无后缀（uid == ""）键与状态。
func (s *Scheduler) ResetAccountState() {
	s.mu.Lock()
	delete(s.cks, "")
	s.mu.Unlock()

	mu := s.growthLock("")
	mu.Lock()
	s.growthLocksMu.Lock()
	delete(s.growthStates, "")
	s.growthLocksMu.Unlock()
	for _, k := range growthAccountScopedKeys {
		_ = s.st.DeleteCacheKey(growthCacheTable, k)
	}
	mu.Unlock()
}

// growthAccountScopedKeys 历史无后缀的账号口径缓存键（仅用于清理旧版升级残留）。
var growthAccountScopedKeys = []string{
	growthKeyReportDay,
	growthKeyAdoptDay,
	growthKeyRewardDay,
	TaskKeyLastSweep,
	TaskKeyLastQueue,
	growthKeyLastRun,
}

// ── 签到状态机 ──

// checkinDay 一日内的签到状态与决策（纯逻辑，便于单测）。
type checkinDay struct {
	day      string    // 状态所属日 "2006-01-02"
	mainAt   time.Time // 当日主时刻：random=窗口内随机（持久化），fixed=cron 当日触发
	mainHit  bool      // 主时刻已尝试（含宕机后补签）
	retryAt  time.Time // 待重试时刻（零值表示无）
	retryTag string    // 重试所属阶段（日志前缀）
	fbHit    bool      // 末班已尝试
	secured  bool      // 当日已落袋（领取成功或早已签，含手动）
}

// decide 判定此刻应执行的尝试并推进状态；label 为空表示无事可做，
// allowRetry 表示该尝试失败后还可安排一次重试（重试本身失败则当天放弃）。
func (d *checkinDay) decide(now time.Time, fallbackEnabled bool, fallbackAt time.Time) (label string, allowRetry bool) {
	switch {
	case d.secured:
	case !d.retryAt.IsZero() && !now.Before(d.retryAt):
		d.retryAt = time.Time{}
		return d.retryTag + "重试", false
	case !d.mainHit && !d.mainAt.IsZero() && !now.Before(d.mainAt):
		d.mainHit = true
		return "定时签到", true
	case fallbackEnabled && !d.fbHit && !now.Before(fallbackAt):
		d.fbHit = true
		return "末班签到", true
	}
	return "", false
}

// tickCheckin 每分钟驱动多账号签到状态机：主时刻（含宕机补签）→ 失败 5 分钟重试一次
// → 23:50 末班兜底（同样只带一次重试）。所有尝试先查状态，已签（含手动）即落袋跳过。
func (s *Scheduler) tickCheckin() { s.tickCheckinAt(time.Now()) }

// tickCheckinAt 可注入时刻的签到驱动（单测可确定性推进分钟，不依赖墙钟）。
func (s *Scheduler) tickCheckinAt(now time.Time) {
	cfg := s.cfg.Get()
	if cfg.Region != "cn" {
		return
	}
	accts := s.eligibleAccounts()
	if len(accts) == 0 {
		return
	}
	day := now.Format("2006-01-02")
	fallbackAt := minuteOfDay(now, checkinFallbackMin)

	for idx, acct := range accts {
		s.mu.Lock()
		d := s.rollDayLocked(acct.uid, idx, now, cfg)
		label, allowRetry := d.decide(now, cfg.CheckinFallback, fallbackAt)
		s.mu.Unlock()

		if label == "" {
			continue
		}
		if s.attemptCheckinAccount(acct, label) {
			s.mu.Lock()
			if cur := s.cks[acct.uid]; cur != nil && cur.day == day {
				cur.secured = true
			}
			s.mu.Unlock()
			continue
		}
		if allowRetry {
			s.mu.Lock()
			if cur := s.cks[acct.uid]; cur != nil && cur.day == day {
				cur.retryAt, cur.retryTag = now.Add(checkinRetryDelay), label
			}
			s.mu.Unlock()
		}
	}
}

// rollDayLocked 按 uid 跨日重置状态并推算当日主时刻（调用方需持 s.mu）：
//   - random 模式：每个 uid 独立复用/生成窗口内随机时刻并持久化到 random_target:<uid>；
//   - fixed 模式：idx=0 严格命中 cron 时刻，其余账号依次顺延 idx 分钟（钳制在 23:45 之前），
//     避免多账号同秒冲击上游。
func (s *Scheduler) rollDayLocked(uid string, idx int, now time.Time, cfg config.Config) *checkinDay {
	day := now.Format("2006-01-02")
	if s.cks == nil {
		s.cks = make(map[string]*checkinDay)
	}
	if d, ok := s.cks[uid]; ok && d.day == day {
		return d
	}
	d := &checkinDay{day: day}
	s.cks[uid] = d
	if cfg.CheckinMode == "random" {
		d.mainAt = s.randomMainAtFor(uid, now, cfg)
		return d
	}
	base := fixedMainAt("签到", cfg.CheckinCron, now)
	if base.IsZero() {
		slog.Warn("签到 cron 当日无触发时刻", "uid", uidTail(uid), "cron", cfg.CheckinCron)
		return d
	}
	d.mainAt = staggerFixedMainAt(base, idx, now)
	return d
}

// staggerFixedMainAt fixed 模式多账号确定性错峰：idx=0 精确等于 base，
// idx>0 顺延 idx 分钟并在 base < 23:45 时钳制在 23:45 之前。
func staggerFixedMainAt(base time.Time, idx int, now time.Time) time.Time {
	if idx <= 0 {
		return base
	}
	limit := minuteOfDay(now, checkinFixedStaggerClampMin)
	if !base.Before(limit) {
		return base
	}
	staggered := base.Add(time.Duration(idx) * time.Minute)
	if staggered.After(limit) {
		return limit
	}
	return staggered
}

// randomMainAtFor 取指定账号当日随机主时刻：优先复用缓存里今日已生成的（重启不重摇），
// 否则窗口内随机并落缓存。
func (s *Scheduler) randomMainAtFor(uid string, now time.Time, cfg config.Config) time.Time {
	day := now.Format("2006-01-02")
	key := CheckinRandomTargetKey(uid)
	if payload, _, ok := s.st.GetCache(CheckinCacheTable, key, randomTargetTTL); ok {
		if t, err := time.Parse(time.RFC3339, payload); err == nil && t.Format("2006-01-02") == day {
			return t
		}
	}
	sm, ok1 := config.ParseHHMM(cfg.CheckinRandomStart)
	em, ok2 := config.ParseHHMM(cfg.CheckinRandomEnd)
	if !ok1 || !ok2 || em <= sm {
		sm, em = 9*60, 18*60 // Normalize 已挡非法配置，双保险
	}
	t := minuteOfDay(now, randomMinute(sm, em))
	_ = s.st.SetCache(CheckinCacheTable, key, t.Format(time.RFC3339))
	slog.Info("已生成今日随机签到时刻", "uid", uidTail(uid), "at", t.Format("15:04"))
	return t
}

// randomMinute 在 [startMin, endMin) 内随机取一个分钟数。
func randomMinute(startMin, endMin int) int { return startMin + rand.IntN(endMin-startMin) }

// markCheckinSecured 标记指定账号当日已落袋，避免手动签到后定时任务重复请求。
func (s *Scheduler) markCheckinSecured(uid string, now time.Time) {
	day := now.Format("2006-01-02")
	s.mu.Lock()
	if s.cks == nil {
		s.cks = make(map[string]*checkinDay)
	}
	d := s.cks[uid]
	if d == nil || d.day != day {
		d = &checkinDay{day: day}
		s.cks[uid] = d
	}
	d.secured = true
	s.mu.Unlock()
}

// ClaimCheckinFor 为指定账号执行一次签到（供 admin 面板 / 一键全签复用）：
// 先查状态（已签直接返回 alreadyChecked=true），未签则领取，同步更新 checkin_cache 与日内状态。
func (s *Scheduler) ClaimCheckinFor(uid string) (credit float64, alreadyChecked bool, err error) {
	acct, reason := s.resolveAccount(uid)
	if reason != "" || acct.client == nil {
		if reason == "not_logged_in" {
			return 0, false, upstream.ErrNoToken
		}
		return 0, false, fmt.Errorf("账号不可用: %s", reason)
	}
	statusKey := CheckinStatusKey(acct.uid)
	if res, serr := acct.client.CheckinStatus(); serr == nil {
		var env struct {
			Data struct {
				Active         bool `json:"active"`
				TodayCheckedIn bool `json:"today_checked_in"`
			} `json:"data"`
		}
		if json.Unmarshal(res.Raw, &env) == nil {
			_ = s.st.SetCache(CheckinCacheTable, statusKey, string(res.Raw))
			if env.Data.TodayCheckedIn {
				s.markCheckinSecured(acct.uid, time.Now())
				return 0, true, nil
			}
		}
	}
	res, cerr := acct.client.ClaimCheckin()
	if cerr != nil {
		return 0, false, cerr
	}
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Credit         float64 `json:"credit"`
			TodayCheckedIn bool    `json:"today_checked_in"`
		} `json:"data"`
	}
	_ = json.Unmarshal(res.Raw, &env)
	if env.Code == 0 && (env.Data.Credit > 0 || env.Data.TodayCheckedIn) {
		_ = s.st.SetCache(CheckinCacheTable, statusKey, string(res.Raw))
		s.markCheckinSecured(acct.uid, time.Now())
		return env.Data.Credit, env.Data.Credit <= 0 && env.Data.TodayCheckedIn, nil
	}
	return 0, false, fmt.Errorf("签到未成功: code=%d msg=%s", env.Code, env.Msg)
}

// AttemptCheckinFor 为指定账号执行一次带日志标签的签到尝试，返回当日是否已落袋。
func (s *Scheduler) AttemptCheckinFor(uid, label string) bool {
	acct, reason := s.resolveAccount(uid)
	if reason != "" || acct.client == nil {
		return false
	}
	if s.attemptCheckinAccount(acct, label) {
		s.markCheckinSecured(acct.uid, time.Now())
		return true
	}
	return false
}

// attemptCheckinAccount 对单个账号执行一次签到尝试：先查状态（已签则跳过），未签则领取；
// 返回当日是否已落袋（领取成功或早已签）。
func (s *Scheduler) attemptCheckinAccount(acct schedAccount, label string) bool {
	if acct.client == nil {
		return false
	}
	statusKey := CheckinStatusKey(acct.uid)
	if res, err := acct.client.CheckinStatus(); err == nil {
		var env struct {
			Data struct {
				Active         bool `json:"active"`
				TodayCheckedIn bool `json:"today_checked_in"`
			} `json:"data"`
		}
		if json.Unmarshal(res.Raw, &env) == nil {
			_ = s.st.SetCache(CheckinCacheTable, statusKey, string(res.Raw))
			if env.Data.TodayCheckedIn {
				slog.Info(label+"：今日已签到，跳过", "uid", uidTail(acct.uid))
				return true
			}
		}
	}
	res, err := acct.client.ClaimCheckin()
	if err != nil {
		slog.Error(label+"失败", "uid", uidTail(acct.uid), "error", err)
		return false
	}
	var env struct {
		Code int `json:"code"`
		Data struct {
			Credit         float64 `json:"credit"`
			TodayCheckedIn bool    `json:"today_checked_in"`
		} `json:"data"`
	}
	_ = json.Unmarshal(res.Raw, &env)
	if env.Code == 0 && (env.Data.Credit > 0 || env.Data.TodayCheckedIn) {
		slog.Info(label+"成功", "uid", uidTail(acct.uid), "credit", env.Data.Credit)
		_ = s.st.SetCache(CheckinCacheTable, statusKey, string(res.Raw))
		return true
	}
	slog.Warn(label+"未成功", "uid", uidTail(acct.uid), "code", env.Code, "checked_in", env.Data.TodayCheckedIn)
	return false
}

// fixedMainAt 取 cron 当日触发时刻；表达式非法或当日无触发（如仅工作日）返回零值。
// label 用于非法表达式时的日志前缀（签到/成长上报等）。
func fixedMainAt(label, cronExpr string, now time.Time) time.Time {
	sched, err := secondsParser.Parse(cronExpr)
	if err != nil {
		slog.Error(label+" cron 非法，当日无触发时刻", "error", err, "cron", cronExpr)
		return time.Time{}
	}
	day := now.Format("2006-01-02")
	if t := sched.Next(dayStart(now)); t.Format("2006-01-02") == day {
		return t
	}
	return time.Time{}
}

// dayStart 当地时区当日零点。
func dayStart(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

// minuteOfDay 当地时区当日第 minutes 分钟（0-1439）。
func minuteOfDay(t time.Time, minutes int) time.Time {
	return dayStart(t).Add(time.Duration(minutes) * time.Minute)
}

// sleepContext 可被 ctx 打断的等待，返回 true 表示正常睡完。
func sleepContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// refreshModels 定时刷新模型列表（/v3/config）。失败不改变已有缓存：
// ModelCache.Refresh 在同 region 失败时保留旧表，换 region 失败时才回落内置表。
func (s *Scheduler) refreshModels() {
	if s.models == nil {
		return
	}
	if err := s.models.Refresh(s.client); err != nil {
		slog.Warn("定时刷新模型列表失败（已保留旧表）", "error", err)
	}
}

// ── 日志清理 ──

// cleanupLogs 清理过期日志：先按保留天数，再按累计大小上限删最旧。
func (s *Scheduler) cleanupLogs() {
	cfg := s.cfg.Get()
	days := cfg.LogRetentionDays
	n, err := s.st.CleanupLogs(days)
	if err != nil {
		slog.Error("清理日志失败", "error", err)
		return
	}
	if n > 0 {
		slog.Info("已清理过期日志", "count", n, "retention_days", days)
	}
	maxBytes := int64(cfg.LogMaxSizeMB) * 1024 * 1024
	if maxBytes <= 0 {
		return
	}
	m, err := s.st.CleanupLogsBySize(maxBytes)
	if err != nil {
		slog.Error("按大小清理日志失败", "error", err)
		return
	}
	if m > 0 {
		slog.Info("已按大小上限清理日志", "count", m, "max_mb", cfg.LogMaxSizeMB)
	}
}

// ── 号池探测 ──

// tickPoolProbe 每 6 小时探测一次全池账号（cron 由 pool_probe_cron 决定）。
//
// 每个号加 0–5 分钟随机抖动：4 个号散在 5 分钟窗口内，而不是同一秒一起打上游
// （billing 接口有频率限制，并发同一时刻更容易被限）。用 time.AfterFunc 而不是
// sleep 串行：串行 4×5=20 分钟会跨过下一个整点，与签到/成长撞车。
func (s *Scheduler) tickPoolProbe() {
	p := s.getPool()
	if p == nil {
		return
	}
	for _, uid := range p.Members() {
		uid := uid
		delay := time.Duration(rand.IntN(300)) * time.Second
		time.AfterFunc(delay, func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			res := p.Probe(ctx, uid)
			switch {
			case res.Skipped:
				// 手动禁用：不探测是正常的，不打日志噪音
			case res.OK:
				slog.Info("号池探测成功", "uid", uidTail(uid), "msg", res.Msg)
			default:
				slog.Warn("号池探测未恢复", "uid", uidTail(uid), "reason", res.Reason, "msg", res.Msg)
			}
		})
	}
}

// uidTail 日志里只打 uid 后缀（uid 是账号标识，完整打进日志没有额外价值）。
func uidTail(uid string) string {
	if len(uid) <= 8 {
		return uid
	}
	return uid[len(uid)-8:]
}
