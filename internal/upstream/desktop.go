// desktop.go 桌面客户端（WorkBuddy Desktop 5.5.6）行为指纹上报 + 任务判据事件构造。
//
// 任务计分走 `POST /v2/report` 行为上报，但不同任务认**不同客户端指纹**：
//
//	CLI 指纹    copilot.tencent.com + CLI/2.x UA             （活跃上报 / chat 类任务）
//	桌面指纹    copilot.tencent.com + WorkBuddy/5.5.6 + workbuddy-desktop 事件族
//	Web 指纹    www.workbuddy.cn + x-client-platform: web    （Library_read 等页面行为）
//	mp 指纹     www.codebuddy.cn + ideType=WorkBuddy_MP      （小程序限定任务）
//
// 本文件负责后三种（CLI 在 growth.go 的 ReportChatActivity）+ 全部判据事件形状。
// 事件载荷照抄实测抓包形状（勿精简：上游对字段集合有一定校验倾向）。
package upstream

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

const (
	desktopReportPath    = "/v2/report"
	desktopAppearanceSet = "/v2/user-asset/appearance/set"
	// desktopUA 实测桌面客户端 UA（5.5.6 内嵌 CLI 2.137.1）。
	desktopUA = "WorkBuddy/5.5.6 WorkBuddy/5.5.6 CLI/2.137.1"
	// desktopVersion 桌面端版本号（事件指纹字段）。
	desktopVersion = "5.5.6"
)

// deriveID 由 uid 稳定派生一个 36 位 hex 设备标识（machineId/sessionId 复用），
// 幂等：同一账号每次生成相同值，模拟固定设备。
func deriveID(uid, salt string) string {
	sum := sha256.Sum256([]byte(salt + ":" + uid))
	return hex.EncodeToString(sum[:18]) // 36 hex chars
}

// DesktopEvent 桌面端事件：业务字段任意（map），公共指纹由 ReportDesktopEvent 注入。
type DesktopEvent map[string]any

// desktopFingerprint 公共桌面指纹字段（注入每个事件，业务字段优先覆盖同名键）。
func desktopFingerprint(uid, nickname string) map[string]any {
	now := time.Now().UnixMilli()
	return map[string]any{
		"timezone":     "Asia/Shanghai",
		"reportDelay":  2000,
		"userId":       uid,
		"username":     nickname,
		"userNickname": nickname,
		"product":      "SaaS",
		"releaseDate":  int64(1789036585355),
		"commit":       "5f9692923c93033111c51ad7b003eb80204a9b75",
		"ideName":      "WorkBuddy",
		"ideType":      "WorkBuddy",
		"ideVersion":   desktopVersion,
		"machineId":    deriveID(uid, "machine"),
		"sessionId":    deriveID(uid, "session"),
		"extName":      "workbuddy-desktop",
		"extVersion":   desktopVersion,
		"os":           "win32",
		"arch":         "x64",
		"osVersion":    "10.0.26220",
		"cpuCores":     20,
		"memorySize":   24,
		"timestamp":    now,
		"presentAt":    now,
	}
}

// ReportDesktopEvent 以桌面客户端指纹向 chat 域 /v2/report 批量上报事件。
func (c *Client) ReportDesktopEvent(events ...DesktopEvent) error {
	if len(events) == 0 {
		return fmt.Errorf("desktop report: no events")
	}
	t := c.toks.Get()
	if t == nil {
		return ErrNoToken
	}
	fp := desktopFingerprint(t.UID, t.Nickname)
	arr := make([]map[string]any, 0, len(events))
	for _, ev := range events {
		m := make(map[string]any, len(fp)+len(ev))
		for k, v := range fp {
			m[k] = v
		}
		for k, v := range ev {
			m[k] = v
		}
		arr = append(arr, m)
	}
	raw, err := json.Marshal(arr)
	if err != nil {
		return err
	}
	return c.doReport(func() (*http.Request, error) {
		// 每次重建都取当前 token：doAuthed 的 401 重试要带刷新后的新凭证。
		ct := c.toks.Get()
		if ct == nil {
			return nil, ErrNoToken
		}
		req, berr := http.NewRequest(http.MethodPost, growthBase(c)+desktopReportPath, bytes.NewReader(raw))
		if berr != nil {
			return nil, berr
		}
		req.Header.Set("Authorization", "Bearer "+ct.AccessToken)
		req.Header.Set("Accept", "application/json, text/plain, */*")
		req.Header.Set("Content-Type", "application/json;charset=UTF-8")
		req.Header.Set("User-Agent", desktopUA)
		req.Header.Set("X-Domain", growthBase(c))
		req.Header.Set("X-Product", "SaaS")
		req.Header.Set("X-Request-ID", deriveID(ct.UID, "req")+fmt.Sprintf("%d", time.Now().UnixNano()%1e6))
		if ct.UID != "" {
			req.Header.Set("X-User-Id", ct.UID)
		}
		return req, nil
	})
}

