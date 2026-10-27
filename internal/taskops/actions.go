// actions.go 成长任务动作表：每个任务的「纯 API 一键完成」实现。
//
// 覆盖范围（实测可点亮，无需安装官方客户端）：
//
//	chat_5              累计 5 条 chat_request_send 上报（自动补差额）
//	first_buddy         前置上报 → 同意协议 → 领养（+300 分 +8 能）
//	Model_chat_GLM5.2   accept → glm-5.2 真实对话一次 → 对齐模型上报
//	RichMeow_Chat       桌面指纹六事件链（含成功回执）
//	Buddy_App(_QQ)      buddyapp 五连事件（同一组事件覆盖两个任务）
//	automation_1        automated_task_create_suc 事件
//	Library_read        web 域 web_element_click(library_doc_intro_click)
//	template_5          模板使用事件组 ×5
//	playbook_prompt     灵感案例做同款事件组
//	create_canvas       设计画布创建事件组（+300 分）
//	expert_5            真实专家召唤+使用链 ×5（市场真实 id + 真实对话 requestId）
//	Expert_team_use_3   专家团召唤+使用链 ×3
//	Expert_lighthouse   轻量云专家链（mode=LOCAL）
//	Hp_Appearance       appearance/set + appearance_skin_apply
//	skill_1             真实对话 + skill_info 技能加载事件
//	black_cat           夜间窗口内 glm-5.2 对话补足
//	school_season       小程序口径：mp 对话 + activityId
//	Sequential_Tasks_1  小程序口径：mp 首对话
//	Sequential_Tasks_2  小程序口径：mp expert_actual_use（市场真实专家 id）
//	Sequential_Tasks_3  小程序口径：mp 对话 ×5（45s 真人节奏）
//	Sequential_Tasks_4..7  小程序链预留（每日零点解锁一环，判据按源码事件形状预置）
//
// 不做：Expert_Philanthropy（需真实捐款，服务端校验捐赠回执）。
package taskops

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"buddy2api-go/internal/upstream"
)

// Action 一个可自动化的任务动作。
type Action struct {
	Code string // 目标任务码
	Desc string // 展示用说明
	// MP 是否小程序口径专属任务（决定列表回读 / accept / claim 走 mp 变体）。
	MP bool
	// Attempt 尝试型（判据未完全实测验证，跑了可能不点亮）。
	Attempt bool
	Run     func(r *Runner) (string, error)
}

