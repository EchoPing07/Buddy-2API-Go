// Package proxy 实现 OpenAI 兼容业务端点（/v1/chat/completions、/v1/models、/health）。
package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"buddy2api-go/internal/apikey"
	"buddy2api-go/internal/config"
	"buddy2api-go/internal/pool"
	"buddy2api-go/internal/store"
	"buddy2api-go/internal/upstream"
)

// passthroughBodyKeys 透传字段白名单。
var passthroughBodyKeys = []string{
	"model", "messages", "tools", "tool_choice", "temperature",
	"max_tokens", "max_completion_tokens", "top_p", "stream",
	"stream_options", "stop", "presence_penalty", "frequency_penalty",
	"n", "response_format", "seed", "user", "reasoning_effort",
	"verbosity", "reasoning_summary",
}

// thoughtPrefixRE 上游 reasoning_content 的 "Thought: Xms\n" 前缀。
var thoughtPrefixRE = regexp.MustCompile(`(?s)^\s*Thought: \d+ms\n`)

const maxBodySize = 48 << 20 // 48MB

// Handler 业务端点处理器。
type Handler struct {
	client *upstream.Client
	models *upstream.ModelCache
	keys   *apikey.Manager
	st     *store.Store
	cfg    *config.Manager
	// pool 号池（实验特性）。nil = 关闭：走单账号路径，零行为变化。
	pool *pool.Manager

	// 日志写入队列（带缓冲，避免请求路径上同步写库）
	logCh   chan *store.LogEntry
	logOnce sync.Once
}

// New 创建处理器并启动异步日志 worker。
func New(client *upstream.Client, models *upstream.ModelCache, keys *apikey.Manager, st *store.Store, cfg *config.Manager, p *pool.Manager) *Handler {
	h := &Handler{
		client: client, models: models, keys: keys, st: st, cfg: cfg, pool: p,
		logCh: make(chan *store.LogEntry, 256),
	}
	h.logOnce.Do(func() { go h.logWorker() })
	return h
}

// Pool 返回号池管理器（nil = 实验未开启）。admin 用它接号池管理端点。
func (h *Handler) Pool() *pool.Manager { return h.pool }

func (h *Handler) logWorker() {
	for l := range h.logCh {
		if err := h.st.InsertLog(l); err != nil {
			slog.Error("写请求日志失败", "error", err)
		}
	}
}

func (h *Handler) writeLog(l *store.LogEntry) {
	if l.CreatedAt == 0 {
		l.CreatedAt = time.Now().Unix()
	}
	select {
	case h.logCh <- l:
	default:
		slog.Warn("日志队列已满，丢弃一条日志")
	}
}

