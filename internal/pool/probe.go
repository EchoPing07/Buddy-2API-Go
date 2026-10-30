package pool

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"time"

	"buddy2api-go/internal/billing"
	"buddy2api-go/internal/upstream"
)

// probeMinInterval 手动探测限频：同一账号 30 秒内只允许手动探一次，
// 避免用户连点把上游打爆（上游 billing 接口有频率限制）。
const probeMinInterval = 30 * time.Second

// ErrProbeTooFrequent 手动探测过于频繁。
var ErrProbeTooFrequent = errors.New("探测过于频繁，请稍后再试")

// ProbeResult 一次探测的结果。
type ProbeResult struct {
	UID      string  `json:"uid"`
	OK       bool    `json:"ok"`       // 凭证可用且还有额度
	Disabled bool    `json:"disabled"` // 探测后是否仍被禁用
	Reason   string  `json:"reason"`   // 禁用原因（"" = 可用）
	Msg      string  `json:"msg"`      // 可读说明
	Credits  float64 `json:"credits"`  // 可用额度
	Skipped  bool    `json:"skipped"`  // 手动禁用，未探测
}

// Probe 探测单个账号：凭证可用吗 + 还有额度吗。
//
// 用的就是现成的 billing 接口 `/v2/billing/meter/get-user-resource`（client.Resources），
// 一次调用同时回答两个问题；它内部已经调了 token 刷新，所以探测本身就把
// 「凭证过期」的号顺手刷回来了——这正是自动恢复能覆盖 AutoCredential 的原因。
//
// 状态迁移严格按 §5.4：
//   - 手动禁用：不探测（用户的决定优先），只更新探测时间让 UI 知道「没在探测它」；
//   - 凭证错误：AutoDisabled = credential（保持）；
//   - 有额度：AutoDisabled 清空（恢复），连击计数归零；
//   - 无额度：AutoDisabled = quota（保持）；
//   - 网络错误：**不改任何状态**，只记 msg（下次 cron 再试）。
//
// 最后一条是关键：把「网络抖动」当成「号还是坏的」会让一次断网把好号永久禁掉。
func (m *Manager) Probe(ctx context.Context, uid string) ProbeResult {
	res := ProbeResult{UID: uid}

	// 锁内只取**值快照**，绝不把 *AccountState 带出锁外。
	// 早期版本留下指针后在锁外读 st.AutoDisabled：AccountState 上的 AutoDisabled
	// 是 string（ptr+len 双字），与 NoteError/setAutoDisabled 并发时会被撕裂，
	// 属于真 data race（-race 必报，但本机 CGO_ENABLED=0 跑不了 -race 所以长期潜伏）。
	m.mu.RLock()
	st, exists := m.accounts[uid]
	ts := m.toks[uid]
	manual := exists && st.ManualDisabled
	prevDisabled := ""
	if exists {
		prevDisabled = st.AutoDisabled
	}
	m.mu.RUnlock()

	if !exists {
		res.Msg = "号池内无此账号"
		return res
	}
	if manual {
		// 手动禁用：不探（探测成功也不该自动打开），但记一次时间供 UI 展示
		res.Skipped = true
		res.Disabled = true
		res.Reason = "manual"
		res.Msg = "已手动禁用，探测不会自动恢复"
		m.recordProbe(uid, false, "manual-disabled", 0)
		return res
	}
	if ts == nil {
		res.Msg = "凭证未加载"
		return res
	}

	// 走该号自己的凭证（ResourcesWith），不是 c.toks 当前活动账号。
	raw, err := m.client.ResourcesWith(ts)
	credentialErr := false
	if err != nil && looksLikeCredentialError(err) {
		credentialErr = true
		// 凭证类错误：先强刷一次（可能只是过期），刷得动就重试一轮。
		// 这是 AutoCredential 能自愈的唯一路径——刷不动才算真的失效。
		if rerr := ts.ForceRefresh(m.client.RefreshFn); rerr == nil {
			if raw2, err2 := m.client.ResourcesWith(ts); err2 == nil {
				raw, err = raw2, nil
				credentialErr = false
			}
		}
	}
	if err != nil {
		res.Msg = truncRunes(err.Error(), 200)
		if credentialErr {
			// 凭证确实失效（刷不动）：标记成因并保持禁用，等用户重新登录。
			// 这与「网络错误不改状态」的区分是本函数的要点：
			// 401 是可确定归因的，DNS/超时不是。
			m.setAutoDisabled(uid, AutoCredential, "凭证已失效且刷新失败（探测确认）")
			m.router.DropUID(uid)
			res.Disabled = true
			res.Reason = AutoCredential
			m.recordProbe(uid, false, res.Msg, 0)
			slog.Warn("号池探测确认凭证失效", "uid", shortUID(uid), "error", err)
			return res
		}
		// 网络/上游故障：**不改任何状态**，只留痕，下次 cron 再试。
		// 把「网络抖动」当成「号还是坏的」会让一次断网把好号永久禁掉。
		res.Disabled = prevDisabled != ""
		res.Reason = prevDisabled
		m.recordProbe(uid, false, "probe failed: "+res.Msg, 0)
		slog.Warn("号池探测失败（不改状态）", "uid", shortUID(uid), "error", err)
		return res
	}

	credits := SumAvailable(raw.Data)
	res.Credits = credits
	if credits > 0 {
		// 恢复：清自动域与连击计数，**不碰手动域**
		m.mu.Lock()
		st := m.accounts[uid]
		was := ""
		if st != nil {
			was = st.AutoDisabled
			st.AutoDisabled = ""
			st.AutoDisabledAt = 0
			st.AutoDisabledMsg = ""
			st.QuotaFailStreak = 0
		}
		m.mu.Unlock()
		if was != "" {
			slog.Info("账号已自动恢复", "uid", shortUID(uid), "was", was, "credits", credits)
		}
		res.OK = true
		res.Msg = "credit=" + fmtFloat(credits)
		m.recordProbe(uid, true, res.Msg, credits)
		return res
	}

	// 有响应但额度为 0：保持/升级为 quota 禁用
	m.setAutoDisabled(uid, AutoQuota, "额度已用尽（探测确认）")
	m.router.DropUID(uid)
	res.Disabled = true
	res.Reason = AutoQuota
	res.Msg = "credit=0"
	m.recordProbe(uid, false, res.Msg, 0)
	return res
}

