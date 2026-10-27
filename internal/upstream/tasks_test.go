package upstream

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"buddy2api-go/internal/auth"
)

// taskSrv 任务接口桩：记录请求（路径 / 头 / 体），按预设返回。
type taskSrv struct {
	mu       sync.Mutex
	paths    []string
	headers  []http.Header
	bodies   []string
	listResp string   // 默认口径列表响应
	mpResp   string   // mp 口径列表响应
	claims   []string // claim 命中的 task_code
	webHit   int      // Web 域请求数
	chatHit  int      // chat 域请求数
}

func (s *taskSrv) handler(web bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, 0, 512)
		buf := make([]byte, 512)
		for {
			n, err := r.Body.Read(buf)
			body = append(body, buf[:n]...)
			if err != nil {
				break
			}
		}
		s.mu.Lock()
		s.paths = append(s.paths, r.URL.Path)
		s.headers = append(s.headers, r.Header.Clone())
		s.bodies = append(s.bodies, string(body))
		if web {
			s.webHit++
		} else {
			s.chatHit++
		}
		s.mu.Unlock()

		switch {
		case strings.HasSuffix(r.URL.Path, "/claim"):
			code := r.URL.Path[strings.LastIndex(r.URL.Path, "/tasks/")+len("/tasks/") : strings.LastIndex(r.URL.Path, "/claim")]
			s.mu.Lock()
			s.claims = append(s.claims, code)
			s.mu.Unlock()
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"credit":100,"energy":5}}`))
		case strings.HasSuffix(r.URL.Path, "/tasks/accept"):
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		case strings.HasSuffix(r.URL.Path, "/tasks"):
			if r.Header.Get("X-Client-Platform") == mpPlatform {
				_, _ = w.Write([]byte(s.mpResp))
				return
			}
			_, _ = w.Write([]byte(s.listResp))
		default:
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		}
	}
}

// newTaskClient 构造一个同时替换 growthBase 与 webBase 的测试客户端。
func newTaskClient(t *testing.T, srv *taskSrv) (*Client, func()) {
	t.Helper()
	chatSrv := httptest.NewServer(srv.handler(false))
	webSrv := httptest.NewServer(srv.handler(true))
	c, cleanup1 := testClient(chatSrv.URL)
	origWeb := webBase
	webBase = func(*Client) string { return webSrv.URL }
	return c, func() {
		webBase = origWeb
		cleanup1()
		chatSrv.Close()
		webSrv.Close()
	}
}

const defaultTasksJSON = `{"code":0,"msg":"ok","data":{"tasks":[
 {"task_code":"chat_5","title":"对话5次","reward_credit":100,"target":5,"current":2,"accept_status":"accepted"},
 {"task_code":"first_buddy","title":"领养","reward_credit":300,"reward_energy":8,"target":1,"current":0,"progress":{"current":0,"target":1}},
 {"task_code":"automation_1","title":"定时任务","reward_credit":100,"target":1,"current":1,"accept_status":"not_accepted"},
 {"task_code":"create_canvas","title":"画布","reward_credit":300,"target":1,"current":1,"accept_status":"claimed"},
 {"task_code":"locked_one","title":"未解锁","reward_credit":50,"target":1,"current":0,"locked":true}
]}}`

const mpOnlyTasksJSON = `{"code":0,"msg":"ok","data":{"tasks":[
 {"task_code":"chat_5","title":"对话5次","reward_credit":100,"target":5,"current":2,"accept_status":"accepted"},
 {"task_code":"school_season","title":"校园日","reward_credit":100,"reward_energy":5,"target":1,"current":0},
 {"task_code":"Sequential_Tasks_1","title":"小程序首对话","reward_credit":100,"target":1,"current":1}
]}}`

// TestListTasksParsing 解析：progress 对象 / 平铺 / claimed / locked / Claimable 推算。
func TestListTasksParsing(t *testing.T) {
	srv := &taskSrv{listResp: defaultTasksJSON, mpResp: mpOnlyTasksJSON}
	c, cleanup := newTaskClient(t, srv)
	defer cleanup()

	tasks, err := c.ListTasks()
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 5 {
		t.Fatalf("应有 5 个任务，得到 %d", len(tasks))
	}
	byCode := map[string]Task{}
	for _, tk := range tasks {
		byCode[tk.TaskCode] = tk
	}
	if tk := byCode["chat_5"]; tk.Current != 2 || tk.Target != 5 || tk.Claimable || tk.Done() {
		t.Errorf("chat_5 应为 2/5 且未达标，得到 %+v", tk)
	}
	// progress 对象形态覆盖平铺字段
	if tk := byCode["first_buddy"]; tk.Target != 1 || tk.Current != 0 || tk.Credit != 300 || tk.Energy != 8 {
		t.Errorf("first_buddy 解析错误: %+v", tk)
	}
	// 达标未领 → Claimable
	if tk := byCode["automation_1"]; !tk.Claimable || !tk.Done() {
		t.Errorf("automation_1 应达标可领，得到 %+v", tk)
	}
	// 已领
	if tk := byCode["create_canvas"]; !tk.Claimed || tk.Claimable {
		t.Errorf("create_canvas 应已领取且不可再领，得到 %+v", tk)
	}
	if tk := byCode["locked_one"]; !tk.Locked {
		t.Errorf("locked_one 应锁定，得到 %+v", tk)
	}
}

// TestListTasksMPHeader 小程序口径必须带 X-Client-Platform: miniprogram，且任务标记 MP。
func TestListTasksMPHeader(t *testing.T) {
	srv := &taskSrv{listResp: defaultTasksJSON, mpResp: mpOnlyTasksJSON}
	c, cleanup := newTaskClient(t, srv)
	defer cleanup()

	tasks, err := c.ListTasksMP()
	if err != nil {
		t.Fatalf("ListTasksMP: %v", err)
	}
	if len(tasks) != 3 {
		t.Fatalf("mp 列表应有 3 个任务，得到 %d", len(tasks))
	}
	if !tasks[0].MP {
		t.Error("mp 口径任务应标记 MP=true")
	}
	found := false
	for _, h := range srv.headers {
		if h.Get("X-Client-Platform") == mpPlatform {
			found = true
		}
	}
	if !found {
		t.Errorf("mp 请求缺少 X-Client-Platform 头，记录的头: %v", srv.headers)
	}
}

// TestListTasksMergedDedup 双口径合并：默认优先，mp 补齐且按 task_code 去重。
func TestListTasksMergedDedup(t *testing.T) {
	srv := &taskSrv{listResp: defaultTasksJSON, mpResp: mpOnlyTasksJSON}
	c, cleanup := newTaskClient(t, srv)
	defer cleanup()

	tasks, err := c.ListTasksMerged()
	if err != nil {
		t.Fatalf("ListTasksMerged: %v", err)
	}
	codes := map[string]int{}
	for _, tk := range tasks {
		codes[tk.TaskCode]++
	}
	if codes["chat_5"] != 1 {
		t.Errorf("chat_5 应去重为 1 条，得到 %d", codes["chat_5"])
	}
	if codes["school_season"] != 1 || codes["Sequential_Tasks_1"] != 1 {
		t.Errorf("mp 专属任务应被合并进来，得到 %v", codes)
	}
}

// TestListTasksMergedMPFailureSilent mp 列表失败时静默返回默认口径结果。
func TestListTasksMergedMPFailureSilent(t *testing.T) {
	chatSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Client-Platform") == mpPlatform {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"code":500,"msg":"down"}`))
			return
		}
		_, _ = w.Write([]byte(defaultTasksJSON))
	}))
	defer chatSrv.Close()
	c, cleanup := testClient(chatSrv.URL)
	defer cleanup()

	tasks, err := c.ListTasksMerged()
	if err != nil {
		t.Fatalf("mp 失败不应影响默认口径: %v", err)
	}
	if len(tasks) != 5 {
		t.Errorf("应返回默认口径 5 条，得到 %d", len(tasks))
	}
}

