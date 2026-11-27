// growth.go 成长任务管理端点：聚合总览（60s 缓存）+ 8 个手动动作（均支持 ?uid= 多账号）。
//
// 手动动作统一约定：
//   - 执行前清目标账号 overview 缓存（前端动作后再 force 刷新，总览立即新鲜）
//   - scheduler 侧方法自带 region/登录/互斥前置，不可用时返回 {ok:false, reason}
//   - 直接调上游的端点（lottery/makeup/bonus）在 handler 层前置检查
package admin

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"buddy2api-go/internal/config"
	"buddy2api-go/internal/pool"
	"buddy2api-go/internal/scheduler"
	"buddy2api-go/internal/upstream"
)

const (
	growthCacheTable   = scheduler.GrowthCacheTable
	growthKeyOverview  = "overview_v1" // 旧版全局概览键（仅无 uid 兼容回退/清理）
	growthKeyReportDay = scheduler.GrowthKeyReportDay
	growthKeyAdoptDay  = scheduler.GrowthKeyAdoptDay
	growthKeyRewardDay = scheduler.GrowthKeyRewardDay
	growthKeyLastRun   = scheduler.GrowthKeyLastRun
	growthOverviewTTL  = 60 // overview 缓存秒数
	growthDayTTL       = 48 * 3600
	growthLastRunTTL   = 14 * 24 * 3600
)

// growthAPI 成长端点依赖的上游接口（*upstream.Client 实现；测试可注入 fake）。
// 单独抽接口仅为可测性：lottery/makeup/bonus handler 无需网络即可测。
type growthAPI interface {
	GrowthStreakInfo() (*upstream.GrowthStreakInfo, error)
	GrowthHeatmap() ([]upstream.HeatmapCell, error)
	BuddyInfo() (*upstream.Buddy, error)
	TravelStatus() (*upstream.TravelState, error)
	GrowthLotteryChances() (int, error)
	GrowthLotteryDraw(clientToken string) (*upstream.GrowthLotteryDrawResult, error)
	UseMakeupCard(targetDate string) error
	ClaimGift() (int64, error)
	ClaimCompensation() (int64, error)
}

var _ growthAPI = (*upstream.Client)(nil)

// ── 聚合总览 ──────────────────────────────────────────────────────────────────

// growthHeatDay 热力格单日视图。
type growthHeatDay struct {
	Date    string `json:"date"`
	Score   int    `json:"score"`
	Present bool   `json:"present"` // false = 无该日格（无判据）
}

type growthOverviewHeatmap struct {
	Cells     []upstream.HeatmapCell `json:"cells"`
	Yesterday *growthHeatDay         `json:"yesterday"`
	Today     *growthHeatDay         `json:"today"`
}

type growthTierView struct {
	Tier    string `json:"tier"`
	Days    int    `json:"days"`
	Credit  int    `json:"credit"`
	Energy  int    `json:"energy"`
	Cards   int    `json:"cards"`
	Chances int    `json:"chances"`
	Status  string `json:"status"` // claimed | available | locked
}

type growthMakeupCards struct {
	Balance int `json:"balance"`
	Max     int `json:"max"`
}

type growthOverviewStreak struct {
	Days          int               `json:"days"`
	RemainingDays int               `json:"remaining_days"`
	MakeupCards   growthMakeupCards `json:"makeup_cards"`
	Tiers         []growthTierView  `json:"tiers"`
}

