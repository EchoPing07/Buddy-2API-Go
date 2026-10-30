package billing

import (
	"encoding/json"
	"testing"
	"time"
)

// TestParseFlexibleTimeBillingZone 验证无时区格式的日期串按北京时间（UTC+8）解析，
// 不随部署时区漂移；RFC3339 自带偏移不受影响。过期判定（Expired → 是否计入
// 可用额度聚合）依赖该解析，非 +8 时区部署时若用 time.Local 解析会偏差最多 8 小时。
func TestParseFlexibleTimeBillingZone(t *testing.T) {
	ts, ok := ParseFlexibleTime("2026-09-20 18:35:00")
	if !ok {
		t.Fatal("日期串解析失败")
	}
	if _, off := ts.Zone(); off != 8*3600 {
		t.Errorf("解析偏移 = %d 秒，期望 +28800（北京时间，不随部署时区变化）", off)
	}
	// RFC3339 自带偏移，解析结果应保留串内时区而非套用 TZ
	ts2, ok := ParseFlexibleTime("2026-09-20T18:35:00+09:00")
	if !ok {
		t.Fatal("RFC3339 解析失败")
	}
	if _, off := ts2.Zone(); off != 9*3600 {
		t.Errorf("RFC3339 偏移 = %d 秒，期望 +32400（保留串内时区）", off)
	}
}

// TestParseFlexibleTimeMillis 毫秒时间戳（上游 DeductionEndTime 实测就是）。
func TestParseFlexibleTimeMillis(t *testing.T) {
	// 1793380602000 ms = 2026-10-31 01:16:42 +0800
	ts, ok := ParseFlexibleTime(float64(1793380602000))
	if !ok {
		t.Fatal("毫秒时间戳解析失败")
	}
	if ts.Unix() != 1793380602 {
		t.Errorf("unix = %d，期望 1793380602", ts.Unix())
	}
	// 秒级时间戳
	ts2, ok := ParseFlexibleTime(float64(1793380602))
	if !ok || ts2.Unix() != 1793380602 {
		t.Errorf("秒时间戳解析 = %v/%v", ts2, ok)
	}
}

// TestNumField 覆盖 NumField 的回退与容错路径。
func TestNumField(t *testing.T) {
	acc := map[string]any{
		"CapacityRemainPrecise":      "0",    // "0" 是合法零值，不回退
		"CapacityRemain":             "500",  // 不该被用到
		"CapacitySizePrecise":        "",     // 空串触发回退
		"CapacitySize":               "1500", //
		"CapacityUsedPrecise":        nil,    // nil 触发回退
		"CapacityUsed":               "300",  //
		"CycleCapacityRemain":        42.5,   // 数字形态直接返回
		"CycleCapacityRemainPrecise": "abc",  // 解析失败触发回退
	}
	cases := []struct {
		keys []string
		want float64
	}{
		{[]string{"CapacityRemainPrecise", "CapacityRemain"}, 0},
		{[]string{"CapacitySizePrecise", "CapacitySize"}, 1500},
		{[]string{"CapacityUsedPrecise", "CapacityUsed"}, 300},
		{[]string{"CycleCapacityRemainPrecise", "CycleCapacityRemain"}, 42.5},
		{[]string{"Missing", "AlsoMissing"}, 0},
	}
	for _, c := range cases {
		if got := NumField(acc, c.keys...); got != c.want {
			t.Errorf("NumField(%v) = %v，期望 %v", c.keys, got, c.want)
		}
	}
}

// TestTotalDosageRealShape 用**上游真实字段形态**验证额度聚合：
// capacity 是字符串、DeductionEndTime 是毫秒数字、日期串无时区。
// 这条测试是号池探测的护栏：算错会让探测把健康账号打成「额度用尽」。
func TestTotalDosageRealShape(t *testing.T) {
	future := time.Now().Add(48 * time.Hour).UnixMilli()
	past := time.Now().Add(-48 * time.Hour).UnixMilli()
	data, _ := json.Marshal(map[string]any{
		"data": map[string]any{
			"Response": map[string]any{
				"Data": map[string]any{
					"Accounts": []any{
						// 有效包，字符串额度，周期优先
						map[string]any{
							"PackageName": "A", "CapacityRemainPrecise": "0",
							"CycleCapacityRemainPrecise": "499.06",
							"CycleCapacityUsedPrecise":   "0.94",
							"DeductionEndTime":           future,
						},
						// 有效包，只有账户级额度（回落分支）
						map[string]any{
							"PackageName": "B", "CapacityRemainPrecise": "120.5",
							"DeductionEndTime": future,
						},
						// 已过期包，必须排除
						map[string]any{
							"PackageName": "C", "CapacityRemainPrecise": "999",
							"DeductionEndTime": past,
						},
						// 无到期信息：视为有效（与面板一致）
						map[string]any{
							"PackageName": "D", "CapacityRemainPrecise": "1",
						},
					},
				},
			},
		},
	})
	got := TotalDosage(data)
	want := 499.06 + 120.5 + 1
	if diff := got - want; diff > 0.01 || diff < -0.01 {
		t.Errorf("TotalDosage = %v，期望 %v（字符串额度 + 毫秒到期 + 排除过期包）", got, want)
	}
}

// TestTotalDosageZeroWhenExhausted 全部耗尽时必须返回 0（探测据此禁用账号）。
func TestTotalDosageZeroWhenExhausted(t *testing.T) {
	data := []byte(`{"Accounts":[{"CapacityRemainPrecise":"0","CycleCapacityRemainPrecise":"0"},{"CapacityRemain":"0"}]}`)
	if got := TotalDosage(data); got != 0 {
		t.Errorf("全耗尽应返回 0，得到 %v", got)
	}
}

// TestTotalDosageBadPayload 非 JSON / 缺字段时返回 0 而不是 panic。
func TestTotalDosageBadPayload(t *testing.T) {
	for _, in := range []string{``, `not json`, `{}`, `{"Accounts":"notarray"}`} {
		if got := TotalDosage([]byte(in)); got != 0 {
			t.Errorf("%q → %v，期望 0", in, got)
		}
	}
}