// TestClaimRewardWebDomain 默认任务领奖走 Web 域 + x-client-platform: web。
func TestClaimRewardWebDomain(t *testing.T) {
	srv := &taskSrv{listResp: defaultTasksJSON, mpResp: mpOnlyTasksJSON}
	c, cleanup := newTaskClient(t, srv)
	defer cleanup()

	credit, energy, err := c.ClaimReward("automation_1")
	if err != nil {
		t.Fatalf("ClaimReward: %v", err)
	}
	if credit != 100 || energy != 5 {
		t.Errorf("奖励解析错误: credit=%d energy=%d", credit, energy)
	}
	if srv.webHit != 1 || srv.chatHit != 0 {
		t.Errorf("领奖应只发往 Web 域，web=%d chat=%d", srv.webHit, srv.chatHit)
	}
	if !strings.Contains(srv.paths[0], "/activity/growth/tasks/automation_1/claim") {
		t.Errorf("领奖路径错误: %s", srv.paths[0])
	}
	if got := srv.headers[0].Get("x-client-platform"); got != "web" {
		t.Errorf("Web 域领奖应带 x-client-platform: web，得到 %q", got)
	}
}

// TestClaimRewardMPFallsBackToWeb mp 领奖在 chat 域 400 时降级 Web 域。
func TestClaimRewardMPFallsBackToWeb(t *testing.T) {
	chatSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":400,"msg":"task not completed"}`))
	}))
	defer chatSrv.Close()
	webSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"credit":200,"energy":5}}`))
	}))
	defer webSrv.Close()

	c, cleanup := testClient(chatSrv.URL)
	defer cleanup()
	origWeb := webBase
	webBase = func(*Client) string { return webSrv.URL }
	defer func() { webBase = origWeb }()

	credit, energy, err := c.ClaimRewardMP("school_season")
	if err != nil {
		t.Fatalf("mp 领奖降级失败: %v", err)
	}
	if credit != 200 || energy != 5 {
		t.Errorf("降级后奖励解析错误: credit=%d energy=%d", credit, energy)
	}
}

// TestClaimRewardAlreadyClaimed 重复领取返回 (0,0,nil)，不视为错误。
func TestClaimRewardAlreadyClaimed(t *testing.T) {
	webSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"already_claimed":true,"credit":0,"energy":0}}`))
	}))
	defer webSrv.Close()
	chatSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
	}))
	defer chatSrv.Close()

	c, cleanup := testClient(chatSrv.URL)
	defer cleanup()
	origWeb := webBase
	webBase = func(*Client) string { return webSrv.URL }
	defer func() { webBase = origWeb }()

	credit, energy, err := c.ClaimReward("chat_5")
	if err != nil {
		t.Fatalf("已领取不应报错: %v", err)
	}
	if credit != 0 || energy != 0 {
		t.Errorf("已领取应返回 0/0，得到 %d/%d", credit, energy)
	}
}