type growthOverviewBuddy struct {
	Has  bool   `json:"has"`
	ID   int64  `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

type growthOverviewLottery struct {
	Chances int `json:"chances"`
}

type growthOverviewToday struct {
	ReportDone   bool `json:"report_done"`
	AdoptTried   bool `json:"adopt_tried"`
	RewardDone   bool `json:"reward_done"`
	ReportCount  int  `json:"report_count"`  // 配置基准条数
	ReportJitter int  `json:"report_jitter"` // 波动幅度 ±N（0=固定）
}

// growthOverview 成长总览响应（前端「任务」视图唯一数据入口）。
type growthOverview struct {
	Available bool                         `json:"available"`
	Reason    string                       `json:"reason,omitempty"`
	Cached    bool                         `json:"cached"`
	UpdatedAt int64                        `json:"updated_at"`
	Streak    *growthOverviewStreak        `json:"streak"`
	Heatmap   *growthOverviewHeatmap       `json:"heatmap"`
	Buddy     growthOverviewBuddy          `json:"buddy"`
	Travel    *upstream.TravelState        `json:"travel"`
	Lottery   *growthOverviewLottery       `json:"lottery"`
	Today     growthOverviewToday          `json:"today"`
	LastRun   *scheduler.GrowthChainResult `json:"last_run"`
	Degraded  []string                     `json:"degraded"`
}

// buildGrowthOverview 聚合各上游切片为总览（纯函数，便于单测）。
func buildGrowthOverview(now time.Time, reportCount, reportJitter int,
	info *upstream.GrowthStreakInfo, cells []upstream.HeatmapCell, cellsOK bool,
	buddy *upstream.Buddy, buddyOK bool, ts *upstream.TravelState, chances *int,
	reportDone, adoptTried, rewardDone bool, lastRun *scheduler.GrowthChainResult) growthOverview {

	ov := growthOverview{
		Available: true,
		UpdatedAt: now.Unix(),
		Today: growthOverviewToday{
			ReportDone: reportDone, AdoptTried: adoptTried,
			RewardDone: rewardDone, ReportCount: reportCount, ReportJitter: reportJitter,
		},
		LastRun:  lastRun,
		Degraded: []string{},
	}

	if info != nil {
		st := growthOverviewStreak{
			Days:          info.Streak.Days,
			RemainingDays: info.Redemption.RemainingDays,
			MakeupCards:   growthMakeupCards{Balance: info.MakeupCards.Balance, Max: info.MakeupCards.Max},
			Tiers:         make([]growthTierView, 0, len(info.Redemption.Tiers)),
		}
		for _, sp := range info.Redemption.Tiers {
			v := growthTierView{Tier: sp.Tier, Days: sp.Days, Credit: sp.Credit, Energy: sp.Energy, Cards: sp.Cards, Chances: sp.Chances}
			switch {
			case info.Claimed(sp.Tier):
				v.Status = "claimed"
			case info.Streak.Days >= sp.Days:
				v.Status = "available"
			default:
				v.Status = "locked"
			}
			st.Tiers = append(st.Tiers, v)
		}
		ov.Streak = &st
	} else {
		ov.Degraded = append(ov.Degraded, "streak")
	}

	if cellsOK {
		hm := growthOverviewHeatmap{Cells: cells}
		yd := upstream.GrowthYesterdayDate(now)
		td := upstream.GrowthTodayDate(now)
		if score, ok := upstream.HeatmapDayScore(cells, yd); ok {
			hm.Yesterday = &growthHeatDay{Date: yd, Score: score, Present: true}
		} else {
			hm.Yesterday = &growthHeatDay{Date: yd, Present: false}
		}
		if score, ok := upstream.HeatmapDayScore(cells, td); ok {
			hm.Today = &growthHeatDay{Date: td, Score: score, Present: true}
		} else {
			hm.Today = &growthHeatDay{Date: td, Present: false}
		}
		ov.Heatmap = &hm
	} else {
		ov.Degraded = append(ov.Degraded, "heatmap")
	}

	if !buddyOK {
		ov.Degraded = append(ov.Degraded, "buddy") // 查询失败 ≠ 无猫
	} else if buddy != nil {
		ov.Buddy = growthOverviewBuddy{Has: true, ID: buddy.ID, Name: buddy.Name}
		if ts != nil {
			ov.Travel = ts
		} else {
			ov.Degraded = append(ov.Degraded, "travel")
		}
	} else {
		ov.Buddy = growthOverviewBuddy{Has: false}
	}

	if chances != nil {
		ov.Lottery = &growthOverviewLottery{Chances: *chances}
	} else {
		ov.Degraded = append(ov.Degraded, "lottery")
	}
	return ov
}

// resolveGrowthTarget 解析 ?uid= 目标账号：
//   - rawUID==""：走主账号（兼容旧行为与单测 fakeGrowthAPI）；
//   - rawUID!=""：校验属于号池（否则 httpStatus=404）、CN 端点匹配且非凭证失效。
func (h *Handler) resolveGrowthTarget(rawUID string) (effectiveUID string, g growthAPI, httpStatus int, reason string) {
	rawUID = strings.TrimSpace(rawUID)
	cfg := h.cfg.Get()
	cnDomain := config.EndpointOf("cn").Domain

	if rawUID == "" {
		t := h.toks.Get()
		if t != nil {
			effectiveUID = t.UID
		}
		if cfg.Region != "cn" {
			return effectiveUID, nil, 0, "region_global"
		}
		if t == nil {
			return "", nil, 0, "not_logged_in"
		}
		if !regionMatches(t.Domain, cnDomain) {
			return effectiveUID, nil, 0, "region_global"
		}
		g = h.growth
		if g == nil {
			g = h.client
		}
		return effectiveUID, g, 0, ""
	}

	targetUID, cli, tok, found := h.resolveUIDClient(rawUID)
	if !found {
		return "", nil, http.StatusNotFound, "账号不存在"
	}
	if cfg.Region != "cn" || !regionMatches(tok.Domain, cnDomain) {
		return targetUID, nil, 0, "region_global"
	}
	if h.pool != nil {
		if st, ok := h.pool.AccountStateOf(targetUID); ok && st.AutoDisabled == pool.AutoCredential {
			return targetUID, nil, 0, "credential_disabled"
		}
	}
	active := h.toks.Get()
	if active != nil && active.UID == targetUID && h.growth != nil {
		g = h.growth
	} else {
		g = cli
	}
	return targetUID, g, 0, ""
}

// growthOverview GET /admin/growth/overview — 聚合总览（60s 缓存，?force=1 强刷，?uid= 指定账号）。
func (h *Handler) growthOverview(w http.ResponseWriter, r *http.Request) {
	rawUID := strings.TrimSpace(r.URL.Query().Get("uid"))
	effectiveUID, g, status, reason := h.resolveGrowthTarget(rawUID)
	if status != 0 {
		jsonErr(w, status, reason)
		return
	}
	if reason != "" {
		jsonWrite(w, http.StatusOK, map[string]any{"available": false, "reason": reason})
		return
	}

	cfg := h.cfg.Get()
	force := r.URL.Query().Get("force") == "1"
	ovKey := scheduler.GrowthOverviewKey(effectiveUID)
	if !force {
		if payload, _, ok := h.st.GetCache(growthCacheTable, ovKey, growthOverviewTTL); ok {
			var parsed map[string]any
			if err := json.Unmarshal([]byte(payload), &parsed); err == nil {
				parsed["cached"] = true
				jsonWrite(w, http.StatusOK, parsed)
				return
			}
		}
		if rawUID == "" {
			if payload, _, ok := h.st.GetCache(growthCacheTable, growthKeyOverview, growthOverviewTTL); ok {
				var parsed map[string]any
				if err := json.Unmarshal([]byte(payload), &parsed); err == nil {
					parsed["cached"] = true
					jsonWrite(w, http.StatusOK, parsed)
					return
				}
			}
		}
	}

	now := time.Now()
	info, infoErr := g.GrowthStreakInfo()
	if infoErr != nil {
		slog.Warn("growth overview: streak 拉取失败", "uid", effectiveUID, "error", infoErr)
		info = nil
	}
	cells, cellsErr := g.GrowthHeatmap()
	if cellsErr != nil {
		slog.Warn("growth overview: heatmap 拉取失败", "uid", effectiveUID, "error", cellsErr)
		cells = nil
	}
	buddy, buddyErr := g.BuddyInfo()
	if buddyErr != nil {
		slog.Warn("growth overview: buddy 拉取失败", "uid", effectiveUID, "error", buddyErr)
		buddy = nil
	}
	var ts *upstream.TravelState
	if buddy != nil {
		ts, _ = g.TravelStatus()
	}
	chances, chancesErr := g.GrowthLotteryChances()
	if chancesErr != nil {
		slog.Warn("growth overview: lottery chances 拉取失败", "uid", effectiveUID, "error", chancesErr)
	}

	day := upstream.GrowthTodayDate(now)
	flag := func(key, legacyKey string) bool {
		if payload, _, ok := h.st.GetCache(growthCacheTable, key, growthDayTTL); ok && payload == day {
			return true
		}
		if rawUID == "" && legacyKey != key {
			if payload, _, ok := h.st.GetCache(growthCacheTable, legacyKey, growthDayTTL); ok && payload == day {
				return true
			}
		}
		return false
	}
	var lastRun *scheduler.GrowthChainResult
	if payload, _, ok := h.st.GetCache(growthCacheTable, scheduler.GrowthLastRunKey(effectiveUID), growthLastRunTTL); ok {
		var lr scheduler.GrowthChainResult
		if err := json.Unmarshal([]byte(payload), &lr); err == nil {
			lastRun = &lr
		}
	} else if rawUID == "" {
		if payload, _, ok := h.st.GetCache(growthCacheTable, growthKeyLastRun, growthLastRunTTL); ok {
			var lr scheduler.GrowthChainResult
			if err := json.Unmarshal([]byte(payload), &lr); err == nil {
				lastRun = &lr
			}
		}
	}

	ov := buildGrowthOverview(now, cfg.GrowthReportCount, cfg.GrowthReportJitter, info, cells, cellsErr == nil,
		buddy, buddyErr == nil, ts, chancesPtr(chances, chancesErr),
		flag(scheduler.GrowthReportDayKey(effectiveUID), growthKeyReportDay),
		flag(scheduler.GrowthAdoptDayKey(effectiveUID), growthKeyAdoptDay),
		flag(scheduler.GrowthRewardDayKey(effectiveUID), growthKeyRewardDay),
		lastRun)

	if raw, err := json.Marshal(ov); err == nil {
		_ = h.st.SetCache(growthCacheTable, ovKey, string(raw))
	}
	jsonWrite(w, http.StatusOK, ov)
}

// chancesPtr 把 (值, 错误) 转为指针（错误 → nil = degraded）。
func chancesPtr(v int, err error) *int {
	if err != nil {
		return nil
	}
	return &v
}

// ── 动作端点 ─────────────────────────────────────────────────────────────────

// growthInvalidateOverview 清指定账号的 overview 缓存（动作端点统一前置）。
func (h *Handler) growthInvalidateOverview(rawUID, effectiveUID string) {
	_ = h.st.DeleteCacheKey(growthCacheTable, scheduler.GrowthOverviewKey(effectiveUID))
	if rawUID == "" {
		_ = h.st.DeleteCacheKey(growthCacheTable, growthKeyOverview)
	}
}

// prepareGrowthSchedAction 校验 scheduler 类动作（run/report/adopt/travel/redeem）的目标账号并清概览缓存。
func (h *Handler) prepareGrowthSchedAction(w http.ResponseWriter, r *http.Request) (rawUID, effectiveUID string, ok bool) {
	rawUID = strings.TrimSpace(r.URL.Query().Get("uid"))
	if rawUID == "" {
		if t := h.toks.Get(); t != nil {
			effectiveUID = t.UID
		}
		h.growthInvalidateOverview(rawUID, effectiveUID)
		return rawUID, effectiveUID, true
	}
	var status int
	var reason string
	effectiveUID, _, status, reason = h.resolveGrowthTarget(rawUID)
	if status != 0 {
		jsonErr(w, status, reason)
		return "", "", false
	}
	if reason != "" {
		jsonWrite(w, http.StatusOK, map[string]any{"ok": false, "reason": reason})
		return "", "", false
	}
	h.growthInvalidateOverview(rawUID, effectiveUID)
	return rawUID, effectiveUID, true
}

// growthRun POST /admin/growth/run — 手动执行完整链。
func (h *Handler) growthRun(w http.ResponseWriter, r *http.Request) {
	rawUID, effectiveUID, ok := h.prepareGrowthSchedAction(w, r)
	if !ok {
		return
	}
	var res scheduler.GrowthChainResult
	if rawUID != "" {
		if gf, has := h.sched.(growthForScheduler); has {
			res = gf.RunGrowthChainFor(effectiveUID)
		} else {
			res = h.sched.RunGrowthChain()
		}
	} else {
		res = h.sched.RunGrowthChain()
	}
	resp := map[string]any{"ok": res.Ran, "result": res}
	if !res.Ran {
		resp["reason"] = res.Reason
	}
	jsonWrite(w, http.StatusOK, resp)
}

// growthReport POST /admin/growth/report — 手动活跃上报 {count?}（缺省取配置）。
func (h *Handler) growthReport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Count int `json:"count"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	rawUID, effectiveUID, ok := h.prepareGrowthSchedAction(w, r)
	if !ok {
		return
	}
	count := req.Count
	if count > 10 {
		count = 10 // 钳制防风控；0/缺省交由 scheduler 按配置+jitter 波动
	}
	var res scheduler.ActivityReportResult
	if rawUID != "" {
		if gf, has := h.sched.(growthForScheduler); has {
			res = gf.RunActivityReportsFor(effectiveUID, count)
		} else {
			res = h.sched.RunActivityReports(count)
		}
	} else {
		res = h.sched.RunActivityReports(count)
	}
	resp := map[string]any{"ok": res.Ran, "sent": res.Sent, "total": res.Total, "detail": res.Detail}
	if !res.Ran {
		resp["reason"] = res.Reason
	}
	jsonWrite(w, http.StatusOK, resp)
}

