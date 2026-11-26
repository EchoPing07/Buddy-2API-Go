package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"buddy2api-go/internal/auth"
)

// withURLParam 注入 chi 路由参数，无需挂完整路由树。
func withURLParam(r *http.Request, key, val string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add(key, val)
	return r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
}

// TestActivateAccount 切换账号：token.json 变为目标档案、旧账号被归档、
// 账号口径缓存被清（ResetAccountState + Reconfigure 各一次），且列表不含任何凭证。
func TestActivateAccount(t *testing.T) {
	h, _, fs := newGrowthTestHandler(t, "cn", true, &fakeGrowthAPI{}) // 当前账号 UID = "u"
	book := auth.NewAccountBook(h.toks.DataDir())
	if err := book.Save(&auth.Token{
		UID: "uid-b", AccessToken: "tb", Nickname: "B",
		Domain: "copilot.tencent.com", ExpiresAt: time.Now().Add(time.Hour).Unix(),
	}); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	h.activateAccount(w, withURLParam(
		httptest.NewRequest(http.MethodPost, "/admin/accounts/uid-b/activate", nil), "uid", "uid-b"))
	if w.Code != http.StatusOK {
		t.Fatalf("activate 状态码 = %d, body = %s", w.Code, w.Body.String())
	}
	if got := h.toks.Get(); got == nil || got.UID != "uid-b" {
		t.Fatalf("活动凭证未切换：%+v", got)
	}
	if fs.resetN != 1 || fs.reconfigureN != 1 {
		t.Fatalf("换账号应复位并重装配调度器一次，得到 reset=%d reconfigure=%d", fs.resetN, fs.reconfigureN)
	}
	if _, err := book.Load("u"); err != nil {
		t.Fatalf("旧账号未归档：%v", err)
	}

	// 列表：两个账号，当前账号标记正确，且响应体不含凭证字段
	w = httptest.NewRecorder()
	h.listAccounts(w, httptest.NewRequest(http.MethodGet, "/admin/accounts", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("list 状态码 = %d", w.Code)
	}
	if regexp.MustCompile(`access_token|refresh_token`).Match(w.Body.Bytes()) {
		t.Fatalf("账号列表泄漏了凭证：%s", w.Body.String())
	}
	var resp struct {
		Items []struct {
			UID      string `json:"uid"`
			Nickname string `json:"nickname"`
			Active   bool   `json:"active"`
		} `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Items) != 2 {
		t.Fatalf("列表应有 2 个账号，得到 %s", w.Body.String())
	}
	if !resp.Items[0].Active {
		t.Fatalf("当前账号应置顶，得到 %s", w.Body.String())
	}
	for _, it := range resp.Items {
		if (it.UID == "uid-b") != it.Active {
			t.Errorf("账号 %s 的 active 标记错误：%s", it.UID, w.Body.String())
		}
	}

	// 当前账号不允许移除（否则 token.json 会让档案自愈复活）
	w = httptest.NewRecorder()
	h.deleteAccountProfile(w, withURLParam(
		httptest.NewRequest(http.MethodDelete, "/admin/accounts/uid-b", nil), "uid", "uid-b"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("移除当前账号应 400，得到 %d", w.Code)
	}
}

// TestActivateSameAccountKeepsSchedulerState 切到同一账号不清缓存，
// 否则当天活跃上报会按新会话再发一轮。
func TestActivateSameAccountKeepsSchedulerState(t *testing.T) {
	h, _, fs := newGrowthTestHandler(t, "cn", true, &fakeGrowthAPI{}) // 当前账号 UID = "u"
	book := auth.NewAccountBook(h.toks.DataDir())
	if err := book.Save(&auth.Token{UID: "u", AccessToken: "t2"}); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	h.activateAccount(w, withURLParam(
		httptest.NewRequest(http.MethodPost, "/admin/accounts/u/activate", nil), "uid", "u"))
	if w.Code != http.StatusOK {
		t.Fatalf("activate 状态码 = %d, body = %s", w.Code, w.Body.String())
	}
	if fs.resetN != 0 || fs.reconfigureN != 0 {
		t.Fatalf("同账号切换不应复位调度器，得到 reset=%d reconfigure=%d", fs.resetN, fs.reconfigureN)
	}
}

// TestUpdateAccountRemark 覆盖账号备注的设置、同步（token.json / accounts / pool）、清空与校验。
func TestUpdateAccountRemark(t *testing.T) {
	h, _ := newPoolPanelFixture(t, "acc1", "acc2")
	fs := &fakeSched{}
	h.sched = fs

	// 1) 更新当前活动账号备注：同步到档案、token.json 与号池快照，且不触碰调度器
	w := httptest.NewRecorder()
	h.updateAccountProfile(w, withURLParam(
		httptest.NewRequest(http.MethodPut, "/admin/accounts/acc1", strings.NewReader(`{"remark":"  主力工作号  "}`)),
		"uid", "acc1"))
	if w.Code != http.StatusOK {
		t.Fatalf("更新当前账号备注失败 %d: %s", w.Code, w.Body.String())
	}
	if fs.resetN != 0 || fs.reconfigureN != 0 {
		t.Errorf("改备注不应触发调度器复位: reset=%d reconfigure=%d", fs.resetN, fs.reconfigureN)
	}
	if got := h.toks.Get(); got == nil || got.Remark != "主力工作号" {
		t.Fatalf("token.json 未同步备注: %+v", got)
	}
	if loaded, err := h.accountBook().Load("acc1"); err != nil || loaded.Remark != "主力工作号" {
		t.Fatalf("账号档案未同步备注: %+v, %v", loaded, err)
	}

	// GET /admin/account 返回 remark
	w = httptest.NewRecorder()
	h.account(w, httptest.NewRequest(http.MethodGet, "/admin/account", nil))
	if !strings.Contains(w.Body.String(), `"remark":"主力工作号"`) {
		t.Errorf("/admin/account 未返回备注: %s", w.Body.String())
	}

	// GET /admin/pool 实时反映备注（无需重载号池）
	w = httptest.NewRecorder()
	h.poolView(w, httptest.NewRequest(http.MethodGet, "/admin/pool", nil))
	if !strings.Contains(w.Body.String(), `"remark":"主力工作号"`) {
		t.Errorf("/admin/pool 未同步备注: %s", w.Body.String())
	}

	// 2) 更新非活动账号备注：仅改目标档案，不影响当前活动账号
	w = httptest.NewRecorder()
	h.updateAccountProfile(w, withURLParam(
		httptest.NewRequest(http.MethodPut, "/admin/accounts/acc2", strings.NewReader(`{"remark":"备用小号"}`)),
		"uid", "acc2"))
	if w.Code != http.StatusOK {
		t.Fatalf("更新非活动账号备注失败 %d: %s", w.Code, w.Body.String())
	}
	if got := h.toks.Get(); got == nil || got.Remark != "主力工作号" {
		t.Errorf("改非活动账号备注误改了当前账号: %+v", got)
	}

	// GET /admin/accounts 列表含两个账号的备注
	w = httptest.NewRecorder()
	h.listAccounts(w, httptest.NewRequest(http.MethodGet, "/admin/accounts", nil))
	if !strings.Contains(w.Body.String(), `"remark":"主力工作号"`) || !strings.Contains(w.Body.String(), `"remark":"备用小号"`) {
		t.Errorf("/admin/accounts 缺少备注: %s", w.Body.String())
	}

	// 3) 清空备注
	w = httptest.NewRecorder()
	h.updateAccountProfile(w, withURLParam(
		httptest.NewRequest(http.MethodPut, "/admin/accounts/acc2", strings.NewReader(`{"remark":""}`)),
		"uid", "acc2"))
	if w.Code != http.StatusOK {
		t.Fatalf("清空备注失败 %d: %s", w.Code, w.Body.String())
	}
	if loaded, err := h.accountBook().Load("acc2"); err != nil || loaded.Remark != "" {
		t.Fatalf("备注未清空: %+v, %v", loaded, err)
	}

	// 4) 校验错误：缺字段 / 超长 / 不存在
	for _, tc := range []struct {
		name string
		uid  string
		body string
		code int
	}{
		{"缺少 remark 字段", "acc1", `{}`, http.StatusBadRequest},
		{"备注超长", "acc1", `{"remark":"` + strings.Repeat("备", 65) + `"}`, http.StatusBadRequest},
		{"账号不存在", "ghost", `{"remark":"x"}`, http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.updateAccountProfile(w, withURLParam(
				httptest.NewRequest(http.MethodPut, "/admin/accounts/"+tc.uid, strings.NewReader(tc.body)),
				"uid", tc.uid))
			if w.Code != tc.code {
				t.Errorf("状态码 = %d，期望 %d: %s", w.Code, tc.code, w.Body.String())
			}
		})
	}
}

// TestUpdateAccountRemarkUnarchivedActive 仅存在 token.json（尚未入档）的旧安装也能直接设置备注。
func TestUpdateAccountRemarkUnarchivedActive(t *testing.T) {
	h, _, _ := newGrowthTestHandler(t, "cn", true, &fakeGrowthAPI{}) // 活动账号 u，档案目录为空
	w := httptest.NewRecorder()
	h.updateAccountProfile(w, withURLParam(
		httptest.NewRequest(http.MethodPut, "/admin/accounts/u", strings.NewReader(`{"remark":"老账号"}`)),
		"uid", "u"))
	if w.Code != http.StatusOK {
		t.Fatalf("未入档活动账号设备注失败 %d: %s", w.Code, w.Body.String())
	}
	if got := h.toks.Get(); got == nil || got.Remark != "老账号" {
		t.Fatalf("token.json 未更新备注: %+v", got)
	}
	if loaded, err := h.accountBook().Load("u"); err != nil || loaded.Remark != "老账号" {
		t.Fatalf("应顺带归档并保留备注: %+v, %v", loaded, err)
	}
}

// TestAccountRemarkImportExportRoundTrip 单导入带备注、重导无备注不覆盖原备注、批量导出导入保留备注。
func TestAccountRemarkImportExportRoundTrip(t *testing.T) {
	h, _, _ := newGrowthTestHandler(t, "cn", true, &fakeGrowthAPI{})

	// 1) 单导入带 remark
	body := `{"access_token":"` + testJWT(t, "uid-r1", "昵称1") + `","remark":"测试备注1"}`
	w := httptest.NewRecorder()
	h.accountImport(w, httptest.NewRequest(http.MethodPost, "/admin/account/import", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("单导入失败 %d: %s", w.Code, w.Body.String())
	}
	if loaded, err := h.accountBook().Load("uid-r1"); err != nil || loaded.Remark != "测试备注1" {
		t.Fatalf("单导入备注未落盘: %+v, %v", loaded, err)
	}

	// 2) 同账号重新导入（未传 remark）应保留原备注
	bodyNoRemark := `{"access_token":"` + testJWT(t, "uid-r1", "昵称1") + `"}`
	w = httptest.NewRecorder()
	h.accountImport(w, httptest.NewRequest(http.MethodPost, "/admin/account/import", strings.NewReader(bodyNoRemark)))
	if w.Code != http.StatusOK {
		t.Fatalf("重导失败 %d: %s", w.Code, w.Body.String())
	}
	if loaded, err := h.accountBook().Load("uid-r1"); err != nil || loaded.Remark != "测试备注1" {
		t.Fatalf("无 remark 重导清空了原备注: %+v, %v", loaded, err)
	}

	// 3) 导出 → 另一实例批量导入，备注须完整保留
	w = httptest.NewRecorder()
	h.accountExport(w, httptest.NewRequest(http.MethodGet, "/admin/account/export?scope=selected&uids=uid-r1", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("导出失败 %d: %s", w.Code, w.Body.String())
	}
	h2, _, _ := newGrowthTestHandler(t, "cn", false, &fakeGrowthAPI{})
	w2 := httptest.NewRecorder()
	h2.accountImportBatch(w2, httptest.NewRequest(http.MethodPost, "/admin/account/import/batch", strings.NewReader(w.Body.String())))
	if w2.Code != http.StatusOK {
		t.Fatalf("批量导入失败 %d: %s", w2.Code, w2.Body.String())
	}
	if loaded, err := h2.accountBook().Load("uid-r1"); err != nil || loaded.Remark != "测试备注1" {
		t.Fatalf("导出导入后备注丢失: %+v, %v", loaded, err)
	}
}
