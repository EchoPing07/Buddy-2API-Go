package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"buddy2api-go/internal/config"
	"buddy2api-go/internal/pool"
)

// SetPool 注入号池，并挂载探测余额回调使每次 Probe 自动同步写入 resource_cache。
func (h *Handler) SetPool(p *pool.Manager) {
	h.pool = p
	if p != nil {
		p.SetResourceHook(h.cacheProbeResource)
	}
}

// cacheProbeResource 将号池 Probe 拿到的原始 resource JSON 加工后写入 per-uid 缓存。
func (h *Handler) cacheProbeResource(uid string, raw []byte) {
	if h == nil || h.st == nil || uid == "" || len(raw) == 0 {
		return
	}
	processed := processResources(json.RawMessage(raw))
	b, err := json.Marshal(processed)
	if err != nil {
		return
	}
	_ = h.st.SetCache("resource_cache", resourceCacheKey(uid), string(b))
	for _, stale := range resourceCacheStaleKeys {
		_ = h.st.DeleteCacheKey("resource_cache", stale)
	}
}

// ── 号池状态与策略 ──

// poolView GET /admin/pool。面板数据全部来自内存快照，
// 唯一的额外成本是余额（poolCredits）——按 uid 读本地 resource_cache，不每次打上游。
func (h *Handler) poolView(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil {
		jsonWrite(w, http.StatusOK, pool.PoolView{
			Enabled:  true,
			Strategy: h.cfg.Get().PoolStrategy,
			Accounts: []pool.AccountView{},
		})
		return
	}
	view := h.pool.Snapshot(h.poolCredits)
	if t := h.toks.Get(); t != nil {
		for i := range view.Accounts {
			if view.Accounts[i].UID == t.UID {
				view.Accounts[i].Active = true
			}
		}
	}
	jsonWrite(w, http.StatusOK, view)
}

// poolSetStrategy PUT /admin/pool/strategy {"strategy":...}：切换多账号路由策略。
// 校验合法性（400），持久化到 config.json，并受 BUDDY2API_POOL_STRATEGY env 锁定约束。
func (h *Handler) poolSetStrategy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Strategy string `json:"strategy"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	strategy := strings.TrimSpace(req.Strategy)
	if !config.ValidPoolStrategy(strategy) {
		jsonErr(w, http.StatusBadRequest, "不支持的路由策略（仅支持 expire_first / balanced / credits_first）")
		return
	}
	if err := h.cfg.Update(func(c *config.Config) error {
		c.PoolStrategy = strategy
		return nil
	}); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonWrite(w, http.StatusOK, map[string]any{
		"ok":       true,
		"strategy": h.cfg.Get().PoolStrategy,
	})
}

// poolCredits 从 per-uid 余额缓存里读某个账号的可用额度，返回 (额度, 读到它的时刻)。
// Snapshot 将其与账号自身最近一次 Probe 写入的时间戳比对，谁新用谁。
func (h *Handler) poolCredits(uid string) (float64, int64) {
	if uid == "" {
		return 0, 0
	}
	payload, updatedAt, ok := h.st.GetCache("resource_cache", resourceCacheKey(uid), 0)
	if !ok || payload == "" {
		return 0, 0
	}
	var cached struct {
		TotalDosage float64 `json:"total_dosage"`
	}
	if err := json.Unmarshal([]byte(payload), &cached); err != nil {
		return 0, 0
	}
	return cached.TotalDosage, updatedAt
}

// poolDisable / poolEnable 手动禁用 / 启用某账号。
func (h *Handler) poolDisable(w http.ResponseWriter, r *http.Request) { h.setPoolManual(w, r, true) }
func (h *Handler) poolEnable(w http.ResponseWriter, r *http.Request)  { h.setPoolManual(w, r, false) }

func (h *Handler) setPoolManual(w http.ResponseWriter, r *http.Request, disabled bool) {
	uid := chi.URLParam(r, "uid")
	if h.pool == nil {
		jsonErr(w, http.StatusNotFound, pool.ErrNoAccount.Error())
		return
	}
	if err := h.pool.SetManualDisabled(uid, disabled); err != nil {
		jsonErr(w, http.StatusNotFound, err.Error())
		return
	}
	jsonWrite(w, http.StatusOK, map[string]any{"ok": true})
}

// poolProbe 立即探测单个账号（限频 30 秒/号）。
func (h *Handler) poolProbe(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil {
		jsonErr(w, http.StatusNotFound, pool.ErrNoAccount.Error())
		return
	}
	uid := chi.URLParam(r, "uid")
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	res, err := h.pool.ProbeWithLimit(ctx, uid)
	if err != nil {
		jsonErr(w, http.StatusTooManyRequests, err.Error())
		return
	}
	jsonWrite(w, http.StatusOK, res)
}

// poolProbeAll 探测全池（同步执行并返回逐条结果）。
func (h *Handler) poolProbeAll(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil {
		jsonWrite(w, http.StatusOK, map[string]any{"items": []pool.ProbeResult{}})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	jsonWrite(w, http.StatusOK, map[string]any{"items": h.pool.ProbeAll(ctx)})
}

// poolClearCool 手动清除某个 (账号, 模型) 的冷却。
func (h *Handler) poolClearCool(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil {
		jsonErr(w, http.StatusNotFound, pool.ErrNoAccount.Error())
		return
	}
	uid := chi.URLParam(r, "uid")
	model := chi.URLParam(r, "model")
	if err := h.pool.ClearModelCool(uid, model); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonWrite(w, http.StatusOK, map[string]any{"ok": true})
}

// poolReset 清空全部会话粘性绑定。
func (h *Handler) poolReset(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil {
		jsonWrite(w, http.StatusOK, map[string]any{"ok": true, "cleared": 0})
		return
	}
	before := h.pool.SessionCount()
	h.pool.ResetSessions()
	jsonWrite(w, http.StatusOK, map[string]any{"ok": true, "cleared": before})
}
