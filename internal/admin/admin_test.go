package admin

import (
	"encoding/json"
	"fmt"
	"strconv"
	"testing"
	"time"
)

// daysLeftStr 把 *int 转为可读串（nil → "nil"），避免 t.Logf 用 %v 打印指针地址。
func daysLeftStr(p *int) string {
	if p == nil {
		return "nil"
	}
	return strconv.Itoa(*p)
}

// buildEnvelope 按上游 get-user-resource 真实响应结构（data.Response.Data）构造测试数据，
// 日期相对 now 偏移，保证测试不随时间失效。
func buildEnvelope(now time.Time) []byte {
	d100 := now.Add(100 * 24 * time.Hour).Format("2006-01-02 15:04:05") // 仅 ExpiredTime 为空时回退 DeductionEndTime
	deductionMs := now.Add(1000 * 24 * time.Hour).UnixMilli()
	d50 := now.Add(50 * 24 * time.Hour).Format("2006-01-02 15:04:05")   // CycleEndTime
	d365 := now.Add(365 * 24 * time.Hour).Format("2006-01-02 15:04:05") // 有效 ExpiredTime
	tpl := `{
	  "code": 0,
	  "data": {"Response": {"Data": {
	    "TotalDosage": 700, "TotalCount": 4,
	    "Accounts": [
	      {"PackageName":"体验版(ExpiredTime空→回退DeductionEndTime)","ProductName":"腾讯云代码助手","CapacityRemainPrecise":"500","CapacitySizePrecise":"500","CapacityUsedPrecise":"0","CycleCapacityRemainPrecise":"499.06","CapacityUnit":"credits","PackageType":"1","ResourceType":"2","AutoRenewFlag":0,"Status":0,"ExpiredTime":"","CycleEndTime":"%s","DeductionEndTime":%d},
	      {"PackageName":"裂变包(有ExpiredTime→优先用)","ProductName":"腾讯云代码助手","CapacityRemainPrecise":"1500","CapacityCapacityRemainPrecise":"1500","CycleCapacityRemainPrecise":"1500","CapacityUnit":"credits","PackageType":"1","ExpiredTime":"%s","CycleEndTime":"%s","DeductionEndTime":%d},
	      {"PackageName":"仅CycleEndTime(ExpiredTime空+DeductionEndTime=0)","ProductName":"腾讯云代码助手","CapacityRemainPrecise":"100","CycleCapacityRemainPrecise":"100","CapacityUnit":"credits","PackageType":"1","ExpiredTime":"","DeductionEndTime":0,"CycleEndTime":"%s"}
	    ]
	  }}}
	}`
	return []byte(fmt.Sprintf(tpl, d100, deductionMs, d365, d50, deductionMs, d50))
}

