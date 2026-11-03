package pool

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"buddy2api-go/internal/store"
)

// TestSnapshotCreditsPerAccount 面板必须给**每个**号显示自己的额度。
//
// 回归：admin 注入的 creditsOf 只能回答「当前活动账号」的余额（缓存键是单账号设计的
// 固定字串），其余号一律得 0，于是号池面板上 N 个号里只有当前这个显额度、其它全是 —。
// 修法：探测时把读到的额度存进 AccountState（并落库），Snapshot 按时间戳取新。
func TestSnapshotCreditsPerAccount(t *testing.T) {
	m, srv, _ := newProbeFixture(t, "acc1", "acc2")
	withBilling(t, srv, func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"code":0,"data":{"Accounts":[{"CapacityRemainPrecise":"300"}]}}`))
	})
	// 两个号各自探测，得到各自的额度
	if res := m.Probe(context.Background(), "acc1"); !res.OK {
		t.Fatalf("acc1 探测失败: %+v", res)
	}
	if res := m.Probe(context.Background(), "acc2"); !res.OK {
		t.Fatalf("acc2 探测失败: %+v", res)
	}

	// creditsOf 只认得「当前活动账号」（这里当它一个都不认，模拟全为非活动号）
	empty := func(string) (float64, int64) { return 0, 0 }
	view := m.Snapshot(empty)
	if len(view.Accounts) != 2 {
		t.Fatalf("应返回 2 个账号，实际 %d", len(view.Accounts))
	}
	for _, a := range view.Accounts {
		if a.Credits <= 0 {
			t.Errorf("%s 的额度应来自自己的探测结果，实际 %v（早期这里只有当前活动账号有值）",
				a.UID, a.Credits)
		}
		if a.CreditsAt <= 0 {
			t.Errorf("%s 的 credits_at 应被填上（前端靠它区分「未知」与「额度 0」），实际 %d",
				a.UID, a.CreditsAt)
		}
	}
}

// TestSnapshotCreditsPrefersNewest 探测值与注入值按时间戳取新，谁也不该无条件覆盖对方。
//
// 两个方向都覆盖：
//   - 注入值更新（刚刷过余额页）→ 用注入值；
//   - 探测值更新（几小时前手动探过、之后余额缓存已过期重建得更早）→ 用探测值。
func TestSnapshotCreditsPrefersNewest(t *testing.T) {
	m, _, _ := newProbeFixture(t, "acc1")
	m.mu.Lock()
	m.accounts["acc1"].Credits = 100
	m.accounts["acc1"].CreditsAt = 1000
	m.mu.Unlock()

	// 注入值更新 → 用它
	view := m.Snapshot(func(string) (float64, int64) { return 900, 2000 })
	if view.Accounts[0].Credits != 900 {
		t.Errorf("注入值更新时应用注入值，得到 %v", view.Accounts[0].Credits)
	}

	// 探测值更新 → 仍用探测值
	view = m.Snapshot(func(string) (float64, int64) { return 900, 500 })
	if view.Accounts[0].Credits != 100 {
		t.Errorf("探测值更新时应保留探测值，得到 %v（旧值覆盖了更新的额度）", view.Accounts[0].Credits)
	}

	// 注入值为「不知道」（at=0）时不得把探测值清掉
	view = m.Snapshot(func(string) (float64, int64) { return 0, 0 })
	if view.Accounts[0].Credits != 100 || view.Accounts[0].CreditsAt != 1000 {
		t.Errorf("未知的注入值不应清掉已知额度，得到 %v/%d",
			view.Accounts[0].Credits, view.Accounts[0].CreditsAt)
	}
}

// TestSnapshotWithoutProbeCreditsUnknown 从未探测过的号：额度「未知」而不是「0」。
// 前端靠 credits_at == 0 显示 —，若这里填 0/0 就会被误解成「额度已用尽」。
func TestSnapshotWithoutProbeCreditsUnknown(t *testing.T) {
	m, _, _ := newProbeFixture(t, "acc1")
	view := m.Snapshot(func(string) (float64, int64) { return 0, 0 })
	if view.Accounts[0].CreditsAt != 0 {
		t.Errorf("未探测过的号 credits_at 应为 0（未知），得到 %d", view.Accounts[0].CreditsAt)
	}
}

// TestProbeCreditsPersistAcrossReload 额度必须落库：进程重启后（Load 重扫）
// 面板不该又变回一排 —。
func TestProbeCreditsPersistAcrossReload(t *testing.T) {
	m, srv, _ := newProbeFixture(t, "acc1")
	withBilling(t, srv, func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"code":0,"data":{"Accounts":[{"CapacityRemainPrecise":"777"}]}}`))
	})
	if res := m.Probe(context.Background(), "acc1"); !res.OK {
		t.Fatalf("探测失败: %+v", res)
	}
	// 落库是异步的：等 worker 把这条写完（最多 ~2s，稳态是毫秒级）
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		rows, err := m.st.LoadPoolAccounts()
		if err != nil {
			t.Fatalf("读池状态失败: %v", err)
		}
		for _, r := range rows {
			if r.UID == "acc1" && r.Credits > 0 {
				goto reload
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("探测得到的额度没有落库（重启后又会变回一排 —）")

reload:
	m.Load()
	st, ok := m.AccountStateOf("acc1")
	if !ok {
		t.Fatal("重载后账号丢失")
	}
	if st.Credits != 777 {
		t.Errorf("重载后额度应保持 777，实际 %v", st.Credits)
	}
	if st.CreditsAt <= 0 {
		t.Errorf("重载后 credits_at 应 > 0，实际 %d", st.CreditsAt)
	}
}

// TestProbeNetworkErrorKeepsCredits 网络故障（读到 0 个字节）绝不能把已知额度清成 0。
// 否则一次断网就会让面板把好号显示成「额度 0」。
func TestProbeNetworkErrorKeepsCredits(t *testing.T) {
	m, srv, _ := newProbeFixture(t, "acc1")
	withBilling(t, srv, func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"code":0,"data":{"Accounts":[{"CapacityRemainPrecise":"500"}]}}`))
	})
	if res := m.Probe(context.Background(), "acc1"); !res.OK {
		t.Fatalf("首次探测失败: %+v", res)
	}

	withBilling(t, srv, func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"msg":"boom"}`))
	})
	if res := m.Probe(context.Background(), "acc1"); res.OK {
		t.Fatal("500 不应判定为可用")
	}
	st, _ := m.AccountStateOf("acc1")
	if st.Credits != 500 {
		t.Errorf("网络故障不应清掉已知额度，实际 %v", st.Credits)
	}
	if st.CreditsAt <= 0 {
		t.Errorf("网络故障不应把 credits_at 清零（否则 UI 会当从未探测过），实际 %d", st.CreditsAt)
	}
}

