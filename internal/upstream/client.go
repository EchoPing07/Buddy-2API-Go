package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"buddy2api-go/internal/auth"
	"buddy2api-go/internal/config"
)

// ErrNoToken 透传 auth 包错误。
var ErrNoToken = auth.ErrNoToken

// Client 上游客户端。regionProvider 每次请求动态读取当前 region（支持热切换）。
type Client struct {
	long           *http.Client // chat：可能长时间流式
	short          *http.Client // 其余接口
	toks           *auth.TokenStore
	regionProvider func() string
}

// New 创建上游客户端。chatTimeoutSeconds 设置 chat 上游的响应头超时（上游多久不开始
// 响应即判死），仅在构建 Transport 时生效一次，运行时修改需重启；一旦进入流式阶段，
// 只要持续吐字节（含 reasoning 思考流）就不再被本端总超时截断，客户端断开由请求 context 兜底。
func New(toks *auth.TokenStore, regionProvider func() string, chatTimeoutSeconds int) *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = time.Duration(chatTimeoutSeconds) * time.Second
	return &Client{
		long:           &http.Client{Transport: transport},
		short:          &http.Client{Timeout: 30 * time.Second},
		toks:           toks,
		regionProvider: regionProvider,
	}
}

func (c *Client) ep() config.Endpoint { return config.EndpointOf(c.regionProvider()) }

// TokenSummary 返回当前 token 副本（未登录为 nil）。
func (c *Client) TokenSummary() *auth.Token { return c.toks.Get() }

func (c *Client) token() (*auth.Token, error) {
	t := c.toks.Get()
	if t == nil {
		return nil, ErrNoToken
	}
	return t, nil
}

// ── 统一包络 ──

// envelope 上游统一响应包络 {code,msg,data}。
type envelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

func postJSON(client *http.Client, url string, body string, headers http.Header) (int, []byte, error) {
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header = headers
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	resp.Body.Close()
	if rerr != nil {
		return resp.StatusCode, nil, rerr
	}
	return resp.StatusCode, raw, nil
}

// ── chat ──

// Chat 单次 chat 请求，返回上游 SSE 响应（调用方负责关闭 Body）。
func (c *Client) Chat(ctx context.Context, body []byte) (*http.Response, error) {
	t, err := c.token()
	if err != nil {
		return nil, err
	}
	return c.chatOnce(ctx, body, t)
}

func (c *Client) chatOnce(ctx context.Context, body []byte, t *auth.Token) (*http.Response, error) {
	url := c.ep().Upstream + "/v2/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = chatHeaders(t)
	return c.long.Do(req)
}

// DoChat 完整 chat 流程：确保 token 有效 → 请求 → 401 则刷新重试一次。
func (c *Client) DoChat(ctx context.Context, body []byte) (*http.Response, error) {
	if _, err := c.token(); err != nil {
		return nil, err
	}
	// 过期则刷新（best-effort，失败仍尝试请求）
	_ = c.toks.EnsureValid(c.RefreshFn)

	resp, err := c.Chat(ctx, body)
	if err != nil {
		return nil, fmt.Errorf("上游请求失败: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		if err := c.toks.ForceRefresh(c.RefreshFn); err != nil {
			return nil, fmt.Errorf("token 已失效且刷新失败，请重新登录: %w", err)
		}
		resp, err = c.Chat(ctx, body)
		if err != nil {
			return nil, fmt.Errorf("上游请求失败: %w", err)
		}
	}
	return resp, nil
}

// Ping 发最小 chat 验证凭证可用。
func (c *Client) Ping() error {
	body := map[string]any{
		"model":          "auto",
		"stream":         true,
		"messages":       []map[string]string{{"role": "user", "content": "ping"}},
		"stream_options": map[string]bool{"include_usage": true},
	}
	raw, _ := json.Marshal(body)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	resp, err := c.DoChat(ctx, raw)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return fmt.Errorf("上游返回 %d: %s", resp.StatusCode, truncStr(string(raw), 300))
	}
	// 读一小段确认 SSE 正常
	buf := make([]byte, 64)
	n, _ := resp.Body.Read(buf)
	if n == 0 {
		return fmt.Errorf("上游响应为空")
	}
	return nil
}

// ── token 刷新 ──