func openaiError(w http.ResponseWriter, status int, errType, code, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"error":{"message":%q,"type":%q,"code":%q}}`, msg, errType, code)
}

// upstreamErrorMessage 解析上游错误体提取可读信息；非 JSON 或解析失败时回退截断原文，
// 避免把上游 HTML/非 JSON 错误体原样当作 JSON 透传给客户端。
//
// 用 upstream.BizCode 统一解包（顶层的 {code,msg} 与嵌套的 {error:{data:{code,msg}}}
// 都能解），否则 14018 这类额度耗尽的错误会解析失败，前端只能看到一大段截断原文。
// isErrorFrame 判断流内 error 字段是否是**真实**错误帧。
//
// "error":null 会被 json.RawMessage 解析成字面量 null（4 字节），
// 许多 OpenAI 兼容上游在正常 chunk 里就这么写。不排除它会让第一个正常帧
// 就把「本流已记过错误」的标志置位，后续真正的错误帧冷却被吞掉。
func isErrorFrame(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	t := strings.TrimSpace(string(raw))
	return t != "" && t != "null"
}

func upstreamErrorMessage(raw []byte, status int) string {
	code, msg := upstream.BizCode(raw)
	if code != 0 || msg != "" {
		if msg != "" {
			return fmt.Sprintf("上游返回 %d（code=%d）: %s", status, code, trunc(msg, 300))
		}
		return fmt.Sprintf("上游返回 %d（code=%d）", status, code)
	}
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return fmt.Sprintf("上游返回 %d 且响应体为空", status)
	}
	return fmt.Sprintf("上游返回 %d: %s", status, trunc(s, 300))
}

// ── chat/completions ──

// Chat 处理 POST /v1/chat/completions。
//
// 号池开关关闭时走原路径（chatSingle），行为逐字不变；开启时按会话粘性分发并
// 允许换号重试。两个分支共享同一套 body 解析 / 日志收尾，只差「用哪份凭证请求」。
func (h *Handler) Chat(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	key := apikey.FromContext(r.Context())

	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodySize))
	if err != nil {
		openaiError(w, http.StatusBadRequest, "invalid_request_error", "read_body_failed", "读取请求体失败: "+err.Error())
		return
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		openaiError(w, http.StatusBadRequest, "invalid_request_error", "invalid_json", "请求体不是合法 JSON: "+err.Error())
		return
	}
	model, _ := body["model"].(string)
	if model == "" {
		openaiError(w, http.StatusBadRequest, "invalid_request_error", "missing_model", "缺少 model 字段")
		return
	}
	streamWanted := false
	if v, ok := body["stream"].(bool); ok {
		streamWanted = v
	}

	upBody := buildUpstreamBody(body)

	logEntry := &store.LogEntry{
		APIKeyID: keyID(key), APIKeyName: keyName(key),
		Model: model, Stream: streamWanted,
	}
	defer func() {
		logEntry.DurationMs = time.Since(start).Milliseconds()
		h.writeLog(logEntry)
		if key != nil {
			h.keys.RecordUsage(key.ID, int64(logEntry.TotalTokens))
		}
	}()

	// 号池关闭（或未构造）：单账号路径，不加任何额外分支开销。
	if h.pool == nil || !h.pool.Enabled() {
		h.chatSingle(w, r, upBody, body, streamWanted, logEntry)
		return
	}
	h.chatPooled(w, r, upBody, body, streamWanted, logEntry, key)
}

// chatSingle 单账号 chat（号池关闭时的唯一路径，也是号池耗尽后的参考行为）。
func (h *Handler) chatSingle(w http.ResponseWriter, r *http.Request, upBody []byte, body map[string]any, streamWanted bool, logEntry *store.LogEntry) {
	resp, err := h.client.DoChat(r.Context(), upBody)
	if err != nil {
		logEntry.StatusCode = http.StatusServiceUnavailable
		logEntry.ErrorMsg = trunc(err.Error(), 500)
		openaiError(w, http.StatusServiceUnavailable, "upstream_error", "upstream_unavailable", err.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errRaw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		logEntry.StatusCode = resp.StatusCode
		logEntry.ErrorMsg = trunc(string(errRaw), 500)
		openaiError(w, resp.StatusCode, "upstream_error", "upstream_error", upstreamErrorMessage(errRaw, resp.StatusCode))
		return
	}

	if streamWanted {
		h.passthroughSSE(w, r, resp.Body, logEntry, nil)
	} else {
		h.aggregate(r, resp.Body, body, upBody, logEntry, w, nil, h.client.DoChat)
	}
}

// chatPooled 号池 chat：按会话粘性选号，遇到可重试错误时换号重试。
//
// 重试只给 429/402/401（上游明确拒绝、未开工），5xx 与网络中断**不重试**：
// 上游可能已计费，重试有重复扣费风险，交给调用方按自己的退避决定
// （pi 对 429/5xx 都会重试）。这个取值也叫「网关重试 × pi 重试」的乘法上限：
// 实测 pi 对 429 会重试 3 次，网关取 2 → 一次失败最多 12 次上游调用。
func (h *Handler) chatPooled(w http.ResponseWriter, r *http.Request, upBody []byte, body map[string]any,
	streamWanted bool, logEntry *store.LogEntry, key *apikey.KeyInfo) {

	// routeModel 是**路由与冷却的唯一口径**，取自客户端请求体里声明的 model。
	//
	// 不能用 logEntry.Model：流式路径会把它覆盖成上游 chunk 回报的 model（可能是
	// "glm-5.3" 这种别名，也可能为空串）。若冷却记在别名上，而选号查询用的是请求
	// model，冷却就对路由完全不可见——被限流的号会被反复分配。空 model 还会把冷却
	// 写到 "" 这个键上，污染整个池。
	routeModel, _ := body["model"].(string)
	sessKey := pool.SessionKey(r.Header.Get, body, keyID(key))
	tried := map[string]bool{}
	maxRetry := h.pool.MaxRetry()

	// lastStatus/lastRaw 保存最后一次上游错误：候选耗尽时优先透传它。
	// 没有这两个变量的话，「账号数 ≤ 1+maxRetry」这种最常见的小池配置会让
	// 真实 429 被改写成网关自造的 503 —— pi 靠状态码决定要不要重试，
	// 改码会改变它的行为，也会让用户看不到上游原文（含 requestId）。
	lastStatus := 0
	var lastRaw []byte
	attempts := 0

	for attempt := 0; ; attempt++ {
		uid, ok := h.pool.Resolve(sessKey, routeModel, tried)
		if !ok {
			// 无候选：若已经打过上游并拿到错误，透传那个错误而不是自造 503
			if lastStatus != 0 {
				h.writeUpstreamFailure(w, logEntry, lastStatus, lastRaw, attempts)
				return
			}
			h.writePoolExhausted(w, logEntry, routeModel, len(tried))
			return
		}
		tried[uid] = true
		attempts++
		logEntry.AccountUID = uid // 每轮覆盖，最后一次即最终生效号

		ts, err := h.pool.TokenStore(uid)
		if err != nil {
			if lastStatus != 0 {
				h.writeUpstreamFailure(w, logEntry, lastStatus, lastRaw, attempts)
				return
			}
			h.writePoolExhausted(w, logEntry, routeModel, len(tried))
			return
		}
		resp, err := h.client.DoChatWith(r.Context(), ts, upBody)
		if err != nil {
			// transport 类（DNS / 连接 / 超时 / http2）：不重试、不惩罚账号，直接透传。
			// 实测库里 73 条 503 全是这类，按失败禁用会让一次断网把全池打成禁用。
			logEntry.StatusCode = http.StatusServiceUnavailable
			logEntry.ErrorMsg = trunc(err.Error(), 500)
			openaiError(w, http.StatusServiceUnavailable, "upstream_error", "upstream_unavailable", err.Error())
			return
		}

		if resp.StatusCode == http.StatusOK {
			h.pool.NoteSuccess(uid, routeModel)
			// 成功即重绑：让后续轮次收敛到持续成功的号，而不是跳回已被冷却的原号。
			h.pool.Bind(sessKey, routeModel, uid)
			if streamWanted {
				h.passthroughSSE(w, r, resp.Body, logEntry, func(kind upstream.ChatErrKind, raw []byte) {
					// 流内错误帧：不重试（字节已发出），但**必须记冷却**，否则下一轮
					// 还会被分到同一个已限额的号上。
					h.pool.NoteError(uid, routeModel, kind, raw)
				})
			} else {
				// 工具停转重试必须复用同一个 uid 的凭证（见 aggregate 的 doChat 说明）
				doChatWith := func(ctx context.Context, b []byte) (*http.Response, error) {
					return h.client.DoChatWith(ctx, ts, b)
				}
				h.aggregate(r, resp.Body, body, upBody, logEntry, w, func(kind upstream.ChatErrKind, raw []byte) {
					h.pool.NoteError(uid, routeModel, kind, raw)
				}, doChatWith)
			}
			resp.Body.Close()
			return
		}

		// 非 200：读完错误体（限量）后分类。必须读完并关闭，否则连接不能复用。
		errRaw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		kind := upstream.ClassifyChatError(resp.StatusCode, errRaw)
		h.pool.NoteError(uid, routeModel, kind, errRaw)
		lastStatus, lastRaw = resp.StatusCode, errRaw

		if !kind.Retryable() || attempt >= maxRetry {
			h.writeUpstreamFailure(w, logEntry, resp.StatusCode, errRaw, attempts)
			return
		}
		// 可重试：只解绑「失败的正是本会话绑定的号」，其它号的失败不影响本会话绑定。
		h.pool.UnbindIf(sessKey, routeModel, uid)
		slog.Info("号池换号重试", "model", routeModel, "failed_uid", shortUID(uid),
			"kind", kind.String(), "attempt", attempt+1)
	}
}

// writeUpstreamFailure 透传上游错误。HTTP 状态码与错误体尽量原样：
// pi 靠状态码决定要不要重试，改码会改变它的行为，也会让用户看不到上游原文。
func (h *Handler) writeUpstreamFailure(w http.ResponseWriter, logEntry *store.LogEntry,
	status int, raw []byte, attempts int) {
	logEntry.StatusCode = status
	logEntry.ErrorMsg = trunc(string(raw), 500)
	msg := upstreamErrorMessage(raw, status)
	if attempts > 1 {
		msg = fmt.Sprintf("%s（号池：已尝试 %d 个账号均失败）", msg, attempts)
	}
	openaiError(w, status, "upstream_error", "upstream_error", msg)
}

// writePoolExhausted 全池无可用账号：网关自造的 503。
//
// 这是唯一用 503 的场景：此时没有任何上游错误可透传，必须自己说清是号池状态
// 而非上游故障，否则用户会去找 CodeBuddy 客服。pi 收到 503 会重试 4 次，
// 但每次都被网关立即拒绝（无上游成本），可接受。
func (h *Handler) writePoolExhausted(w http.ResponseWriter, logEntry *store.LogEntry, model string, triedCount int) {
	msg := fmt.Sprintf("号池内没有可用于 %s 的账号（共 %d 个账号，已检查 %d 个）。",
		model, len(h.pool.Members()), triedCount)
	msg += "可能原因：账号被禁用、额度用尽、或该模型正在限额冷却中。请到管理后台「账号」页查看号池状态。"
	logEntry.StatusCode = http.StatusServiceUnavailable
	logEntry.ErrorMsg = msg
	openaiError(w, http.StatusServiceUnavailable, "upstream_error", "pool_exhausted", msg)
}

func shortUID(uid string) string {
	if len(uid) <= 8 {
		return uid
	}
	return uid[len(uid)-8:]
}

// buildUpstreamBody 白名单透传 + 强制 stream:true + include_usage。
func buildUpstreamBody(body map[string]any) []byte {
	out := make(map[string]any, len(body)+2)
	for _, k := range passthroughBodyKeys {
		if v, ok := body[k]; ok {
			out[k] = v
		}
	}
	out["stream"] = true
	out["stream_options"] = map[string]bool{"include_usage": true}
	raw, _ := json.Marshal(out)
	return raw
}

// sseChunk 最小化解析 SSE chunk 需要的字段。
type sseChunk struct {
	ID      string `json:"id"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int    `json:"index"`
		FinishReason string `json:"finish_reason"`
		Delta        struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *usageJSON `json:"usage"`
	// Error 流内错误帧（HTTP 200 已开流，上游在流里报错）。
	// 实测形态：{"error":{"message":"...","code":"6004","requestId":"..."}}，
	// code 是字符串而非数字，故用 BizCode 统一解（见 upstream.ClassifyChatError）。
	Error json.RawMessage `json:"error"`
}

