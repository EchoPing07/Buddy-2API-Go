// growth.go 成长任务管理端点：聚合总览（60s 缓存）+ 8 个手动动作。
//
// 手动动作统一约定：
//   - 执行前清 overview 缓存（前端动作后再 force 刷新，总览立即新鲜）
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

	"buddy2api-go/internal/scheduler"
	"buddy2api-go/internal/upstream"
)

const (
	growthCacheTable   = "growth_cache"
	growthKeyOverview  = "overview_v1"
	growthKeyReportDay = "report_day"
	growthKeyAdoptDay  = "adopt_day"
	growthKeyRewardDay = "reward_day"
	growthKeyLastRun   = "last_run"
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
	ReportDone  bool `json:"report_done"`
	AdoptTried  bool `json:"adopt_tried"`
	RewardDone  bool `json:"reward_done"`
	ReportCount int  `json:"report_count"`
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
// info 为 nil、cellsOK/buddyOK 为 false、chances 为 nil 分别表示对应上游 GET 失败
// （计入 degraded）。buddyOK=false 与「真无猫」（buddyOK=true 且 buddy==nil）严格区分：
// 查询失败不得呈现为无猫空态，否则前端会诱导用户点击注定失败的领养写操作。
func buildGrowthOverview(now time.Time, reportCount int,
	info *upstream.GrowthStreakInfo, cells []upstream.HeatmapCell, cellsOK bool,
	buddy *upstream.Buddy, buddyOK bool, ts *upstream.TravelState, chances *int,
	reportDone, adoptTried, rewardDone bool, lastRun *scheduler.GrowthChainResult) growthOverview {

	ov := growthOverview{
		Available: true,
		UpdatedAt: now.Unix(),
		Today: growthOverviewToday{
			ReportDone: reportDone, AdoptTried: adoptTried,
			RewardDone: rewardDone, ReportCount: reportCount,
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
		// 旅行状态仅在有猫时才有意义（无猫时上游查询必然空，跳过不算降级）
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

// growthOverview GET /admin/growth/overview — 聚合总览（60s 缓存，?force=1 强刷）。
func (h *Handler) growthOverview(w http.ResponseWriter, r *http.Request) {
	cfg := h.cfg.Get()
	if cfg.Region != "cn" {
		jsonWrite(w, http.StatusOK, map[string]any{"available": false, "reason": "region_global"})
		return
	}
	if h.toks.Get() == nil {
		jsonWrite(w, http.StatusOK, map[string]any{"available": false, "reason": "not_logged_in"})
		return
	}
	force := r.URL.Query().Get("force") == "1"
	if !force {
		if payload, _, ok := h.st.GetCache(growthCacheTable, growthKeyOverview, growthOverviewTTL); ok {
			var parsed map[string]any
			if err := json.Unmarshal([]byte(payload), &parsed); err == nil {
				parsed["cached"] = true
				jsonWrite(w, http.StatusOK, parsed)
				return
			}
		}
	}

	now := time.Now()
	// 5 个上游 GET（region=cn 且已登录才到这里；单账号顺序拉取，总量可控）
	info, infoErr := h.growth.GrowthStreakInfo()
	if infoErr != nil {
		slog.Warn("growth overview: streak 拉取失败", "error", infoErr)
		info = nil
	}
	cells, cellsErr := h.growth.GrowthHeatmap()
	if cellsErr != nil {
		slog.Warn("growth overview: heatmap 拉取失败", "error", cellsErr)
		cells = nil
	}
	buddy, buddyErr := h.growth.BuddyInfo()
	if buddyErr != nil {
		slog.Warn("growth overview: buddy 拉取失败", "error", buddyErr)
		buddy = nil
	}
	var ts *upstream.TravelState
	if buddy != nil {
		ts, _ = h.growth.TravelStatus() // 失败计入 degraded
	}
	chances, chancesErr := h.growth.GrowthLotteryChances()
	if chancesErr != nil {
		slog.Warn("growth overview: lottery chances 拉取失败", "error", chancesErr)
	}

	// 今日标志（cache 镜像，TTL 48h）+ last_run（14d）
	day := upstream.GrowthTodayDate(now)
	flag := func(key string) bool {
		payload, _, ok := h.st.GetCache(growthCacheTable, key, growthDayTTL)
		return ok && payload == day
	}
	var lastRun *scheduler.GrowthChainResult
	if payload, _, ok := h.st.GetCache(growthCacheTable, growthKeyLastRun, growthLastRunTTL); ok {
		var lr scheduler.GrowthChainResult
		if err := json.Unmarshal([]byte(payload), &lr); err == nil {
			lastRun = &lr
		}
	}

	ov := buildGrowthOverview(now, cfg.GrowthReportCount, info, cells, cellsErr == nil,
		buddy, buddyErr == nil, ts, chancesPtr(chances, chancesErr),
		flag(growthKeyReportDay), flag(growthKeyAdoptDay), flag(growthKeyRewardDay), lastRun)

	if raw, err := json.Marshal(ov); err == nil {
		_ = h.st.SetCache(growthCacheTable, growthKeyOverview, string(raw))
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

// growthPrecheck 直接调上游类端点的前置检查；返回非空 reason 表示不可用。
func (h *Handler) growthPrecheck() string {
	if h.cfg.Get().Region != "cn" {
		return "region_global"
	}
	if h.toks.Get() == nil {
		return "not_logged_in"
	}
	return ""
}

// growthInvalidateOverview 清 overview 缓存（动作端点统一前置）。
func (h *Handler) growthInvalidateOverview() {
	_ = h.st.DeleteCacheKey(growthCacheTable, growthKeyOverview)
}

// growthRun POST /admin/growth/run — 手动执行完整链。
func (h *Handler) growthRun(w http.ResponseWriter, r *http.Request) {
	h.growthInvalidateOverview()
	res := h.sched.RunGrowthChain()
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
	count := req.Count
	if count <= 0 {
		count = h.cfg.Get().GrowthReportCount
	}
	if count > 10 {
		count = 10 // 钳制防风控
	}
	h.growthInvalidateOverview()
	res := h.sched.RunActivityReports(count)
	resp := map[string]any{"ok": res.Ran, "sent": res.Sent, "total": res.Total, "detail": res.Detail}
	if !res.Ran {
		resp["reason"] = res.Reason
	}
	jsonWrite(w, http.StatusOK, resp)
}

// growthAdopt POST /admin/growth/adopt — 手动领养（force 语义）。
func (h *Handler) growthAdopt(w http.ResponseWriter, r *http.Request) {
	h.growthInvalidateOverview()
	res := h.sched.RunGrowthAdopt()
	resp := map[string]any{"ok": res.Ran, "status": res.Status, "detail": res.Detail}
	if !res.Ran {
		resp["reason"] = res.Reason
	}
	jsonWrite(w, http.StatusOK, resp)
}

// growthTravel POST /admin/growth/travel — 手动旅行巡检（单动作状态机）。
func (h *Handler) growthTravel(w http.ResponseWriter, r *http.Request) {
	h.growthInvalidateOverview()
	res := h.sched.RunTravelTick()
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
	h.growthInvalidateOverview()
	res := h.sched.RunGrowthRedeem(tier)
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
// 不持 growthMu：与自动链并发时至多一次重复写被上游拒绝，分类为 skip
// （chances 余额递减天然幂等），链内互斥由 scheduler 侧负责。
func (h *Handler) growthLottery(w http.ResponseWriter, r *http.Request) {
	if reason := h.growthPrecheck(); reason != "" {
		jsonWrite(w, http.StatusOK, map[string]any{"ok": false, "reason": reason})
		return
	}
	h.growthInvalidateOverview()
	chances, err := h.growth.GrowthLotteryChances()
	if err != nil {
		jsonWrite(w, http.StatusOK, map[string]any{"ok": true, "status": "fail", "detail": "查询抽奖次数失败: " + err.Error()})
		return
	}
	if chances <= 0 {
		jsonWrite(w, http.StatusOK, map[string]any{"ok": true, "status": "skip", "chances_left": 0, "detail": "暂无抽奖次数"})
		return
	}
	draw, err := h.growth.GrowthLotteryDraw("") // 每次新 client_token
	switch {
	case err == nil:
		resp := map[string]any{"ok": true, "status": "ok", "prize": draw, "detail": "抽中 " + draw.PrizeName}
		// 抽后回查剩余次数（失败则按减一估算）
		if left, err2 := h.growth.GrowthLotteryChances(); err2 == nil {
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
// 不持 growthMu：与链内补签并发时第二个会被上游「已补过」业务错误拒绝
// （卡余额递减天然幂等）。
func (h *Handler) growthMakeup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Date string `json:"date"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if reason := h.growthPrecheck(); reason != "" {
		jsonWrite(w, http.StatusOK, map[string]any{"ok": false, "reason": reason})
		return
	}
	now := time.Now()
	date := strings.TrimSpace(req.Date)
	if date == "" {
		date = upstream.GrowthYesterdayDate(now)
	}
	// 校验：严格 YYYY-MM-DD（10 位日期串）且必须早于今日 CST（补签只对过去生效）
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
	h.growthInvalidateOverview()
	err = h.growth.UseMakeupCard(date)
	resp := map[string]any{"ok": true, "date": date}
	switch {
	case err == nil:
		resp["status"] = "ok"
		resp["detail"] = "补签 " + date + " 成功，连登保住"
	default:
		// 业务错误（无卡/无漏签/已补过）→ skip 展示上游文案；网络/5xx → fail
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
// 不持 growthMu：礼包/补偿为服务端每号一次幂等写，并发重复无害。
func (h *Handler) growthBonus(w http.ResponseWriter, r *http.Request) {
	if reason := h.growthPrecheck(); reason != "" {
		jsonWrite(w, http.StatusOK, map[string]any{"ok": false, "reason": reason})
		return
	}
	h.growthInvalidateOverview()
	gift, _ := h.growth.ClaimGift() // 业务错误（早已领过）静默计 0
	comp, _ := h.growth.ClaimCompensation()
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