// RefreshFn 刷新回调，供 TokenStore 使用。
func (c *Client) RefreshFn(t *auth.Token) (*auth.Token, error) {
	if t.RefreshToken == "" {
		return nil, fmt.Errorf("缺少 refresh_token，无法刷新，请重新登录")
	}
	url := c.ep().Upstream + "/v2/plugin/auth/token/refresh"
	// 刷新用旧 token 的域信息构造头
	old := *t
	old.Domain = orDefault(t.Domain, c.ep().Domain)
	status, raw, err := postJSON(c.short, url, "{}", refreshHeaders(&old))
	if err != nil {
		return nil, fmt.Errorf("刷新请求失败: %w", err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("解析刷新响应失败（HTTP %d）: %s", status, truncStr(string(raw), 300))
	}
	var data tokenData
	if env.Code == 0 && len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, &data); err != nil {
			return nil, fmt.Errorf("解析刷新 data 失败: %w", err)
		}
	}
	if env.Code != 0 || data.AccessToken == "" {
		return nil, fmt.Errorf("刷新失败 code=%d msg=%s（HTTP %d）", env.Code, env.Msg, status)
	}
	return buildToken(data, t), nil
}

// RefreshNow 手动强制刷新。
func (c *Client) RefreshNow() error {
	return c.toks.ForceRefresh(c.RefreshFn)
}

// tokenData OAuth/刷新响应 data 字段。
type tokenData struct {
	AccessToken      string `json:"accessToken"`
	RefreshToken     string `json:"refreshToken"`
	TokenType        string `json:"tokenType"`
	ExpiresIn        int64  `json:"expiresIn"`
	RefreshExpiresIn int64  `json:"refreshExpiresIn"`
	Domain           string `json:"domain"`
	SessionState     string `json:"sessionState"`
	Scope            string `json:"scope"`
}

// buildToken 从响应 data 构造 Token（继承旧 token 的缺失字段）。
func buildToken(d tokenData, old *auth.Token) *auth.Token {
	t := &auth.Token{
		AccessToken:  d.AccessToken,
		RefreshToken: d.RefreshToken,
		TokenType:    orDefault(d.TokenType, "Bearer"),
		Domain:       orDefault(d.Domain, ""),
	}
	if old != nil {
		if t.RefreshToken == "" {
			t.RefreshToken = old.RefreshToken
		}
		if t.Domain == "" {
			t.Domain = old.Domain
		}
	}
	t.EnrichFromJWT()
	if t.ExpiresAt == 0 && d.ExpiresIn > 0 {
		t.ExpiresAt = time.Now().Unix() + d.ExpiresIn
	}
	return t
}

// ── OAuth 设备流 ──

