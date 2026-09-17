package upstream

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"buddy2api-go/internal/auth"
)

// ── 纯逻辑：EligibleTier / Claimed ────────────────────────────────────────────

func tiersDefault() []GrowthTierSpec {
	return []GrowthTierSpec{
		{Tier: "7d", Days: 7, Credit: 80, Energy: 5, Cards: 1, Chances: 1},
		{Tier: "14d", Days: 14, Credit: 150, Energy: 10, Cards: 2, Chances: 2},
		{Tier: "28d", Days: 28, Credit: 400, Energy: 20, Cards: 3, Chances: 3},
	}
}

func TestEligibleTier(t *testing.T) {
	mk := func(days int, s7, s14, s28 string) *GrowthStreakInfo {
		g := &GrowthStreakInfo{}
		g.Streak.Days = days
		g.Redemption.Tiers = tiersDefault()
		g.Redemption.Tier7dStatus, g.Redemption.Tier14dStatus, g.Redemption.Tier28dStatus = s7, s14, s28
		return g
	}
	cases := []struct {
		name string
		g    *GrowthStreakInfo
		want string
	}{
		{"全 locked", mk(4, "locked", "locked", "locked"), ""},
		{"7d 达标可领", mk(7, "available", "locked", "locked"), "7d"},
		{"跨档 14d 可领 28d 锁", mk(20, "claimed", "available", "locked"), "14d"},
		{"28d 达标取最高", mk(28, "claimed", "claimed", "available"), "28d"},
		{"全 claimed", mk(28, "claimed", "claimed", "claimed"), ""},
		{"高档可领时不取低档", mk(28, "available", "available", "available"), "28d"},
		{"tiers 空", func() *GrowthStreakInfo {
			g := mk(10, "available", "available", "available")
			g.Redemption.Tiers = nil
			return g
		}(), ""},
		{"乱序数组（28d 在前）", func() *GrowthStreakInfo {
			g := mk(20, "available", "available", "available")
			g.Redemption.Tiers = []GrowthTierSpec{
				{Tier: "28d", Days: 28}, {Tier: "14d", Days: 14}, {Tier: "7d", Days: 7},
			}
			return g
		}(), "14d"},
	}
	for _, c := range cases {
		if got := c.g.EligibleTier(); got != c.want {
			t.Errorf("%s: EligibleTier=%q, 期望 %q", c.name, got, c.want)
		}
	}
}

func TestClaimed(t *testing.T) {
	g := &GrowthStreakInfo{}
	g.Redemption.Tier7dStatus = "claimed"
	g.Redemption.Tier14dStatus = "available"
	g.Redemption.Tier28dStatus = "locked"
	if !g.Claimed("7d") {
		t.Error("7d 应为已领")
	}
	if g.Claimed("14d") || g.Claimed("28d") {
		t.Error("14d/28d 不应为已领")
	}
	if g.Claimed("unknown") || g.Claimed("") {
		t.Error("未知档位应返回 false")
	}
}

// ── 纯逻辑：HeatmapDayScore ───────────────────────────────────────────────────

func TestHeatmapDayScore(t *testing.T) {
	cells := []HeatmapCell{
		{Date: "2026-01-15", Score: 1},
		{Date: "2026-01-16 00:00:00", Score: 0}, // 带时间后缀：比较取前 10 位
	}
	if s, ok := HeatmapDayScore(cells, "2026-01-15"); !ok || s != 1 {
		t.Errorf("命中格应返回 (1,true)，得到 (%v,%v)", s, ok)
	}
	if s, ok := HeatmapDayScore(cells, "2026-01-16"); !ok || s != 0 {
		t.Errorf("带时间后缀的格应取前 10 位命中，得到 (%v,%v)", s, ok)
	}
	if _, ok := HeatmapDayScore(cells, "2026-01-17"); ok {
		t.Error("无该日格应返回 ok=false")
	}
}

// ── 纯逻辑：GrowthYesterdayDate（CST 口径，不依赖本地时区）────────────────────