// TestSnapshotViewJSONShape 面板契约：credits / credits_at 必须出现在 JSON 里
// （前端 x-text 直接读这两个字段，少一个就整列空白）。
func TestSnapshotViewJSONShape(t *testing.T) {
	m, _, _ := newProbeFixture(t, "acc1")
	m.mu.Lock()
	m.accounts["acc1"].Credits = 12.5
	m.accounts["acc1"].CreditsAt = 4567
	m.mu.Unlock()
	view := m.Snapshot(nil)
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Accounts []map[string]any `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Accounts) != 1 {
		t.Fatalf("应序列化 1 个账号，实际 %d", len(parsed.Accounts))
	}
	if parsed.Accounts[0]["credits"] != 12.5 || parsed.Accounts[0]["credits_at"] != float64(4567) {
		t.Errorf("credits/credits_at 未出现在响应里: %s", string(raw))
	}
}

// TestBackfillCreditsFromProbeMsg 老库升级：credits/credits_at 是后加的列（默认 0），
// 但旧的 last_probe_msg 里其实早就写过 "credit=5015.37"。
// 不回填的话，升级后用户得等下一轮探测（默认 6 小时）才能看到那些号的额度，
// 期间面板一直显示 —。
func TestBackfillCreditsFromProbeMsg(t *testing.T) {
	cases := []struct {
		name      string
		st        AccountState
		wantCred  float64
		wantKnown bool
	}{
		{
			name:      "从消息回填",
			st:        AccountState{LastProbeAt: 1000, LastProbeOK: true, LastProbeMsg: "credit=5015.37"},
			wantCred:  5015.37,
			wantKnown: true,
		},
		{
			name:      "整数形态",
			st:        AccountState{LastProbeAt: 1000, LastProbeOK: true, LastProbeMsg: "credit=0"},
			wantCred:  0,
			wantKnown: true, // 探测确认耗尽 → 已知 0，不是「未知」
		},
		{
			name:      "已有记录时不覆盖",
			st:        AccountState{LastProbeAt: 2000, LastProbeMsg: "credit=999", Credits: 12, CreditsAt: 1500},
			wantCred:  12,
			wantKnown: true,
		},
		{
			name:      "非 credit= 前缀（网络故障消息）不动",
			st:        AccountState{LastProbeAt: 1000, LastProbeMsg: "probe failed: boom"},
			wantCred:  0,
			wantKnown: false,
		},
		{
			name:      "从未探测过",
			st:        AccountState{},
			wantCred:  0,
			wantKnown: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := tc.st
			backfillCreditsFromProbeMsg(&st)
			if st.Credits != tc.wantCred {
				t.Errorf("credits = %v，期望 %v", st.Credits, tc.wantCred)
			}
			if (st.CreditsAt > 0) != tc.wantKnown {
				t.Errorf("credits_at = %d，期望已知=%v", st.CreditsAt, tc.wantKnown)
			}
		})
	}
}

// TestLoadBackfillsCreditsFromOldRow 端到端：老库里只写了 last_probe_msg，
// Load 之后面板就能读到额度（不必等下一轮探测）。
func TestLoadBackfillsCreditsFromOldRow(t *testing.T) {
	m, st, _ := newTestManager(t, "acc1")
	// 模拟旧版本落下的行：credits/credits_at 都是 0，只有消息里带额度
	if err := st.SavePoolAccount(&store.PoolAccountRow{
		UID: "acc1", LastProbeAt: 5000, LastProbeOK: true, LastProbeMsg: "credit=1234.56",
	}); err != nil {
		t.Fatal(err)
	}
	m.Load()
	got, ok := m.AccountStateOf("acc1")
	if !ok {
		t.Fatal("账号丢失")
	}
	if got.Credits != 1234.56 || got.CreditsAt != 5000 {
		t.Errorf("应从 last_probe_msg 回填额度 1234.56@5000，实际 %v@%d", got.Credits, got.CreditsAt)
	}
}
