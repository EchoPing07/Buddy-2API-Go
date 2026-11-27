package admin

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"buddy2api-go/internal/apikey"
	"buddy2api-go/internal/auth"
	"buddy2api-go/internal/billing"
	"buddy2api-go/internal/config"
	"buddy2api-go/internal/pool"
	"buddy2api-go/internal/scheduler"
	"buddy2api-go/internal/store"
	"buddy2api-go/internal/taskops"
	"buddy2api-go/internal/upstream"
)

// SchedulerControl 由 scheduler 实现：设置变更后重装配定时任务 + 成长任务链手动触发。
type SchedulerControl interface {
	Reconfigure()
	// ResetAccountState 换账号后复位旧版无后缀日内状态（内存 + 成长日标志/last_run 镜像）。
	// 现已全面按 UID 隔离缓存与日内状态，不会清除其他账号的数据。
	ResetAccountState()
	RunGrowthChain() scheduler.GrowthChainResult
	RunTravelTick() scheduler.TravelTickResult
	RunActivityReports(count int) scheduler.ActivityReportResult
	RunGrowthAdopt() scheduler.GrowthAdoptResult
	RunGrowthRedeem(tier string) scheduler.GrowthRedeemActionResult
}

// taskRunnerProvider scheduler 可选实现：暴露共享的任务执行器（单一互斥锁）。
// 单独成接口而不并进 SchedulerControl，是为了不强制所有 fake 实现新方法。
type taskRunnerProvider interface {
	Runner() *taskops.Runner
}

// checkinForScheduler scheduler 可选实现：多账号签到枚举与单号签到。
type checkinForScheduler interface {
	EligibleAccountUIDs() []string
	ClaimCheckinFor(uid string) (credit float64, alreadyChecked bool, err error)
}

// growthForScheduler scheduler 可选实现：按 UID 执行成长任务链与单项动作。
type growthForScheduler interface {
	RunGrowthChainFor(uid string) scheduler.GrowthChainResult
	RunTravelTickFor(uid string) scheduler.TravelTickResult
	RunActivityReportsFor(uid string, count int) scheduler.ActivityReportResult
	RunGrowthAdoptFor(uid string) scheduler.GrowthAdoptResult
	RunGrowthRedeemFor(uid string, tier string) scheduler.GrowthRedeemActionResult
}

type uidRunnerEntry struct {
	ts     *auth.TokenStore
	runner *taskops.Runner
}

// Handler 管理后台 API 处理器。
type Handler struct {
	cfg     *config.Manager
	st      *store.Store
	toks    *auth.TokenStore
	client  *upstream.Client
	growth  growthAPI // 成长端点依赖（生产为 client 本体；测试可注入 fake）
	models  *upstream.ModelCache
	keys    *apikey.Manager
	sched   SchedulerControl
	session *Session
	tasks   *taskops.Runner // 主账号任务动作执行器（New() 中创建，见 tasks.go）

	uidRunnersMu sync.Mutex
	uidRunners   map[string]uidRunnerEntry // 非主账号 per-uid 任务执行器

	// pool 多账号号池管理器（原生常驻）。
	pool *pool.Manager
}

// New 创建处理器。
func New(cfg *config.Manager, st *store.Store, toks *auth.TokenStore, client *upstream.Client,
	models *upstream.ModelCache, keys *apikey.Manager, sched SchedulerControl, session *Session) *Handler {
	return &Handler{cfg: cfg, st: st, toks: toks, client: client, growth: client, models: models,
		keys: keys, sched: sched, session: session,
		// 复用 scheduler 的执行器（单一实例 = 单一互斥锁），避免面板一键完成与
		// 夜猫子 cron 并发跑两轮真实对话。sched 为 nil 的测试路径才自建。
		tasks: runnerOrDefault(sched, client)}
}

// Routes 挂载 /admin 路由。
func (h *Handler) Routes() func(chi.Router) {
	return func(r chi.Router) {
		r.Post("/login", h.login)
		r.Post("/logout", h.logout)
		r.Get("/session", h.sessionCheck)

		r.Group(func(r chi.Router) {
			r.Use(h.requireSession)
			r.Get("/account", h.account)
			r.Post("/account/oauth/start", h.oauthStart)
			r.Get("/account/oauth/poll", h.oauthPoll)
			r.Post("/account/refresh", h.accountRefresh)
			r.Post("/account/test", h.accountTest)
			r.Delete("/account", h.accountDelete)
			r.Get("/account/export", h.accountExport)
			r.Post("/account/import", h.accountImport)
			r.Post("/account/import/batch", h.accountImportBatch)
			// 多账号档案：手动切换到某个已存账号（无轮询 / 无故障转移）。
			r.Get("/accounts", h.listAccounts)
			r.Put("/accounts/{uid}", h.updateAccountProfile)
			r.Post("/accounts/{uid}/activate", h.activateAccount)
			r.Delete("/accounts/{uid}", h.deleteAccountProfile)

			r.Get("/resources", h.resources)
			r.Get("/checkin/status", h.checkinStatus)
			r.Post("/checkin/claim", h.checkinClaim)
			r.Get("/checkin/summary", h.checkinSummary)
			r.Post("/checkin/claim_all", h.checkinClaimAll)

			// ── 成长任务（列表 / 接受 / 领奖 / 一键完成 / 扫描 / 补领清扫 / 补偿）──
			r.Get("/tasks", h.tasksList)
			r.Post("/tasks/accept", h.tasksAccept)
			r.Post("/tasks/claim", h.tasksClaim)
			r.Post("/tasks/auto", h.tasksAuto)
			r.Post("/tasks/auto_all", h.tasksAutoAll)
			r.Get("/tasks/scan", h.tasksScan)
			r.Post("/tasks/sweep", h.tasksSweep)
			r.Get("/tasks/actions", h.tasksActions)
			r.Get("/compensation", h.compensation)
			r.Post("/compensation/claim", h.compensationClaim)

			r.Get("/growth/overview", h.growthOverview)
			r.Post("/growth/run", h.growthRun)
			r.Post("/growth/report", h.growthReport)
			r.Post("/growth/adopt", h.growthAdopt)
			r.Post("/growth/travel", h.growthTravel)
			r.Post("/growth/redeem", h.growthRedeem)
			r.Post("/growth/lottery", h.growthLottery)
			r.Post("/growth/makeup", h.growthMakeup)
			r.Post("/growth/bonus", h.growthBonus)

			r.Get("/api-keys", h.listKeys)
			r.Post("/api-keys", h.createKey)
			r.Put("/api-keys/{id}", h.updateKey)
			r.Delete("/api-keys/{id}", h.deleteKey)

			r.Get("/logs", h.logs)
			r.Get("/stats", h.stats)

			r.Get("/settings", h.getSettings)
			r.Put("/settings", h.putSettings)
			r.Get("/experiments", h.getExperiments)
			r.Put("/experiments", h.putExperiments)

			// ── 多账号调度 ──
			r.Get("/pool", h.poolView)
			r.Put("/pool/strategy", h.poolSetStrategy)
			r.Post("/pool/accounts/{uid}/disable", h.poolDisable)
			r.Post("/pool/accounts/{uid}/enable", h.poolEnable)
			r.Post("/pool/accounts/{uid}/probe", h.poolProbe)
			r.Post("/pool/probe_all", h.poolProbeAll)
			r.Post("/pool/cool/{uid}/{model}/clear", h.poolClearCool)
			r.Post("/pool/reset", h.poolReset)
			r.Get("/models", h.listModels)
			r.Post("/models/refresh", h.refreshModels)
		})
	}
}

// runnerOrDefault 取 scheduler 的共享执行器；没有 scheduler（单测）时自建一个。
func runnerOrDefault(sched SchedulerControl, client *upstream.Client) *taskops.Runner {
	if p, ok := sched.(taskRunnerProvider); ok {
		if r := p.Runner(); r != nil {
			return r
		}
	}
	return taskops.NewRunner(client)
}

// ── util ──

func jsonWrite(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func jsonErr(w http.ResponseWriter, status int, msg string) {
	jsonWrite(w, status, map[string]any{"error": msg})
}

func readJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "读取请求体失败")
		return false
	}
	if len(raw) == 0 {
		return true // 空体
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		jsonErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return false
	}
	return true
}