// TestAcceptTasksSplitMP accept 默认与 mp 必须分开提交（混提交会让 mp 任务 400）。
func TestAcceptTasksSplitMP(t *testing.T) {
	srv := &taskSrv{listResp: defaultTasksJSON, mpResp: mpOnlyTasksJSON}
	c, cleanup := newTaskClient(t, srv)
	defer cleanup()

	if err := c.AcceptTasks([]string{"chat_5", "first_buddy"}); err != nil {
		t.Fatalf("AcceptTasks: %v", err)
	}
	if err := c.AcceptTasksMP([]string{"school_season"}); err != nil {
		t.Fatalf("AcceptTasksMP: %v", err)
	}
	if len(srv.bodies) != 2 {
		t.Fatalf("应有 2 次 accept 请求，得到 %d", len(srv.bodies))
	}
	if !strings.Contains(srv.bodies[0], "chat_5") {
		t.Errorf("默认口径请求体应含 chat_5: %s", srv.bodies[0])
	}
	// 第一次（默认口径）不应带 mp 头，第二次（mp）必须带
	if srv.headers[0].Get("X-Client-Platform") != "" {
		t.Error("默认口径 accept 不应带 mp 头")
	}
	if srv.headers[1].Get("X-Client-Platform") != mpPlatform {
		t.Errorf("mp accept 必须带 mp 头，得到 %q", srv.headers[1].Get("X-Client-Platform"))
	}
}

// TestAcceptTasksEmptyNoRequest 空列表不发请求。
func TestAcceptTasksEmptyNoRequest(t *testing.T) {
	srv := &taskSrv{listResp: defaultTasksJSON}
	c, cleanup := newTaskClient(t, srv)
	defer cleanup()
	if err := c.AcceptTasks(nil); err != nil {
		t.Fatal(err)
	}
	if err := c.AcceptTasksMP(nil); err != nil {
		t.Fatal(err)
	}
	if len(srv.paths) != 0 {
		t.Errorf("空列表不应发请求，实际 %v", srv.paths)
	}
}