// TestProcessResourcesExpireFallback 验证到期时间解析回退链。
//
// 复现 bug：上游实测响应里 ExpiredTime 常为空字符串，真正到期落在 DeductionEndTime
// （毫秒时间戳）与 CycleEndTime（日期串）。旧实现只读 ExpiredTime，导致 expire_time
// 为空、days_left 为 null（前端显示“到期：—”）。修复后应按
// ExpiredTime → DeductionEndTime → CycleEndTime 回退。
func TestProcessResourcesExpireFallback(t *testing.T) {
	now := time.Now()
	out := processResources(buildEnvelope(now))
	accs, ok := out["accounts"].([]resourceAccount)
	if !ok || len(accs) != 3 {
		t.Fatalf("accounts 解析异常: ok=%v len=%v", ok, len(accs))
	}

	// 按包名建索引（processResources 会按到期升序排序，位置不固定）
	byName := map[string]resourceAccount{}
	for _, a := range accs {
		byName[a.PackageName] = a
		t.Logf("%s: expire_time=%q days_left=%s expired=%v warn=%q",
			a.PackageName, a.ExpireTime, daysLeftStr(a.DaysLeft), a.Expired, a.Warn)
	}

	// 1) ExpiredTime 空 → 应回退到 DeductionEndTime（数字 ms），展示串非空、days_left 解析出来
	if a, ok := byName["体验版(ExpiredTime空→回退DeductionEndTime)"]; !ok {
		t.Fatal("缺少体验版用例")
	} else {
		if a.ExpireTime == "" {
			t.Errorf("体验版 expire_time 为空（应回退 DeductionEndTime）")
		}
		if a.DaysLeft == nil {
			t.Errorf("体验版 days_left 为 nil（应解析出剩余天数）")
		}
		if a.Expired {
			t.Errorf("体验版不应标记已过期")
		}
	}

	// 2) ExpiredTime 有值 → 优先用它，不被 Deduction/Cycle 覆盖
	if a, ok := byName["裂变包(有ExpiredTime→优先用)"]; !ok {
		t.Fatal("缺少裂变包用例")
	} else {
		want := now.Add(365 * 24 * time.Hour).Format("2006-01-02 15:04:05")
		if a.ExpireTime != want {
			t.Errorf("裂变包 expire_time=%q，期望优先取 ExpiredTime=%q", a.ExpireTime, want)
		}
		if a.DaysLeft == nil {
			t.Errorf("裂变包 days_left 为 nil")
		}
	}

	// 3) ExpiredTime 空 + DeductionEndTime=0 → 回退到 CycleEndTime
	if a, ok := byName["仅CycleEndTime(ExpiredTime空+DeductionEndTime=0)"]; !ok {
		t.Fatal("缺少仅CycleEndTime用例")
	} else {
		want := now.Add(50 * 24 * time.Hour).Format("2006-01-02 15:04:05")
		if a.ExpireTime != want {
			t.Errorf("仅Cycle expire_time=%q，期望回退 CycleEndTime=%q", a.ExpireTime, want)
		}
		if a.DaysLeft == nil {
			t.Errorf("仅Cycle days_left 为 nil")
		}
	}

	_ = json.Marshal
}

// TestProcessResourcesCapacityFallback 验证额度包数值字段的 Precise → 非Precise 回退。
//
// 这些用例均不含周期字段（hasCycle=false），有效值=账户级，专门测回退链。
// 复现 bug：上游对某些包只返回非 Precise 字段或 Precise 为空串，旧实现仅读 *Precise，
// 导致“已用”一栏恒为 0。修复后按 Precise → 非Precise 取首个有值字段（空串/nil 回退，
// "0" 不回退），与 references/wicm84266964-Buddy2api 契约一致。
func TestProcessResourcesCapacityFallback(t *testing.T) {
	tpl := `{"code":0,"data":{"Response":{"Data":{
		"TotalDosage": 1000, "TotalCount": 4,
		"Accounts": [
			{"PackageName":"A_仅Precise字符串","CapacityRemainPrecise":"400.5","CapacitySizePrecise":"500","CapacityUsedPrecise":"99.5","ExpiredTime":""},
			{"PackageName":"B_Precise为空回退非Precise","CapacityRemainPrecise":"","CapacitySizePrecise":"","CapacityUsedPrecise":"","CapacityRemain":"400","CapacitySize":"500","CapacityUsed":"99","ExpiredTime":""},
			{"PackageName":"C_完全无Precise","CapacityRemain":"300","CapacitySize":"600","CapacityUsed":"300","ExpiredTime":""},
			{"PackageName":"D_Precise为零不回退","CapacityUsedPrecise":"0","CapacityUsed":"999","CapacityRemainPrecise":"500","CapacitySizePrecise":"500","ExpiredTime":""}
		]
	}}}}`
	out := processResources([]byte(tpl))
	accs, ok := out["accounts"].([]resourceAccount)
	if !ok || len(accs) != 4 {
		t.Fatalf("accounts 解析异常: ok=%v len=%v", ok, len(accs))
	}
	byName := map[string]resourceAccount{}
	for _, a := range accs {
		byName[a.PackageName] = a
	}

	cases := []struct {
		name                           string
		wantUsed, wantRemain, wantSize float64
	}{
		{"A_仅Precise字符串", 99.5, 400.5, 500},
		{"B_Precise为空回退非Precise", 99, 400, 500},
		{"C_完全无Precise", 300, 300, 600},
		{"D_Precise为零不回退", 0, 500, 500},
	}
	for _, c := range cases {
		a, ok := byName[c.name]
		if !ok {
			t.Fatalf("缺少用例 %q", c.name)
		}
		if a.CapacityUsed != c.wantUsed {
			t.Errorf("%s capacity_used=%v，期望 %v", c.name, a.CapacityUsed, c.wantUsed)
		}
		if a.CapacityRemain != c.wantRemain {
			t.Errorf("%s capacity_remain=%v，期望 %v", c.name, a.CapacityRemain, c.wantRemain)
		}
		if a.CapacitySize != c.wantSize {
			t.Errorf("%s capacity_size=%v，期望 %v", c.name, a.CapacitySize, c.wantSize)
		}
		t.Logf("%s: used=%v remain=%v size=%v", c.name, a.CapacityUsed, a.CapacityRemain, a.CapacitySize)
	}
}

