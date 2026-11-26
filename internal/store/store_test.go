package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// TestGetStatsDailyTokensAndCredit 验证近 14 天 daily 聚合同时返回 requests/tokens/credit。
func TestGetStatsDailyTokensAndCredit(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	defer s.Close()

	now := time.Now()
	mk := func(daysAgo int, tokens int, credit float64) *LogEntry {
		return &LogEntry{
			Model: "glm-5.3", StatusCode: 200, FinishReason: "stop",
			TotalTokens: tokens, Credit: credit,
			CreatedAt: now.AddDate(0, 0, -daysAgo).Unix(),
		}
	}
	// 同一天两条 + 前一天一条
	for _, l := range []*LogEntry{mk(1, 300, 0.79), mk(1, 200, 0.52), mk(2, 999, 1.58)} {
		if err := s.InsertLog(l); err != nil {
			t.Fatalf("插入日志失败: %v", err)
		}
	}

	st, err := s.GetStats()
	if err != nil {
		t.Fatalf("GetStats 失败: %v", err)
	}
	if len(st.Daily) < 2 {
		t.Fatalf("daily 条数 = %d, want >= 2", len(st.Daily))
	}

	type day struct {
		req    int64
		tok    int64
		credit float64
	}
	got := map[string]day{}
	for _, d := range st.Daily {
		got[d.Date] = day{d.Requests, d.Tokens, d.Credit}
	}

	find := func(tokens int64, credit float64) bool {
		for _, d := range got {
			if d.tok == tokens && d.credit == credit {
				return true
			}
		}
		return false
	}
	if !find(500, 1.31) { // 同日两条聚合：300+200 tokens，0.79+0.52 credit
		t.Errorf("未找到 tokens=500/credit=1.31 的聚合行, got %+v", got)
	}
	if !find(999, 1.58) {
		t.Errorf("未找到 tokens=999/credit=1.58 的聚合行, got %+v", got)
	}
	for date, d := range got {
		if d.req != 2 && d.req != 1 {
			t.Errorf("%s 请求数 = %d, want 1 或 2", date, d.req)
		}
	}
	fmt.Println("daily:", got) // 调试输出便于失败时定位
}

// TestGetStatsByModelNoTruncate 校验模型数超过原截断值时仍全量返回，且按请求数降序。
func TestGetStatsByModelNoTruncate(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	defer s.Close()

	const n = 12 // 大于原查询的 LIMIT 10
	for i := 0; i < n; i++ {
		model := fmt.Sprintf("model-%02d", i)
		// 请求数递减，model-00 最多，便于同时校验排序
		for j := 0; j <= n-i; j++ {
			if err := s.InsertLog(&LogEntry{Model: model, StatusCode: 200, TotalTokens: 10}); err != nil {
				t.Fatalf("插入日志失败: %v", err)
			}
		}
	}

	st, err := s.GetStats()
	if err != nil {
		t.Fatalf("GetStats 失败: %v", err)
	}
	if len(st.ByModel) != n {
		t.Fatalf("ByModel 条数 = %d, want %d（截断回归？）", len(st.ByModel), n)
	}
	if st.ByModel[0].Model != "model-00" {
		t.Errorf("首行模型 = %q, want model-00（应按请求数降序）", st.ByModel[0].Model)
	}
	for i := 1; i < len(st.ByModel); i++ {
		if st.ByModel[i-1].Requests < st.ByModel[i].Requests {
			t.Errorf("第 %d/%d 行未降序: %d < %d", i, i+1, st.ByModel[i-1].Requests, st.ByModel[i].Requests)
		}
	}
}