// TestTaskByCodeWaitingPoll 异步计分：首轮未达标 → 轮询到达标即返回。
func TestTaskByCodeWaitingPoll(t *testing.T) {
	origGap := claimPollGap
	claimPollGap = 0
	t.Cleanup(func() { claimPollGap = origGap })

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		cur := 0
		if calls >= 3 {
			cur = 5
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"tasks":[
		 {"task_code":"chat_5","target":5,"current":` + itoa(cur) + `}]}}`))
	}))
	defer srv.Close()
	c, cleanup := testClient(srv.URL)
	defer cleanup()

	tk, err := c.TaskByCodeWaiting("chat_5", false)
	if err != nil {
		t.Fatalf("TaskByCodeWaiting: %v", err)
	}
	if tk == nil || !tk.Done() {
		t.Fatalf("轮询应最终拿到达标状态，得到 %+v", tk)
	}
	if calls < 3 {
		t.Errorf("应至少轮询 3 次，实际 %d", calls)
	}
}

// TestTaskByCodeWaitingExhausted 预算耗尽返回最后一次读数（不报错）。
func TestTaskByCodeWaitingExhausted(t *testing.T) {
	origGap, origAttempts := claimPollGap, claimPollAttempts
	claimPollGap, claimPollAttempts = 0, 2
	t.Cleanup(func() { claimPollGap, claimPollAttempts = origGap, origAttempts })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"tasks":[
		 {"task_code":"chat_5","target":5,"current":1}]}}`))
	}))
	defer srv.Close()
	c, cleanup := testClient(srv.URL)
	defer cleanup()

	tk, err := c.TaskByCodeWaiting("chat_5", false)
	if err != nil {
		t.Fatalf("预算耗尽不应报错: %v", err)
	}
	if tk == nil || tk.Current != 1 {
		t.Errorf("应返回最后一次读数 1/5，得到 %+v", tk)
	}
}

// TestTaskViews 辅助方法（Done / Progress / FindTask）。
func TestTaskViews(t *testing.T) {
	tk := Task{TaskCode: "a", Target: 5, Current: 2}
	if tk.Done() || tk.Progress() != "2/5" {
		t.Errorf("未达标视图错误: %s", tk.Progress())
	}
	tk.Current = 5
	if !tk.Done() || tk.Progress() != "5/5" {
		t.Errorf("达标视图错误: %s", tk.Progress())
	}
	tk = Task{TaskCode: "b", AcceptStatus: "accepted"}
	if tk.Done() || tk.Progress() != "accepted" {
		t.Errorf("无 target 视图错误: %s", tk.Progress())
	}
	if FindTask([]Task{{TaskCode: "x"}}, "x") == nil {
		t.Error("FindTask 应命中")
	}
	if FindTask([]Task{{TaskCode: "x"}}, "y") != nil {
		t.Error("FindTask 未命中应返回 nil")
	}
}

// TestCompensationStatusPending 补偿「可领」判定：三者与门。
func TestCompensationStatusPending(t *testing.T) {
	cases := []struct {
		st   CompensationStatus
		want bool
	}{
		{CompensationStatus{Active: false, CreditNum: 500}, false},
		{CompensationStatus{Active: true, Claimed: true, CreditNum: 500}, false},
		{CompensationStatus{Active: true, CreditNum: 0}, false},
		{CompensationStatus{Active: true, CreditNum: 500}, true},
	}
	for _, c := range cases {
		if got := c.st.Pending(); got != c.want {
			t.Errorf("%+v: Pending=%v want %v", c.st, got, c.want)
		}
	}
}

// TestGetCompensationStatus 解析补偿状态端点。
func TestGetCompensationStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/billing/meter/compensation-status") {
			t.Errorf("路径错误: %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"active":true,"claimed":false,"credit_num":500,"end_time":"2026-10-22 23:59:59"}}`))
	}))
	defer srv.Close()
	c, cleanup := testClient(srv.URL)
	defer cleanup()

	st, err := c.GetCompensationStatus()
	if err != nil {
		t.Fatalf("GetCompensationStatus: %v", err)
	}
	if !st.Pending() || st.CreditNum != 500 || st.EndTime != "2026-10-22 23:59:59" {
		t.Errorf("解析错误: %+v", st)
	}
}