// TestProcessResourcesCyclePreferred 验证存在周期数据时，有效展示值优先取周期级。
//
// 复现 bug：CodeBuddy 免费/裂变包按月周期计费，账户级 CapacityUsed 恒为 0，
// 真实已用落在 CycleCapacityUsed。旧实现只提取账户级，前端「已用」恒显 0、
// 进度条恒满。修复后：周期级 Remain/Used 任一 > 0 时，capacity_* 取周期值，
// 并额外暴露 cycle_used/cycle_size；周期总量缺失回退账户总量。
func TestProcessResourcesCyclePreferred(t *testing.T) {
	tpl := `{"code":0,"data":{"Response":{"Data":{
		"TotalDosage": 1000, "TotalCount": 3,
		"Accounts": [
			{"PackageName":"E_真实已用","CapacityRemainPrecise":"500","CapacitySizePrecise":"500","CapacityUsedPrecise":"0","CycleCapacityRemainPrecise":"335.48","CycleCapacitySizePrecise":"500","CycleCapacityUsedPrecise":"164.52","ExpiredTime":""},
			{"PackageName":"F_周期全零用账户级","CapacityRemainPrecise":"500","CapacitySizePrecise":"500","CapacityUsedPrecise":"0","CycleCapacityRemainPrecise":"0","CycleCapacityUsedPrecise":"0","CycleCapacitySizePrecise":"500","ExpiredTime":""},
			{"PackageName":"G_周期总量缺失回退账户","CapacityRemainPrecise":"500","CapacitySizePrecise":"500","CapacityUsedPrecise":"0","CycleCapacityRemainPrecise":"100","CycleCapacityUsedPrecise":"50","ExpiredTime":""}
		]
	}}}}`
	out := processResources([]byte(tpl))
	accs, ok := out["accounts"].([]resourceAccount)
	if !ok || len(accs) != 3 {
		t.Fatalf("accounts 解析异常: ok=%v len=%v", ok, len(accs))
	}
	byName := map[string]resourceAccount{}
	for _, a := range accs {
		byName[a.PackageName] = a
	}

	// E：账户 used=0 但周期 used=164.52 → 有效已用应为 164.52（核心修复点）
	if a := byName["E_真实已用"]; a.PackageName == "" {
		t.Fatal("缺少 E 用例")
	} else {
		if a.CapacityUsed != 164.52 {
			t.Errorf("E capacity_used=%v，期望 164.52（周期优先）", a.CapacityUsed)
		}
		if a.CapacityRemain != 335.48 {
			t.Errorf("E capacity_remain=%v，期望 335.48", a.CapacityRemain)
		}
		if a.CapacitySize != 500 {
			t.Errorf("E capacity_size=%v，期望 500", a.CapacitySize)
		}
		if a.CycleUsed != 164.52 || a.CycleSize != 500 || a.CycleRemain != 335.48 {
			t.Errorf("E cycle 字段错: remain=%v used=%v size=%v", a.CycleRemain, a.CycleUsed, a.CycleSize)
		}
		t.Logf("E: effective used=%v remain=%v size=%v | cycle used=%v remain=%v size=%v",
			a.CapacityUsed, a.CapacityRemain, a.CapacitySize, a.CycleUsed, a.CycleRemain, a.CycleSize)
	}

	// F：周期 Remain=0 且 Used=0 → 无周期活动，用账户级（used=0）
	if a := byName["F_周期全零用账户级"]; a.PackageName == "" {
		t.Fatal("缺少 F 用例")
	} else {
		if a.CapacityUsed != 0 {
			t.Errorf("F capacity_used=%v，期望 0（周期全零回退账户级）", a.CapacityUsed)
		}
		if a.CapacityRemain != 500 {
			t.Errorf("F capacity_remain=%v，期望 500（账户级）", a.CapacityRemain)
		}
		t.Logf("F: effective used=%v remain=%v size=%v", a.CapacityUsed, a.CapacityRemain, a.CapacitySize)
	}

	// G：周期有活动但无 CycleCapacitySize → 周期总量回退账户总量
	if a := byName["G_周期总量缺失回退账户"]; a.PackageName == "" {
		t.Fatal("缺少 G 用例")
	} else {
		if a.CapacityUsed != 50 || a.CapacityRemain != 100 || a.CapacitySize != 500 {
			t.Errorf("G effective 错: used=%v remain=%v size=%v（期望 50/100/500）", a.CapacityUsed, a.CapacityRemain, a.CapacitySize)
		}
		if a.CycleSize != 500 {
			t.Errorf("G cycle_size=%v，期望 500（回退账户总量）", a.CycleSize)
		}
		t.Logf("G: effective used=%v remain=%v size=%v | cycle_size=%v", a.CapacityUsed, a.CapacityRemain, a.CapacitySize, a.CycleSize)
	}
}