// TestGetStatsRanges 验证五档时间范围（all / 24h / 7d / 30d / 90d）的总量、模型分布、
// 近期错误、趋势分桶粒度与零值桶补齐，以及非法范围拒绝。
func TestGetStatsRanges(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	defer s.Close()

	// 固定本地参考时刻，避免跨越午夜或整点导致用例抖动
	now := time.Date(2026, 6, 15, 14, 30, 0, 0, time.Local)
	logs := []*LogEntry{
		// 24h 内（也是今日）：成功 1 条 + 失败 1 条
		{Model: "m-24h", StatusCode: 200, TotalTokens: 100, Credit: 0.5, CreatedAt: now.Add(-2 * time.Hour).Unix()},
		{Model: "m-24h", StatusCode: 500, ErrorMsg: "err-24h", TotalTokens: 50, Credit: 0.2, CreatedAt: now.Add(-10 * time.Hour).Unix()},
		// 3 天前（落入 7d / 30d / 90d / all，不入 24h）
		{Model: "m-7d", StatusCode: 200, TotalTokens: 200, Credit: 1.0, CreatedAt: now.AddDate(0, 0, -3).Unix()},
		// 20 天前（落入 30d / 90d / all，不入 7d）
		{Model: "m-30d", StatusCode: 429, ErrorMsg: "err-30d", TotalTokens: 300, Credit: 1.5, CreatedAt: now.AddDate(0, 0, -20).Unix()},
		// 60 天前（落入 90d / all，不入 30d）
		{Model: "m-90d", StatusCode: 200, TotalTokens: 400, Credit: 2.0, CreatedAt: now.AddDate(0, 0, -60).Unix()},
		// 120 天前（仅落入 all）
		{Model: "m-old", StatusCode: 502, ErrorMsg: "err-old", TotalTokens: 500, Credit: 2.5, CreatedAt: now.AddDate(0, 0, -120).Unix()},
	}
	for _, l := range logs {
		if err := s.InsertLog(l); err != nil {
			t.Fatalf("插入日志失败: %v", err)
		}
	}

	cases := []struct {
		rng             string
		wantGranularity string
		wantReq         int64
		wantTokens      int64
		wantErrs        int64
		wantModels      int
		wantRecentErrs  int
		wantBuckets     int // 0 表示不要求固定桶数（all 只返回有数据日期）
	}{
		{StatsRange24h, "hour", 2, 150, 1, 1, 1, 24},
		{StatsRange7d, "day", 3, 350, 1, 2, 1, 7},
		{StatsRange30d, "day", 4, 650, 2, 3, 2, 30},
		{StatsRange90d, "day", 5, 1050, 2, 4, 2, 90},
		{StatsRangeAll, "day", 6, 1550, 3, 5, 3, 0},
	}
	for _, tc := range cases {
		st, err := s.getStatsAt(tc.rng, now)
		if err != nil {
			t.Fatalf("getStatsAt(%q) 失败: %v", tc.rng, err)
		}
		if st.Range != tc.rng || st.Granularity != tc.wantGranularity {
			t.Errorf("%s: range=%q granularity=%q，期望 %q/%q", tc.rng, st.Range, st.Granularity, tc.rng, tc.wantGranularity)
		}
		if st.TotalRequests != tc.wantReq || st.TotalTokens != tc.wantTokens || st.ErrorCount != tc.wantErrs {
			t.Errorf("%s: req=%d tok=%d err=%d，期望 %d/%d/%d",
				tc.rng, st.TotalRequests, st.TotalTokens, st.ErrorCount, tc.wantReq, tc.wantTokens, tc.wantErrs)
		}
		if len(st.ByModel) != tc.wantModels {
			t.Errorf("%s: by_model=%d，期望 %d", tc.rng, len(st.ByModel), tc.wantModels)
		}
		if len(st.RecentErrors) != tc.wantRecentErrs {
			t.Errorf("%s: recent_errors=%d，期望 %d", tc.rng, len(st.RecentErrors), tc.wantRecentErrs)
		}
		if tc.wantBuckets > 0 && len(st.Daily) != tc.wantBuckets {
			t.Errorf("%s: daily 桶数=%d，期望 %d", tc.rng, len(st.Daily), tc.wantBuckets)
		}
		// 各桶之和必须与范围总量一致（不得丢数据或重复统计）
		var sumReq, sumTok int64
		for _, d := range st.Daily {
			sumReq += d.Requests
			sumTok += d.Tokens
		}
		if sumReq != st.TotalRequests || sumTok != st.TotalTokens {
			t.Errorf("%s: 趋势之和(%d/%d) != 范围总量(%d/%d)", tc.rng, sumReq, sumTok, st.TotalRequests, st.TotalTokens)
		}
		// 今日统计恒为 2 条（与范围无关）
		if st.TodayRequests != 2 || st.TodayTokens != 150 {
			t.Errorf("%s: today=%d/%d，期望 2/150", tc.rng, st.TodayRequests, st.TodayTokens)
		}
	}

	// 空范围回退 all
	if st, err := s.getStatsAt("", now); err != nil || st.Range != StatsRangeAll || st.TotalRequests != 6 {
		t.Errorf("空范围应回退 all: st=%+v err=%v", st, err)
	}
	// 非法范围报错
	if _, err := s.getStatsAt("14d", now); err == nil {
		t.Error("非法范围 14d 应返回错误")
	}
	// 无数据时固定窗口不补零（前端据空切片显示空态）
	emptyNow := now.AddDate(1, 0, 0)
	if st, err := s.getStatsAt(StatsRange24h, emptyNow); err != nil || len(st.Daily) != 0 {
		t.Errorf("无数据时 24h daily 应为空，实际 len=%d err=%v", len(st.Daily), err)
	}
}

