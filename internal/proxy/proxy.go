// Package proxy 实现 OpenAI 兼容业务端点（/v1/chat/completions、/v1/models、/health）。
package proxy

import (
	"bufio"
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

	// 日志写入队列（带缓冲，避免请求路径上同步写库）
	logCh   chan *store.LogEntry
	logOnce sync.Once
}

// New 创建处理器并启动异步日志 worker。
func New(client *upstream.Client, models *upstream.ModelCache, keys *apikey.Manager, st *store.Store, cfg *config.Manager) *Handler {
	h := &Handler{
		client: client, models: models, keys: keys, st: st, cfg: cfg,
		logCh: make(chan *store.LogEntry, 256),
	}
	h.logOnce.Do(func() { go h.logWorker() })
	return h
}

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

// ── chat/completions ──

// Chat 处理 POST /v1/chat/completions。
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
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(resp.StatusCode)
		w.Write(errRaw)
		return
	}

	if streamWanted {
		h.passthroughSSE(w, r, resp.Body, logEntry)
	} else {
		h.aggregate(r, resp.Body, body, upBody, logEntry, w)
	}
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
}

type usageJSON struct {
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	TotalTokens      int     `json:"total_tokens"`
	Credit           float64 `json:"credit"`
}

// passthroughSSE 流式透传：逐行转发（Flusher 实时），末尾补全 finish_reason。
func (h *Handler) passthroughSSE(w http.ResponseWriter, r *http.Request, body io.Reader, logEntry *store.LogEntry) {
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
	var lastChunk sseChunk
	clientGone := false

	writeLine := func(line string) bool {
		if _, err := io.WriteString(w, line); err != nil {
			clientGone = true
			return false
		}
		return true
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
						if !writeLine("data: " + string(raw) + "\n\n") {
							break
						}
					}
				}
				if !writeLine("data: [DONE]\n\n") {
					break
				}
				if flusher != nil {
					flusher.Flush()
				}
				if logEntry.FinishReason == "" {
					logEntry.FinishReason = "stop"
				}
				break
			}
			if strings.HasPrefix(trimmed, "data: ") {
				data := trimmed[6:]
				var chunk sseChunk
				if json.Unmarshal([]byte(data), &chunk) == nil {
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
			if !writeLine(line) {
				break
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			if err != io.EOF {
				slog.Error("读取上游 SSE 失败", "error", err)
				if !clientGone {
					logEntry.ErrorMsg = trunc(err.Error(), 300)
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
func (h *Handler) aggregate(r *http.Request, body io.Reader, reqBody map[string]any, upBody []byte, logEntry *store.LogEntry, w http.ResponseWriter) {
	agg := scanSSE(body)
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
				if resp, err := h.client.DoChat(r.Context(), raw); err == nil {
					if resp.StatusCode == http.StatusOK {
						agg2 := scanSSE(resp.Body)
						resp.Body.Close()
						if agg2 != nil && len(agg2.ToolCalls) > 0 {
							agg = agg2 // 重试产出工具调用则采用重试结果
							applyAggToLog(agg, logEntry)
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
func scanSSE(body io.Reader) *aggregated {
	agg := &aggregated{}
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

// Models 处理 GET /v1/models。
func (h *Handler) Models(w http.ResponseWriter, r *http.Request) {
	ids := h.models.List()
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
