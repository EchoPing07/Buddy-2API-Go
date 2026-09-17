package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestParseHHMM(t *testing.T) {
	cases := []struct {
		in     string
		want   int
		wantOK bool
	}{
		{"09:00", 540, true},
		{"23:59", 1439, true},
		{"0:00", 0, true},
		{" 09:30 ", 570, true},
		{"24:00", 0, false},
		{"09:60", 0, false},
		{"-1:00", 0, false},
		{"09:0", 540, true},
		{"0900", 0, false},
		{"", 0, false},
		{"aa:bb", 0, false},
	}
	for _, c := range cases {
		got, ok := ParseHHMM(c.in)
		if ok != c.wantOK || (ok && got != c.want) {
			t.Errorf("ParseHHMM(%q)=%v,%v 期望 %v,%v", c.in, got, ok, c.want, c.wantOK)
		}
	}
}

func TestValidateCheckinWindow(t *testing.T) {
	ok := []struct{ start, end string }{
		{"09:00", "18:00"},
		{"00:00", "23:30"}, // 上边界刚好允许
		{"09:00", "23:30"},
	}
	for _, c := range ok {
		if err := ValidateCheckinWindow(c.start, c.end); err != nil {
			t.Errorf("ValidateCheckinWindow(%q,%q) 不应报错，得到 %v", c.start, c.end, err)
		}
	}
	bad := []struct{ start, end string }{
		{"18:00", "09:00"}, // 结束早于开始
		{"09:00", "09:00"}, // 相等
		{"9:00", "25:00"},  // 格式非法
		{"09:00", "23:31"}, // 超过最晚 23:30
		{"", "18:00"},
		{"09:00", ""},
	}
	for _, c := range bad {
		if err := ValidateCheckinWindow(c.start, c.end); err == nil {
			t.Errorf("ValidateCheckinWindow(%q,%q) 应报错", c.start, c.end)
		}
	}
}

func TestNormalizeCheckin(t *testing.T) {
	// 空配置 → 全部补默认
	var c Config
	c.Normalize()
	if c.CheckinMode != "fixed" || c.CheckinRandomStart != "09:00" || c.CheckinRandomEnd != "18:00" {
		t.Errorf("空配置补默认失败: mode=%q window=%q~%q", c.CheckinMode, c.CheckinRandomStart, c.CheckinRandomEnd)
	}
	// 非法 mode / 非法窗口 → 回退默认，合法自定义保留
	c = Config{CheckinMode: "foo", CheckinRandomStart: "25:00", CheckinRandomEnd: "x"}
	c.Normalize()
	if c.CheckinMode != "fixed" || c.CheckinRandomStart != "09:00" || c.CheckinRandomEnd != "18:00" {
		t.Errorf("非法值未回退默认: mode=%q window=%q~%q", c.CheckinMode, c.CheckinRandomStart, c.CheckinRandomEnd)
	}
	c = Config{CheckinMode: "random", CheckinRandomStart: "08:30", CheckinRandomEnd: "12:00"}
	c.Normalize()
	if c.CheckinMode != "random" || c.CheckinRandomStart != "08:30" || c.CheckinRandomEnd != "12:00" {
		t.Errorf("合法自定义被误改: mode=%q window=%q~%q", c.CheckinMode, c.CheckinRandomStart, c.CheckinRandomEnd)
	}
}

func TestValidateChatTimeoutSeconds(t *testing.T) {
	for _, n := range []int{1, 60, MaxChatTimeoutSeconds} { // 上下边界均合法
		if err := ValidateChatTimeoutSeconds(n); err != nil {
			t.Errorf("ValidateChatTimeoutSeconds(%d) 不应报错，得到 %v", n, err)
		}
	}
	for _, n := range []int{0, -1, MaxChatTimeoutSeconds + 1, 9999999999} { // 9999999999 秒→纳秒换算溢出区
		if err := ValidateChatTimeoutSeconds(n); err == nil {
			t.Errorf("ValidateChatTimeoutSeconds(%d) 应报错", n)
		}
	}
}

func TestNormalizeChatTimeout(t *testing.T) {
	c := Config{ChatTimeoutSeconds: 0}
	c.Normalize()
	if c.ChatTimeoutSeconds != 60 {
		t.Errorf("0 应回退默认 60，得到 %d", c.ChatTimeoutSeconds)
	}
	c = Config{ChatTimeoutSeconds: 9999999999}
	c.Normalize()
	if c.ChatTimeoutSeconds != MaxChatTimeoutSeconds {
		t.Errorf("超界应钳到 %d，得到 %d", MaxChatTimeoutSeconds, c.ChatTimeoutSeconds)
	}
	c = Config{ChatTimeoutSeconds: 300}
	c.Normalize()
	if c.ChatTimeoutSeconds != 300 {
		t.Errorf("合法值不应被改，得到 %d", c.ChatTimeoutSeconds)
	}
}