// TestProcessResourcesTotalDosageAggregation 验证 total_dosage 为本地聚合值：
// Σ 未过期包的有效剩余（周期优先 capacity_remain），不再透传上游 TotalDosage。
//
// 复现 bug：上游 TotalDosage 按账户级 CapacityRemain 聚合，而按月周期计费的
// 体验版包周期额度用尽后账户级 Remain 不随周期扣减（幻影剩余），导致余额页
// 顶部「官方总额度」恒多算该幻影值（实测 500），真实可用额度为 0 时仍显示
// 500。修复后 total_dosage 与包卡片「剩余」合计一致（实测 2236 → 1736.88），
// 上游原始值保留在 upstream_total_dosage 供对照。
func TestProcessResourcesTotalDosageAggregation(t *testing.T) {
	expired := time.Now().Add(-24 * time.Hour).Format("2006-01-02 15:04:05") // 已过期
	tpl := `{"code":0,"data":{"Response":{"Data":{
		"TotalDosage": 9999, "TotalCount": 5,
		"Accounts": [
			{"PackageName":"H_周期用尽的体验版(幻影500)","CapacityRemainPrecise":"500","CapacitySizePrecise":"500","CapacityUsedPrecise":"0","CycleCapacityRemainPrecise":"0","CycleCapacityUsedPrecise":"500","CycleCapacitySizePrecise":"500","CapacityUnit":"credits","ExpiredTime":""},
			{"PackageName":"I_正常裂变包","CapacityRemainPrecise":"1236.88","CapacitySizePrecise":"1500","CapacityUsedPrecise":"263.12","CycleCapacityRemainPrecise":"1236.88","CycleCapacitySizePrecise":"1500","CycleCapacityUsedPrecise":"263.12","CapacityUnit":"credits","ExpiredTime":""},
			{"PackageName":"J_已过期包不计入","CapacityRemainPrecise":"200","CapacitySizePrecise":"200","CapacityUsedPrecise":"0","CycleCapacityRemainPrecise":"200","CycleCapacitySizePrecise":"200","CycleCapacityUsedPrecise":"0","CapacityUnit":"credits","ExpiredTime":"%s"},
			{"PackageName":"K_到期未知计入","CapacityRemainPrecise":"80","CapacitySizePrecise":"100","CapacityUsedPrecise":"20","CycleCapacityRemainPrecise":"80","CycleCapacitySizePrecise":"100","CycleCapacityUsedPrecise":"20","CapacityUnit":"credits"},
			{"PackageName":"L_未动用签到包","CapacityRemainPrecise":"100","CapacitySizePrecise":"100","CapacityUsedPrecise":"0","CycleCapacityRemainPrecise":"100","CycleCapacitySizePrecise":"100","CycleCapacityUsedPrecise":"0","CapacityUnit":"credits","ExpiredTime":""}
		]
	}}}}`
	out := processResources([]byte(fmt.Sprintf(tpl, expired)))

	// total_dosage = 0(幻影不计) + 1236.88 + 0(已过期不计) + 80 + 100 = 1416.88
	if got, _ := out["total_dosage"].(float64); got != 1416.88 {
		t.Errorf("total_dosage=%v，期望 1416.88（Σ未过期包有效剩余，剔除幻影/已过期）", out["total_dosage"])
	}
	// upstream_total_dosage 原样透传官方值，不受聚合影响
	if got, _ := out["upstream_total_dosage"].(float64); got != 9999 {
		t.Errorf("upstream_total_dosage=%v，期望 9999（透传官方原始值）", out["upstream_total_dosage"])
	}

	accs, ok := out["accounts"].([]resourceAccount)
	if !ok || len(accs) != 5 {
		t.Fatalf("accounts 解析异常: ok=%v len=%v", ok, len(accs))
	}
	byName := map[string]resourceAccount{}
	for _, a := range accs {
		byName[a.PackageName] = a
	}
	// H：周期用尽 → 有效剩余 0（幻影 500 不进合计，包卡片口径一致）
	if a := byName["H_周期用尽的体验版(幻影500)"]; a.CapacityRemain != 0 {
		t.Errorf("H capacity_remain=%v，期望 0（周期优先）", a.CapacityRemain)
	}
	// J：已过期 → 不计入合计，但包级字段保留原值
	if a := byName["J_已过期包不计入"]; !a.Expired || a.CapacityRemain != 200 {
		t.Errorf("J expired=%v remain=%v，期望 expired=true remain=200（字段保留原值，仅不计入合计）", a.Expired, a.CapacityRemain)
	}
	// K：到期未知 → 不标记过期（已计入合计）
	if a := byName["K_到期未知计入"]; a.Expired || a.DaysLeft != nil {
		t.Errorf("K expired=%v days_left=%s，期望未过期且 days_left=nil", a.Expired, daysLeftStr(a.DaysLeft))
	}
	t.Logf("total_dosage=%v upstream_total_dosage=%v", out["total_dosage"], out["upstream_total_dosage"])
}

