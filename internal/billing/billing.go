// Package billing 解析上游 billing 接口（get-user-resource / checkin 等）的额度包数据。
//
// 抽成独立包的原因：admin（余额面板）与 pool（号池探测）都要回答「这个账号还有多少
// 可用额度」，而上游返回的字段形态并不直观——数值是**字符串**、到期时间是**毫秒整数
// 或带格式的日期串**、日期串**不带时区**。两份实现一旦不一致，就会出现
// 「面板显示有额度、探测判定额度耗尽把号禁用」这类互相矛盾的故障。
package billing

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
)

// TZ 上游 billing 日期串（"2006-01-02 15:04:05" 等无时区格式）为腾讯云北京时间
// （UTC+8，无夏令时，实测周期边界如 CycleStartTime "2026-08-01 00:00:00" 印证），
// 与部署时区无关。若用 time.Local 解析，非 +8 时区部署（Docker TZ 覆盖）的到期判定
// 会偏差最多 8 小时；Expired 直接决定包是否计入可用额度聚合。
// 用 FixedZone 而非 LoadLocation，避免运行环境缺 tzdata 时解析失败。
var TZ = time.FixedZone("UTC+8", 8*60*60)

// NumField 按优先级依次尝试 keys，返回首个"有值"字段的解析结果，与参考实现
// _to_float(precise, _to_float(nonPrecise)) 语义一致：nil / 空串 / 解析失败视为无值
// 并回退到下一个 key；"0" 是合法零值，不触发回退。用于额度包的 capacity 数值字段，
// 上游对免费/裂变包常只返回非 Precise 字段或 Precise 为空串，缺少回退会导致
// "已用/剩余/总量"恒为 0。
//
// 必须处理 string 形态：上游的 CapacityRemainPrecise 等字段实测是 "499.06" 这种字符串。
func NumField(acc map[string]any, keys ...string) float64 {
	for _, k := range keys {
		switch n := acc[k].(type) {
		case nil:
			continue
		case string:
			if strings.TrimSpace(n) == "" {
				continue
			}
			var f float64
			if _, err := fmt.Sscanf(n, "%g", &f); err == nil {
				return f
			}
		case float64:
			return n
		}
	}
	return 0
}

// ResolveExpireTime 按上游契约回退选取到期时间：ExpiredTime → DeductionEndTime → CycleEndTime。
// 实测 CodeBuddy 免费/裂变包 ExpiredTime 常为空字符串，真正到期信息落在 DeductionEndTime
// （毫秒时间戳）或 CycleEndTime（日期串）。返回 (展示串, 时间, 是否有效)；空值/0/9999-99-99
// 哨兵跳过，数字时间戳格式化为本地可读串，字符串原样返回。
func ResolveExpireTime(acc map[string]any) (string, time.Time, bool) {
	for _, key := range []string{"ExpiredTime", "DeductionEndTime", "CycleEndTime"} {
		raw := acc[key]
		switch v := raw.(type) {
		case string:
			if v == "" || strings.HasPrefix(v, "9999-99-99") {
				continue
			}
		case float64:
			if v == 0 {
				continue
			}
		}
		if et, ok := ParseFlexibleTime(raw); ok && !et.IsZero() {
			if s, ok := raw.(string); ok {
				return s, et, true
			}
			return et.Local().Format("2006-01-02 15:04:05"), et, true
		}
	}
	return "", time.Time{}, false
}

// ParseFlexibleTime 宽容解析时间：数字（秒/毫秒）、RFC3339、常见格式。
// 无时区格式的日期串按 TZ（北京时间）解析；RFC3339 自带偏移不受影响。
func ParseFlexibleTime(v any) (time.Time, bool) {
	switch t := v.(type) {
	case float64:
		sec := int64(t)
		if sec > 1e12 { // 毫秒
			return time.Unix(sec/1e3, 0), true
		}
		return time.Unix(sec, 0), true
	case string:
		if t == "" {
			return time.Time{}, false
		}
		layouts := []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02T15:04:05", "2006-01-02"}
		for _, layout := range layouts {
			if ts, err := time.ParseInLocation(layout, t, TZ); err == nil {
				return ts, true
			}
		}
	}
	return time.Time{}, false
}

// TotalDosage 从 get-user-resource 的 data 节点算「真实可用额度」。
//
// 口径与 admin 余额面板的 total_dosage 一致（Σ 未过期包的周期优先剩余）。**不用**上游的
// TotalDosage：它按账户级 CapacityRemain 聚合，而按月周期计费的包（体验版/裂变包）
// 周期额度用尽后账户级 Remain 不随周期扣减，会一直计入幻影剩余
// （实测：真实可用 0 时 TotalDosage 仍显示 500）。
//
// 面板与号池探测共用同一份计算，避免「面板说有额度、探测判定耗尽」的自相矛盾。
func TotalDosage(data []byte) float64 {
	accs, ok := Accounts(data)
	if !ok {
		return 0
	}
	now := time.Now()
	sum := 0.0
	for _, acc := range accs {
		// 到期判定与面板一致：到期未知的包视为有效
		if _, et, ok := ResolveExpireTime(acc); ok && et.Before(now) {
			continue
		}
		// 周期优先：存在周期数据（剩余或已用 > 0）时用周期级，否则回退账户级
		cycRemain := NumField(acc, "CycleCapacityRemainPrecise", "CycleCapacityRemain")
		cycUsed := NumField(acc, "CycleCapacityUsedPrecise", "CycleCapacityUsed")
		if cycRemain > 0 || cycUsed > 0 {
			sum += cycRemain
			continue
		}
		sum += NumField(acc, "CapacityRemainPrecise", "CapacityRemain")
	}
	return round2(sum)
}

// Accounts 解出 data 节点下的 Accounts 数组。
//
// 上游包络层级不稳定（实测有 data.Response.Data 这种三段嵌套），逐层下探到底。
func Accounts(data []byte) ([]map[string]any, bool) {
	node, ok := UnwrapData(data)
	if !ok {
		return nil, false
	}
	raw, ok := node["Accounts"].([]any)
	if !ok {
		return nil, false
	}
	out := make([]map[string]any, 0, len(raw))
	for _, ra := range raw {
		if acc, ok := ra.(map[string]any); ok {
			out = append(out, acc)
		}
	}
	return out, true
}

// UnwrapData 逐层下探上游包络，返回承载业务字段的节点。
//
// 实测层级是 data.Response.Data（三段），但不同接口/版本不一致，
// 故按已知键逐层试探，取最深一层能命中的节点。
func UnwrapData(data []byte) (map[string]any, bool) {
	var top map[string]any
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, false
	}
	node := top
	for _, k := range []string{"data", "Response", "Data"} {
		if next, ok := node[k].(map[string]any); ok {
			node = next
		}
	}
	return node, true
}

func round2(f float64) float64 {
	return math.Round(f*100) / 100
}
