# Buddy2API-Go

用 Go 实现的 **CodeBuddy → OpenAI 兼容代理网关**：单账号、单二进制、内置 Web 管理面板。

- `POST /v1/chat/completions`（流式透传 / 非流式聚合）
- `GET /v1/models`（从上游 `/v3/config` 实时拉取，两套端点模型动态适配）
- Web 管理后台：扫码登录（OAuth 设备流）、多 API Key 管理、官方余额、每日签到、请求日志、仪表盘
- 默认中国端点（`copilot.tencent.com`），可切换国际端点（`www.codebuddy.ai`）

## 快速开始

### 二进制直跑

```bash
go build -o buddy2api ./cmd/buddy2api
./buddy2api                # 默认监听 127.0.0.1:10082，数据目录 ./data
```

### Docker

```bash
docker compose up -d       # http://127.0.0.1:10082
```

> 首次启动若无管理密码，会随机生成一个并打印到 stdout（只显示一次），请立即登录修改。
> 也可用环境变量 `BUDDY2API_ADMIN_PASSWORD` 直接指定。

## 使用

1. 打开 `http://127.0.0.1:10082`，输入管理密码登录
2. 「账号」页 → **扫码登录**（OAuth 设备流，浏览器完成授权，支持 GitHub 登录）
3. 「API Keys」页 → 创建 Key（随机或自定义，支持备注/启停/每日限额/模型白名单）
4. 在任意 OpenAI 兼容客户端填入：

```
Base URL: http://127.0.0.1:10082/v1
API Key:  sk-...
Model:    auto / glm-5.3 / kimi-k2.6 / ...（以 /v1/models 实际返回为准）
```

```bash
curl http://127.0.0.1:10082/v1/chat/completions \
  -H "Authorization: Bearer sk-..." \
  -H "Content-Type: application/json" \
  -d '{"model":"auto","stream":true,"messages":[{"role":"user","content":"你好"}]}'
```

## 端点

| 端点 | 说明 |
|---|---|
| `POST /v1/chat/completions` | OpenAI 兼容 chat（API Key 鉴权） |
| `GET /v1/models` | 模型列表（craft 可用集，动态拉取） |
| `GET /health` | 健康检查（region / 凭证状态 / 版本） |
| `/admin/*` | 管理后台 API（Cookie 会话） |
| `/` | Web 管理面板 |

## 配置

优先级：**env > `data/config.json` > 内置默认**。env 统一 `BUDDY2API_*` 前缀：

| env | 说明 |
|---|---|
| `BUDDY2API_LISTEN` | 监听地址（默认 `127.0.0.1:10082`） |
| `BUDDY2API_REGION` | `cn`（默认）/ `global`，两套端点凭证不互通 |
| `BUDDY2API_ADMIN_PASSWORD` | 管理密码（明文，启动时哈希写回） |
| `BUDDY2API_AUTO_CHECKIN` | 自动签到开关（默认关闭） |
| `BUDDY2API_CHECKIN_CRON` | 签到 cron，6 段含秒（默认 `0 0 9 * * *`） |
| `BUDDY2API_RESOURCE_CACHE_SECONDS` | 余额缓存秒数（默认 300） |
| `BUDDY2API_LOG_RETENTION_DAYS` | 日志保留天数（默认 90） |
| `BUDDY2API_DATA_DIR` | 数据目录（默认 `./data`） |

完整变量见 `.env.example`。

## 数据与安全

- `data/token.json`：账号凭证（0600），仅 OAuth 模式
- `data/config.json`：全局配置（含 bcrypt 密码哈希）
- `data/buddy2api.db`：SQLite（API Keys / 请求日志 / 缓存），**只存元信息，不存对话内容**
- API Key 明文存储（管理页可复制完整 Key），校验用常量时间比对
- 默认只监听 `127.0.0.1`；公网部署请放反代后并加 TLS
- 出站请求复刻官方 CLI 指纹头；chat 请求绝不携带 refresh_token

## 技术栈

Go 1.22+ · chi · modernc.org/sqlite（纯 Go，无 cgo） · robfig/cron · Alpine.js + ECharts（CDN）· go:embed 单二进制

## License

MIT