// actions 动作表。顺序即执行顺序（先解锁依赖项：chat_5 / first_buddy 在最前）。
var actions = []*Action{
	{Code: "chat_5", Desc: "上报 5 条对话活跃事件（自动补足差额）", Run: runChat5},
	{Code: "first_buddy", Desc: "上报解锁 → 同意协议 → 领取第一只 Buddy（+300 分）", Run: runFirstBuddy},
	{Code: "Model_chat_GLM5.2", Desc: "glm-5.2 真实对话一次 → 对齐模型上报", Run: runModelChat},
	{Code: "RichMeow_Chat", Desc: "桌面指纹对话事件链（已验证纯 API 可点亮）", Run: runRichMeow},
	{Code: "Buddy_App", Desc: "进入 Buddy 应用事件链", Run: runBuddyApp},
	{Code: "Buddy_App_QQ", Desc: "进入企鹅教师助手事件链（与 Buddy_App 共用）", Run: runBuddyApp},
	{Code: "automation_1", Desc: "定时任务创建成功事件", Run: runAutomationCreate},
	{Code: "Library_read", Desc: "资料库介绍阅读事件（web 域上报）", Run: runLibraryRead},
	{Code: "template_5", Desc: "模板使用事件组 ×5", Run: runTemplateUse},
	{Code: "playbook_prompt", Desc: "灵感案例「做同款」发送事件组", Run: runPlaybookPrompt},
	{Code: "create_canvas", Desc: "设计创意画布创建事件组（+300 分）", Run: runCreateCanvas},
	{Code: "expert_5", Desc: "真实专家召唤+使用链 ×5", Run: runExpertUse},
	{Code: "Expert_team_use_3", Desc: "专家团召唤+使用链 ×3", Run: runExpertTeamUse},
	{Code: "Expert_lighthouse", Desc: "轻量云专家召唤+使用链（真实对话 requestId）", Run: runExpertLighthouse},
	{Code: "Hp_Appearance", Desc: "设置主题 + 皮肤生效事件", Run: runAppearance},
	{Code: "skill_1", Desc: "真实对话 + skill_info 技能加载事件", Run: runSkillFresh},
	{Code: "black_cat", Desc: "夜猫子：23:00–08:00 窗口内 glm-5.2 对话补足", Attempt: true, Run: runBlackCat},

	// ── 小程序口径（默认列表不下发，accept/claim 均要求 mp 头）──
	{Code: "school_season", Desc: "校园日：mp 对话 + activityId 上报", MP: true, Run: runSchoolSeason},
	{Code: "Sequential_Tasks_1", Desc: "小程序首对话：mp 对话上报", MP: true, Run: runSequentialChat1},
	{Code: "Sequential_Tasks_2", Desc: "小程序选专家对话：mp expert_actual_use", MP: true, Run: runSequentialExpert},
	{Code: "Sequential_Tasks_3", Desc: "小程序五次对话：mp 对话 ×5（45s 节奏）", MP: true, Run: runSequentialChat5},
	{Code: "Sequential_Tasks_4", Desc: "小程序定时任务（预留）：定时任务创建事件", MP: true, Attempt: true, Run: runSequentialAutomation},
	{Code: "Sequential_Tasks_5", Desc: "小程序使用 GLM5.2（预留）：带模型字段的 mp 对话", MP: true, Attempt: true, Run: runSequentialModelChat},
	{Code: "Sequential_Tasks_6", Desc: "小程序十次对话（预留）：mp 对话 ×target", MP: true, Attempt: true, Run: runSequentialChat10},
	{Code: "Sequential_Tasks_7", Desc: "小程序体验灵感（预留）：灵感事件组", MP: true, Attempt: true, Run: runSequentialPlaybook},
}

// ActionFor 查任务对应的动作（无则 nil = 不可自动化）。
func ActionFor(code string) *Action {
	code = strings.TrimSpace(code)
	for _, a := range actions {
		if a.Code == code {
			return a
		}
	}
	return nil
}

// ActionInfo 动作的对外视图（不含 func 字段，可直接 JSON 序列化给前端）。
type ActionInfo struct {
	Code    string `json:"code"`
	Desc    string `json:"desc"`
	MP      bool   `json:"mp"`
	Attempt bool   `json:"attempt"`
}

// Actions 返回动作清单的对外视图（供前端展示「支持哪些任务」）。
func Actions() []ActionInfo {
	out := make([]ActionInfo, 0, len(actions))
	for _, a := range actions {
		out = append(out, ActionInfo{Code: a.Code, Desc: a.Desc, MP: a.MP, Attempt: a.Attempt})
	}
	return out
}

// ── 通用工具 ─────────────────────────────────────────────────────────────────

// needProgress 读任务并算出还差多少次。
//
// 返回 error 的三态必须区分开，否则会把「查不到」当成「已达标」：
//   - err != nil           查询失败（网络/5xx）：调用方必须把它当失败上报，不能静默跳过
//   - task == nil, err==nil 当前账号没有下发此任务（正常态，无可执行）
//   - done == true          已领取或进度已达标
//
// 曾经这里的错误分支返回 (0, fallback, false)，而调用方只判 done/need<=0，
// 于是上游一次瞬时 500 就让 9 个动作统一「成功」返回「进度已达标，无需上报」。
func (r *Runner) needProgress(code string, mp bool, fallbackTarget int64) (need int64, target int64, done bool, err error) {
	t, err := r.taskByCode(code, mp)
	if err != nil {
		return 0, fallbackTarget, false, err
	}
	if t == nil {
		return 0, fallbackTarget, true, nil // 无此任务：无可执行
	}
	target = t.Target
	if target <= 0 {
		target = fallbackTarget
	}
	if t.Claimed || t.Current >= target {
		return 0, target, true, nil
	}
	return target - t.Current, target, false, nil
}

