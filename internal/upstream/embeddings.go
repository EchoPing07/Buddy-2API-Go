// ── /v2/embeddings ──────────────────────────────────────────────────────────
//
// 上游嵌入端点。与 chat 模型注册表相互独立：/v3/config 的 models 列表（含 nes-gf 等
// 内部特性模型）不可作为嵌入模型名；实测嵌入注册表仅 bge-m3 可用（1024 维），
// 其余名号一律返回 code=11351 "embedding model [x] not found"。
//
// 响应形态与 chat 系接口不同：成功时顶层即 OpenAI 形状的 {data,model,object,usage}，
// 不套 {code,msg,data} 包络；失败时为 {code,msg,requestId}。

package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// EmbeddingsModel 实测可用的嵌入模型（上游嵌入注册表中的唯一已知项）。
const EmbeddingsModel = "bge-m3"

// embeddingsBase embeddings 请求主机。包级变量便于单测以 httptest 替换（生产恒为当前端点）。
var embeddingsBase = func(c *Client) string { return c.ep().Upstream }

// EmbeddingUsage 嵌入计费用量（上游口径，无 credit 字段）。
type EmbeddingUsage struct {
	PromptTokens int `json:"prompt_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// EmbeddingItem 单条输入对应的向量（object/index 由代理层补全，此处只取向量本体）。
type EmbeddingItem struct {
	Embedding []float64 `json:"embedding"`
}

// EmbeddingsResult /v2/embeddings 成功结果。
type EmbeddingsResult struct {
	Model string
	Data  []EmbeddingItem
	Usage EmbeddingUsage
}

// EmbeddingsError 上游嵌入业务错误（HTTP 非 200 或业务 code != 0）。
type EmbeddingsError struct {
	Status int
	Code   int
	Msg    string
}

func (e *EmbeddingsError) Error() string {
	if e.Msg != "" {
		return fmt.Sprintf("embeddings 返回 %d（code=%d）: %s", e.Status, e.Code, e.Msg)
	}
	return fmt.Sprintf("embeddings 返回 %d（code=%d）", e.Status, e.Code)
}

// ModelNotFound 判断嵌入错误是否为「模型不在注册表」（上游统一 500 + code=11351，
// 文案形如 "embedding model [x] not found"）；代理层据此映射 OpenAI 的 404 语义。
func (e *EmbeddingsError) ModelNotFound() bool {
	return e.Code == 11351 && bytes.Contains([]byte(e.Msg), []byte("not found"))
}

// DoEmbeddings 完整嵌入流程：确保 token 有效 → 请求 → 401 则刷新重试一次。
// 非流式：一次请求拿全量向量；批量大小与单条长度上限由上游裁决。
func (c *Client) DoEmbeddings(ctx context.Context, model string, inputs []string) (*EmbeddingsResult, error) {
	if _, err := c.token(); err != nil {
		return nil, err
	}
	_ = c.toks.EnsureValid(c.RefreshFn) // 过期则刷新（best-effort，失败仍尝试请求）

	body, _ := json.Marshal(map[string]any{"model": model, "input": inputs})
	status, raw, err := c.embeddingsOnce(ctx, body)
	if err != nil {
		return nil, fmt.Errorf("上游请求失败: %w", err)
	}
	if status == http.StatusUnauthorized {
		if err := c.toks.ForceRefresh(c.RefreshFn); err != nil {
			return nil, fmt.Errorf("token 已失效且刷新失败，请重新登录: %w", err)
		}
		status, raw, err = c.embeddingsOnce(ctx, body)
		if err != nil {
			return nil, fmt.Errorf("上游请求失败: %w", err)
		}
	}
	return parseEmbeddingsResponse(status, raw, len(inputs))
}

// embeddingsOnce 单次 POST /v2/embeddings。用 long 客户端：大批量嵌入可能超过
// short 的 30 秒总超时，且本端无流式语义，响应头超时（chat 同款）已够兜底。
func (c *Client) embeddingsOnce(ctx context.Context, body []byte) (int, []byte, error) {
	t, err := c.token()
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, embeddingsBase(c)+"/v2/embeddings", bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header = chatHeaders(t)
	resp, err := c.long.Do(req)
	if err != nil {
		return 0, nil, err
	}
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 16<<20)) // 1024 维 float 批量可达 MB 级
	resp.Body.Close()
	if rerr != nil {
		return resp.StatusCode, nil, rerr
	}
	return resp.StatusCode, raw, nil
}

// parseEmbeddingsResponse 解析上游响应：HTTP 非 200 → EmbeddingsError（错误体为
// {code,msg}）；200 → 按 OpenAI 形状解出 {model,data,usage}，并校验 data 与输入
// 条数一致（错位即协议异常，宁可不返回也不能错位配对）。
func parseEmbeddingsResponse(status int, raw []byte, want int) (*EmbeddingsResult, error) {
	if status != http.StatusOK {
		var env struct {
			Code int    `json:"code"`
			Msg  string `json:"msg"`
		}
		_ = json.Unmarshal(raw, &env)
		return nil, &EmbeddingsError{Status: status, Code: env.Code, Msg: env.Msg}
	}
	var top struct {
		Code  int             `json:"code"`
		Msg   string          `json:"msg"`
		Model string          `json:"model"`
		Data  json.RawMessage `json:"data"`
		Usage EmbeddingUsage  `json:"usage"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, fmt.Errorf("解析 embeddings 响应失败: %s", truncStr(string(raw), 300))
	}
	// 防御：上游若以 200 包业务错误码（chat 系接口的形态），按错误处理
	if top.Code != 0 {
		return nil, &EmbeddingsError{Status: status, Code: top.Code, Msg: top.Msg}
	}
	res := &EmbeddingsResult{Model: top.Model, Usage: top.Usage}
	if len(top.Data) == 0 || json.Unmarshal(top.Data, &res.Data) != nil {
		return nil, fmt.Errorf("embeddings 响应缺少 data 向量数组: %s", truncStr(string(raw), 200))
	}
	if len(res.Data) != want {
		return nil, fmt.Errorf("embeddings 返回 %d 条向量，与输入 %d 条不符", len(res.Data), want)
	}
	return res, nil
}