type usageJSON struct {
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	TotalTokens      int     `json:"total_tokens"`
	Credit           float64 `json:"credit"`
}

// passthroughSSE 流式透传：逐行转发（Flusher 实时），末尾补全 finish_reason。
// 写失败/客户端断开统一标记 clientGone 并在循环出口记录 ErrorMsg，避免日志误判成功。
// onError 流内错误帧回调（号池用；nil = 关闭号池，零开销）。
// 只在侦测到 "error" 键时才调用，不改变任何转发行为。
func (h *Handler) passthroughSSE(w http.ResponseWriter, r *http.Request, body io.Reader, logEntry *store.LogEntry, onError func(upstream.ChatErrKind, []byte)) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}
	logEntry.StatusCode = http.StatusOK

	reader := bufio.NewReaderSize(body, 64<<10)
	sawFinish := false
	notedError := false
	var lastChunk sseChunk
	clientGone := false

	// writeLine 写入一行；失败标记 clientGone（不再从内部 break，统一在循环出口处理）。
	writeLine := func(line string) bool {
		if clientGone {
			return false
		}
		if _, err := io.WriteString(w, line); err != nil {
			clientGone = true
			return false
		}
		return true
	}
	flush := func() {
		if flusher != nil && !clientGone {
			flusher.Flush()
		}
	}

	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			trimmed := strings.TrimRight(line, "\r\n")
			if trimmed == "data: [DONE]" {
				// finish_reason 补全：上游可能发空 finish_reason 直接 DONE
				if !sawFinish {
					synth := map[string]any{
						"id":      orDefault(lastChunk.ID, "chatcmpl-"+fmt.Sprint(time.Now().UnixNano())),
						"object":  "chat.completion.chunk",
						"created": orDefaultInt(lastChunk.Created, time.Now().Unix()),
						"model":   orDefault(lastChunk.Model, logEntry.Model),
						"choices": []any{map[string]any{
							"index":         0,
							"delta":         map[string]any{},
							"finish_reason": "stop",
						}},
					}
					if lastChunk.Usage != nil {
						synth["usage"] = lastChunk.Usage
					}
					if raw, err := json.Marshal(synth); err == nil {
						writeLine("data: " + string(raw) + "\n\n")
					}
				}
				writeLine("data: [DONE]\n\n")
				flush()
				if logEntry.FinishReason == "" {
					logEntry.FinishReason = "stop"
				}
				break
			}
			if strings.HasPrefix(trimmed, "data: ") {
				data := trimmed[6:]
				var chunk sseChunk
				if json.Unmarshal([]byte(data), &chunk) == nil {
					// 流内错误帧：原样透传（字节已发出，不可回滚重发），
					// 但钩子会把它记成冷却，让下一轮请求换个号。
					// noted 保证同一流只记一次：上游可能连发多帧，
					// 重复计数会把「额度连续失败 2 次」的门槛一次推满而误禁用。
					// "error":null 是正常帧的常见写法（json.RawMessage 会存下 null 四字节）。
					// 必须在判类型之前排除：否则第一个 null 帧就把 notedError 置位，
					// 后面真正的错误帧冷却被吞掉（已实测复现）。
					// notedError 也只在该帧确实可惩罚时置位——不可惩罚的帧不该挡住
					// 后面真正的 6004。
					if onError != nil && !notedError && isErrorFrame(chunk.Error) {
						kind := upstream.ClassifyChatError(http.StatusOK, []byte(data))
						if kind.Punishes() {
							notedError = true
							onError(kind, []byte(data))
						}
					}
					if chunk.Model != "" {
						lastChunk.Model = chunk.Model
						logEntry.Model = chunk.Model
					}
					if chunk.ID != "" {
						lastChunk.ID = chunk.ID
					}
					if chunk.Created != 0 {
						lastChunk.Created = chunk.Created
					}
					if chunk.Usage != nil {
						lastChunk.Usage = chunk.Usage
						logEntry.PromptTokens = chunk.Usage.PromptTokens
						logEntry.CompletionTokens = chunk.Usage.CompletionTokens
						logEntry.TotalTokens = chunk.Usage.TotalTokens
						logEntry.Credit = chunk.Usage.Credit
					}
					for _, ch := range chunk.Choices {
						if ch.FinishReason != "" {
							sawFinish = true
							logEntry.FinishReason = ch.FinishReason
						}
					}
				}
			}
			writeLine(line)
			flush()
		}
		if err != nil {
			if err != io.EOF {
				if clientGone {
					logEntry.ErrorMsg = orDefault(logEntry.ErrorMsg, "client canceled")
				} else {
					slog.Error("读取上游 SSE 失败", "error", err)
					logEntry.ErrorMsg = orDefault(logEntry.ErrorMsg, trunc(err.Error(), 300))
				}
			}
			break
		}
		// 客户端断开
		select {
		case <-r.Context().Done():
			clientGone = true
		default:
		}
		if clientGone {
			logEntry.ErrorMsg = orDefault(logEntry.ErrorMsg, "client canceled")
			break
		}
	}
}

