// web.go Web 端（www.workbuddy.cn）请求构造。
//
// 为什么需要第二主机：成长中心的**任务领奖**与**web 域行为上报**只在 Web 端提供。
// 实测领奖端点 `POST /activity/growth/tasks/<code>/claim` 在 chat 域（copilot.tencent.com）
// 不存在（400 "task not completed"），必须打 https://www.workbuddy.cn，并带
// Origin/Referer 指向成长中心、`x-client-platform: web`。这是早期领奖一直失败的真实原因。
//
// 该主机只服务成长域，不参与 chat 转发，故单独一个 helper，不污染 growthBase。
package upstream

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
)

// webBaseCN Web 端主机（cn 域固定值；global 无成长体系，不受理该域请求）。
const webBaseCN = "https://www.workbuddy.cn"

// webBase 返回 Web 端主机。包级变量便于单测以 httptest 替换。
var webBase = func(c *Client) string {
	if c.WebBaseOverride != "" {
		return c.WebBaseOverride
	}
	return webBaseCN
}

// webRequest 构造指向 Web 端的请求：Bearer 鉴权 + Web 端浏览器形状的头组
// （Origin/Referer 指向 workbuddy.cn，UA 沿用 CLI 口径——实测该组合被受理）。
func (c *Client) webRequest(method, path string, body any) (*http.Request, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, webBase(c)+path, rdr)
	if err != nil {
		return nil, err
	}
	t := c.toks.Get()
	if t == nil {
		return nil, ErrNoToken
	}
	req.Header.Set("Authorization", "Bearer "+t.AccessToken)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", webBase(c))
	req.Header.Set("User-Agent", userAgent())
	if t.UID != "" {
		req.Header.Set("X-User-Id", t.UID)
	}
	if t.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", t.EnterpriseID)
		req.Header.Set("X-Tenant-Id", t.EnterpriseID)
	}
	return req, nil
}