// TestLoadEnvChatTimeoutClamp 复现 bug：Load 的 Normalize 在 env 覆盖之前执行，
// envInt 只挡 <=0，超界 env 值会绕过钳制直接进运行配置并被落盘。
func TestLoadEnvChatTimeoutClamp(t *testing.T) {
	t.Setenv("BUDDY2API_CHAT_TIMEOUT_SECONDS", "9999999999")
	dir := t.TempDir()
	m, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Get().ChatTimeoutSeconds; got != MaxChatTimeoutSeconds {
		t.Errorf("env 超界应钳到 %d，得到 %d", MaxChatTimeoutSeconds, got)
	}
	// 落盘的 config.json 也应是钳制后的值
	raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var persisted Config
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.ChatTimeoutSeconds != MaxChatTimeoutSeconds {
		t.Errorf("落盘值应已钳制，得到 %d", persisted.ChatTimeoutSeconds)
	}
}

// TestNormalizeGrowth 验证 growth 配置归一化：空回落默认、条数钳 1..10。
func TestNormalizeGrowth(t *testing.T) {
	// 空配置：开关/cron 回落默认；count 零值钳 1（缺省 5 来自 defaults()，
	// 仅 Load 路径应用；Normalize 只做钳制修复，与 ResourceCacheSeconds 语义不同）
	var c Config
	c.Normalize()
	if c.AutoGrowth {
		t.Error("auto_growth 默认应为 false")
	}
	if c.GrowthReportCron != "0 0 10 * * *" || c.GrowthTravelCron != "0 0 9,21 * * *" {
		t.Errorf("growth cron 默认值异常: %q / %q", c.GrowthReportCron, c.GrowthTravelCron)
	}
	if c.GrowthReportCount != 1 {
		t.Errorf("零值 count 应钳为 1，得到 %d", c.GrowthReportCount)
	}
	if d := defaults(); d.GrowthReportCount != 5 || d.AutoGrowth {
		t.Errorf("defaults 应为 count=5 且开关关，得到 %+v", d)
	}
	// 条数钳制：<=0 → 1；>10 → 10；合法值保留
	c = Config{GrowthReportCount: 0}
	c.Normalize()
	if c.GrowthReportCount != 1 {
		t.Errorf("0 应钳为 1，得到 %d", c.GrowthReportCount)
	}
	c = Config{GrowthReportCount: 99}
	c.Normalize()
	if c.GrowthReportCount != 10 {
		t.Errorf("99 应钳为 10，得到 %d", c.GrowthReportCount)
	}
	c = Config{GrowthReportCron: "0 0 15 * * *", GrowthReportCount: 3}
	c.Normalize()
	if c.GrowthReportCron != "0 0 15 * * *" || c.GrowthReportCount != 3 {
		t.Errorf("合法自定义不应被改: %q %d", c.GrowthReportCron, c.GrowthReportCount)
	}
	// cron 空白串回落默认（config 层不校验 cron 合法性，与 checkin_cron 同策略）
	c = Config{GrowthReportCron: "  ", GrowthTravelCron: ""}
	c.Normalize()
	if c.GrowthReportCron != "0 0 10 * * *" || c.GrowthTravelCron != "0 0 9,21 * * *" {
		t.Errorf("空白 cron 应回落默认，得到 %q / %q", c.GrowthReportCron, c.GrowthTravelCron)
	}
}

// TestLoadEnvGrowth 验证 growth 字段 env 覆盖与超界钳制（envInt 只挡 <=0，
// >10 的 env 值需在 Load 内重钳，防止超界值进入运行配置并被落盘）。
func TestLoadEnvGrowth(t *testing.T) {
	t.Setenv("BUDDY2API_AUTO_GROWTH", "true")
	t.Setenv("BUDDY2API_GROWTH_REPORT_CRON", "0 0 11 * * *")
	t.Setenv("BUDDY2API_GROWTH_TRAVEL_CRON", "0 0 8,20 * * *")
	t.Setenv("BUDDY2API_GROWTH_REPORT_COUNT", "99")
	dir := t.TempDir()
	m, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := m.Get()
	if !got.AutoGrowth || got.GrowthReportCron != "0 0 11 * * *" || got.GrowthTravelCron != "0 0 8,20 * * *" {
		t.Errorf("env 覆盖失败: %+v", got)
	}
	if got.GrowthReportCount != 10 {
		t.Errorf("env 超界条数应钳为 10，得到 %d", got.GrowthReportCount)
	}
	// env 覆盖字段运行时不可被 PUT 改写（Update 试图改 false/3 应被 env 值 true/10 保持）
	if err := m.Update(func(c *Config) error { c.AutoGrowth = false; c.GrowthReportCount = 3; return nil }); err != nil {
		t.Fatal(err)
	}
	if !m.Get().AutoGrowth || m.Get().GrowthReportCount != 10 {
		t.Errorf("env 字段应不可被 Update 覆盖: %+v", m.Get())
	}
}