// TestEligibleTiersAll 兑换要把所有已解锁未领档位都列出来（按 days 升序）。
func TestEligibleTiersAll(t *testing.T) {
	info := &GrowthStreakInfo{}
	info.Streak.Days = 20
	info.Redemption.Tier7dStatus = "claimed"
	info.Redemption.Tiers = []GrowthTierSpec{
		{Tier: "28d", Days: 28},
		{Tier: "7d", Days: 7},
		{Tier: "14d", Days: 14},
	}
	got := info.EligibleTiers()
	if len(got) != 1 || got[0] != "14d" {
		t.Errorf("20 天时只有 14d 可领，得到 %v", got)
	}
	// 30 天：14d/28d 均可领（7d 已领），升序
	info.Streak.Days = 30
	got = info.EligibleTiers()
	if len(got) != 2 || got[0] != "14d" || got[1] != "28d" {
		t.Errorf("30 天时应为 [14d 28d]，得到 %v", got)
	}
	// EligibleTier 仍返回最高档
	if top := info.EligibleTier(); top != "28d" {
		t.Errorf("EligibleTier 应返回最高档 28d，得到 %s", top)
	}
}

// TestGrowthLotteryChancesFallback /chances 返回 0 时回退 /summary 的 chances。
func TestGrowthLotteryChancesFallback(t *testing.T) {
	var hits []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "/lottery/chances") {
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"balance":0}}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"chances":3}}`))
	}))
	defer srv.Close()
	c, cleanup := testClient(srv.URL)
	defer cleanup()

	n, err := c.GrowthLotteryChances()
	if err != nil {
		t.Fatalf("GrowthLotteryChances: %v", err)
	}
	if n != 3 {
		t.Errorf("应回退到 summary 拿到 3 次，得到 %d", n)
	}
	if len(hits) != 2 {
		t.Errorf("应请求两次（chances → summary），实际 %v", hits)
	}
}

// TestReportChatActivityModel 模型字段可定制（Model_chat_GLM5.2 / black_cat 判据）。
func TestReportChatActivityModel(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		body = string(buf[:n])
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
	}))
	defer srv.Close()
	c, cleanup := testClient(srv.URL)
	defer cleanup()

	if err := c.ReportChatActivityModel("c1", "r1", "glm-5.2", "GLM-5.2"); err != nil {
		t.Fatalf("ReportChatActivityModel: %v", err)
	}
	var arr []map[string]any
	if err := json.Unmarshal([]byte(body), &arr); err != nil {
		t.Fatalf("请求体应为事件数组: %v (%s)", err, body)
	}
	if len(arr) != 1 {
		t.Fatalf("应为单元素数组，得到 %d", len(arr))
	}
	if arr[0]["requestModelId"] != "glm-5.2" || arr[0]["requestModelName"] != "GLM-5.2" {
		t.Errorf("模型字段未生效: %v", arr[0])
	}
	if arr[0]["userId"] != "test-uid" {
		t.Errorf("userId 必须等于 token UID: %v", arr[0]["userId"])
	}
}

// TestInNightWindow 夜猫子窗口边界。
func TestInNightWindow(t *testing.T) {
	cases := []struct {
		hour int
		want bool
	}{
		{22, false}, {23, true}, {0, true}, {7, true}, {8, false}, {12, false},
	}
	for _, c := range cases {
		at := time.Date(2026, 10, 26, c.hour, 30, 0, 0, time.Local)
		if got := InNightWindow(at); got != c.want {
			t.Errorf("%02d:30 InNightWindow=%v want %v", c.hour, got, c.want)
		}
	}
}

// TestBlackcatNeed 进度差额：已达标记 / 不存在 / 有差额。
func TestBlackcatNeed(t *testing.T) {
	cases := []struct {
		name string
		resp string
		want int64
	}{
		{"有差额", `{"code":0,"data":{"tasks":[{"task_code":"black_cat","target":3,"current":1}]}}`, 2},
		{"已达标", `{"code":0,"data":{"tasks":[{"task_code":"black_cat","target":3,"current":3}]}}`, 0},
		{"已领取", `{"code":0,"data":{"tasks":[{"task_code":"black_cat","target":3,"current":3,"accept_status":"claimed"}]}}`, 0},
		{"无此任务", `{"code":0,"data":{"tasks":[]}}`, 0},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(c.resp))
		}))
		client, cleanup := testClient(srv.URL)
		got, err := client.BlackcatNeed()
		cleanup()
		srv.Close()
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got != c.want {
			t.Errorf("%s: need=%d want %d", c.name, got, c.want)
		}
	}
}

// TestDesktopEventFingerprint 桌面事件必须带 workbuddy-desktop 指纹与 userId。
func TestDesktopEventFingerprint(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 8192)
		n, _ := r.Body.Read(buf)
		body = string(buf[:n])
		if r.Header.Get("User-Agent") != desktopUA {
			t.Errorf("桌面上报 UA 错误: %q", r.Header.Get("User-Agent"))
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
	}))
	defer srv.Close()
	c, cleanup := testClient(srv.URL)
	defer cleanup()

	err := c.ReportDesktopEvent(DesktopAutomationCreateEvent("t"))
	if err != nil {
		t.Fatalf("ReportDesktopEvent: %v", err)
	}
	var arr []map[string]any
	if err := json.Unmarshal([]byte(body), &arr); err != nil {
		t.Fatalf("请求体应为数组: %v", err)
	}
	ev := arr[0]
	if ev["extName"] != "workbuddy-desktop" || ev["ideType"] != "WorkBuddy" {
		t.Errorf("桌面指纹缺失: %v", ev)
	}
	if ev["userId"] != "test-uid" {
		t.Errorf("userId 应为 token UID，得到 %v", ev["userId"])
	}
	if ev["eventCode"] != "automated_task_create_suc" {
		t.Errorf("eventCode 错误: %v", ev["eventCode"])
	}
}

// TestDesktopChatSequenceShape 桌面对话链必须含 6 个事件，且 response 带 isSuccessful。
func TestDesktopChatSequenceShape(t *testing.T) {
	events := DesktopChatSequence("c1", "r1", "m1", "fast-model", "fast-model")
	if len(events) != 6 {
		t.Fatalf("应为 6 事件，得到 %d", len(events))
	}
	want := []string{"agent_task_created", "chat_message_send", "chat_request_send",
		"chat_message_response", "chat_message_status", "chat_request_response"}
	for i, code := range want {
		if events[i]["eventCode"] != code {
			t.Errorf("第 %d 个事件应为 %s，得到 %v", i, code, events[i]["eventCode"])
		}
	}
	if events[3]["isSuccessful"] != true {
		t.Error("chat_message_response 必须带 isSuccessful=true（RichMeow 判据）")
	}
}

// TestMPChatEventActivityID 校园日事件带 activityId，普通任务不带。
func TestMPChatEventActivityID(t *testing.T) {
	ev := MPChatEvent("c1", "")
	if _, ok := ev["activityId"]; ok {
		t.Error("普通 mp 对话事件不应带 activityId")
	}
	ev = MPSchoolSeasonChatEvent("c1")
	if ev["activityId"] != schoolOpenDayActivityID {
		t.Errorf("校园日事件必须带 activityId=%s，得到 %v", schoolOpenDayActivityID, ev["activityId"])
	}
}

// TestReportWebEventHeaders Web 域上报：x-client-platform=web + 浏览器 UA + Origin。
func TestReportWebEventHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-client-platform") != "web" {
			t.Errorf("缺少 x-client-platform: web，得到 %q", r.Header.Get("x-client-platform"))
		}
		if !strings.Contains(r.Header.Get("User-Agent"), "Mozilla") {
			t.Errorf("web 上报 UA 应为浏览器形态，得到 %q", r.Header.Get("User-Agent"))
		}
		if r.Header.Get("Origin") == "" {
			t.Error("缺少 Origin")
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
	}))
	defer srv.Close()
	c, cleanup := testClient(srv.URL)
	defer cleanup()
	origWeb := webBase
	webBase = func(*Client) string { return srv.URL }
	defer func() { webBase = origWeb }()

	if err := c.ReportWebEvent("web_element_click", "https://x/y", "library_doc_intro_click", "资料库"); err != nil {
		t.Fatalf("ReportWebEvent: %v", err)
	}
}

// itoa 极简整数转字符串（避免为一个测试引入 strconv）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// ── 回归护栏（对应代码审查里修掉的缺陷）──
//
// 注意：本文件多个用例会改写包级主机变量（growthBase/webBase/mpReportBaseFn）
// 与轮询参数（TaskPollTuning），**不可加 t.Parallel()**。──────────────────────────────────────

// TestExpiredTokenRefreshedForAllEndpoints token 已过期时，**每个**鉴权端点都必须
// 走「刷新 → 用新 token 重建请求 → 成功」。
//
// 修前：growthRequest 先把旧 token 写进头，再调 EnsureValid（刷新结果对已建好的请求
// 无效），desktop/mp 上报连刷新都没调。结果「列表能读、领奖与上报恒 401」。
func TestExpiredTokenRefreshedForAllEndpoints(t *testing.T) {
	type hit struct{ path, auth, platform string }
	run := func(name string, fn func(c *Client) error) {
		var hits []hit
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits = append(hits, hit{r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("X-Client-Platform")})
			if r.URL.Path == "/v2/plugin/auth/token/refresh" {
				_, _ = w.Write([]byte(`{"code":0,"data":{"accessToken":"NEW","expiresIn":3600,"refreshToken":"r2"}}`))
				return
			}
			if r.Header.Get("Authorization") != "Bearer NEW" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"code":401,"msg":"expired"}`))
				return
			}
			switch r.URL.Path {
			case "/v2/activity/growth/tasks":
				_, _ = w.Write([]byte(`{"code":0,"data":{"tasks":[{"task_code":"chat_5","target":5,"current":5}]}}`))
			case "/portal/operation-platform/market/expert/list":
				_, _ = w.Write([]byte(`{"code":0,"data":{"experts":[{"expert_id":"ex_1"}]}}`))
			case "/v2/report":
				_, _ = w.Write([]byte(`{"code":0}`))
			default:
				_, _ = w.Write([]byte(`{"code":0,"data":{"credit":100,"energy":5}}`))
			}
		}))
		defer srv.Close()

		toks, err := auth.NewTokenStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if err := toks.Save(&auth.Token{AccessToken: "OLD", TokenType: "Bearer", UID: "u",
			RefreshToken: "r", ExpiresAt: time.Now().Add(-time.Hour).Unix()}); err != nil {
			t.Fatal(err)
		}
		c := New(toks, func() string { return "cn" }, 10)
		c.GrowthBaseOverride, c.WebBaseOverride, c.MPBaseOverride = srv.URL, srv.URL, srv.URL
		prev := authBase
		authBase = func(*Client) string { return srv.URL }
		defer func() { authBase = prev }()

		if err := fn(c); err != nil {
			t.Errorf("%s: 过期 token 下应自动刷新重试成功，得到 %v（请求轨迹 %v）", name, err, hits)
			return
		}
		for _, h := range hits {
			if h.path != "/v2/plugin/auth/token/refresh" && h.auth == "Bearer OLD" {
				t.Errorf("%s: 请求 %s 仍带旧 token", name, h.path)
			}
		}
	}

	run("ListTasks", func(c *Client) error { _, e := c.ListTasks(); return e })
	run("ClaimReward", func(c *Client) error { _, _, e := c.ClaimReward("chat_5"); return e })
	run("ClaimRewardMP", func(c *Client) error { _, _, e := c.ClaimRewardMP("chat_5"); return e })
	run("AcceptTasks", func(c *Client) error { return c.AcceptTasks([]string{"chat_5"}) })
	run("AcceptTasksMP", func(c *Client) error { return c.AcceptTasksMP([]string{"chat_5"}) })
	run("ReportDesktopEvent", func(c *Client) error {
		return c.ReportDesktopEvent(DesktopAutomationCreateEvent("x"))
	})
	run("ReportWebEvent", func(c *Client) error {
		return c.ReportWebEvent("e", "https://www.workbuddy.cn/x", "id", "n")
	})
	run("ReportMPEvent", func(c *Client) error { return c.ReportMPEvent(MPChatEvent("c", "")) })
	run("SetAppearanceTheme", func(c *Client) error { return c.SetAppearanceTheme("k") })
	run("MarketExpertList", func(c *Client) error { _, e := c.MarketExpertList("agent"); return e })
	run("GetCompensationStatus", func(c *Client) error { _, e := c.GetCompensationStatus(); return e })
}

