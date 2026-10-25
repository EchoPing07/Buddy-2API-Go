package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
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