// OAuthStart 启动设备流，返回 state 与授权链接。
func (c *Client) OAuthStart() (state, authURL string, err error) {
	nonce := randHex(8)
	host := c.ep().Upstream
	url := fmt.Sprintf("%s/v2/plugin/auth/state?platform=CLI&nonce=%s", host, nonce)
	body := fmt.Sprintf(`{"nonce":"%s"}`, nonce)
	status, raw, err := postJSON(c.short, url, body, oauthHeaders())
	if err != nil {
		return "", "", fmt.Errorf("auth/state 请求失败: %w", err)
	}
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			State   string `json:"state"`
			AuthURL string `json:"authUrl"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return "", "", fmt.Errorf("解析 auth/state 响应失败（HTTP %d）: %s", status, truncStr(string(raw), 300))
	}
	if env.Code != 0 || env.Data.State == "" || env.Data.AuthURL == "" {
		return "", "", fmt.Errorf("auth/state 失败 code=%d msg=%s（HTTP %d）", env.Code, env.Msg, status)
	}
	return env.Data.State, env.Data.AuthURL, nil
}

// OAuthPoll 轮询登录状态。返回 (token, true, nil) 表示登录成功；
// (nil, false, nil) 表示等待中；error 表示失败/超时。
func (c *Client) OAuthPoll(state string) (*auth.Token, bool, error) {
	url := fmt.Sprintf("%s/v2/plugin/auth/token?state=%s", c.ep().Upstream, state)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, false, err
	}
	req.Header = oauthHeaders()
	resp, err := c.short.Do(req)
	if err != nil {
		return nil, false, err
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()

	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, false, fmt.Errorf("解析轮询响应失败: %s", truncStr(string(raw), 200))
	}
	switch {
	case env.Code == 0:
		var d tokenData
		if err := json.Unmarshal(env.Data, &d); err != nil || d.AccessToken == "" {
			return nil, false, fmt.Errorf("轮询响应缺少 accessToken: %s", truncStr(string(raw), 200))
		}
		t := buildToken(d, nil)
		if t.Domain == "" {
			t.Domain = c.ep().Domain
		}
		return t, true, nil
	case env.Code == 11217:
		return nil, false, nil // authorization_pending
	default:
		return nil, false, fmt.Errorf("轮询失败 code=%d msg=%s", env.Code, env.Msg)
	}
}

// ── billing / 签到 ──

// BillingResult billing 类接口结果：Raw 为上游原始 JSON，Data 为解包后的 data 节点。
type BillingResult struct {
	Raw  json.RawMessage
	Data json.RawMessage
}

func (c *Client) billing(path string) (*BillingResult, int, error) {
	t, err := c.token()
	if err != nil {
		return nil, 0, err
	}
	// billing 前也确保 token 有效
	if err := c.toks.EnsureValid(c.RefreshFn); err != nil {
		return nil, 0, err
	}
	t = c.toks.Get()
	if t == nil { // EnsureValid 与 Get 之间可能被并发登出清空
		return nil, 0, ErrNoToken
	}
	old := *t
	old.Domain = orDefault(t.Domain, c.ep().Domain)
	status, raw, err := postJSON(c.short, c.ep().Upstream+path, "{}", billingHeaders(&old))
	if err != nil {
		return nil, status, err
	}
	var env envelope
	_ = json.Unmarshal(raw, &env)
	if status != http.StatusOK {
		return &BillingResult{Raw: raw}, status, fmt.Errorf("billing 返回 %d: %s", status, truncStr(string(raw), 300))
	}
	if env.Code != 0 {
		return &BillingResult{Raw: raw}, status, fmt.Errorf("billing code=%d msg=%s", env.Code, env.Msg)
	}
	return &BillingResult{Raw: raw, Data: env.Data}, status, nil
}

// Resources 官方余额（get-user-resource）。
func (c *Client) Resources() (*BillingResult, error) {
	r, _, err := c.billing("/v2/billing/meter/get-user-resource")
	return r, err
}

// CheckinStatus 签到状态。
func (c *Client) CheckinStatus() (*BillingResult, error) {
	r, _, err := c.billing("/v2/billing/meter/checkin-activity-status")
	return r, err
}

// ClaimCheckin 领取每日签到。
func (c *Client) ClaimCheckin() (*BillingResult, error) {
	r, _, err := c.billing("/v2/billing/meter/daily-checkin")
	return r, err
}

// ── /v3/config 模型列表 ──

// ModelInfo 模型详情。
type ModelInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ModelsConfig /v3/config 解析结果。
type ModelsConfig struct {
	Models []ModelInfo `json:"models"` // 全部模型详情
	Craft  []string    `json:"craft"`  // craft agent 可用模型 ID（对话可用集）
}

// FetchConfig 拉取 /v3/config（VSCode 指纹）。
func (c *Client) FetchConfig() (*ModelsConfig, error) {
	t, err := c.token()
	if err != nil {
		return nil, err
	}
	old := *t
	old.Domain = orDefault(t.Domain, c.ep().Domain)
	req, err := http.NewRequest(http.MethodGet, c.ep().Upstream+"/v3/config", nil)
	if err != nil {
		return nil, err
	}
	req.Header = configHeaders(&old)
	resp, err := c.short.Do(req)
	if err != nil {
		return nil, err
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("/v3/config 返回 %d: %s", resp.StatusCode, truncStr(string(raw), 200))
	}
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Models []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"models"`
			Agents []struct {
				Name   string   `json:"name"`
				Models []string `json:"models"`
			} `json:"agents"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("解析 /v3/config 失败: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("/v3/config code=%d msg=%s", env.Code, env.Msg)
	}
	mc := &ModelsConfig{}
	for _, m := range env.Data.Models {
		mc.Models = append(mc.Models, ModelInfo{ID: m.ID, Name: m.Name})
	}
	for _, a := range env.Data.Agents {
		if a.Name == "craft" {
			mc.Craft = a.Models
			break
		}
	}
	if len(mc.Craft) == 0 {
		// 找不到 craft agent 则退化为全部模型 ID
		for _, m := range env.Data.Models {
			mc.Craft = append(mc.Craft, m.ID)
		}
	}
	return mc, nil
}

// ── util ──

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

func truncStr(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

// ErrUpstreamAuth 判断是否为需要重新登录的错误。
func ErrUpstreamAuth(err error) bool { return errors.Is(err, ErrNoToken) }
