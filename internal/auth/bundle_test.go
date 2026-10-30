package auth

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// bundleJWT 构造结构合法的假 JWT（ParseJWT 不验签）。
func bundleJWT(t *testing.T, uid, nickname, domain string, exp int64) string {
	t.Helper()
	iss := "https://" + domain + "/auth/realms/sso-ent-1"
	payload, err := json.Marshal(map[string]any{
		"sub": uid, "iss": iss, "nickname": nickname, "exp": exp,
	})
	if err != nil {
		t.Fatal(err)
	}
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

// TestBundleRoundTrip 导出包 → 解析包，凭证字段与格式声明须完整往返。
//
// 包格式是导入导出的唯一契约：字段改名或漏写 version 会让导入侧静默读错。
func TestBundleRoundTrip(t *testing.T) {
	exp := time.Now().Add(time.Hour).Unix()
	tok := bundleJWT(t, "uid-a", "账号A", "copilot.tencent.com", exp)
	b := NewBundle(1700000000, "uid-a", []*Token{
		{AccessToken: tok, RefreshToken: "rt-a", UID: "uid-a", Nickname: "账号A", Domain: "copilot.tencent.com", ExpiresAt: exp},
		{AccessToken: bundleJWT(t, "uid-b", "账号B", "copilot.tencent.com", exp), UID: "uid-b"},
	})
	if b.Format != BundleFormat || b.Version != BundleVersion {
		t.Fatalf("格式声明错误: %+v", b)
	}

	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	got, err := UnmarshalBundle(raw)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got.ExportedAt != 1700000000 || got.ActiveUID != "uid-a" {
		t.Errorf("元信息丢失: %+v", got)
	}
	if len(got.Accounts) != 2 {
		t.Fatalf("账号数 = %d，期望 2", len(got.Accounts))
	}
	if got.Accounts[0].RefreshToken != "rt-a" || got.Accounts[0].UID != "uid-a" {
		t.Errorf("凭证字段丢失: %+v", got.Accounts[0])
	}
}

// TestNewBundleCopiesTokens 打包不得保留调用方指针：活动 token.json 随时可能被刷新改写。
func TestNewBundleCopiesTokens(t *testing.T) {
	live := &Token{UID: "uid-a", AccessToken: "old"}
	b := NewBundle(time.Now().Unix(), "uid-a", []*Token{live})
	live.AccessToken = "refreshed" // 模拟打包后发生的刷新
	if b.Accounts[0].AccessToken != "old" {
		t.Errorf("包内容随活动凭证一起变了（应拷贝快照）: %q", b.Accounts[0].AccessToken)
	}
}

// TestUnmarshalBundleEnrichesFromJWT 包里只给 access_token 时，uid/nickname/domain/expires_at 须从 JWT 补齐：
// 档案库拒绝无 uid 的凭证，不补齐就等于「导入成功但列表里没有」。
func TestUnmarshalBundleEnrichesFromJWT(t *testing.T) {
	exp := time.Now().Add(2 * time.Hour).Unix()
	raw := []byte(`{"format":"buddy2api.accounts","version":1,"accounts":[{"access_token":"` +
		bundleJWT(t, "uid-z", "昵称Z", "www.codebuddy.ai", exp) + `"}]}`)
	b, err := UnmarshalBundle(raw)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	a := b.Accounts[0]
	if a.UID != "uid-z" || a.Nickname != "昵称Z" || a.Domain != "www.codebuddy.ai" {
		t.Errorf("未从 JWT 补齐字段: %+v", a)
	}
	if a.EnterpriseID != "ent-1" {
		t.Errorf("未从 JWT 补齐 enterprise_id: %+v", a)
	}
	if a.ExpiresAt != exp {
		t.Errorf("expires_at = %d，期望 %d", a.ExpiresAt, exp)
	}
}

// TestUnmarshalBundleRejects 非法输入的拒绝口径。
//
// 导入侧必须凭 format 拒绝任意 JSON：把别人的 config.json 当凭证包导入，只会往档案库里
// 塞一堆空凭证；版本上限则防止旧程序读新格式而静默读错。
func TestUnmarshalBundleRejects(t *testing.T) {
	cases := []struct{ name, body, wantErr string }{
		{"非 JSON", `not json`, "不是合法 JSON"},
		{"旧裸 token.json", `{"access_token":"x","refresh_token":"y"}`, "不是凭证包"},
		{"format 不匹配", `{"format":"other","version":1,"accounts":[{"access_token":"x"}]}`, "不是凭证包"},
		{"版本过高", `{"format":"buddy2api.accounts","version":99,"accounts":[{"access_token":"x"}]}`, "版本"},
		{"版本缺失", `{"format":"buddy2api.accounts","accounts":[{"access_token":"x"}]}`, "版本"},
		{"空账号列表", `{"format":"buddy2api.accounts","version":1,"accounts":[]}`, "没有账号"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := UnmarshalBundle([]byte(tc.body))
			if err == nil {
				t.Fatalf("应报错")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("错误信息 %q 未包含 %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestUnmarshalBundleKeepsBadTokenForLater 结构校验不做 JWT 校验：
// 批量导入要能逐条报错（一条坏不应拒绝整包），故坏凭证必须原样留在包里交给 admin 逐条判定。
func TestUnmarshalBundleKeepsBadTokenForLater(t *testing.T) {
	raw := []byte(`{"format":"buddy2api.accounts","version":1,"accounts":[{"access_token":"not-a-jwt"}]}`)
	b, err := UnmarshalBundle(raw)
	if err != nil {
		t.Fatalf("结构校验不应因单条凭证非法而失败: %v", err)
	}
	if len(b.Accounts) != 1 || b.Accounts[0].AccessToken != "not-a-jwt" {
		t.Errorf("坏凭证应原样保留交给逐条判定: %+v", b.Accounts)
	}
}