// TestProcessResourcesTotalDosageMissingUpstream 验证上游 Data 缺 TotalDosage 字段时
// upstream_total_dosage 落 0（number 对缺失字段返回 0，不 panic），且可用额度聚合
// 不依赖该字段照常计算。前端对照行守卫为 >0，0 时隐藏，不会误导显示「官方 TotalDosage：0」。
func TestProcessResourcesTotalDosageMissingUpstream(t *testing.T) {
	tpl := `{"code":0,"data":{"Response":{"Data":{
		"TotalCount": 1,
		"Accounts": [
			{"PackageName":"M_上游缺TotalDosage","CapacityRemainPrecise":"400.5","CapacitySizePrecise":"500","CapacityUsedPrecise":"99.5","CycleCapacityRemainPrecise":"400.5","CycleCapacitySizePrecise":"500","CycleCapacityUsedPrecise":"99.5","CapacityUnit":"credits","ExpiredTime":""}
		]
	}}}}`
	out := processResources([]byte(tpl))
	if got, _ := out["upstream_total_dosage"].(float64); got != 0 {
		t.Errorf("upstream_total_dosage=%v，期望 0（字段缺失时 number 落 0）", out["upstream_total_dosage"])
	}
	if got, _ := out["total_dosage"].(float64); got != 400.5 {
		t.Errorf("total_dosage=%v，期望 400.5（本地聚合不依赖上游字段）", out["total_dosage"])
	}
}

