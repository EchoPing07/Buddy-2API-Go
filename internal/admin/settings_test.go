package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"buddy2api-go/internal/config"
)

// TestPutSettingsHealsIllegalStoredCron 复现并锁定设置页死锁修复：
// 存量（env/旧配置）非法 cron + PUT 未携带该字段时，保存应 200，
// 且非法值回落默认、无关字段正常写入，不因一条改不了的非法 cron 阻断整单。
func TestPutSettingsHealsIllegalStoredCron(t *testing.T) {
	h, _, fs := newGrowthTestHandler(t, "cn", false, &fakeGrowthAPI{})
	if err := h.cfg.Update(func(c *config.Config) error {
		c.CheckinCron = "0 0 9 * *"   // 5 段（缺秒）非法
		c.GrowthReportCron = "bad"    // 非法
		c.GrowthTravelCron = "0 0 9,21 * * *"
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPut, "/admin/settings", strings.NewReader(`{"log_retention_days":30}`))
	rr := httptest.NewRecorder()
	h.putSettings(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("存量非法 cron + 不提交该字段应 200，得到 %d: %s", rr.Code, rr.Body.String())
	}

	got := h.cfg.Get()
	if got.CheckinCron != config.DefaultCheckinCron {
		t.Errorf("checkin_cron 应回落默认 %q，得到 %q", config.DefaultCheckinCron, got.CheckinCron)
	}
	if got.GrowthReportCron != config.DefaultGrowthReportCron {
		t.Errorf("growth_report_cron 应回落默认 %q，得到 %q", config.DefaultGrowthReportCron, got.GrowthReportCron)
	}
	if got.GrowthTravelCron != "0 0 9,21 * * *" {
		t.Errorf("合法 growth_travel_cron 不应被改，得到 %q", got.GrowthTravelCron)
	}
	if got.LogRetentionDays != 30 {
		t.Errorf("log_retention_days 应为 30，得到 %d", got.LogRetentionDays)
	}
	if fs.reconfigureN == 0 {
		t.Error("保存成功后应触发调度器 Reconfigure")
	}
}

// TestPutSettingsRejectsIllegalCronWhenSubmitted 用户显式提交非法 cron 时仍应 400，
// 不能因为兜底逻辑而静默吞掉用户输入（字段始终可见可改，用户可据此修正）。
func TestPutSettingsRejectsIllegalCronWhenSubmitted(t *testing.T) {
	h, _, _ := newGrowthTestHandler(t, "cn", false, &fakeGrowthAPI{})
	req := httptest.NewRequest(http.MethodPut, "/admin/settings", strings.NewReader(`{"growth_report_cron":"0 0 10 * *"}`))
	rr := httptest.NewRecorder()
	h.putSettings(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("显式提交非法 cron 应 400，得到 %d: %s", rr.Code, rr.Body.String())
	}
}