func (h *Handler) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !h.session.Valid(r) {
			jsonErr(w, http.StatusUnauthorized, "未登录或会话已过期")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ── 登录 ──

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Password == "" || !h.cfg.VerifyAdminPassword(req.Password) {
		time.Sleep(500 * time.Millisecond) // 减缓爆破
		jsonErr(w, http.StatusUnauthorized, "密码错误")
		return
	}
	h.session.Issue(w, r)
	jsonWrite(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	h.session.Clear(w, r)
	jsonWrite(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) sessionCheck(w http.ResponseWriter, r *http.Request) {
	jsonWrite(w, http.StatusOK, map[string]any{"logged_in": h.session.Valid(r)})
}

// ── 账号 ──

// invalidateAccountCaches 切换活动账号后清理旧版无后缀缓存与调度器残留状态。
//
// 现已全面按 UID 隔离余额/签到/成长缓存与日内状态，因此换号不再清其他账号的 scoped 缓存；
// 此处仅清理历史残留的无后缀键并通知调度器重装配。
func (h *Handler) invalidateAccountCaches() {
	del := func(table, key string) {
		if err := h.st.DeleteCacheKey(table, key); err != nil {
			slog.Warn("换账号清理缓存失败", "table", table, "key", key, "error", err)
		}
	}
	for _, k := range resourceCacheStaleKeys {
		del("resource_cache", k)
	}
	del(scheduler.CheckinCacheTable, scheduler.CheckinKeyStatus)
	del(scheduler.GrowthCacheTable, growthKeyOverview)
	del(scheduler.GrowthCacheTable, scheduler.GrowthKeyOverview)
	if h.sched != nil {
		h.sched.ResetAccountState()
		h.sched.Reconfigure()
	}
}

// clearAccountCaches 删除指定账号的全部 per-uid 缓存，并释放其非主账号 runner。
// includeRandomTarget=true 时连带清除当日随机签到时刻（删除账号档案用）；
// 退出登录主账号时保留 random_target（同日重登不重摇）。
func (h *Handler) clearAccountCaches(uid string, includeRandomTarget bool) {
	if uid == "" {
		return
	}
	del := func(table, key string) {
		if err := h.st.DeleteCacheKey(table, key); err != nil {
			slog.Warn("清理账号缓存失败", "uid", uid, "table", table, "key", key, "error", err)
		}
	}
	del("resource_cache", resourceCacheKey(uid))
	del(scheduler.CheckinCacheTable, scheduler.CheckinStatusKey(uid))
	if includeRandomTarget {
		del(scheduler.CheckinCacheTable, scheduler.CheckinRandomTargetKey(uid))
	}
	del(scheduler.GrowthCacheTable, scheduler.GrowthOverviewKey(uid))
	del(scheduler.GrowthCacheTable, scheduler.GrowthReportDayKey(uid))
	del(scheduler.GrowthCacheTable, scheduler.GrowthAdoptDayKey(uid))
	del(scheduler.GrowthCacheTable, scheduler.GrowthRewardDayKey(uid))
	del(scheduler.GrowthCacheTable, scheduler.GrowthLastRunKey(uid))
	del(scheduler.GrowthCacheTable, scheduler.TaskLastSweepKey(uid))
	del(scheduler.GrowthCacheTable, scheduler.TaskLastQueueKey(uid))

	h.uidRunnersMu.Lock()
	delete(h.uidRunners, uid)
	h.uidRunnersMu.Unlock()
}

// regionMatches 与 pool.regionMatches 保持一致：空域视为兼容匹配。
func regionMatches(domain, regionDomain string) bool {
	return domain == "" || domain == regionDomain
}

// accountDisplayLabel 返回账号展示名：优先 remark，其次 nickname，回退 uid 后 8 位。
func accountDisplayLabel(t *auth.Token) string {
	if t == nil {
		return "未命名"
	}
	if t.Remark != "" {
		return t.Remark
	}
	if t.Nickname != "" {
		return t.Nickname
	}
	if len(t.UID) <= 8 {
		if t.UID != "" {
			return t.UID
		}
		return "未命名"
	}
	return "…" + t.UID[len(t.UID)-8:]
}

// resolveUIDClient 按 ?uid= 解析目标账号的 client 与凭证：
//   - uid 为空或 "active"：返回当前主账号（found=false 表示未登录）；
//   - uid 非空：支持原始 UID 与档案 ID（m.ID = h_<hex>），校验存在于号池（或池未挂载时匹配主账号）。
func (h *Handler) resolveUIDClient(uid string) (targetUID string, cli *upstream.Client, tok *auth.Token, found bool) {
	uid = strings.TrimSpace(uid)
	active := h.toks.Get()
	if uid == "" || uid == "active" {
		if active == nil {
			return "", h.client, nil, false
		}
		return active.UID, h.client, active, true
	}
	lookupRaw := func(raw string) (string, *upstream.Client, *auth.Token, bool) {
		if h.pool != nil {
			ts, err := h.pool.TokenStore(raw)
			if err != nil || ts == nil {
				return raw, nil, nil, false
			}
			t := ts.Get()
			if t == nil {
				return raw, nil, nil, false
			}
			if active != nil && active.UID == raw {
				return raw, h.client, t, true
			}
			return raw, h.client.WithTokenStore(ts), t, true
		}
		if active != nil && active.UID == raw {
			return raw, h.client, active, true
		}
		return raw, nil, nil, false
	}
	if target, c, t, ok := lookupRaw(uid); ok {
		return target, c, t, true
	}
	// 兼容传入档案 ID（如含 . / @ / : 的 UID 编码为 h_<hex>）
	if archived, err := h.accountBook().Load(uid); err == nil && archived != nil && archived.UID != "" && archived.UID != uid {
		if target, c, t, ok := lookupRaw(archived.UID); ok {
			return target, c, t, true
		}
	}
	return uid, nil, nil, false
}

// accountSwitched 判定本次凭证写入是否真的换了账号。
//
// 补传过期 token.json、重扫同一账号是最常见的恢复路径，此时清日标志会让当天重新上报一轮
// （上游按新会话计，活跃上报翻倍），所以只在账号真的变了时才失效。
// 判不准（旧凭证缺失 / 任一侧 UID 为空）时按「换了」处理：宁可多失效一次，也别让新账号整天静默跳过。
func accountSwitched(prev, next *auth.Token) bool {
	if prev == nil || next == nil || prev.UID == "" || next.UID == "" {
		return true
	}
	return prev.UID != next.UID
}

func maskToken(s string) string {
	if len(s) <= 20 {
		return s
	}
	return s[:16] + "..." + s[len(s)-6:]
}

func (h *Handler) account(w http.ResponseWriter, r *http.Request) {
	cfg := h.cfg.Get()
	ep := config.EndpointOf(cfg.Region)
	t := h.toks.Get()
	if t == nil {
		jsonWrite(w, http.StatusOK, map[string]any{
			"logged_in": false, "region": cfg.Region, "region_name": ep.Name,
		})
		return
	}
	jsonWrite(w, http.StatusOK, map[string]any{
		"logged_in":     true,
		"mode":          "oauth",
		"access_token":  maskToken(t.AccessToken),
		"has_refresh":   t.RefreshToken != "",
		"uid":           t.UID,
		"id":            auth.AccountID(t.UID),
		"enterprise_id": t.EnterpriseID,
		"nickname":      t.Nickname,
		"remark":        t.Remark,
		"domain":        t.Domain,
		"expires_at":    t.ExpiresAt,
		"expires_human": t.ExpiresInHuman(),
		"expired":       t.IsExpired(),
		"saved_at":      t.SavedAt,
		"region":        cfg.Region,
		"region_name":   ep.Name,
		"region_match":  t.Domain == ep.Domain, // 凭证与当前端点是否匹配
	})
}

func (h *Handler) oauthStart(w http.ResponseWriter, r *http.Request) {
	state, authURL, err := h.client.OAuthStart()
	if err != nil {
		jsonErr(w, http.StatusBadGateway, err.Error())
		return
	}
	jsonWrite(w, http.StatusOK, map[string]any{
		"state": state, "auth_url": authURL, "expires_in": 300,
	})
}

func (h *Handler) oauthPoll(w http.ResponseWriter, r *http.Request) {
	state := strings.TrimSpace(r.URL.Query().Get("state"))
	if state == "" {
		jsonErr(w, http.StatusBadRequest, "缺少 state 参数")
		return
	}
	tok, ok, err := h.client.OAuthPoll(state)
	if err != nil {
		jsonWrite(w, http.StatusOK, map[string]any{"status": "error", "message": err.Error()})
		return
	}
	if !ok {
		jsonWrite(w, http.StatusOK, map[string]any{"status": "pending"})
		return
	}
	prev := h.toks.Get()
	if tok.Remark == "" {
		// 重登已有账号时保留本地备注，避免扫码续期后备注被清空
		tok.Remark = h.existingRemark(tok.UID, prev)
	}
	if err := h.toks.Save(tok); err != nil {
		jsonErr(w, http.StatusInternalServerError, "保存凭证失败: "+err.Error())
		return
	}
	h.archiveAccount(tok) // 扫码得到的账号自动入档，切换列表里才看得到
	if h.pool != nil {
		h.pool.ReloadMembers()
	}
	if accountSwitched(prev, tok) {
		// 真的换了账号才清；同一账号重新扫码不该丢掉今日「已上报」（见 accountSwitched）
		h.invalidateAccountCaches()
	}
	jsonWrite(w, http.StatusOK, map[string]any{
		"status": "success",
		"account": map[string]any{
			"uid": tok.UID, "nickname": tok.Nickname, "remark": tok.Remark, "domain": tok.Domain,
			"expires_human": tok.ExpiresInHuman(),
		},
	})
}

func (h *Handler) accountRefresh(w http.ResponseWriter, r *http.Request) {
	if err := h.client.RefreshNow(); err != nil {
		jsonErr(w, http.StatusBadGateway, err.Error())
		return
	}
	t := h.toks.Get()
	jsonWrite(w, http.StatusOK, map[string]any{"ok": true, "expires_human": t.ExpiresInHuman(), "expires_at": t.ExpiresAt})
}

func (h *Handler) accountTest(w http.ResponseWriter, r *http.Request) {
	if err := h.client.Ping(); err != nil {
		jsonWrite(w, http.StatusOK, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	jsonWrite(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) accountDelete(w http.ResponseWriter, r *http.Request) {
	// 先记下 uid：Clear 之后 h.toks.Get() 就是 nil 了，事后取不到。
	var clearedUID string
	if t := h.toks.Get(); t != nil {
		clearedUID = t.UID
	}
	if err := h.toks.Clear(); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 号池为每个账号自持一份 TokenStore（含 refresh_token），Clear 只清了
	// token.json 那一份。不摘掉池成员，用户「退出登录」后 chat 仍会继续以该账号
	// 发请求（还可能把刷新的 token 写回已删除的文件）——既是隐私问题，
	// 也让「登出后不该再用这个号」的预期失效。
	if h.pool != nil && clearedUID != "" {
		h.pool.DeleteAccount(clearedUID)
	}
	if clearedUID != "" {
		h.clearAccountCaches(clearedUID, false)
	}
	h.invalidateAccountCaches()
	jsonWrite(w, http.StatusOK, map[string]any{"ok": true})
}

// 导出范围（GET /admin/account/export?scope=）。
//
// 只有两种：单个（当前活动账号）与批量（uids 指定）。刻意不提供「全部账号」这种第三种 ——
// 批量导出在前端默认全选（且 /admin/accounts 会自愈补存未入档的活动账号），
// 「全选 + 导出」已经完全覆盖它，多一个分支只是多一处口径要维护。
const (
	exportScopeCurrent  = "current"  // 只导当前活动账号（token.json）
	exportScopeSelected = "selected" // 导出 uids 指定的账号
)

// accountExport 导出凭证包（下载附件）。
//
// 两种范围统一输出 auth.Bundle（见 auth/bundle.go）：单账号导出与批量导出同构，导入侧只需
// 一条路径。刻意不再兼容旧的 token.json 裸格式 —— 两种形状并存时，导入侧要猜「收到的到底是
// 一份凭证还是一个包」，而猜错会把 {format, accounts} 当成一份凭证存进档案（access_token 为空）。
//
//   - scope=current（默认）：只导 token.json 里那一份
//   - scope=selected：uids 用逗号分隔（可重复传参）；档案不存在的按缺失报错
func (h *Handler) accountExport(w http.ResponseWriter, r *http.Request) {
	scope := strings.TrimSpace(r.URL.Query().Get("scope"))
	if scope == "" {
		scope = exportScopeCurrent
	}
	active := h.toks.Get()
	activeUID := ""
	if active != nil {
		activeUID = active.UID
	}

	var toks []*auth.Token
	switch scope {
	case exportScopeCurrent:
		if active == nil {
			jsonErr(w, http.StatusBadRequest, "尚未登录，无凭证可导出")
			return
		}
		toks = []*auth.Token{active}
	case exportScopeSelected:
		uids := exportUIDs(r)
		if len(uids) == 0 {
			jsonErr(w, http.StatusBadRequest, "未指定要导出的账号")
			return
		}
		var missing []string
		for _, uid := range uids {
			// 活动账号以 token.json 为准：档案是旁挂副本，刷新后可能落后于 token.json
			if active != nil && active.UID == uid {
				toks = append(toks, active)
				continue
			}
			t, err := h.accountBook().Load(uid)
			if err != nil {
				if errors.Is(err, auth.ErrAccountNotFound) {
					missing = append(missing, uid)
					continue
				}
				jsonErr(w, http.StatusInternalServerError, "读取账号档案失败: "+err.Error())
				return
			}
			toks = append(toks, t)
		}
		if len(toks) == 0 {
			jsonErr(w, http.StatusBadRequest, "所选账号不存在，无凭证可导出")
			return
		}
		// 部分缺失但仍有可导的：照导，前端据 missing 提示（不静默少导账号）
		// 值含 uid（JWT sub，可控）：经 headerSafe 剔除控制字符，否则构成响应头注入
		if len(missing) > 0 {
			w.Header().Set("X-Export-Missing", headerSafe(strings.Join(missing, ",")))
		}
	default:
		jsonErr(w, http.StatusBadRequest, "未知的导出范围: "+scope)
		return
	}

	now := time.Now()
	b := auth.NewBundle(now.Unix(), activeUID, toks)
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 文件名纯 ASCII（账号昵称可能含中文，塞进 header 需要 RFC 5987 编码，得不偿失）
	name := "buddy2api-accounts-" + now.Format("20060102-150405") + ".json"
	if scope == exportScopeCurrent {
		name = "buddy2api-account-" + safeFileToken(activeUID) + ".json"
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("X-Export-Count", strconv.Itoa(len(b.Accounts)))
	w.Write(data)
}

// exportUIDs 解析 scope=selected 的 uids 参数：支持 uids=a,b 与重复的 uids=a&uids=b，去重保序。
func exportUIDs(r *http.Request) []string {
	q := r.URL.Query()
	var out []string
	seen := map[string]bool{}
	for _, raw := range q["uids"] {
		for _, uid := range strings.Split(raw, ",") {
			uid = strings.TrimSpace(uid)
			if uid == "" || seen[uid] {
				continue
			}
			seen[uid] = true
			out = append(out, uid)
		}
	}
	return out
}

// headerSafe 过滤要放进 HTTP 头的值：只留可打印 ASCII，其余（控制字符、CR/LF、非 ASCII）化为 '?'。
//
// 这些值可能来自 uid（JWT sub，攻击者可控），直接 Set 会让 CR/LF 剖断头部形成响应头注入。
// 不做 URL 编码：结果只用于前端提示文案，可读性优先于可逆性。
func headerSafe(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 0x20 && r < 0x7f {
			b.WriteRune(r)
		} else {
			b.WriteByte('?')
		}
		if b.Len() >= 512 { // 头长度上限：异常 uid 不应无限制撑大响应头
			break
		}
	}
	return b.String()
}

// safeFileToken 把 uid 归一为可安全放进文件名 / header 的片段。
// 昵称与 uid 都可能含任意字符，此处不允许任何非 ASCII 字母数字，其余一律下划线。
func safeFileToken(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
		if b.Len() >= 32 {
			break
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}

// accountImport 导入账号凭证，必须含 access_token（合法 JWT），其余可选。
//
// 默认「仅导入」：只把凭证存进账号档案（data/accounts/<id>.json），不动活动账号 token.json ——
// 用户可一次导入多个账号，再在「账号切换」里挑一个启用。activate=true 时才写 token.json
// （立即启用），与切换档案同口径：清账号口径缓存 + 复位调度器日内状态。
func (h *Handler) accountImport(w http.ResponseWriter, r *http.Request) {
	var req importRequest
	if !readJSON(w, r, &req) {
		return
	}
	t, errMsg := h.importedToken(req)
	if t == nil {
		jsonErr(w, http.StatusBadRequest, errMsg)
		return
	}
	prev := h.toks.Get()
	if t.Remark == "" {
		// 未提供新备注时沿用本地已有备注（重导裸 token.json 不应清空原备注）
		t.Remark = h.existingRemark(t.UID, prev)
	}
	if !req.Activate {
		// 仅导入：唯一产出就是这份档案，所以存档失败必须报错（启用路径可以只留痕，
		// 那里 token.json 已经落地，用户仍有可用凭证）。
		if err := h.accountBook().Save(t); err != nil {
			jsonErr(w, http.StatusInternalServerError, "保存账号档案失败: "+err.Error())
			return
		}
		if prev != nil && prev.UID == t.UID && t.Remark != prev.Remark {
			_ = h.toks.UpdateRemark(t.UID, t.Remark)
		}
		if h.pool != nil {
			h.pool.SetAccountRemark(t.UID, t.Remark)
			h.pool.ReloadMembers()
		}
		jsonWrite(w, http.StatusOK, map[string]any{
			"ok":       true,
			"uid":      t.UID,
			"nickname": t.Nickname,
			"remark":   t.Remark,
			"domain":   t.Domain,
			"activate": false,
		})
		return
	}
	if err := h.toks.Save(t); err != nil {
		jsonErr(w, http.StatusInternalServerError, "保存凭证失败: "+err.Error())
		return
	}
	h.archiveAccount(t) // 导入的账号同样入档
	if h.pool != nil {
		h.pool.SetAccountRemark(t.UID, t.Remark)
		h.pool.ReloadMembers()
	}
	if accountSwitched(prev, t) {
		// 同账号重导不清，否则今日上报会按新会话再发一轮（见 accountSwitched）
		h.invalidateAccountCaches()
	}
	jsonWrite(w, http.StatusOK, map[string]any{
		"ok":       true,
		"uid":      t.UID,
		"nickname": t.Nickname,
		"remark":   t.Remark,
		"domain":   t.Domain,
		"activate": true,
	})
}

const maxAccountRemarkRunes = 64

// importRequest 单账号导入请求体；也是批量包里单条的宽松形式（两者字段一一对应）。
type importRequest struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresAt    int64  `json:"expires_at"`
	Domain       string `json:"domain"`
	Nickname     string `json:"nickname"`
	Remark       string `json:"remark"`
	Activate     bool   `json:"activate"`
}

// importedToken 校验并归一一份待导入凭证（单导入与批量导入共用）。
// 失败返回 (nil, 给用户看的原因)；成功后 token_type 默认 Bearer、domain 缺省按当前端点。
func (h *Handler) importedToken(req importRequest) (*auth.Token, string) {
	tok := strings.TrimSpace(req.AccessToken)
	if tok == "" {
		return nil, "access_token 不能为空"
	}
	if _, _, _, _, _, err := auth.ParseJWT(tok); err != nil {
		return nil, "access_token 不是合法 JWT: " + err.Error()
	}
	remark := strings.TrimSpace(req.Remark)
	if utf8.RuneCountInString(remark) > maxAccountRemarkRunes {
		return nil, fmt.Sprintf("备注不能超过 %d 个字符", maxAccountRemarkRunes)
	}
	t := &auth.Token{
		AccessToken:  tok,
		RefreshToken: strings.TrimSpace(req.RefreshToken),
		TokenType:    strings.TrimSpace(req.TokenType),
		ExpiresAt:    req.ExpiresAt,
		Domain:       strings.TrimSpace(req.Domain),
		Nickname:     strings.TrimSpace(req.Nickname),
		Remark:       remark,
	}
	t.EnrichFromJWT()
	if t.TokenType == "" {
		t.TokenType = "Bearer"
	}
	if t.Domain == "" {
		t.Domain = config.EndpointOf(h.cfg.Get().Region).Domain
	}
	return t, ""
}

// bundleImportMaxBytes 批量导入请求体上限。
//
// 刻意比 readJSON 的 1MB 宽松：一条真实凭证（access_token ~1.4KB + refresh_token ~1.1KB，
// 缩进 JSON 后约 5KB）意味着 1MB 只能装 ~200 个账号，而「导出全部」不限条数 —— 导出能产出、
// 导入却装不下的包是最难解释的一类失败。8MB 可容纳上千个账号，同时仍防住内存放大。
const bundleImportMaxBytes = 8 << 20

// accountImportBatch 批量导入凭证包（POST /admin/account/import/batch，body 即 auth.Bundle）。
//
// 与单导入的「仅导入」完全同口径：**只入档，不换活动账号**。刻意不提供 activate ——
// 一次导入多个账号时该启用哪个无从判断，切账号统一由「账号切换」卡片承担。
//
// 语义是逐条独立尽力而为之（不是全或无）：一条坏掉不应让整包失败，否则用户得先手工清理
// 包里的脏数据才能导入剩下的九十九条。每条结果（ok / error）都回给前端逐条展示。
func (h *Handler) accountImportBatch(w http.ResponseWriter, r *http.Request) {
	if r.ContentLength > bundleImportMaxBytes {
		jsonErr(w, http.StatusRequestEntityTooLarge, fmt.Sprintf(
			"凭证包过大（%.1f MB）：单次导入上限 %.0f MB，请拆分后再导入",
			float64(r.ContentLength)/(1<<20), float64(bundleImportMaxBytes)/(1<<20)))
		return
	}
	// 多读 1 字节：用来区分「恰好到上限」与「被截断」——
	// 直接 LimitReader 到上限会让超限请求变成半截 JSON，用户看到的是
	// 「不是合法 JSON: unexpected end of JSON input」，完全指不出真正原因。
	raw, err := io.ReadAll(io.LimitReader(r.Body, bundleImportMaxBytes+1))
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "读取请求体失败")
		return
	}
	if len(raw) > bundleImportMaxBytes {
		jsonErr(w, http.StatusRequestEntityTooLarge, fmt.Sprintf(
			"凭证包过大：单次导入上限 %.0f MB，请拆分后再导入",
			float64(bundleImportMaxBytes)/(1<<20)))
		return
	}
	bundle, err := auth.UnmarshalBundle(raw)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	book := h.accountBook()
	type importResult struct {
		UID      string `json:"uid,omitempty"`
		Nickname string `json:"nickname,omitempty"`
		Remark   string `json:"remark,omitempty"`
		Domain   string `json:"domain,omitempty"`
		OK       bool   `json:"ok"`
		Error    string `json:"error,omitempty"`
		// Overwrote 本条目覆盖了已存在的同名档案（重复导入 / 回复旧备份时会出现）。
		// 结果里必须显式标出来：否则用户无法意识到本地较新的凭证已被包里的旧数据替换。
		Overwrote bool `json:"overwrote,omitempty"`
	}
	results := make([]importResult, 0, len(bundle.Accounts))
	// 先取现存档案的 uid 集合（与已有备注）：Save 是「同名覆盖」，不先查就只能事后猜。
	// List 失败不阻断导入（档案目录可能尚不存在），此时按「都是新账号」处理。
	existing := map[string]bool{}
	existingRemarks := map[string]string{}
	if metas, err := book.List(); err == nil {
		for _, m := range metas {
			existing[m.UID] = true
			if m.Remark != "" {
				existingRemarks[m.UID] = m.Remark
			}
		}
	}
	active := h.toks.Get()
	if active != nil && active.Remark != "" && existingRemarks[active.UID] == "" {
		existingRemarks[active.UID] = active.Remark
	}
	imported, failed, overwrote := 0, 0, 0
	// seen 记录本包内已出现过的 uid：同一包里同一账号出现两次时后者会静默覆盖前者，
	// 若不报出来，用户会以为导入成功了两份不同账号（imported 也会虚高）。
	seen := map[string]int{}
	for i, item := range bundle.Accounts {
		t, errMsg := h.importedToken(importRequest{
			AccessToken:  item.AccessToken,
			RefreshToken: item.RefreshToken,
			TokenType:    item.TokenType,
			ExpiresAt:    item.ExpiresAt,
			Domain:       item.Domain,
			Nickname:     item.Nickname,
			Remark:       item.Remark,
		})
		if t == nil {
			failed++
			results = append(results, importResult{Error: fmt.Sprintf("第 %d 条：%s", i+1, errMsg)})
			continue
		}
		if t.Remark == "" {
			t.Remark = existingRemarks[t.UID]
		}
		if prev, dup := seen[t.UID]; dup {
			// 不算失败（凭证本身合法），但不能计入 imported 的新增数，且必须提示覆盖了哪一条
			failed++
			results = append(results, importResult{UID: t.UID, Nickname: t.Nickname, Remark: t.Remark,
				Error: fmt.Sprintf("第 %d 条：与第 %d 条是同一账号（uid %s），已跳过；请确认包里是否重复",
					i+1, prev, t.UID)})
			continue
		}
		if err := book.Save(t); err != nil {
			failed++
			results = append(results, importResult{UID: t.UID, Nickname: t.Nickname, Remark: t.Remark,
				Error: "保存账号档案失败: " + err.Error()})
			continue
		}
		if active != nil && active.UID == t.UID && t.Remark != active.Remark {
			_ = h.toks.UpdateRemark(t.UID, t.Remark)
		}
		if h.pool != nil {
			h.pool.SetAccountRemark(t.UID, t.Remark)
		}
		seen[t.UID] = i + 1
		imported++
		was := existing[t.UID]
		if was {
			overwrote++
		}
		results = append(results, importResult{UID: t.UID, Nickname: t.Nickname, Remark: t.Remark, Domain: t.Domain,
			OK: true, Overwrote: was})
	}
	// 全部失败时给 400：前端就能用统一的错误气泡，而不是弹一个「成功」再列全红。
	// 部分成功则以 200 + 逐条结果返回，前端展示明细。
	if imported > 0 && h.pool != nil {
		h.pool.ReloadMembers()
	}
	status := http.StatusOK
	if imported == 0 {
		status = http.StatusBadRequest
	}
	jsonWrite(w, status, map[string]any{
		"ok":        imported > 0,
		"imported":  imported,
		"failed":    failed,
		"overwrote": overwrote,
		"total":     len(results),
		"active":    false, // 批量导入恒不切换活动账号（与单导入「仅导入」一致）
		"items":     results,
	})
}

// ── 多账号档案 ──
//
// 活动账号始终只有 token.json（auth.TokenStore）；data/accounts/<id>.json 是旁挂副本。
// 切换 = 归档当前 → 用目标档案覆盖 token.json，因此 upstream / proxy / scheduler 无需改动。
// 唯一必须做的是 invalidateAccountCaches：余额/签到/成长缓存与「今日已完成」日标志都是
// 按单账号设计的，不清会让新账号继承旧账号状态并当天静默跳过任务。

// accountBook 档案库与 token.json 同处数据目录。
func (h *Handler) accountBook() *auth.AccountBook { return auth.NewAccountBook(h.toks.DataDir()) }

// archiveAccount 把凭证存入档案库。失败只留痕：登录/导入本身已成功，
// 档案写不进去不该回滚主流程（用户也就少一份可切换的副本）。
func (h *Handler) archiveAccount(t *auth.Token) {
	if err := h.accountBook().Save(t); err != nil {
		slog.Warn("保存账号档案失败", "uid", t.UID, "error", err)
	}
}

// existingRemark 查询指定账号本地已有的备注（优先当前活动凭证，其次档案库）。
// 用于 OAuth 重登或导入未携带 remark 的凭证时避免静默丢失用户备注。
func (h *Handler) existingRemark(uid string, active *auth.Token) string {
	if uid == "" {
		return ""
	}
	if active != nil && active.UID == uid && active.Remark != "" {
		return active.Remark
	}
	if old, err := h.accountBook().Load(uid); err == nil && old.Remark != "" {
		return old.Remark
	}
	return ""
}

// accountItem 账号列表项（不含任何凭证）。
type accountItem struct {
	UID              string  `json:"uid"`
	RawUID           string  `json:"raw_uid"`
	Nickname         string  `json:"nickname"`
	Remark           string  `json:"remark"`
	Domain           string  `json:"domain"`
	EnterpriseID     string  `json:"enterprise_id"`
	HasRefresh       bool    `json:"has_refresh"`
	ExpiresAt        int64   `json:"expires_at"`
	ExpiresHuman     string  `json:"expires_human"`
	Expired          bool    `json:"expired"`
	Active           bool    `json:"active"`
	RegionMatch      bool    `json:"region_match"`
	InPool           bool    `json:"in_pool"`
	PoolAvailable    bool    `json:"pool_available"`
	PoolAutoDisabled string  `json:"pool_auto_disabled"`
	Credits          float64 `json:"credits"`
	CreditsAt        int64   `json:"credits_at"`
	EarliestExpireAt int64   `json:"earliest_expire_at"`
	ExpiringCredits  float64 `json:"expiring_credits"`
}

// newAccountItem 元信息 → 列表项。active 为当前 token.json（nil = 未登录）。
func newAccountItem(m auth.AccountMeta, active *auth.Token, regionDomain string) accountItem {
	t := auth.Token{ExpiresAt: m.ExpiresAt}
	item := accountItem{
		UID:          m.ID,
		RawUID:       m.UID,
		Nickname:     m.Nickname,
		Remark:       m.Remark,
		Domain:       m.Domain,
		ExpiresAt:    m.ExpiresAt,
		ExpiresHuman: t.ExpiresInHuman(),
		Expired:      t.IsExpired(),
		Active:       active != nil && active.UID == m.UID,
		RegionMatch:  regionMatches(m.Domain, regionDomain),
	}
	if item.Active && active != nil {
		item.EnterpriseID = active.EnterpriseID
		item.HasRefresh = active.RefreshToken != ""
	}
	return item
}

// enrichAccountPoolInfo 从号池填充账号的调度状态、额度摘要与凭证元信息（无开关门控）。
func (h *Handler) enrichAccountPoolInfo(item *accountItem, uid string) {
	if h.pool == nil {
		return
	}
	if st, ok := h.pool.AccountStateOf(uid); ok {
		item.InPool = true
		item.PoolAvailable = st.Available()
		item.PoolAutoDisabled = st.AutoDisabled
		item.Credits = st.Credits
		item.CreditsAt = st.CreditsAt
		item.EarliestExpireAt = st.EarliestExpireAt
		item.ExpiringCredits = st.ExpiringCredits
	}
	if ts, err := h.pool.TokenStore(uid); err == nil && ts != nil {
		if t := ts.Get(); t != nil {
			item.EnterpriseID = t.EnterpriseID
			item.HasRefresh = t.RefreshToken != ""
		}
	}
}

func (h *Handler) listAccounts(w http.ResponseWriter, r *http.Request) {
	book := h.accountBook()
	metas, err := book.List()
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "读取账号列表失败: "+err.Error())
		return
	}
	// 升级前只登录过一次的旧安装：token.json 有凭证但没有档案。补存一份（幂等），
	// 否则列表空着，用户第一次切换时旧账号就丢了。
	if active := h.toks.Get(); active != nil && !hasAccountUID(metas, active.UID) {
		h.archiveAccount(active)
		if metas, err = book.List(); err != nil {
			jsonErr(w, http.StatusInternalServerError, "读取账号列表失败: "+err.Error())
			return
		}
	}
	active := h.toks.Get()
	regionDomain := config.EndpointOf(h.cfg.Get().Region).Domain
	items := make([]accountItem, 0, len(metas))
	for _, m := range metas {
		item := newAccountItem(m, active, regionDomain)
		h.enrichAccountPoolInfo(&item, m.UID)
		items = append(items, item)
	}
	// 当前账号置顶（List 已按 SavedAt 倒序，但刚归档的旧账号会排到前面）。
	sort.SliceStable(items, func(i, j int) bool { return items[i].Active && !items[j].Active })
	jsonWrite(w, http.StatusOK, map[string]any{"items": items})
}

func hasAccountUID(metas []auth.AccountMeta, uid string) bool {
	for _, m := range metas {
		if m.UID == uid {
			return true
		}
	}
	return false
}

// activateAccount 切换到指定档案。
func (h *Handler) activateAccount(w http.ResponseWriter, r *http.Request) {
	book := h.accountBook()
	next, err := book.Load(chi.URLParam(r, "uid"))
	if err != nil {
		if errors.Is(err, auth.ErrAccountNotFound) {
			jsonErr(w, http.StatusNotFound, "账号档案不存在")
			return
		}
		jsonErr(w, http.StatusInternalServerError, "读取账号档案失败: "+err.Error())
		return
	}
	// 同一账号：不动凭证也不清缓存（清了当天活跃上报会按新会话再发一轮）。
	if cur := h.toks.Get(); cur != nil && cur.UID == next.UID {
		jsonWrite(w, http.StatusOK, map[string]any{"ok": true, "uid": next.UID, "nickname": next.Nickname, "remark": next.Remark})
		return
	}
	// 先归档当前：刷新后 token.json 是最新的，档案可能还停在旧 token 上。
	if cur := h.toks.Get(); cur != nil {
		h.archiveAccount(cur)
	}
	if err := h.toks.Save(next); err != nil {
		jsonErr(w, http.StatusInternalServerError, "保存凭证失败: "+err.Error())
		return
	}
	// 池成员里 token.json 那一份会被 EnsureValid 持续刷新；切号后重新加载一次，
	// 让池用的凭证与「当前活动账号」保持一致（否则池可能还在用切换前的旧 token）。
	if h.pool != nil {
		h.pool.ReloadMembers()
	}
	h.invalidateAccountCaches()
	jsonWrite(w, http.StatusOK, map[string]any{"ok": true, "uid": next.UID, "nickname": next.Nickname, "remark": next.Remark})
}

// updateAccountProfile 更新指定账号的备注（PUT /admin/accounts/{uid}）。
// 同时落盘账号档案与活动凭证 token.json（若为当前账号），并同步号池内存快照；
// 不触碰账号口径缓存或调度器状态（仅本地展示元信息）。
func (h *Handler) updateAccountProfile(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "uid")
	var req struct {
		Remark *string `json:"remark"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Remark == nil {
		jsonErr(w, http.StatusBadRequest, "缺少 remark 参数")
		return
	}
	remark := strings.TrimSpace(*req.Remark)
	if utf8.RuneCountInString(remark) > maxAccountRemarkRunes {
		jsonErr(w, http.StatusBadRequest, fmt.Sprintf("备注不能超过 %d 个字符", maxAccountRemarkRunes))
		return
	}
	book := h.accountBook()
	t, err := book.UpdateRemark(id, remark)
	if err != nil {
		if errors.Is(err, auth.ErrAccountNotFound) {
			// 旧安装仅存在 token.json（尚未入档）：允许直接给当前账号写备注并顺带归档。
			if active := h.toks.Get(); active != nil && (active.UID == id || auth.AccountID(active.UID) == id) {
				if err := h.toks.UpdateRemark(active.UID, remark); err != nil {
					jsonErr(w, http.StatusInternalServerError, "保存备注失败: "+err.Error())
					return
				}
				t = h.toks.Get()
				h.archiveAccount(t)
			} else {
				jsonErr(w, http.StatusNotFound, "账号档案不存在")
				return
			}
		} else {
			jsonErr(w, http.StatusInternalServerError, "更新账号备注失败: "+err.Error())
			return
		}
	} else if active := h.toks.Get(); active != nil && active.UID == t.UID {
		if err := h.toks.UpdateRemark(t.UID, remark); err != nil {
			jsonErr(w, http.StatusInternalServerError, "更新当前账号备注失败: "+err.Error())
			return
		}
	}
	if h.pool != nil {
		h.pool.SetAccountRemark(t.UID, remark)
	}
	active := h.toks.Get()
	regionDomain := config.EndpointOf(h.cfg.Get().Region).Domain
	item := newAccountItem(auth.AccountMeta{
		ID:        auth.AccountID(t.UID),
		UID:       t.UID,
		Nickname:  t.Nickname,
		Remark:    t.Remark,
		Domain:    t.Domain,
		ExpiresAt: t.ExpiresAt,
		SavedAt:   t.SavedAt,
	}, active, regionDomain)
	item.EnterpriseID = t.EnterpriseID
	item.HasRefresh = t.RefreshToken != ""
	h.enrichAccountPoolInfo(&item, t.UID)
	jsonWrite(w, http.StatusOK, map[string]any{
		"ok":      true,
		"uid":     item.UID,
		"raw_uid": t.UID,
		"remark":  t.Remark,
		"item":    item,
	})
}

// deleteAccountProfile 移除档案。当前正在使用的账号不允许移除：
// token.json 还在，下次列表自愈会把档案重新写回来。
func (h *Handler) deleteAccountProfile(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "uid")
	book := h.accountBook()
	t, err := book.Load(id)
	if err != nil {
		if errors.Is(err, auth.ErrAccountNotFound) {
			jsonErr(w, http.StatusNotFound, "账号档案不存在")
			return
		}
		jsonErr(w, http.StatusInternalServerError, "读取账号档案失败: "+err.Error())
		return
	}
	if active := h.toks.Get(); active != nil && active.UID == t.UID {
		jsonErr(w, http.StatusBadRequest, "当前账号正在使用，请先切换到其他账号或退出登录")
		return
	}
	if err := book.Delete(id); err != nil {
		jsonErr(w, http.StatusInternalServerError, "删除账号档案失败: "+err.Error())
		return
	}
	// 一并清掉池状态与该账号全部 scoped 缓存：否则删了号又重新导入同一个账号，
	// 会继承旧的「额度耗尽」标记或陈旧缓存。
	if h.pool != nil {
		h.pool.DeleteAccount(t.UID)
	}
	h.clearAccountCaches(t.UID, true)
	jsonWrite(w, http.StatusOK, map[string]any{"ok": true})
}

// ── 余额 ──

// resourceAccount 额度包明细。
//
// CapacityRemain/Size/Used 为「有效展示值」：当存在周期数据时优先取周期级
// （CycleCapacity*），否则回退账户级（Capacity*）。CodeBuddy 免费/裂变包按月
// 周期计费，账户级 CapacityUsed 恒为 0，真实已用落在周期级；直接取账户级会让
// 前端「已用」恒显 0、进度条恒满。与 references/wicm84266964 effective_remain 一致。
// CycleRemain/Used/Size 始终为周期级原始值（无周期数据则为 0），供前端细分展示。
type resourceAccount struct {
	PackageName    string  `json:"package_name"`
	ProductName    string  `json:"product_name"`
	CapacityRemain float64 `json:"capacity_remain"` // 有效剩余（周期优先）
	CapacitySize   float64 `json:"capacity_size"`   // 有效总量（周期优先）
	CapacityUsed   float64 `json:"capacity_used"`   // 有效已用（周期优先）
	CycleRemain    float64 `json:"cycle_remain"`    // 本周期剩余
	CycleUsed      float64 `json:"cycle_used"`      // 本周期已用
	CycleSize      float64 `json:"cycle_size"`      // 本周期总量
	CapacityUnit   string  `json:"capacity_unit"`
	PackageType    string  `json:"package_type"`
	ResourceType   any     `json:"resource_type"`
	AutoRenewFlag  any     `json:"auto_renew_flag"`
	Status         any     `json:"status"`
	ExpireTime     string  `json:"expire_time"`
	ExpireAt       int64   `json:"expire_at"` // Unix 秒；0 = 未知
	DaysLeft       *int    `json:"days_left"` // nil = 未知
	Expired        bool    `json:"expired"`
	Warn           string  `json:"warn"` // "7d" | "30d" | ""

	// Key 稳定标识，供前端 x-for 的 :key 使用。上游无可靠 id 字段，同名同类额度包可并存，
	// 故以内容指纹为键（内容相同的包渲染等价，允许重复）。不可用数组下标：
	// 前端「隐藏已用完」过滤数组后，Alpine 会按下标复用 DOM 节点，导致卡片内容错位。
	Key string `json:"key"`
}

// mergedResourceAccount 全部账号聚合模式下的额度包明细（附加所属账号）。
type mergedResourceAccount struct {
	resourceAccount
	AccountUID   string `json:"account_uid"`
	AccountLabel string `json:"account_label"`
}

// resourceAccountSummary 全部账号聚合模式下的单账号摘要。
type resourceAccountSummary struct {
	UID              string  `json:"uid"`
	Label            string  `json:"label"`
	Nickname         string  `json:"nickname"`
	Remark           string  `json:"remark"`
	Active           bool    `json:"active"`
	RegionMatch      bool    `json:"region_match"`
	TotalDosage      float64 `json:"total_dosage"`
	EarliestExpireAt int64   `json:"earliest_expire_at"`
	ExpiringCredits  float64 `json:"expiring_credits"`
	UpdatedAt        int64   `json:"updated_at"`
	Error            string  `json:"error,omitempty"`
}

// resourceAccountKey 由额度包展示字段计算稳定指纹，用作前端 x-for 的 :key。
// 取 sha256 摘要前 8 字节：包名可含任意字符，指纹与额度包在数组中的顺序无关。
func resourceAccountKey(a resourceAccount) string {
	h := sha256.New()
	// 用长度前缀而非分隔符：上游字段可含任意字节（含 \x00），
	// 分隔符拼接会使 ("a\x00b","c") 与 ("a","b\x00c") 碰撞。
	writeField := func(s string) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(s)))
		h.Write(n[:])
		h.Write([]byte(s))
	}
	writeNum := func(f float64) {
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], math.Float64bits(f))
		h.Write(b[:])
	}
	writeField(a.PackageName)
	writeField(a.ProductName)
	writeNum(a.CapacityRemain)
	writeNum(a.CapacitySize)
	writeNum(a.CapacityUsed)
	writeField(a.ExpireTime)
	if a.Expired {
		h.Write([]byte{1})
	} else {
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// resourceCacheKey 构造 per-uid 余额缓存键（v4 新增 expire_at 且按 uid 隔离）。
func resourceCacheKey(uid string) string {
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return "v4:default"
	}
	return "v4:" + uid
}

// resourceCacheStaleKeys 升级时一并清理的旧版全局缓存键；删除操作幂等。
var resourceCacheStaleKeys = []string{"default", "default_v2", "default_v3"}

func (h *Handler) writeResourceCache(uid string, processed map[string]any) {
	if raw, err := json.Marshal(processed); err == nil {
		_ = h.st.SetCache("resource_cache", resourceCacheKey(uid), string(raw))
	}
	for _, stale := range resourceCacheStaleKeys {
		_ = h.st.DeleteCacheKey("resource_cache", stale)
	}
}

func (h *Handler) resources(w http.ResponseWriter, r *http.Request) {
	cfg := h.cfg.Get()
	force := r.URL.Query().Get("force") == "1"
	uid := strings.TrimSpace(r.URL.Query().Get("uid"))

	if uid == "all" {
		h.resourcesAll(w, cfg, force)
		return
	}

	var (
		targetUID string
		label     string
		cli       *upstream.Client
	)
	if uid == "" || uid == "active" {
		t := h.toks.Get()
		if t != nil {
			targetUID = t.UID
			label = accountDisplayLabel(t)
		}
		cli = h.client
	} else {
		var t *auth.Token
		var found bool
		targetUID, cli, t, found = h.resolveUIDClient(uid)
		if !found {
			jsonErr(w, http.StatusNotFound, "账号不存在")
			return
		}
		label = accountDisplayLabel(t)
	}

	key := resourceCacheKey(targetUID)
	if !force {
		if payload, updatedAt, ok := h.st.GetCache("resource_cache", key, cfg.ResourceCacheSeconds); ok {
			var parsed any
			_ = json.Unmarshal([]byte(payload), &parsed)
			jsonWrite(w, http.StatusOK, map[string]any{
				"mode":       "single",
				"cached":     true,
				"updated_at": updatedAt,
				"uid":        targetUID,
				"label":      label,
				"data":       parsed,
			})
			return
		}
	}
	res, err := cli.Resources()
	if err != nil {
		jsonErr(w, http.StatusBadGateway, err.Error())
		return
	}
	now := time.Now()
	processed := processResources(res.Raw)
	h.writeResourceCache(targetUID, processed)
	if h.pool != nil && targetUID != "" {
		h.pool.UpdateQuotaFromResource(targetUID, billing.QuotaSummary(res.Data), now.Unix())
	}
	jsonWrite(w, http.StatusOK, map[string]any{
		"mode":       "single",
		"cached":     false,
		"updated_at": now.Unix(),
		"uid":        targetUID,
		"label":      label,
		"data":       processed,
	})
}

func (h *Handler) resourcesAll(w http.ResponseWriter, cfg config.Config, force bool) {
	type targetEntry struct {
		uid    string
		tok    *auth.Token
		cli    *upstream.Client
		active bool
	}
	regionDomain := config.EndpointOf(cfg.Region).Domain
	active := h.toks.Get()
	activeUID := ""
	if active != nil {
		activeUID = active.UID
	}

	var entries []targetEntry
	if h.pool != nil {
		for _, mUID := range h.pool.Members() {
			ts, err := h.pool.TokenStore(mUID)
			if err != nil || ts == nil {
				continue
			}
			t := ts.Get()
			if t == nil || !regionMatches(t.Domain, regionDomain) {
				continue
			}
			cli := h.client
			if mUID != activeUID {
				cli = h.client.WithTokenStore(ts)
			}
			entries = append(entries, targetEntry{
				uid: mUID, tok: t, cli: cli, active: mUID == activeUID,
			})
		}
	} else if active != nil && regionMatches(active.Domain, regionDomain) {
		entries = append(entries, targetEntry{
			uid: active.UID, tok: active, cli: h.client, active: true,
		})
	}
	// 主账号排在最前，其余保持成员序。
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].active && !entries[j].active
	})

	now := time.Now()
	accountsSummary := make([]resourceAccountSummary, 0, len(entries))
	mergedPackages := make([]mergedResourceAccount, 0)
	var (
		totalDosage           float64
		expiring7d            float64
		globalEarliestExpire  int64
		latestUpdatedAt       int64
	)

	for _, e := range entries {
		label := accountDisplayLabel(e.tok)
		sumItem := resourceAccountSummary{
			UID:         e.uid,
			Label:       label,
			Nickname:    e.tok.Nickname,
			Remark:      e.tok.Remark,
			Active:      e.active,
			RegionMatch: true,
		}

		var (
			pkgs      []resourceAccount
			acctTotal float64
			acctSum   billing.Summary
			updatedAt int64
			gotCache  bool
		)
		if !force {
			if payload, uAt, ok := h.st.GetCache("resource_cache", resourceCacheKey(e.uid), cfg.ResourceCacheSeconds); ok {
				var parsed struct {
					TotalDosage float64           `json:"total_dosage"`
					Accounts    []resourceAccount `json:"accounts"`
				}
				if err := json.Unmarshal([]byte(payload), &parsed); err == nil {
					pkgs = parsed.Accounts
					acctTotal = parsed.TotalDosage
					acctSum = summarizeResourcePackages(pkgs, acctTotal, now)
					updatedAt = uAt
					gotCache = true
				}
			}
		}

		if !gotCache {
			res, err := e.cli.Resources()
			if err != nil {
				sumItem.Error = err.Error()
				accountsSummary = append(accountsSummary, sumItem)
				continue
			}
			fetchAt := time.Now()
			processed := processResources(res.Raw)
			h.writeResourceCache(e.uid, processed)
			acctSum = billing.QuotaSummary(res.Data)
			if h.pool != nil {
				h.pool.UpdateQuotaFromResource(e.uid, acctSum, fetchAt.Unix())
			}
			if rawPkgs, ok := processed["accounts"].([]resourceAccount); ok {
				pkgs = rawPkgs
			}
			if td, ok := processed["total_dosage"].(float64); ok {
				acctTotal = td
			} else {
				acctTotal = acctSum.Total
			}
			updatedAt = fetchAt.Unix()
		}

		sumItem.TotalDosage = math.Round(acctTotal*100) / 100
		sumItem.EarliestExpireAt = acctSum.EarliestExpireAt
		sumItem.ExpiringCredits = acctSum.ExpiringCredits
		sumItem.UpdatedAt = updatedAt
		accountsSummary = append(accountsSummary, sumItem)

		totalDosage += sumItem.TotalDosage
		if updatedAt > latestUpdatedAt {
			latestUpdatedAt = updatedAt
		}
		if acctSum.EarliestExpireAt > 0 && (globalEarliestExpire == 0 || acctSum.EarliestExpireAt < globalEarliestExpire) {
			globalEarliestExpire = acctSum.EarliestExpireAt
		}
		for _, pkg := range pkgs {
			merged := mergedResourceAccount{
				resourceAccount: pkg,
				AccountUID:      e.uid,
				AccountLabel:    label,
			}
			// 全部模式下同一额度包可能在不同账号间指纹相同，叠加 uid 保证 Alpine :key 全局唯一。
			merged.Key = e.uid + ":" + pkg.Key
			if !pkg.Expired && pkg.CapacityRemain > 0 && pkg.DaysLeft != nil && *pkg.DaysLeft <= 7 {
				expiring7d += pkg.CapacityRemain
			}
			mergedPackages = append(mergedPackages, merged)
		}
	}

	sort.SliceStable(mergedPackages, func(i, j int) bool {
		ei, ej := mergedPackages[i].ExpireAt, mergedPackages[j].ExpireAt
		switch {
		case ei == 0 && ej == 0:
			return false
		case ei == 0:
			return false // 无到期时间排最后
		case ej == 0:
			return true
		default:
			return ei < ej
		}
	})
	if latestUpdatedAt == 0 && len(entries) > 0 {
		latestUpdatedAt = now.Unix()
	}

	jsonWrite(w, http.StatusOK, map[string]any{
		"mode":               "all",
		"updated_at":         latestUpdatedAt,
		"total_dosage":       math.Round(totalDosage*100) / 100,
		"expiring_7d":        math.Round(expiring7d*100) / 100,
		"earliest_expire_at": globalEarliestExpire,
		"accounts_summary":   accountsSummary,
		"accounts":           mergedPackages,
	})
}

// summarizeResourcePackages 从已加工的 resourceAccount 切片推导 CST 到期摘要（缓存命中时复用）。
func summarizeResourcePackages(pkgs []resourceAccount, total float64, now time.Time) billing.Summary {
	sum := billing.Summary{Total: math.Round(total*100) / 100}
	for _, p := range pkgs {
		if p.CapacityRemain <= 0 || p.ExpireAt <= 0 {
			continue
		}
		et := time.Unix(p.ExpireAt, 0)
		if et.Before(now) {
			continue
		}
		if sum.EarliestExpireAt == 0 || p.ExpireAt < sum.EarliestExpireAt {
			sum.EarliestExpireAt = p.ExpireAt
		}
	}
	if sum.EarliestExpireAt == 0 {
		return sum
	}
	earliestDay := time.Unix(sum.EarliestExpireAt, 0).In(billing.TZ).Format("2006-01-02")
	var expiring float64
	for _, p := range pkgs {
		if p.CapacityRemain <= 0 || p.ExpireAt <= 0 {
			continue
		}
		et := time.Unix(p.ExpireAt, 0)
		if et.Before(now) {
			continue
		}
		if et.In(billing.TZ).Format("2006-01-02") == earliestDay {
			expiring += p.CapacityRemain
		}
	}
	sum.ExpiringCredits = math.Round(expiring*100) / 100
	return sum
}

// processResources 解包 data.Response.Data 并本地加工。
func processResources(raw json.RawMessage) map[string]any {
	var top map[string]any
	if err := json.Unmarshal(raw, &top); err != nil {
		return map[string]any{"raw": json.RawMessage(raw)}
	}
	node := top
	for _, k := range []string{"data", "Response", "Data"} {
		if next, ok := node[k].(map[string]any); ok {
			node = next
		}
	}
	out := map[string]any{
		"upstream_total_dosage": number(node["TotalDosage"]), // 官方原始值（账户级聚合，含周期外幻影），仅供对照
		"total_count":           number(node["TotalCount"]),
		"raw":                   top,
	}
	now := time.Now()
	var accounts []resourceAccount
	if rawAccounts, ok := node["Accounts"].([]any); ok {
		for _, ra := range rawAccounts {
			acc, ok := ra.(map[string]any)
			if !ok {
				continue
			}
			a := resourceAccount{
				PackageName:   strOr(acc, "PackageName", "ProductName"),
				ProductName:   str(acc, "ProductName"),
				CapacityUnit:  toStr(acc["CapacityUnit"]),
				PackageType:   toStr(acc["PackageType"]),
				ResourceType:  acc["ResourceType"],
				AutoRenewFlag: acc["AutoRenewFlag"],
				Status:        acc["Status"],
			}
			// 账户级与周期级分别取值（Precise → 非Precise 回退）。
			acctRemain := numField(acc, "CapacityRemainPrecise", "CapacityRemain")
			acctSize := numField(acc, "CapacitySizePrecise", "CapacitySize")
			acctUsed := numField(acc, "CapacityUsedPrecise", "CapacityUsed")
			cycRemain := numField(acc, "CycleCapacityRemainPrecise", "CycleCapacityRemain")
			cycUsed := numField(acc, "CycleCapacityUsedPrecise", "CycleCapacityUsed")
			cycSize := numField(acc, "CycleCapacitySizePrecise", "CycleCapacitySize")
			if cycSize == 0 {
				cycSize = acctSize // 周期总量缺失回退账户总量（与参考契约一致）
			}
			// 有效展示值：存在周期数据（剩余或已用 > 0）时优先用周期级，否则用账户级。
			// CodeBuddy 免费/裂变包按月计费，账户级 CapacityUsed 恒为 0，真实已用
			// 落在 CycleCapacityUsed；不做周期优先会让「已用」恒显 0、进度条恒满。
			a.CycleRemain, a.CycleUsed, a.CycleSize = cycRemain, cycUsed, cycSize
			if cycRemain > 0 || cycUsed > 0 {
				a.CapacityRemain, a.CapacitySize, a.CapacityUsed = cycRemain, cycSize, cycUsed
			} else {
				a.CapacityRemain, a.CapacitySize, a.CapacityUsed = acctRemain, acctSize, acctUsed
			}
			// 到期时间按上游契约回退：ExpiredTime → DeductionEndTime → CycleEndTime。
			// 实测免费/裂变包 ExpiredTime 常为空，真正到期落在 Deduction/Cycle 字段。
			disp, et, ok := resolveExpireTime(acc)
			a.ExpireTime = disp
			if ok {
				a.ExpireAt = et.Unix()
				days := int(et.Sub(now).Hours() / 24)
				a.DaysLeft = &days
				a.Expired = et.Before(now)
				switch {
				case a.Expired:
					a.Warn = "expired"
				case days <= 7:
					a.Warn = "7d"
				case days <= 30:
					a.Warn = "30d"
				}
			}
			accounts = append(accounts, a)
		}
	}
	// 内容指纹（见 resourceAccount.Key）须在全部字段赋值完成后计算，故单独遍历一轮。
	for i := range accounts {
		accounts[i].Key = resourceAccountKey(accounts[i])
	}
	// 可用额度 total_dosage = Σ 未过期包的有效剩余（capacity_remain，周期优先口径），
	// 与包卡片「剩余」合计一致；到期未知（days_left == nil）的包视为有效，纳入累计。
	// 不透传上游 TotalDosage：该字段按账户级 CapacityRemain 聚合，而按月周期计费的
	// 包（体验版/裂变包）周期额度用尽后账户级 Remain 不随周期扣减，周期外幻影剩余
	// 会被一直计入（实测体验版恒多算 500，真实可用为 0 时总额度仍显示 500）。
	// 官方原始值保留在 upstream_total_dosage 供前端对照展示。
	available := 0.0
	for _, a := range accounts {
		if !a.Expired {
			available += a.CapacityRemain
		}
	}
	out["total_dosage"] = math.Round(available*100) / 100

	// 按到期时间升序（未知的排最后）
	sort.Slice(accounts, func(i, j int) bool {
		pi, pj := accounts[i].DaysLeft, accounts[j].DaysLeft
		switch {
		case pi == nil && pj == nil:
			return false
		case pi == nil:
			return false // i 未知 → 排后
		case pj == nil:
			return true // j 未知 → i 排前
		default:
			return *pi < *pj
		}
	})
	out["accounts"] = accounts
	return out
}

// ── 签到 ──

func (h *Handler) checkinRandomTarget(uid string) string {
	key := scheduler.CheckinRandomTargetKey(uid)
	if payload, _, ok := h.st.GetCache(scheduler.CheckinCacheTable, key, 48*3600); ok {
		if t, err := time.Parse(time.RFC3339, payload); err == nil && t.Format("2006-01-02") == time.Now().Format("2006-01-02") {
			return t.Format("15:04")
		}
	}
	return ""
}

func (h *Handler) checkinStatus(w http.ResponseWriter, r *http.Request) {
	reqUID := strings.TrimSpace(r.URL.Query().Get("uid"))
	var (
		targetUID string
		cli       *upstream.Client
	)
	if reqUID == "" {
		if t := h.toks.Get(); t != nil {
			targetUID = t.UID
		}
		cli = h.client
	} else {
		var found bool
		targetUID, cli, _, found = h.resolveUIDClient(reqUID)
		if !found {
			jsonErr(w, http.StatusNotFound, "账号不存在")
			return
		}
	}

	randomTarget := h.checkinRandomTarget(targetUID)
	statusKey := scheduler.CheckinStatusKey(targetUID)
	// 优先读缓存（60s），避免高频查询；同时兼容旧版无后缀键（仅无 uid 参数时回退）
	if payload, updatedAt, ok := h.st.GetCache(scheduler.CheckinCacheTable, statusKey, 60); ok {
		var parsed any
		_ = json.Unmarshal([]byte(payload), &parsed)
		jsonWrite(w, http.StatusOK, map[string]any{"cached": true, "updated_at": updatedAt, "uid": targetUID, "random_target": randomTarget, "data": parsed})
		return
	}
	if reqUID == "" && statusKey != scheduler.CheckinKeyStatus {
		if payload, updatedAt, ok := h.st.GetCache(scheduler.CheckinCacheTable, scheduler.CheckinKeyStatus, 60); ok {
			var parsed any
			_ = json.Unmarshal([]byte(payload), &parsed)
			jsonWrite(w, http.StatusOK, map[string]any{"cached": true, "updated_at": updatedAt, "uid": targetUID, "random_target": randomTarget, "data": parsed})
			return
		}
	}

	res, err := cli.CheckinStatus()
	if err != nil {
		jsonErr(w, http.StatusBadGateway, err.Error())
		return
	}
	_ = h.st.SetCache(scheduler.CheckinCacheTable, statusKey, string(res.Raw))
	var parsed any
	_ = json.Unmarshal(res.Raw, &parsed)
	jsonWrite(w, http.StatusOK, map[string]any{"cached": false, "updated_at": time.Now().Unix(), "uid": targetUID, "random_target": randomTarget, "data": parsed})
}

func (h *Handler) checkinClaim(w http.ResponseWriter, r *http.Request) {
	reqUID := strings.TrimSpace(r.URL.Query().Get("uid"))
	var (
		targetUID string
		cli       *upstream.Client
	)
	if reqUID == "" {
		if t := h.toks.Get(); t != nil {
			targetUID = t.UID
		}
		cli = h.client
	} else {
		var found bool
		targetUID, cli, _, found = h.resolveUIDClient(reqUID)
		if !found {
			jsonErr(w, http.StatusNotFound, "账号不存在")
			return
		}
	}
	res, err := cli.ClaimCheckin()
	if err != nil {
		jsonErr(w, http.StatusBadGateway, err.Error())
		return
	}
	// 清掉状态缓存，确保前端紧接着 loadCheckin/loadCheckinSummary 拿到最新的 today_checked_in / streak_days。
	_ = h.st.DeleteCacheKey(scheduler.CheckinCacheTable, scheduler.CheckinStatusKey(targetUID))
	if reqUID == "" {
		_ = h.st.DeleteCacheKey(scheduler.CheckinCacheTable, scheduler.CheckinKeyStatus)
	}
	var parsed any
	_ = json.Unmarshal(res.Raw, &parsed)
	jsonWrite(w, http.StatusOK, map[string]any{"ok": true, "uid": targetUID, "data": parsed})
}

type checkinSummaryItem struct {
	UID         string  `json:"uid"`
	Label       string  `json:"label"`
	Active      bool    `json:"active"`
	RegionMatch bool    `json:"region_match"`
	CheckedIn   bool    `json:"checked_in"`
	TodayCredit float64 `json:"today_credit"`
	StreakDays  int     `json:"streak_days"`
	Error       string  `json:"error,omitempty"`
}

type checkinSummaryResp struct {
	Items        []checkinSummaryItem `json:"items"`
	CheckedCount int                  `json:"checked_count"`
	TotalCount   int                  `json:"total_count"`
}

func (h *Handler) eligibleCheckinUIDs() []string {
	if cs, ok := h.sched.(checkinForScheduler); ok {
		return cs.EligibleAccountUIDs()
	}
	regionDomain := config.EndpointOf(h.cfg.Get().Region).Domain
	if h.pool != nil {
		var out []string
		for _, uid := range h.pool.Members() {
			ts, err := h.pool.TokenStore(uid)
			if err != nil || ts == nil || ts.Get() == nil {
				continue
			}
			if st, ok := h.pool.AccountStateOf(uid); ok && st.AutoDisabled == pool.AutoCredential {
				continue
			}
			if t := ts.Get(); !regionMatches(t.Domain, regionDomain) {
				continue
			}
			out = append(out, uid)
		}
		return out
	}
	if t := h.toks.Get(); t != nil && regionMatches(t.Domain, regionDomain) && t.UID != "" {
		return []string{t.UID}
	}
	return nil
}

func (h *Handler) buildCheckinSummary() checkinSummaryResp {
	uids := h.eligibleCheckinUIDs()
	regionDomain := config.EndpointOf(h.cfg.Get().Region).Domain
	active := h.toks.Get()
	activeUID := ""
	if active != nil {
		activeUID = active.UID
	}

	items := make([]checkinSummaryItem, 0, len(uids))
	checkedCount := 0
	for _, uid := range uids {
		_, cli, t, ok := h.resolveUIDClient(uid)
		if !ok || t == nil {
			continue
		}
		item := checkinSummaryItem{
			UID:         uid,
			Label:       accountDisplayLabel(t),
			Active:      uid == activeUID,
			RegionMatch: regionMatches(t.Domain, regionDomain),
		}
		statusKey := scheduler.CheckinStatusKey(uid)
		var raw []byte
		if payload, _, hit := h.st.GetCache(scheduler.CheckinCacheTable, statusKey, 60); hit && payload != "" {
			raw = []byte(payload)
		} else {
			res, err := cli.CheckinStatus()
			if err != nil {
				item.Error = err.Error()
				items = append(items, item)
				continue
			}
			raw = res.Raw
			_ = h.st.SetCache(scheduler.CheckinCacheTable, statusKey, string(raw))
		}
		var env struct {
			Data struct {
				TodayCheckedIn bool    `json:"today_checked_in"`
				TodayCredit    float64 `json:"today_credit"`
				StreakDays     int     `json:"streak_days"`
			} `json:"data"`
		}
		if err := json.Unmarshal(raw, &env); err == nil {
			item.CheckedIn = env.Data.TodayCheckedIn
			item.TodayCredit = env.Data.TodayCredit
			item.StreakDays = env.Data.StreakDays
		}
		if item.CheckedIn {
			checkedCount++
		}
		items = append(items, item)
	}
	return checkinSummaryResp{
		Items:        items,
		CheckedCount: checkedCount,
		TotalCount:   len(items),
	}
}

func (h *Handler) checkinSummary(w http.ResponseWriter, r *http.Request) {
	jsonWrite(w, http.StatusOK, h.buildCheckinSummary())
}

func (h *Handler) checkinClaimAll(w http.ResponseWriter, r *http.Request) {
	type claimResult struct {
		UID            string  `json:"uid"`
		Label          string  `json:"label"`
		Status         string  `json:"status"` // claimed | skipped | failed
		Credit         float64 `json:"credit,omitempty"`
		AlreadyChecked bool    `json:"already_checked,omitempty"`
		Error          string  `json:"error,omitempty"`
	}

	uids := h.eligibleCheckinUIDs()
	cs, hasSchedClaimer := h.sched.(checkinForScheduler)
	results := make([]claimResult, 0, len(uids))
	claimed, skipped, failed := 0, 0, 0

	for _, uid := range uids {
		_, cli, t, ok := h.resolveUIDClient(uid)
		if !ok || t == nil {
			continue
		}
		label := accountDisplayLabel(t)
		var (
			credit  float64
			already bool
			err     error
		)
		if hasSchedClaimer {
			credit, already, err = cs.ClaimCheckinFor(uid)
		} else {
			credit, already, err = claimCheckinDirect(cli)
		}
		switch {
		case err != nil:
			failed++
			results = append(results, claimResult{UID: uid, Label: label, Status: "failed", Error: err.Error()})
		case already:
			skipped++
			results = append(results, claimResult{UID: uid, Label: label, Status: "skipped", AlreadyChecked: true})
		default:
			claimed++
			// 清除由 daily-checkin 写入的部分响应缓存，使紧随其后的 summary 拉取完整的 streak_days/today_credit。
			_ = h.st.DeleteCacheKey(scheduler.CheckinCacheTable, scheduler.CheckinStatusKey(uid))
			results = append(results, claimResult{UID: uid, Label: label, Status: "claimed", Credit: credit})
		}
	}

	summary := h.buildCheckinSummary()
	jsonWrite(w, http.StatusOK, map[string]any{
		"ok":      true,
		"claimed": claimed,
		"skipped": skipped,
		"failed":  failed,
		"results": results,
		"summary": summary,
	})
}

func claimCheckinDirect(cli *upstream.Client) (float64, bool, error) {
	if res, serr := cli.CheckinStatus(); serr == nil {
		var env struct {
			Data struct {
				TodayCheckedIn bool `json:"today_checked_in"`
			} `json:"data"`
		}
		if json.Unmarshal(res.Raw, &env) == nil && env.Data.TodayCheckedIn {
			return 0, true, nil
		}
	}
	res, err := cli.ClaimCheckin()
	if err != nil {
		return 0, false, err
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
		return env.Data.Credit, env.Data.Credit <= 0 && env.Data.TodayCheckedIn, nil
	}
	return 0, false, fmt.Errorf("签到未成功: code=%d msg=%s", env.Code, env.Msg)
}

// ── keys ──

func (h *Handler) listKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := h.keys.List()
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonWrite(w, http.StatusOK, map[string]any{"keys": keys})
}

func (h *Handler) createKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name      string `json:"name"`
		CustomKey string `json:"custom_key"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	k, err := h.keys.Create(req.Name, req.CustomKey)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	jsonWrite(w, http.StatusOK, map[string]any{"key": k})
}

func (h *Handler) updateKey(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		jsonErr(w, http.StatusBadRequest, "id 非法")
		return
	}
	var req struct {
		Name   *string `json:"name"`
		Status *string `json:"status"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if err := h.keys.Update(id, req.Name, req.Status); err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	k, _ := h.st.GetKey(id)
	jsonWrite(w, http.StatusOK, map[string]any{"key": k})
}

func (h *Handler) deleteKey(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		jsonErr(w, http.StatusBadRequest, "id 非法")
		return
	}
	if err := h.keys.Delete(id); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonWrite(w, http.StatusOK, map[string]any{"ok": true})
}

// ── 日志 / 统计 ──

func (h *Handler) logs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	accountUID := strings.TrimSpace(q.Get("account_uid"))
	if accountUID != "" {
		if resolved, _, _, ok := h.resolveUIDClient(accountUID); ok && resolved != "" {
			accountUID = resolved
		} else if archived, err := h.accountBook().Load(accountUID); err == nil && archived != nil && archived.UID != "" {
			accountUID = archived.UID
		}
	}
	f := store.LogFilter{
		Model:      strings.TrimSpace(q.Get("model")),
		AccountUID: accountUID,
		Status:     0,
		Page:       atoiDefault(q.Get("page"), 1),
		PageSize:   atoiDefault(q.Get("page_size"), 20),
	}
	if v := q.Get("key_id"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			f.KeyID = n
		}
	}
	switch q.Get("status") {
	case "error":
		f.Status = -1
	case "":
	default:
		if n, err := strconv.Atoi(q.Get("status")); err == nil {
			f.Status = n
		}
	}
	items, total, err := h.st.QueryLogs(f)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonWrite(w, http.StatusOK, map[string]any{"logs": items, "total": total, "page": f.Page, "page_size": f.PageSize})
}

func (h *Handler) stats(w http.ResponseWriter, r *http.Request) {
	rng := strings.TrimSpace(r.URL.Query().Get("range"))
	if rng == "" {
		rng = store.StatsRangeAll
	}
	if !store.ValidStatsRange(rng) {
		jsonErr(w, http.StatusBadRequest, "不支持的统计范围: "+rng+"（仅支持 all/24h/7d/30d/90d）")
		return
	}
	st, err := h.st.GetStatsRange(rng)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonWrite(w, http.StatusOK, st)
}

// ── 设置 ──

func (h *Handler) getSettings(w http.ResponseWriter, r *http.Request) {
	cfg := h.cfg.Get()
	jsonWrite(w, http.StatusOK, map[string]any{
		"listen":                 cfg.Listen,
		"region":                 cfg.Region,
		"auto_checkin":           cfg.AutoCheckin,
		"checkin_cron":           cfg.CheckinCron,
		"checkin_mode":           cfg.CheckinMode,
		"checkin_random_start":   cfg.CheckinRandomStart,
		"checkin_random_end":     cfg.CheckinRandomEnd,
		"checkin_fallback":       cfg.CheckinFallback,
		"auto_growth":            cfg.AutoGrowth,
		"growth_report_cron":     cfg.GrowthReportCron,
		"growth_travel_cron":     cfg.GrowthTravelCron,
		"growth_report_count":    cfg.GrowthReportCount,
		"growth_report_jitter":   cfg.GrowthReportJitter,
		"growth_queue_cron":      cfg.GrowthQueueCron,
		"blackcat_cron":          cfg.BlackcatCron,
		"task_sweep_enabled":     cfg.TaskSweepEnabled,
		"task_sweep_minutes":     cfg.TaskSweepMinutes,
		"resource_cache_seconds": cfg.ResourceCacheSeconds,
		"log_retention_days":     cfg.LogRetentionDays,
		"log_max_size_mb":        cfg.LogMaxSizeMB,
		"chat_timeout_seconds":   cfg.ChatTimeoutSeconds,
		"pool_strategy":          cfg.PoolStrategy,
		"pool_session_ttl":       cfg.PoolSessionTTL,
		"pool_max_retry":         cfg.PoolMaxRetry,
		"pool_cool_max_min":      cfg.PoolCoolMaxMin,
		"pool_probe_cron":        cfg.PoolProbeCron,
	})
}

func (h *Handler) putSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OldPassword          *string `json:"old_password"`
		NewPassword          *string `json:"new_password"`
		Listen               *string `json:"listen"`
		Region               *string `json:"region"`
		AutoCheckin          *bool   `json:"auto_checkin"`
		CheckinCron          *string `json:"checkin_cron"`
		CheckinMode          *string `json:"checkin_mode"`
		CheckinRandomStart   *string `json:"checkin_random_start"`
		CheckinRandomEnd     *string `json:"checkin_random_end"`
		CheckinFallback      *bool   `json:"checkin_fallback"`
		AutoGrowth           *bool   `json:"auto_growth"`
		GrowthReportCron     *string `json:"growth_report_cron"`
		GrowthTravelCron     *string `json:"growth_travel_cron"`
		GrowthReportCount    *int    `json:"growth_report_count"`
		GrowthReportJitter   *int    `json:"growth_report_jitter"`
		GrowthQueueCron      *string `json:"growth_queue_cron"`
		BlackcatCron         *string `json:"blackcat_cron"`
		TaskSweepEnabled     *bool   `json:"task_sweep_enabled"`
		TaskSweepMinutes     *int    `json:"task_sweep_minutes"`
		ResourceCacheSeconds *int    `json:"resource_cache_seconds"`
		LogRetentionDays     *int    `json:"log_retention_days"`
		LogMaxSizeMB         *int    `json:"log_max_size_mb"`
		ChatTimeoutSeconds   *int    `json:"chat_timeout_seconds"`
		PoolStrategy         *string `json:"pool_strategy"`
		PoolSessionTTL       *int    `json:"pool_session_ttl"`
		PoolMaxRetry         *int    `json:"pool_max_retry"`
		PoolCoolMaxMin       *int    `json:"pool_cool_max_min"`
		PoolProbeCron        *string `json:"pool_probe_cron"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	oldCfg := h.cfg.Get()
	regionChanged := false

	// 改密码（独立处理：需验证旧密码）
	if req.NewPassword != nil {
		if old := deref(req.OldPassword); old == "" || !h.cfg.VerifyAdminPassword(old) {
			jsonErr(w, http.StatusBadRequest, "旧密码错误")
			return
		}
		if len(*req.NewPassword) < 6 {
			jsonErr(w, http.StatusBadRequest, "新密码至少 6 位")
			return
		}
		if err := h.cfg.SetAdminPasswordHash(hashPassword(*req.NewPassword)); err != nil {
			jsonErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	// 监听地址格式校验（保存后需用户自行重启生效）
	if req.Listen != nil {
		if err := config.ValidateListen(*req.Listen); err != nil {
			jsonErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	err := h.cfg.Update(func(c *config.Config) error {
		if req.Listen != nil {
			c.Listen = strings.TrimSpace(*req.Listen)
		}
		if req.Region != nil {
			if *req.Region != "cn" && *req.Region != "global" {
				return fmt.Errorf("region 仅支持 cn/global")
			}
			if *req.Region != c.Region {
				regionChanged = true
				c.Region = *req.Region
			}
		}
		if req.AutoCheckin != nil {
			c.AutoCheckin = *req.AutoCheckin
		}
		if req.CheckinCron != nil {
			if !validCron(*req.CheckinCron) {
				return fmt.Errorf("cron 表达式非法（需 6 段含秒，如 0 0 9 * * *）")
			}
			c.CheckinCron = *req.CheckinCron
		}
		if req.CheckinMode != nil {
			if m := strings.TrimSpace(*req.CheckinMode); m != "fixed" && m != "random" {
				return fmt.Errorf("签到方式仅支持 fixed/random")
			} else {
				c.CheckinMode = m
			}
		}
		if req.CheckinRandomStart != nil || req.CheckinRandomEnd != nil {
			start, end := c.CheckinRandomStart, c.CheckinRandomEnd
			if req.CheckinRandomStart != nil {
				start = strings.TrimSpace(*req.CheckinRandomStart)
			}
			if req.CheckinRandomEnd != nil {
				end = strings.TrimSpace(*req.CheckinRandomEnd)
			}
			if err := config.ValidateCheckinWindow(start, end); err != nil {
				return err
			}
			c.CheckinRandomStart, c.CheckinRandomEnd = start, end
		}
		if req.CheckinFallback != nil {
			c.CheckinFallback = *req.CheckinFallback
		}
		if req.AutoGrowth != nil {
			c.AutoGrowth = *req.AutoGrowth
		}
		if req.GrowthReportCron != nil {
			if !validCron(*req.GrowthReportCron) {
				return fmt.Errorf("上报链 cron 表达式非法（需 6 段含秒，如 0 0 10 * * *）")
			}
			c.GrowthReportCron = strings.TrimSpace(*req.GrowthReportCron)
		}
		if req.GrowthTravelCron != nil {
			if !validCron(*req.GrowthTravelCron) {
				return fmt.Errorf("旅行巡检 cron 表达式非法（需 6 段含秒，如 0 0 9,21 * * *）")
			}
			c.GrowthTravelCron = strings.TrimSpace(*req.GrowthTravelCron)
		}
		if req.GrowthQueueCron != nil {
			if !validCron(*req.GrowthQueueCron) {
				return fmt.Errorf("任务队列 cron 表达式非法（需 6 段含秒，如 0 0 1 * * *）")
			}
			c.GrowthQueueCron = strings.TrimSpace(*req.GrowthQueueCron)
		}
		if req.BlackcatCron != nil {
			if !validCron(*req.BlackcatCron) {
				return fmt.Errorf("夜猫子 cron 表达式非法（需 6 段含秒，如 0 0 23 * * *）")
			}
			c.BlackcatCron = strings.TrimSpace(*req.BlackcatCron)
		}
		if req.TaskSweepEnabled != nil {
			c.TaskSweepEnabled = *req.TaskSweepEnabled
		}
		if req.TaskSweepMinutes != nil {
			n := *req.TaskSweepMinutes
			if n < 1 {
				n = 1
			}
			if n > config.TaskSweepMinutesMax {
				n = config.TaskSweepMinutesMax
			}
			c.TaskSweepMinutes = n
		}
		if req.GrowthReportCount != nil {
			n := *req.GrowthReportCount
			if n < 1 {
				n = 1
			}
			if n > config.GrowthReportCountMax {
				n = config.GrowthReportCountMax // 钳制防风控，不报错（与 Normalize 口径一致）
			}
			c.GrowthReportCount = n
		}
		if req.GrowthReportJitter != nil {
			n := *req.GrowthReportJitter
			if n < 0 {
				n = 0
			}
			if n > config.GrowthReportJitterMax {
				n = config.GrowthReportJitterMax
			}
			c.GrowthReportJitter = n
		}
		if req.ResourceCacheSeconds != nil && *req.ResourceCacheSeconds > 0 {
			c.ResourceCacheSeconds = *req.ResourceCacheSeconds
		}
		if req.LogRetentionDays != nil && *req.LogRetentionDays > 0 {
			c.LogRetentionDays = *req.LogRetentionDays
		}
		if req.LogMaxSizeMB != nil && *req.LogMaxSizeMB > 0 {
			c.LogMaxSizeMB = *req.LogMaxSizeMB
		}
		// 写入 Transport 的构建参数，保存后需重启生效（同 listen）
		if req.ChatTimeoutSeconds != nil {
			if err := config.ValidateChatTimeoutSeconds(*req.ChatTimeoutSeconds); err != nil {
				return err
			}
			c.ChatTimeoutSeconds = *req.ChatTimeoutSeconds
		}
		if req.PoolStrategy != nil {
			s := strings.TrimSpace(*req.PoolStrategy)
			if !config.ValidPoolStrategy(s) {
				return fmt.Errorf("pool_strategy 仅支持 expire_first/balanced/credits_first")
			}
			c.PoolStrategy = s
		}
		if req.PoolSessionTTL != nil {
			n := *req.PoolSessionTTL
			if n < 5 {
				n = 5
			}
			if n > config.PoolSessionTTLMax {
				n = config.PoolSessionTTLMax
			}
			c.PoolSessionTTL = n
		}
		if req.PoolMaxRetry != nil {
			n := *req.PoolMaxRetry
			if n < 0 {
				n = 0
			}
			if n > config.PoolMaxRetryMax {
				n = config.PoolMaxRetryMax
			}
			c.PoolMaxRetry = n
		}
		if req.PoolCoolMaxMin != nil {
			n := *req.PoolCoolMaxMin
			if n < 5 {
				n = 5
			}
			if n > config.PoolCoolMaxMinMax {
				n = config.PoolCoolMaxMinMax
			}
			c.PoolCoolMaxMin = n
		}
		if req.PoolProbeCron != nil {
			if !validCron(*req.PoolProbeCron) {
				return fmt.Errorf("号池探测 cron 表达式非法（需 6 段含秒，如 0 0 0,6,12,18 * * *）")
			}
			c.PoolProbeCron = strings.TrimSpace(*req.PoolProbeCron)
		}
		// 存量 cron 兜底：请求未携带该字段时（前端在功能关闭时不下发），
		// 校验存量值；非法（常见来源是 env 写错位数）则回落默认，
		// 避免一条用户看不见/改不了的非法 cron 永久阻断无关设置保存。
		if req.CheckinCron == nil && !validCron(c.CheckinCron) {
			slog.Warn("存量 checkin_cron 非法，回落默认", "value", c.CheckinCron)
			c.CheckinCron = config.DefaultCheckinCron
		}
		if req.GrowthReportCron == nil && !validCron(c.GrowthReportCron) {
			slog.Warn("存量 growth_report_cron 非法，回落默认", "value", c.GrowthReportCron)
			c.GrowthReportCron = config.DefaultGrowthReportCron
		}
		if req.GrowthTravelCron == nil && !validCron(c.GrowthTravelCron) {
			slog.Warn("存量 growth_travel_cron 非法，回落默认", "value", c.GrowthTravelCron)
			c.GrowthTravelCron = config.DefaultGrowthTravelCron
		}
		if req.GrowthQueueCron == nil && !validCron(c.GrowthQueueCron) {
			slog.Warn("存量 growth_queue_cron 非法，回落默认", "value", c.GrowthQueueCron)
			c.GrowthQueueCron = config.DefaultGrowthQueueCron
		}
		if req.BlackcatCron == nil && !validCron(c.BlackcatCron) {
			slog.Warn("存量 blackcat_cron 非法，回落默认", "value", c.BlackcatCron)
			c.BlackcatCron = config.DefaultBlackcatCron
		}
		if req.PoolProbeCron == nil && !validCron(c.PoolProbeCron) {
			slog.Warn("存量 pool_probe_cron 非法，回落默认", "value", c.PoolProbeCron)
			c.PoolProbeCron = config.DefaultPoolProbeCron
		}
		return nil
	})
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}

	// region 切换 → 立即重新拉取模型列表
	if regionChanged {
		if err := h.models.Refresh(h.client); err != nil {
			// 失败已回退内置表，不阻断设置保存
			_ = err
		}
	}
	// 签到/成长/探测开关或 cron 变化 → 重装配定时任务
	if h.sched != nil {
		h.sched.Reconfigure()
	}
	_ = oldCfg

	h.getSettings(w, r)
}

// ── 实验特性 ──

// getExperiments 读实验特性开关（GET /admin/experiments）。
// 多账号调度已升级为原生常驻，此处仅保留 embeddings_enabled。
func (h *Handler) getExperiments(w http.ResponseWriter, r *http.Request) {
	cfg := h.cfg.Get()
	jsonWrite(w, http.StatusOK, map[string]any{
		"embeddings_enabled": cfg.EmbeddingsEnabled,
	})
}

// putExperiments 改实验特性开关（PUT /admin/experiments）。
// 兼容忽略旧版请求体里的 pool_* 字段（不报错、不改写）。
func (h *Handler) putExperiments(w http.ResponseWriter, r *http.Request) {
	var req struct {
		EmbeddingsEnabled *bool `json:"embeddings_enabled"`
		PoolEnabled       any   `json:"pool_enabled"`
		PoolSessionTTL    any   `json:"pool_session_ttl"`
		PoolMaxRetry      any   `json:"pool_max_retry"`
		PoolCoolMaxMin    any   `json:"pool_cool_max_min"`
		PoolProbeCron     any   `json:"pool_probe_cron"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	hasLegacyPool := req.PoolEnabled != nil || req.PoolSessionTTL != nil ||
		req.PoolMaxRetry != nil || req.PoolCoolMaxMin != nil || req.PoolProbeCron != nil
	if req.EmbeddingsEnabled == nil && !hasLegacyPool {
		jsonErr(w, http.StatusBadRequest, "未提供任何可修改的字段")
		return
	}
	if req.EmbeddingsEnabled != nil {
		if err := h.cfg.Update(func(c *config.Config) error {
			c.EmbeddingsEnabled = *req.EmbeddingsEnabled
			return nil
		}); err != nil {
			jsonErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	h.getExperiments(w, r)
}

// ── 模型 ──

func (h *Handler) listModels(w http.ResponseWriter, r *http.Request) {
	jsonWrite(w, http.StatusOK, map[string]any{
		"ids":    h.models.List(),
		"models": h.models.View(), // 含当前生效倍率（基础倍率 + 折扣活动计算）
		"state":  h.models.State(),
	})
}

func (h *Handler) refreshModels(w http.ResponseWriter, r *http.Request) {
	if err := h.models.Refresh(h.client); err != nil {
		jsonErr(w, http.StatusBadGateway, "刷新失败（已回退/保留旧表）: "+err.Error())
		return
	}
	jsonWrite(w, http.StatusOK, map[string]any{"ids": h.models.List(), "state": h.models.State()})
}

// ── util ──

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	if n, err := strconv.Atoi(s); err == nil && n > 0 {
		return n
	}
	return def
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func str(m map[string]any, k string) string {
	v, _ := m[k].(string)
	return v
}

func toStr(v any) string {
	s, _ := v.(string)
	return s
}

func strOr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func number(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case string:
		var f float64
		_, _ = fmt.Sscanf(n, "%g", &f)
		return f
	}
	return 0
}

// numField 转发到 billing.NumField（实现与注释见该包：必须处理上游的字符串形态）。
func numField(acc map[string]any, keys ...string) float64 {
	return billing.NumField(acc, keys...)
}

// resolveExpireTime 转发到 billing.ResolveExpireTime（实现与注释见该包）。
func resolveExpireTime(acc map[string]any) (string, time.Time, bool) {
	return billing.ResolveExpireTime(acc)
}