// growthAdopt POST /admin/growth/adopt — 手动领养（force 语义）。
func (h *Handler) growthAdopt(w http.ResponseWriter, r *http.Request) {
	rawUID, effectiveUID, ok := h.prepareGrowthSchedAction(w, r)
	if !ok {
		return
	}
	var res scheduler.GrowthAdoptResult
	if rawUID != "" {
		if gf, has := h.sched.(growthForScheduler); has {
			res = gf.RunGrowthAdoptFor(effectiveUID)
		} else {
			res = h.sched.RunGrowthAdopt()
		}
	} else {
		res = h.sched.RunGrowthAdopt()
	}
	resp := map[string]any{"ok": res.Ran, "status": res.Status, "detail": res.Detail}
	if !res.Ran {
		resp["reason"] = res.Reason
	}
	jsonWrite(w, http.StatusOK, resp)
}

// growthTravel POST /admin/growth/travel — 手动旅行巡检（单动作状态机）。
func (h *Handler) growthTravel(w http.ResponseWriter, r *http.Request) {
	rawUID, effectiveUID, ok := h.prepareGrowthSchedAction(w, r)
	if !ok {
		return
	}
	var res scheduler.TravelTickResult
	if rawUID != "" {
		if gf, has := h.sched.(growthForScheduler); has {
			res = gf.RunTravelTickFor(effectiveUID)
		} else {
			res = h.sched.RunTravelTick()
		}
	} else {
		res = h.sched.RunTravelTick()
	}
	resp := map[string]any{"ok": res.Ran, "result": res}
	if !res.Ran {
		resp["reason"] = res.Reason
	}
	jsonWrite(w, http.StatusOK, resp)
}

