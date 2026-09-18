# 更新日志

本项目版本号遵循 [语义化版本](https://semver.org/lang/zh-CN/)。条目按发布时间倒序，每版给出「升级注意 / 新功能 / 修复 / 重构 / 工程与文档」五类，无内容的小节省略。

全量对比：`https://github.com/EchoPing07/Buddy-2API-Go/compare/<旧版本>...<新版本>`

---

## [v0.2.0] - 2026-09-19

> 两条主线：**成长任务体系**移植与**前端多页重构**，另含 5 项修复。
> 对比：[v0.1.8...v0.2.0](https://github.com/EchoPing07/Buddy-2API-Go/compare/v0.1.8...v0.2.0)

### ⚠️ 升级注意

- **前端页面 URL 规范化**：`/dashboard` 与 `/dashboard/` 现在 308 跳回 `/`，各页尾斜杠（如 `/keys/`）308 去掉；页面与静态资源仅接受 `GET` / `HEAD`，其余方法返回统一 JSON `405`。`/v1/*` 与 `/admin/*` 完全不受影响。
- **`GET /admin/stats` 响应变更**：移除 `by_key` 字段；`by_model` 不再截断为 Top 10（前端已同步，第三方脚本消费需注意）。
- **数据库自动新增 `growth_cache` 表**（`CREATE TABLE IF NOT EXISTS`，无需手工迁移）；余额缓存键版本化到 `default_v3`，升级后首次查询自动失效重算，并清理旧缓存行。
- 成长任务为**默认关闭的实验性功能**，仅国内版账号可用；其实现为模拟官方客户端活跃上报，可能不符合上游服务条款，请自行评估风险。

### ✨ 新功能

**成长任务体系**

- 移植活跃地图连登上报、连登奖励兑换（7d / 14d / 28d）、抽奖、补签卡、猫猫领养与旅行。
- 新增「任务」页：每日签到卡从「余额」页迁入，另有连登与奖励 / 活跃地图热力图 / 猫猫旅行状态机三张卡片。
- 自动任务链：**上报 → 连登自检 → 领养 → 礼包 → 补签 → 兑换 → 抽奖 → 旅行巡检**，单步失败不阻断后续；错过当日上报时点则启动 30 秒后自动补跑（22:00 前有效）。
- 风控细节：上报同会话多轮、每条间隔 1.5 秒；日内状态镜像落 SQLite，重启不丢。
- 上报条数支持随机波动（`growth_report_jitter`，`0` 为固定不波动，如 `8±2` → 6–10 条）；每日默认条数由 5 调整为 10。
- 新增 9 个管理端点：`GET /admin/growth/overview`（60s 缓存 + 降级标记）与 8 个手动动作端点（`run` / `report` / `adopt` / `travel` / `redeem` / `lottery` / `makeup` / `bonus`）。

**其他**

- 模型列表每小时 01 分自动刷新，长期运行可自动反映上游上下架；拉取失败仅告警并保留旧表。
- 余额页新增「隐藏已用完」开关（过滤剩余为 0 或已过期的额度包，状态持久化，卡片头显示隐藏数量并提供「显示全部」出口）。
- 账号页刷新 Token 增加二次确认，明确区分「凭证已过期刷新换新」与「有效期内刷新会立即作废现有 access_token」。

### 🐛 修复

- **设置页保存整单失败**：存量非法 cron（env 写错位数 / 旧配置）会让 `PUT /admin/settings` 整单 400，而该字段在功能关闭时前端并不下发，用户既看不见也改不了。现在未随请求提交的 cron 字段会自愈回落默认并告警，显式提交的非法值仍返回 400。
- **HTTPS 部署下会话 cookie 被浏览器丢弃**：`Secure` 改为按链路判定（直连 TLS，或反代声明 `X-Forwarded-Proto: https`，多段取最后一段），`Clear` 与 `Issue` 同口径。
- **额度包卡片内容错位**：关闭「隐藏已用完」过滤后 Alpine 按数组下标复用 DOM 导致串位；新增额度包稳定 key（展示字段内容指纹）供 `x-for` 使用。
- **仪表盘模型列表被截断**：去掉 `by_model` 的 `LIMIT 10` 与前端二次截断，模型行改为滚动容器，`maxReq` 改用 `reduce` 以避免模型数增长触达 `Math.max` 参数上限。

### ♻️ 重构

- **前端拆分为多页文档结构**：`internal/web` 改为 `go:embed` 文件系统 —— `shell.html` + `pages/<key>.html` + `assets/{app.css,app.js,alpine.js}` + `assets/pages/<key>.js`，替代原 2042 行单文件 `index.html`。深链、刷新、新标签页直接可用，每页 `<title>` / `data-page` 各自正确。
- **软导航**：侧边栏链接由 `app.js` 拦截，切页不重载文档、不重新请求资源、已加载数据保活；任务 / 日志页声明 `realtime`，每次进入都刷新（间隔 <5s 退化为保活）；会话过期或退出登录清空保活缓存。
- **HTTP 层收口**：页面与静态资源带内容哈希 `Etag`（`no-cache` + `If-None-Match` → 304），文本响应走 gzip（首页 52KB → 约 12KB，`app.js` 50KB → 约 19KB）；`Range` 请求与 `/v1` 流式响应（SSE）绕过压缩；全站带 `X-Content-Type-Options: nosniff`；405 统一 JSON body 并透传 `Flusher` / `Hijacker` / `ReaderFrom` / `Pusher`，SSE 逐块刷出不受影响。
- **前端样式基元与弹窗能力统一**：新增 `stat-grid`（标签→值→备注三级层次）、`action-row`（卡片底部操作区）、`btn-warn`、`btn.is-active`、图例等基元；确认弹窗支持 `detail` / `okText` / `danger`，非破坏性操作不再恒为红色危险态。
- **任务页信息层次重排与降级收敛**：指标与操作分区；写操作不再被只读查询失败连带隐藏；签到卡独立三态（加载 / 正常 / 失败可重试）；档位「还差 N 天」取数修正并补键盘可达性。
- **设置页卡片拆分与按条件下发**：日志独立成卡，缓存移入「基本」，签到与任务分离；子项随开关收纳；保存时 cron / 随机窗口仅在对应开关与模式下携带、数字字段留空不下发、`chat_timeout` 本地先校验；失败后回滚为服务端真实值，「需重启」提示仅在监听地址真的变更时出现。

### 🧪 工程与文档

- 发布流水线新增**门禁作业**：先跑 `go vet ./...` + `go test ./...`（含需要 `node` 的前端行为测试），不通过则不产出任何制品；二进制与 Docker 作业依赖该门禁。
- 测试函数由 26 个增至 118 个（含 `TestFrontendInitialLoad` 等前端行为回归），新增 `internal/web/web_test.go`、`main_test.go`、`internal/upstream/growth_test.go`、`internal/scheduler/growth_test.go`、`internal/admin/growth_test.go`、`internal/admin/settings_test.go` 等，覆盖路由与 405 / Etag / gzip、cookie Secure、余额聚合回归、任务链编排与跨日边界、错误分类器等。
- README 重写 Web 与成长任务章节、补 5 个新 env、补 `Sliverkiss/workbuddy2api` 致谢与实验性功能风险声明；`.env.example` 补成长任务配置块；新增本文件。
- 统计：44 个文件变更，+8732 / −1594。

---

## [v0.1.8] - 2026-08-31

### 🐛 修复

- **官方总额度多算 500（周期外幻影额度）**：上游 `get-user-resource` 的 `TotalDosage` 按账户级 `CapacityRemain` 聚合，按月周期计费的包（体验版 / 裂变包）周期额度用尽后账户级仍为 500，幻影剩余被恒计入总额度。`total_dosage` 不再透传上游值，改为本地聚合「Σ 未过期包的有效剩余」（周期优先 `capacity_remain` 口径，与包卡片「剩余」合计一致），并新增 `upstream_total_dosage` 字段透传官方原始值。
- billing 无时区日期串改按北京时间（UTC+8）解析，过期判定不再随部署时区漂移（原实现非 +8 时区最多偏差 8 小时）。

### ♻️ 重构

- 余额缓存键 `default` → `default_v2`（缓存存的是加工后结构，换键立即失效旧口径值），写入时顺带清理 v1 遗留行；`store` 新增 `DeleteCacheKey`（幂等 + 表名白名单）。
- 余额页主指标「官方总额度」→「可用额度」，新增官方 TotalDosage 对照行（仅两值不等且 >0 时显示）；额度包卡片改用 `fmtDosage` 修复浮点长尾直出。

---

## [v0.1.7] - 2026-08-24

### ✨ 新功能

- **模型列表展示当前生效倍率**：解析上游 `/v3/config` 的 `models[].credits` 与 `modelPromotions` 折扣活动，`ModelCache.View()` 按请求时刻实时评估折扣窗口（支持跨零点时段窗、IANA 时区、起止日期与优先级），统计页模型 chip 显示为「名称 + 倍率」（如 `GLM-5.3 x0.50`），同名模型自动消歧。
- 近 14 天图表恢复 Tokens 维度：柱 = Tokens 用量（左轴）、线 = 请求数（右轴，Catmull-Rom 平滑），悬停提示展示 请求数 / Tokens 用量 / Credits 三项。

### ⚠️ 升级注意

- `GET /admin/models` 的 `models[]` 由 `{id,name}` 扩展为 `{id,name,credits?,promo?}`；`GET /admin/stats` 的 `daily[]` 新增 `tokens` 字段（前端为唯一消费者，已同步）。
- 倍率仅作展示参考，数据来自上游配置接口而非计费接口，不影响代理转发与积分扣费。

---

## [v0.1.6] - 2026-08-24

### ♻️ 重构

- 仪表盘文案修正：「总 Tokens」→「Tokens 用量」、「累计积分」→「积分消耗」；近 14 天图表折线改为每日 Credits，右轴刻度随 Credit 缩放。

### ⚠️ 升级注意

- `GET /admin/stats` 的 `daily[]` 中 `tokens` 字段移除、新增 `credit` 字段（v0.1.7 已恢复 `tokens` 并与 `credit` 并存）。

---

## [v0.1.5] - 2026-08-22

### ♻️ 重构

- 设置页日志设置独立成卡片：日志保留天数 / 日志大小上限从「签到与缓存」中移出。

---

## [v0.1.4] - 2026-08-20

### 🐛 修复

- **官方余额「已用额度」恒为 0**：免费 / 裂变包按月周期计费，账户级 `CapacityUsed` 恒为 0，真实值在周期级 `CycleCapacityUsed`。现按周期级优先、账户级回退取值，并新增 `numField` 按 `Precise` → 非 `Precise` 顺序取首个有效字段，修复上游仅返回非 Precise 字段或 Precise 为空串时数值恒为 0 的健壮性缺陷。

---

## [v0.1.3] - 2026-08-20

### 🐛 修复

- **chat 流式被 10 分钟总超时强制截断**：上游 chat client 原以 `http.Client.Timeout` 作为全周期总超时，覆盖到读完整个 Body，SSE 含长推理时被强制中断。改为 Transport 的 `ResponseHeaderTimeout`（仅约束「上游多久不开始响应」），流式阶段不再受本端总超时约束，客户端断开由请求 context 兜底。
- 新增 `chat_timeout_seconds` 配置（默认 60），可在管理后台「设置 → 基本」修改，改后需重启生效。

---

## [v0.1.2] - 2026-08-20

### 🐛 修复

- **官方余额到期时间无法获取**：上游对免费 / 裂变包常返回空 `ExpiredTime`，真实到期信息在 `DeductionEndTime`（毫秒时间戳）或 `CycleEndTime`（日期串）。到期时间解析改为按 `ExpiredTime` → `DeductionEndTime` → `CycleEndTime` 顺序回退取首个有效值，跳过空串 / `0` / `9999-99-99` 哨兵；修复额度包 `expire_time` 全空、`days_left` 为 `null` 导致的「到期」恒显示「—」、临期标注与按到期排序失效。

---

## [v0.1.1] - 2026-08-20

### 🐛 修复

- **并发安全与崩溃**：修复 `upstream/billing` 在 `EnsureValid` 与 `Get` 之间因并发登出返回 `nil` 导致的空指针 panic；`auth/TokenStore` 改读写锁 + 独立刷新锁，刷新 HTTP 移出 token 锁外（原实现刷新全程持锁，高并发下最长阻塞 30s），单飞靠「进入后重检 + 落盘前指针校验」。
- **计费与日志准确性**：工具停转重试改为累计 token（首轮 + 重试），不再覆盖首轮 usage 导致漏计费；SSE 写失败 / 客户端断开统一在循环出口记录 `ErrorMsg`，修复「写失败型断开」被误判为成功。
- **错误处理加固**：上游非 200 不再原样透传错误体，改为解析 `{code,msg}` 生成 OpenAI 风格错误；`store` 缓存表名加白名单防 SQL 注入；HTTP 服务异常退出改走统一关闭路径并显式关闭 DB。

### ♻️ 重构

- **鉴权热路径性能**：`apikey/Manager` 内存缓存全量 key（变更后 reload），去掉每请求全表 `SELECT`；`RecordUsage` 改异步队列；Key 比对不再提前 `break`，耗时与命中位置无关。
- 资源列表排序由 O(n²) 冒泡改 `sort.Slice`。

### ⚠️ 升级注意

- Docker 默认监听改为 `0.0.0.0:10082`（局域网可访问），公网部署请放反代并加 TLS，或改回仅本机监听。

---

## [v0.1.0] - 2026-08-19

### ✨ 新功能

- 首个公开版本：CodeBuddy / WorkBuddy 转 OpenAI 兼容代理网关完整实现（单账号、单二进制、内置 Web 管理面板），含 `/v1/chat/completions`（流式透传 / 非流式聚合）、`/v1/models`、`/health`、多 API Key、官方余额、每日签到、请求日志与仪表盘。
- 发布准备：README / License / Release 流水线（交叉编译 8 平台 + SHA256SUMS + 多架构 GHCR 镜像）、凭证导入导出、内嵌 Alpine.js（无外部依赖）、Docker 时区内置 `Asia/Shanghai`。
