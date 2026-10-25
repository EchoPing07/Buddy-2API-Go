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
