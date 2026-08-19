// Package scheduler 定时任务：自动签到（cron 6 段含秒）+ 日志清理。
package scheduler

import (
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"buddy2api-go/internal/config"
	"buddy2api-go/internal/store"
	"buddy2api-go/internal/upstream"
)

// Scheduler cron 调度器。
type Scheduler struct {
	mu        sync.Mutex
	c         *cron.Cron
	cfg       *config.Manager
	client    *upstream.Client
	st        *store.Store
	checkinID cron.EntryID
}

// New 创建调度器并启动日志清理任务（每天 03:00）。
func New(cfg *config.Manager, client *upstream.Client, st *store.Store) *Scheduler {
	parser := cron.NewParser(
		cron.Second | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor,
	)
	s := &Scheduler{c: cron.New(cron.WithParser(parser), cron.WithChain()), cfg: cfg, client: client, st: st}
	if _, err := s.c.AddFunc("0 0 3 * * *", s.cleanupLogs); err != nil {
		slog.Error("注册日志清理任务失败", "error", err)
	}
	s.Reconfigure()
	s.c.Start()
	return s
}

// Reconfigure 按配置重装配签到任务，仅当 auto_checkin 开启才注册。
func (s *Scheduler) Reconfigure() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.checkinID != 0 {
		s.c.Remove(s.checkinID)
		s.checkinID = 0
	}
	cfg := s.cfg.Get()
	if !cfg.AutoCheckin {
		return
	}
	id, err := s.c.AddFunc(cfg.CheckinCron, s.doCheckin)
	if err != nil {
		slog.Error("注册签到任务失败", "error", err, "cron", cfg.CheckinCron)
		return
	}
	s.checkinID = id
	slog.Info("自动签到已开启", "cron", cfg.CheckinCron)
}

// Stop 停止调度器。
func (s *Scheduler) Stop() { <-s.c.Stop().Done() }

// doCheckin 执行签到：已签则跳过，结果写 checkin_cache 并记日志。
func (s *Scheduler) doCheckin() {
	// 先查状态
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
				slog.Info("定时签到：今日已签到，跳过")
				return
			}
		}
	}
	res, err := s.client.ClaimCheckin()
	if err != nil {
		slog.Error("定时签到失败", "error", err)
		return
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
		slog.Info("定时签到成功", "credit", env.Data.Credit)
		_ = s.st.SetCache("checkin_cache", "status", string(res.Raw))
	} else {
		slog.Warn("定时签到未成功", "code", env.Code, "checked_in", env.Data.TodayCheckedIn)
	}
}

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

var _ = time.Now
