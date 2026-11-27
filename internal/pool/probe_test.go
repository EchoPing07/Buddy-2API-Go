package pool

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"buddy2api-go/internal/auth"
	"buddy2api-go/internal/upstream"
)

// newProbeFixture 造一个带假 billing 上游的号池。
//
// billingHandler 决定 /v2/billing/meter/get-user-resource 的响应，
// 用于覆盖「有额度 / 无额度 / 401 / 网络故障」四条探测分支。
func newProbeFixture(t *testing.T, uids ...string) (*Manager, *httptest.Server, *int) {
	t.Helper()
	// newTestManager 传的 client 是 nil（多数用例用不到上游），探测需要真实 client，
	// 故这里自建一份并注入。
	m, _, _ := newTestManager(t, uids...)
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"code":0,"data":{"Accounts":[]}}`))
	}))
	t.Cleanup(srv.Close)

	ts, err := m.TokenStore("acc1")
	if err != nil && len(uids) > 0 {
		ts, err = m.TokenStore(uids[0])
	}
	if err != nil {
		t.Fatalf("取 TokenStore 失败: %v", err)
	}
	client := upstream.New(ts, func() string { return "cn" }, 30)
	client.AuthBaseOverride = srv.URL
	client.BillingBaseOverride = srv.URL
	m.client = client
	return m, srv, &hits
}

// withBilling 把假上游换成指定行为的 handler。
func withBilling(t *testing.T, srv *httptest.Server, fn func(w http.ResponseWriter)) {
	t.Helper()
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fn(w) })
}

// TestProbeRecoversWithRealPayloadShape 有额度且**用真实字段形态**（字符串额度）→ 恢复。
//
// 这条是 P0 级回归：早期 SumAvailable 按数字解析，字符串额度恒为 0，
// 探测会把每个健康账号打成「额度用尽」。
func TestProbeRecoversWithRealPayloadShape(t *testing.T) {
	m, srv, _ := newProbeFixture(t, "acc1")
	// 先打成禁用
	m.mu.Lock()
	m.accounts["acc1"].AutoDisabled = AutoQuota
	m.mu.Unlock()

	payload, _ := json.Marshal(map[string]any{
		"code": 0,
		"data": map[string]any{
			"Accounts": []any{map[string]any{
				// 上游真实形态：字符串额度 + 毫秒到期时间
				"CapacityRemainPrecise":      "0",
				"CycleCapacityRemainPrecise": "499.06",
				"DeductionEndTime":           time.Now().Add(24 * time.Hour).UnixMilli(),
			}},
		},
	})
	withBilling(t, srv, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(payload)
	})

	res := m.Probe(context.Background(), "acc1")
	if !res.OK {
		t.Fatalf("应探测到额度并恢复，实际 ok=%v msg=%s", res.OK, res.Msg)
	}
	if res.Credits < 400 {
		t.Errorf("额度 = %v，期望约 499.06（字符串形态必须能解析）", res.Credits)
	}
	st, _ := m.AccountStateOf("acc1")
	if st.AutoDisabled != "" {
		t.Errorf("探测成功应清空自动禁用，实际 %q", st.AutoDisabled)
	}
}

// TestProbeKeepsQuotaWhenZero 有响应但额度为 0 → 保持/升级为 quota 禁用。
func TestProbeKeepsQuotaWhenZero(t *testing.T) {
	m, srv, _ := newProbeFixture(t, "acc1")
	withBilling(t, srv, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"code":0,"data":{"Accounts":[{"CapacityRemainPrecise":"0","CycleCapacityRemainPrecise":"0"}]}}`))
	})
	res := m.Probe(context.Background(), "acc1")
	if res.OK {
		t.Error("额度为 0 不应判定为可用")
	}
	st, _ := m.AccountStateOf("acc1")
	if st.AutoDisabled != AutoQuota {
		t.Errorf("应标为 quota，实际 %q", st.AutoDisabled)
	}
}

// TestProbeDoesNotTouchManualDisabled 手动禁用的号不探测、也不被自动恢复。
func TestProbeDoesNotTouchManualDisabled(t *testing.T) {
	m, _, hits := newProbeFixture(t, "acc1")
	if err := m.SetManualDisabled("acc1", true); err != nil {
		t.Fatal(err)
	}
	before := *hits
	res := m.Probe(context.Background(), "acc1")
	if !res.Skipped {
		t.Error("手动禁用应跳过探测")
	}
	if *hits != before {
		t.Errorf("手动禁用不应打上游，实际多了 %d 次", *hits-before)
	}
	st, _ := m.AccountStateOf("acc1")
	if !st.ManualDisabled {
		t.Error("ManualDisabled 不应被改写")
	}
}