// aggregate 非流式聚合 + finish_reason 修正 + 工具停转防护。
// onError 流内错误帧回调（号池用；nil = 关闭号池）。
//
// doChat 决定工具停转重试用哪份凭证：号池必须复用**同一个账号**的凭证，
// 否则重试会打到「当前活动账号」——那个号可能已被禁用/额度耗尽，
// 而且它的费用与日志会记到另一个 uid 名下。
func (h *Handler) aggregate(r *http.Request, body io.Reader, reqBody map[string]any, upBody []byte, logEntry *store.LogEntry, w http.ResponseWriter, onError func(upstream.ChatErrKind, []byte), doChat func(context.Context, []byte) (*http.Response, error)) {
	agg := scanSSE(body, onError)
	if agg == nil {
		logEntry.StatusCode = http.StatusBadGateway
		logEntry.ErrorMsg = "上游未返回有效 SSE"
		openaiError(w, http.StatusBadGateway, "upstream_error", "empty_upstream", "上游未返回有效数据")
		return
	}
	logEntry.StatusCode = http.StatusOK
	applyAggToLog(agg, logEntry)

	// 工具停转防护：带 tools 且历史含 tool 结果，上游以 stop+纯文本结束未调工具 → tool_choice=required 重试一次
	if needToolRetry(reqBody, agg) {
		slog.Info("检测到工具停转，使用 tool_choice=required 重试一次", "model", logEntry.Model)
		var retryBody map[string]any
		if err := json.Unmarshal(upBody, &retryBody); err == nil {
			retryBody["tool_choice"] = "required"
			if raw, err := json.Marshal(retryBody); err == nil {
				if resp, err := doChat(r.Context(), raw); err == nil {
					if resp.StatusCode == http.StatusOK {
						// 工具停转重试复用同一个 uid（轮转在调用方已结束），
						// 故 onError 仍指向同一账号；错误同样记冷却。
						agg2 := scanSSE(resp.Body, onError)
						resp.Body.Close()
						// 重试无论是否采用其结果，上游都已计费：累计 token，避免漏计费
						if agg2 != nil && agg2.Usage != nil {
							logEntry.PromptTokens += agg2.Usage.PromptTokens
							logEntry.CompletionTokens += agg2.Usage.CompletionTokens
							logEntry.TotalTokens += agg2.Usage.TotalTokens
							logEntry.Credit += agg2.Usage.Credit
						}
						if agg2 != nil && len(agg2.ToolCalls) > 0 {
							if agg2.Model != "" {
								logEntry.Model = agg2.Model
							}
							fr := agg2.FinishReason
							if fr == "" {
								fr = "tool_calls"
							}
							logEntry.FinishReason = fr
							agg = agg2 // 重试产出工具调用则采用重试结果
						}
					} else {
						resp.Body.Close()
					}
				}
			}
		}
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write(agg.completionJSON())
}

// toolCallAccum 按 index 合并的 tool_calls 累积器。
type toolCallAccum struct {
	Index int
	ID    string
	Type  string
	Name  string
	Args  strings.Builder
}

// aggregated 非流式聚合结果。
type aggregated struct {
	ID           string
	Created      int64
	Model        string
	Content      strings.Builder
	Reasoning    strings.Builder
	ToolCalls    []*toolCallAccum
	FinishReason string
	Usage        *usageJSON
	sawAny       bool
}

// scanSSE 扫描上游 SSE 流聚合成单个 chat.completion。
func scanSSE(body io.Reader, onError func(upstream.ChatErrKind, []byte)) *aggregated {
	agg := &aggregated{}
	notedError := false
	reader := bufio.NewReaderSize(body, 64<<10)
	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			trimmed := strings.TrimRight(line, "\r\n")
			if strings.HasPrefix(trimmed, "data: ") {
				data := trimmed[6:]
				if data == "[DONE]" {
					break
				}
				var chunk sseChunk
				if json.Unmarshal([]byte(data), &chunk) == nil {
					// "error":null 是正常帧的常见写法（json.RawMessage 会存下 null 四字节）。
					// 必须在判类型之前排除：否则第一个 null 帧就把 notedError 置位，
					// 后面真正的错误帧冷却被吞掉（已实测复现）。
					// notedError 也只在该帧确实可惩罚时置位——不可惩罚的帧不该挡住
					// 后面真正的 6004。
					if onError != nil && !notedError && isErrorFrame(chunk.Error) {
						kind := upstream.ClassifyChatError(http.StatusOK, []byte(data))
						if kind.Punishes() {
							notedError = true
							onError(kind, []byte(data))
						}
					}
					agg.sawAny = true
					if chunk.ID != "" {
						agg.ID = chunk.ID
					}
					if chunk.Created != 0 {
						agg.Created = chunk.Created
					}
					if chunk.Model != "" {
						agg.Model = chunk.Model
					}
					if chunk.Usage != nil {
						agg.Usage = chunk.Usage
					}
					for _, ch := range chunk.Choices {
						if ch.FinishReason != "" {
							agg.FinishReason = ch.FinishReason
						}
						agg.Content.WriteString(ch.Delta.Content)
						agg.Reasoning.WriteString(ch.Delta.ReasoningContent)
						for _, tc := range ch.Delta.ToolCalls {
							var acc *toolCallAccum
							for _, a := range agg.ToolCalls {
								if a.Index == tc.Index {
									acc = a
									break
								}
							}
							if acc == nil {
								acc = &toolCallAccum{Index: tc.Index, Type: "function"}
								agg.ToolCalls = append(agg.ToolCalls, acc)
							}
							if tc.ID != "" {
								acc.ID = tc.ID
							}
							if tc.Type != "" {
								acc.Type = tc.Type
							}
							acc.Name += tc.Function.Name
							acc.Args.WriteString(tc.Function.Arguments)
						}
					}
				}
			}
		}
		if err != nil {
			break
		}
	}
	if !agg.sawAny {
		return nil
	}
	return agg
}

