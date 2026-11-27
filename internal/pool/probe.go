package pool

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"time"

	"buddy2api-go/internal/billing"
	"buddy2api-go/internal/config"
	"buddy2api-go/internal/upstream"
)

// probeMinInterval 手动探测限频：同一账号 30 秒内只允许手动探一次，
// 避免用户连点把上游打爆（上游 billing 接口有频率限制）。
const probeMinInterval = 30 * time.Second

// asyncProbeMinInterval 自动异步探测（未探测补全 / 过期包刷新）限频。
const asyncProbeMinInterval = 5 * time.Minute

// 可在单测中调整的异步探测抖动延迟。
var (
	unprobedInitDelay     = 200 * time.Millisecond
	unprobedPerAcctJitter = 50 * time.Millisecond
)

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

// SetResourceHook 注入探测成功获取上游 resource 原始 JSON 后的回调（用于同步更新 resource_cache）。
func (m *Manager) SetResourceHook(fn func(uid string, raw []byte)) {
	if m == nil {
		return
	}
	m.hookMu.Lock()
	m.resourceHook = fn
	m.hookMu.Unlock()
}

func (m *Manager) getResourceHook() func(uid string, raw []byte) {
	m.hookMu.RLock()
	fn := m.resourceHook
	m.hookMu.RUnlock()
	return fn
}

// UpdateQuotaFromResource 从已拉取的 billing.Summary 更新账号额度、到期摘要及自动禁用/恢复状态。
func (m *Manager) UpdateQuotaFromResource(uid string, sum billing.Summary, at int64) {
	if m == nil || uid == "" {
		return
	}
	if at <= 0 {
		at = time.Now().Unix()
	}
	var dropSessions bool
	var recoveredFrom string

	m.mu.Lock()
	st := m.accounts[uid]
	if st == nil {
		m.mu.Unlock()
		return
	}
	st.Credits = sum.Total
	st.EarliestExpireAt = sum.EarliestExpireAt
	st.ExpiringCredits = sum.ExpiringCredits
	st.CreditsAt = at
	if sum.Total > 0 {
		if st.AutoDisabled == AutoQuota {
			recoveredFrom = st.AutoDisabled
			st.AutoDisabled = ""
			st.AutoDisabledAt = 0
			st.AutoDisabledMsg = ""
			st.QuotaFailStreak = 0
		} else {
			st.QuotaFailStreak = 0
		}
	} else {
		if st.AutoDisabled != AutoQuota {
			st.AutoDisabled = AutoQuota
			st.AutoDisabledAt = at
		}
		st.AutoDisabledMsg = "额度已用尽（余额刷新确认）"
		dropSessions = true
	}
	row := m.rowOfLocked(st)
	m.mu.Unlock()

	m.persist(persistOp{account: row})
	if recoveredFrom != "" {
		slog.Info("余额刷新触发账号自动恢复", "uid", shortUID(uid), "was", recoveredFrom, "credits", sum.Total)
	}
	if dropSessions {
		m.router.DropUID(uid)
	}
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
func (m *Manager) Probe(ctx context.Context, uid string) ProbeResult {
	res := ProbeResult{UID: uid}

	// 锁内只取**值快照**，绝不把 *AccountState 带出锁外。
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
		m.recordProbeSummary(uid, false, "manual-disabled", billing.Summary{}, false)
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
			m.setAutoDisabled(uid, AutoCredential, "凭证已失效且刷新失败（探测确认）")
			m.router.DropUID(uid)
			res.Disabled = true
			res.Reason = AutoCredential
			m.recordProbeSummary(uid, false, res.Msg, billing.Summary{}, false)
			slog.Warn("号池探测确认凭证失效", "uid", shortUID(uid), "error", err)
			return res
		}
		// 网络/上游故障：**不改任何状态**，只留痕，下次 cron 再试。
		res.Disabled = prevDisabled != ""
		res.Reason = prevDisabled
		m.recordProbeSummary(uid, false, "probe failed: "+res.Msg, billing.Summary{}, false)
		slog.Warn("号池探测失败（不改状态）", "uid", shortUID(uid), "error", err)
		return res
	}

	if fn := m.getResourceHook(); fn != nil && len(raw.Raw) > 0 {
		fn(uid, raw.Raw)
	}

	sum := billing.QuotaSummary(raw.Data)
	res.Credits = sum.Total
	if sum.Total > 0 {
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
			slog.Info("账号已自动恢复", "uid", shortUID(uid), "was", was, "credits", sum.Total)
		}
		res.OK = true
		res.Msg = "credit=" + fmtFloat(sum.Total)
		m.recordProbeSummary(uid, true, res.Msg, sum, true)
		return res
	}

	// 有响应但额度为 0：保持/升级为 quota 禁用
	m.setAutoDisabled(uid, AutoQuota, "额度已用尽（探测确认）")
	m.router.DropUID(uid)
	res.Disabled = true
	res.Reason = AutoQuota
	res.Msg = "credit=0"
	m.recordProbeSummary(uid, false, res.Msg, sum, true)
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

