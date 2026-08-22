package config

import "testing"

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