// TestProbeNetworkErrorKeepsState 网络/上游故障**不改任何状态**。
//
// 这是最关键的一条：把网络抖动当成「号还是坏的」会让一次断网把好号永久禁掉。
func TestProbeNetworkErrorKeepsState(t *testing.T) {
	m, srv, _ := newProbeFixture(t, "acc1")
	// 先手动置成 quota（模拟之前被判过）
	m.mu.Lock()
	m.accounts["acc1"].AutoDisabled = AutoQuota
	m.mu.Unlock()

	// 500 不是凭证错误 → 属网络/上游故障
	withBilling(t, srv, func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"msg":"boom"}`))
	})
	res := m.Probe(context.Background(), "acc1")
	if res.OK {
		t.Error("500 不应判定为可用")
	}
	st, _ := m.AccountStateOf("acc1")
	// 状态保持原样（仍是 quota），不能被改成别的、也不能被清空
	if st.AutoDisabled != AutoQuota {
		t.Errorf("网络错误不应改变自动禁用原因，实际 %q", st.AutoDisabled)
	}
	if res.Reason != AutoQuota {
		t.Errorf("结果的 reason 应反映当前状态，实际 %q", res.Reason)
	}
}

// TestProbeCredentialMarksAccount 401 且刷不动 → 标 credential。
func TestProbeCredentialMarksAccount(t *testing.T) {
	m, srv, _ := newProbeFixture(t, "acc1")
	withBilling(t, srv, func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"code":401,"msg":"unauthorized"}`))
	})
	// token 无 refresh_token → ForceRefresh 必然失败，故走「刷不动」分支
	res := m.Probe(context.Background(), "acc1")
	if res.OK {
		t.Error("401 不应判定为可用")
	}
	st, _ := m.AccountStateOf("acc1")
	if st.AutoDisabled != AutoCredential {
		t.Errorf("401 刷不动应标为 credential，实际 %q", st.AutoDisabled)
	}
	if res.Reason != AutoCredential {
		t.Errorf("reason = %q，期望 %q", res.Reason, AutoCredential)
	}
}

// TestProbeUnknownUID 不存在的账号不 panic。
func TestProbeUnknownUID(t *testing.T) {
	m, _, _ := newProbeFixture(t, "acc1")
	res := m.Probe(context.Background(), "nope")
	if res.OK || res.Msg == "" {
		t.Errorf("未知账号应返回不可用且有说明，实际 %+v", res)
	}
}

// TestProbeWithLimit 手动探测限频 30 秒。
func TestProbeWithLimit(t *testing.T) {
	m, srv, _ := newProbeFixture(t, "acc1")
	withBilling(t, srv, func(w http.ResponseWriter) {
		w.Write([]byte(`{"code":0,"data":{"Accounts":[{"CapacityRemain":"10"}]}}`))
	})
	if _, err := m.ProbeWithLimit(context.Background(), "acc1"); err != nil {
		t.Fatalf("首次探测应允许: %v", err)
	}
	if _, err := m.ProbeWithLimit(context.Background(), "acc1"); err != ErrProbeTooFrequent {
		t.Errorf("30 秒内第二次应被限频，得到 %v", err)
	}
}

// TestProbeDataRaceNoLeak 探测期间并发改状态不应产生数据竞争。
// 本机 CGO_ENABLED=0 跑不了 -race，这里至少保证不 panic / 不死锁。
func TestProbeDataRaceNoLeak(t *testing.T) {
	m, srv, _ := newProbeFixture(t, "acc1")
	withBilling(t, srv, func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusInternalServerError) // 走「不改状态」分支（原本会读 st.AutoDisabled）
		w.Write([]byte(`{}`))
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			m.Probe(context.Background(), "acc1")
		}
	}()
	for i := 0; i < 200; i++ {
		m.NoteError("acc1", "glm", upstream.ErrKindQuotaExhausted, body14018())
	}
	<-done
}