func TestGrowthYesterdayDate(t *testing.T) {
	// UTC 16:00 = CST 次日 00:00（跨日边界）
	now := time.Date(2026, 1, 16, 16, 0, 0, 0, time.UTC)
	if got := GrowthYesterdayDate(now); got != "2026-01-16" {
		t.Errorf("UTC 16:00（CST 0:00）昨日应为 2026-01-16，得到 %q", got)
	}
	// 边界前一分钟：UTC 15:59 = CST 23:59 → 昨日为前一日
	now = time.Date(2026, 1, 16, 15, 59, 0, 0, time.UTC)
	if got := GrowthYesterdayDate(now); got != "2026-01-15" {
		t.Errorf("UTC 15:59（CST 23:59）昨日应为 2026-01-15，得到 %q", got)
	}
	// 验证不依赖本地时区：切换 time.Local 后结果不变
	orig := time.Local
	if loc, err := time.LoadLocation("America/New_York"); err == nil {
		time.Local = loc
		defer func() { time.Local = orig }()
		now = time.Date(2026, 1, 16, 16, 0, 0, 0, time.UTC)
		if got := GrowthYesterdayDate(now); got != "2026-01-16" {
			t.Errorf("切本地时区后昨日应不变，得到 %q", got)
		}
	}
}

// ── 纯逻辑：错误分类器 ─────────────────────────────────────────────────────────

func TestErrorClassifiers(t *testing.T) {
	mk := func(status int, msg string) error {
		return &GrowthError{Status: status, Msg: msg}
	}
	cases := []struct {
		name string
		fn   func(error) bool
		err  error
		want bool
	}{
		{"redeem 409 duplicate 命中", IsRedeemAlreadyClaimed, mk(409, "duplicate redemption"), true},
		{"redeem 409 已领取 命中", IsRedeemAlreadyClaimed, mk(409, "本月该档已领取"), true},
		{"redeem 500 不命中", IsRedeemAlreadyClaimed, mk(500, "duplicate"), false},
		{"redeem 403 marker 不匹配", IsRedeemNotEnoughDays, mk(403, "other reason"), false},
		{"redeem 403 连续登录天数不足 命中", IsRedeemNotEnoughDays, mk(403, "连续登录天数不足"), true},
		{"redeem 403 状态不匹配", IsRedeemNotEnoughDays, mk(400, "连续登录天数不足"), false},
		{"lottery 400 insufficient 命中", IsLotteryNoChance, mk(400, "Insufficient Lottery Chance Balance"), true},
		{"lottery 400 disabled 命中", IsLotteryDisabled, mk(400, "lottery disabled"), true},
		{"lottery 400 不匹配 disabled", IsLotteryDisabled, mk(400, "insufficient lottery chance balance"), false},
		{"buddy 400 first_buddy 命中", IsBuddyTaskIncomplete, mk(400, "first_buddy task not completed yet"), true},
		{"buddy 500 不命中", IsBuddyTaskIncomplete, mk(500, "first_buddy task not completed yet"), false},
		{"业务错误（Status=200 Code!=0）不命中任何正常态", IsRedeemAlreadyClaimed, mk(200, "duplicate"), false},
		{"非 GrowthError", IsRedeemAlreadyClaimed, errors.New("network refused"), false},
		{"nil", IsRedeemAlreadyClaimed, nil, false},
	}
	for _, c := range cases {
		if got := c.fn(c.err); got != c.want {
			t.Errorf("%s: 得到 %v, 期望 %v", c.name, got, c.want)
		}
	}
	// 业务错误（Status=200 + Code!=0）也应不被误判为正常态
	if IsRedeemAlreadyClaimed(&GrowthError{Code: 1001, Msg: "duplicate"}) {
		t.Error("业务错误不应命中正常态分类器（Status=200）")
	}
}

func TestGrowthErrorText(t *testing.T) {
	if got := (&GrowthError{Status: 409, Msg: "dup"}).Error(); got != "growth HTTP 409: dup" {
		t.Errorf("HTTP 错误文案异常: %q", got)
	}
	if got := (&GrowthError{Code: 1001, Msg: "bad"}).Error(); got != "growth code=1001: bad" {
		t.Errorf("业务错误文案异常: %q", got)
	}
}

// ── 纯逻辑：chatRequestEvent 序列化（34 字段 + userId + 数组体）────────────────

