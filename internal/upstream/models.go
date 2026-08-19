package upstream

import (
	"log/slog"
	"sync"
	"time"
)

// builtinCraftModels 内置默认模型表（2026-08 实测快照，DEVELOPMENT.md §6.6）。
// 未登录或 /v3/config 拉取失败时回退使用。
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

// ModelCache 内存模型列表缓存：启动时拉取，region 切换/手动刷新时重拉，
// 失败回退内置默认表。
type ModelCache struct {
	mu        sync.RWMutex
	region    string
	craft     []string
	models    []ModelInfo
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

// Refresh 按当前 region 重新拉取 /v3/config；失败时若 region 与缓存一致则保留旧数据，
// 否则回退内置表。返回错误供调用方感知。
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
	mc.fetchedAt = time.Now()
	mc.fallback = false
	slog.Info("模型列表已刷新", "region", region, "count", len(cfg.Craft))
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
