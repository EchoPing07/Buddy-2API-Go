# Buddy-2API-Go

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE.txt)
[![Go Version](https://img.shields.io/badge/Go-1.25+-blue)](go.mod)

> 用 Go 实现的 **CodeBuddy / WorkBuddy 转 OpenAI 兼容代理网关**：单账号、单二进制、内置 Web 管理面板。
>
> 项目地址：<https://github.com/EchoPing07/Buddy-2API-Go>

- `POST /v1/chat/completions` —— OpenAI 兼容对话（流式透传 / 非流式聚合）
- `GET /v1/models` —— 实时模型列表（从上游 `/v3/config` 动态拉取并缓存，cn / global 两套端点自动适配）
- 内置 Web 管理后台 —— OAuth 登录、API Key 管理、官方余额、每日签到、请求日志、仪表盘等
- 默认中国端点（`copilot.tencent.com`），可切换国际端点（`www.codebuddy.ai`）

## ⚠️ 声明

- **本项目仅用于学习与研究目的**，请勿用于任何违法违规用途，不要干坏事。使用者需自行承担一切后果，作者不对任何滥用行为负责。
- 本项目的代码参考了 wicm84266964/Buddy2api、Sliverkiss/CodeBuddy2api、cyl2361341082-alt/Buddy2api、ShouZhuo0413/codebuddy2api 等项目，Web 管理面板的 UI 设计参考了 [grok2api](https://github.com/EchoPing07/grok2api)（详见 [致谢与参考](#-致谢与参考)）。
- **本项目定位为个人单账号使用，明确不接受“号池”（多账号池 / 多凭证轮询）相关的建议与提交，相关需求请勿提 Issue 或 PR。**
- 本项目**不存储对话内容**，数据库仅保存元信息（模型、token 数、耗时、状态码等）。

## ✨ 功能特性

| 模块          | 说明                                                      |
| ----------- | ------------------------------------------------------- |
| 单账号代理       | 一份凭证（`data/token.json`），OAuth 登录                        |
| OpenAI 兼容端点 | `/v1/chat/completions`（流式 / 非流式）、`/v1/models`、`/health` |
| 多 API Key   | 随机 / 自定义 Key，支持备注、启停、使用量统计                              |
| 官方余额        | 实时拉取额度包明细，本地聚合可用额度（剔除周期外幻影/已过期包），官方 TotalDosage 对照展示，标注到期 / 临期 |
| 每日签到        | 独立开关，cron 定时 / 时间范围内随机二选一，失败重试 + 末班兜底，也可手动领取     |
| 仪表盘         | 请求量、Token、模型分布、Key 用量等聚合统计                              |
| 模型倍率         | 模型列表展示当前实际倍率（如 `GLM-5.2 x0.50`），自动套用官方分时段折扣（夜间折扣 / 限时免费等，支持跨零点时段窗与时区） |
| 自动刷新        | token 过期自动刷新，401 时刷新后重试一次                               |
| 指纹头         | 出站请求复刻官方 CLI 指纹头；chat 请求绝不携带 refresh_token              |

## 🚀 快速开始

### 方式一：下载发行版二进制（推荐）

从 [GitHub Releases](https://github.com/EchoPing07/Buddy-2API-Go/releases) 下载对应平台的压缩包（附 `SHA256SUMS` 校验和）：

| 平台 | 架构 | 格式 |
|---|---|---|
| Linux | amd64 / arm64 / armv7 / riscv64 | `.tar.gz` |
| Windows | amd64 / arm64 | `.zip` |
| macOS | amd64 / arm64 | `.tar.gz` |

解压后直接运行（无需任何运行时依赖）：

```bash
./buddy2api-linux-amd64      # 默认监听 127.0.0.1:10082，数据目录 ./data
```

> Windows 双击 `buddy2api-windows-amd64.exe` 或在 cmd / PowerShell 中运行即可。

### 方式二：Docker（推荐）

```bash
docker compose up -d          # 浏览器访问 http://<服务器IP>:10082（局域网可访问）
```

或手动运行：

```bash
docker run -d \
  --name buddy2api \
  -p 0.0.0.0:10082:10082 \
  -v "$(pwd)/data:/app/data" \
  -e BUDDY2API_LISTEN=0.0.0.0:10082 \
  ghcr.io/echoping07/buddy-2api-go:latest
```

> 首次启动未设置管理密码时，默认密码为 `password`，登录后请尽快在管理后台修改；也可通过环境变量 `BUDDY2API_ADMIN_PASSWORD` 直接指定。

#### Docker 时区说明

镜像**默认使用中国时区（`Asia/Shanghai`）**，并已内置 `tzdata`。需要其他时区时，通过 Docker 环境变量 `TZ` 覆盖即可，无需重新构建镜像：

```yaml
# docker-compose.yml
services:
  buddy2api:
    image: ghcr.io/echoping07/buddy-2api-go:latest
    environment:
      TZ: Asia/Tokyo        # 例如东京时区；默认 Asia/Shanghai
```

```bash
# 或 docker run 时传入
docker run -e TZ=UTC -d --name buddy2api ...
```

#### Docker 数据路径

| 位置 | 路径 | 说明 |
|---|---|---|
| **容器内数据目录** | `/app/data` | 镜像已声明 `VOLUME /app/data`，工作目录为 `/app`，程序默认数据目录 `./data` 即 `/app/data` |
| **宿主机映射（compose 默认）** | `./data` → `/app/data` | 建议在部署目录下建 `data/` 文件夹 |
| **二进制直跑** | `./data`（可用 `-data` 或 `BUDDY2API_DATA_DIR` 修改） | 相对可执行文件所在目录 |

### 方式三：源码构建（进阶）

需要 Go 1.25+：

```bash
git clone https://github.com/EchoPing07/Buddy-2API-Go.git
cd Buddy-2API-Go
go build -o buddy2api .        # Windows 用 buddy2api.exe
./buddy2api                    # 默认监听 127.0.0.1:10082，数据目录 ./data
```

带版本号注入：

```bash
go build -ldflags="-s -w -X buddy2api-go/internal/proxy.Version=v1.0.0" -o buddy2api .
```

## 📖 使用

1. 打开管理后台：本机部署访问 `http://127.0.0.1:10082`，Docker/局域网部署访问 `http://<服务器IP>:10082`，输入管理密码登录（默认 `password`）
2. 「账号」页 → **登录**（OAuth 设备流，浏览器完成授权）
3. 「密钥」页 → 创建 API Key（随机或自定义，支持备注/启停）
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

## 🖥️ 命令行参数

| 参数 | 说明 |
|---|---|
| `-data <dir>` | 数据目录（默认 `./data`，等价 env `BUDDY2API_DATA_DIR`） |
| `-version` | 打印版本后退出 |

## 🔌 端点

### OpenAI 兼容（业务端点，API Key 鉴权）

| 端点 | 说明 |
|---|---|
| `POST /v1/chat/completions` | OpenAI 兼容 chat（流式透传 / 非流式聚合） |
| `GET /v1/models` | 模型列表（craft 可用集，从 `/v3/config` 动态拉取并缓存） |
| `GET /health` | 健康检查（`status` / `region` / `has_token` / `expired` / `version`） |

### 管理后台（`/admin/*`，Cookie 会话）

| 端点 | 说明 |
|---|---|
| `POST /admin/login` `POST /admin/logout` `GET /admin/session` | 密码登录 / 登出 / 会话状态 |
| `GET /admin/account` `POST /admin/account/oauth/start` `GET /admin/account/oauth/poll` | 账号摘要 / OAuth 发起 / OAuth 轮询 |
| `POST /admin/account/refresh` `POST /admin/account/test` `DELETE /admin/account` | 手动刷新 / 测试凭证 / 清空凭证 |
| `GET /admin/resources` | 官方余额（带缓存，`?force=1` 强刷） |
| `GET /admin/checkin/status` `POST /admin/checkin/claim` | 签到状态 / 领取 |
| `GET /admin/api-keys` `POST /admin/api-keys` `PUT /admin/api-keys/{id}` `DELETE /admin/api-keys/{id}` | API Key 增删改查 |
| `GET /admin/logs` | 请求日志（分页 + 筛选 model/key/status） |
| `GET /admin/stats` | 仪表盘聚合 |
| `GET /admin/settings` `PUT /admin/settings` | 读 / 改配置（密码、region、签到、cron 等） |
| `GET /admin/models` `POST /admin/models/refresh` | 模型列表（含当前生效倍率）/ 手动重新拉取 `/v3/config`。倍率取自上游 `models[].credits` 与 `modelPromotions` 折扣活动，按请求时刻实时评估时段窗口；仅作展示参考，非计费接口 |

### Web

| 端点 | 说明 |
|---|---|
| `GET /` | Web 管理面板（go:embed 单 HTML，内嵌 Alpine.js + 手写 SVG 图表，无外部依赖），含 统计 / 账号 / 密钥 / 余额 / 日志 / 设置 六个页面 |

## ⚙️ 配置

优先级：**env > `data/config.json` > 内置默认**。env 统一 `BUDDY2API_*` 前缀：

| env | 说明 |
|---|---|
| `BUDDY2API_LISTEN` | 监听地址（二进制默认 `127.0.0.1:10082`；Docker 镜像内默认 `0.0.0.0:10082`） |
| `BUDDY2API_REGION` | `cn`（默认，`copilot.tencent.com`）/ `global`（`www.codebuddy.ai`），两套端点凭证不互通，切换后需重新扫码登录 |
| `BUDDY2API_ADMIN_PASSWORD` | 管理密码（明文，启动时 bcrypt 哈希写回 config.json，优先级最高）；未设置且无 hash 时默认 `password` |
| `BUDDY2API_AUTO_CHECKIN` | 自动签到开关（默认关闭） |
| `BUDDY2API_CHECKIN_MODE` | 签到方式（默认 `fixed`）：`fixed` 按 cron 定时 / `random` 每天在时间范围内随机一个时刻 |
| `BUDDY2API_CHECKIN_CRON` | `fixed` 模式签到 cron，6 段含秒（默认 `0 0 9 * * *`） |
| `BUDDY2API_CHECKIN_RANDOM_START` / `BUDDY2API_CHECKIN_RANDOM_END` | `random` 模式时间范围 `HH:MM`（默认 `09:00` / `18:00`，结束最晚 `23:30`） |
| `BUDDY2API_CHECKIN_FALLBACK` | 末班兜底（默认开启）：当天没签成时 23:50 尝试，若失败 23:55 重试，再失败当天放弃 |
| `BUDDY2API_RESOURCE_CACHE_SECONDS` | 余额缓存秒数（默认 300） |
| `BUDDY2API_LOG_RETENTION_DAYS` | 日志保留天数（默认 90） |
| `BUDDY2API_LOG_MAX_SIZE_MB` | 日志表容量上限 MB（默认 50） |
| `BUDDY2API_CHAT_TIMEOUT_SECONDS` | chat 上游响应头超时秒数（默认 60，1-3600）。上游自请求发出至开始响应的等待上限，超时即中止本次请求；流式连接建立后（含推理阶段）不再受超时约束。也可在管理后台「设置 → 基本」修改，改后需重启生效 |
| `BUDDY2API_DATA_DIR` | 数据目录（默认 `./data`） |

指纹头伪装另有 `CB_GATEWAY_USER_AGENT` / `CB_GATEWAY_STAINLESS_OS` 等可选 env（一般无需修改），完整变量见 [.env.example](.env.example)。

## 💾 数据与安全

| 文件                  | 说明                                            |
| ------------------- | --------------------------------------------- |
| `data/token.json`   | 账号凭证                                          |
| `data/config.json`  | 全局配置（含 bcrypt 密码哈希）                           |
| `data/buddy2api.db` | SQLite（API Keys / 请求日志 / 缓存），**只存元信息，不存对话内容** |

- API Key 明文存储（管理页可复制完整 Key），校验用常量时间比对
- 二进制直跑默认只监听 `127.0.0.1`；Docker（compose / 本文示例）默认全网卡监听 `0.0.0.0:10082`，局域网可直接访问——公网部署请务必放反代后并加 TLS，或改回仅本机监听
- 出站请求复刻官方 CLI 指纹头；chat 请求绝不携带 refresh_token
- 日志不记录请求/响应正文，只记元信息

## 📁 项目结构

```
Buddy-2API-Go/
├── main.go                # 入口
├── internal/
│   ├── config/            # 配置加载（config.json + env 覆盖）
│   ├── store/             # SQLite 数据层（API Keys / 日志 / 缓存）
│   ├── auth/              # 凭证：token.json 读写、JWT 解析、OAuth 设备流
│   ├── upstream/          # 上游客户端：chat 转发、billing、checkin、指纹头
│   ├── proxy/             # /v1/chat/completions 代理（流式透传 + 非流式聚合）
│   ├── apikey/            # OpenAI 端点 Key 管理（明文存储/随机/校验/限额）
│   ├── admin/             # 管理后台 API（登录/账号/keys/日志/签到/余额/设置）
│   ├── scheduler/         # 签到定时任务
│   └── web/               # 前端（go:embed 单 HTML，内嵌 Alpine.js）
├── Dockerfile
├── docker-compose.yml
├── .env.example
├── LICENSE.txt
└── go.mod / go.sum
```

> `data/` 为运行时自动生成的数据目录，路径说明见上文 [Docker 数据路径](#docker-数据路径重要别挂错)。

## 🛠️ 技术栈

Go 1.25+ · chi · modernc.org/sqlite（纯 Go，无 cgo） · robfig/cron/v3 · bcrypt · Alpine.js（内嵌）+ 手写 SVG 图表 · go:embed 单二进制

仅 3 个非标准库依赖：`chi`、`modernc.org/sqlite`、`cron/v3`，其余用标准库 + `golang.org/x/crypto`。

## 🙏 致谢与参考

- 上游接口契约与指纹头规范等实现参考了以下开源项目：
  - [wicm84266964/Buddy2api](https://github.com/wicm84266964/Buddy2api) —— 指纹头、billing、token 刷新、SQLite schema
  - [Sliverkiss/CodeBuddy2api](https://github.com/Sliverkiss/CodeBuddy2api) —— OAuth 设备流、Web 管理面板结构
  - [cyl2361341082-alt/Buddy2api](https://github.com/cyl2361341082-alt/Buddy2api) —— 非流式聚合、finish_reason 修正等实现思路
  - [ShouZhuo0413/codebuddy2api](https://github.com/ShouZhuo0413/codebuddy2api) —— 部分实现思路
- 实时模型列表接口（`GET /v3/config`）参考了 [kuops/opencode-codebuddy-auth](https://github.com/kuops/opencode-codebuddy-auth)。
- Web 管理面板的 UI 设计参考了 [chenyme/grok2api](https://github.com/chenyme/grok2api)。

## 📄 License

本项目采用 [MIT](LICENSE.txt) 协议开源，仅供学习研究使用。
