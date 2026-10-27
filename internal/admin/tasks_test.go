package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"buddy2api-go/internal/auth"
	"buddy2api-go/internal/taskops"
	"buddy2api-go/internal/upstream"
)

// postJSONReq 发一个 JSON POST 到 handler func，返回解析后的响应体。
func postJSONReq(t *testing.T, fn http.HandlerFunc, body string) (int, map[string]any) {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(http.MethodPost, "/x", nil)
	} else {
		r = httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body))
	}
	w := httptest.NewRecorder()
	fn(w, r)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// getReq 发一个 GET 到 handler func。
func getReq(t *testing.T, fn http.HandlerFunc) (int, map[string]any) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	w := httptest.NewRecorder()
	fn(w, r)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// TestTasksGateNotLoggedIn 未登录：所有任务端点都返回 ok:false + 人话原因，
// 且不发起任何上游调用（httptest 里没有上游服务器，若发起调用会得到网关错误而非门控响应）。
func TestTasksGateNotLoggedIn(t *testing.T) {
	h := newTaskTestHandler(t, "cn", "", false)
	cases := []struct {
		name string
		fn   http.HandlerFunc
	}{
		{"tasksList", h.tasksList},
		{"tasksAccept", h.tasksAccept},
		{"tasksClaim", h.tasksClaim},
		{"tasksAuto", h.tasksAuto},
		{"tasksAutoAll", h.tasksAutoAll},
		{"tasksScan", h.tasksScan},
		{"tasksSweep", h.tasksSweep},
		{"compensation", h.compensation},
		{"compensationClaim", h.compensationClaim},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, out := postJSONReq(t, c.fn, "")
			if code != http.StatusOK {
				t.Errorf("门控失败应为 200（前端按 reason 展示），得到 %d", code)
			}
			if out["ok"] != false {
				t.Errorf("应返回 ok:false，得到 %v", out["ok"])
			}
			if out["reason"] != "not_logged_in" {
				t.Errorf("reason 应为 not_logged_in，得到 %v", out["reason"])
			}
			if out["detail"] == nil || out["detail"] == "" {
				t.Error("应带人话 detail")
			}
		})
	}
}

// TestTasksGateGlobal global 账号：reason=region_global（不发起上游调用）。
func TestTasksGateGlobal(t *testing.T) {
	h := newTaskTestHandler(t, "global", "", true)
	code, out := postJSONReq(t, h.tasksScan, "")
	if code != http.StatusOK || out["reason"] != "region_global" {
		t.Errorf("global 应被门控，得到 code=%d out=%v", code, out)
	}
}

// TestTasksGateEnterprise 企业版账号：reason=enterprise（上游 growth 域一律 403）。
func TestTasksGateEnterprise(t *testing.T) {
	h := newTaskTestHandler(t, "cn", "ent-123", true)
	_, out := postJSONReq(t, h.tasksScan, "")
	if out["reason"] != "enterprise" {
		t.Errorf("企业版应被门控，得到 %v", out["reason"])
	}
	if out["detail"] == nil || !strings.Contains(out["detail"].(string), "企业版") {
		t.Errorf("detail 应说明企业版无成长体系，得到 %v", out["detail"])
	}
}

// TestTasksActionsNoUpstream 动作清单端点不需要上游（门控外的纯数据端点）。
func TestTasksActionsNoUpstream(t *testing.T) {
	h := newTaskTestHandler(t, "cn", "", false)
	code, out := getReq(t, h.tasksActions)
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("动作清单应直接返回，得到 code=%d out=%v", code, out)
	}
	list, ok := out["actions"].([]any)
	if !ok || len(list) == 0 {
		t.Fatalf("应返回非空动作清单，得到 %v", out["actions"])
	}
	// 抽查关键任务码
	var codes []string
	for _, item := range list {
		if m, ok := item.(map[string]any); ok {
			if c, ok := m["code"].(string); ok {
				codes = append(codes, c)
			}
		}
	}
	joined := strings.Join(codes, ",")
	for _, want := range []string{"chat_5", "first_buddy", "automation_1", "school_season"} {
		if !strings.Contains(joined, want) {
			t.Errorf("动作清单应含 %s，得到 %v", want, codes)
		}
	}
}