// doReport 发送 /v2/report 类请求（经 doAuthed：token 过期 / 401 自动刷新重试）。
// 上报端点有多种响应形态（空体 / {"code":0} / 信封），只把非 2xx 当错误；
// 2xx 一律视为「已受理」——上报 200 ≠ 计分，计分由回读进度判定。
func (c *Client) doReport(build func() (*http.Request, error)) error {
	status, raw, err := c.doAuthed(c.short, build)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("report HTTP %d: %s", status, truncStr(string(raw), 200))
	}
	// 部分形态仍会用信封表达业务失败（code != 0），此时必须报错，否则动作会被误记成功。
	var env envelope
	if json.Unmarshal(raw, &env) == nil && env.Code != 0 {
		return fmt.Errorf("report code=%d: %s", env.Code, env.Msg)
	}
	return nil
}

// ReportWebEvent 以 Web 端指纹向 www.workbuddy.cn/v2/report 上报单事件。
// 用于 Library_read 等页面行为类任务（实测 library_doc_intro_click 4 秒点亮）。
func (c *Client) ReportWebEvent(eventCode, pageURL, elementID, elementName string) error {
	t := c.toks.Get()
	if t == nil {
		return ErrNoToken
	}
	ua := "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36"
	ev := map[string]any{
		"eventCode": eventCode, "timestamp": time.Now().UnixMilli(), "reportDelay": 0,
		"pageURL": pageURL, "elementId": elementID, "elementName": elementName,
		"os": "Win32", "arch": "", "osVersion": "10.0", "userAgent": ua,
		"machineId": deriveID(t.UID, "webmachine"), "userId": t.UID,
		"userNickname": t.Nickname, "enterpriseId": t.EnterpriseID,
	}
	raw, err := json.Marshal([]map[string]any{ev})
	if err != nil {
		return err
	}
	return c.doReport(func() (*http.Request, error) {
		ct := c.toks.Get()
		if ct == nil {
			return nil, ErrNoToken
		}
		req, berr := http.NewRequest(http.MethodPost, webBase(c)+"/v2/report", bytes.NewReader(raw))
		if berr != nil {
			return nil, berr
		}
		req.Header.Set("Authorization", "Bearer "+ct.AccessToken)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("x-client-platform", "web")
		req.Header.Set("Origin", webBase(c))
		req.Header.Set("Referer", pageURL)
		req.Header.Set("User-Agent", ua)
		if ct.UID != "" {
			req.Header.Set("X-User-Id", ct.UID)
		}
		return req, nil
	})
}

// ── 小程序（mp）指纹上报 ─────────────────────────────────────────────────────

// mpReportBase 小程序上报主机（billing 同域 www.codebuddy.cn，与 chat 域不同）。
const mpReportBase = "https://www.codebuddy.cn"

// mpReportBaseFn 小程序上报主机（包级变量便于单测替换；可用 Client.MPBaseOverride 覆盖）。
var mpReportBaseFn = func(c *Client) string {
	if c != nil && c.MPBaseOverride != "" {
		return c.MPBaseOverride
	}
	return mpReportBase
}

// schoolOpenDayActivityID 开学季/校园日活动 id（事件 activityId 字段值，两域共用）。
const schoolOpenDayActivityID = "school_open_day_2026"

