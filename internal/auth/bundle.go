// 批量凭证包：一次导出 / 导入多个账号的容器格式（账号档案 data/accounts 的搬运载体）。
//
// 刻意与单账号 token.json 不同构：包内含 format / version 声明，导入侧凭此拒绝任意 JSON，
// 也不会把「一个包」误当成「一份凭证」。
//
// 包内每条记录与 Token 字段一一对应，额外多出四项导出元信息：format / version /
// exported_at / active_uid。active_uid 只记录「导出时哪个账号在生效」，供人工核对；
// 导入侧不据此启用任何账号 —— 批量导入刻意只入档（一次导入多个账号时启用哪个无从判断）。
package auth

import (
	"encoding/json"
	"errors"
	"fmt"
)

const (
	// BundleFormat 凭证包格式标识（导入侧据此判定「这是不是本项目的包」）。
	BundleFormat = "buddy2api.accounts"
	// BundleVersion 当前包格式版本。新增字段可保持兼容时不必提升；
	// 语义变化（字段改名 / 单位变更）必须提升，否则旧程序会静默读错。
	BundleVersion = 1
)

// Bundle 批量凭证包。
type Bundle struct {
	Format     string  `json:"format"`
	Version    int     `json:"version"`
	ExportedAt int64   `json:"exported_at"`          // unix 秒
	ActiveUID  string  `json:"active_uid,omitempty"` // 导出时的活动账号（仅记录，不据此启用）
	Accounts   []Token `json:"accounts"`
}

// NewBundle 组装凭证包。toks 逐份拷贝：调用方可能继续持有并改写这些凭证
// （活动 token.json 随时可能被刷新），不能把指针直接塞进包里。
func NewBundle(exportedAt int64, activeUID string, toks []*Token) Bundle {
	accounts := make([]Token, 0, len(toks))
	for _, t := range toks {
		if t == nil {
			continue
		}
		accounts = append(accounts, *t)
	}
	return Bundle{
		Format:     BundleFormat,
		Version:    BundleVersion,
		ExportedAt: exportedAt,
		ActiveUID:  activeUID,
		Accounts:   accounts,
	}
}

// UnmarshalBundle 解析凭证包并补齐 JWT 派生字段。
//
// 只做结构校验（format / version / 是否含账号），不校验每条 access_token 是否合法：
// 这是逐个账号的「导入」步骤该报的错，批量语义下要能逐条报、逐条跳过，不能一条坏就整包拒绝。
func UnmarshalBundle(raw []byte) (*Bundle, error) {
	var b Bundle
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, fmt.Errorf("不是合法 JSON: %w", err)
	}
	if b.Format != BundleFormat {
		return nil, fmt.Errorf("不是凭证包（format 应为 %q）", BundleFormat)
	}
	if b.Version < 1 || b.Version > BundleVersion {
		return nil, fmt.Errorf("凭证包版本 %d 不受支持（本程序支持 1～%d）", b.Version, BundleVersion)
	}
	if len(b.Accounts) == 0 {
		return nil, errors.New("凭证包里没有账号")
	}
	// 包内可能只有 access_token（手工拼的包）：uid / nickname / domain / expires_at 从 JWT 补齐，
	// 否则档案缺 uid 会被档案库拒绝入库。
	for i := range b.Accounts {
		b.Accounts[i].EnrichFromJWT()
	}
	return &b, nil
}