// TestTasksClaimMissingCode 缺少 task_code → 400。
func TestTasksClaimMissingCode(t *testing.T) {
	h := newTaskTestHandler(t, "cn", "", true)
	code, out := postJSONReq(t, h.tasksClaim, `{}`)
	if code != http.StatusBadRequest {
		t.Errorf("缺少 task_code 应 400，得到 %d", code)
	}
	if out["error"] == nil {
		t.Error("应返回 error 文案")
	}
}

// TestTasksAutoUnsupportedTask 无自动动作的任务 → 501 + 指引。
func TestTasksAutoUnsupportedTask(t *testing.T) {
	h := newTaskTestHandler(t, "cn", "", true)
	code, out := postJSONReq(t, h.tasksAuto, `{"task_code":"Expert_Philanthropy"}`)
	if code != http.StatusNotImplemented {
		t.Errorf("不支持的任务应 501，得到 %d", code)
	}
	msg, _ := out["error"].(string)
	if !strings.Contains(msg, "客户端") {
		t.Errorf("错误文案应指引去客户端操作，得到 %q", msg)
	}
}

// TestTasksRoutesRegistered 路由注册自检：新端点真的挂在 /admin 路由树上（chi.Walk 走真实树）。
func TestTasksRoutesRegistered(t *testing.T) {
	h := newTaskTestHandler(t, "cn", "", false)
	r := chi.NewRouter()
	r.Route("/admin", h.Routes())

	got := map[string]bool{}
	if err := chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		got[method+" "+route] = true
		return nil
	}); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	want := []string{
		"GET /admin/tasks",
		"POST /admin/tasks/accept",
		"POST /admin/tasks/claim",
		"POST /admin/tasks/auto",
		"POST /admin/tasks/auto_all",
		"GET /admin/tasks/scan",
		"POST /admin/tasks/sweep",
		"GET /admin/tasks/actions",
		"GET /admin/compensation",
		"POST /admin/compensation/claim",
	}
	for _, w := range want {
		if !got[w] {
			var have []string
			for k := range got {
				if strings.Contains(k, "/tasks") || strings.Contains(k, "/compensation") {
					have = append(have, k)
				}
			}
			t.Errorf("路由表缺少 %q，现有任务/补偿路由: %v", w, have)
		}
	}
}