// TestCacheDeleteKey 验证缓存行的写入、读取与指定 key 删除（幂等、不影响其他 key、非法表拒绝）。
// 用于换缓存 key 后清理遗留旧行，避免永久残留。
func TestCacheDeleteKey(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	defer s.Close()

	if err := s.SetCache("resource_cache", "default", "v1-payload"); err != nil {
		t.Fatalf("写入旧 key 失败: %v", err)
	}
	if err := s.SetCache("resource_cache", "default_v2", "v2-payload"); err != nil {
		t.Fatalf("写入新 key 失败: %v", err)
	}

	if err := s.DeleteCacheKey("resource_cache", "default"); err != nil {
		t.Fatalf("删除旧 key 失败: %v", err)
	}
	// 幂等：重复删除不报错
	if err := s.DeleteCacheKey("resource_cache", "default"); err != nil {
		t.Fatalf("重复删除应幂等: %v", err)
	}
	// 旧 key 已删，新 key 不受影响
	if _, _, ok := s.GetCache("resource_cache", "default", 60); ok {
		t.Errorf("旧 key 删除后不应命中")
	}
	if payload, _, ok := s.GetCache("resource_cache", "default_v2", 60); !ok || payload != "v2-payload" {
		t.Errorf("新 key 被误删或读值不对: ok=%v payload=%q", ok, payload)
	}
	// 非法缓存表拒绝（与 SetCache 同样约束）
	if err := s.DeleteCacheKey("logs", "x"); err == nil {
		t.Errorf("非法表应返回错误")
	}
}

// TestEnsureColumnIdempotent 加列幂等：二次 Open 不能重复 ALTER（会报 duplicate column）。
// 这条保护的是生产库就地升级：用户库里已有几千条日志，migrate 必须可重入。
func TestEnsureColumnIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	for i := 0; i < 3; i++ {
		st, err := Open(path)
		if err != nil {
			t.Fatalf("第 %d 次 Open 失败: %v", i+1, err)
		}
		st.Close()
	}
}