// TestMPScopeByTaskCodeNotListOrigin mp 任务的口径按**任务码**判定，不依赖它来自
// 哪份列表。默认列表也下发 mp 任务时，合并结果里 MP=false，但 NeedsMP 必须仍为 true
// ——否则领奖走 Web 域恒 400，且每轮清扫都会重试一次。
func TestMPScopeByTaskCodeNotListOrigin(t *testing.T) {
	tasks, err := parseGrowthTasks(json.RawMessage(
		`{"tasks":[{"task_code":"Sequential_Tasks_1","target":1,"current":1}]}`), false)
	if err != nil {
		t.Fatal(err)
	}
	if tasks[0].MP {
		t.Fatal("默认口径解析结果 MP 应为 false（观察值）")
	}
	if !tasks[0].NeedsMP() {
		t.Error("Sequential_Tasks_1 的 NeedsMP 必须为 true（码白名单判定）")
	}
	for _, code := range []string{"school_season", "Sequential_Tasks_1", "Sequential_Tasks_7"} {
		if !IsMPTaskCode(code) {
			t.Errorf("%s 应在 mpTaskCodes 白名单里", code)
		}
	}
	for _, code := range []string{"chat_5", "first_buddy", "automation_1", "black_cat"} {
		if IsMPTaskCode(code) {
			t.Errorf("%s 不应在 mpTaskCodes 白名单里", code)
		}
	}
}

