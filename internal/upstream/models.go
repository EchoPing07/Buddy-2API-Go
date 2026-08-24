package upstream

import (
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "time/tzdata" // 内嵌时区库，保证跨平台解析 Asia/Shanghai 等时区
)

// builtinCraftModels 内置默认模型表，未登录或拉取失败时回退使用。
var builtinCraftModels = map[string][]string{
	"cn": {
		"auto", "hy3", "glm-5.3", "glm-5.2", "glm-5.1", "glm-5v-turbo",
		"kimi-k3-1", "kimi-k2.7", "kimi-k2.6", "minimax-m3",
		"deepseek-v4-pro", "deepseek-v4-flash",
	},
	"global": {
		"default-model", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna",
		"gpt-5.5", "gpt-5.4", "gpt-5.3-codex", "gemini-3.5-flash",
		"glm-5.3", "glm-5.2", "kimi-k2.6", "minimax-m3",
	},
}

// ModelCache 内存模型列表缓存，失败回退内置默认表。
type ModelCache struct {
	mu        sync.RWMutex
	region    string
	craft     []string
	models    []ModelInfo
	promos    []ModelPromotion // 分时段折扣活动（/v3/config modelPromotions）
	fetchedAt time.Time
	fallback  bool // 当前数据是否来自内置回退表
}

// NewModelCache 创建缓存并立即尝试拉取（失败回退内置表）。
func NewModelCache(region string, client *Client) *ModelCache {
	mc := &ModelCache{}
	mc.applyFallback(region)
	if err := mc.Refresh(client); err != nil {
		slog.Warn("启动拉取模型列表失败，已回退内置默认表", "error", err)
	}
	return mc
}

// Refresh 按当前 region 重新拉取 /v3/config，失败则保留旧数据或回退内置表。
func (mc *ModelCache) Refresh(client *Client) error {
	region := client.regionProvider()
	cfg, err := client.FetchConfig()
	if err != nil {
		mc.mu.Lock()
		if mc.region != region {
			mc.applyFallbackLocked(region) // 换 region 且拉取失败 → 内置表
		}
		mc.mu.Unlock()
		return err
	}
	mc.mu.Lock()
	defer mc.mu.Unlock()
	mc.region = region
	mc.craft = cfg.Craft
	mc.models = cfg.Models
	mc.promos = cfg.Promotions
	mc.fetchedAt = time.Now()
	mc.fallback = false
	slog.Info("模型列表已刷新", "region", region, "count", len(cfg.Craft), "promotions", len(cfg.Promotions))
	return nil
}

func (mc *ModelCache) applyFallback(region string) {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	mc.applyFallbackLocked(region)
}

func (mc *ModelCache) applyFallbackLocked(region string) {
	mc.region = region
	mc.craft = builtinCraftModels[region]
	if mc.craft == nil {
		mc.craft = builtinCraftModels["cn"]
	}
	mc.models = nil
	mc.promos = nil
	mc.fetchedAt = time.Now()
	mc.fallback = true
}

// List 返回对话可用模型 ID（craft 集）。
func (mc *ModelCache) List() []string {
	mc.mu.RLock()
	defer mc.mu.RUnlock()
	out := make([]string, len(mc.craft))
	copy(out, mc.craft)
	return out
}

// Models 返回模型详情。
func (mc *ModelCache) Models() []ModelInfo {
	mc.mu.RLock()
	defer mc.mu.RUnlock()
	out := make([]ModelInfo, len(mc.models))
	copy(out, mc.models)
	return out
}

// State 缓存状态（调试/设置页展示）。
type ModelState struct {
	Region    string    `json:"region"`
	Count     int       `json:"count"`
	Fallback  bool      `json:"fallback"`
	FetchedAt time.Time `json:"fetched_at"`
}

func (mc *ModelCache) State() ModelState {
	mc.mu.RLock()
	defer mc.mu.RUnlock()
	return ModelState{Region: mc.region, Count: len(mc.craft), Fallback: mc.fallback, FetchedAt: mc.fetchedAt}
}

// ── 模型倍率（credits）与分时段折扣 ─────────────────────────────────────────