// TestPoolCreditsColumnsUpgrade 老库升级：库里已存在**旧的** pool_account_state
// （建表语句里没有 credits / credits_at），Open 必须补上这两列，原数据不得丢。
//
// 回归背景：额度原先只在当前活动账号上现查余额缓存，号池面板只显一个号的额度。
// 修法是把每个号的探测额度落到 pool_account_state，而生产库里这张表早已存在，
// 只靠 CREATE TABLE IF NOT EXISTS 加不上新列。
func TestPoolCreditsColumnsUpgrade(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	// 1) 造一张「旧版」表并写入一行
	raw, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.Exec(`CREATE TABLE pool_account_state (
		uid                TEXT PRIMARY KEY,
		manual_disabled    INTEGER NOT NULL DEFAULT 0,
		manual_disabled_at INTEGER NOT NULL DEFAULT 0,
		auto_disabled      TEXT    NOT NULL DEFAULT '',
		auto_disabled_at   INTEGER NOT NULL DEFAULT 0,
		auto_disabled_msg  TEXT    NOT NULL DEFAULT '',
		quota_fail_streak  INTEGER NOT NULL DEFAULT 0,
		last_probe_at      INTEGER NOT NULL DEFAULT 0,
		last_probe_ok      INTEGER NOT NULL DEFAULT 0,
		last_probe_msg     TEXT    NOT NULL DEFAULT ''
	)`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO pool_account_state (uid, quota_fail_streak, last_probe_msg)
		VALUES ('old-uid', 3, '旧数据')`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	// 2) 正常 Open：迁移必须补列（不报错）
	st, err := Open(path)
	if err != nil {
		t.Fatalf("老库升级失败: %v", err)
	}
	defer st.Close()

	rows, err := st.LoadPoolAccounts()
	if err != nil {
		t.Fatalf("读取升级后的表失败: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("老数据应保留 1 行，实际 %d", len(rows))
	}
	if rows[0].UID != "old-uid" || rows[0].QuotaFailStreak != 3 || rows[0].LastProbeMsg != "旧数据" {
		t.Errorf("升级丢了老数据: %+v", rows[0])
	}
	// 新列默认 0 = 「从未读到过额度」，UI 据此显示 —（而非额度 0）
	if rows[0].Credits != 0 || rows[0].CreditsAt != 0 {
		t.Errorf("新列默认值应为 0，得到 credits=%v credits_at=%d", rows[0].Credits, rows[0].CreditsAt)
	}

	// 3) 写入额度后能读回
	rows[0].Credits, rows[0].CreditsAt = 88.5, 1234
	if err := st.SavePoolAccount(&rows[0]); err != nil {
		t.Fatal(err)
	}
	again, _ := st.LoadPoolAccounts()
	if len(again) != 1 || again[0].Credits != 88.5 || again[0].CreditsAt != 1234 {
		t.Errorf("额度未落库: %+v", again)
	}
}

// TestLogAccountUID 日志能写入并读回 account_uid（号池观测列的往返）。
func TestLogAccountUID(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if err := st.InsertLog(&LogEntry{Model: "glm", StatusCode: 200, AccountUID: "acc-xyz"}); err != nil {
		t.Fatal(err)
	}
	logs, total, err := st.QueryLogs(LogFilter{Page: 1, PageSize: 5})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Fatalf("total = %d，期望 1", total)
	}
	if logs[0].AccountUID != "acc-xyz" {
		t.Errorf("account_uid = %q，期望 acc-xyz", logs[0].AccountUID)
	}
	// 按账号过滤
	if _, total, err := st.QueryLogs(LogFilter{AccountUID: "acc-xyz", Page: 1, PageSize: 5}); err != nil || total != 1 {
		t.Errorf("按 account_uid 过滤失败：total=%d err=%v", total, err)
	}
	if _, total, _ := st.QueryLogs(LogFilter{AccountUID: "other", Page: 1, PageSize: 5}); total != 0 {
		t.Errorf("不匹配的 account_uid 应无结果，实际 %d", total)
	}
}

// TestPoolStateRoundTrip 池状态表往返 + 过期冷却不返回。
func TestPoolStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	row := PoolAccountRow{
		UID: "u1", ManualDisabled: true, ManualDisabledAt: 100,
		AutoDisabled: "quota", AutoDisabledAt: 200, AutoDisabledMsg: "额度已用尽",
		QuotaFailStreak: 2, LastProbeAt: 300, LastProbeOK: false, LastProbeMsg: "credit=0",
		Credits: 1234.5, CreditsAt: 300,
	}
	if err := st.SavePoolAccount(&row); err != nil {
		t.Fatal(err)
	}
	rows, err := st.LoadPoolAccounts()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("应有 1 行，实际 %d", len(rows))
	}
	got := rows[0]
	if !got.ManualDisabled || got.AutoDisabled != "quota" || got.QuotaFailStreak != 2 || got.AutoDisabledMsg != "额度已用尽" {
		t.Errorf("往返后字段不符：%+v", got)
	}
	if got.Credits != 1234.5 || got.CreditsAt != 300 {
		t.Errorf("额度未往返：credits=%v credits_at=%d", got.Credits, got.CreditsAt)
	}
	// UPSERT 覆盖
	row.ManualDisabled = false
	if err := st.SavePoolAccount(&row); err != nil {
		t.Fatal(err)
	}
	rows, _ = st.LoadPoolAccounts()
	if len(rows) != 1 || rows[0].ManualDisabled {
		t.Error("UPSERT 应覆盖而不是插入新行")
	}

	// 冷却：过期的读不出来
	if err := st.SavePoolCool(&PoolCoolRow{UID: "u1", Model: "glm", Until: 1000, ResetAt: 900, Code: "6004", Hits: 3}); err != nil {
		t.Fatal(err)
	}
	if err := st.SavePoolCool(&PoolCoolRow{UID: "u1", Model: "ds", Until: 9000, Code: "429"}); err != nil {
		t.Fatal(err)
	}
	cools, err := st.LoadPoolCools(5000)
	if err != nil {
		t.Fatal(err)
	}
	if len(cools) != 1 || cools[0].Model != "ds" {
		t.Errorf("应只返回未过期的冷却，实际 %+v", cools)
	}
	// 清理过期
	if n, err := st.CleanupPoolCools(5000); err != nil || n != 1 {
		t.Errorf("应清理 1 条过期冷却，实际 %d err=%v", n, err)
	}
	// 删账号连带删冷却
	if err := st.DeletePoolAccount("u1"); err != nil {
		t.Fatal(err)
	}
	if rows, _ := st.LoadPoolAccounts(); len(rows) != 0 {
		t.Error("删除账号后不应残留状态")
	}
	if cools, _ := st.LoadPoolCools(0); len(cools) != 0 {
		t.Error("删除账号后不应残留冷却")
	}
}