// completionJSON 构造 OpenAI chat.completion 响应。
func (a *aggregated) completionJSON() []byte {
	msg := map[string]any{"role": "assistant"}
	if a.Content.Len() > 0 {
		msg["content"] = a.Content.String()
	} else {
		msg["content"] = nil
	}
	if reasoning := thoughtPrefixRE.ReplaceAllString(a.Reasoning.String(), ""); reasoning != "" {
		msg["reasoning_content"] = reasoning
	}
	if len(a.ToolCalls) > 0 {
		tcs := make([]any, 0, len(a.ToolCalls))
		for _, tc := range a.ToolCalls {
			tcs = append(tcs, map[string]any{
				"id":   orDefault(tc.ID, "call_"+fmt.Sprint(tc.Index)),
				"type": tc.Type,
				"function": map[string]any{
					"name":      tc.Name,
					"arguments": tc.Args.String(),
				},
			})
		}
		msg["tool_calls"] = tcs
	}

	// finish_reason 修正：空则按内容推断
	finish := a.FinishReason
	if finish == "" {
		if len(a.ToolCalls) > 0 {
			finish = "tool_calls"
		} else {
			finish = "stop"
		}
	}

	resp := map[string]any{
		"id":      orDefault(a.ID, "chatcmpl-"+fmt.Sprint(time.Now().UnixNano())),
		"object":  "chat.completion",
		"created": orDefaultInt(a.Created, time.Now().Unix()),
		"model":   a.Model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       msg,
			"finish_reason": finish,
		}},
	}
	if a.Usage != nil {
		resp["usage"] = a.Usage
	}
	raw, _ := json.Marshal(resp)
	return raw
}

