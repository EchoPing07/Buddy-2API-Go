// ── /v1/embeddings（OpenAI 兼容嵌入端点） ────────────────────────────────────

package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"buddy2api-go/internal/apikey"
	"buddy2api-go/internal/store"
	"buddy2api-go/internal/upstream"
)

// openAIEmbeddingItem OpenAI embeddings 响应的单条向量（上游缺 index，由本端按下标补全）。
type openAIEmbeddingItem struct {
	Object    string    `json:"object"`
	Index     int       `json:"index"`
	Embedding []float64 `json:"embedding"`
}

// openAIEmbeddingsResponse OpenAI embeddings 响应体。
type openAIEmbeddingsResponse struct {
	Object string                 `json:"object"`
	Data   []openAIEmbeddingItem  `json:"data"`
	Model  string                 `json:"model"`
	Usage  upstream.EmbeddingUsage `json:"usage"`
}

// normalizeEmbeddingsInput 把 OpenAI 的 input 字段（字符串或字符串数组）归一为 []string。
// OpenAI 另允许 token id 数组（[[1,2],[3]]），上游仅收文本，此处直接拒绝并说明。
func normalizeEmbeddingsInput(v any) ([]string, error) {
	switch t := v.(type) {
	case string:
		return []string{t}, nil
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("input 数组元素须为字符串（token id 数组不支持）")
			}
			out = append(out, s)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("input 须为字符串或字符串数组")
	}
}

// Embeddings 处理 POST /v1/embeddings：实验特性开关（默认关闭，管理后台「实验」页控制）
// → 校验 → DoEmbeddings（含 401 刷新重试）→ 转成 OpenAI 形状（补 index）写出。
// 日志与用量记账与 Chat 同一套链路。
func (h *Handler) Embeddings(w http.ResponseWriter, r *http.Request) {
	if !h.cfg.Get().EmbeddingsEnabled {
		openaiError(w, http.StatusForbidden, "invalid_request_error", "embeddings_disabled",
			"嵌入端点未开启：请在管理后台「实验」页开启（默认关闭）")
		return
	}
	start := time.Now()
	key := apikey.FromContext(r.Context())

	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodySize))
	if err != nil {
		openaiError(w, http.StatusBadRequest, "invalid_request_error", "read_body_failed", "读取请求体失败: "+err.Error())
		return
	}
	var body struct {
		Model string `json:"model"`
		Input any    `json:"input"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		openaiError(w, http.StatusBadRequest, "invalid_request_error", "invalid_json", "请求体不是合法 JSON: "+err.Error())
		return
	}
	if body.Model == "" {
		openaiError(w, http.StatusBadRequest, "invalid_request_error", "missing_model", "缺少 model 字段")
		return
	}
	if body.Input == nil {
		openaiError(w, http.StatusBadRequest, "invalid_request_error", "missing_input", "缺少 input 字段")
		return
	}
	inputs, err := normalizeEmbeddingsInput(body.Input)
	if err != nil {
		openaiError(w, http.StatusBadRequest, "invalid_request_error", "invalid_input", err.Error())
		return
	}
	if len(inputs) == 0 || (len(inputs) == 1 && inputs[0] == "") {
		openaiError(w, http.StatusBadRequest, "invalid_request_error", "invalid_input", "input 不能为空")
		return
	}

	logEntry := &store.LogEntry{
		APIKeyID: keyID(key), APIKeyName: keyName(key),
		Model: body.Model,
	}
	defer func() {
		logEntry.DurationMs = time.Since(start).Milliseconds()
		h.writeLog(logEntry)
		if key != nil {
			h.keys.RecordUsage(key.ID, int64(logEntry.TotalTokens))
		}
	}()

	res, err := h.client.DoEmbeddings(r.Context(), body.Model, inputs)
	if err != nil {
		var ue *upstream.EmbeddingsError
		switch {
		case errors.As(err, &ue) && ue.ModelNotFound():
			// 上游对未知嵌入模型统一 500，映射为 OpenAI 语义的 404 并附已知模型提示
			logEntry.StatusCode = http.StatusNotFound
			logEntry.ErrorMsg = trunc(err.Error(), 500)
			openaiError(w, http.StatusNotFound, "invalid_request_error", "model_not_found",
				fmt.Sprintf("嵌入模型 %q 不存在，当前上游嵌入模型仅支持 %q", body.Model, upstream.EmbeddingsModel))
		case errors.As(err, &ue):
			logEntry.StatusCode = http.StatusBadGateway
			logEntry.ErrorMsg = trunc(err.Error(), 500)
			openaiError(w, http.StatusBadGateway, "upstream_error", "upstream_error", err.Error())
		default:
			logEntry.StatusCode = http.StatusServiceUnavailable
			logEntry.ErrorMsg = trunc(err.Error(), 500)
			openaiError(w, http.StatusServiceUnavailable, "upstream_error", "upstream_unavailable", err.Error())
		}
		return
	}

	logEntry.StatusCode = http.StatusOK
	logEntry.Model = orDefault(res.Model, body.Model)
	logEntry.PromptTokens = res.Usage.PromptTokens
	logEntry.TotalTokens = res.Usage.TotalTokens

	resp := openAIEmbeddingsResponse{
		Object: "list",
		Data:   make([]openAIEmbeddingItem, len(res.Data)),
		Model:  orDefault(res.Model, body.Model),
		Usage:  res.Usage,
	}
	for i, item := range res.Data {
		resp.Data[i] = openAIEmbeddingItem{Object: "embedding", Index: i, Embedding: item.Embedding}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	out, _ := json.Marshal(resp)
	w.Write(out)
}