// TestProgressPartialMerge progress 对象只带部分字段时，另一字段必须沿用平铺值。
// 修前是「两值整体覆盖」，{"current":3} 会把 target 抹成 0 → Claimable 恒 false。
func TestProgressPartialMerge(t *testing.T) {
	cases := []struct {
		name          string
		raw           string
		wantTarget    int64
		wantCurrent   int64
		wantClaimable bool
	}{
		{"仅 current", `{"tasks":[{"task_code":"a","target":5,"progress":{"current":5}}]}`, 5, 5, true},
		{"仅 target", `{"tasks":[{"task_code":"a","target":5,"current":9,"progress":{"target":9}}]}`, 9, 9, true},
		{"完整对象", `{"tasks":[{"task_code":"a","progress":{"current":5,"target":5}}]}`, 5, 5, true},
		{"无 progress", `{"tasks":[{"task_code":"a","target":5,"current":5}]}`, 5, 5, true},
		{"progress 为 null", `{"tasks":[{"task_code":"a","target":5,"current":2,"progress":null}]}`, 5, 2, false},
	}
	for _, c := range cases {
		got, err := parseGrowthTasks(json.RawMessage(c.raw), false)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got[0].Target != c.wantTarget || got[0].Current != c.wantCurrent {
			t.Errorf("%s: target=%d current=%d want %d/%d",
				c.name, got[0].Target, got[0].Current, c.wantTarget, c.wantCurrent)
		}
		if got[0].Claimable != c.wantClaimable {
			t.Errorf("%s: claimable=%v want %v", c.name, got[0].Claimable, c.wantClaimable)
		}
	}
}

