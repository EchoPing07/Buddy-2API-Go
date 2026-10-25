// 账号档案：data/accounts/<id>.json 存放多份凭证，供管理后台手动切换。
//
// 活动账号永远只有 token.json 一份（TokenStore），档案目录只是旁挂副本：
// 切换 = 归档当前 token.json → 用目标档案覆盖 token.json。upstream / proxy / scheduler
// 都只读 token.json，因此无需感知多账号。不做轮询、不做故障转移。
package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ErrAccountNotFound 档案不存在。
var ErrAccountNotFound = errors.New("账号档案不存在")

// accountIDRe 档案 id（= 文件名）允许的字符集。
//
// id 会直接拼进文件路径并出现在 URL 里，故须保守；含其它字符的 UID 统一走哈希，
// 目录穿越（../、斜杠）因此在构造路径前就被排除。
var accountIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// AccountMeta 档案元信息。刻意不含 access/refresh token：列表接口直接序列化它，
// 从类型上杜绝凭证随列表出网。
type AccountMeta struct {
	ID        string // 档案 id（文件名，可安全用于 URL）
	UID       string // 原始 uid（判定是否当前账号）
	Nickname  string
	Domain    string
	ExpiresAt int64
	SavedAt   int64
}

// AccountBook 账号档案目录（<data>/accounts）。
type AccountBook struct{ dir string }

// NewAccountBook 创建档案库；目录在首次 Save 时按需创建。
func NewAccountBook(dataDir string) *AccountBook {
	return &AccountBook{dir: filepath.Join(dataDir, "accounts")}
}

// accountID 把 UID 归一为安全文件名；含非法字符时退回哈希，
// 保证同一 UID 始终映射同一份档案（Load / Delete 可用原始 UID 调回）。
func accountID(uid string) string {
	if accountIDRe.MatchString(uid) {
		return uid
	}
	sum := sha256.Sum256([]byte(uid))
	return "h_" + hex.EncodeToString(sum[:8])
}

// List 返回全部档案元信息，按 SavedAt 倒序（最近保存/切换的在前）。
// 目录不存在视为空列表；单份损坏只跳过该份，不让整页失败。
func (b *AccountBook) List() ([]AccountMeta, error) {
	entries, err := os.ReadDir(b.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]AccountMeta, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		t, err := b.loadFile(filepath.Join(b.dir, e.Name()))
		if err != nil {
			continue
		}
		out = append(out, AccountMeta{
			ID:        strings.TrimSuffix(e.Name(), ".json"),
			UID:       t.UID,
			Nickname:  t.Nickname,
			Domain:    t.Domain,
			ExpiresAt: t.ExpiresAt,
			SavedAt:   t.SavedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SavedAt > out[j].SavedAt })
	return out, nil
}

// Save 写入（或覆盖）一份档案；SavedAt 取当前时间，使列表按最近使用排序。
func (b *AccountBook) Save(t *Token) error {
	if t == nil || t.UID == "" {
		return errors.New("凭证缺少 uid，无法存档")
	}
	if err := os.MkdirAll(b.dir, 0o700); err != nil {
		return err
	}
	// 拷贝后再改 SavedAt：调用方传的可能是活动凭证本身，不能就地改写。
	cp := *t
	cp.SavedAt = time.Now().Unix()
	data, err := json.MarshalIndent(&cp, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(b.dir, accountID(t.UID)+".json"), data, 0o600)
}

// Load 读取一份档案；id 可以是档案 id，也可直接传原始 UID。
func (b *AccountBook) Load(id string) (*Token, error) {
	t, err := b.loadFile(filepath.Join(b.dir, accountID(id)+".json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrAccountNotFound
		}
		return nil, err
	}
	return t, nil
}

// Delete 删除一份档案；不存在视为已删除（幂等）。
func (b *AccountBook) Delete(id string) error {
	if err := os.Remove(filepath.Join(b.dir, accountID(id)+".json")); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// loadFile 读单份档案。旧文件可能缺字段，用 JWT claims 补齐。
func (b *AccountBook) loadFile(path string) (*Token, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var t Token
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, fmt.Errorf("解析账号档案 %s: %w", filepath.Base(path), err)
	}
	t.EnrichFromJWT()
	return &t, nil
}
