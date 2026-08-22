// Package scheduler 定时任务：自动签到状态机（cron 定时 / 时间范围随机 / 末班兜底）+ 日志清理。
package scheduler

import (
	"encoding/json"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"buddy2api-go/internal/config"
	"buddy2api-go/internal/store"
	"buddy2api-go/internal/upstream"
)

const (
	// checkinTickSpec 每分钟驱动一次签到状态机（随机时刻为分钟粒度，秒级无意义）。
	checkinTickSpec = "0 * * * * *"
	// checkinRetryDelay 一次尝试失败后的重试间隔；每个阶段只重试一次，再失败当天放弃。
	checkinRetryDelay = 5 * time.Minute
	// checkinFallbackMin 末班兜底时刻（当日 23:50），失败 23:55 重试一次后认栽。
	checkinFallbackMin = 23*60 + 50
	// randomTargetTTL 随机主时刻缓存有效期（跨日后失效重摇）。
	randomTargetTTL = 48 * 3600
)

// secondsParser 6 段含秒 cron 解析器（与 admin.validCron 一致），用于推算 fixed 模式当日触发时刻。
var secondsParser = cron.NewParser(
	cron.Second | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor,
)

// Scheduler cron 调度器。
type Scheduler struct {
	mu     sync.Mutex
	c      *cron.Cron
	cfg    *config.Manager
	client *upstream.Client
	st     *store.Store
	tickID cron.EntryID
	ck     checkinDay // 签到日内状态（mu 保护，跨日自动重置）
}

// New 创建调度器并启动日志清理任务（每天 03:00）。
func New(cfg *config.Manager, client *upstream.Client, st *store.Store) *Scheduler {
	s := &Scheduler{c: cron.New(cron.WithParser(secondsParser), cron.WithChain()), cfg: cfg, client: client, st: st}
	if _, err := s.c.AddFunc("0 0 3 * * *", s.cleanupLogs); err != nil {
		slog.Error("注册日志清理任务失败", "error", err)
	}
	s.Reconfigure()
	s.c.Start()
	return s
}

// Reconfigure 按配置重装配签到任务：仅当 auto_checkin 开启注册分钟 tick；日内状态下次 tick 重建。
func (s *Scheduler) Reconfigure() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tickID != 0 {
		s.c.Remove(s.tickID)
		s.tickID = 0
	}
	s.ck = checkinDay{}
	cfg := s.cfg.Get()
	if !cfg.AutoCheckin {
		return
	}
	id, err := s.c.AddFunc(checkinTickSpec, s.tickCheckin)
	if err != nil {
		slog.Error("注册签到任务失败", "error", err)
		return
	}
	s.tickID = id
	mode := cfg.CheckinMode
	if mode == "random" {
		mode += " " + cfg.CheckinRandomStart + "~" + cfg.CheckinRandomEnd
	}
	slog.Info("自动签到已开启", "mode", mode, "fallback", cfg.CheckinFallback)
}

// Stop 停止调度器。
func (s *Scheduler) Stop() { <-s.c.Stop().Done() }

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

// tickCheckin 每分钟驱动签到状态机：主时刻（含宕机补签）→ 失败 5 分钟重试一次
// → 23:50 末班兜底（同样只带一次重试）。所有尝试先查状态，已签（含手动）即落袋跳过。
func (s *Scheduler) tickCheckin() {
	now := time.Now()
	cfg := s.cfg.Get()

	s.mu.Lock()
	s.rollDayLocked(now, cfg)
	label, allowRetry := s.ck.decide(now, cfg.CheckinFallback, minuteOfDay(now, checkinFallbackMin))
	s.mu.Unlock()

	if label == "" {
		return
	}
	if s.attemptCheckin(label) {
		s.mu.Lock()
		s.ck.secured = true
		s.mu.Unlock()
		return
	}
	if allowRetry {
		s.mu.Lock()
		s.ck.retryAt, s.ck.retryTag = now.Add(checkinRetryDelay), label
		s.mu.Unlock()
	}
}

// rollDayLocked 跨日时重置状态并推算当日主时刻：
// random 复用/生成窗口内随机时刻，fixed 取 cron 当日触发时刻（当日无触发则留零值）。
func (s *Scheduler) rollDayLocked(now time.Time, cfg config.Config) {
	day := now.Format("2006-01-02")
	if s.ck.day == day {
		return
	}
	s.ck = checkinDay{day: day}
	if cfg.CheckinMode == "random" {
		s.ck.mainAt = s.randomMainAt(now, cfg)
		return
	}
	s.ck.mainAt = fixedMainAt(cfg.CheckinCron, now)
	if s.ck.mainAt.IsZero() {
		slog.Warn("签到 cron 当日无触发时刻", "cron", cfg.CheckinCron)
	}
}

// randomMainAt 取当日随机主时刻：优先复用缓存里今日已生成的（重启不重摇），否则窗口内随机并落缓存。
func (s *Scheduler) randomMainAt(now time.Time, cfg config.Config) time.Time {
	day := now.Format("2006-01-02")
	if payload, _, ok := s.st.GetCache("checkin_cache", "random_target", randomTargetTTL); ok {
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
	_ = s.st.SetCache("checkin_cache", "random_target", t.Format(time.RFC3339))
	slog.Info("已生成今日随机签到时刻", "at", t.Format("15:04"))
	return t
}

// randomMinute 在 [startMin, endMin) 内随机取一个分钟数。
func randomMinute(startMin, endMin int) int { return startMin + rand.IntN(endMin-startMin) }

// attemptCheckin 执行一次签到尝试：先查状态（已签则跳过），未签则领取；
// 返回当日是否已落袋（领取成功或早已签）。
func (s *Scheduler) attemptCheckin(label string) bool {
	if res, err := s.client.CheckinStatus(); err == nil {
		var env struct {
			Data struct {
				Active         bool `json:"active"`
				TodayCheckedIn bool `json:"today_checked_in"`
			} `json:"data"`
		}
		if json.Unmarshal(res.Raw, &env) == nil {
			_ = s.st.SetCache("checkin_cache", "status", string(res.Raw))
			if env.Data.TodayCheckedIn {
				slog.Info(label + "：今日已签到，跳过")
				return true
			}
		}
	}
	res, err := s.client.ClaimCheckin()
	if err != nil {
		slog.Error(label+"失败", "error", err)
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
	if env.Code == 0 && env.Data.Credit > 0 {
		slog.Info(label+"成功", "credit", env.Data.Credit)
		_ = s.st.SetCache("checkin_cache", "status", string(res.Raw))
		return true
	}
	slog.Warn(label+"未成功", "code", env.Code, "checked_in", env.Data.TodayCheckedIn)
	return false
}

// fixedMainAt 取 cron 当日触发时刻；表达式非法或当日无触发（如仅工作日）返回零值。
func fixedMainAt(cronExpr string, now time.Time) time.Time {
	sched, err := secondsParser.Parse(cronExpr)
	if err != nil {
		slog.Error("签到 cron 非法，今日仅剩末班兜底", "error", err, "cron", cronExpr)
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