// TestLockedTaskNotClaimable 锁定任务即使进度达标也不能标成可领（否则清扫每轮
// 都会对它 claim 一次并记 error）。
func TestLockedTaskNotClaimable(t *testing.T) {
	tasks, err := parseGrowthTasks(json.RawMessage(
		`{"tasks":[{"task_code":"Sequential_Tasks_3","target":5,"current":5,"locked":true}]}`), true)
	if err != nil {
		t.Fatal(err)
	}
	if tasks[0].Claimable {
		t.Error("locked 任务不应标记 Claimable")
	}
	if !tasks[0].Done() {
		t.Error("进度达标时 Done 仍应 true（Done 含已领取语义）")
	}
}

// TestMPMachineIDDerived 小程序 machineId 必须按账号派生（幂等且账号间互异），
// 不能是所有账号共用的字面量。
func TestMPMachineIDDerived(t *testing.T) {
	a, b := mpMachineID("uid-a"), mpMachineID("uid-b")
	if len(a) != 36 {
		t.Errorf("machineId 应为 36 字符 UUID 形态，得到 %d（%q）", len(a), a)
	}
	if a == b {
		t.Error("不同账号的 mp machineId 必须不同（共用同一设备指纹易被风控聚合）")
	}
	if mpMachineID("uid-a") != a {
		t.Error("同一账号的 mp machineId 必须恒定（否则每天换设备）")
	}
}
