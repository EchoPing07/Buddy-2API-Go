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
		c.CheckinCron = "0 0 9 * *" // 5 段（缺秒）非法
		c.GrowthReportCron = "bad"  // 非法
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

// TestPutSettingsTaskScheduleFields 任务侧新排程字段的校验与写入：
// 合法值写入、非法 cron 400、清扫间隔钳制、存量非法 cron 自愈。
func TestPutSettingsTaskScheduleFields(t *testing.T) {
	t.Run("合法值写入", func(t *testing.T) {
		h, _, fs := newGrowthTestHandler(t, "cn", false, &fakeGrowthAPI{})
		body := `{"growth_queue_cron":"0 30 2 * * *","blackcat_cron":"0 15 23 * * *",
			"task_sweep_enabled":false,"task_sweep_minutes":5}`
		rr := httptest.NewRecorder()
		h.putSettings(rr, httptest.NewRequest(http.MethodPut, "/admin/settings", strings.NewReader(body)))
		if rr.Code != http.StatusOK {
			t.Fatalf("合法值应 200，得到 %d: %s", rr.Code, rr.Body.String())
		}
		got := h.cfg.Get()
		if got.GrowthQueueCron != "0 30 2 * * *" || got.BlackcatCron != "0 15 23 * * *" {
			t.Errorf("cron 未写入: %q / %q", got.GrowthQueueCron, got.BlackcatCron)
		}
		if got.TaskSweepEnabled {
			t.Error("task_sweep_enabled=false 未生效")
		}
		if got.TaskSweepMinutes != 5 {
			t.Errorf("task_sweep_minutes 应为 5，得到 %d", got.TaskSweepMinutes)
		}
		if fs.reconfigureN == 0 {
			t.Error("应触发 Reconfigure（重启清扫 ticker）")
		}
	})

	t.Run("非法 cron 400", func(t *testing.T) {
		for _, tc := range []struct{ name, body, wantMsg string }{
			{"任务队列", `{"growth_queue_cron":"0 0 1 * *"}`, "任务队列"},
			{"夜猫子", `{"blackcat_cron":"bad"}`, "夜猫子"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				h, _, _ := newGrowthTestHandler(t, "cn", false, &fakeGrowthAPI{})
				rr := httptest.NewRecorder()
				h.putSettings(rr, httptest.NewRequest(http.MethodPut, "/admin/settings", strings.NewReader(tc.body)))
				if rr.Code != http.StatusBadRequest {
					t.Fatalf("非法 cron 应 400，得到 %d: %s", rr.Code, rr.Body.String())
				}
				if !strings.Contains(rr.Body.String(), tc.wantMsg) {
					t.Errorf("错误文案应指明字段 %q，得到 %s", tc.wantMsg, rr.Body.String())
				}
			})
		}
	})

	t.Run("清扫间隔钳制", func(t *testing.T) {
		h, _, _ := newGrowthTestHandler(t, "cn", false, &fakeGrowthAPI{})
		rr := httptest.NewRecorder()
		h.putSettings(rr, httptest.NewRequest(http.MethodPut, "/admin/settings",
			strings.NewReader(`{"task_sweep_minutes":99999}`)))
		if rr.Code != http.StatusOK {
			t.Fatalf("超界间隔应钳制而非报错，得到 %d: %s", rr.Code, rr.Body.String())
		}
		if got := h.cfg.Get().TaskSweepMinutes; got != config.TaskSweepMinutesMax {
			t.Errorf("应钳为 %d，得到 %d", config.TaskSweepMinutesMax, got)
		}

		rr = httptest.NewRecorder()
		h.putSettings(rr, httptest.NewRequest(http.MethodPut, "/admin/settings",
			strings.NewReader(`{"task_sweep_minutes":0}`)))
		if got := h.cfg.Get().TaskSweepMinutes; got < 1 {
			t.Errorf("0 应被钳到 >=1，得到 %d", got)
		}
	})

	t.Run("存量非法 cron 自愈", func(t *testing.T) {
		h, _, _ := newGrowthTestHandler(t, "cn", false, &fakeGrowthAPI{})
		if err := h.cfg.Update(func(c *config.Config) error {
			c.GrowthQueueCron = "bad"
			c.BlackcatCron = "also-bad"
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		rr := httptest.NewRecorder()
		h.putSettings(rr, httptest.NewRequest(http.MethodPut, "/admin/settings", strings.NewReader(`{"log_retention_days":30}`)))
		if rr.Code != http.StatusOK {
			t.Fatalf("存量非法 cron 不应阻断无关保存，得到 %d: %s", rr.Code, rr.Body.String())
		}
		got := h.cfg.Get()
		if got.GrowthQueueCron != config.DefaultGrowthQueueCron {
			t.Errorf("growth_queue_cron 应回落默认，得到 %q", got.GrowthQueueCron)
		}
		if got.BlackcatCron != config.DefaultBlackcatCron {
			t.Errorf("blackcat_cron 应回落默认，得到 %q", got.BlackcatCron)
		}
	})

	t.Run("getSettings 透出新字段", func(t *testing.T) {
		h, _, _ := newGrowthTestHandler(t, "cn", false, &fakeGrowthAPI{})
		rr := httptest.NewRecorder()
		h.getSettings(rr, httptest.NewRequest(http.MethodGet, "/admin/settings", nil))
		body := rr.Body.String()
		for _, key := range []string{"growth_queue_cron", "blackcat_cron", "task_sweep_enabled", "task_sweep_minutes"} {
			if !strings.Contains(body, `"`+key+`"`) {
				t.Errorf("getSettings 缺少字段 %s: %s", key, body)
			}
		}
	})
}