func TestChatRequestEventShape(t *testing.T) {
	var captured []chatRequestEvent
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v2/report" {
			t.Errorf("请求行异常: %s %s", r.Method, r.URL.Path)
		}
		var body json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&body)
		if !strings.HasPrefix(string(body), "[") || !strings.HasSuffix(string(body), "]") {
			t.Errorf("上报 body 应为数组，得到 %s", string(body))
		}
		_ = json.Unmarshal(body, &captured)
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
	}))
	defer srv.Close()

	client, cleanup := testClient(srv.URL)
	defer cleanup()
	if err := client.ReportChatActivity("b2a-1000", "b2a-1000-r1"); err != nil {
		t.Fatal("ReportChatActivity 失败:", err)
	}
	if len(captured) != 1 {
		t.Fatalf("上报应为单元素数组，得到 %d 条", len(captured))
	}
	ev := captured[0]
	if ev.UserID != "test-uid" {
		t.Errorf("userId 应=token UID，得到 %q", ev.UserID)
	}
	if ev.EventCode != "chat_request_send" || ev.Mode != "craft" || ev.ConversationID != "b2a-1000" {
		t.Errorf("事件核心字段异常: %+v", ev)
	}
	if ev.RequestID != "b2a-1000-r1" {
		t.Errorf("requestId 应独立，得到 %q", ev.RequestID)
	}
	if ev.RootRequestID != "b2a-1000" || ev.ParentConversationID != "b2a-1000" {
		t.Errorf("root/parent 应=conversationId: %q / %q", ev.RootRequestID, ev.ParentConversationID)
	}
	if ev.Timestamp != ev.PresentAt || ev.Timestamp == 0 {
		t.Errorf("presentAt 应同 timestamp: %d vs %d", ev.PresentAt, ev.Timestamp)
	}
	// 事件体全量字段校验（防上游加严）：完整形状应为 36 个 JSON 键
	raw, _ := json.Marshal(ev)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if len(m) != 36 {
		t.Errorf("事件应含 36 个 JSON 键，得到 %d：%v", len(m), m)
	}
}

// requestID 空时回落 conversationID。
func TestReportChatActivityDefaultRequestID(t *testing.T) {
	var captured []chatRequestEvent
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
	}))
	defer srv.Close()
	client, cleanup := testClient(srv.URL)
	defer cleanup()
	if err := client.ReportChatActivity("b2a-2000", ""); err != nil {
		t.Fatal(err)
	}
	if len(captured) != 1 || captured[0].RequestID != "b2a-2000" {
		t.Errorf("空 requestID 应回落 conversationID，得到 %+v", captured)
	}
}

// ── growthCall 传输语义（httptest）─────────────────────────────────────────────

func TestGrowthCallEnvelope(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		respBody   string
		wantData   string
		wantErr    bool
		wantStatus int // GrowthError.Status
		wantCode   int // GrowthError.Code
	}{
		{"200+code0", 200, `{"code":0,"msg":"ok","data":{"days":5}}`, `{"days":5}`, false, 0, 0},
		{"200+code非0", 200, `{"code":1001,"msg":"biz","data":null}`, "", true, 200, 1001},
		{"404", 404, `{"code":0,"msg":"not found"}`, "", true, 404, 0},
		{"500", 500, `oops`, "", true, 500, 0},
		{"信封缺data", 200, `{"code":0,"msg":"ok"}`, "null", false, 0, 0},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.status)
			_, _ = w.Write([]byte(c.respBody))
		}))
		client, cleanup := testClient(srv.URL)
		data, err := client.GrowthStreakInfo()
		if c.wantErr {
			var ge *GrowthError
			if !errors.As(err, &ge) {
				t.Errorf("%s: 应返回 *GrowthError，得到 %v", c.name, err)
			} else if ge.Status != c.wantStatus || ge.Code != c.wantCode {
				t.Errorf("%s: GrowthError{Status:%d Code:%d}，期望 {Status:%d Code:%d}",
					c.name, ge.Status, ge.Code, c.wantStatus, c.wantCode)
			}
			if data != nil {
				t.Errorf("%s: 出错时不应返回数据", c.name)
			}
		} else {
			if err != nil {
				t.Errorf("%s: 不应报错: %v", c.name, err)
				continue
			}
			// 信封缺 data 时 Unmarshal("null") 到 struct 不报错、零值——接受
		}
		cleanup()
		srv.Close()
	}
}

