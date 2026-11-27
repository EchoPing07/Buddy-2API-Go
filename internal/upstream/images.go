// images.go 原生图像生成接口（国内区与全球区共用）。
//
// 协议已由本地国内账号实测：
//
//	POST /v2/images/generations  {model:"hunyuan-image-alpha",prompt,size,n}   （国内区）
//	POST /v2/images/generations  {model:"hy-image-3.5",prompt,size}            （国际区）
//	POST /v2/images/edits        同上，另带 image:[dataURL,...]
//
// 成功响应仍套 {code,msg,data}，内层 data 已是 OpenAI images 的
// {created,data:[{url}],usage}。HTTP 200 也必须检查业务 code。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"buddy2api-go/internal/auth"
)

// 已实测的图像模型。/v3/config 的 tags 是发现依据，这里仅作拉取失败时的回退。
const (
	ImageGenerationModel       = "hunyuan-image-alpha"
	ImageEditModel             = "hunyuan-image-alpha-edit"
	GlobalImageGenerationModel = "hy-image-3.5"
)

// ImageModel 一个图像模型及其能力（目录 tag）。
//
// Kind 取 text-to-image / image-to-image：上游按能力分不同端点，
// 文生图不能拿改图模型顶替（反之亦然），所以两边都要按 Kind 选默认值。
type ImageModel struct {
	ID   string
	Kind string
}

// ImageUsage 是图像接口的用量口径，不能直接复用 chat usage。
type ImageUsage struct {
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	TotalTokens  int     `json:"total_tokens"`
	Credit       float64 `json:"credit"`
	ImageCounts  int     `json:"output_image_counts"`
}

// ImagesResult 保留 OpenAI images 响应本体及记账所需用量。
type ImagesResult struct {
	Data  json.RawMessage
	Usage ImageUsage
}

// ImagesError 保留上游状态与原始错误体，供号池分类和 OpenAI 错误映射。
type ImagesError struct {
	Status int
	Raw    []byte
}

func (e *ImagesError) Error() string {
	return upstreamErrorText(e.Status, e.Raw)
}

func upstreamErrorText(status int, raw []byte) string {
	code, msg := BizCode(raw)
	if msg != "" {
		return fmt.Sprintf("图像接口返回 %d（code=%d）: %s", status, code, truncStr(msg, 300))
	}
	return fmt.Sprintf("图像接口返回 %d: %s", status, truncStr(string(raw), 300))
}

// DoImages 使用当前活动账号请求图像接口；DoImagesWith 使用号池指定账号。
func (c *Client) DoImages(ctx context.Context, edit bool, body []byte) (*ImagesResult, error) {
	return c.doImages(ctx, c.toks, edit, body)
}

func (c *Client) DoImagesWith(ctx context.Context, ts *auth.TokenStore, edit bool, body []byte) (*ImagesResult, error) {
	return c.doImages(ctx, ts, edit, body)
}

// doImages 与 chat 一样在 401 后强刷并重试一次，但不把网络/5xx 自动重放：
// 图像请求可能已生成并扣费，未知结果不能靠换号盲目重试。
func (c *Client) doImages(ctx context.Context, ts *auth.TokenStore, edit bool, body []byte) (*ImagesResult, error) {
	if ts == nil {
		return nil, ErrNoToken
	}
	t := ts.Get()
	if t == nil {
		return nil, ErrNoToken
	}
	_ = ts.EnsureValid(c.RefreshFn)
	if current := ts.Get(); current != nil {
		t = current
	}
	status, raw, err := c.imagesOnce(ctx, t, edit, body)
	if err != nil {
		return nil, fmt.Errorf("图像上游请求失败: %w", err)
	}
	if status == http.StatusUnauthorized {
		if rerr := ts.ForceRefresh(c.RefreshFn); rerr == nil {
			t = ts.Get()
			status, raw, err = c.imagesOnce(ctx, t, edit, body)
			if err != nil {
				return nil, fmt.Errorf("图像上游请求失败: %w", err)
			}
		}
	}
	return parseImagesResponse(status, raw)
}

func (c *Client) imagesOnce(ctx context.Context, t *auth.Token, edit bool, body []byte) (int, []byte, error) {
	if t == nil {
		return 0, nil, ErrNoToken
	}
	path := "/v2/images/generations"
	if edit {
		path = "/v2/images/edits"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.chatBase()+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header = chatHeaders(t)
	resp, err := c.long.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, raw, nil
}

func parseImagesResponse(status int, raw []byte) (*ImagesResult, error) {
	if status < 200 || status >= 300 {
		return nil, &ImagesError{Status: status, Raw: raw}
	}
	var env struct {
		Code int             `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("解析图像响应失败: %w", err)
	}
	if env.Code != 0 {
		return nil, &ImagesError{Status: status, Raw: raw}
	}
	var inner struct {
		Data  []json.RawMessage `json:"data"`
		Usage ImageUsage        `json:"usage"`
	}
	if len(env.Data) == 0 || json.Unmarshal(env.Data, &inner) != nil || len(inner.Data) == 0 {
		return nil, fmt.Errorf("图像响应缺少图片: %s", truncStr(string(raw), 300))
	}
	return &ImagesResult{Data: env.Data, Usage: inner.Usage}, nil
}
