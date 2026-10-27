// blackcat.go 夜猫子任务（black_cat）：23:00–08:00 窗口内的 glm-5.2 真实对话补足。
//
// 判据：`black_cat` 要求在**夜间窗口内**完成 N 次 glm-5.2 对话并上报 chat 事件链
// （每天 1 次、累计 3 天）。窗口外行为不计分——所以先查进度，未达标才在窗口内补足。
//
// 这是整套任务体系里唯一「整任务都是真实模型对话」的，故调度侧对暂停选号（paused）
// 账号也跳过（与其他纯 RPC 任务不同）。
package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// blackCatTaskCode 夜猫子任务码。
const blackCatTaskCode = "black_cat"

// InNightWindow 当前是否处于夜猫子计数窗口（23:00–08:00 本地时区）。
func InNightWindow(now time.Time) bool {
	h := now.Hour()
	return h >= 23 || h < 8
}

// BlackcatNeed 查 black_cat 任务剩余差额（需要再完成几次对话）。
// 任务不存在 / 已达标返回 0；拉取失败返回错误。
func (c *Client) BlackcatNeed() (int64, error) {
	tasks, err := c.ListTasks()
	if err != nil {
		return 0, err
	}
	t := FindTask(tasks, blackCatTaskCode)
	if t == nil || t.Claimed || (t.Target > 0 && t.Current >= t.Target) {
		return 0, nil
	}
	if t.Target <= 0 {
		return 0, nil
	}
	return t.Target - t.Current, nil
}

// RunNightChats 夜猫子：发 need 次 glm-5.2 真实对话（读干流）并上报事件链。
// 返回成功次数；中断时把已成功次数一并返回，便于调用方如实汇报。
func (c *Client) RunNightChats(need int) (int64, error) {
	var ok int64
	for i := 0; i < need; i++ {
		body, err := json.Marshal(map[string]any{
			"model":    "glm-5.2",
			"messages": []map[string]any{{"role": "user", "content": "1+1等于几？直接回答。"}},
			"stream":   true,
		})
		if err != nil {
			return ok, err
		}
		resp, err := c.DoChat(context.Background(), body)
		if err != nil {
			return ok, fmt.Errorf("第 %d 次对话失败: %w", i+1, err)
		}
		if resp.StatusCode >= 400 {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
			resp.Body.Close()
			return ok, fmt.Errorf("第 %d 次对话失败 http=%d: %s", i+1, resp.StatusCode, raw)
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20)) // 读干流，避免残留连接
		resp.Body.Close()
		cid := fmt.Sprintf("b2a-night-%d-%d", time.Now().UnixMilli(), i)
		if err := c.ReportChatActivityModel(cid, "", "glm-5.2", "GLM-5.2"); err != nil {
			return ok, fmt.Errorf("第 %d 次上报失败: %w", i+1, err)
		}
		ok++
		if i < need-1 {
			time.Sleep(nightChatGap)
		}
	}
	return ok, nil
}

// nightChatGap 夜猫子连续对话间隔（真实节奏，防风控）。测试可置 0。
var nightChatGap = 4 * time.Second
