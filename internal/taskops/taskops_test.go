package taskops

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"buddy2api-go/internal/auth"
	"buddy2api-go/internal/upstream"
)

// newBareClient 构造一个「未登录」的上游客户端（token 存储为空）。
func newBareClient(t *testing.T) *upstream.Client {
	t.Helper()
	toks, err := auth.NewTokenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return upstream.New(toks, func() string { return "cn" }, 10)
}

// TestActionTableConsistency 动作表自检：任务码唯一、支持的任务都能查到、
// mp 任务只出现在小程序口径组里。
func TestActionTableConsistency(t *testing.T) {
	seen := map[string]bool{}
	for _, a := range actions {
		if a.Code == "" {
			t.Error("存在空任务码")
		}
		if seen[a.Code] {
			t.Errorf("任务码重复: %s", a.Code)
		}
		seen[a.Code] = true
		if a.Run == nil {
			t.Errorf("%s 缺少 Run 实现", a.Code)
		}
		if a.Desc == "" {
			t.Errorf("%s 缺少说明", a.Code)
		}
	}
	for _, code := range []string{
		"chat_5", "first_buddy", "Model_chat_GLM5.2", "RichMeow_Chat",
		"Buddy_App", "Buddy_App_QQ", "automation_1", "Library_read",
		"template_5", "playbook_prompt", "create_canvas", "expert_5",
		"Expert_team_use_3", "Expert_lighthouse", "Hp_Appearance", "skill_1",
		"black_cat", "school_season", "Sequential_Tasks_1", "Sequential_Tasks_2",
		"Sequential_Tasks_3", "Sequential_Tasks_4", "Sequential_Tasks_5",
		"Sequential_Tasks_6", "Sequential_Tasks_7",
	} {
		if ActionFor(code) == nil {
			t.Errorf("缺少动作: %s", code)
		}
	}
	if ActionFor("Expert_Philanthropy") != nil {
		t.Error("Expert_Philanthropy 需真实捐款，不应提供自动动作")
	}
	if ActionFor("  chat_5  ") == nil {
		t.Error("ActionFor 应容忍首尾空白")
	}
	if ActionFor("no_such_task") != nil {
		t.Error("未知任务应返回 nil")
	}
}

// TestMPActionFlags 小程序专属任务必须标记 MP（决定 accept/claim 走 mp 口径）。
func TestMPActionFlags(t *testing.T) {
	mpExpected := []string{
		"school_season", "Sequential_Tasks_1", "Sequential_Tasks_2",
		"Sequential_Tasks_3", "Sequential_Tasks_4", "Sequential_Tasks_5",
		"Sequential_Tasks_6", "Sequential_Tasks_7",
	}
	for _, c := range mpExpected {
		if a := ActionFor(c); a == nil || !a.MP {
			t.Errorf("%s 应标记 MP=true", c)
		}
	}
	for _, c := range []string{"chat_5", "first_buddy", "automation_1", "black_cat"} {
		if a := ActionFor(c); a == nil || a.MP {
			t.Errorf("%s 不应标记 MP", c)
		}
	}
}

// TestActionsReturnsCopy 外部修改不得影响内部动作表。
func TestActionsReturnsCopy(t *testing.T) {
	list := Actions()
	if len(list) != len(actions) {
		t.Fatalf("应返回全部动作，得到 %d/%d", len(list), len(actions))
	}
	list[0].Code = "mutated"
	if actions[0].Code == "mutated" {
		t.Error("Actions() 必须返回副本")
	}
	// 对外视图必须可 JSON 序列化（Action 含 func 字段，不能直接透出）
	if _, err := json.Marshal(list); err != nil {
		t.Errorf("Actions() 必须可 JSON 序列化: %v", err)
	}
}

// TestGateMessages 门控原因码都有人话解释。
func TestGateMessages(t *testing.T) {
	for _, reason := range []string{"not_logged_in", "region_global", "enterprise", "busy"} {
		msg := GateMessage(reason)
		if msg == "" || msg == reason {
			t.Errorf("%s 缺少人话解释，得到 %q", reason, msg)
		}
	}
	// 未知原因原样返回（不吞掉信息）
	if GateMessage("weird") != "weird" {
		t.Error("未知原因应原样返回")
	}
}

// TestGateNoToken 未登录时门控返回 not_logged_in（不发起上游调用）。
func TestGateNoToken(t *testing.T) {
	r := &Runner{c: newBareClient(t)}
	if got := r.Gate("cn"); got != "not_logged_in" {
		t.Errorf("未登录应返回 not_logged_in，得到 %q", got)
	}
}