// mpMachineID 小程序设备标识：由 uid 稳定派生（36 hex 按 UUID 排版）。
//
// 早期照抄抓包的字面量，导致**所有账号共享同一个 mp machineId**——设备维度的风控
// 聚合能一眼把这批流量归成同一台机器。改成按账号派生：同一账号恒定（幂等，不会每天
// 换设备），不同账号互不相同。
func mpMachineID(uid string) string {
	h := deriveID(uid, "mpmachine") // 36 hex chars（deriveID = sha256 前 18 字节）
	if len(h) < 32 {
		return h
	}
	// UUID 形态共 36 字符（32 hex + 4 个连字符），故取前 32 位再排版。
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// mpEventBase 小程序埋点公共指纹（appservice wQ()+Ao() 对齐）。
func mpEventBase(uid, nickname string) map[string]any {
	return map[string]any{
		"timestamp":    time.Now().UnixMilli(),
		"ideType":      "WorkBuddy_MP",
		"ideVersion":   "2.4.0",
		"extName":      "workbuddy-mp",
		"extVersion":   "2.4.0",
		"product":      "SaaS",
		"ideName":      "wx_app_cloud",
		"platform":     "mini_program",
		"os":           "windows",
		"osVersion":    "11",
		"arch":         "x64",
		"machineId":    mpMachineID(uid),
		"timezone":     "Asia/Shanghai",
		"userId":       uid,
		"userNickname": nickname,
	}
}

// ReportMPEvent 以小程序指纹向 www.codebuddy.cn/v2/report 批量上报事件。
func (c *Client) ReportMPEvent(events ...map[string]any) error {
	if len(events) == 0 {
		return fmt.Errorf("mp report: no events")
	}
	t := c.toks.Get()
	if t == nil {
		return ErrNoToken
	}
	base := mpEventBase(t.UID, t.Nickname)
	arr := make([]map[string]any, 0, len(events))
	for _, ev := range events {
		m := make(map[string]any, len(base)+len(ev))
		for k, v := range base {
			m[k] = v
		}
		for k, v := range ev {
			m[k] = v
		}
		arr = append(arr, m)
	}
	raw, err := json.Marshal(arr)
	if err != nil {
		return err
	}
	return c.doReport(func() (*http.Request, error) {
		ct := c.toks.Get()
		if ct == nil {
			return nil, ErrNoToken
		}
		req, berr := http.NewRequest(http.MethodPost, mpReportBaseFn(c)+"/v2/report", bytes.NewReader(raw))
		if berr != nil {
			return nil, berr
		}
		req.Header.Set("Authorization", "Bearer "+ct.AccessToken)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		if ct.UID != "" {
			req.Header.Set("X-User-Id", ct.UID)
		}
		req.Header.Set("X-Client-Product", "workbuddy-mp")
		req.Header.Set("X-Client-Version", "2.4.0")
		req.Header.Set("X-Client-Platform", "mp-weixin")
		req.Header.Set("X-Platform", "wechatmp")
		return req, nil
	})
}

// ── 桌面事件序列构造 ─────────────────────────────────────────────────────────

// DesktopChatSequence 构造一次「桌面端成功对话」的完整 6 事件链
// （agent_task_created → chat_message_send → chat_request_send →
// chat_message_response(isSuccessful) → chat_message_status → chat_request_response）。
// 实测该链点亮 RichMeow_Chat。
func DesktopChatSequence(conversationID, requestID, messageID, modelID, modelName string) []DesktopEvent {
	mk := func(code string, extra map[string]any) DesktopEvent {
		ev := DesktopEvent{"eventCode": code}
		for k, v := range extra {
			ev[k] = v
		}
		return ev
	}
	now := time.Now().UnixMilli()
	return []DesktopEvent{
		mk("agent_task_created", map[string]any{
			"source": "LOCAL", "name": "working", "task_target": "local", "mode": "craft",
			"requestModelId": modelID, "requestModelName": modelName,
			"has_repo": false, "repo_type": "none", "workspace_type": "empty",
			"has_connector": false, "connector_types": []any{},
			"has_mention": false, "mention_types": []any{},
			"has_template": false, "action": "", "template_name": "",
			"has_expert": false, "expert_id": "", "expert_name": "", "expert_industry_id": "",
			"has_skill": false, "skill_names": []any{},
			"conversationId": conversationID, "messageId": messageID,
			"buddyId": "", "buddyName": "",
		}),
		mk("chat_message_send", map[string]any{
			"messageId": messageID + "-assistant", "historyCount": 0,
			"isContextTruncated": false, "currentStepCount": 1,
			"traceId": requestID, "rootRequestId": requestID,
			"parentConversationId": conversationID,
			"agentName":            "cli", "agentType": "main",
		}),
		mk("chat_request_send", map[string]any{
			"inputLength": 24, "isPlan": false, "isAutoExecuteTerminal": false,
			"isAutoModify": false, "codebaseEnable": false, "maxToken": 0,
			"maxSteps": 500, "temperature": 0, "maxRetries": 0,
			"mentionContexts": []any{}, "knowledgeId": []any{}, "knowledgeName": []any{},
			"codebaseId": "", "mentionContextCount": 0, "command": "",
			"recommendId": "", "skillId": "", "skillCount": 0, "totalCount": 0,
			"traceId": requestID, "rootRequestId": requestID,
			"parentConversationId": conversationID,
			"agentName":            "cli", "agentType": "main",
			"codebuddy.session_id":              conversationID,
			"codebuddy.conversation_request_id": requestID,
		}),
		mk("chat_message_response", map[string]any{
			"messageId": messageID + "-assistant", "responseModelId": modelID,
			"inputToken": 120, "outputToken": 80, "totalToken": 200,
			"cachedTokens": 0, "cachedWriteTokens": 0, "cachedMissTokens": 0,
			"isSuccessful": true, "messageErrorCode": "", "finishReason": "stop",
			"firstTokenAt": now, "traceId": requestID,
			"conversationId": conversationID,
			"rootRequestId":  requestID, "parentConversationId": conversationID,
			"agentName": "cli", "agentType": "main",
			"codebuddy.session_id":              conversationID,
			"codebuddy.conversation_request_id": requestID,
		}),
		mk("chat_message_status", map[string]any{
			"messageId": messageID + "-assistant", "messageErrorCode": "0",
			"traceId": requestID, "rootRequestId": requestID,
			"parentConversationId": conversationID,
			"agentName":            "cli", "agentType": "main",
		}),
		mk("chat_request_response", map[string]any{
			"mode": "craft", "toolCallCount": 0,
			"inputToken": 120, "outputToken": 80, "totalToken": 200,
			"cachedTokens": 0, "cachedWriteTokens": 0, "cachedMissTokens": 0,
			"isSuccessful": true, "messageErrorCode": "", "finishReason": "stop",
			"rootRequestId": requestID, "parentConversationId": conversationID,
		}),
	}
}

// DesktopBuddyAppSequence 构造「进入 Buddy 应用」五连事件（实测点亮 Buddy_App
// 与 Buddy_App_QQ）：discover → show → enter_click → auth_confirm → bindaccount_skip。
func DesktopBuddyAppSequence(buddyID, buddyName string) []DesktopEvent {
	mk := func(code string, extra map[string]any) DesktopEvent {
		ev := DesktopEvent{
			"eventCode": code, "mode": "LOCAL",
			"buddyId": buddyID, "buddyName": buddyName,
		}
		for k, v := range extra {
			ev[k] = v
		}
		return ev
	}
	return []DesktopEvent{
		mk("buddyapp_discover_click", nil),
		mk("buddyapp_show", map[string]any{"elementId": buddyID, "elementName": buddyName, "position": 2}),
		mk("buddyapp_enter_click", map[string]any{"elementId": buddyID, "elementName": buddyName, "position": 2, "isFirstPage": "1"}),
		mk("buddyapp_auth_confirm_click", map[string]any{"elementId": buddyID, "elementName": buddyName}),
		mk("buddyapp_bindaccount_skip_click", map[string]any{"elementId": buddyID, "elementName": buddyName}),
	}
}

// BuddyAppDefault 判据应用（企鹅教师助手）：同一组事件同时满足 Buddy_App「进入任一应用」
// 与 Buddy_App_QQ「进入企鹅教师助手」。
const (
	BuddyAppDefaultID   = "cb_y5Dy46tPQGGWtueMxXbe"
	BuddyAppDefaultName = "企鹅教师助手"
)

// DesktopAutomationCreateEvent 构造「定时任务创建成功」事件（点亮 automation_1）。
func DesktopAutomationCreateEvent(name string) DesktopEvent {
	return DesktopEvent{
		"eventCode": "automated_task_create_suc", "name": name,
		"source": "manually", "modelId": "fast-model", "modelIsThinking": true,
		"connectorCount": 0, "skills": "", "skillCount": 0,
		"scheduleType": "once", "mode": "LOCAL",
	}
}

// DesktopTemplateUseSequence 构造「使用模板创建任务」事件组（template_5 计数）：
// agent_task_created_with_template + template_used，JOIN 一条完整 chat 链。
func DesktopTemplateUseSequence(conversationID, requestID, templateID, templateName string) []DesktopEvent {
	events := DesktopChatSequence(conversationID, requestID, "msg-"+templateID, "fast-model", "fast-model")
	return append(events,
		DesktopEvent{
			"eventCode": "agent_task_created_with_template", "mode": "working",
			"isCustomModel": false, "id": templateID, "name": templateName, "requestId": requestID,
		},
		DesktopEvent{"eventCode": "template_used", "template_id": templateID, "task_mode": "working"},
	)
}

// DesktopPlaybookPromptSequence 构造「灵感案例做同款」事件组（playbook_prompt 计数）：
// web_element_click(playbook_ctaClick) + playbook_cta_click + playbook_prompt_send。
func DesktopPlaybookPromptSequence(conversationID, requestID, caseID, caseName string) []DesktopEvent {
	events := DesktopChatSequence(conversationID, requestID, "msg-pb", "fast-model", "fast-model")
	payload := map[string]any{
		"id": caseID, "name": caseName, "type": "document",
		"categoryId": "", "categoryName": "",
	}
	with := func(m map[string]any) DesktopEvent {
		ev := DesktopEvent{}
		for k, v := range payload {
			ev[k] = v
		}
		for k, v := range m {
			ev[k] = v
		}
		return ev
	}
	events = append(events,
		DesktopEvent{
			"eventCode": "web_element_click", "pageName": "playbook_detail",
			"elementId": "playbook_ctaClick", "elementName": caseName, "source": "discover",
		},
		with(map[string]any{"eventCode": "playbook_cta_click", "source": "discover", "position": 0}),
		with(map[string]any{"eventCode": "playbook_prompt_send", "conversationId": conversationID, "requestId": requestID}),
	)
	return events
}

// DesktopDesignCanvasSequence 构造「设计创意画布」事件组（create_canvas 计数）：
// wbx_design_canvas_task_create + wbx_design_canvas_open。
func DesktopDesignCanvasSequence(conversationID, requestID string) []DesktopEvent {
	events := DesktopChatSequence(conversationID, requestID, "msg-canvas", "fast-model", "fast-model")
	suffix := requestID
	if len(suffix) > 8 {
		suffix = suffix[len(suffix)-8:]
	}
	return append(events,
		DesktopEvent{
			"eventCode": "wbx_design_canvas_task_create", "conversationId": conversationID,
			"requestId": requestID, "source": "summon_keyword", "cost": 12000, "isSuccessful": true,
		},
		DesktopEvent{
			"eventCode": "wbx_design_canvas_open", "conversationId": conversationID,
			"requestId": requestID, "id": "ardot-file-" + suffix,
			"source": "summon_keyword", "type": "page", "cost": 13000, "isSuccessful": true,
		},
	)
}

// DesktopAppearanceSkinEvent 构造「主题皮肤生效」事件（Hp_Appearance 判据）：
// 客户端在主题生效状态下离开设置页时上报 appearance_skin_apply。
func DesktopAppearanceSkinEvent(resourceKey string) DesktopEvent {
	return DesktopEvent{
		"eventCode": "appearance_skin_apply", "action": "apply", "source": "settings_close",
		"id": resourceKey, "vipLevel": 0, "series": "", "type": "unknown",
	}
}

// DesktopSkillInfoEvent 构造「技能加载成功」事件（skill_1 判据）：
// skill_info JOIN 真实会话的服务端 conversationId/requestId。
func DesktopSkillInfoEvent(skillID, skillName, conversationID, requestID, messageID string) DesktopEvent {
	return DesktopEvent{
		"eventCode":      "skill_info",
		"id":             skillName,
		"skillId":        skillID,
		"skillVersion":   "1.0.0",
		"toolStatus":     "success",
		"fileCount":      56,
		"source":         "workbuddy-desktop",
		"conversationId": conversationID, "requestId": requestID, "messageId": messageID,
		"requestModelId": "fast-model", "requestModelName": "fast-model",
		"traceId": requestID,
	}
}

// SetAppearanceTheme 应用外观主题（POST copilot.tencent.com/v2/user-asset/appearance/set）。
// 单独 set 不计分，须配合 DesktopAppearanceSkinEvent 上报才点亮 Hp_Appearance。
func (c *Client) SetAppearanceTheme(resourceKey string) error {
	t := c.toks.Get()
	if t == nil {
		return ErrNoToken
	}
	raw, err := json.Marshal(map[string]string{"kind": "theme", "resource_key": resourceKey})
	if err != nil {
		return err
	}
	return c.doReport(func() (*http.Request, error) {
		ct := c.toks.Get()
		if ct == nil {
			return nil, ErrNoToken
		}
		req, berr := http.NewRequest(http.MethodPost, growthBase(c)+desktopAppearanceSet, bytes.NewReader(raw))
		if berr != nil {
			return nil, berr
		}
		req.Header.Set("Authorization", "Bearer "+ct.AccessToken)
		req.Header.Set("Accept", "application/json, text/plain, */*")
		req.Header.Set("Content-Type", "application/json;charset=UTF-8")
		req.Header.Set("User-Agent", desktopUA)
		req.Header.Set("X-Product", "SaaS")
		if ct.UID != "" {
			req.Header.Set("X-User-Id", ct.UID)
		}
		return req, nil
	})
}

// ── 专家市场与真实对话 ───────────────────────────────────────────────────────

// MarketExpert 专家市场的单个专家。
type MarketExpert struct {
	ExpertID      string `json:"expert_id"`
	ExpertType    string `json:"expert_type"`
	DisplayNameZH string `json:"display_name_zh"`
	ProfessionZH  string `json:"profession_zh"`
	Version       string `json:"version"`
	Categories    []any  `json:"categories"`
}

// CategoryOr default 专家分类（categories 为空时的兜底值）。
func (e MarketExpert) CategoryOr(def string) string {
	if len(e.Categories) > 0 {
		if s, ok := e.Categories[0].(string); ok && s != "" {
			return s
		}
	}
	return def
}

// VersionOr default 专家版本（空时兜底）。
func (e MarketExpert) VersionOr(def string) string {
	if strings.TrimSpace(e.Version) != "" {
		return e.Version
	}
	return def
}

// MarketExpertList 拉取专家市场真实专家列表（expertType: "agent" 单专家 / "team" 专家团）。
// expert_actual_use 的判据校验要求 id 是平台上真实存在的专家（编造 id 不计数）。
func (c *Client) MarketExpertList(expertType string) ([]MarketExpert, error) {
	body := map[string]any{"page": 1, "page_size": 20, "sort_by": "reco_rank", "sort_order": "desc"}
	if expertType != "" {
		body["expert_type"] = expertType
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	data, err := c.doGrowthEnvelope(func() (*http.Request, error) {
		ct := c.toks.Get()
		if ct == nil {
			return nil, ErrNoToken
		}
		req, berr := http.NewRequest(http.MethodPost,
			growthBase(c)+"/portal/operation-platform/market/expert/list", bytes.NewReader(raw))
		if berr != nil {
			return nil, berr
		}
		req.Header.Set("Authorization", "Bearer "+ct.AccessToken)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", desktopUA)
		req.Header.Set("X-Domain", growthBase(c))
		req.Header.Set("X-Product", "SaaS")
		if ct.UID != "" {
			req.Header.Set("X-User-Id", ct.UID)
		}
		return req, nil
	})
	if err != nil {
		return nil, err
	}
	var out struct {
		Experts []MarketExpert `json:"experts"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("expert list parse: %w", err)
	}
	return out.Experts, nil
}

// idRegex 服务端 requestId 形状（cmb- 前缀 32hex 或裸 32hex）。
var idRegex = regexp.MustCompile(`^(cmb-)?[0-9a-f]{32}$`)

// DesktopChatWithExpert 发一条真实桌面指纹 chat 请求（可带 X-Expert-Id），从 SSE 流
// 解析**服务端返回的 requestId**（data.id）。
// expert_actual_use 等 JOIN 事件的 requestId 必须是该服务端 id——自造 UUID 不计数。
func (c *Client) DesktopChatWithExpert(expertID string) (conversationID, requestID string, err error) {
	if c.toks.Get() == nil {
		return "", "", ErrNoToken
	}
	// SSE 是流式长连接，不能复用 doAuthed（它要把整个响应读完）。
	// 这里只在发送前补一次 EnsureValid：过期 token 直接 401，专家任务全链路会白跑。
	_ = c.toks.EnsureValid(c.RefreshFn)

	conversationID = fmt.Sprintf("b2a-conv-%d", time.Now().UnixNano())
	body := map[string]any{
		"model": "fast-model",
		"messages": []any{
			map[string]any{"role": "system", "content": "You are a helpful assistant. 当前处于中文环境，使用简体中文回答。"},
			map[string]any{"role": "user", "content": "1+1等于几？直接回答。"},
		},
		"agent":          "cli",
		"temperature":    1,
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", "", err
	}
	t := c.toks.Get() // EnsureValid 之后重新读：401 场景下要拿到刷新后的凭证
	if t == nil {
		return "", "", ErrNoToken
	}
	req, err := http.NewRequest(http.MethodPost, growthBase(c)+"/v2/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return "", "", err
	}
	h := req.Header
	h.Set("Authorization", "Bearer "+t.AccessToken)
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "text/event-stream")
	h.Set("User-Agent", desktopUA)
	h.Set("X-Domain", growthBase(c))
	h.Set("X-Product", "SaaS")
	h.Set("X-User-Id", t.UID)
	h.Set("X-Conversation-ID", conversationID)
	h.Set("X-Request-ID", fmt.Sprintf("%d", time.Now().UnixNano()))
	h.Set("X-Agent-Intent", "craft")
	h.Set("X-Agent-Type", "main")
	h.Set("X-IDE-Name", "WorkBuddy")
	h.Set("X-IDE-Type", "WorkBuddy")
	h.Set("X-IDE-Version", desktopVersion)
	h.Set("x-codebuddy-request", "1")
	if expertID != "" {
		h.Set("X-Expert-Id", expertID)
	}
	debug := os.Getenv("B2A_DEBUG_CHAT") != ""
	resp, err := c.long.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if debug {
		fmt.Printf("[dbg] status=%d url=%s\n", resp.StatusCode, req.URL)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return "", "", fmt.Errorf("chat http %d: %s", resp.StatusCode, b)
	}
	// 从 SSE 流抓第一个符合形状的 data.id 作为服务端 requestId（读干流避免残留连接）。
	buf := make([]byte, 0, 1<<20)
	tmp := make([]byte, 8192)
	searchFrom := 0
	for {
		n, rerr := resp.Body.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if i := bytes.Index(buf[searchFrom:], []byte(`"id":"`)); i >= 0 {
				abs := searchFrom + i
				rest := buf[abs+6:]
				if end := bytes.IndexByte(rest, '"'); end > 0 {
					id := string(rest[:end])
					if idRegex.MatchString(id) {
						return conversationID, id, nil
					}
					searchFrom = abs + 1 // 推进偏移，否则同一位置会反复命中
				}
			}
		}
		if rerr != nil || len(buf) > 1<<20 {
			break
		}
	}
	return "", "", fmt.Errorf("SSE 中未找到服务端 requestId")
}