// needToolRetry 判断是否触发工具停转防护。
func needToolRetry(reqBody map[string]any, agg *aggregated) bool {
	if _, hasTools := reqBody["tools"]; !hasTools {
		return false
	}
	hasToolResult := false
	if msgs, ok := reqBody["messages"].([]any); ok {
		for _, m := range msgs {
			if mm, ok := m.(map[string]any); ok {
				if role, _ := mm["role"].(string); role == "tool" {
					hasToolResult = true
					break
				}
			}
		}
	}
	if !hasToolResult {
		return false
	}
	return agg.FinishReason == "stop" && len(agg.ToolCalls) == 0 && agg.Content.Len() > 0
}

func applyAggToLog(agg *aggregated, l *store.LogEntry) {
	if agg.Model != "" {
		l.Model = agg.Model
	}
	finish := agg.FinishReason
	if finish == "" {
		if len(agg.ToolCalls) > 0 {
			finish = "tool_calls"
		} else {
			finish = "stop"
		}
	}
	l.FinishReason = finish
	if agg.Usage != nil {
		l.PromptTokens = agg.Usage.PromptTokens
		l.CompletionTokens = agg.Usage.CompletionTokens
		l.TotalTokens = agg.Usage.TotalTokens
		l.Credit = agg.Usage.Credit
	}
}