// ProbeAll 探测全池（管理页「全部探测」与 cron 共用）。
func (m *Manager) ProbeAll(ctx context.Context) []ProbeResult {
	members := m.Members()
	out := make([]ProbeResult, 0, len(members))
	for _, uid := range members {
		out = append(out, m.Probe(ctx, uid))
	}
	return out
}

// ProbeWithLimit 手动探测（带 30 秒限频）。
func (m *Manager) ProbeWithLimit(ctx context.Context, uid string) (ProbeResult, error) {
	now := time.Now()
	if prev, ok := m.probeLimit.Load(uid); ok {
		if t, ok := prev.(time.Time); ok && now.Sub(t) < probeMinInterval {
			return ProbeResult{}, ErrProbeTooFrequent
		}
	}
	m.probeLimit.Store(uid, now)
	return m.Probe(ctx, uid), nil
}

// setAutoDisabled 设置自动禁用域并落库（异步）。已处于同一原因时不重复写。
func (m *Manager) setAutoDisabled(uid, reason, msg string) {
	m.mu.Lock()
	st := m.accounts[uid]
	if st == nil {
		m.mu.Unlock()
		return
	}
	if st.AutoDisabled != reason {
		st.AutoDisabled = reason
		st.AutoDisabledAt = time.Now().Unix()
	}
	st.AutoDisabledMsg = truncRunes(msg, 200)
	row := m.rowOfLocked(st)
	m.mu.Unlock()
	m.persist(persistOp{account: row})
}

// recordProbe 更新探测域并落库（异步）。
func (m *Manager) recordProbe(uid string, ok bool, msg string, credits float64) {
	now := time.Now().Unix()
	m.mu.Lock()
	st := m.accounts[uid]
	if st == nil {
		m.mu.Unlock()
		return
	}
	st.LastProbeAt = now
	st.LastProbeOK = ok
	st.LastProbeMsg = truncRunes(msg, 200)
	row := m.rowOfLocked(st)
	m.mu.Unlock()
	m.persist(persistOp{account: row})
}

// SumAvailable 从 get-user-resource 的 data 节点算「真实可用额度」。
//
// 实现放在 internal/billing：admin 余额面板与号池探测必须用**同一份**口径，
// 否则会出现「面板显示还有额度、探测判定耗尽把号禁用」这种自相矛盾的故障。
// 上游字段形态很反直觉（capacity 是字符串、到期时间是毫秒数字、日期串无时区），
// 自行实现极易踩坑——这里早期版本就踩过：按数字解析导致额度恒为 0，
// 探测会把全部健康账号打成「额度用尽」。
func SumAvailable(data json.RawMessage) float64 {
	return billing.TotalDosage(data)
}

func fmtFloat(f float64) string {
	if f == math.Trunc(f) {
		return itoa(int64(f))
	}
	return itoa(int64(f)) + "." + itoa(int64(math.Round(f*100))%100)
}

// looksLikeCredentialError 判定 billing 失败是否属于「凭证不可用」。
//
// 只认哨兵错误，不按文案匹配：billing 的其它错误（超时 / DNS / 5xx / 业务码）
// 一律当网络或上游故障，**不改账号状态**——一次断网不该把好号永久禁掉。
func looksLikeCredentialError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, upstream.ErrNoToken) || errors.Is(err, upstream.ErrUpstreamUnauthorized)
}