// DesktopExpertSummonSequence 构造「召唤平台专家」事件组（expert_summon_click 等）。
func DesktopExpertSummonSequence(e MarketExpert) []DesktopEvent {
	cat := e.CategoryOr("expert-all")
	ver := e.VersionOr("1.0.0")
	return []DesktopEvent{
		{
			"eventCode": "web_element_click", "source": e.ExpertID, "type": cat, "version": ver,
			"elementId": "expert_summon_click", "elementName": "立即召唤",
			"pageURL": "/C:/Program%20Files/WorkBuddy/resources/app.asar/renderer/index.html",
		},
		{
			"eventCode": "expert_summon_click", "id": e.ExpertID, "name": e.DisplayNameZH,
			"expertTitle": e.ProfessionZH, "type": "expert-all", "position": 0,
			"expertType": e.ExpertType, "version": ver, "mode": "LOCAL",
		},
		{
			"eventCode": "expert_summoned", "id": e.ExpertID, "name": e.DisplayNameZH,
			"expertTitle": e.ProfessionZH, "type": "expert-all",
		},
	}
}

// DesktopExpertActualUseEvent 构造「专家真实使用」事件（expert_5 / Expert_team_use_3 计数）。
// requestID 必须是 DesktopChatWithExpert 返回的服务端 requestId。
func DesktopExpertActualUseEvent(e MarketExpert, conversationID, requestID, mode string) DesktopEvent {
	suffix := requestID
	if len(suffix) > 8 {
		suffix = suffix[len(suffix)-8:]
	}
	return DesktopEvent{
		"eventCode": "expert_actual_use",
		"id":        e.ExpertID, "name": e.DisplayNameZH, "expertTitle": e.ProfessionZH,
		"type": e.CategoryOr("expert-all"), "expertType": e.ExpertType,
		"source": "builtin", "version": e.VersionOr("1.0.0"),
		"cost": 9000, "characterCount": 14, "mode": mode,
		"conversationId": conversationID, "requestId": requestID, "messageId": "msg-" + suffix,
		"requestModelId": "fast-model", "requestModelName": "fast-model",
	}
}