// TestProcessResourcesTotalDosageEqualsUpstream 验证健康账号（无周期外幻影、无过期包、
// 账户级与周期级一致）时本地聚合与官方 TotalDosage 相等——这是前端对照行隐藏
// （x-show 两值不等才显示）的数据前提。
func TestProcessResourcesTotalDosageEqualsUpstream(t *testing.T) {
	tpl := `{"code":0,"data":{"Response":{"Data":{
		"TotalDosage": 600, "TotalCount": 2,
		"Accounts": [
			{"PackageName":"N_正常包","CapacityRemainPrecise":"500","CapacitySizePrecise":"500","CapacityUsedPrecise":"0","CycleCapacityRemainPrecise":"500","CycleCapacitySizePrecise":"500","CycleCapacityUsedPrecise":"0","CapacityUnit":"credits","ExpiredTime":""},
			{"PackageName":"O_正常包","CapacityRemainPrecise":"100","CapacitySizePrecise":"100","CapacityUsedPrecise":"0","CycleCapacityRemainPrecise":"100","CycleCapacitySizePrecise":"100","CycleCapacityUsedPrecise":"0","CapacityUnit":"credits","ExpiredTime":""}
		]
	}}}}`
	out := processResources([]byte(tpl))
	got, _ := out["total_dosage"].(float64)
	up, _ := out["upstream_total_dosage"].(float64)
	if got != 600 || up != 600 {
		t.Errorf("total_dosage=%v upstream_total_dosage=%v，期望均为 600（健康账号两口径一致，对照行应隐藏）", got, up)
	}
}

// TestParseFlexibleTimeBillingZone 验证无时区格式的日期串按北京时间（UTC+8）解析，
// 不随部署时区漂移；RFC3339 自带偏移不受影响。过期判定（Expired → 是否计入
// 可用额度聚合）依赖该解析，非 +8 时区部署时若用 time.Local 解析会偏差最多 8 小时。
func TestParseFlexibleTimeBillingZone(t *testing.T) {
	ts, ok := parseFlexibleTime("2026-09-20 18:35:00")
	if !ok {
		t.Fatal("日期串解析失败")
	}
	if _, off := ts.Zone(); off != 8*3600 {
		t.Errorf("解析偏移 = %d 秒，期望 +28800（北京时间，不随部署时区变化）", off)
	}
	// RFC3339 自带偏移，解析结果应保留串内时区而非套用 billingTZ
	ts2, ok := parseFlexibleTime("2026-09-20T18:35:00+09:00")
	if !ok {
		t.Fatal("RFC3339 解析失败")
	}
	if _, off := ts2.Zone(); off != 9*3600 {
		t.Errorf("RFC3339 偏移 = %d 秒，期望 +32400（保留串内时区）", off)
	}
}

// TestNumField 覆盖 numField 的回退与容错路径。
func TestNumField(t *testing.T) {
	cases := []struct {
		name string
		acc  map[string]any
		keys []string
		want float64
	}{
		{"首个有值", map[string]any{"a": "1.5"}, []string{"a", "b"}, 1.5},
		{"空串回退下一个", map[string]any{"a": "", "b": "2.5"}, []string{"a", "b"}, 2.5},
		{"nil 回退下一个", map[string]any{"a": nil, "b": 3.5}, []string{"a", "b"}, 3.5},
		{"\"0\" 不回退（合法零）", map[string]any{"a": "0", "b": "999"}, []string{"a", "b"}, 0},
		{"float64 零不回退", map[string]any{"a": float64(0), "b": "999"}, []string{"a", "b"}, 0},
		{"Precise 不可解析→回退非Precise", map[string]any{"a": "abc", "b": "7.5"}, []string{"a", "b"}, 7.5},
		{"全部缺失→0", map[string]any{}, []string{"a", "b"}, 0},
		{"含空白字符的串", map[string]any{"a": "  4.5  "}, []string{"a"}, 4.5},
	}
	for _, c := range cases {
		got := numField(c.acc, c.keys...)
		if got != c.want {
			t.Errorf("%s: numField=%v，期望 %v", c.name, got, c.want)
		}
	}
}