// growthRedeem POST /admin/growth/redeem — 手动兑换 {tier}。
func (h *Handler) growthRedeem(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tier string `json:"tier"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	tier := strings.TrimSpace(req.Tier)
	if tier != "7d" && tier != "14d" && tier != "28d" {
		jsonErr(w, http.StatusBadRequest, "tier 仅支持 7d/14d/28d")
		return
	}
	rawUID, effectiveUID, ok := h.prepareGrowthSchedAction(w, r)
	if !ok {
		return
	}
	var res scheduler.GrowthRedeemActionResult
	if rawUID != "" {
		if gf, has := h.sched.(growthForScheduler); has {
			res = gf.RunGrowthRedeemFor(effectiveUID, tier)
		} else {
			res = h.sched.RunGrowthRedeem(tier)
		}
	} else {
		res = h.sched.RunGrowthRedeem(tier)
	}
	resp := map[string]any{"ok": res.Ran, "status": res.Status, "detail": res.Detail}
	if res.Granted != nil {
		resp["granted"] = res.Granted
	}
	if !res.Ran {
		resp["reason"] = res.Reason
	}
	jsonWrite(w, http.StatusOK, resp)
}

// growthLottery POST /admin/growth/lottery — 手动抽奖（一次）。
func (h *Handler) growthLottery(w http.ResponseWriter, r *http.Request) {
	rawUID := strings.TrimSpace(r.URL.Query().Get("uid"))
	effectiveUID, g, status, reason := h.resolveGrowthTarget(rawUID)
	if status != 0 {
		jsonErr(w, status, reason)
		return
	}
	if reason != "" {
		jsonWrite(w, http.StatusOK, map[string]any{"ok": false, "reason": reason})
		return
	}
	h.growthInvalidateOverview(rawUID, effectiveUID)
	chances, err := g.GrowthLotteryChances()
	if err != nil {
		jsonWrite(w, http.StatusOK, map[string]any{"ok": true, "status": "fail", "detail": "查询抽奖次数失败: " + err.Error()})
		return
	}
	if chances <= 0 {
		jsonWrite(w, http.StatusOK, map[string]any{"ok": true, "status": "skip", "chances_left": 0, "detail": "暂无抽奖次数"})
		return
	}
	draw, err := g.GrowthLotteryDraw("")
	switch {
	case err == nil:
		resp := map[string]any{"ok": true, "status": "ok", "prize": draw, "detail": "抽中 " + draw.PrizeName}
		if left, err2 := g.GrowthLotteryChances(); err2 == nil {
			resp["chances_left"] = left
		} else {
			resp["chances_left"] = chances - 1
		}
		jsonWrite(w, http.StatusOK, resp)
	case upstream.IsLotteryNoChance(err) || upstream.IsLotteryDisabled(err):
		jsonWrite(w, http.StatusOK, map[string]any{"ok": true, "status": "skip", "chances_left": 0, "detail": "无抽奖次数或抽奖未开启"})
	default:
		jsonWrite(w, http.StatusOK, map[string]any{"ok": true, "status": "fail", "detail": "抽奖失败: " + err.Error()})
	}
}

// growthMakeup POST /admin/growth/makeup — 手动补签 {date?}（缺省昨日 CST）。
func (h *Handler) growthMakeup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Date string `json:"date"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	rawUID := strings.TrimSpace(r.URL.Query().Get("uid"))
	effectiveUID, g, status, reason := h.resolveGrowthTarget(rawUID)
	if status != 0 {
		jsonErr(w, status, reason)
		return
	}
	if reason != "" {
		jsonWrite(w, http.StatusOK, map[string]any{"ok": false, "reason": reason})
		return
	}
	now := time.Now()
	date := strings.TrimSpace(req.Date)
	if date == "" {
		date = upstream.GrowthYesterdayDate(now)
	}
	if len(date) != 10 {
		jsonErr(w, http.StatusBadRequest, "date 格式应为 YYYY-MM-DD")
		return
	}
	t, err := time.ParseInLocation("2006-01-02", date, upstream.CSTShanghai())
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "date 格式应为 YYYY-MM-DD")
		return
	}
	if t.Format("2006-01-02") != date {
		jsonErr(w, http.StatusBadRequest, "date 不是合法日期")
		return
	}
	if date >= upstream.GrowthTodayDate(now) {
		jsonErr(w, http.StatusBadRequest, "date 不得是今日或未来（补签仅对过去日期生效）")
		return
	}
	h.growthInvalidateOverview(rawUID, effectiveUID)
	err = g.UseMakeupCard(date)
	resp := map[string]any{"ok": true, "date": date}
	switch {
	case err == nil:
		resp["status"] = "ok"
		resp["detail"] = "补签 " + date + " 成功，连登保住"
	default:
		var ge *upstream.GrowthError
		if errors.As(err, &ge) && ge.Status < 500 {
			resp["status"] = "skip"
			resp["detail"] = "补签未生效: " + ge.Msg
		} else {
			resp["status"] = "fail"
			resp["detail"] = "补签失败: " + err.Error()
		}
	}
	jsonWrite(w, http.StatusOK, resp)
}