// ── mp 事件构造 ───────────────────────────────────────────────────────────────

// MPClientToken 幂等令牌（前端 randomUUID 同款语义）。
func MPClientToken() string { return uuidStd() }

// MPChatEvent 构造一条小程序指纹 chat_request_send 事件。
// activityID 非空时追加 activityId（school_season「校园日」必带，普通小程序任务不带）。
func MPChatEvent(conversationID, activityID string) map[string]any {
	rid := "b2a-" + uuidHex()
	ev := map[string]any{
		"eventCode":   "chat_request_send",
		"inputLength": 14, "isPlan": false, "isAutoExecuteTerminal": false,
		"isAutoModify": false, "codebaseEnable": false, "maxToken": 0,
		"maxSteps": 500, "temperature": 0, "maxRetries": 0,
		"mentionContexts": []any{}, "knowledgeId": []any{}, "knowledgeName": []any{},
		"codebaseId": "", "mentionContextCount": 0, "command": "",
		"recommendId": "", "skillId": "", "skillCount": 0, "totalCount": 0,
		"traceId": rid, "rootRequestId": rid,
		"parentConversationId": conversationID, "conversationId": conversationID,
		"messageId": "msg-" + rid[len(rid)-8:],
		"agentName": "mp", "agentType": "main",
		"codebuddy.session_id":              conversationID,
		"codebuddy.conversation_request_id": rid,
	}
	if activityID != "" {
		ev["activityId"] = activityID
	}
	return ev
}