// ModelDisplay 管理端模型视图：附当前生效倍率展示串。
type ModelDisplay struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Credits string `json:"credits,omitempty"` // 当前生效倍率，如 "x0.50"；无官方数据（如 auto）时为空
	Promo   string `json:"promo,omitempty"`   // 生效中的折扣活动名，如 "夜间折扣"
}

// View 计算模型列表的当前生效倍率视图（按调用时刻评估折扣窗口）。
func (mc *ModelCache) View() []ModelDisplay {
	now := time.Now()
	mc.mu.RLock()
	defer mc.mu.RUnlock()
	out := make([]ModelDisplay, 0, len(mc.models))
	for _, m := range mc.models {
		d := ModelDisplay{ID: m.ID, Name: m.Name}
		if base, ok := parseRateX(m.Credits); ok {
			rate, promo := effectiveRate(base, mc.promos, m.ID, now)
			d.Credits = formatRateX(rate)
			d.Promo = promo
		}
		out = append(out, d)
	}
	return out
}

// effectiveRate 返回 modelID 在 now 时刻的生效倍率与命中的活动名。
// 规则：取能解析出 discountedCredits 且时间窗命中的最高优先级活动，
// 其 discountedCredits 即生效期倍率；无命中则为基础倍率。
func effectiveRate(base float64, promos []ModelPromotion, modelID string, now time.Time) (float64, string) {
	bestPri := -1
	rate, promo := base, ""
	for i := range promos {
		p := &promos[i]
		if !p.Enabled || p.Kind != "discount" || p.Discount.DiscountedCredits == "" {
			continue
		}
		if !promoCoversModel(p, modelID) {
			continue
		}
		v, ok := parseRateX(p.Discount.DiscountedCredits)
		if !ok || v < 0 {
			continue
		}
		if !promoActive(&p.Schedule, now) {
			continue
		}
		if p.Priority > bestPri { // 首次命中时 -1 必被超过；同优先级取先出现者
			bestPri = p.Priority
			rate = v
			promo = p.Badge.Label
		}
	}
	return rate, promo
}

func promoCoversModel(p *ModelPromotion, modelID string) bool {
	for _, id := range p.ModelIDs {
		if id == modelID {
			return true
		}
	}
	return false
}

// promoActive 判断 now 是否落在活动的生效时间窗内：
// validFrom/validUntil 为绝对起止（可空）；daily 为每日时段窗（可空 = 不限时段）。
func promoActive(s *PromoSchedule, now time.Time) bool {
	loc := schedLoc(s.Timezone)
	nl := now.In(loc)
	if s.ValidFrom != "" {
		if t, err := time.Parse(time.RFC3339, s.ValidFrom); err == nil && nl.Before(t) {
			return false
		}
	}
	if s.ValidUntil != "" {
		if t, err := time.Parse(time.RFC3339, s.ValidUntil); err == nil && !nl.Before(t) {
			return false
		}
	}
	if len(s.Daily) == 0 {
		return true
	}
	cur := nl.Hour()*60 + nl.Minute()
	for _, w := range s.Daily {
		st, ok1 := parseHM(w.Start)
		en, ok2 := parseHM(w.End)
		if !ok1 || !ok2 {
			continue
		}
		if st <= en { // 常规窗，如 09:00-12:00
			if cur >= st && cur < en {
				return true
			}
		} else if cur >= st || cur < en { // 跨零点，如 23:00-07:50
			return true
		}
	}
	return false
}

func schedLoc(tz string) *time.Location {
	if strings.TrimSpace(tz) == "" {
		return time.Local
	}
	if loc, err := time.LoadLocation(tz); err == nil {
		return loc
	}
	return time.Local
}

func parseHM(s string) (int, bool) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 2 {
		return 0, false
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || h < 0 || h > 24 || m < 0 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

// parseRateX 解析官方倍率展示串：兼容 "x0.79" / "0.50x" / "x0" 等形式。
func parseRateX(s string) (float64, bool) {
	s = strings.TrimSpace(strings.ToLower(s))
	s = strings.TrimPrefix(s, "x")
	s = strings.TrimSuffix(s, "x")
	if s == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		return 0, false
	}
	return f, true
}

// formatRateX 统一格式化为 "xN.NN" 展示串（与上游样式一致，保留两位小数）。
func formatRateX(f float64) string {
	return "x" + strconv.FormatFloat(f, 'f', 2, 64)
}
