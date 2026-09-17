// Package config 加载与热更新配置（data/config.json，env 覆盖）。
package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/crypto/bcrypt"
)

// Endpoint 一套上游端点（中国/国际）。
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

// Config 全局配置。
type Config struct {
	AdminPasswordHash    string `json:"admin_password_hash"`
	Listen               string `json:"listen"`
	Region               string `json:"region"`
	AutoCheckin          bool   `json:"auto_checkin"`
	CheckinCron          string `json:"checkin_cron"`
	CheckinMode          string `json:"checkin_mode"`         // "fixed"（按 cron）| "random"（时间范围内随机）
	CheckinRandomStart   string `json:"checkin_random_start"` // random 模式窗口起 "HH:MM"
	CheckinRandomEnd     string `json:"checkin_random_end"`   // random 模式窗口止 "HH:MM"
	CheckinFallback      bool   `json:"checkin_fallback"`     // 末班兜底：23:50/23:55 各再试一次
	AutoGrowth           bool   `json:"auto_growth"`          // 成长任务总开关（活跃地图/连登奖励/猫猫旅行）
	GrowthReportCron     string `json:"growth_report_cron"`   // 上报+奖励链 cron（6 段含秒）
	GrowthTravelCron     string `json:"growth_travel_cron"`   // 旅行巡检 cron
	GrowthReportCount    int    `json:"growth_report_count"`  // 每日上报条数（1-10；领养前置 chat_5 需 5）
	GrowthReportJitter   int    `json:"growth_report_jitter"` // 每日上报条数随机波动 ±N（0=固定不波动，上限 GrowthReportJitterMax）
	ResourceCacheSeconds int    `json:"resource_cache_seconds"`
	LogRetentionDays     int    `json:"log_retention_days"`
	LogMaxSizeMB         int    `json:"log_max_size_mb"`
	ChatTimeoutSeconds   int    `json:"chat_timeout_seconds"`
}

// 默认 cron 表达式（defaults() 与 admin 存量兜底共用，避免字面量两处漂移）。
const (
	DefaultCheckinCron      = "0 0 9 * * *"
	DefaultGrowthReportCron = "0 0 10 * * *"
	DefaultGrowthTravelCron = "0 0 9,21 * * *"
)

