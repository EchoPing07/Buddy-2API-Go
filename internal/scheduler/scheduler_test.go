package scheduler

import (
	"testing"
	"time"
)

// at 构造 2026-08-22（周六）当地时刻。
func at(h, m int) time.Time { return time.Date(2026, 8, 22, h, m, 0, 0, time.Local) }

// TestCheckinDayDecide 覆盖签到状态机各分支：
// 主时刻触发（含宕机补签）→ 失败 5 分钟重试一次 → 末班兜底 → 全部失败当天放弃。
func TestCheckinDayDecide(t *testing.T) {
	fb := at(23, 50)

	// 1) 主时刻前：无事可做
	d := &checkinDay{mainAt: at(9, 0)}
	if l, r := d.decide(at(8, 59), true, fb); l != "" || r {
		t.Errorf("主时刻前不应动作: label=%q retry=%v", l, r)
	}
	// 2) 主时刻到 → 定时签到，允许一次重试
	if l, r := d.decide(at(9, 0), true, fb); l != "定时签到" || !r || !d.mainHit {
		t.Errorf("主时刻应触发: label=%q retry=%v mainHit=%v", l, r, d.mainHit)
	}
	// 3) 宕机错过主时刻 → 下一次 tick 直接补签
	d2 := &checkinDay{mainAt: at(9, 0)}
	if l, _ := d2.decide(at(15, 0), true, fb); l != "定时签到" {
		t.Errorf("错过主时刻应补签: label=%q", l)
	}
	// 4) 重试到期 → 阶段重试，且不再允许重试（再失败当天放弃）
	d3 := &checkinDay{mainAt: at(9, 0), mainHit: true, retryAt: at(9, 5), retryTag: "定时签到"}
	if l, _ := d3.decide(at(9, 4), true, fb); l != "" {
		t.Errorf("重试时刻未到不应动作: label=%q", l)
	}
	if l, r := d3.decide(at(9, 5), true, fb); l != "定时签到重试" || r || !d3.retryAt.IsZero() {
		t.Errorf("重试应触发且只一次: label=%q retry=%v retryAt=%v", l, r, d3.retryAt)
	}
	if l, _ := d3.decide(at(9, 6), true, fb); l != "" {
		t.Errorf("重试后再无动作（等待末班）: label=%q", l)
	}
	// 5) 主阶段已放弃 → 末班 23:50 前静默，到点触发末班签到
	d4 := &checkinDay{mainAt: at(9, 0), mainHit: true}
	if l, _ := d4.decide(at(23, 49), true, fb); l != "" {
		t.Errorf("末班前不应动作: label=%q", l)
	}
	if l, r := d4.decide(at(23, 50), true, fb); l != "末班签到" || !r || !d4.fbHit {
		t.Errorf("末班应触发: label=%q retry=%v fbHit=%v", l, r, d4.fbHit)
	}
	// 6) 末班失败 → 重试一次后彻底放弃
	d5 := &checkinDay{mainHit: true, fbHit: true, retryAt: at(23, 55), retryTag: "末班签到"}
	if l, r := d5.decide(at(23, 55), true, fb); l != "末班签到重试" || r {
		t.Errorf("末班重试应触发且不再重试: label=%q retry=%v", l, r)
	}
	if l, _ := d5.decide(at(23, 59), true, fb); l != "" {
		t.Errorf("末班重试失败后当天认栽: label=%q", l)
	}
	// 7) 关闭末班兜底 → 全天只剩主尝试+重试
	d6 := &checkinDay{mainAt: at(9, 0), mainHit: true}
	if l, _ := d6.decide(at(23, 55), false, fb); l != "" {
		t.Errorf("关闭兜底后末班不应触发: label=%q", l)
	}
	// 8) 当日已落袋 → 一切静默
	d7 := &checkinDay{mainAt: at(9, 0), secured: true}
	if l, _ := d7.decide(at(9, 0), true, fb); l != "" {
		t.Errorf("已落袋不应再动作: label=%q", l)
	}
	// 9) 当日无主时刻（cron 当日不触发）→ 只剩末班
	d8 := &checkinDay{}
	if l, _ := d8.decide(at(12, 0), true, fb); l != "" {
		t.Errorf("无主时刻白天不应动作: label=%q", l)
	}
	if l, _ := d8.decide(at(23, 50), true, fb); l != "末班签到" {
		t.Errorf("无主时刻末班应触发: label=%q", l)
	}
}

// TestFixedMainAt 验证 fixed 模式当日主时刻推算：取 cron 当日触发、无触发/非法表达式返回零值。
func TestFixedMainAt(t *testing.T) {
	// 每天 9 点：即使当前已过 9 点，也返回今日 9 点（供补签判断）
	if got := fixedMainAt("0 0 9 * * *", at(12, 0)); got != at(9, 0) {
		t.Errorf("每日 cron 应返回今日 09:00，得到 %v", got)
	}
	// 仅周一：2026-08-22 是周六，当日无触发
	if got := fixedMainAt("0 0 9 * * 1", at(12, 0)); !got.IsZero() {
		t.Errorf("周六用仅周一 cron 应返回零值，得到 %v", got)
	}
	// 仅周六：当日有触发
	if got := fixedMainAt("0 0 9 * * 6", at(8, 0)); got != at(9, 0) {
		t.Errorf("周六用仅周六 cron 应返回今日 09:00，得到 %v", got)
	}
	// 非法表达式
	if got := fixedMainAt("not-a-cron", at(12, 0)); !got.IsZero() {
		t.Errorf("非法 cron 应返回零值，得到 %v", got)
	}
}

// TestRandomMinute 随机时刻必须始终落在 [start, end) 内。
func TestRandomMinute(t *testing.T) {
	const sm, em = 9 * 60, 18 * 60
	for i := 0; i < 1000; i++ {
		if m := randomMinute(sm, em); m < sm || m >= em {
			t.Fatalf("随机时刻越界: %d 不在 [%d,%d)", m, sm, em)
		}
	}
	if m := randomMinute(sm, sm+1); m != sm {
		t.Errorf("单分钟窗口应恒为边界值: %d", m)
	}
}

// TestMinuteOfDay 当日分钟数换算与末班时刻。
func TestMinuteOfDay(t *testing.T) {
	if got := minuteOfDay(at(15, 30), 23*60+50); got != at(23, 50) {
		t.Errorf("末班应为 23:50，得到 %v", got)
	}
	if got := minuteOfDay(at(15, 30), 0); got != at(0, 0) {
		t.Errorf("0 分钟应为零点，得到 %v", got)
	}
}