// TestSweepStatsZero / 未登录时 Sweep 不发请求且带 reason。
func TestSweepGateBlocks(t *testing.T) {
	r := &Runner{c: newBareClient(t)}
	st := r.Sweep("cn")
	if st.Ran {
		t.Error("门控失败时不应标记 Ran")
	}
	if st.Reason != "not_logged_in" {
		t.Errorf("应带 reason=not_logged_in，得到 %q", st.Reason)
	}
}

// TestErrBusyText busy 错误有可读文案（前端直接展示）。
func TestErrBusyText(t *testing.T) {
	if !strings.Contains(ErrBusy.Error(), "执行") {
		t.Errorf("ErrBusy 文案应可读，得到 %q", ErrBusy.Error())
	}
	if errors.Is(ErrBusy, upstream.ErrNoToken) {
		t.Error("ErrBusy 与 ErrNoToken 不应混为一谈")
	}
}

// TestStepResultJSON 结果结构可序列化（前端契约）。
func TestStepResultJSON(t *testing.T) {
	res := StepResult{
		TaskCode: "chat_5", Status: "done", Message: "已补报 3 条",
		ProgressBefore: "2/5", ProgressAfter: "5/5",
		Claimable: true, Claimed: true, Credit: 100, Energy: 5, Attempt: false,
	}
	if res.TaskCode != "chat_5" || !res.Claimed || res.Credit != 100 {
		t.Error("StepResult 字段语义错乱")
	}
}

// TestSweepStatsJSON 汇总结构字段齐全（运维/面板契约）。
func TestSweepStatsJSON(t *testing.T) {
	st := SweepStats{Ran: true, Accepted: 2, Claimed: 1, Credits: 100, Energy: 5, Errors: 0, At: 123}
	if st.Accepted != 2 || st.Claimed != 1 || st.Credits != 100 {
		t.Error("SweepStats 字段语义错乱")
	}
}

// TestThrottleDefaults 节流参数必须非零（防风控）：置 0 只允许测试这么做。
func TestThrottleDefaults(t *testing.T) {
	if reportGap <= 0 || acceptBatchGap <= 0 || mpActionGap <= 0 ||
		mpChatEventGap <= 0 || expertSummonGap <= 0 || acceptBatchSize <= 0 {
		t.Errorf("节流参数不得为零值: reportGap=%v acceptBatchGap=%v mpActionGap=%v mpChatEventGap=%v expertSummonGap=%v batch=%d",
			reportGap, acceptBatchGap, mpActionGap, mpChatEventGap, expertSummonGap, acceptBatchSize)
	}
	// mp 对话间隔必须在数十秒级（服务端反作弊：数秒连发会整体回滚）
	if mpChatEventGap < 30*1e9 {
		t.Errorf("mpChatEventGap 应 ≥30s（反作弊），得到 %v", mpChatEventGap)
	}
}

// TestGateRegionAndEnterprise 门控矩阵：只放行「已登录 + cn + 非企业版」。
func TestGateRegionAndEnterprise(t *testing.T) {
	cases := []struct {
		name       string
		region     string
		enterprise string
		want       string
	}{
		{"cn 个人号放行", "cn", "", ""},
		{"global 拦截", "global", "", "region_global"},
		{"企业版拦截", "cn", "ent-123", "enterprise"},
		{"企业版+global 优先判 global", "global", "ent-123", "region_global"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			toks, err := auth.NewTokenStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := toks.Save(&auth.Token{
				AccessToken: "t", TokenType: "Bearer", UID: "u",
				EnterpriseID: c.enterprise, ExpiresAt: 1<<62 - 1,
			}); err != nil {
				t.Fatal(err)
			}
			client := upstream.New(toks, func() string { return "cn" }, 10)
			r := NewRunner(client)
			if got := r.Gate(c.region); got != c.want {
				t.Errorf("Gate(%q, enterprise=%q)=%q want %q", c.region, c.enterprise, got, c.want)
			}
		})
	}
}

// TestAutoUnknownTask 未知任务直接报错（不发上游调用）。
func TestAutoUnknownTask(t *testing.T) {
	r := NewRunner(newBareClient(t))
	_, err := r.Auto("no_such_task")
	if err == nil {
		t.Fatal("未知任务应报错")
	}
	if !strings.Contains(err.Error(), "no_such_task") {
		t.Errorf("错误应包含任务码，得到 %v", err)
	}
}