// TestTasksEndToEndWithFakeUpstream 端到端：用假上游驱动真实 taskops 编排，
// 验证「列表 → 一键完成（动作+回读+领奖）→ 清扫」整链路。
// 这是唯一能同时验证 admin 适配、taskops 编排与 upstream 协议的测试。
func TestTasksEndToEndWithFakeUpstream(t *testing.T) {
	// 缩短节流与回读轮询，避免测试拖到分钟级
	restoreThrottle := taskops.ThrottleTuning(-1, -1, -1, -1)
	t.Cleanup(restoreThrottle)
	restorePoll := upstream.TaskPollTuning(time.Millisecond, 3)
	t.Cleanup(restorePoll)

	var mu sync.Mutex
	accepted := map[string]bool{}
	claimed := map[string]bool{}
	claimedCredit := int64(0)
	progress := map[string]int64{"chat_5": 2} // 初始 2/5，动作补 3 条后达标

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.HasSuffix(p, "/tasks/accept"):
			var body struct {
				TaskCodes []string `json:"task_codes"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			for _, c := range body.TaskCodes {
				accepted[c] = true
			}
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		case strings.HasSuffix(p, "/claim"):
			code := p[strings.LastIndex(p, "/tasks/")+len("/tasks/") : strings.LastIndex(p, "/claim")]
			if claimed[code] {
				_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"already_claimed":true}}`))
				return
			}
			claimed[code] = true
			claimedCredit += 100
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"credit":100,"energy":5}}`))
		case strings.HasSuffix(p, "/tasks"):
			cur := progress["chat_5"]
			target := int64(5)
			if r.Header.Get("X-Client-Platform") == "miniprogram" {
				_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"tasks":[
				 {"task_code":"Sequential_Tasks_1","title":"小程序首对话","reward_credit":100,"target":1,"current":0}]}}`))
				return
			}
			_, _ = fmt.Fprintf(w, `{"code":0,"msg":"ok","data":{"tasks":[
			 {"task_code":"chat_5","title":"对话5次","reward_credit":100,"reward_energy":5,
			  "target":%d,"current":%d,"accept_status":"accepted"},
			 {"task_code":"automation_1","title":"定时任务","reward_credit":100,"target":1,"current":1}
			]}}`, target, cur)
		case strings.HasSuffix(p, "/v2/report"):
			// 行为上报：把 chat_5 进度推到达标（模拟服务端异步计分）
			progress["chat_5"] = 5
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		default:
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		}
	}))
	defer srv.Close()

	h := newTaskTestHandler(t, "cn", "", true)
	h.client.GrowthBaseOverride = srv.URL
	h.client.WebBaseOverride = srv.URL
	h.client.MPBaseOverride = srv.URL

	// ① 列表：chat_5 2/5 未达标，automation_1 已达标可领
	code, out := getReq(t, h.tasksList)
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("任务列表失败: code=%d out=%v", code, out)
	}
	tasks, _ := out["tasks"].([]any)
	if len(tasks) < 2 {
		t.Fatalf("应返回默认+mp 合并的任务，得到 %d", len(tasks))
	}

	// ② 一键完成 chat_5：动作（chat_5 需补报差额）→ 回读达标 → 自动领奖
	code, out = postJSONReq(t, h.tasksAuto, `{"task_code":"chat_5"}`)
	if code != http.StatusOK {
		t.Fatalf("一键完成失败: code=%d out=%v", code, out)
	}
	res, _ := out["result"].(map[string]any)
	if res == nil {
		t.Fatalf("应返回 result，得到 %v", out)
	}
	if res["status"] != "done" {
		t.Errorf("chat_5 应 done，得到 %v (%v)", res["status"], res["message"])
	}
	if res["claimed"] != true {
		t.Errorf("达标后应自动领奖，得到 %v (%v)", res["claimed"], res["message"])
	}
	if res["credit"] != float64(100) {
		t.Errorf("应领到 100 分，得到 %v", res["credit"])
	}
	if claimedCredit != 100 {
		t.Errorf("上游应记录 100 分领取，得到 %d", claimedCredit)
	}

	// ③ 已达标的 automation_1 走「补领」而非重跑动作
	code, out = postJSONReq(t, h.tasksAuto, `{"task_code":"automation_1"}`)
	if code != http.StatusOK {
		t.Fatalf("补领失败: code=%d", code)
	}
	res, _ = out["result"].(map[string]any)
	if res["status"] != "done" || res["claimed"] != true {
		t.Errorf("automation_1 应直接补领成功，得到 %v", res)
	}
	if claimedCredit != 200 {
		t.Errorf("两轮共应领 200 分，得到 %d", claimedCredit)
	}

	// ④ 再领一次同任务：幂等（already_claimed → 0 分）
	code, out = postJSONReq(t, h.tasksClaim, `{"task_code":"automation_1"}`)
	if code != http.StatusOK {
		t.Fatalf("重复领奖应 200: %d", code)
	}
	if out["already_claimed"] != true {
		t.Errorf("重复领奖应标记 already_claimed，得到 %v", out)
	}
	if claimedCredit != 200 {
		t.Errorf("重复领奖不应增加积分，得到 %d", claimedCredit)
	}

	// ⑤ 清扫：accept 未接受的任务（mp 任务走 mp 口径）
	before := len(accepted)
	code, out = postJSONReq(t, h.tasksSweep, "")
	if code != http.StatusOK {
		t.Fatalf("清扫失败: code=%d out=%v", code, out)
	}
	st, _ := out["stats"].(map[string]any)
	if st == nil || st["ran"] != true {
		t.Fatalf("清扫应 ran=true，得到 %v", out["stats"])
	}
	if len(accepted) <= before {
		t.Errorf("清扫应接受未接受的任务，accepted 从 %d 变为 %d", before, len(accepted))
	}
}

