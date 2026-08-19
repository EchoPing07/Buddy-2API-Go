// Package config 负责加载与热更新 data/config.json，env（BUDDY2API_*）优先于文件。
package config

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/crypto/bcrypt"
)

// Endpoint 描述一套上游端点（中国/国际）。
// OAuth 设备流 host 与 Upstream 同 host（实测确认）。
type Endpoint struct {
	Region   string // "cn" | "global"
	Name     string // "中国" | "国际"
	Upstream string // API 端点（也是 OAuth host）
	Domain   string // 默认 X-Domain
}

var endpoints = map[string]Endpoint{
	"cn": {
		Region:   "cn",
		Name:     "中国",
		Upstream: "https://copilot.tencent.com",
		Domain:   "www.codebuddy.cn",
	},
	"global": {
		Region:   "global",
		Name:     "国际",
		Upstream: "https://www.codebuddy.ai",
		Domain:   "www.codebuddy.ai",
	},
}

// EndpointOf 返回 region 对应端点，未知 region 回退中国。
func EndpointOf(region string) Endpoint {
	if ep, ok := endpoints[strings.TrimSpace(region)]; ok {
		return ep
	}
	return endpoints["cn"]
}

// Regions 返回合法 region 列表。
func Regions() []string { return []string{"cn", "global"} }

// Config 全局配置，见 DEVELOPMENT.md §5.2。
type Config struct {
	AdminPasswordHash    string `json:"admin_password_hash"`
	Listen               string `json:"listen"`
	Region               string `json:"region"`
	AutoCheckin          bool   `json:"auto_checkin"`
	CheckinCron          string `json:"checkin_cron"`
	ResourceCacheSeconds int    `json:"resource_cache_seconds"`
	LogRetentionDays     int    `json:"log_retention_days"`
}

func defaults() Config {
	return Config{
		Listen:               "127.0.0.1:10082",
		Region:               "cn",
		AutoCheckin:          false,
		CheckinCron:          "0 0 9 * * *",
		ResourceCacheSeconds: 300,
		LogRetentionDays:     90,
	}
}

// Normalize 补齐空字段为默认值、校验取值范围。
func (c *Config) Normalize() {
	d := defaults()
	if strings.TrimSpace(c.Listen) == "" {
		c.Listen = d.Listen
	}
	if c.Region != "global" {
		c.Region = "cn" // 默认中国端点
	}
	if strings.TrimSpace(c.CheckinCron) == "" {
		c.CheckinCron = d.CheckinCron
	}
	if c.ResourceCacheSeconds <= 0 {
		c.ResourceCacheSeconds = d.ResourceCacheSeconds
	}
	if c.LogRetentionDays <= 0 {
		c.LogRetentionDays = d.LogRetentionDays
	}
}

// Manager 线程安全的配置管理器（读写锁 + 文件持久化）。
type Manager struct {
	mu   sync.RWMutex
	path string
	cfg  Config
	// envSets 记录被 env 覆盖的字段名，运行时修改不生效（env 优先级最高）
	envSets map[string]bool
}