// growthBonus POST /admin/growth/bonus — 手动领礼包+补偿（两个都试，静默已领）。
func (h *Handler) growthBonus(w http.ResponseWriter, r *http.Request) {
	rawUID := strings.TrimSpace(r.URL.Query().Get("uid"))
	effectiveUID, g, status, reason := h.resolveGrowthTarget(rawUID)
	if status != 0 {
		jsonErr(w, status, reason)
		return
	}
	if reason != "" {
		jsonWrite(w, http.StatusOK, map[string]any{"ok": false, "reason": reason})
		return
	}
	h.growthInvalidateOverview(rawUID, effectiveUID)
	gift, _ := g.ClaimGift()
	comp, _ := g.ClaimCompensation()
	detail := ""
	if gift > 0 {
		detail = "礼包 +" + itoa64(gift) + " 积分"
	}
	if comp > 0 {
		if detail != "" {
			detail += " / "
		}
		detail += "补偿 +" + itoa64(comp) + " 积分"
	}
	if detail == "" {
		detail = "暂无可领取（早已领过或无补偿）"
	}
	jsonWrite(w, http.StatusOK, map[string]any{"ok": true, "gift": gift, "compensation": comp, "detail": detail})
}

func itoa64(n int64) string {
	return strconv.FormatInt(n, 10)
}