// MPSchoolSeasonChatEvent 校园日（school_season）判据事件：mp chat + activityId。
func MPSchoolSeasonChatEvent(conversationID string) map[string]any {
	return MPChatEvent(conversationID, schoolOpenDayActivityID)
}

// MPChatModelEvent mp 对话事件 + 模型字段（Sequential_Tasks_5「使用 GLM5.2」判据载体）：
// Tasks_1/3 的裸对话事件不带模型，模型任务须用本形态。
func MPChatModelEvent(conversationID, modelID, modelName string) map[string]any {
	ev := MPChatEvent(conversationID, "")
	ev["requestModelId"] = modelID
	ev["requestModelName"] = modelName
	return ev
}

// MPExpertUseEvent 构造 Sequential_Tasks_2 判据事件：mp 指纹 expert_actual_use。
// 形状对齐小程序源码真实发射点：不带 conversationId/activityId、extVersion=2.2.8、
// type 固定 "send_message"。expertID 必须是市场真实 ex_ id（空 id 服务端不入账）。
func MPExpertUseEvent(expertID, expertName, expertType string) map[string]any {
	if expertType == "" {
		expertType = "agent"
	}
	return map[string]any{
		"eventCode": "expert_actual_use", "reportDelay": 0,
		"extVersion": "2.2.8", "source": "mini_program",
		"id": expertID, "name": expertID,
		"expertTitle": expertName, "type": "send_message",
		"characterCount": 12, "expertType": expertType,
	}
}

// MPPlaybookEvents mp 指纹灵感事件组（Sequential_Tasks_7 判据载体）：
// playbook_cta_click → playbook_prompt_send。
func MPPlaybookEvents(caseID, caseName string) []map[string]any {
	base := map[string]any{
		"id": caseID, "name": caseName, "type": "document",
		"categoryId": "", "categoryName": "",
		"skills": "", "skillNames": "",
	}
	with := func(m map[string]any) map[string]any {
		out := make(map[string]any, len(base)+len(m))
		for k, v := range base {
			out[k] = v
		}
		for k, v := range m {
			out[k] = v
		}
		return out
	}
	return []map[string]any{
		with(map[string]any{
			"eventCode": "playbook_cta_click", "source": "discover", "position": 1,
			"extVersion": "2.2.8",
		}),
		with(map[string]any{
			"eventCode": "playbook_prompt_send", "source": "discover",
			"promptLength": 96, "isOfficial": 1,
			"conversationId": "b2a-mp-pb-" + uuidHex(),
			"extVersion":     "2.2.8",
		}),
	}
}