// ── models / health ──

// Models 处理 GET /v1/models。对话模型后按实验开关追加嵌入模型（挂 /v1/embeddings，
// 默认关闭，开启由管理后台「实验」页控制），便于客户端发现；上游嵌入注册表与 chat
// 模型列表相互独立，故不并入 ModelCache。
func (h *Handler) Models(w http.ResponseWriter, r *http.Request) {
	ids := h.models.List()
	if h.cfg.Get().EmbeddingsEnabled {
		ids = append(ids, upstream.EmbeddingsModel)
	}
	data := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		data = append(data, map[string]any{
			"id":       id,
			"object":   "model",
			"created":  0,
			"owned_by": "codebuddy",
		})
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

// Health 处理 GET /health。
func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	cfg := h.cfg.Get()
	tok := h.client.TokenSummary()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]any{
		"status":    "ok",
		"region":    cfg.Region,
		"has_token": tok != nil,
		"expired":   tok != nil && tok.IsExpired(),
		"version":   Version,
	})
}

// Version 由 main 通过 ldflags 注入（-X buddy2api-go/internal/proxy.Version=...）。
var Version = "dev"

// ── util ──

func keyID(k *apikey.KeyInfo) int64 {
	if k == nil {
		return 0
	}
	return k.ID
}

func keyName(k *apikey.KeyInfo) string {
	if k == nil {
		return ""
	}
	return k.Name
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func orDefaultInt(v, def int64) int64 {
	if v == 0 {
		return def
	}
	return v
}
