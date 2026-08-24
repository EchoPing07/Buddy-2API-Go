package upstream

import (
	"testing"
	"time"
)

func TestParseRateX(t *testing.T) {
	cases := []struct {
		in   string
		want float64
		ok   bool
	}{
		{"x0.79", 0.79, true},
		{"0.50x", 0.50, true},
		{"x0", 0, true},
		{"0x", 0, true},
		{" x1.62 ", 1.62, true},
		{"", 0, false},
		{"auto", 0, false},
		{"-1", 0, false},
	}
	for _, c := range cases {
		got, ok := parseRateX(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("parseRateX(%q) = %v,%v; want %v,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestFormatRateX(t *testing.T) {
	if got := formatRateX(0.5); got != "x0.50" {
		t.Errorf("formatRateX(0.5) = %q, want x0.50", got)
	}
	if got := formatRateX(3.47); got != "x3.47" {
		t.Errorf("formatRateX(3.47) = %q, want x3.47", got)
	}
}

func mustCN(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("加载时区失败: %v", err)
	}
	return loc
}

func TestPromoActiveCrossMidnight(t *testing.T) {
	loc := mustCN(t)
	sched := &PromoSchedule{
		Timezone: "Asia/Shanghai",
		Daily:    []DailyWindow{{Start: "23:00", End: "7:50"}}, // 跨零点
	}
	cases := []struct {
		hhmm string
		want bool
	}{
		{"01:00", true},  // 窗内（零点后）
		{"07:49", true},  // 窗内边界前
		{"23:30", true},  // 窗内（零点前）
		{"12:00", false}, // 白天窗外
		{"07:50", false}, // 结束时刻不含
		{"23:00", true},  // 起始时刻含
	}
	for _, c := range cases {
		hm, ok := parseHM(c.hhmm)
		if !ok {
			t.Fatalf("parseHM(%q) 失败", c.hhmm)
		}
		now := time.Date(2026, 8, 25, hm/60, hm%60, 0, 0, loc)
		if got := promoActive(sched, now); got != c.want {
			t.Errorf("promoActive(23:00-7:50 @ %s +08) = %v, want %v", c.hhmm, got, c.want)
		}
	}
}

func TestPromoActiveDailyAndValidity(t *testing.T) {
	loc := mustCN(t)
	// 高峰双窗 09:00-12:00 / 14:00-18:00
	peak := &PromoSchedule{
		Timezone: "Asia/Shanghai",
		Daily:    []DailyWindow{{Start: "9:00", End: "12:00"}, {Start: "14:00", End: "18:00"}},
	}
	if !promoActive(peak, time.Date(2026, 8, 25, 10, 0, 0, 0, loc)) {
		t.Error("10:00 应在高峰窗内")
	}
	if promoActive(peak, time.Date(2026, 8, 25, 13, 0, 0, 0, loc)) {
		t.Error("13:00 不应在高峰窗内")
	}

	// 已过期的限时免费
	expired := &PromoSchedule{Timezone: "Asia/Shanghai", ValidUntil: "2026-08-06T00:00:00+08:00"}
	if promoActive(expired, time.Date(2026, 8, 25, 10, 0, 0, 0, loc)) {
		t.Error("已过 validUntil 的活动不应生效")
	}
	future := &PromoSchedule{Timezone: "Asia/Shanghai", ValidFrom: "2026-09-01T00:00:00+08:00"}
	if promoActive(future, time.Date(2026, 8, 25, 10, 0, 0, 0, loc)) {
		t.Error("未到 validFrom 的活动不应生效")
	}

	// 无时间限制 = 恒生效
	always := &PromoSchedule{}
	if !promoActive(always, time.Date(2026, 8, 25, 3, 0, 0, 0, loc)) {
		t.Error("无 schedule 限制的活动应恒生效")
	}
}

func TestEffectiveRatePriorityAndFallback(t *testing.T) {
	loc := mustCN(t)
	night := time.Date(2026, 8, 25, 1, 0, 0, 0, loc)
	day := time.Date(2026, 8, 25, 12, 0, 0, 0, loc)

	mkPromo := func(id, modelID string, pri int, credits string, daily []DailyWindow) ModelPromotion {
		p := ModelPromotion{ID: id, Kind: "discount", Enabled: true, Priority: pri}
		p.Badge.Label = id
		p.ModelIDs = []string{modelID, modelID + "-ioa"}
		p.Discount.DiscountedCredits = credits
		p.Schedule.Daily = daily
		return p
	}
	nightWin := []DailyWindow{{Start: "23:00", End: "7:50"}}
	promos := []ModelPromotion{
		mkPromo("夜间折扣", "glm-5.2", 100, "0.50x", nightWin),
		mkPromo("限时免费", "glm-5.2", 200, "0x", nil), // 无时间限制的高优先级
	}

	// 高优先级免费命中 → x0
	rate, promo := effectiveRate(0.79, promos, "glm-5.2", night)
	if rate != 0 || promo != "限时免费" {
		t.Errorf("night: rate=%v promo=%q; want 0/限时免费", rate, promo)
	}

	// 去掉免费后（只留夜间折扣），折扣只在窗内生效
	promos = promos[:1]
	if r, p := effectiveRate(0.79, promos, "glm-5.2", night); r != 0.50 || p == "" {
		t.Errorf("night: rate=%v promo=%q; want 0.50", r, p)
	}
	if r, p := effectiveRate(0.79, promos, "glm-5.2", day); r != 0.79 || p != "" {
		t.Errorf("day: rate=%v promo=%q; want base 0.79", r, p)
	}

	// 不覆盖的模型 → 基础倍率
	if r, _ := effectiveRate(0.08, promos, "deepseek-v4-flash", night); r != 0.08 {
		t.Errorf("uncovered model rate = %v; want base 0.08", r)
	}

	// disabled 活动 → 忽略
	disabled := mkPromo("d", "glm-5.2", 300, "0x", nil)
	disabled.Enabled = false
	if r, _ := effectiveRate(0.79, []ModelPromotion{disabled}, "glm-5.2", day); r != 0.79 {
		t.Errorf("disabled promo should be ignored, got %v", r)
	}
}
