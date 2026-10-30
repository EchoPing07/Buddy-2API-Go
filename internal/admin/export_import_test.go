package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"buddy2api-go/internal/auth"
)

// decodeBundle 解析导出响应为凭证包（导出内容必须是 auth.Bundle，不再是裸 token.json）。
func decodeBundle(t *testing.T, body []byte) auth.Bundle {
	t.Helper()
	var b auth.Bundle
	if err := json.Unmarshal(body, &b); err != nil {
		t.Fatalf("导出内容不是合法凭证包: %v\n%s", err, body)
	}
	if b.Format != auth.BundleFormat {
		t.Fatalf("导出包 format = %q，期望 %q", b.Format, auth.BundleFormat)
	}
	return b
}

// bundleUIDs 包里所有账号的 uid 集合（断言导出范围用）。
func bundleUIDs(b auth.Bundle) map[string]bool {
	out := make(map[string]bool, len(b.Accounts))
	for _, a := range b.Accounts {
		out[a.UID] = true
	}
	return out
}

// TestAccountExportScopes 导出三种范围：current / all / selected。
//
// TestAccountExportScopes 导出两种范围：current（单个） / selected（批量勾选）。
//
// 刻意没有「all」这种第三种范围：批量导出在前端默认全选，而 /admin/accounts 会自愈补存未入档的
// 活动账号，所以「全选 + 导出」已完全覆盖它的产物。「导出全部」单独作为一条分支只是多一处口径
// 要维护；本用例的「selected 全选」条目就是它的等价物（断言导出内容与旧 all 完全一致）。
//
// 导出格式统一为凭证包（单账号与多账号同构），文件名走 Content-Disposition，
// 账号数经 X-Export-Count 告知前端，便于「已导出 N 个账号」的提示不必再解析响应体。
func TestAccountExportScopes(t *testing.T) {
	h, _, _ := newGrowthTestHandler(t, "cn", true, &fakeGrowthAPI{}) // 活动账号 UID = "u"
	book := auth.NewAccountBook(h.toks.DataDir())
	for _, uid := range []string{"uid-b", "uid-c"} {
		if err := book.Save(&auth.Token{
			UID: uid, AccessToken: "tok-" + uid, Nickname: "昵称-" + uid,
			Domain: "copilot.tencent.com", ExpiresAt: time.Now().Add(time.Hour).Unix(),
		}); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		name      string
		query     string
		wantUIDs  []string
		wantCode  int
		wantCount string
	}{
		{name: "默认=当前账号", query: "", wantUIDs: []string{"u"}, wantCode: http.StatusOK, wantCount: "1"},
		{name: "current", query: "?scope=current", wantUIDs: []string{"u"}, wantCode: http.StatusOK, wantCount: "1"},
		// 批量导出的全选形态：与旧 scope=all 产物等价
		{name: "selected 全选", query: "?scope=selected&uids=u,uid-b,uid-c",
			wantUIDs: []string{"u", "uid-b", "uid-c"}, wantCode: http.StatusOK, wantCount: "3"},
		// 逗号分隔
		{name: "selected 逗号", query: "?scope=selected&uids=uid-b,uid-c",
			wantUIDs: []string{"uid-b", "uid-c"}, wantCode: http.StatusOK, wantCount: "2"},
		// 重复参数 + 去重
		{name: "selected 重复参数", query: "?scope=selected&uids=uid-b&uids=uid-c&uids=uid-b",
			wantUIDs: []string{"uid-b", "uid-c"}, wantCode: http.StatusOK, wantCount: "2"},
		// 活动账号取 token.json（档案是可能落后的旁挂副本）
		{name: "selected 含活动账号", query: "?scope=selected&uids=u",
			wantUIDs: []string{"u"}, wantCode: http.StatusOK, wantCount: "1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.accountExport(w, httptest.NewRequest(http.MethodGet, "/admin/account/export"+tc.query, nil))
			if w.Code != tc.wantCode {
				t.Fatalf("状态码 = %d，期望 %d: %s", w.Code, tc.wantCode, w.Body.String())
			}
			if got := w.Header().Get("X-Export-Count"); got != tc.wantCount {
				t.Errorf("X-Export-Count = %q，期望 %q", got, tc.wantCount)
			}
			b := decodeBundle(t, w.Body.Bytes())
			got := bundleUIDs(b)
			if len(got) != len(tc.wantUIDs) {
				t.Fatalf("导出账号 = %v，期望 %v", got, tc.wantUIDs)
			}
			for _, uid := range tc.wantUIDs {
				if !got[uid] {
					t.Errorf("导出账号缺少 %q：%v", uid, got)
				}
			}
			// 活动账号必须标注，供人工核对（导入侧不据此启用）
			if b.ActiveUID != "u" {
				t.Errorf("active_uid = %q，期望 u", b.ActiveUID)
			}
			if !strings.Contains(w.Header().Get("Content-Disposition"), ".json") {
				t.Errorf("Content-Disposition 未带 json 文件名: %q", w.Header().Get("Content-Disposition"))
			}
			// 导出内容必须是完整凭证（access_token 不能是掩码）
			for _, a := range b.Accounts {
				if a.AccessToken == "" || strings.Contains(a.AccessToken, "...") {
					t.Errorf("账号 %s 的 access_token 不是原值: %q", a.UID, a.AccessToken)
				}
			}
		})
	}
}