// ── 回归护栏 ─────────────────────────────────────────────────────────────────

// TestTasksAcceptCodeLimit 单次手动 accept 的任务码数量必须有上限：
// 缺省分支只提交列表里真实存在的任务（自然有界），显式分支是用户直接给的，
// 没有上限时一个请求可塞几千个码，各触发一次上游 accept 并长期占住全局锁。
func TestTasksAcceptCodeLimit(t *testing.T) {
	h := newTaskTestHandler(t, "cn", "", true)
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/tasks/accept") {
			atomic.AddInt32(&calls, 1)
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"tasks":[]}}`))
	}))
	defer srv.Close()
	h.client.GrowthBaseOverride, h.client.WebBaseOverride, h.client.MPBaseOverride = srv.URL, srv.URL, srv.URL

	codes := make([]string, 0, maxAcceptCodes+1)
	for i := 0; i <= maxAcceptCodes; i++ {
		codes = append(codes, fmt.Sprintf(`"t%d"`, i))
	}
	body := `{"task_codes":[` + strings.Join(codes, ",") + `]}`
	code, out := postJSONReq(t, h.tasksAccept, body)
	if code != http.StatusBadRequest {
		t.Errorf("超过上限应 400，得到 %d (%v)", code, out)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("被拒的请求不应发起任何上游 accept，实际 %d 次", got)
	}
}

// TestTasksClaimMPScopeByCode 领奖口径按任务码判定，不能凭动作表推断：
// 没有自动化动作的小程序任务（如 Expert_Philanthropy 之外的 mp 任务）也要走 mp 端点。
func TestTasksClaimMPScopeByCode(t *testing.T) {
	h := newTaskTestHandler(t, "cn", "", true)
	var platform []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/claim") {
			platform = append(platform, r.Header.Get("X-Client-Platform"))
			if r.Header.Get("X-Client-Platform") != "miniprogram" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"code":400,"msg":"task not found"}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"credit":100}}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"data":null}`))
	}))
	defer srv.Close()
	h.client.GrowthBaseOverride, h.client.WebBaseOverride, h.client.MPBaseOverride = srv.URL, srv.URL, srv.URL

	code, out := postJSONReq(t, h.tasksClaim, `{"task_code":"school_season"}`)
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("mp 任务领奖应成功，得到 %d (%v)", code, out)
	}
	if out["credit"] != float64(100) {
		t.Errorf("应领到 100 分，得到 %v", out["credit"])
	}
}

// runnerSched 实现了 Runner() 的假 scheduler，用于验证 admin 的复用路径。
type runnerSched struct {
	fakeSched
	r *taskops.Runner
}

func (s *runnerSched) Runner() *taskops.Runner { return s.r }

// TestAdminSharesSchedulerRunner admin 必须复用 scheduler 的执行器，
// 否则面板「一键完成全部」与夜猫子 cron 会并发跑两轮真实对话。
func TestAdminSharesSchedulerRunner(t *testing.T) {
	h := newTaskTestHandler(t, "cn", "", true)
	shared := taskops.NewRunner(h.client)

	// ① scheduler 暴露 Runner() → admin 必须拿到同一个实例
	got := runnerOrDefault(&runnerSched{r: shared}, h.client)
	if got != shared {
		t.Error("admin 应复用 scheduler 的执行器（同一实例才能共享互斥锁）")
	}

	// ② scheduler 未暴露（单测 / 旧实现）→ 退回自建，不能返回 nil
	if got := runnerOrDefault(&fakeSched{}, h.client); got == nil {
		t.Error("无 Runner() 时应退回自建实例，不能返回 nil")
	}
	if got := runnerOrDefault(nil, h.client); got == nil {
		t.Error("sched 为 nil 时应退回自建实例")
	}
}