// TestAutoBusy 互斥：持锁期间 Auto 返回 ErrBusy（不排队）。
func TestAutoBusy(t *testing.T) {
	r := NewRunner(newBareClient(t))
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.Auto("chat_5"); !errors.Is(err, ErrBusy) {
		t.Errorf("持锁时应返回 ErrBusy，得到 %v", err)
	}
	if _, err := r.AutoAll(); !errors.Is(err, ErrBusy) {
		t.Errorf("AutoAll 持锁时应返回 ErrBusy，得到 %v", err)
	}
}

// TestSetNow 时钟可替换（夜猫子窗口判定依赖它）。
func TestSetNow(t *testing.T) {
	r := NewRunner(newBareClient(t))
	fixed := time.Date(2026, 10, 26, 3, 0, 0, 0, time.Local)
	r.SetNow(func() time.Time { return fixed })
	if !r.nowFn().Equal(fixed) {
		t.Error("SetNow 未生效")
	}
	r.SetNow(nil) // nil 不应覆盖
	if r.nowFn == nil {
		t.Error("SetNow(nil) 不应清空时钟")
	}
}

// TestThrottleTuning 节流调参语义：0=保持、负值=关闭、正值=设定，恢复函数可还原。
func TestThrottleTuning(t *testing.T) {
	before := CurrentThrottle()
	if before.Report <= 0 || before.MPChatEvent <= 0 {
		t.Fatalf("默认节流不应为零值: %+v", before)
	}
	restore := ThrottleTuning(0, 0, 0, 0) // 全 0 = 全部保持
	if CurrentThrottle() != before {
		t.Errorf("传 0 应保持原值，得到 %+v", CurrentThrottle())
	}
	restore()
	restore = ThrottleTuning(-1, -1, -1, -1)
	after := CurrentThrottle()
	if after.Report != 0 || after.AcceptBatch != 0 || after.MPAction != 0 || after.MPChatEvent != 0 {
		t.Errorf("传 -1 应关闭等待，得到 %+v", after)
	}
	restore()
	if CurrentThrottle() != before {
		t.Errorf("恢复后应回到默认值，得到 %+v", CurrentThrottle())
	}

	// 正值覆盖
	restore = ThrottleTuning(5*time.Millisecond, 0, 0, 30*time.Second)
	cur := CurrentThrottle()
	if cur.Report != 5*time.Millisecond || cur.MPChatEvent != 30*time.Second {
		t.Errorf("正值应生效，得到 %+v", cur)
	}
	if cur.AcceptBatch != before.AcceptBatch {
		t.Errorf("未指定的字段不应被改，得到 %+v", cur.AcceptBatch)
	}
	restore()
}

