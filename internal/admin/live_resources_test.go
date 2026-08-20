package admin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"buddy2api-go/internal/auth"
	"buddy2api-go/internal/upstream"
)

// TestLiveResources 用真实令牌打上游 get-user-resource，对照「原始响应」与「processResources 加工结果」。
//
// 默认跳过，需显式开启：BUDDY2API_LIVE=1 go test -run TestLiveResources -v ./internal/admin/
// 令牌取自项目根 data/token.json（与运行中的服务同源）。
func TestLiveResources(t *testing.T) {
	if os.Getenv("BUDDY2API_LIVE") != "1" {
		t.Skip("跳过实时测试（设置 BUDDY2API_LIVE=1 启用）")
	}
	root, _ := filepath.Abs("../..")
	toks, err := auth.NewTokenStore(filepath.Join(root, "data"))
	if err != nil {
		t.Fatal("加载 token 失败:", err)
	}
	if toks.Get() == nil {
		t.Skip("data/token.json 不存在或未登录")
	}
	t.Logf("账号 domain=%s uid=%s expires=%s", toks.Get().Domain, toks.Get().UID, toks.Get().ExpiresInHuman())

	client := upstream.New(toks, func() string { return "cn" }, 60)
	res, err := client.Resources()
	if err != nil {
		t.Fatal("上游 Resources 失败:", err)
	}

	// 1) 原始上游响应（美化）
	t.Log("========== 原始上游响应 ==========")
	var pretty any
	if err := json.Unmarshal(res.Raw, &pretty); err == nil {
		out, _ := json.MarshalIndent(pretty, "", "  ")
		t.Log(string(out))
	} else {
		t.Log(string(res.Raw))
	}

	// 2) processResources 加工结果
	t.Log("========== processResources 加工结果 ==========")
	processed := processResources(res.Raw)
	out, _ := json.MarshalIndent(processed, "", "  ")
	t.Log(string(out))

	// 3) 逐包对照：上游原始字段 vs 加工后字段
	t.Log("========== 字段对照 ==========")
	if accs, ok := processed["accounts"].([]resourceAccount); ok {
		for i, a := range accs {
			t.Logf("包%d %s: used=%v remain=%v size=%v | cycle: remain=%v used=%v size=%v | expire=%q days_left=%v",
				i+1, a.PackageName, a.CapacityUsed, a.CapacityRemain, a.CapacitySize,
				a.CycleRemain, a.CycleUsed, a.CycleSize, a.ExpireTime, daysLeftStr(a.DaysLeft))
		}
	} else {
		t.Log("accounts 解析失败")
	}
}