// TestAccountExportSelectedPartialMissing 部分所选账号不存在：照导可导的，另经 X-Export-Missing
// 告知被跳过的 uid —— 静默少导会让用户以为备份完整。
func TestAccountExportSelectedPartialMissing(t *testing.T) {
	h, _, _ := newGrowthTestHandler(t, "cn", true, &fakeGrowthAPI{})
	book := auth.NewAccountBook(h.toks.DataDir())
	if err := book.Save(&auth.Token{UID: "uid-b", AccessToken: "tok-b"}); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	h.accountExport(w, httptest.NewRequest(http.MethodGet,
		"/admin/account/export?scope=selected&uids=uid-b,uid-ghost", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Export-Missing"); got != "uid-ghost" {
		t.Errorf("X-Export-Missing = %q，期望 uid-ghost", got)
	}
	b := decodeBundle(t, w.Body.Bytes())
	if len(b.Accounts) != 1 || b.Accounts[0].UID != "uid-b" {
		t.Errorf("应只导出存在的账号，得到 %+v", b.Accounts)
	}
}

// TestAccountExportRejects 非法导出请求：未知范围、空选择、无活动账号、所选全不存在。
func TestAccountExportRejects(t *testing.T) {
	t.Run("未知范围", func(t *testing.T) {
		h, _, _ := newGrowthTestHandler(t, "cn", true, &fakeGrowthAPI{})
		w := httptest.NewRecorder()
		h.accountExport(w, httptest.NewRequest(http.MethodGet, "/admin/account/export?scope=bogus", nil))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("状态码 = %d，期望 400: %s", w.Code, w.Body.String())
		}
	})
	t.Run("selected 空 uids", func(t *testing.T) {
		h, _, _ := newGrowthTestHandler(t, "cn", true, &fakeGrowthAPI{})
		w := httptest.NewRecorder()
		h.accountExport(w, httptest.NewRequest(http.MethodGet, "/admin/account/export?scope=selected", nil))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("状态码 = %d，期望 400: %s", w.Code, w.Body.String())
		}
	})
	t.Run("selected 全不存在", func(t *testing.T) {
		h, _, _ := newGrowthTestHandler(t, "cn", true, &fakeGrowthAPI{})
		w := httptest.NewRecorder()
		h.accountExport(w, httptest.NewRequest(http.MethodGet,
			"/admin/account/export?scope=selected&uids=ghost", nil))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("状态码 = %d，期望 400: %s", w.Code, w.Body.String())
		}
	})
	t.Run("未登录导出当前账号", func(t *testing.T) {
		h, _, _ := newGrowthTestHandler(t, "cn", false, &fakeGrowthAPI{})
		w := httptest.NewRecorder()
		h.accountExport(w, httptest.NewRequest(http.MethodGet, "/admin/account/export?scope=current", nil))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("状态码 = %d，期望 400: %s", w.Code, w.Body.String())
		}
	})
	t.Run("未登录导出批量（档案库有账号）", func(t *testing.T) {
		h, _, _ := newGrowthTestHandler(t, "cn", false, &fakeGrowthAPI{})
		book := auth.NewAccountBook(h.toks.DataDir())
		if err := book.Save(&auth.Token{UID: "uid-b", AccessToken: "tok-b"}); err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		h.accountExport(w, httptest.NewRequest(http.MethodGet, "/admin/account/export?scope=selected&uids=uid-b", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("状态码 = %d，期望 200（无活动账号也应有档案可导）: %s", w.Code, w.Body.String())
		}
		b := decodeBundle(t, w.Body.Bytes())
		if b.ActiveUID != "" {
			t.Errorf("无活动账号时 active_uid 应为空，得到 %q", b.ActiveUID)
		}
		if len(b.Accounts) != 1 {
			t.Errorf("应导出 1 份档案，得到 %+v", b.Accounts)
		}
	})
	t.Run("all 已不再支持", func(t *testing.T) {
		h, _, _ := newGrowthTestHandler(t, "cn", true, &fakeGrowthAPI{})
		w := httptest.NewRecorder()
		h.accountExport(w, httptest.NewRequest(http.MethodGet, "/admin/account/export?scope=all", nil))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("scope=all 应已移除（批量默认全选已覆盖它），得到 %d: %s", w.Code, w.Body.String())
		}
	})
}