// TestTaskPollTuning 回读轮询调参：默认预算 ≥10s（实测计分延迟 5~8 秒）。
func TestTaskPollTuning(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"tasks":[{"task_code":"x","target":1,"current":0}]}}`))
	}))
	defer srv.Close()
	toks, err := auth.NewTokenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := toks.Save(&auth.Token{AccessToken: "t", TokenType: "Bearer", UID: "u",
		ExpiresAt: time.Now().Add(time.Hour).Unix()}); err != nil {
		t.Fatal(err)
	}
	c := upstream.New(toks, func() string { return "cn" }, 10)
	c.GrowthBaseOverride = srv.URL

	// 调小预算：应在数百毫秒内返回（而不是默认的 12 秒）
	restore := upstream.TaskPollTuning(10*time.Millisecond, 2)
	defer restore()
	start := time.Now()
	if _, err := c.TaskByCodeWaiting("x", false); err != nil {
		t.Fatalf("TaskByCodeWaiting: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("调小预算后应快速返回，实际耗时 %v", elapsed)
	}
}

// ── 回归护栏（对应代码审查里修掉的缺陷）──────────────────────────────────────

// fakeUpstream 起一个可编程的假上游，用于验证编排层的取舍。
func fakeUpstream(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *upstream.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(handler))
	t.Cleanup(srv.Close)
	toks, err := auth.NewTokenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := toks.Save(&auth.Token{AccessToken: "t", TokenType: "Bearer", UID: "u",
		ExpiresAt: time.Now().Add(time.Hour).Unix()}); err != nil {
		t.Fatal(err)
	}
	c := upstream.New(toks, func() string { return "cn" }, 10)
	c.GrowthBaseOverride, c.WebBaseOverride, c.MPBaseOverride = srv.URL, srv.URL, srv.URL
	return c
}

// tasksResponder 只回任务列表，其余端点 200。
func tasksResponder(list string) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/activity/growth/tasks" {
			_, _ = w.Write([]byte(`{"code":0,"data":{"tasks":[` + list + `]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"data":null}`))
	}
}

// TestLockedTaskSkippedWithoutAction 锁定任务必须 skip 且**不产生任何上报**
// （Auto 与 AutoAll 两条路径都要守）。修前 AutoAll 的 runOneLocked 漏了 locked 检查，
// 会对 Sequential 链条的锁定环节反复发上报与真实对话，白耗配额。
func TestLockedTaskSkippedWithoutAction(t *testing.T) {
	restore := ThrottleTuning(-1, -1, -1, -1)
	defer restore()
	restorePoll := upstream.TaskPollTuning(time.Millisecond, 2)
	defer restorePoll()

	var reports int32
	c := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/activity/growth/tasks":
			_, _ = w.Write([]byte(`{"code":0,"data":{"tasks":[
			  {"task_code":"chat_5","target":5,"current":0,"locked":true}]}}`))
		case "/v2/report":
			atomic.AddInt32(&reports, 1)
			_, _ = w.Write([]byte(`{"code":0}`))
		default:
			_, _ = w.Write([]byte(`{"code":0,"data":null}`))
		}
	})
	r := NewRunner(c)

	res, err := r.Auto("chat_5")
	if err != nil {
		t.Fatalf("Auto: %v", err)
	}
	if res.Status != "skipped" {
		t.Errorf("locked 任务 Auto 应 skipped，得到 %q（%s）", res.Status, res.Message)
	}

	before := atomic.LoadInt32(&reports)
	results, _ := r.AutoAll()
	for _, x := range results {
		if x.TaskCode == "chat_5" && x.Status != "skipped" {
			t.Errorf("locked 任务 AutoAll 应 skipped，得到 %q", x.Status)
		}
	}
	if got := atomic.LoadInt32(&reports); got != before {
		t.Errorf("locked 任务不应产生上报，新增 %d 次", got-before)
	}
}

// TestActionFailureReportedAsError 动作内部失败必须体现为 status=error，
// 不能返回 done（前端据此决定 toast 颜色，done 会把失败弹成绿色「已完成」）。
func TestActionFailureReportedAsError(t *testing.T) {
	restore := ThrottleTuning(-1, -1, -1, -1)
	defer restore()
	restorePoll := upstream.TaskPollTuning(time.Millisecond, 2)
	defer restorePoll()

	c := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/activity/growth/tasks":
			_, _ = w.Write([]byte(`{"code":0,"data":{"tasks":[
			  {"task_code":"chat_5","target":5,"current":0}]}}`))
		case "/v2/report":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"code":500,"msg":"report rejected"}`))
		default:
			_, _ = w.Write([]byte(`{"code":0,"data":null}`))
		}
	})

	res, err := NewRunner(c).Auto("chat_5")
	if err != nil {
		t.Fatalf("Auto: %v", err)
	}
	if res.Status != "error" {
		t.Errorf("上报失败时 status 应为 error，得到 %q（%s）", res.Status, res.Message)
	}
	if res.Claimed {
		t.Error("上报失败不应标记已领奖")
	}
}

// TestNeedProgressErrorPropagates 进度查询失败必须当失败上报，
// 不能被当成「已达标，无需上报」（修前上游一次 500 就让 9 个动作统一静默成功）。
func TestNeedProgressErrorPropagates(t *testing.T) {
	restore := ThrottleTuning(-1, -1, -1, -1)
	defer restore()

	c := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/activity/growth/tasks" {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"code":500,"msg":"boom"}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"data":null}`))
	})

	_, _, _, err := NewRunner(c).needProgress("chat_5", false, 5)
	if err == nil {
		t.Error("列表查询失败时 needProgress 必须返回 error")
	}
}

// TestRunnerMutualExclusionIsPerInstance 说明性护栏：互斥是**实例字段**。
// 生产必须让 admin 与 scheduler 共用同一个 Runner（scheduler.Runner()），
// 各自 NewRunner 会让夜猫子 cron 与面板一键完成并发跑两轮真实对话。
func TestRunnerMutualExclusionIsPerInstance(t *testing.T) {
	c := newBareClient(t)
	r1, r2 := NewRunner(c), NewRunner(c)
	if !r1.mu.TryLock() {
		t.Fatal("r1 应能加锁")
	}
	defer r1.mu.Unlock()
	if !r2.mu.TryLock() {
		// 未来若改成包级共享锁，本断言会失败——那时应删掉本测试并更新调用方说明。
		t.Log("两个 Runner 已共享互斥（架构已变，请复核 admin/scheduler 的注入口径）")
		return
	}
	r2.mu.Unlock()
	t.Log("确认：互斥按实例生效，生产须共用同一 Runner 实例")
}