// chatOnce 发一条真实 chat（读干流，避免残留连接）。
func (r *Runner) chatOnce(model string) error {
	body, err := json.Marshal(map[string]any{
		"model":    model,
		"messages": []map[string]any{{"role": "user", "content": "hi，请回复一句话"}},
		"stream":   true,
	})
	if err != nil {
		return err
	}
	resp, err := r.c.DoChat(context.Background(), body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return fmt.Errorf("对话失败 http=%d: %s", resp.StatusCode, truncStr(string(raw), 160))
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return nil
}

// truncStr 按**字符**截断文本（避免把上游长响应原样透给前端）。
// 必须按 rune 切：上游错误体是中文，按字节切会把一个汉字劈成半个，前端收到非法 UTF-8。
func truncStr(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// newIDs 生成一组事件关联 id。
func newIDs(prefix string) (conv, req, msg string) {
	base := time.Now().UnixNano()
	conv = fmt.Sprintf("b2a-%s-%d", prefix, base)
	req = fmt.Sprintf("b2a-%s-req-%d", prefix, base)
	msg = fmt.Sprintf("msg-%d", base%1e10)
	return
}

// ── 基础任务 ─────────────────────────────────────────────────────────────────

// runChat5 补足 chat_5 的进度：按差额上报 chat_request_send（同会话多轮）。
func runChat5(r *Runner) (string, error) {
	need, target, done, perr := r.needProgress("chat_5", false, 5)
	if perr != nil {
		return "", fmt.Errorf("读取任务进度失败: %w", perr)
	}
	if done || need <= 0 {
		return "进度已达标，无需上报", nil
	}
	conv := fmt.Sprintf("b2a-chat5-%d", time.Now().UnixMilli())
	for i := int64(0); i < need; i++ {
		if err := r.c.ReportChatActivity(conv, fmt.Sprintf("%s-r%d", conv, i)); err != nil {
			return "", fmt.Errorf("上报第 %d/%d 条对话事件失败: %w", i+1, need, err)
		}
		if i < need-1 {
			time.Sleep(reportGap)
		}
	}
	return fmt.Sprintf("已补报 %d 条对话事件（目标 %d）", need, target), nil
}

// runFirstBuddy 领养：前置上报（解锁 chat_5 门槛）→ 同意协议 → 领养。
func runFirstBuddy(r *Runner) (string, error) {
	conv := fmt.Sprintf("b2a-adopt-%d", time.Now().UnixMilli())
	if err := r.c.ReportChatActivity(conv, ""); err != nil {
		return "", fmt.Errorf("前置上报: %w", err)
	}
	time.Sleep(reportGap) // 给上游事件处理留时间（门槛真实来源是「当日有活跃上报」）
	if err := r.c.BuddyAgreement(); err != nil {
		return "", fmt.Errorf("同意协议: %w", err)
	}
	if err := r.c.BuddyFirst(); err != nil {
		if upstream.IsBuddyTaskIncomplete(err) {
			return "前置已上报，但领养门槛未过（上游要求当日活跃），请稍后重试", nil
		}
		return "", fmt.Errorf("领取 Buddy: %w", err)
	}
	return "已领取 Buddy（+300 分 +8 能量）", nil
}

// runModelChat 完成 Model_chat_GLM5.2：真实对话一次 + 对齐模型上报。
func runModelChat(r *Runner) (string, error) {
	const modelID, modelName = "glm-5.2", "GLM-5.2"
	if err := r.chatOnce(modelID); err != nil {
		return "", fmt.Errorf("对话请求: %w", err)
	}
	time.Sleep(reportGap)
	conv := fmt.Sprintf("b2a-glm52-%d", time.Now().UnixMilli())
	if err := r.c.ReportChatActivityModel(conv, "", modelID, modelName); err != nil {
		return "", fmt.Errorf("对话已完成，但进度上报失败: %w", err)
	}
	return "已完成 glm-5.2 对话并上报", nil
}

// runRichMeow 完成 RichMeow_Chat（桌面端对话 1 次）：六事件链含成功回执。
func runRichMeow(r *Runner) (string, error) {
	conv, req, msg := newIDs("rm")
	events := upstream.DesktopChatSequence(conv, req, msg, "fast-model", "fast-model")
	if err := r.c.ReportDesktopEvent(events...); err != nil {
		return "", err
	}
	return "已按桌面端指纹上报完整对话事件链（agent_task_created→chat_response）", nil
}

// runBuddyApp 完成 Buddy_App / Buddy_App_QQ：同一组五连事件覆盖两个任务。
func runBuddyApp(r *Runner) (string, error) {
	events := upstream.DesktopBuddyAppSequence(upstream.BuddyAppDefaultID, upstream.BuddyAppDefaultName)
	if err := r.c.ReportDesktopEvent(events...); err != nil {
		return "", err
	}
	return "已上报 buddyapp 进入五连事件（同时覆盖 Buddy_App 与 Buddy_App_QQ）", nil
}

// runAutomationCreate 完成 automation_1：定时任务创建成功事件。
func runAutomationCreate(r *Runner) (string, error) {
	if err := r.c.ReportDesktopEvent(upstream.DesktopAutomationCreateEvent("b2a 自动化")); err != nil {
		return "", err
	}
	return "已上报定时任务创建事件", nil
}

// runLibraryRead 完成 Library_read：web 域资料库介绍点击。
func runLibraryRead(r *Runner) (string, error) {
	const docURL = "https://www.workbuddy.cn/space/d/o0KWYeynteVv06UnAZqIFm"
	if err := r.c.ReportWebEvent("web_element_click", docURL,
		"library_doc_intro_click", "WorkBuddy资料库介绍"); err != nil {
		return "", err
	}
	return "已上报资料库介绍阅读事件", nil
}

// runTemplateUse 完成 template_5：五组模板使用事件。
func runTemplateUse(r *Runner) (string, error) {
	templates := [][2]string{
		{"1", "深度研究"}, {"2", "周报生成"}, {"3", "竞品分析"}, {"4", "活动策划"}, {"5", "代码评审"},
	}
	for i, tp := range templates {
		conv, req, _ := newIDs(fmt.Sprintf("tpl%d", i))
		events := upstream.DesktopTemplateUseSequence(conv, req, tp[0], tp[1])
		if err := r.c.ReportDesktopEvent(events...); err != nil {
			return "", fmt.Errorf("第 %d/5 组模板事件上报失败: %w", i+1, err)
		}
		time.Sleep(300 * time.Millisecond)
	}
	return "已上报 template_used ×5", nil
}

// runPlaybookPrompt 完成 playbook_prompt：灵感案例 Dialog 发送 Prompt 事件组。
func runPlaybookPrompt(r *Runner) (string, error) {
	conv, req, _ := newIDs("pb")
	events := upstream.DesktopPlaybookPromptSequence(conv, req,
		"pm-gtm-launch-plan", "新产品上市 GTM 发布计划一页纸")
	if err := r.c.ReportDesktopEvent(events...); err != nil {
		return "", err
	}
	return "已上报 playbook_cta_click + playbook_prompt_send", nil
}

// runCreateCanvas 完成 create_canvas：设计画布创建/打开事件组。
func runCreateCanvas(r *Runner) (string, error) {
	conv, req, _ := newIDs("canvas")
	events := upstream.DesktopDesignCanvasSequence(conv, req)
	if err := r.c.ReportDesktopEvent(events...); err != nil {
		return "", err
	}
	return "已上报 wbx_design_canvas_task_create/open", nil
}

// hpThemeKey Hp_Appearance 判据主题（和平精英激战金秋）。
const hpThemeKey = "theme-tkmw7j"

// runAppearance 完成 Hp_Appearance：设置主题 API + 皮肤生效事件。
func runAppearance(r *Runner) (string, error) {
	if err := r.c.SetAppearanceTheme(hpThemeKey); err != nil {
		return "", fmt.Errorf("设置主题: %w", err)
	}
	time.Sleep(2 * time.Second)
	if err := r.c.ReportDesktopEvent(upstream.DesktopAppearanceSkinEvent(hpThemeKey)); err != nil {
		return "", err
	}
	return "已设置主题并上报皮肤生效事件", nil
}

// runSkillFresh 完成 skill_1：真实对话（取服务端 requestId）+ skill_info 事件。
func runSkillFresh(r *Runner) (string, error) {
	conv, req, err := r.c.DesktopChatWithExpert("")
	if err != nil {
		return "", fmt.Errorf("真实对话: %w", err)
	}
	msgID := "msg-" + tail8(req)
	events := upstream.DesktopChatSequence(conv, req, msgID, "fast-model", "fast-model")
	for _, ev := range events {
		if ev["eventCode"] == "chat_message_response" {
			ev["finishReason"] = "tool_calls" // 模型发起工具调用（技能加载）语义
		}
	}
	events = append(events, upstream.DesktopSkillInfoEvent(
		"skill_2097350077599879168", "润泽小馆·日报撰写", conv, req, msgID))
	if err := r.c.ReportDesktopEvent(events...); err != nil {
		return "", fmt.Errorf("skill_info 事件: %w", err)
	}
	return "已上报真实对话 + skill_info 技能加载事件", nil
}

// tail8 取字符串末 8 位（拼事件 id 用）。
func tail8(s string) string {
	if len(s) <= 8 {
		return s
	}
	return s[len(s)-8:]
}

// ── 专家类 ───────────────────────────────────────────────────────────────────

func runExpertUse(r *Runner) (string, error)     { return r.expertBatch("agent", 5) }
func runExpertTeamUse(r *Runner) (string, error) { return r.expertBatch("team", 3) }

// expertBatch 专家召唤+使用的公共实现：市场真实 id → 召唤链 → 真实对话取服务端
// requestId → 使用事件 + chat 链。自造专家 id 或自造 requestId 均不计数。
func (r *Runner) expertBatch(expertType string, count int) (string, error) {
	experts, err := r.c.MarketExpertList(expertType)
	if err != nil {
		return "", fmt.Errorf("拉取专家列表: %w", err)
	}
	if len(experts) == 0 {
		return "", fmt.Errorf("专家市场列表为空")
	}
	ok, fail := 0, 0
	for i, e := range experts {
		if ok >= count {
			break
		}
		if err := r.c.ReportDesktopEvent(upstream.DesktopExpertSummonSequence(e)...); err != nil {
			fail++
			continue
		}
		conv, req, cerr := r.c.DesktopChatWithExpert(e.ExpertID)
		if cerr != nil {
			fail++
			continue
		}
		events := append(
			upstream.DesktopChatSequence(conv, req, "msg-"+tail8(req), "fast-model", "fast-model"),
			upstream.DesktopExpertActualUseEvent(e, conv, req, "craft"))
		if err := r.c.ReportDesktopEvent(events...); err != nil {
			fail++
			continue
		}
		ok++
		if i < len(experts)-1 && ok < count {
			time.Sleep(expertSummonGap)
		}
	}
	msg := fmt.Sprintf("已对 %d 位真实专家完成召唤+使用链（类型 %s）", ok, expertType)
	if fail > 0 {
		msg += fmt.Sprintf("，%d 位失败", fail)
	}
	return msg, nil
}

// lighthouseExpertID 腾讯轻量云专家 id（Expert_lighthouse 判据专家，可领一个月轻量服务器）。
const lighthouseExpertID = "ex_2cvvUZQhDyeJ"

// runExpertLighthouse 完成 Expert_lighthouse：与 expert_5 同构，两处差异——
// chat 链的 agent_task_created 需带 has_expert:true + expert_id；expert_actual_use
// 的 mode 为 LOCAL（非 craft）、type 为空、cost=0。
func runExpertLighthouse(r *Runner) (string, error) {
	lh := upstream.MarketExpert{
		ExpertID: lighthouseExpertID, ExpertType: "agent",
		DisplayNameZH: "腾讯轻量云专家", ProfessionZH: "腾讯轻量云专家", Version: "1.0.2",
	}
	// 市场列表命中真实条目则用其信息（version 等以服务端为准）。
	if experts, err := r.c.MarketExpertList("agent"); err == nil {
		for _, e := range experts {
			if e.ExpertID == lighthouseExpertID {
				lh = e
				break
			}
		}
	}
	if err := r.c.ReportDesktopEvent(upstream.DesktopExpertSummonSequence(lh)...); err != nil {
		return "", fmt.Errorf("召唤链: %w", err)
	}
	conv, req, err := r.c.DesktopChatWithExpert(lh.ExpertID)
	if err != nil {
		return "", fmt.Errorf("真实对话: %w", err)
	}
	events := upstream.DesktopChatSequence(conv, req, "msg-"+tail8(req), "fast-model", "fast-model")
	for _, ev := range events {
		if ev["eventCode"] == "agent_task_created" {
			ev["has_expert"] = true
			ev["expert_id"] = lh.ExpertID
			ev["expert_name"] = lh.DisplayNameZH
			ev["expert_industry_id"] = ""
		}
	}
	use := upstream.DesktopExpertActualUseEvent(lh, conv, req, "LOCAL")
	use["type"] = "" // 对齐真实样本：轻量云专家 actual_use 的 type 为空、cost=0
	use["cost"] = 0
	events = append(events, use)
	if err := r.c.ReportDesktopEvent(events...); err != nil {
		return "", fmt.Errorf("使用事件: %w", err)
	}
	return "已上报轻量云专家召唤+使用链（真实对话 requestId）", nil
}

// ── 夜猫子 ───────────────────────────────────────────────────────────────────

// runBlackCat 完成 black_cat：仅夜间窗口内计数，窗口外直接提示。
func runBlackCat(r *Runner) (string, error) {
	if !upstream.InNightWindow(r.nowFn()) {
		return "当前不在 23:00–08:00 计数窗口，行为不计分；排程会在夜间自动补足", nil
	}
	need, err := r.c.BlackcatNeed()
	if err != nil {
		return "", err
	}
	if need <= 0 {
		return "进度已达标，无需补足", nil
	}
	ok, runErr := r.c.RunNightChats(int(need))
	if runErr != nil {
		return "", fmt.Errorf("夜间对话完成 %d/%d 次后中断: %w", ok, need, runErr)
	}
	return fmt.Sprintf("已完成 %d 次夜间对话并上报", ok), nil
}

// ── 小程序口径任务 ───────────────────────────────────────────────────────────

// runMPMiniChat 小程序限定任务通用闭环：mp 对话事件按差额补报 → 回读领奖。
// activityID 非空时事件带 activityId（school_season 必带；Sequential_Tasks_1/3 不带）。
func (r *Runner) runMPMiniChat(code, activityID string, fallbackTarget int64) (string, error) {
	need, target, done, perr := r.needProgress(code, true, fallbackTarget)
	if perr != nil {
		return "", fmt.Errorf("读取任务进度失败: %w", perr)
	}
	if done || need <= 0 {
		return "进度已达标，无需上报", nil
	}
	if need > 0 {
		// 首条也等：上一轮残留进度被反作弊回滚后立即重报同样无效。
		time.Sleep(mpActionGap)
	}
	for i := int64(0); i < need; i++ {
		conv := fmt.Sprintf("b2a-mp-%d-%d", time.Now().UnixMilli(), i)
		ev := upstream.MPChatEvent(conv, activityID)
		if err := r.c.ReportMPEvent(ev); err != nil {
			return "", fmt.Errorf("第 %d/%d 条小程序对话事件上报失败: %w", i+1, need, err)
		}
		if i < need-1 {
			time.Sleep(mpChatEventGap) // 真人节奏：连发会被反作弊整体回滚
		}
	}
	return fmt.Sprintf("已补报 %d 条小程序对话事件（目标 %d）", need, target), nil
}

// runSchoolSeason 完成 school_season「校园日」：mp chat + activityId=school_open_day_2026。
func runSchoolSeason(r *Runner) (string, error) {
	return r.runMPMiniChat("school_season", "school_open_day_2026", 1)
}

// runSequentialChat1 完成 Sequential_Tasks_1「小程序内完成 1 次有效对话」。
func runSequentialChat1(r *Runner) (string, error) {
	return r.runMPMiniChat("Sequential_Tasks_1", "", 1)
}

// runSequentialChat5 完成 Sequential_Tasks_3「小程序内完成 5 次有效对话」。
// 服务端按上报条数累加，但要求真人节奏（mpChatEventGap）。
func runSequentialChat5(r *Runner) (string, error) {
	return r.runMPMiniChat("Sequential_Tasks_3", "", 5)
}

// runSequentialChat10 完成 Sequential_Tasks_6「小程序内完成 10 次有效对话」（预留）。
func runSequentialChat10(r *Runner) (string, error) {
	return r.runMPMiniChat("Sequential_Tasks_6", "", 10)
}

// runSequentialModelChat 完成 Sequential_Tasks_5「使用 GLM5.2」（预留）：
// 判据疑为带 requestModelId 的 mp 对话事件。
func runSequentialModelChat(r *Runner) (string, error) {
	need, target, done, perr := r.needProgress("Sequential_Tasks_5", true, 1)
	if perr != nil {
		return "", fmt.Errorf("读取任务进度失败: %w", perr)
	}
	if done || need <= 0 {
		return "进度已达标，无需上报", nil
	}
	time.Sleep(mpActionGap)
	for i := int64(0); i < need; i++ {
		conv := fmt.Sprintf("b2a-mp-glm-%d-%d", time.Now().UnixMilli(), i)
		if err := r.c.ReportMPEvent(upstream.MPChatModelEvent(conv, "glm-5.2", "GLM-5.2")); err != nil {
			return "", fmt.Errorf("第 %d/%d 条小程序模型事件上报失败: %w", i+1, need, err)
		}
		if i < need-1 {
			time.Sleep(mpChatEventGap)
		}
	}
	return fmt.Sprintf("已补报 %d 条带模型字段的小程序对话事件（目标 %d）", need, target), nil
}

// runSequentialExpert 完成 Sequential_Tasks_2「在小程序内选中专家并完成有效对话」：
// 专家 id 必须是市场真实 ex_ id（空 id 服务端不入账），拉不到就整任务不动作。
func runSequentialExpert(r *Runner) (string, error) {
	experts, err := r.c.MarketExpertList("agent")
	if err != nil {
		return "", fmt.Errorf("拉取专家市场列表: %w", err)
	}
	if len(experts) == 0 {
		return "", fmt.Errorf("专家市场列表为空，无法构造真实专家事件")
	}
	e := experts[0]
	time.Sleep(mpActionGap)
	if err := r.c.ReportMPEvent(upstream.MPExpertUseEvent(e.ExpertID, e.DisplayNameZH, e.ExpertType)); err != nil {
		return "", err
	}
	return fmt.Sprintf("已上报小程序专家使用事件（%s / %s）", e.ExpertID, e.DisplayNameZH), nil
}

// runSequentialAutomation 完成 Sequential_Tasks_4「创建定时任务」（预留）：
// mp 源码无 automation 事件发射点，判据疑为 PC 口径 → 复用 automation_1 同源事件。
func runSequentialAutomation(r *Runner) (string, error) {
	if err := r.c.ReportDesktopEvent(upstream.DesktopAutomationCreateEvent("b2a mp 自动化")); err != nil {
		return "", err
	}
	return "已上报定时任务创建事件（PC 同源判据）", nil
}

// runSequentialPlaybook 完成 Sequential_Tasks_7「体验灵感功能」（预留）：
// PC 灵感事件组 + mp 指纹形态各报一轮（判据认哪侧待实测）。
func runSequentialPlaybook(r *Runner) (string, error) {
	conv, req, _ := newIDs("pbmp")
	if err := r.c.ReportDesktopEvent(
		upstream.DesktopPlaybookPromptSequence(conv, req, "pm-gtm-launch-plan", "新产品上市 GTM 发布计划一页纸")...,
	); err != nil {
		return "", fmt.Errorf("PC 灵感事件组: %w", err)
	}
	time.Sleep(mpActionGap)
	if err := r.c.ReportMPEvent(upstream.MPPlaybookEvents("pm-gtm-launch-plan", "新产品上市 GTM 发布计划一页纸")...); err != nil {
		return "", fmt.Errorf("mp 灵感事件组: %w", err)
	}
	return "已上报灵感事件组（PC + mp 双形态）", nil
}
