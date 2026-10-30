package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"buddy2api-go/internal/pool"
)

// SetPool 注入号池（nil = 实验未开启）。
//
// 与 others 的注入方式一致（sched / models 都是 New 之后可换）：
// main 在构造时注入一次；实验开关切换后不必重新注入（Manager 自己读配置热开关），
// 但如果开关从关到开需要新建 Manager，则由 putExperiments 侧重注入。
func (h *Handler) SetPool(p *pool.Manager) { h.pool = p }

// ── 号池状态 ──

// poolView GET /admin/pool。面板数据全部来自内存快照，
// 唯一的额外成本是余额（creditsOf）——那一项用缓存表，不每次打上游。
func (h *Handler) poolView(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil {
		// 未构造（开关本来是关的）：回一个空视图而不是 404，
		// 前端可以无差别渲染，由 enabled 字段决定显示什么。
		jsonWrite(w, http.StatusOK, pool.PoolView{Accounts: []pool.AccountView{}})
		return
	}
	view := h.pool.Snapshot(h.poolCredits)
	// 当前活动账号（后台任务走它，与池化无关，但 UI 需要标出来）
	if t := h.toks.Get(); t != nil {
		for i := range view.Accounts {
			if view.Accounts[i].UID == t.UID {
				view.Accounts[i].Active = true
			}
		}
	}
	jsonWrite(w, http.StatusOK, view)
}

// poolCredits 从余额缓存里读某个账号的可用额度。
//
// 号池账号的余额没有按账号分片的缓存（缓存键是单账号设计的固定字串），
// 所以这里只在「该账号正好是当前活动账号」时用现成缓存，其余返回 0 交给 UI 显示「—」。
// 刻意不在这里打上游：面板刷新一次要 ×N 个账号，得不偿失；真要看余额点「探测」。
func (h *Handler) poolCredits(uid string) float64 {
	t := h.toks.Get()
	if t == nil || t.UID != uid {
		return 0
	}
	payload, _, ok := h.st.GetCache("resource_cache", resourceCacheKey, 0)
	if !ok || payload == "" {
		return 0
	}
	// 缓存内容是 processResources 加工后的结构，直接取 total_dosage
	var cached struct {
		TotalDosage float64 `json:"total_dosage"`
	}
	if err := json.Unmarshal([]byte(payload), &cached); err != nil {
		return 0
	}
	return cached.TotalDosage
}

// poolDisable / poolEnable 手动禁用 / 启用某账号。
func (h *Handler) poolDisable(w http.ResponseWriter, r *http.Request) { h.setPoolManual(w, r, true) }
func (h *Handler) poolEnable(w http.ResponseWriter, r *http.Request)  { h.setPoolManual(w, r, false) }

func (h *Handler) setPoolManual(w http.ResponseWriter, r *http.Request, disabled bool) {
	uid := chi.URLParam(r, "uid")
	if err := h.pool.SetManualDisabled(uid, disabled); err != nil {
		jsonErr(w, http.StatusNotFound, err.Error())
		return
	}
	jsonWrite(w, http.StatusOK, map[string]any{"ok": true})
}

// poolProbe 立即探测单个账号（限频 30 秒/号）。
func (h *Handler) poolProbe(w http.ResponseWriter, r *http.Request) {
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
// 账号数是个位数，串行 30 秒超时封顶即可；异步反而让前端要轮询。
func (h *Handler) poolProbeAll(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	jsonWrite(w, http.StatusOK, map[string]any{"items": h.pool.ProbeAll(ctx)})
}

// poolClearCool 手动清除某个 (账号, 模型) 的冷却（「我知道好了，让我试试」）。
func (h *Handler) poolClearCool(w http.ResponseWriter, r *http.Request) {
	uid := chi.URLParam(r, "uid")
	model := chi.URLParam(r, "model")
	if err := h.pool.ClearModelCool(uid, model); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonWrite(w, http.StatusOK, map[string]any{"ok": true})
}

// poolReset 清空全部会话粘性绑定（「我换号了，让所有会话重新分配」）。
func (h *Handler) poolReset(w http.ResponseWriter, r *http.Request) {
	before := h.pool.SessionCount()
	h.pool.ResetSessions()
	jsonWrite(w, http.StatusOK, map[string]any{"ok": true, "cleared": before})
}