func TestProbePopulatesExpiryAndCallsResourceHook(t *testing.T) {
	m, srv, _ := newProbeFixture(t, "acc1")
	// 固定到两天后北京时间正午，保证 +2h 的第二个包仍在同一 CST 自然日
	cst := time.FixedZone("UTC+8", 8*3600)
	n := time.Now().In(cst).Add(48 * time.Hour)
	expTs := time.Date(n.Year(), n.Month(), n.Day(), 12, 0, 0, 0, cst)
	payload, _ := json.Marshal(map[string]any{
		"code": 0,
		"data": map[string]any{
			"Accounts": []any{
				map[string]any{
					"CapacityRemainPrecise":      "0",
					"CycleCapacityRemainPrecise": "150.5",
					"DeductionEndTime":           expTs.UnixMilli(),
				},
				map[string]any{
					"CapacityRemainPrecise": "49.5",
					"DeductionEndTime":      expTs.Add(2 * time.Hour).UnixMilli(),
				},
			},
		},
	})
	withBilling(t, srv, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(payload)
	})

	var hookUID string
	var hookRaw []byte
	m.SetResourceHook(func(uid string, raw []byte) {
		hookUID = uid
		hookRaw = raw
	})

	res := m.Probe(context.Background(), "acc1")
	if !res.OK {
		t.Fatalf("探测失败: %+v", res)
	}
	if hookUID != "acc1" || len(hookRaw) == 0 {
		t.Errorf("SetResourceHook 未被正确调用: uid=%q rawLen=%d", hookUID, len(hookRaw))
	}
	st, _ := m.AccountStateOf("acc1")
	if st.EarliestExpireAt != expTs.Unix() {
		t.Errorf("EarliestExpireAt = %d，期望 %d", st.EarliestExpireAt, expTs.Unix())
	}
	if st.ExpiringCredits != 200 {
		t.Errorf("ExpiringCredits = %v，期望 200", st.ExpiringCredits)
	}

	// 持久化并 Reload 后仍保留到期字段
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		rows, err := m.st.LoadPoolAccounts()
		if err == nil && len(rows) > 0 && rows[0].EarliestExpireAt == expTs.Unix() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	m.Load()
	st2, _ := m.AccountStateOf("acc1")
	if st2.EarliestExpireAt != expTs.Unix() || st2.ExpiringCredits != 200 {
		t.Errorf("Load 后到期字段丢失: %+v", st2)
	}
}

func TestProbeUnprobedAsyncAndStaleExpireReprobe(t *testing.T) {
	prevInit, prevJitter := unprobedInitDelay, unprobedPerAcctJitter
	unprobedInitDelay = 0
	unprobedPerAcctJitter = 0
	t.Cleanup(func() {
		unprobedInitDelay = prevInit
		unprobedPerAcctJitter = prevJitter
	})

	m, srv, _ := newProbeFixture(t, "acc1", "acc2", "acc3")
	// acc2 手动禁用，不应被 ProbeUnprobedAsync 探测
	if err := m.SetManualDisabled("acc2", true); err != nil {
		t.Fatal(err)
	}
	// acc3 跨区，不应被 ProbeUnprobedAsync 探测
	ts3, _ := m.TokenStore("acc3")
	_ = ts3.Save(&auth.Token{
		UID: "acc3", AccessToken: "at-3", Domain: "www.codebuddy.ai",
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})

	var calls atomic.Int32
	withBilling(t, srv, func(w http.ResponseWriter) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"code":0,"data":{"Accounts":[{"CapacityRemainPrecise":"88"}]}}`))
	})

	m.ProbeUnprobedAsync()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		st, _ := m.AccountStateOf("acc1")
		if st.CreditsAt > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	st1, _ := m.AccountStateOf("acc1")
	st2, _ := m.AccountStateOf("acc2")
	st3, _ := m.AccountStateOf("acc3")
	if st1.Credits != 88 || st1.CreditsAt == 0 {
		t.Errorf("acc1 应被异步探测补全额度，实际 %+v", st1)
	}
	if st2.CreditsAt != 0 || st3.CreditsAt != 0 {
		t.Errorf("手动禁用/跨区账号不应被异步探测: acc2=%d acc3=%d", st2.CreditsAt, st3.CreditsAt)
	}

	// 测试 Resolve 遇过期 EarliestExpireAt 触发异步重探（且限频）
	m.asyncProbeAt.Delete("acc1")
	m.mu.Lock()
	m.accounts["acc1"].EarliestExpireAt = time.Now().Add(-time.Hour).Unix()
	m.mu.Unlock()
	before := calls.Load()
	m.Resolve("s-stale", "glm", nil)
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if calls.Load() > before {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if calls.Load() <= before {
		t.Error("expire_first 遇到过期的 EarliestExpireAt 应触发异步重探")
	}
	// 5 分钟限频期内再次 Resolve 不重复探测
	after := calls.Load()
	m.Resolve("s-stale2", "glm", nil)
	time.Sleep(50 * time.Millisecond)
	if calls.Load() != after {
		t.Errorf("异步重探应受限频保护，实际多发了 %d 次", calls.Load()-after)
	}
	// Close 后再调用 ProbeUnprobedAsync 不得 panic
	m.Close()
	m.ProbeUnprobedAsync()
}
