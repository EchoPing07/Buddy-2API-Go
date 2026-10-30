package pool

// AccountState 账号级池状态。
//
// 手动域与自动域**分开**，不合并成一个 disabled 字段，三个理由：
//  1. 用户手动关掉的号，探测成功后不该自动打开（最气人的 bug）；
//  2. 6004 只封一个模型，账号其它模型仍可用（那是模型级维度，见 cools）；
//  3. UI 要能分别展示「这个号被禁用」与「这个号只是 glm 撞了限额」。
type AccountState struct {
	UID string

	// 手动域（用户操作，探测永不改写）
	ManualDisabled   bool  `json:"manual_disabled"`
	ManualDisabledAt int64 `json:"manual_disabled_at,omitempty"`

	// 自动域（错误驱动 + 探测恢复）
	AutoDisabled    string `json:"auto_disabled"` // "" | "quota" | "credential"
	AutoDisabledAt  int64  `json:"auto_disabled_at,omitempty"`
	AutoDisabledMsg string `json:"auto_disabled_msg"`
	QuotaFailStreak int    `json:"quota_fail_streak"` // 连续额度类失败次数

	// 探测域
	LastProbeAt  int64  `json:"last_probe_at,omitempty"`
	LastProbeOK  bool   `json:"last_probe_ok"`
	LastProbeMsg string `json:"last_probe_msg"`

	// 观测域（不落盘，随重启清零）
	SuccessCount int64 `json:"success_count"`
	ErrorCount   int64 `json:"error_count"`
	LastUsedAt   int64 `json:"last_used_at,omitempty"`
}

// Available 账号级可用 = 未手动禁用，且没有被自动禁用。
// 注意这只回答「整号能不能用」；某个模型是否可用还要看 cools。
func (s *AccountState) Available() bool {
	return s != nil && !s.ManualDisabled && s.AutoDisabled == ""
}
