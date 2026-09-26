package admin

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"buddy2api-go/internal/store"
	"buddy2api-go/internal/upstream"
)

// accountScopedCacheRows 凭证变更后必须清空的「账号口径」展示缓存行（表, key）。
// 这些 key 都是固定字串（单账号设计），换账号后不删的话，新账号在 TTL 内会读到上一账号的余额/签到。
var accountScopedCacheRows = [][2]string{
	{"resource_cache", resourceCacheKey},
	{"resource_cache", "default"},    // 历史残留键
	{"resource_cache", "default_v2"}, // 历史残留键
	{"checkin_cache", "status"},
	{"growth_cache", growthKeyOverview},
}

// testJWT 构造只要求「结构合法」的假 JWT：auth.ParseJWT 不验签，只看 payload。
func testJWT(t *testing.T, uid, nickname string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"sub":      uid,
		"iss":      "https://copilot.tencent.com/auth/realms/copilot",
		"nickname": nickname,
		"exp":      time.Now().Add(24 * time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

// TestCredentialChangeInvalidatesAccountCaches 凭证切换（导入 / 登出）后：
// 账号口径展示缓存全清、random_target 保留、scheduler.ResetAccountState 恰好调用一次。
//
// 回归背景：换账号后旧账号的余额/签到/成长会在整个 TTL（最长 14 天）内继续展示给新账号。
// 成长日标志 / last_run 的清理在 scheduler.ResetAccountState 内，由 TestResetAccountStateDropsDayState 覆盖。
// oauthPoll 走 h.client.OAuthPoll（无注入点，测不进来），三条路径共用 invalidateAccountCaches，
// 此处覆盖可注入的 import / delete 两条。
func TestCredentialChangeInvalidatesAccountCaches(t *testing.T) {
	const newUID = "uid-new"

	cases := []struct {
		name string
		req  func(token string) *http.Request
		call func(h *Handler, w http.ResponseWriter, r *http.Request)
	}{
		{
			name: "import",
			req: func(token string) *http.Request {
				body := `{"access_token":"` + token + `"}`
				return httptest.NewRequest(http.MethodPost, "/admin/account/import", strings.NewReader(body))
			},
			call: func(h *Handler, w http.ResponseWriter, r *http.Request) { h.accountImport(w, r) },
		},
		{
			name: "delete",
			req: func(string) *http.Request {
				return httptest.NewRequest(http.MethodDelete, "/admin/account", nil)
			},
			call: func(h *Handler, w http.ResponseWriter, r *http.Request) { h.accountDelete(w, r) },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, st, _ := newGrowthTestHandler(t, "cn", true, &fakeGrowthAPI{})
			fs := &fakeSched{}
			h.sched = fs

			// 旧账号的缓存：账号口径行 + 与账号无关的 random_target（验证未被误删）
			for _, row := range accountScopedCacheRows {
				if err := st.SetCache(row[0], row[1], `{"stale":true}`); err != nil {
					t.Fatalf("写缓存 %s/%s: %v", row[0], row[1], err)
				}
			}
			randomAt := time.Now().Format(time.RFC3339)
			if err := st.SetCache("checkin_cache", "random_target", randomAt); err != nil {
				t.Fatal(err)
			}

			rec := httptest.NewRecorder()
			tc.call(h, rec, tc.req(testJWT(t, newUID, "新账号")))
			if rec.Code != http.StatusOK {
				t.Fatalf("应 200，得到 %d: %s", rec.Code, rec.Body.String())
			}
			if tc.name == "import" {
				if got := h.toks.Get(); got == nil || got.UID != newUID {
					t.Fatalf("导入应落到新凭证，得到 %+v", got)
				}
			}

			for _, row := range accountScopedCacheRows {
				// ttl=0 → 不做过期判断，只问「行还在不在」
				if payload, _, ok := st.GetCache(row[0], row[1], 0); ok && payload != "" {
					t.Errorf("%s/%s 应随凭证变更清空，仍存在: %s", row[0], row[1], payload)
				}
			}
			if payload, _, ok := st.GetCache("checkin_cache", "random_target", 0); !ok || payload != randomAt {
				t.Errorf("random_target 与账号无关（当日随机签到时刻），不应被清除，得到 %q ok=%v", payload, ok)
			}
			if fs.resetN != 1 {
				t.Errorf("应调用 ResetAccountState 恰好 1 次，得到 %d", fs.resetN)
			}
			if fs.reconfigureN != 1 {
				t.Errorf("应调用 Reconfigure 恰好 1 次（补排成长补跑），得到 %d", fs.reconfigureN)
			}
		})
	}
}

// seedAccountDayState 写入旧账号的账号口径展示缓存与成长日标志，模拟「今日已完成」，
// 返回 CST 日期串（与 scheduler 日标志同口径）。
func seedAccountDayState(t *testing.T, st *store.Store) string {
	t.Helper()
	for _, row := range accountScopedCacheRows {
		if err := st.SetCache(row[0], row[1], `{"stale":true}`); err != nil {
			t.Fatalf("写缓存 %s/%s: %v", row[0], row[1], err)
		}
	}
	day := upstream.GrowthTodayDate(time.Now())
	for _, k := range []string{growthKeyReportDay, growthKeyAdoptDay, growthKeyRewardDay} {
		if err := st.SetCache(growthCacheTable, k, day); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SetCache(growthCacheTable, growthKeyLastRun, `{"day":"`+day+`"}`); err != nil {
		t.Fatal(err)
	}
	return day
}

// postImport 走一遍导入端点：token 由 uid 构造（auth.ParseJWT 不验签）。
func postImport(t *testing.T, h *Handler, uid, nickname string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"access_token":"` + testJWT(t, uid, nickname) + `"}`
	rec := httptest.NewRecorder()
	h.accountImport(rec, httptest.NewRequest(http.MethodPost, "/admin/account/import", strings.NewReader(body)))
	return rec
}

// TestAccountImportSameAccountKeepsDayState 回归 Bug：重导同一账号凭证不得失效缓存/复位日内状态。
//
// 场景：token 过期后补传同一份 token.json（同一 JWT sub → 同一 UID），或刷新失败后重扫同一账号。
// 若清了 report_day，当天会再上报一轮（新 conversation id 在上游看是新会话，活跃上报翻倍，
// 正是 1.5s/条限速要避免的）。
func TestAccountImportSameAccountKeepsDayState(t *testing.T) {
	h, st, _ := newGrowthTestHandler(t, "cn", true, &fakeGrowthAPI{}) // 旧凭证 UID="u"
	fs := &fakeSched{}
	h.sched = fs
	day := seedAccountDayState(t, st)

	rec := postImport(t, h, "u", "同名账号")
	if rec.Code != http.StatusOK {
		t.Fatalf("重导同一账号应 200，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if payload, _, ok := st.GetCache(growthCacheTable, growthKeyReportDay, 0); !ok || payload != day {
		t.Errorf("同账号重导不应清 report_day，得到 ok=%v payload=%q", ok, payload)
	}
	// 展示缓存也不该被清：前端看到的还是同一账号的数据，无需重取
	for _, row := range accountScopedCacheRows {
		if _, _, ok := st.GetCache(row[0], row[1], 0); !ok {
			t.Errorf("%s/%s 同账号重导不应清除", row[0], row[1])
		}
	}
	if fs.resetN != 0 || fs.reconfigureN != 0 {
		t.Errorf("同账号重导不应复位日内状态/重装配调度，得到 resetN=%d reconfigureN=%d",
			fs.resetN, fs.reconfigureN)
	}
}

// TestAccountImportSwitchInvalidatesAndRearms 真换账号（UID 不同）→ 清成长日标志 + 复位日内状态 + 重装配；
// 后两步缺一不可：只清标志不重装配的话，新账号要等到第二天 10:00 的 cron 才上报，白丢一天连登。
func TestAccountImportSwitchInvalidatesAndRearms(t *testing.T) {
	h, st, _ := newGrowthTestHandler(t, "cn", true, &fakeGrowthAPI{}) // 旧凭证 UID="u"
	fs := &fakeSched{}
	h.sched = fs
	seedAccountDayState(t, st)

	rec := postImport(t, h, "uid-other", "新账号")
	if rec.Code != http.StatusOK {
		t.Fatalf("换账号导入应 200，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if got := h.toks.Get(); got == nil || got.UID != "uid-other" {
		t.Fatalf("导入应落到新凭证，得到 %+v", got)
	}
	// admin 自己服务的展示缓存由 invalidateAccountCaches 直接删（成长日标志归 ResetAccountState，
	// 由 scheduler 侧的真库用例覆盖）
	for _, row := range accountScopedCacheRows {
		if payload, _, ok := st.GetCache(row[0], row[1], 0); ok && payload != "" {
			t.Errorf("%s/%s 应随换账号清除，仍存在: %s", row[0], row[1], payload)
		}
	}
	if fs.resetN != 1 {
		t.Errorf("换账号应复位日内状态 1 次，得到 %d", fs.resetN)
	}
	if fs.reconfigureN != 1 {
		t.Errorf("换账号应重装配调度 1 次（补排当日成长补跑），得到 %d", fs.reconfigureN)
	}
}

// TestInvalidateOrderingResetBeforeReconfigure 顺序断言：ResetAccountState 必须先于 Reconfigure。
// Reconfigure 内的 scheduleGrowthCatchup 读 report_day 决定排不排补跑，先跑它就只能读到旧标志。
func TestInvalidateOrderingResetBeforeReconfigure(t *testing.T) {
	h, st, _ := newGrowthTestHandler(t, "cn", true, &fakeGrowthAPI{})
	fs := &fakeSched{}
	h.sched = fs
	seedAccountDayState(t, st)

	rec := postImport(t, h, "uid-other", "新账号")
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if len(fs.calls) != 2 || fs.calls[0] != "reset" || fs.calls[1] != "reconfigure" {
		t.Errorf("调用顺序应为 [reset reconfigure]，得到 %v", fs.calls)
	}
}

// TestAccountRefreshNeverInvalidates 续期只换同一账号的 token，绝不能失效缓存/复位日内状态：
// 否则每次自动刷新都会清掉当天「已上报」并触发一次补跑（重复上报）。
// 测试环境无网络，上游必然报错，故不断言状态码 —— 契约是「无论刷新成败都不触达复位」。
func TestAccountRefreshNeverInvalidates(t *testing.T) {
	h, st, _ := newGrowthTestHandler(t, "cn", true, &fakeGrowthAPI{})
	fs := &fakeSched{}
	h.sched = fs
	day := seedAccountDayState(t, st)

	rec := httptest.NewRecorder()
	h.accountRefresh(rec, httptest.NewRequest(http.MethodPost, "/admin/account/refresh", nil))
	if fs.resetN != 0 || fs.reconfigureN != 0 {
		t.Errorf("续期不应复位日内状态/重装配，得到 resetN=%d reconfigureN=%d (HTTP %d)",
			fs.resetN, fs.reconfigureN, rec.Code)
	}
	if payload, _, ok := st.GetCache(growthCacheTable, growthKeyReportDay, 0); !ok || payload != day {
		t.Errorf("续期不应清 report_day，得到 ok=%v payload=%q", ok, payload)
	}
}