// growthCall 对 2xx 但非 JSON 响应体（反代错误页等）必须报错，且不包装为
// *GrowthError——否则会被正常态分类器误判、写操作被静默记为假成功。
func TestGrowthCallNonJSONBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html>502 Bad Gateway</html>"))
	}))
	client, cleanup := testClient(srv.URL)
	defer cleanup()
	defer srv.Close()
	_, err := client.GrowthStreakInfo()
	if err == nil {
		t.Fatal("200 + 非 JSON 响应体应报错")
	}
	var ge *GrowthError
	if errors.As(err, &ge) {
		t.Errorf("非 JSON 解析错误不应包装为 GrowthError: %v", err)
	}
	if !strings.Contains(err.Error(), "非 JSON") {
		t.Errorf("错误文案应说明非 JSON: %v", err)
	}
	// BuddyInfo 同样不得把非 JSON 200 当成「无猫」成功
	if buddy, err := client.BuddyInfo(); err == nil || buddy != nil {
		t.Errorf("非 JSON 响应下 BuddyInfo 应报错，得到 buddy=%v err=%v", buddy, err)
	}
}

// claim-gift 路径回退链：首路径 404 → 二路径 200；双 404 → 报错。
func TestClaimGiftPathFallback(t *testing.T) {
	// 1) 首路径 404 → 二路径 200
	var hitPaths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitPaths = append(hitPaths, r.URL.Path)
		if r.URL.Path == "/billing/meter/claim-gift" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":0,"msg":"no route"}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"credit":50}}`))
	}))
	client, cleanup := testClient(srv.URL)
	defer cleanup()
	credit, err := client.ClaimGift()
	if err != nil || credit != 50 {
		t.Fatalf("路径回退失败: credit=%d err=%v", credit, err)
	}
	if len(hitPaths) != 2 || hitPaths[1] != "/v2/billing/meter/claim-gift" {
		t.Errorf("应回退到 /v2 前缀路径，实际命中 %v", hitPaths)
	}

	// 2) 双 404 → 报错（非业务错误）
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":0,"msg":"no route"}`))
	}))
	client2, cleanup2 := testClient(srv2.URL)
	defer cleanup2()
	if _, err := client2.ClaimCompensation(); err == nil {
		t.Error("双 404 应报错")
	}

	// 3) 业务错误（已领过）原样返回，不走回退
	srv3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":1001,"msg":"already claimed"}`))
	}))
	client3, cleanup3 := testClient(srv3.URL)
	defer cleanup3()
	_, err = client3.ClaimGift()
	var ge *GrowthError
	if !errors.As(err, &ge) || ge.Code != 1001 {
		t.Errorf("业务错误应原样返回（Code=1001），得到 %v", err)
	}
}

