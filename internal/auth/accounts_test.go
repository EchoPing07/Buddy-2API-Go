package auth

import (
	"errors"
	"os"
	"testing"
	"time"
)

// TestAccountBookRoundTrip 覆盖存档 → 列表 → 读取 → 删除，以及非法 UID 的文件名归一。
func TestAccountBookRoundTrip(t *testing.T) {
	dir := t.TempDir()
	b := NewAccountBook(dir)

	// 目录尚未创建：空列表、无错误；不存在的档案返回哨兵错误
	if metas, err := b.List(); err != nil || len(metas) != 0 {
		t.Fatalf("空目录 List = %v, %v", metas, err)
	}
	if _, err := b.Load("missing"); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("不存在的档案应返回 ErrAccountNotFound，得到 %v", err)
	}

	exp := time.Now().Add(time.Hour).Unix()
	if err := b.Save(&Token{UID: "uid-a", Nickname: "A", Domain: "copilot.tencent.com", ExpiresAt: exp}); err != nil {
		t.Fatal(err)
	}
	if err := b.Save(&Token{UID: "uid-b", Nickname: "B"}); err != nil {
		t.Fatal(err)
	}

	metas, err := b.List()
	if err != nil || len(metas) != 2 {
		t.Fatalf("List = %v, %v，期望 2 条", metas, err)
	}
	for _, m := range metas {
		if m.SavedAt == 0 {
			t.Errorf("%s 缺少 SavedAt", m.ID)
		}
	}
	got, err := b.Load("uid-a")
	if err != nil || got.Nickname != "A" || got.Domain != "copilot.tencent.com" || got.ExpiresAt != exp {
		t.Fatalf("Load = %+v, %v", got, err)
	}

	// 非法 UID（含路径分隔符与 ..）必须归一为哈希文件名，落点不逃出档案目录
	weird := "uid/../../evil"
	if err := b.Save(&Token{UID: weird, Nickname: "weird"}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(b.dir)
	if err != nil || len(entries) != 3 {
		t.Fatalf("ReadDir = %d 项, %v，期望 3 项", len(entries), err)
	}
	for _, e := range entries {
		if !accountIDRe.MatchString(e.Name()[:len(e.Name())-len(".json")]) {
			t.Errorf("档案文件名未归一：%q", e.Name())
		}
	}
	if got, err := b.Load(weird); err != nil || got.Nickname != "weird" {
		t.Fatalf("按原始 UID 读回失败：%+v, %v", got, err)
	}

	// 空 UID 拒绝存档
	if err := b.Save(&Token{}); err == nil {
		t.Error("空 UID 应拒绝存档")
	}

	// 删除幂等
	if err := b.Delete("uid-b"); err != nil {
		t.Fatal(err)
	}
	if err := b.Delete("uid-b"); err != nil {
		t.Fatalf("重复删除应幂等，得到 %v", err)
	}
	if _, err := b.Load("uid-b"); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("删除后应读不到，得到 %v", err)
	}
}

// TestAccountBookUpdateRemark 覆盖备注更新、保留 SavedAt、清空备注与非法 UID 归一。
func TestAccountBookUpdateRemark(t *testing.T) {
	dir := t.TempDir()
	b := NewAccountBook(dir)

	if err := b.Save(&Token{UID: "uid-a", Nickname: "A", Domain: "copilot.tencent.com"}); err != nil {
		t.Fatal(err)
	}
	before, err := b.Load("uid-a")
	if err != nil {
		t.Fatal(err)
	}

	updated, err := b.UpdateRemark("uid-a", "主力号")
	if err != nil {
		t.Fatalf("UpdateRemark 失败: %v", err)
	}
	if updated.Remark != "主力号" {
		t.Fatalf("Remark = %q，期望 主力号", updated.Remark)
	}
	if updated.SavedAt != before.SavedAt {
		t.Errorf("改备注不应改动 SavedAt（会导致列表跳动）：before=%d after=%d", before.SavedAt, updated.SavedAt)
	}

	metas, err := b.List()
	if err != nil || len(metas) != 1 || metas[0].Remark != "主力号" {
		t.Fatalf("List 应返回备注，得到 %+v, err=%v", metas, err)
	}

	// 清空备注
	cleared, err := b.UpdateRemark("uid-a", "")
	if err != nil || cleared.Remark != "" {
		t.Fatalf("清空备注失败: %+v, %v", cleared, err)
	}

	// 不存在的档案
	if _, err := b.UpdateRemark("missing", "x"); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("不存在的档案应返回 ErrAccountNotFound，得到 %v", err)
	}

	// 非法 UID（哈希文件名）同样可通过原始 UID 或 AccountID 更新
	weird := "uid/../../remark"
	if err := b.Save(&Token{UID: weird, Nickname: "W"}); err != nil {
		t.Fatal(err)
	}
	if got, err := b.UpdateRemark(AccountID(weird), "特殊ID备注"); err != nil || got.Remark != "特殊ID备注" {
		t.Fatalf("按 AccountID 更新备注失败: %+v, %v", got, err)
	}
}

// TestTokenStoreUpdateRemarkSurvivesRefresh 刷新过程中修改备注，刷新完成后必须保留最新备注，
// 且不能因改备注打断刷新单飞（s.t == t 指针校验）。
func TestTokenStoreUpdateRemarkSurvivesRefresh(t *testing.T) {
	dir := t.TempDir()
	ts, err := NewTokenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.Save(&Token{
		UID: "u1", AccessToken: "old-at", RefreshToken: "rt", Remark: "旧备注",
	}); err != nil {
		t.Fatal(err)
	}

	// 模拟 refreshFn 在锁外执行期间用户更新了备注，且上游响应不含 Remark
	err = ts.ForceRefresh(func(old *Token) (*Token, error) {
		if err := ts.UpdateRemark("u1", "刷新中改的新备注"); err != nil {
			return nil, err
		}
		return &Token{UID: old.UID, AccessToken: "new-at", RefreshToken: "new-rt"}, nil
	})
	if err != nil {
		t.Fatalf("ForceRefresh 失败: %v", err)
	}
	got := ts.Get()
	if got == nil || got.AccessToken != "new-at" {
		t.Fatalf("改备注不应打断刷新结果: %+v", got)
	}
	if got.Remark != "刷新中改的新备注" {
		t.Fatalf("刷新后备注丢失: got=%q，期望 刷新中改的新备注", got.Remark)
	}

	// 重新从磁盘加载验证持久化
	reloaded, err := NewTokenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if r := reloaded.Get(); r == nil || r.Remark != "刷新中改的新备注" || r.AccessToken != "new-at" {
		t.Fatalf("落盘内容不符: %+v", r)
	}
}