// Load 读取 config.json（不存在则用默认值），应用 env 覆盖，
// 首次启动若无管理密码则随机生成并打印一次。
func Load(dataDir string) (*Manager, error) {
	m := &Manager{path: filepath.Join(dataDir, "config.json"), envSets: map[string]bool{}}
	cfg := defaults()
	if raw, err := os.ReadFile(m.path); err == nil {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("解析 %s: %w", m.path, err)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	cfg.Normalize()

	// env 覆盖（优先级最高）
	envStr := func(name, field string, dst *string) {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			*dst = v
			m.envSets[field] = true
		}
	}
	envBool := func(name, field string, dst *bool) {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			if b, err := strconv.ParseBool(v); err == nil {
				*dst = b
				m.envSets[field] = true
			}
		}
	}
	envInt := func(name, field string, dst *int) {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				*dst = n
				m.envSets[field] = true
			}
		}
	}
	envStr("BUDDY2API_LISTEN", "listen", &cfg.Listen)
	envStr("BUDDY2API_REGION", "region", &cfg.Region)
	envStr("BUDDY2API_CHECKIN_CRON", "checkin_cron", &cfg.CheckinCron)
	envBool("BUDDY2API_AUTO_CHECKIN", "auto_checkin", &cfg.AutoCheckin)
	envInt("BUDDY2API_RESOURCE_CACHE_SECONDS", "resource_cache_seconds", &cfg.ResourceCacheSeconds)
	envInt("BUDDY2API_LOG_RETENTION_DAYS", "log_retention_days", &cfg.LogRetentionDays)

	// 管理密码：env 明文 > 文件 hash > 随机生成
	if pw := strings.TrimSpace(os.Getenv("BUDDY2API_ADMIN_PASSWORD")); pw != "" {
		hash, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
		if err != nil {
			return nil, fmt.Errorf("哈希管理密码失败: %w", err)
		}
		if cfg.AdminPasswordHash != string(hash) {
			cfg.AdminPasswordHash = string(hash)
			m.envSets["admin_password_hash"] = true
		}
	}
	firstRun := false
	if cfg.AdminPasswordHash == "" {
		pw, err := randomPassword(12)
		if err != nil {
			return nil, err
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
		if err != nil {
			return nil, fmt.Errorf("哈希管理密码失败: %w", err)
		}
		cfg.AdminPasswordHash = string(hash)
		firstRun = true
		m.cfg = cfg
		if err := m.saveLocked(); err != nil {
			return nil, err
		}
		slog.Warn("首次启动：已生成管理后台随机密码（只显示这一次，请立即登录修改）",
			"password", pw)
	}
	m.cfg = cfg
	if !firstRun {
		if err := m.saveLocked(); err != nil { // 把 env 归一化结果落盘
			return nil, err
		}
	}
	return m, nil
}

// Get 返回配置副本。
func (m *Manager) Get() Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg
}

// Effective 返回经 env 重放的配置（env 字段运行时不可被 PUT 覆盖）。
func (m *Manager) Effective() Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg
}

// Update 在锁内修改配置并落盘。fn 返回 error 则放弃修改。
// listen 与 admin_password_hash 不经此接口修改（前者需重启，后者走改密码逻辑）。
func (m *Manager) Update(fn func(*Config) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	nc := m.cfg
	if err := fn(&nc); err != nil {
		return err
	}
	// env 覆盖的字段改回 env 值（保持优先级）
	if m.envSets["region"] {
		nc.Region = m.cfg.Region
	}
	if m.envSets["auto_checkin"] {
		nc.AutoCheckin = m.cfg.AutoCheckin
	}
	if m.envSets["checkin_cron"] {
		nc.CheckinCron = m.cfg.CheckinCron
	}
	if m.envSets["resource_cache_seconds"] {
		nc.ResourceCacheSeconds = m.cfg.ResourceCacheSeconds
	}
	if m.envSets["log_retention_days"] {
		nc.LogRetentionDays = m.cfg.LogRetentionDays
	}
	nc.Normalize()
	old := m.cfg
	m.cfg = nc
	if err := m.saveLocked(); err != nil {
		m.cfg = old
		return err
	}
	return nil
}

// SetAdminPasswordHash 设置新密码哈希并落盘（改密码专用）。
func (m *Manager) SetAdminPasswordHash(hash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	old := m.cfg.AdminPasswordHash
	m.cfg.AdminPasswordHash = hash
	if err := m.saveLocked(); err != nil {
		m.cfg.AdminPasswordHash = old
		return err
	}
	return nil
}

// VerifyAdminPassword 校验管理密码。
func (m *Manager) VerifyAdminPassword(pw string) bool {
	m.mu.RLock()
	hash := m.cfg.AdminPasswordHash
	m.mu.RUnlock()
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

func (m *Manager) saveLocked() error {
	if dir := filepath.Dir(m.path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(m.cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(m.path, data, 0o600)
}

// randomPassword 生成 n 位字母数字密码。
func randomPassword(n int) (string, error) {
	const alphabet = "abcdefghjkmnpqrstuvwxyzABCDEFGHJKMNPQRSTUVWXYZ23456789"
	var sb strings.Builder
	for i := 0; i < n; i++ {
		idx, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			return "", err
		}
		sb.WriteByte(alphabet[idx.Int64()])
	}
	return sb.String(), nil
}