// TestTasksFullFlowWithRefreshingFake token 过期 + mp 任务 + 锁定任务 + 动作失败，
// 四条最容易出错的路径在同一个假上游里串起来跑（端到端回归）。
func TestTasksFullFlowWithRefreshingFake(t *testing.T) {
	restoreThrottle := taskops.ThrottleTuning(-1, -1, -1, -1)
	t.Cleanup(restoreThrottle)
	restorePoll := upstream.TaskPollTuning(time.Millisecond, 2)
	t.Cleanup(restorePoll)

	var mu sync.Mutex
	var oldTokenHits int
	claimed := map[string]bool{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/v2/plugin/auth/token/refresh" {
			_, _ = w.Write([]byte(`{"code":0,"data":{"accessToken":"NEW","expiresIn":3600,"refreshToken":"r2"}}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer NEW" {
			oldTokenHits++
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":401,"msg":"expired"}`))
			return
		}
		switch {
		case strings.EqualFold(r.Header.Get("X-Client-Platform"), "miniprogram") &&
			strings.HasSuffix(r.URL.Path, "/tasks"):
			// mp 列表同样下发这些任务（含常规任务）—— 验证口径按码判定
			_, _ = w.Write([]byte(`{"code":0,"data":{"tasks":[
			  {"task_code":"Sequential_Tasks_1","target":1,"current":1,"accept_status":"accepted"},
			  {"task_code":"Sequential_Tasks_3","target":5,"current":5,"locked":true}]}}`))
		case strings.HasSuffix(r.URL.Path, "/tasks"):
			_, _ = w.Write([]byte(`{"code":0,"data":{"tasks":[
			  {"task_code":"chat_5","target":5,"current":0},
			  {"task_code":"Sequential_Tasks_1","target":1,"current":1,"accept_status":"accepted"},
			  {"task_code":"Sequential_Tasks_3","target":5,"current":5,"locked":true}]}}`))
		case strings.HasSuffix(r.URL.Path, "/claim"):
			code := r.URL.Path[strings.LastIndex(r.URL.Path, "/tasks/")+len("/tasks/") : strings.LastIndex(r.URL.Path, "/claim")]
			mpCode := strings.HasPrefix(code, "Sequential_Tasks")
			if mpCode && r.Header.Get("X-Client-Platform") != "miniprogram" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"code":400,"msg":"task not found"}`))
				return
			}
			if claimed[code] {
				_, _ = w.Write([]byte(`{"code":0,"data":{"already_claimed":true}}`))
				return
			}
			claimed[code] = true
			_, _ = w.Write([]byte(`{"code":0,"data":{"credit":100,"energy":5}}`))
		case r.URL.Path == "/v2/report":
			w.WriteHeader(http.StatusInternalServerError) // 动作失败必须如实上报
			_, _ = w.Write([]byte(`{"code":500,"msg":"report rejected"}`))
		default:
			_, _ = w.Write([]byte(`{"code":0,"data":null}`))
		}
	}))
	defer srv.Close()

	h := newTaskTestHandler(t, "cn", "", true)
	h.client.GrowthBaseOverride, h.client.WebBaseOverride, h.client.MPBaseOverride = srv.URL, srv.URL, srv.URL
	h.client.AuthBaseOverride = srv.URL // 刷新也指向假上游
	// 换成「已过期 + 带 refresh_token」的凭证，逼出刷新重试路径
	old := h.toks.Get()
	if err := h.toks.Save(&auth.Token{
		UID: old.UID, AccessToken: "OLD", RefreshToken: "r",
		TokenType: "Bearer", ExpiresAt: time.Now().Add(-time.Hour).Unix(),
	}); err != nil {
		t.Fatal(err)
	}

	// ① 清扫：mp 任务必须走 mp 端点领到；locked 任务不得尝试
	st := h.RunTaskSweep()
	if st.Claimed != 1 {
		t.Errorf("应领到 1 项（Sequential_Tasks_1 走 mp 端点），得到 claimed=%d errors=%d reason=%q",
			st.Claimed, st.Errors, st.Reason)
	}

	// ② 单任务一键完成：上报 500 → status 必须是 error
	restore := upstream.TaskPollTuning(time.Millisecond, 1)
	defer restore()
	res, err := h.runner().Auto("chat_5")
	if err != nil {
		t.Fatalf("Auto: %v", err)
	}
	if res.Status != "error" {
		t.Errorf("上报全部失败时 status 应为 error，得到 %q（%s）", res.Status, res.Message)
	}
	if res.Claimed {
		t.Error("上报失败不应领奖")
	}

	// ③ 全程不应残留旧 token 请求（除首次 401 的探测外，Retry 必须带新 token）
	if oldTokenHits > 2 {
		t.Errorf("旧 token 被使用 %d 次，刷新重试未生效", oldTokenHits)
	}
}

// TestTasksListIncludesLastResults 列表响应要带上「上一轮清扫/队列」的落库结果，
// 否则「一键完成超时后台继续」跑完的结果在刷新后无处可查。
func TestTasksListIncludesLastResults(t *testing.T) {
	h := newTaskTestHandler(t, "cn", "", true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"tasks":[]}}`))
	}))
	defer srv.Close()
	h.client.GrowthBaseOverride, h.client.WebBaseOverride, h.client.MPBaseOverride = srv.URL, srv.URL, srv.URL

	// 未扫过：字段应为 null（前端按空态处理）
	_, out := getReq(t, h.tasksList)
	if out["last_sweep"] != nil {
		t.Errorf("未清扫时 last_sweep 应为 nil，得到 %v", out["last_sweep"])
	}

	// 落一轮后应能读回
	h.recordSweep(taskops.SweepStats{Ran: true, At: 1000, Accepted: 2, Claimed: 3, Credits: 300})
	_, out = getReq(t, h.tasksList)
	ls, ok := out["last_sweep"].(map[string]any)
	if !ok {
		t.Fatalf("落库后 last_sweep 应可读，得到 %v", out["last_sweep"])
	}
	if ls["claimed"] != float64(3) || ls["credits"] != float64(300) {
		t.Errorf("last_sweep 内容不符: %v", ls)
	}
}