// TestAccountExportSelectedCoversUnarchivedActive 旧安装（只登录过一次、没有档案）的批量导出
// 必须能包含活动账号：前端全选用的是 /admin/accounts 列表，而该端点会自愈补存活动账号，
// 所以全选一定含当前账号（否则「批量导出全部」会漏掉唯一在用的那份）。
func TestAccountExportSelectedCoversUnarchivedActive(t *testing.T) {
	h, _, _ := newGrowthTestHandler(t, "cn", true, &fakeGrowthAPI{}) // token.json 有 "u"，档案库为空

	// 模拟前端：先取列表（会自愈入档），再用列表里的 uid 做全选导出
	lw := httptest.NewRecorder()
	h.listAccounts(lw, httptest.NewRequest(http.MethodGet, "/admin/accounts", nil))
	if lw.Code != http.StatusOK {
		t.Fatalf("列表失败 %d: %s", lw.Code, lw.Body.String())
	}
	var list struct {
		Items []struct {
			UID string `json:"uid"`
		} `json:"items"`
	}
	if err := json.Unmarshal(lw.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || list.Items[0].UID != "u" {
		t.Fatalf("列表应只含未入档的活动账号 u，得到 %s", lw.Body.String())
	}

	w := httptest.NewRecorder()
	h.accountExport(w, httptest.NewRequest(http.MethodGet, "/admin/account/export?scope=selected&uids=u", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d: %s", w.Code, w.Body.String())
	}
	b := decodeBundle(t, w.Body.Bytes())
	if !bundleUIDs(b)["u"] {
		t.Errorf("批量全选导出漏掉未入档的活动账号: %+v", b.Accounts)
	}
}

// TestAccountExportImportRoundTrip 导出 → 导入（批量）闭环：账号、凭证字段均须原样还原，
// 且无论包里 active_uid 是谁，批量导入都不得切换活动账号。
func TestAccountExportImportRoundTrip(t *testing.T) {
	h, _, fs := newGrowthTestHandler(t, "cn", true, &fakeGrowthAPI{}) // 活动账号 UID = "u"
	// 活动凭证换成真 JWT：导出的是原值，导入侧会重新校验 JWT（fixture 默认的 "t" 无法过检）
	if err := h.toks.Save(&auth.Token{UID: "u", AccessToken: testJWT(t, "u", "账号U"),
		Nickname: "账号U", Domain: "copilot.tencent.com"}); err != nil {
		t.Fatal(err)
	}
	book := auth.NewAccountBook(h.toks.DataDir())
	ends := time.Now().Add(3 * time.Hour).Unix()
	if err := book.Save(&auth.Token{
		UID: "uid-b", AccessToken: testJWT(t, "uid-b", "账号B"), RefreshToken: "rt-b",
		Nickname: "账号B", Domain: "copilot.tencent.com", ExpiresAt: ends,
	}); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	h.accountExport(w, httptest.NewRequest(http.MethodGet, "/admin/account/export?scope=selected&uids=u,uid-b", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("导出失败 %d: %s", w.Code, w.Body.String())
	}
	exported := decodeBundle(t, w.Body.Bytes())
	if len(exported.Accounts) != 2 {
		t.Fatalf("应导出 2 个账号，得到 %d", len(exported.Accounts))
	}

	// 换一个干净实例导入，验证还原（不能同实例自查自，那样档案早已存在）
	h2, _, fs2 := newGrowthTestHandler(t, "cn", true, &fakeGrowthAPI{}) // 活动账号 UID = "u"
	if err := h2.toks.Save(&auth.Token{UID: "u", AccessToken: testJWT(t, "u", "账号U")}); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(exported)
	if err != nil {
		t.Fatal(err)
	}
	w2 := httptest.NewRecorder()
	h2.accountImportBatch(w2, httptest.NewRequest(http.MethodPost, "/admin/account/import/batch", strings.NewReader(string(body))))
	if w2.Code != http.StatusOK {
		t.Fatalf("批量导入失败 %d: %s", w2.Code, w2.Body.String())
	}
	var resp struct {
		Imported int  `json:"imported"`
		Failed   int  `json:"failed"`
		Total    int  `json:"total"`
		Active   bool `json:"active"`
		Items    []struct {
			UID   string `json:"uid"`
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		} `json:"items"`
	}
	if err := json.Unmarshal(w2.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Imported != 2 || resp.Failed != 0 || resp.Total != 2 {
		t.Fatalf("导入结果 = %+v", resp)
	}
	if resp.Active {
		t.Errorf("批量导入不得切换活动账号（active 应为 false）")
	}
	// 活动账号不变、调度器未被触碰
	if got := h2.toks.Get(); got == nil || got.UID != "u" {
		t.Errorf("批量导入换了活动凭证: %+v", got)
	}
	if fs2.resetN != 0 || fs2.reconfigureN != 0 {
		t.Errorf("批量导入不应复位日内状态/重装配，得到 reset=%d reconfigure=%d", fs2.resetN, fs2.reconfigureN)
	}
	metas, err := h2.accountBook().List()
	if err != nil {
		t.Fatal(err)
	}
	if !hasAccountUID(metas, "uid-b") {
		t.Errorf("导出的账号未还原到档案库: %+v", metas)
	}
	// 凭证字段（含 refresh_token / expires_at）必须完整，否则还原后需立刻重新登录
	loaded, err := h2.accountBook().Load("uid-b")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.RefreshToken != "rt-b" || loaded.ExpiresAt != ends || loaded.Nickname != "账号B" {
		t.Errorf("还原的凭证不完整: %+v", loaded)
	}
	_ = fs // h 的调度器（导出不应触碰它）
	if fs.resetN != 0 {
		t.Errorf("导出不应触碰调度器，得到 resetN=%d", fs.resetN)
	}
}

// TestAccountImportBatchPartialFailure 批量导入逐条尽力而为：坏条目单独报错，好的照常入档。
// 全或无会让用户先手工清理包里的脏数据才能导入剩下的，这在批量语义下不可接受。
func TestAccountImportBatchPartialFailure(t *testing.T) {
	h, _, _ := newGrowthTestHandler(t, "cn", true, &fakeGrowthAPI{})
	bundle := auth.Bundle{
		Format:  auth.BundleFormat,
		Version: auth.BundleVersion,
		Accounts: []auth.Token{
			{AccessToken: testJWT(t, "uid-ok", "好账号")},
			{AccessToken: "not-a-jwt"}, // 坏条目
			{AccessToken: ""},          // 空条目
		},
	}
	body, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.accountImportBatch(w, httptest.NewRequest(http.MethodPost, "/admin/account/import/batch", strings.NewReader(string(body))))
	// 有成功项即 200（部分成功由前端逐条展示），不该整包报错
	if w.Code != http.StatusOK {
		t.Fatalf("部分成功应 200，得到 %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Imported int `json:"imported"`
		Failed   int `json:"failed"`
		Total    int `json:"total"`
		Items    []struct {
			UID   string `json:"uid"`
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		} `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Imported != 1 || resp.Failed != 2 || resp.Total != 3 {
		t.Fatalf("汇总 = imported:%d failed:%d total:%d，期望 1/2/3（%s）",
			resp.Imported, resp.Failed, resp.Total, w.Body.String())
	}
	if !resp.Items[0].OK || resp.Items[0].UID != "uid-ok" {
		t.Errorf("第 1 条应成功且带 uid: %+v", resp.Items[0])
	}
	for i := 1; i < 3; i++ {
		if resp.Items[i].OK || resp.Items[i].Error == "" {
			t.Errorf("第 %d 条应失败且带原因: %+v", i+1, resp.Items[i])
		}
	}
	metas, err := h.accountBook().List()
	if err != nil {
		t.Fatal(err)
	}
	if !hasAccountUID(metas, "uid-ok") {
		t.Errorf("成功条目应入档: %+v", metas)
	}
}

// TestAccountImportBatchAllFail 全部失败 → 400（前端可用统一的错误气泡，而非「成功但全红」）。
func TestAccountImportBatchAllFail(t *testing.T) {
	h, _, _ := newGrowthTestHandler(t, "cn", true, &fakeGrowthAPI{})
	body := `{"format":"buddy2api.accounts","version":1,"accounts":[{"access_token":"bad"},{"access_token":""}]}`
	w := httptest.NewRecorder()
	h.accountImportBatch(w, httptest.NewRequest(http.MethodPost, "/admin/account/import/batch", strings.NewReader(body)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("全失败应 400，得到 %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"failed":2`) {
		t.Errorf("响应应包含失败计数与明细: %s", w.Body.String())
	}
}

// TestAccountImportBatchRejectsMalformed 非法请求体（非包 / 旧裸 token.json / 空列表）→ 400，
// 且不得往档案库写任何东西。
func TestAccountImportBatchRejectsMalformed(t *testing.T) {
	bodies := map[string]string{
		"非 JSON":       `not json`,
		"旧裸 token":     `{"access_token":"` + testJWT(t, "uid-x", "X") + `"}`,
		"format 不匹配":   `{"format":"other","version":1,"accounts":[{"access_token":"x"}]}`,
		"版本过高":         `{"format":"buddy2api.accounts","version":9,"accounts":[{"access_token":"x"}]}`,
		"空账号列表":        `{"format":"buddy2api.accounts","version":1,"accounts":[]}`,
		"accounts 非数组": `{"format":"buddy2api.accounts","version":1,"accounts":"x"}`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			h, _, _ := newGrowthTestHandler(t, "cn", true, &fakeGrowthAPI{})
			w := httptest.NewRecorder()
			h.accountImportBatch(w, httptest.NewRequest(http.MethodPost, "/admin/account/import/batch", strings.NewReader(body)))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("状态码 = %d，期望 400: %s", w.Code, w.Body.String())
			}
			metas, err := h.accountBook().List()
			if err != nil {
				t.Fatal(err)
			}
			if len(metas) != 0 {
				t.Errorf("非法请求不应写入档案: %+v", metas)
			}
		})
	}
}

// TestAccountImportBatchDoesNotTouchCaches 批量导入只入档：账号口径缓存与日内状态一概不碰
// （与单导入「仅导入」同口径）。否则批量补录备用账号会让当前账号当天丢掉「已签到 / 已上报」。
func TestAccountImportBatchDoesNotTouchCaches(t *testing.T) {
	h, st, _ := newGrowthTestHandler(t, "cn", true, &fakeGrowthAPI{})
	fs := &fakeSched{}
	h.sched = fs
	day := seedAccountDayState(t, st)

	body := `{"format":"buddy2api.accounts","version":1,"accounts":[{"access_token":"` +
		testJWT(t, "uid-backup", "备用") + `"}]}`
	w := httptest.NewRecorder()
	h.accountImportBatch(w, httptest.NewRequest(http.MethodPost, "/admin/account/import/batch", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d: %s", w.Code, w.Body.String())
	}
	if fs.resetN != 0 || fs.reconfigureN != 0 {
		t.Errorf("批量导入不应复位日内状态/重装配，得到 reset=%d reconfigure=%d", fs.resetN, fs.reconfigureN)
	}
	for _, row := range accountScopedCacheRows {
		if _, _, ok := st.GetCache(row[0], row[1], 0); !ok {
			t.Errorf("%s/%s 批量导入（账号未变）不应清除", row[0], row[1])
		}
	}
	if payload, _, ok := st.GetCache(growthCacheTable, growthKeyReportDay, 0); !ok || payload != day {
		t.Errorf("批量导入不应清 report_day，得到 ok=%v payload=%q", ok, payload)
	}
}

// TestAccountExportFileNameSafe 导出文件名只含安全字符：current 范围用 uid 拼文件名，
// 必须过滤，否则 Content-Disposition 头可能被注入换行（头注入）。
func TestAccountExportFileNameSafe(t *testing.T) {
	h, _, _ := newGrowthTestHandler(t, "cn", true, &fakeGrowthAPI{})
	// uid 含引号、换行与中文：真实 uid 来自 JWT sub，攻击者可控
	if err := h.toks.Save(&auth.Token{UID: "uid-\"x\"\r\n中文", AccessToken: "tok"}); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.accountExport(w, httptest.NewRequest(http.MethodGet, "/admin/account/export?scope=current", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d: %s", w.Code, w.Body.String())
	}
	cd := w.Header().Get("Content-Disposition")
	if strings.ContainsAny(cd, "\r\n") {
		t.Errorf("Content-Disposition 含换行（头注入）: %q", cd)
	}
	m := regexp.MustCompile(`filename="([^"]+)"`).FindStringSubmatch(cd)
	if m == nil {
		t.Fatalf("Content-Disposition 缺少文件名: %q", cd)
	}
	name := m[1]
	if !strings.HasPrefix(name, "buddy2api-account-") || !strings.HasSuffix(name, ".json") {
		t.Errorf("文件名形状异常: %q", name)
	}
	for _, r := range name {
		safe := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.'
		if !safe {
			t.Errorf("文件名含不安全字符 %q: %q", r, name)
		}
	}
}

// TestAccountExportHeaderInjectionMissing 不存在的 uid 会进 X-Export-Missing 头：
// uid 来自 JWT sub（攻击者可控），CR/LF 必须被剔除，否则构成响应头注入。
func TestAccountExportHeaderInjectionMissing(t *testing.T) {
	h, _, _ := newGrowthTestHandler(t, "cn", true, &fakeGrowthAPI{})
	book := auth.NewAccountBook(h.toks.DataDir())
	if err := book.Save(&auth.Token{UID: "uid-b", AccessToken: "tok-b"}); err != nil {
		t.Fatal(err)
	}
	// 手动拼 query：uids 里放 CRLF + 非 ASCII（不清洗就会剖断头部）
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/account/export", nil)
	req.URL.RawQuery = "scope=selected&uids=uid-b," + url.QueryEscape("x\r\nX-Evil: 1") + "," + url.QueryEscape("中文")
	h.accountExport(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d: %s", w.Code, w.Body.String())
	}
	missing := w.Header().Get("X-Export-Missing")
	if _, ok := w.Header()["X-Evil"]; ok {
		t.Errorf("响应头被注入: %v", w.Header())
	}
	for _, r := range missing {
		if r < 0x20 || r >= 0x7f {
			t.Errorf("X-Export-Missing 含非可打印字符 %q: %q", r, missing)
		}
	}
	if strings.Contains(missing, "\n") || strings.Contains(missing, "\r") {
		t.Errorf("X-Export-Missing 含换行: %q", missing)
	}
}

// TestHeaderSafe 头部值清洗：控制字符与非 ASCII 一律化为 '?',长值截断。
func TestHeaderSafe(t *testing.T) {
	if got := headerSafe("uid-b,uid-c"); got != "uid-b,uid-c" {
		t.Errorf("普通值不应被改写，得到 %q", got)
	}
	if got := headerSafe("a\r\nb"); strings.ContainsAny(got, "\r\n") {
		t.Errorf("CR/LF 未剔除: %q", got)
	}
	if got := headerSafe("中文"); strings.ContainsAny(got, "中文") {
		t.Errorf("非 ASCII 未过滤: %q", got)
	}
	if got := headerSafe(strings.Repeat("x", 1000)); len(got) > 512 {
		t.Errorf("未截断（len=%d）", len(got))
	}
}

// TestSafeFileToken 文件片段归一：非 ASCII 字母数字一律化为下划线，超长截断。
func TestSafeFileToken(t *testing.T) {
	if got := safeFileToken("abc-123_X"); got != "abc-123_X" {
		t.Errorf("安全字符不应被改写，得到 %q", got)
	}
	if got := safeFileToken(""); got != "unknown" {
		t.Errorf("空串应回退 unknown，得到 %q", got)
	}
	if got := safeFileToken("中文"); strings.ContainsAny(got, "中文\"") {
		t.Errorf("safeFileToken 未过滤非 ASCII: %q", got)
	}
	if got := safeFileToken("a/b\\c\"d"); strings.ContainsAny(got, `/\`) || strings.Contains(got, `"`) {
		t.Errorf("safeFileToken 未过滤路径分隔符/引号: %q", got)
	}
	if got := safeFileToken(strings.Repeat("x", 100)); len(got) > 32 {
		t.Errorf("safeFileToken 未截断（len=%d）", len(got))
	}
}

// TestAccountImportBatchTooLarge 超限请求必须给出「包过大」而非 JSON 语法错误。
//
// 回归：原先直接 io.ReadAll(io.LimitReader(body, 1<<20))，超限请求被静默截断成半截 JSON，
// 用户看到的是「不是合法 JSON: unexpected end of JSON input」，完全指不出真正原因。
// 而导出侧不限条数（一条真实凭证缩进 JSON 后约 5KB，1MB 只够 ~200 个账号），
// 所以「导得出、导不回」是必然会撞上的组合。
func TestAccountImportBatchTooLarge(t *testing.T) {
	h, _, _ := newGrowthTestHandler(t, "cn", true, &fakeGrowthAPI{})
	body := strings.Repeat("x", bundleImportMaxBytes+100)
	w := httptest.NewRecorder()
	h.accountImportBatch(w, httptest.NewRequest(http.MethodPost, "/admin/account/import/batch", strings.NewReader(body)))
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超限应 413，得到 %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "过大") || strings.Contains(w.Body.String(), "不是合法 JSON") {
		t.Errorf("错误信息应指明「包过大」，不能报成 JSON 语法错误: %s", w.Body.String())
	}
	// 声明了 Content-Length 且超限的走前置分支（不读 body）
	w2 := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/account/import/batch", strings.NewReader("{}"))
	req.ContentLength = bundleImportMaxBytes + 1
	h.accountImportBatch(w2, req)
	if w2.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("按 Content-Length 超限也应 413，得到 %d", w2.Code)
	}
}

// TestAccountImportBatchAcceptsMultipleMB 上限放宽后，一个数 MB 的合法包必须能导入：
// 这是「导出能产出、导入装不下」那条 bug 的正面用例。
func TestAccountImportBatchAcceptsMultipleMB(t *testing.T) {
	h, _, _ := newGrowthTestHandler(t, "cn", true, &fakeGrowthAPI{})
	// 用一个约 2MB 的合法包（超出旧 1MB 上限）
	pad := strings.Repeat("p", 2<<20)
	b, err := json.Marshal(auth.Bundle{Format: auth.BundleFormat, Version: 1, Accounts: []auth.Token{
		{AccessToken: testJWT(t, "uid-big", "大包"), RefreshToken: pad},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(b) <= 1<<20 {
		t.Fatalf("测试前提不成立：包只有 %d 字节", len(b))
	}
	w := httptest.NewRecorder()
	h.accountImportBatch(w, httptest.NewRequest(http.MethodPost, "/admin/account/import/batch", strings.NewReader(string(b))))
	if w.Code != http.StatusOK {
		t.Fatalf("2MB 合法包应能导入（旧 1MB 上限会拒绝），得到 %d: %s", w.Code, w.Body.String())
	}
}

// TestAccountImportBatchFlagsDuplicateUID 同一包内重复 uid 不得静默覆盖并重复计数。
//
// 回归：Save 按 uid 覆盖同名档案，而循环原先无条件 imported++，于是「同一账号写了两遍」
// 会报成「成功导入 2 个」，两个条目都显示「已导入」—— 用户以为导入了两个不同账号。
func TestAccountImportBatchFlagsDuplicateUID(t *testing.T) {
	h, _, _ := newGrowthTestHandler(t, "cn", true, &fakeGrowthAPI{})
	b, err := json.Marshal(auth.Bundle{Format: auth.BundleFormat, Version: 1, Accounts: []auth.Token{
		{AccessToken: testJWT(t, "uid-same", "第一次"), RefreshToken: "OLD"},
		{AccessToken: testJWT(t, "uid-same", "第二次"), RefreshToken: "NEW"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.accountImportBatch(w, httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(string(b))))
	var resp struct {
		Imported int `json:"imported"`
		Failed   int `json:"failed"`
		Items    []struct {
			UID   string `json:"uid"`
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		} `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Imported != 1 || resp.Failed != 1 {
		t.Fatalf("重复 uid 应只计 1 个成功 + 1 个跳过，得到 imported=%d failed=%d（%s）",
			resp.Imported, resp.Failed, w.Body.String())
	}
	if resp.Items[1].OK || !strings.Contains(resp.Items[1].Error, "同一账号") {
		t.Errorf("重复条目应标为跳过并说明原因: %+v", resp.Items[1])
	}
}

// TestAccountImportBatchFlagsOverwrite 覆盖已存在档案时标 overwrote，
// 让用户知道本地较新的凭证被包里的数据替换了（回复旧备份是最常见的触发场景）。
func TestAccountImportBatchFlagsOverwrite(t *testing.T) {
	h, _, _ := newGrowthTestHandler(t, "cn", true, &fakeGrowthAPI{})
	book := auth.NewAccountBook(h.toks.DataDir())
	freshEnd := time.Now().Add(48 * time.Hour).Unix()
	if err := book.Save(&auth.Token{UID: "uid-x", AccessToken: "FRESH", ExpiresAt: freshEnd}); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(auth.Bundle{Format: auth.BundleFormat, Version: 1, Accounts: []auth.Token{
		{AccessToken: testJWT(t, "uid-x", "X")},   // 已存在 → overwrote
		{AccessToken: testJWT(t, "uid-new", "N")}, // 新增 → 不标
	}})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.accountImportBatch(w, httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(string(b))))
	var resp struct {
		Overwrote int `json:"overwrote"`
		Items     []struct {
			UID       string `json:"uid"`
			Overwrote bool   `json:"overwrote"`
		} `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Overwrote != 1 {
		t.Errorf("overwrote 汇总 = %d，期望 1（%s）", resp.Overwrote, w.Body.String())
	}
	if !resp.Items[0].Overwrote {
		t.Errorf("已存在账号应标 overwrote=true: %+v", resp.Items[0])
	}
	if resp.Items[1].Overwrote {
		t.Errorf("新增账号不应标 overwrote: %+v", resp.Items[1])
	}
}