// GrowthStreakInfo / BuddyInfo / TravelState 解析（httptest）。
func TestGrowthParsers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/activity/growth/streak":
			_, _ = w.Write([]byte(`{"code":0,"data":{"streak":{"days":5},"makeup_cards":{"balance":1,"max":3},
				"redemption_status":{"tier_7d_status":"claimed","tier_14d_status":"available","tier_28d_status":"locked",
				"tiers":[{"tier":"7d","days":7,"credit":80,"energy":5,"cards":1,"chances":1}],
				"remaining_days":2}}}`))
		case "/activity/growth/buddy/info":
			_, _ = w.Write([]byte(`{"code":0,"data":{"buddy":{"id":1,"name":"喵喵"}}}`))
		case "/activity/growth/buddy/travel/status":
			_, _ = w.Write([]byte(`{"code":0,"data":{"state":"arrived","daily_limit_reached":false,"record_id":12,"reward_credit":30}}`))
		case "/activity/growth/lottery/chances":
			_, _ = w.Write([]byte(`{"code":0,"data":{"balance":2}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	client, cleanup := testClient(srv.URL)
	defer cleanup()

	info, err := client.GrowthStreakInfo()
	if err != nil {
		t.Fatal(err)
	}
	if info.Streak.Days != 5 || info.MakeupCards.Balance != 1 || info.MakeupCards.Max != 3 {
		t.Errorf("streak 快照解析异常: %+v", info)
	}
	if !info.Claimed("7d") || info.Claimed("14d") || info.Claimed("28d") {
		t.Error("档位状态映射异常")
	}
	if info.Redemption.RemainingDays != 2 {
		t.Errorf("remaining_days 解析异常: %d", info.Redemption.RemainingDays)
	}

	buddy, err := client.BuddyInfo()
	if err != nil || buddy == nil || buddy.Name != "喵喵" || buddy.ID != 1 {
		t.Errorf("buddy 解析异常: %+v err=%v", buddy, err)
	}

	ts, err := client.TravelStatus()
	if err != nil || ts.State != "arrived" || ts.RecordID != 12 || ts.RewardCredit != 30 {
		t.Errorf("travel 解析异常: %+v err=%v", ts, err)
	}

	chances, err := client.GrowthLotteryChances()
	if err != nil || chances != 2 {
		t.Errorf("chances 解析异常: %d err=%v", chances, err)
	}
}

// BuddyInfo 无猫形态：null / 缺失 / 空对象 → (nil,nil)。
func TestBuddyInfoNoBuddy(t *testing.T) {
	for _, body := range []string{
		`{"code":0,"data":{"buddy":null}}`,
		`{"code":0,"data":{}}`,
		`{"code":0,"data":{"buddy":{}}}`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(body))
		}))
		client, cleanup := testClient(srv.URL)
		buddy, err := client.BuddyInfo()
		if err != nil || buddy != nil {
			t.Errorf("body=%s 应判无猫，得到 buddy=%v err=%v", body, buddy, err)
		}
		cleanup()
		srv.Close()
	}
}

// redeem / draw 的 client_token 空时自动生成且每次不同。
func TestClientTokenAutoGen(t *testing.T) {
	var tokens []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Tier        string `json:"tier"`
			ClientToken string `json:"client_token"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		tokens = append(tokens, req.ClientToken)
		_, _ = w.Write([]byte(`{"code":0,"data":{"credit_granted":80}}`))
	}))
	client, cleanup := testClient(srv.URL)
	defer cleanup()
	for i := 0; i < 2; i++ {
		if _, err := client.GrowthRedeem("7d", ""); err != nil {
			t.Fatal(err)
		}
	}
	if len(tokens) != 2 || tokens[0] == "" || tokens[0] == tokens[1] {
		t.Errorf("client_token 应每次新生成且非空，得到 %v", tokens)
	}
	if !strings.HasPrefix(tokens[0], "redeem-7d-") {
		t.Errorf("client_token 前缀异常: %q", tokens[0])
	}
	if _, err := client.GrowthLotteryDraw(""); err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 3 || !strings.HasPrefix(tokens[2], "draw-") {
		t.Errorf("draw client_token 异常: %v", tokens)
	}
}

// 未登录（无 token）时 growth 方法返回 ErrNoToken，不发请求。
func TestGrowthNoToken(t *testing.T) {
	toks, err := auth.NewTokenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	client := New(toks, func() string { return "cn" }, 60)
	if _, err := client.GrowthStreakInfo(); !errors.Is(err, ErrNoToken) {
		t.Errorf("未登录应返回 ErrNoToken，得到 %v", err)
	}
	if err := client.ReportChatActivity("cid", "rid"); !errors.Is(err, ErrNoToken) {
		t.Errorf("未登录上报应返回 ErrNoToken，得到 %v", err)
	}
}

// ── 测试基建 ───────────────────────────────────────────────────────────────────

// testClient 构造指向 base 的测试客户端（growthBase 覆盖为 base），返回清理函数还原。
func testClient(base string) (*Client, func()) {
	dir, err := os.MkdirTemp("", "growth-test-*")
	if err != nil {
		panic(err)
	}
	toks, err := auth.NewTokenStore(dir)
	if err != nil {
		panic(err)
	}
	if err := toks.Save(&auth.Token{
		AccessToken: "test-access-token",
		TokenType:   "Bearer",
		Domain:      "www.codebuddy.cn",
		UID:         "test-uid",
		ExpiresAt:   time.Now().Add(1 * time.Hour).Unix(),
	}); err != nil {
		panic(err)
	}
	orig := growthBase
	growthBase = func(*Client) string { return base }
	client := New(toks, func() string { return "cn" }, 60)
	return client, func() { growthBase = orig }
}