func defaults() Config {
	return Config{
		Listen:               "127.0.0.1:10082",
		Region:               "cn",
		AutoCheckin:          false,
		CheckinCron:          DefaultCheckinCron,
		CheckinMode:          "fixed",
		CheckinRandomStart:   "09:00",
		CheckinRandomEnd:     "18:00",
		CheckinFallback:      true,
		AutoGrowth:           false,
		GrowthReportCron:     DefaultGrowthReportCron,
		GrowthTravelCron:     DefaultGrowthTravelCron,
		GrowthReportCount:    10,
		GrowthReportJitter:   0,
		ResourceCacheSeconds: 300,
		LogRetentionDays:     90,
		LogMaxSizeMB:         50,
		ChatTimeoutSeconds:   60,
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
	if c.CheckinMode != "random" {
		c.CheckinMode = "fixed"
	}
	if err := ValidateCheckinWindow(c.CheckinRandomStart, c.CheckinRandomEnd); err != nil {
		c.CheckinRandomStart = d.CheckinRandomStart
		c.CheckinRandomEnd = d.CheckinRandomEnd
	}
	// 成长任务：cron 空回落默认（合法性校验在 admin putSettings / scheduler 注册时做，
	// 与 checkin_cron 同策略）；上报条数钳 1..10（防风控）
	if strings.TrimSpace(c.GrowthReportCron) == "" {
		c.GrowthReportCron = d.GrowthReportCron
	}
	if strings.TrimSpace(c.GrowthTravelCron) == "" {
		c.GrowthTravelCron = d.GrowthTravelCron
	}
	if c.GrowthReportCount <= 0 {
		c.GrowthReportCount = 1
	}
	if c.GrowthReportCount > GrowthReportCountMax {
		c.GrowthReportCount = GrowthReportCountMax
	}
	if c.GrowthReportJitter < 0 {
		c.GrowthReportJitter = 0
	}
	if c.GrowthReportJitter > GrowthReportJitterMax {
		c.GrowthReportJitter = GrowthReportJitterMax
	}
	if c.ResourceCacheSeconds <= 0 {
		c.ResourceCacheSeconds = d.ResourceCacheSeconds
	}
	if c.LogRetentionDays <= 0 {
		c.LogRetentionDays = d.LogRetentionDays
	}
	if c.LogMaxSizeMB <= 0 {
		c.LogMaxSizeMB = d.LogMaxSizeMB
	}
	if c.ChatTimeoutSeconds <= 0 {
		c.ChatTimeoutSeconds = d.ChatTimeoutSeconds
	}
	if c.ChatTimeoutSeconds > MaxChatTimeoutSeconds {
		c.ChatTimeoutSeconds = MaxChatTimeoutSeconds
	}
}

// MaxCheckinWindowEnd 随机窗口最晚结束时刻：需保证主尝试 + 5 分钟重试在末班 23:50 前完成。
const MaxCheckinWindowEnd = "23:30"

// MaxChatTimeoutSeconds chat 响应头超时上限（秒）。等待上游「开始响应」不该超过一小时；
// 更关键的是过大的秒数在 time.Duration 秒→纳秒换算时会溢出为负，令 Transport 对所有
// chat 请求立即判超时，且该值会持久化到 config.json 无法自愈。
const MaxChatTimeoutSeconds = 3600

// GrowthReportCountMax 每日上报条数上限（防风控：同会话连发过多易被判机器人）。
const GrowthReportCountMax = 10

// GrowthReportJitterMax 上报条数波动幅度上限。±N 后仍受 1..GrowthReportCountMax 钳制，
// 故配置层的合法区间与条数一致即可，实际取值在运行时钳。
const GrowthReportJitterMax = GrowthReportCountMax

// RollReportCount 计算本次实际上报条数：base ± jitter 内均匀取一个整数。
// jitter<=0 → 固定 base；结果钳 1..GrowthReportCountMax（防风控，且保证至少 1 条）。
// 纯函数（随机源由调用方传入），便于单测注入确定性序列。
func RollReportCount(base, jitter, rnd int) int {
	if base <= 0 {
		base = 1
	}
	if base > GrowthReportCountMax {
		base = GrowthReportCountMax
	}
	if jitter <= 0 {
		return base
	}
	if jitter > GrowthReportJitterMax {
		jitter = GrowthReportJitterMax
	}
	// rnd 视为 [0, 2*jitter+1) 的均匀整数偏移，映射到 [-jitter, +jitter]
	span := 2*jitter + 1
	off := rnd % span
	if off < 0 {
		off += span
	}
	n := base - jitter + off
	if n < 1 {
		return 1
	}
	if n > GrowthReportCountMax {
		return GrowthReportCountMax
	}
	return n
}

// ParseHHMM 把 "HH:MM" 解析为当日分钟数（0-1439），格式非法返回 ok=false。
func ParseHHMM(s string) (minutes int, ok bool) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 2 {
		return 0, false
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

// ValidateCheckinWindow 校验随机签到窗口：HH:MM 格式、start < end、end 不晚于 23:30。
func ValidateCheckinWindow(start, end string) error {
	sm, ok1 := ParseHHMM(start)
	em, ok2 := ParseHHMM(end)
	if !ok1 || !ok2 {
		return fmt.Errorf("时间格式应为 HH:MM（如 09:00）")
	}
	if em <= sm {
		return fmt.Errorf("开始时间需早于结束时间")
	}
	if max, _ := ParseHHMM(MaxCheckinWindowEnd); em > max {
		return fmt.Errorf("结束时间最晚 %s（需在末班签到前留出重试时间）", MaxCheckinWindowEnd)
	}
	return nil
}

// Manager 线程安全的配置管理器（读写锁 + 文件持久化）。
type Manager struct {
	mu   sync.RWMutex
	path string
	cfg  Config
	// envSets 记录被 env 覆盖的字段，运行时不可改
	envSets map[string]bool
}

// Load 读取 config.json，应用 env 覆盖；首次启动若无管理密码则使用默认值。
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
	envStr("BUDDY2API_CHECKIN_MODE", "checkin_mode", &cfg.CheckinMode)
	envStr("BUDDY2API_CHECKIN_RANDOM_START", "checkin_random_start", &cfg.CheckinRandomStart)
	envStr("BUDDY2API_CHECKIN_RANDOM_END", "checkin_random_end", &cfg.CheckinRandomEnd)
	envBool("BUDDY2API_CHECKIN_FALLBACK", "checkin_fallback", &cfg.CheckinFallback)
	envBool("BUDDY2API_AUTO_CHECKIN", "auto_checkin", &cfg.AutoCheckin)
	envBool("BUDDY2API_AUTO_GROWTH", "auto_growth", &cfg.AutoGrowth)
	envStr("BUDDY2API_GROWTH_REPORT_CRON", "growth_report_cron", &cfg.GrowthReportCron)
	envStr("BUDDY2API_GROWTH_TRAVEL_CRON", "growth_travel_cron", &cfg.GrowthTravelCron)
	envInt("BUDDY2API_GROWTH_REPORT_COUNT", "growth_report_count", &cfg.GrowthReportCount)
	envInt("BUDDY2API_GROWTH_REPORT_JITTER", "growth_report_jitter", &cfg.GrowthReportJitter)
	envInt("BUDDY2API_RESOURCE_CACHE_SECONDS", "resource_cache_seconds", &cfg.ResourceCacheSeconds)
	envInt("BUDDY2API_LOG_RETENTION_DAYS", "log_retention_days", &cfg.LogRetentionDays)
	envInt("BUDDY2API_LOG_MAX_SIZE_MB", "log_max_size_mb", &cfg.LogMaxSizeMB)
	envInt("BUDDY2API_CHAT_TIMEOUT_SECONDS", "chat_timeout_seconds", &cfg.ChatTimeoutSeconds)
	// env 覆盖发生在上面的 Normalize 之后（envInt 只挡 <=0），超界值在此钳制，
	// 防止 Duration 换算溢出为负导致所有 chat 请求立即超时并被落盘
	if cfg.ChatTimeoutSeconds > MaxChatTimeoutSeconds {
		cfg.ChatTimeoutSeconds = MaxChatTimeoutSeconds
	}
	// growth 上报条数同理：env 超界在 Normalize 后重钳（envInt 只挡 <=0）
	if cfg.GrowthReportCount > GrowthReportCountMax {
		cfg.GrowthReportCount = GrowthReportCountMax
	}
	if cfg.GrowthReportJitter > GrowthReportJitterMax {
		cfg.GrowthReportJitter = GrowthReportJitterMax
	}

	// 默认管理密码：env > 文件 hash > 内置 "password"（生产请尽快修改）。
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
		const pw = "password"
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
		slog.Warn("首次启动：未设置管理密码，已使用默认密码（请尽快在管理后台修改）",
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

// Effective 返回当前配置（env 字段运行时不可被 PUT 覆盖）。
func (m *Manager) Effective() Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg
}

// Update 在锁内修改配置并落盘，fn 返回 error 则放弃修改。
func (m *Manager) Update(fn func(*Config) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	nc := m.cfg
	if err := fn(&nc); err != nil {
		return err
	}
	// env 覆盖的字段保持 env 值
	if m.envSets["region"] {
		nc.Region = m.cfg.Region
	}
	if m.envSets["auto_checkin"] {
		nc.AutoCheckin = m.cfg.AutoCheckin
	}
	if m.envSets["checkin_cron"] {
		nc.CheckinCron = m.cfg.CheckinCron
	}
	if m.envSets["checkin_mode"] {
		nc.CheckinMode = m.cfg.CheckinMode
	}
	if m.envSets["checkin_random_start"] {
		nc.CheckinRandomStart = m.cfg.CheckinRandomStart
	}
	if m.envSets["checkin_random_end"] {
		nc.CheckinRandomEnd = m.cfg.CheckinRandomEnd
	}
	if m.envSets["checkin_fallback"] {
		nc.CheckinFallback = m.cfg.CheckinFallback
	}
	if m.envSets["auto_growth"] {
		nc.AutoGrowth = m.cfg.AutoGrowth
	}
	if m.envSets["growth_report_cron"] {
		nc.GrowthReportCron = m.cfg.GrowthReportCron
	}
	if m.envSets["growth_travel_cron"] {
		nc.GrowthTravelCron = m.cfg.GrowthTravelCron
	}
	if m.envSets["growth_report_count"] {
		nc.GrowthReportCount = m.cfg.GrowthReportCount
	}
	if m.envSets["growth_report_jitter"] {
		nc.GrowthReportJitter = m.cfg.GrowthReportJitter
	}
	if m.envSets["resource_cache_seconds"] {
		nc.ResourceCacheSeconds = m.cfg.ResourceCacheSeconds
	}
	if m.envSets["log_retention_days"] {
		nc.LogRetentionDays = m.cfg.LogRetentionDays
	}
	if m.envSets["listen"] {
		nc.Listen = m.cfg.Listen
	}
	if m.envSets["log_max_size_mb"] {
		nc.LogMaxSizeMB = m.cfg.LogMaxSizeMB
	}
	if m.envSets["chat_timeout_seconds"] {
		nc.ChatTimeoutSeconds = m.cfg.ChatTimeoutSeconds
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

// ValidateChatTimeoutSeconds 校验 chat 响应头超时秒数（1 ~ MaxChatTimeoutSeconds）。
func ValidateChatTimeoutSeconds(n int) error {
	if n < 1 || n > MaxChatTimeoutSeconds {
		return fmt.Errorf("chat 响应超时需在 1-%d 秒之间", MaxChatTimeoutSeconds)
	}
	return nil
}

// ValidateListen 校验监听地址格式（host:port）。
func ValidateListen(listen string) error {
	listen = strings.TrimSpace(listen)
	if listen == "" {
		return fmt.Errorf("监听地址不能为空")
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("格式应为 host:port（如 127.0.0.1:10082 或 0.0.0.0:8080）")
	}
	if port == "" {
		return fmt.Errorf("缺少端口")
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 {
		return fmt.Errorf("端口须为 1-65535")
	}
	if host != "" && host != "localhost" && net.ParseIP(host) == nil {
		return fmt.Errorf("host 须为合法 IP（或留空监听全部接口）")
	}
	return nil
}
