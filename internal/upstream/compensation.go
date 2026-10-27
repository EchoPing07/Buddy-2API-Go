// compensation.go 官方补偿活动的状态查询（/billing/meter/compensation-status）。
//
// 背景：官方在「套餐与积分」页挂限时补偿横幅（例：「模型服务异常额外补偿 500 积分」，
// 带领取截止日），账号侧看得见，网关这边此前是**盲领**——ClaimCompensation 只在成长链
// 里顺带调一次，且写法是 `if credit, err := ...; err == nil { log }`，
// 于是三种完全不同的情形在日志里同构：
//
//	① 没有补偿   ② 已经领过   ③ 真的领取失败（401/5xx/网络）
//
// 补偿带截止日，签到停摆 / 机器关机 / 账号被禁用时就会静默过期。本文件补上
// 「先查状态再决定是否领」所缺的那一半。
//
// 上游口径：active=false 是「当前无补偿活动」的正常 200 返回，不是错误——故本接口
// 只在传输失败 / 非 2xx / 业务 code 非 0 时返回 error。
package upstream

import (
	"encoding/json"
	"net/http"
)

// CompensationStatus 单账号的补偿活动状态。
type CompensationStatus struct {
	// Active 当前是否有生效中的补偿活动（无活动时为 false，属正常返回）。
	Active bool `json:"active"`
	// Claimed 本次活动是否已领取。
	Claimed bool `json:"claimed"`
	// CreditNum 待领额度（积分）。
	CreditNum int64 `json:"credit_num"`
	// ValidityPeriod 发放积分的有效期倍数。上游未明示单位且前端不渲染，只透传不换算。
	ValidityPeriod int64 `json:"validity_period"`
	// EndTime 领取截止时间（上游为 "2006-01-02 15:04:05" 无时区字符串）。
	EndTime string `json:"end_time"`
}

// Pending 是否存在「可领且未领」的补偿——即调用 ClaimCompensation 的前置判据。
// 三者与门：active=false 是没有活动；claimed=true 是已领完；credit_num<=0 是无额度。
func (s CompensationStatus) Pending() bool {
	return s.Active && !s.Claimed && s.CreditNum > 0
}

// GetCompensationStatus 查补偿活动状态（billing 域，与 ClaimCompensation 同域同头）。
func (c *Client) GetCompensationStatus() (CompensationStatus, error) {
	var out CompensationStatus
	data, err := c.growthCall(http.MethodPost, "/billing/meter/compensation-status", map[string]any{})
	if err != nil {
		return out, err
	}
	if len(data) == 0 || string(data) == "null" {
		return out, nil
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return out, err
	}
	return out, nil
}