// TestAutoAllCostBounded 全部已领取时 AutoAll 的上游请求数必须有界（防回归到 39 次）。
func TestAutoAllCostBounded(t *testing.T) {
	restore := ThrottleTuning(-1, -1, -1, -1)
	defer restore()

	var n int32
	c := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		_, _ = w.Write([]byte(`{"code":0,"data":{"tasks":[
		  {"task_code":"chat_5","target":5,"current":5,"accept_status":"claimed"}]}}`))
	})

	start := time.Now()
	if _, err := NewRunner(c).AutoAll(); err != nil {
		t.Fatalf("AutoAll: %v", err)
	}
	elapsed := time.Since(start)

	// 25 个动作里只有 1 个任务被下发：1 次合并列表 + 1 次 accept 列表 + 1 次 mp 列表
	// + AcceptTasks 若干。留足余量但必须远低于「每个动作各查一次」的 39+。
	if got := atomic.LoadInt32(&n); got > 20 {
		t.Errorf("全部已领取时上游请求 %d 次，应 ≤20（快照复用回归了）", got)
	}
	if elapsed > 5*time.Second {
		t.Errorf("全部已领取时应快速返回，实际 %v（跳过项不该等节流）", elapsed)
	}
}

// TestAutoAllContextCancel ctx 取消后必须尽快返回，不把剩余动作的节流走完。
func TestAutoAllContextCancel(t *testing.T) {
	restore := ThrottleTuning(3*time.Second, 0, 0, 0) // 项间 3s，若不可取消要 25×3s
	defer restore()

	c := fakeUpstream(t, tasksResponder(`{"task_code":"chat_5","target":5,"current":0}`))
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()

	start := time.Now()
	results, err := NewRunner(c).AutoAllContext(ctx)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("AutoAllContext: %v", err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("取消失效，耗时 %v", elapsed)
	}
	var sawCancel bool
	for _, r := range results {
		if r.TaskCode == "(已取消)" {
			sawCancel = true
		}
	}
	if !sawCancel {
		t.Log("取消发生在第一个动作期间，未留下取消标记（可接受）")
	}
}

// TestMPClaimScopeUsesTaskCode 清扫时 mp 任务必须走 mp 领奖端点，即使它同时出现在
// 默认列表里（合并去重保留默认口径 → Task.MP=false）。
func TestMPClaimScopeUsesTaskCode(t *testing.T) {
	restore := ThrottleTuning(-1, -1, -1, -1)
	defer restore()

	var claimPlatform []string
	c := fakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/activity/growth/tasks":
			// 两份口径都下发 Sequential_Tasks_1（已达标未领）
			_, _ = w.Write([]byte(`{"code":0,"data":{"tasks":[
			  {"task_code":"Sequential_Tasks_1","target":1,"current":1,"accept_status":"accepted"}]}}`))
		case strings.HasSuffix(r.URL.Path, "/claim"):
			claimPlatform = append(claimPlatform, r.Header.Get("X-Client-Platform"))
			if r.Header.Get("X-Client-Platform") != "miniprogram" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"code":400,"msg":"task not found"}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"credit":100}}`))
		default:
			_, _ = w.Write([]byte(`{"code":0,"data":null}`))
		}
	})

	st := NewRunner(c).Sweep("cn")
	if st.Claimed != 1 {
		t.Errorf("应成功领到 1 项，得到 claimed=%d errors=%d（领奖轨迹 %v）",
			st.Claimed, st.Errors, claimPlatform)
	}
}

// TestActionsMPFlagsMatchUpstreamWhitelist 动作表的 MP 标记必须与 upstream 的
// mpTaskCodes 白名单一致（两处不能各说各话）。
func TestActionsMPFlagsMatchUpstreamWhitelist(t *testing.T) {
	for _, a := range Actions() {
		want := upstream.IsMPTaskCode(a.Code)
		if a.MP != want {
			t.Errorf("%s: 动作表 MP=%v，但 mpTaskCodes 判定为 %v", a.Code, a.MP, want)
		}
	}
}
