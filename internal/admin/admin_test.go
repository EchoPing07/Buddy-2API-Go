package admin

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// buildEnvelope 按上游 get-user-resource 真实响应结构（data.Response.Data）构造测试数据，
// 日期相对 now 偏移，保证测试不随时间失效。
func buildEnvelope(now time.Time) []byte {
	d100 := now.Add(100 * 24 * time.Hour).Format("2006-01-02 15:04:05")  // 仅 ExpiredTime 为空时回退 DeductionEndTime
	deductionMs := now.Add(1000 * 24 * time.Hour).UnixMilli()
	d50 := now.Add(50 * 24 * time.Hour).Format("2006-01-02 15:04:05")     // CycleEndTime
	d365 := now.Add(365 * 24 * time.Hour).Format("2006-01-02 15:04:05")   // 有效 ExpiredTime
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
		t.Logf("%s: expire_time=%q days_left=%v expired=%v warn=%q",
			a.PackageName, a.ExpireTime, a.DaysLeft, a.Expired, a.Warn)
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
