package admin

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"buddy2api-go/internal/apikey"
	"buddy2api-go/internal/auth"
	"buddy2api-go/internal/config"
	"buddy2api-go/internal/store"
	"buddy2api-go/internal/upstream"
)

// Reconfigurable 由 scheduler 实现，设置变更后重新装配定时任务。
type Reconfigurable interface{ Reconfigure() }

// Handler 管理后台 API 处理器。
type Handler struct {
	cfg     *config.Manager
	st      *store.Store
	toks    *auth.TokenStore
	client  *upstream.Client
	models  *upstream.ModelCache
	keys    *apikey.Manager
	sched   Reconfigurable
	session *Session
}

// New 创建处理器。
func New(cfg *config.Manager, st *store.Store, toks *auth.TokenStore, client *upstream.Client,
	models *upstream.ModelCache, keys *apikey.Manager, sched Reconfigurable, session *Session) *Handler {
	return &Handler{cfg: cfg, st: st, toks: toks, client: client, models: models, keys: keys, sched: sched, session: session}
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

			r.Get("/resources", h.resources)
			r.Get("/checkin/status", h.checkinStatus)
			r.Post("/checkin/claim", h.checkinClaim)

			r.Get("/api-keys", h.listKeys)
			r.Post("/api-keys", h.createKey)
			r.Put("/api-keys/{id}", h.updateKey)
			r.Delete("/api-keys/{id}", h.deleteKey)

			r.Get("/logs", h.logs)
			r.Get("/stats", h.stats)

			r.Get("/settings", h.getSettings)
			r.Put("/settings", h.putSettings)
			r.Get("/models", h.listModels)
			r.Post("/models/refresh", h.refreshModels)
		})
	}
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
	h.session.Issue(w)
	jsonWrite(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	h.session.Clear(w)
	jsonWrite(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) sessionCheck(w http.ResponseWriter, r *http.Request) {
	jsonWrite(w, http.StatusOK, map[string]any{"logged_in": h.session.Valid(r)})
}

// ── 账号 ──

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
		"enterprise_id": t.EnterpriseID,
		"nickname":      t.Nickname,
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
	if err := h.toks.Save(tok); err != nil {
		jsonErr(w, http.StatusInternalServerError, "保存凭证失败: "+err.Error())
		return
	}
	jsonWrite(w, http.StatusOK, map[string]any{
		"status": "success",
		"account": map[string]any{
			"uid": tok.UID, "nickname": tok.Nickname, "domain": tok.Domain,
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
	if err := h.toks.Clear(); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonWrite(w, http.StatusOK, map[string]any{"ok": true})
}

// accountExport 导出当前账号凭证为 token.json（下载附件）。
func (h *Handler) accountExport(w http.ResponseWriter, r *http.Request) {
	t := h.toks.Get()
	if t == nil {
		jsonErr(w, http.StatusBadRequest, "尚未登录，无凭证可导出")
		return
	}
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="token.json"`)
	w.Write(data)
}

// accountImport 导入账号凭证，必须含 access_token（合法 JWT），其余可选。
func (h *Handler) accountImport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		ExpiresAt    int64  `json:"expires_at"`
		Domain       string `json:"domain"`
		Nickname     string `json:"nickname"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	req.AccessToken = strings.TrimSpace(req.AccessToken)
	if req.AccessToken == "" {
		jsonErr(w, http.StatusBadRequest, "access_token 不能为空")
		return
	}
	if _, _, _, _, _, err := auth.ParseJWT(req.AccessToken); err != nil {
		jsonErr(w, http.StatusBadRequest, "access_token 不是合法 JWT: "+err.Error())
		return
	}
	t := &auth.Token{
		AccessToken:  req.AccessToken,
		RefreshToken: strings.TrimSpace(req.RefreshToken),
		TokenType:    strings.TrimSpace(req.TokenType),
		ExpiresAt:    req.ExpiresAt,
		Domain:       strings.TrimSpace(req.Domain),
		Nickname:     strings.TrimSpace(req.Nickname),
	}
	t.EnrichFromJWT()
	if t.TokenType == "" {
		t.TokenType = "Bearer"
	}
	if t.Domain == "" {
		t.Domain = config.EndpointOf(h.cfg.Get().Region).Domain
	}
	if err := h.toks.Save(t); err != nil {
		jsonErr(w, http.StatusInternalServerError, "保存凭证失败: "+err.Error())
		return
	}
	jsonWrite(w, http.StatusOK, map[string]any{
		"ok":       true,
		"uid":      t.UID,
		"nickname": t.Nickname,
		"domain":   t.Domain,
	})
}

// ── 余额 ──

// resourceAccount 额度包明细。
type resourceAccount struct {
	PackageName    string  `json:"package_name"`
	ProductName    string  `json:"product_name"`
	CapacityRemain float64 `json:"capacity_remain"`
	CapacitySize   float64 `json:"capacity_size"`
	CapacityUsed   float64 `json:"capacity_used"`
	CycleRemain    float64 `json:"cycle_remain"`
	CapacityUnit   string  `json:"capacity_unit"`
	PackageType    string  `json:"package_type"`
	ResourceType   any     `json:"resource_type"`
	AutoRenewFlag  any     `json:"auto_renew_flag"`
	Status         any     `json:"status"`
	ExpireTime     string  `json:"expire_time"`
	DaysLeft       *int    `json:"days_left"` // nil = 未知
	Expired        bool    `json:"expired"`
	Warn           string  `json:"warn"` // "7d" | "30d" | ""
}

func (h *Handler) resources(w http.ResponseWriter, r *http.Request) {
	cfg := h.cfg.Get()
	force := r.URL.Query().Get("force") == "1"
	table, key := "resource_cache", "default"

	if !force {
		if payload, updatedAt, ok := h.st.GetCache(table, key, cfg.ResourceCacheSeconds); ok {
			var parsed any
			_ = json.Unmarshal([]byte(payload), &parsed)
			jsonWrite(w, http.StatusOK, map[string]any{"cached": true, "updated_at": updatedAt, "data": parsed})
			return
		}
	}
	res, err := h.client.Resources()
	if err != nil {
		jsonErr(w, http.StatusBadGateway, err.Error())
		return
	}
	processed := processResources(res.Raw)
	// 缓存与返回同为加工后结构，保证命中缓存时前端形状一致
	if raw, err := json.Marshal(processed); err == nil {
		_ = h.st.SetCache(table, key, string(raw))
	}
	jsonWrite(w, http.StatusOK, map[string]any{"cached": false, "updated_at": time.Now().Unix(), "data": processed})
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
		"total_dosage": number(node["TotalDosage"]),
		"total_count":  number(node["TotalCount"]),
		"raw":          top,
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
				PackageName:    strOr(acc, "PackageName", "ProductName"),
				ProductName:    str(acc, "ProductName"),
				CapacityRemain: number(acc["CapacityRemainPrecise"]),
				CapacitySize:   number(acc["CapacitySizePrecise"]),
				CapacityUsed:   number(acc["CapacityUsedPrecise"]),
				CycleRemain:    number(acc["CycleCapacityRemainPrecise"]),
				CapacityUnit:   toStr(acc["CapacityUnit"]),
				PackageType:    toStr(acc["PackageType"]),
				ResourceType:   acc["ResourceType"],
				AutoRenewFlag:  acc["AutoRenewFlag"],
				Status:         acc["Status"],
				ExpireTime:     toStr(acc["ExpiredTime"]),
			}
			if t, ok := parseFlexibleTime(acc["ExpiredTime"]); ok {
				days := int(t.Sub(now).Hours() / 24)
				a.DaysLeft = &days
				a.Expired = t.Before(now)
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
	// 按到期时间升序（未知的排最后）
	for i := 0; i < len(accounts); i++ {
		for j := i + 1; j < len(accounts); j++ {
			pi, pj := accounts[i].DaysLeft, accounts[j].DaysLeft
			if pj != nil && (pi == nil || *pj < *pi) {
				accounts[i], accounts[j] = accounts[j], accounts[i]
			}
		}
	}
	out["accounts"] = accounts
	return out
}

// ── 签到 ──

func (h *Handler) checkinStatus(w http.ResponseWriter, r *http.Request) {
	// 优先读缓存（60s），避免高频查询
	if payload, updatedAt, ok := h.st.GetCache("checkin_cache", "status", 60); ok {
		var parsed any
		_ = json.Unmarshal([]byte(payload), &parsed)
		jsonWrite(w, http.StatusOK, map[string]any{"cached": true, "updated_at": updatedAt, "data": parsed})
		return
	}
	res, err := h.client.CheckinStatus()
	if err != nil {
		jsonErr(w, http.StatusBadGateway, err.Error())
		return
	}
	_ = h.st.SetCache("checkin_cache", "status", string(res.Raw))
	var parsed any
	_ = json.Unmarshal(res.Raw, &parsed)
	jsonWrite(w, http.StatusOK, map[string]any{"cached": false, "updated_at": time.Now().Unix(), "data": parsed})
}

func (h *Handler) checkinClaim(w http.ResponseWriter, r *http.Request) {
	res, err := h.client.ClaimCheckin()
	if err != nil {
		jsonErr(w, http.StatusBadGateway, err.Error())
		return
	}
	var parsed any
	_ = json.Unmarshal(res.Raw, &parsed)
	jsonWrite(w, http.StatusOK, map[string]any{"ok": true, "data": parsed})
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
	f := store.LogFilter{
		Model:    strings.TrimSpace(q.Get("model")),
		Status:   0,
		Page:     atoiDefault(q.Get("page"), 1),
		PageSize: atoiDefault(q.Get("page_size"), 20),
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
	st, err := h.st.GetStats()
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
		"resource_cache_seconds": cfg.ResourceCacheSeconds,
		"log_retention_days":     cfg.LogRetentionDays,
		"log_max_size_mb":        cfg.LogMaxSizeMB,
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
		ResourceCacheSeconds *int    `json:"resource_cache_seconds"`
		LogRetentionDays     *int    `json:"log_retention_days"`
		LogMaxSizeMB         *int    `json:"log_max_size_mb"`
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
		if req.ResourceCacheSeconds != nil && *req.ResourceCacheSeconds > 0 {
			c.ResourceCacheSeconds = *req.ResourceCacheSeconds
		}
		if req.LogRetentionDays != nil && *req.LogRetentionDays > 0 {
			c.LogRetentionDays = *req.LogRetentionDays
		}
		if req.LogMaxSizeMB != nil && *req.LogMaxSizeMB > 0 {
			c.LogMaxSizeMB = *req.LogMaxSizeMB
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
	// 签到开关/cron 变化 → 重装配定时任务
	if h.sched != nil {
		h.sched.Reconfigure()
	}
	_ = oldCfg

	h.getSettings(w, r)
}

// ── 模型 ──

func (h *Handler) listModels(w http.ResponseWriter, r *http.Request) {
	jsonWrite(w, http.StatusOK, map[string]any{
		"ids":    h.models.List(),
		"models": h.models.Models(),
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

// parseFlexibleTime 宽容解析时间：数字（秒/毫秒）、RFC3339、常见格式。
func parseFlexibleTime(v any) (time.Time, bool) {
	switch t := v.(type) {
	case float64:
		sec := int64(t)
		if sec > 1e12 { // 毫秒
			return time.Unix(sec/1e3, 0), true
		}
		return time.Unix(sec, 0), true
	case string:
		if t == "" {
			return time.Time{}, false
		}
		layouts := []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02T15:04:05", "2006-01-02"}
		for _, layout := range layouts {
			if ts, err := time.ParseInLocation(layout, t, time.Local); err == nil {
				return ts, true
			}
		}
	}
	return time.Time{}, false
}
