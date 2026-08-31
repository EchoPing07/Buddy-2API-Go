package store

import (
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