// TestSweepRecordedOnManualTrigger 手动清扫按钮也要落库（不能只有 ticker 落）。
func TestSweepRecordedOnManualTrigger(t *testing.T) {
	h := newTaskTestHandler(t, "cn", "", true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/claim") {
			_, _ = w.Write([]byte(`{"code":0,"data":{"credit":100}}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/tasks") {
			_, _ = w.Write([]byte(`{"code":0,"data":{"tasks":[
			  {"task_code":"automation_1","target":1,"current":1}]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"data":null}`))
	}))
	defer srv.Close()
	h.client.GrowthBaseOverride, h.client.WebBaseOverride, h.client.MPBaseOverride = srv.URL, srv.URL, srv.URL

	if _, _, ok := h.st.GetCache(taskCacheTable, taskKeyLastSweep, 3600); ok {
		t.Fatal("清扫前不应有落库")
	}
	code, out := postJSONReq(t, h.tasksSweep, "")
	if code != http.StatusOK {
		t.Fatalf("清扫失败 %d %v", code, out)
	}
	raw, _, ok := h.st.GetCache(taskCacheTable, taskKeyLastSweep, 3600)
	if !ok || raw == "" {
		t.Fatal("手动清扫后应落库")
	}
	var st taskops.SweepStats
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		t.Fatalf("落库内容应为 SweepStats: %v", err)
	}
	if st.Claimed != 1 {
		t.Errorf("应记录领到 1 项，得到 %d", st.Claimed)
	}
}