// ProbeUnprobedAsync 异步补全从未探测过（CreditsAt == 0 && !ManualDisabled）且匹配当前端点的账号额度。
// 内部通过 CAS + 单账号限频 + ctx 退出保护，避免并发重入、刷爆上游或 Close 后 panic。
func (m *Manager) ProbeUnprobedAsync() {
	if m == nil || m.client == nil || m.ctx == nil || m.ctx.Err() != nil {
		return
	}
	if !m.unprobedRunning.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer m.unprobedRunning.Store(false)
		if unprobedInitDelay > 0 {
			timer := time.NewTimer(unprobedInitDelay)
			select {
			case <-timer.C:
			case <-m.ctx.Done():
				timer.Stop()
				return
			}
		}
		regionDomain := config.EndpointOf(m.cfg.Get().Region).Domain
		now := time.Now()

		m.mu.RLock()
		var targets []string
		for _, uid := range m.members {
			st := m.accounts[uid]
			if st == nil || st.ManualDisabled || st.CreditsAt != 0 {
				continue
			}
			if ts := m.toks[uid]; ts != nil {
				if t := ts.Get(); t != nil && !regionMatches(t.Domain, regionDomain) {
					continue
				}
			}
			if prev, ok := m.asyncProbeAt.Load(uid); ok {
				if pt, ok := prev.(time.Time); ok && now.Sub(pt) < asyncProbeMinInterval {
					continue
				}
			}
			targets = append(targets, uid)
		}
		m.mu.RUnlock()

		for i, uid := range targets {
			if m.ctx.Err() != nil {
				return
			}
			if i > 0 && unprobedPerAcctJitter > 0 {
				timer := time.NewTimer(unprobedPerAcctJitter)
				select {
				case <-timer.C:
				case <-m.ctx.Done():
					timer.Stop()
					return
				}
			}
			m.asyncProbeAt.Store(uid, time.Now())
			ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
			m.Probe(ctx, uid)
			cancel()
		}
	}()
}

// triggerStaleExpireProbeAsync 对 EarliestExpireAt 已过期但尚未重探的账号触发限频异步重探。
func (m *Manager) triggerStaleExpireProbeAsync(uids []string) {
	if m == nil || m.client == nil || m.ctx == nil || m.ctx.Err() != nil {
		return
	}
	now := time.Now()
	var due []string
	for _, uid := range uids {
		if prev, ok := m.asyncProbeAt.Load(uid); ok {
			if pt, ok := prev.(time.Time); ok && now.Sub(pt) < asyncProbeMinInterval {
				continue
			}
		}
		m.asyncProbeAt.Store(uid, now)
		due = append(due, uid)
	}
	if len(due) == 0 {
		return
	}
	go func() {
		for _, uid := range due {
			if m.ctx.Err() != nil {
				return
			}
			ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
			m.Probe(ctx, uid)
			cancel()
		}
	}()
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

// recordProbeSummary 更新探测域及额度/到期摘要并落库（异步）。
//
// creditsKnown 为 true 表示本次探测**真的读到了**余额（成功 / 明确 0），此时把额度与
// 到期摘要一并写进状态；为 false（网络故障、凭证刷不动）时只能更新探测时间，绝不能把
// 旧额度清成 0——那会让面板把一个好号显示成「额度 0」。
func (m *Manager) recordProbeSummary(uid string, ok bool, msg string, sum billing.Summary, creditsKnown bool) {
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
	if creditsKnown {
		st.Credits = sum.Total
		st.EarliestExpireAt = sum.EarliestExpireAt
		st.ExpiringCredits = sum.ExpiringCredits
		st.CreditsAt = now
	}
	row := m.rowOfLocked(st)
	m.mu.Unlock()
	m.persist(persistOp{account: row})
}

// SumAvailable 从 get-user-resource 的 data 节点算「真实可用额度」。
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
