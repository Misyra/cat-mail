# CatMail（猫邮）技术规格 v1.1（前后端实现契约）

> 本文档是前后端唯一契约。两个实施方（后端 / 前端）必须严格按本文档的字段名、路径、行为实现。
> 定位：个人/小团队自用的**多供应商事务性邮件 failover 网关**——统一发送 API + 按额度/优先级自动轮换。
> v1.1（2026-11-12 优化轮）：同步幂等键/调用方配额限速/SMTP 入口/通知回执/审计/备份/doctor/自动更新（E8）等已落地契约。
> v1.2（2026-11-14 两轮交付）：同步送达链路三件套（送达事件摄取/健康度送达侧/抑制列表）、SMTP TLS 入口、邮件模板、域名治理面板与安全加固三件套（凭据加密/Webhook 签名+outbox/会话管理）等已落地契约。

## 1. 总体架构

```
用户/程序 ──> POST /v1/send (Bearer cm-xxx) ──┐
本地 SMTP 应用 ──> SMTP 入口 (可选启用, E1) ──┤
                                              ├─> [轮换调度器] ──> Provider适配器 ──> Resend/Brevo/Mailjet/SMTP2GO/Mailgun/Elastic Email/Mailtrap/阿里云/SMTP/Mock
邮件平台 ──> POST /webhooks/mail-events/:token (送达事件摄取, 送达链路三件套) ──┤
管理员   ──> SPA 管理后台 (/api/*, Cookie) ───┘                    (SQLite | PostgreSQL, 单二进制)
运维     ──> /metrics (Prometheus) · catmail doctor · /docs 内嵌文档 · /api/backups · /api/update/* (E8)
```

- 后端：Go + Gin + GORM + glebarez/sqlite（纯 Go，无 CGO）+ robfig/cron v3 + google/uuid + pquerna/otp（TOTP）+ gorm postgres 驱动（D2，按 DSN 自动选方言）
- 前端：React 19 + Vite 7 + TypeScript + Tailwind CSS v4 + react-router v7 + @tanstack/react-query v5 + recharts + lucide-react + sonner
- 前端构建产物输出到 `backend/internal/web/dist`，由后端 `go:embed` 打包，最终**单二进制**部署
- 后端默认监听 `:8080`，环境变量（backend/internal/config/config.go，非法值一律启动即 `log.Fatalf` 快速失败，绝不带病启动）：

| 环境变量 | 说明 | 默认值 |
|---|---|---|
| `CATMAIL_ADDR` | HTTP 监听地址 | `:8080` |
| `CATMAIL_DB` | 数据源：SQLite 文件路径（目录自动创建）；或 `postgres://`/`postgresql://` 连接串走 PostgreSQL（pgx stdlib） | `./data/catmail.db` |
| `CATMAIL_ADMIN_USER` | 首启初始化的管理员用户名（仅数据库无账号时生效） | `admin` |
| `CATMAIL_ADMIN_PASS` | 首启初始化的管理员密码（仅数据库无账号时生效） | `admin123` |
| `CATMAIL_COOKIE_SECURE` | `1`/`true` 时会话 Cookie 附加 Secure（HTTPS/反代 TLS 部署用） | 未设置（关闭） |
| `CATMAIL_ADMIN_CIDR` | 管理面 IP 白名单（E5）：逗号分隔 CIDR（仅 CIDR 记法，单主机写 /32 或 /128），作用于 login、全部会话内 `/api/*` 与 `/metrics`；`/api/health`、`/v1/send`、`/emails` 豁免 | 未设置（不限制） |
| `CATMAIL_TRUSTED_PROXIES` | 可信反代 CIDR 列表（逗号分隔）：设置后 gin 才信任 X-Forwarded-For 解析真实客户端 IP；反代部署必须设置，否则白名单与登录限流的 IP 维度失效 | 未设置（gin 默认） |
| `CATMAIL_SMTP_ADDR` | SMTP 入口监听地址（E1）：空 = 功能关闭；如 `:2525` 启用（见 §2 SMTP 入口节） | 未设置（关闭） |
| `CATMAIL_SMTP_MAX_SIZE_MB` | 单封邮件 DATA 载荷上限（MB，正整数）：EHLO SIZE 广播与 552 拒收共用 | `25` |
| `CATMAIL_SMTP_TLS_ADDR` | SMTP 隐式 TLS 监听地址（465 风格）：空 = 关闭；如 `:465` 启用——accept 后立即 TLS 握手再进会话（防开放中继/AUTH/额度语义与明文入口一致）；仅在证书已配置时可用 | 未设置（关闭） |
| `CATMAIL_SMTP_CERT_FILE` / `CATMAIL_SMTP_KEY_FILE` | TLS 证书与私钥路径（必须成对配置，文件可读且能作为密钥对加载）：配置后明文监听广播 STARTTLS（RFC 3207）且隐式 TLS 监听可用；见 §2 SMTP 入口节。未配置维持纯明文现状 | 未设置（纯明文） |
| `CATMAIL_SMTP_TLS_POLICY` | STARTTLS 策略：`optional`（默认，明文监听广播 STARTTLS 但不强制）/ `require`（明文监听 AUTH 前必须 STARTTLS 升级成功，否则 530；需证书已配置）。非法取值启动即退出 | `optional` |
| `CATMAIL_SECRET_KEY` | 渠道凭据静态加密密钥（安全加固三件套）：base64 编码的 32 字节随机数（`openssl rand -base64 32` 生成）。配置后 Channel 敏感字段 AES-256-GCM 加密落库（`enc:v1:` 前缀密文）并一次性迁移存量明文（幂等）；未配置 = 明文兼容，但库中已有密文时拒绝启动。非法取值（非 base64 / 解码后 ≠32 字节）启动即退出 | 未设置（明文兼容） |
| `CATMAIL_UPDATE_REPO` | 自动更新源仓库（E8，`owner/repo`）：空 = 功能关闭（/api/update/* 返回 400） | 未设置（关闭） |
| `CATMAIL_UPDATE_PRERELEASE` | 更新检查是否纳入预发布版本（`1`/`true`） | 未设置（仅正式版） |
| `CATMAIL_UPDATE_CHECK_ON_START` | 启动后异步检查一次更新，发现新版本经 notify 双通道广播（仅 REPO 已配置时生效） | 未设置（关闭） |

- 请求体上限：发送入口 `/v1/send`、`/emails` 30MB（容纳 base64 附件），管理 API `/api/*` 1MB；超限统一 400「请求体过大」

### 目录结构

```
CatMail/
├── SPEC.md                     ← 本文件
├── README.md  PLAN.md  CHANGELOG.md  LICENSE  research/
├── Dockerfile  docker-compose.yml  .github/workflows/ci.yml
├── scripts/                    ← smoke.sh（集成冒烟）· e2e-server.sh
├── backend/
│   ├── go.mod                  (module catmail, go 1.27+)
│   ├── cmd/catmail/main.go    — 路由装配 + SPA 静态资源 + SMTP 入口启停 + doctor 子命令 + 优雅退出
│   └── internal/
│       ├── config/config.go        — 环境变量配置（快速失败校验）
│       ├── model/                  — User/Setting/ApiKey/Channel/SendLog/IdempotencyRecord/AuditLog/Notification + Template/MailEvent/Suppression/Session/WebhookDelivery（models_*.go）
│       ├── store/store.go          — 打开DB（SQLite|PG 自动选方言）、AutoMigrate、首启初始化、备份族（VACUUM INTO）；encryption.go 凭据加密启动期检查与存量迁移
│       ├── provider/               — 适配器（resend/brevo/mailjet/smtp2go/mailgun/elasticemail/mailtrap/aliyun/smtp/mock）+ 默认额度 + 共用 HTTP 栈
│       ├── rotator/                — 轮换调度引擎（额度/域名过滤/failover/冷却/自动禁用/渠道级限速）+ 通知与回执埋点 + 抑制列表拦截（suppress.go）
│       ├── api/                    — server.go 路由 + 各 handler + 中间件（会话/IP白名单/幂等/Key策略）+ /metrics + 审计 + 送达事件入站端点（handler_webhook.go）
│       ├── smtpserver/             — SMTP 入口（E1，可选启用）：会话/防开放中继/STARTTLS 与隐式 TLS/邮件模板报文头指令/状态码映射
│       ├── inbound/                — 送达事件入站基础设施（Svix/Mailgun 真实签名校验 + 防重放 + 事件归一化 + 退信率告警）
│       ├── render/                 — 邮件模板 {{var}} 渲染（点路径/HTML 语境转义/缺失变量收集）
│       ├── secretcrypt/            — 渠道凭据 AES-256-GCM 静态加密（CATMAIL_SECRET_KEY，gorm serializer 收敛点）
│       ├── httpsig/                — 出站 Webhook 签名契约（X-CatMail-Signature: t=…,v1=…）
│       ├── outbox/                 — Webhook 投递 outbox（到期捞取/行级认领/指数退避/10 次转 dead/签名投递）
│       ├── notify/                 — 管理员通知双通道（站内 + Webhook，6 事件）：webhook 经 outbox 落库投递
│       ├── receipt/                — 调用方 Webhook 回执（D1，send_accepted/send_failed）：经 outbox 落库投递
│       ├── scheduler/              — cron（outbox 投递驱动 / 冷却释放+通知/审计/outbox dead/会话注册表裁剪+每日备份 / 每日日志清理）
│       ├── doctor/                 — catmail doctor 自检（E7，8 大项）
│       ├── update/                 — 自动更新（E8）：GitHub Releases 检查 + SHA-256 校验落盘
│       ├── version/version.go      — 版本号单一来源（-ldflags 注入）
│       ├── docs/                   — /docs 内嵌文档页 + openapi.yaml（B2）
│       └── web/web.go              — go:embed dist + SPA fallback
└── frontend/                   ← React SPA（构建产物输出到 backend/internal/web/dist）
```

---

## 2. 统一发送 API（对外，供用户程序调用）

### `POST /v1/send`

认证：`Authorization: Bearer <ApiKey>`（ApiKey 形如 `cm-` + 32位随机串）。

请求体（JSON，Resend 风格扁平结构；兼容接收 camelCase 别名 replyTo）：

```json
{
  "from": "CatMail <noreply@mail.example.com>",
  "to": ["a@example.com"],
  "subject": "Hello",
  "html": "<p>Hi</p>",
  "text": "Hi",
  "cc": ["b@example.com"],
  "bcc": ["c@example.com"],
  "reply_to": "support@example.com",
  "headers": { "X-Custom": "v" },
  "attachments": [ { "filename": "a.pdf", "content_base64": "JVBERi..." } ],
  "tags": [ { "name": "campaign", "value": "nov" } ],
  "channel": "resend主号"
}
```

| 字段 | 必填 | 说明 |
|---|---|---|
| from | 是（带回退） | `"Name <addr>"` 或纯 `"addr"`（≤1024 字符）；解析出 name+addr。请求未提供时：指定 channel → 用该渠道默认发件人（渠道也未配置 → 400）；未指定渠道 → 留空放行，由 rotator 按各候选渠道的默认发件人逐候选回退（见轮换算法） |
| to | 是 | string 或 string[]，统一转数组；单地址 ≤320 字符且须为合法邮箱 |
| subject | 是 | ≤998 字符（RFC 单头行上限） |
| html / text | 二选一 | 至少一个非空；各 ≤1048576 字符（1MB），超出 → 400「正文过大」 |
| cc / bcc / reply_to | 否 | string 或 string[]；reply_to 同样接受 string[]；to+cc+bcc 合计 ≤100 |
| headers | 否 | object，string→string；≤50 个，键非空 ≤256 字符、值 ≤4096 字符；键值不得含换行符（`\r`/`\n`，防 SMTP/MIME 头注入）→ 400 |
| attachments | 否 | filename（必填，≤255 字符）+ content_base64；也接受别名 `content`；≤10 个，base64 须可解码（容忍中间换行/空格） |
| tags | 否 | `{name,value}[]`，≤20 个，name 必填；适配器尽力透传 |
| channel | 否 | 指定使用的渠道名（精确匹配，≤128 字符）；指定后轮换仅在该渠道内进行：不存在→400，域名不匹配→400，禁用/冷却/超额→429 |
| template_id | 否 | 引用管理端邮件模板发送（见 §2「邮件模板」小节）：渲染结果整体替换 subject/html/text（请求 subject 一并被覆盖，模板发送下三字段皆可缺省）；与自带非空 html/text 并存 → 400 `body_conflict`；模板不存在 → 400 `unknown_template` |
| variables | 否 | 模板渲染变量对象（键 ≤100 个，键名非空 ≤128 字符，支持点路径取嵌套值）；仅 template_id 存在时生效，否则忽略（零开销直通） |

成功响应 `200`：

```json
{
  "id": "9c1e...(=send_log.message_id)",
  "channel": "resend主号",
  "provider": "resend",
  "provider_message_id": "e30ea...",
  "accepted": true,
  "missing_variables": ["user.name"]
}
```

（`missing_variables` 仅在使用 template_id 且渲染变量有缺失时附带：缺失变量占位符留空、发送不阻断，变量名清单供调用方排查。）

错误响应统一（所有接口）：

```json
{ "error": { "type": "invalid_request|unauthorized|rate_limited|quota_exhausted|provider_error|forbidden|server_error", "message": "人类可读原因" } }
```

- 部分参数错误附机器可读 `error.code` 字段（加法扩展 `{error:{type,code,message}}`，不影响既有调用方对 type/message 的解析）：`recipient_suppressed`（收件人命中抑制列表）、`unknown_template`（template_id 不存在）、`body_conflict`（template_id 与自带 html/text 并存）
- 参数错误 → 400 invalid_request（**不换渠道重试**，视为调用方问题）
- 收件人（to+cc+bcc 任一）命中抑制列表 → 400 invalid_request + `code=recipient_suppressed`（被拒请求不进入轮换、不预留额度、不落发送日志，message 列出命中的收件人与原因）
- key 无效/停用 → 401 unauthorized
- 调用方限速（Key 配置了 rate_per_sec 且等待超时）→ 429 rate_limited「请求过于频繁（rate_per_sec 限制）」
- 所有可用渠道额度耗尽 / 指定渠道禁用、冷却、超额 / Key 日额度用尽 / 候选全部限速等待 → 429 quota_exhausted
- 尝试了所有候选渠道仍失败 → 502 provider_error（message 附最后一次平台错误，形如「已尝试 N 个渠道均发送失败：<平台错误>」）
- 管理面来源不在 IP 白名单 → 403 forbidden（仅 login、会话内 `/api/*` 与 `/metrics`）
- 内部错误 → 500 server_error（客户端仅见「服务器内部错误」，详情入服务端日志）
- 未知路径（`/api`、`/v1`、`/emails`、`/webhooks` 及其子路径下的未注册路径）→ 404，type=invalid_request「接口不存在」
- 内部 message_id(uuid) 贯穿日志；请求级去重见下节幂等键（可选）

### 调用方维度配额与限速（B5，ApiKey 四字段）

强制点在发送入口鉴权（/v1/send、/emails、SMTP 入口认证会话三处同口径），**先限速后额度**，被拒不发送、不落发送日志（拒绝在门外）：

1. `rate_per_sec` > 0：按 Key 维度令牌桶限速（进程内按真实时间补充）；等待超过上限（API 路径 3s）→ 429 rate_limited。0 = 不限（零延迟直通）。
2. `daily_quota` > 0：统计该 Key 今日 `status=sent` 的 SendLog 条数（含测试发送与 SMTP 入口发送，共用同一额度池），达到上限 → 429 quota_exhausted「ApiKey 今日额度已用尽（daily_quota=N）」。0 = 不限（零额外 DB 开销）。

### 幂等键防重发（E2，可选请求头 `Idempotency-Key`）

- 仅作用于发送入口（/v1/send 与 /emails）；**头缺失 = 完全不启用去重**，行为与无幂等完全一致（零额外 DB 访问）。作用域绑定 ApiKeyID——不同调用方同 key 互不干扰。
- 键合法性：去首尾空白后 1–128 字符、全部为可见 ASCII（0x21–0x7E，键内空格拒绝）；非法 → 400 invalid_request（**不静默降级为无 key**：显式携带却被去掉去重比报错更糟）。
- 状态机：首次请求 INSERT `pending` 档案并放行执行 → 响应（成功或失败）经响应捕获包装器**存档 status code + 响应体**（`done`）；同 key 重放：
  - `done` → 直接逐字节回放存档响应，响应头附加 `Idempotent-Replay: true`（本次未实际发送）；
  - `pending` 且未超时 → 409 invalid_request「相同的 Idempotency-Key 请求正在处理中」；
  - `pending` 超过 5 分钟 → 视为进程崩溃残留（僵尸），原子认领重发；
  - 档案超过 24h TTL → 懒惰清理（仅带 key 的请求顺手 DELETE，无定时任务），同 key 视为无行重新发送。
- **失败响应（400/429/502）同样存档重放**：调用方带同 key 重试拿到同一错误而非触发二次发送（幂等语义的明示取舍）。

### Resend SDK 兼容端点（已确认支持）

`POST /emails`：与 `POST /v1/send` 完全同构（同样的 Bearer ApiKey 认证、请求/响应/错误体、幂等键、30MB 请求体上限），专为让用户把 Resend SDK 的 base_url 指向 CatMail 后零改造使用。两条路径共用同一 handler。

Resend SDK 兼容细节（dto.go）：
- `stringOrList` 兼容解析：`to`/`cc`/`bcc`/`reply_to` 既接受 `"a@b.com"` 单字符串也接受数组；`replyTo`（camelCase）为等价别名，`reply_to` 优先，`reply_to` 为空时才读别名
- attachments：`{filename, content_base64}`，兼容 Resend SDK 实际发送的 `{filename, content}` 别名（`content_base64` 优先，为空才读 `content`）
- tags：`{name, value}[]` 原样结构透传；headers：string→string 对象透传
- `/emails` 下未知子路径（`/emails/*`）返回统一 JSON 404 错误体，不落 SPA fallback（与 `/v1` 同构）

### SMTP 入口（E1，可选启用）

`CATMAIL_SMTP_ADDR` 未设置（默认空）= 功能关闭，不监听任何端口；设置（如 `:2525`）即启用——只支持 SMTP 的本地应用把 CatMail 当普通 SMTP 服务器用，DATA 收完后解析 MIME（text/html 并存时 multipart/alternative）转交 rotator 走渠道轮换发送，**不重造发送语义**：额度预留/渠道级限速/Key 配额限速/failover/冷却/发送日志全部复用轮换器现状。

- 防开放中继（按对端 IP 逐连接判定，而非只看监听地址）：对端为 loopback（127.0.0.0/8、::1、localhost）允许**免认证**；非 loopback 必须 `AUTH PLAIN|LOGIN`（RCPT 前 530 拦截），凭据 = password 是某个 enabled 的 ApiKey（username 任意），命中后记 ApiKeyID 用于日志归属与 Key 级限速/日额度（SMTP 发送同样落库归因，与 /v1/send 共用额度池）。监听 0.0.0.0 时环回免认证与远程强制 AUTH 并存，不会意外变成开放中继。
- EHLO 扩展：PIPELINING、SIZE（=配置值）、8BITMIME、AUTH PLAIN LOGIN、STARTTLS（证书已配置时广播，见下）；收件人上限 100（与 API 对齐）；单行 64KB 上限；单命令 60s / 单连接 5min 超时；handler panic recover；优雅关停（3s 自然收尾 + 2s 强制）。
- 传输安全（TLS，可选，安全加固配套）：
  - **STARTTLS（RFC 3207）**：`CATMAIL_SMTP_CERT_FILE`/`CATMAIL_SMTP_KEY_FILE` 配置证书后，明文监听 EHLO 广播 STARTTLS；时机限 EHLO 之后、邮件事务之前（顺序错误 503），回 220 后立即握手——成功则连接/读写器整体替换为 TLS 版本并**复位全部会话状态**（问候/认证/事务丢弃，须重新 EHLO），失败断开（连接完整性已不可信）；TLS 1.2 为最低版本。
  - **隐式 TLS 监听**：`CATMAIL_SMTP_TLS_ADDR`（465 风格）启用——accept 后立即 TLS 握手再进会话（握手失败直接断开）；隐式端口同样广播 STARTTLS 扩展但会话内真发起该命令回 503「TLS already active」（广播但不重复协商）。
  - **策略**：`CATMAIL_SMTP_TLS_POLICY=require` 时明文监听必须先 STARTTLS 升级成功才允许 AUTH，否则 530「Must issue a STARTTLS command first」（防明文密码泄露）；隐式 TLS 监听与已升级会话天然豁免。`optional`（默认）广播但不强制。
  - **兼容现状**：未配证书不广播 STARTTLS（发 STARTTLS → 502），维持纯明文行为与旧版完全一致——公网加密场景仍可由反代/隧道（stunnel、frp、cloudflared 等）承担 TLS 终结后转发到本入口。证书不可读/密钥对不匹配/隐式监听缺证书/策略取值非法，一律启动即退出。
- 邮件模板（报文头指令）：DATA 报文携带 `X-CatMail-Template: <模板ID>`（可选 `X-CatMail-Template-Vars: <JSON对象>` 或 `base64:<base64(JSON)>`，供 UTF-8 变量值稳妥携带）即按管理端模板渲染后发送，语义与 /v1/send 的 template_id+variables 完全同口径（见「邮件模板」小节）；指令非法/`unknown_template`/`body_conflict` 均为内容类永久失败 → 501。
- 状态码映射（rotator 统一错误 → SMTP 应答）：

| rotator 结果 | SMTP 应答 |
|---|---|
| accepted (200) | `250 2.0.0 OK queued as <message_id>` |
| invalid_request (400) | `501 5.6.0`（永久失败：内容/参数问题，客户端需改内容重试） |
| quota/rate (429) | `452 4.2.1`（临时失败：可稍后重试） |
| 502/其他 | `451 4.3.0`（临时失败：候选渠道穷尽/内部错误，可重试） |
| MAIL SIZE 超限 / DATA 超限 | `552 5.3.4` |
| 收件人超 100 | `452 4.5.3` |
| 非 loopback 未认证 | `530`（RCPT 前拦截） |
| TLS_POLICY=require 且明文连接发起 AUTH | `530 5.7.0 Must issue a STARTTLS command first` |

- 边界（刻意取舍）：日志落库与失败通知/回执口径与 API 发送一致（429 不落日志但有通知/回执，502 落最后一条失败日志）；抑制列表命中与模板错误按 400 参数类映射 501 永久失败（错误文案含 `recipient_suppressed`/错误码，客户端需改收件人或指令重试）。

### 邮件模板（发送入口可选渲染，三入口同口径）

管理端维护可复用的内容骨架（templates 表，见 §5/§4），三个发送入口（`/v1/send`、`/emails`、SMTP 入口报文头指令；渠道测试发送同样支持）统一经 template_id + variables 引用渲染：

- **渲染时机**：幂等检查之后、rotator.Send 之前——渲染结果即发送快照，幂等键重放语义不变（重放直接回放存档响应，不重渲染）；SMTP 入口在 MIME 解析后、转交轮换器前渲染。
- **替换语义**：渲染结果整体替换 subject/html/text（请求 subject 被覆盖；模板发送下请求可三字段皆缺省）；`SendLog.template_id` 记录所用模板（0=未使用），模板删除后历史日志保留 ID 供追溯。
- **占位符**：`{{var}}`（容忍两侧空白），变量名支持点路径取嵌套对象值（`{{user.name}}`，来自调用方 variables JSON 键）；**HTML 正文语境的变量值做 HTML 转义（`<>&'"`，无逃生门——不提供关闭转义的选项）**，subject/text 为纯文本/邮件头语境原样替换。
- **缺失变量**：占位符留空、不阻断发送（failover 网关可用性优先）；变量名清单在成功响应 `missing_variables` 字段与服务端日志可见（SMTP 路径仅服务端日志）。
- **冲突与未知**：template_id 与请求自带非空 html/text 并存 → 400 `body_conflict`（显式失败优于静默覆盖：模板优先会掩盖调用方误留旧正文的参数 bug；subject 不参与冲突判定——Resend SDK 必填 subject，报错会破坏 SDK 同构迁移）；模板不存在 → 400 `unknown_template`。
- 变量清单为模板派生数据：CRUD 未显式提供时按 subject/html/text 内容自动提取；variables 对象 ≤100 键、键名非空 ≤128 字符。

### 渠道可用性判定与轮换算法（rotator）

```
抑制拦截（统一 hook 点，Send 唯一入口）：To+Cc+Bcc 任一命中 suppressions
  → 400 invalid_request（code=recipient_suppressed，message 列出命中收件人与原因）
  不进轮换、不预留额度、不落发送日志；查询失败 fail-open 放行（防护机制不让 DB 抖动打死发送）
candidates = 所有 status==ENABLED 的渠道
  （AUTO_DISABLED/MANUAL_DISABLED 永不参与；COOLDOWN 且 cooldown_until<=now 视为可用并立即复位为 ENABLED）
  域名过滤：渠道 AllowedDomains 非空且请求提供了 from 时，from 的域名不在列表 → 剔除；
    请求未提供 from 时不做域名过滤（实际发件域名由候选渠道的默认发件人决定，回退后的域名天然匹配）
  指定 channel：请求带 channel 字段 → 候选仅该渠道（不存在或域名不匹配 → 400 invalid_request；
    手动/自动停用、冷却未到期、额度用尽 → 429 quota_exhausted）
对每个候选惰性重置额度（单条条件 UPDATE，仅 anchor 为过期值时生效）：
  day_anchor != 今天            → used_day=0,  day_anchor=今天
  month_anchor != 本月(YYYY-MM) → used_month=0, month_anchor=本月
过滤：daily_limit>0 且 used_day>=daily_limit → 剔除；monthly_limit 同理
若无候选 → 429 quota_exhausted

排序：settings.rotation
  = "priority"(默认)   → priority 升序，同级按 last_used_at 升序（nil 视为最久未用；ID 兜底保证稳定）
  = "round_robin"      → last_used_at 升序（忽略 priority）
attempts = min(settings.max_attempts, len(candidates))，逐个尝试：
  出站限速（A1）：先取渠道级令牌桶令牌（RateLimitPerSec 封/秒，容量=max(1,rate)，0=不限）
    再预留额度；等待超 5s 或 ctx 取消 → 跳过该候选（不计尝试、不失败计数、不预留额度
    ——限速视作临时不可用，继续 failover；令牌不退还，失败重试同样受限）
  额度预留失败（并发下被其他请求抢先占满）→ 不计一次尝试，直接看下一候选
  发送前原子预留额度（P1-1）：单条条件 UPDATE 抢占 used_day/used_month 各 +1
    （anchor 非当前周期按 0 起算；生效额度 = override>0 ? override : 平台默认，0=不限；
    SQLite 单写者保证不会超卖；写库失败 fail-closed 视为超额跳过）
  请求未带 from 且候选配置了默认发件人 → 用渠道默认填充（逐候选独立，作用在消息副本上）
  成功(accepted)：
    预留转为最终记账：last_used_at=now、consecutive_failures=0、last_error=""
    记 SendLog(status=sent)，返回 200；推送 send_accepted 回执（D1，Key 配置了 webhook 才发）
  err.DisableChannel (平台401/403)：
    status=AUTO_DISABLED + auto_disabled_reason + last_error，同时清残留 cooldown_until；
    释放预留额度；换下一家
  err 需冷却 (429，含 Brevo 400+not_enough_credits)：
    status=COOLDOWN + cooldown_until=now+settings.cooldown_seconds；释放预留额度；换下一家
  err.Retryable (网络错误/5xx/402)：
    consecutive_failures 原子 +1（UPDATE ... RETURNING，并发不丢计数）；
    若 >= settings.consecutive_failure_threshold(默认5)
      → AUTO_DISABLED(reason="连续失败N次")；释放预留额度；换下一家
  else (参数类 4xx)：
    释放预留额度；记 SendLog(status=failed)，直接返回 400 invalid_request（不换渠道）
  panic 安全（O8）：预留成功后 defer 配对兜底释放（quotaSettled 标记防双重释放），
    transport panic 不再静默多计额度（api 层 Recovery 兜底进程不崩）
一个候选都没实际尝试 → 429 quota_exhausted：
  全部因限速等待跳过 → 文案「所有候选渠道处于出站限速等待中（rate_limit_per_sec 限制）」；
  全部输在预留环节 → 沿用额度耗尽文案
全部重试耗尽 → 记 failed 日志（附最后一次平台错误），返回 502 provider_error
```

> 铁律：平台"受理(accepted)"之后绝不换渠道重发；额度净增只发生在受理成功时（预留 → 受理保留 / 失败释放），静止状态下 used_day/used_month 只因受理成功 +1。
> 释放按预留周期锚点配对（O8）：跨日/跨月后释放不多减新周期计数。

---

## 3. Provider 适配器契约

```go
// provider/provider.go
type Message struct {
    FromName, FromAddr   string
    To, Cc, Bcc, ReplyTo []string   // ReplyTo 可能来自 string 或 string[]
    Subject, Text, HTML  string
    Headers              map[string]string
    Attachments          []Attachment // {Filename string; ContentBase64 string}
    Tags                 []Tag        // {Name, Value string}
}
type Result struct{ ProviderMessageID string }
type ProviderError struct {
    HTTPStatus     int    // 平台返回的HTTP状态码，网络错误为 0
    Code, Message  string
    Retryable      bool   // 网络错误/429/5xx/402 = true
    DisableChannel bool   // 401/403 = true
    Cooldown       bool   // 429 = true
}
type Transport interface {
    Name() string
    Send(ctx context.Context, ch *model.Channel, m *Message) (*Result, *ProviderError)
}
var registry = map[string]Transport{...}
// "resend" "brevo" "mailjet" "smtp2go" "mailgun" "elasticemail" "mailtrap" "aliyun" "smtp" "mock"（共 10 平台）
```

所有 HTTP 适配器统一：`http.Client{Timeout: 30s}`；响应 body 截断至 2KB 放入错误信息；json 解析出平台 `message/id` 字段。

### 平台映射表（必读）

| provider | endpoint | 认证 | 格式 | 关键字段映射 | 成功取ID |
|---|---|---|---|---|---|
| resend | `POST https://api.resend.com/emails` | `Authorization: Bearer <api_key>` | JSON | from `"Name <a@b>"`；attachments `[{filename, content}]`；reply_to；headers；tags `[{name,value}]` | `id` |
| brevo | `POST https://api.brevo.com/v3/smtp/email` | header `api-key: <api_key>` | JSON | `sender{name,email}`；`to/cc/bcc=[{email}]`（裸地址）；`replyTo={email,name}` 单对象（取第一个）；`attachment[{name,content}]`；`headers`；`tags=[string]`(取 tag.value) | `messageId` |
| mailjet | `POST https://api.mailjet.com/v3.1/send` | Basic `<api_key>:<api_secret>` | JSON | `Messages:[{From:{Email,Name}, To/Cc/Bcc:[{Email}], ReplyTo:{Email}(取第一个), TextPart, HTMLPart, Headers, Attachments:[{Filename,ContentType,Base64Content}]}]`；tags 无映射（不透传） | `Messages[0].To[0].MessageID`（MessageID 是数字，转字符串） |
| smtp2go | `POST https://api.smtp2go.com/v3/email/send` | header `X-Smtp2go-API-Key: <api_key>` | JSON | `sender`（"Name <addr>" 全格式）；`to/cc/bcc` 字符串数组（全格式透传）；`text_body/html_body`；官方无 reply_to 字段 → Reply-To 取第一个经 `custom_headers` 透传；`custom_headers:[{header,value}]`（过滤官方禁设的结构性头 Content-Type/Content-Transfer-Encoding/MIME-Version）；`attachments:[{filename,fileblob,mimetype}]`（fileblob 存 base64，mimetype 按扩展名猜测） | `data.email_id` |
| mailgun | region=eu 用 `https://api.eu.mailgun.net` 否则 `https://api.mailgun.net`，路径 `/v3/{domain}/messages` | Basic `api:<api_key>` | **multipart/form-data** | from/to/cc/bcc 可重复字段（"Name <addr>" 全格式）；subject/text/html；自定义头 `h:<Name>`（headers 中键为 Reply-To 的跳过）；Reply-To 经 `h:Reply-To`（可重复）；`o:tag` 每个 tag 一个可重复 part；附件：base64 解码后作为 file part（`attachment`，filename 原样，Content-Type 按扩展名猜测，application/octet-stream 兜底）；多附件发多个 part | `id`（形如 `<2024...@domain>`） |
| elasticemail | `POST https://api.elasticemail.com/v4/emails/transactional` | header `X-ElasticEmail-ApiKey: <api_key>` | JSON | `Recipients:{To:[{Email}]}`；`Content:{From, Subject, Body:[{ContentType: HTML/PlainText, Content}], Attachments:[{Name, BinaryContent, ContentType}], CustomHeaders?}`；reply_to 取第一个并入 Content.ReplyTo；tags 无映射 | `MessageID`（空则 `TransactionID`） |
| mailtrap | `POST https://send.api.mailtrap.io/api/send` | header `Api-Token: <api_key>` | JSON | body 与 Resend 同构但联系人一律对象：`from:{email,name}`、`to/cc/bcc:[{email}]`、`reply_to:{email}`（取第一个）；`attachments:[{filename, content(base64), content_id?, disposition?}]`；tags 无映射 | `message_ids[0]` |
| aliyun | `POST https://dm.aliyuncs.com`（RPC 风格：form 业务参数 + query 公共参数，HMAC-SHA1 POP 签名，Action=SingleSendMail，Version=2017-06-06） | 渠道 APIKey=AccessKeyId、APISecret=AccessKeySecret | form | `AccountName`=渠道 from_email（发信地址，必填）、`FromAlias`、`AddressType=1`、`ToAddress`（逗号分隔 ≤100）、`Subject`（≤100 字符）、`TextBody`/`HtmlBody`；**不支持附件**；Headers 丢弃；tags 需控制台预创建 TagName，不透传（防 400） | `EnvId`（空则 `RequestId`） |
| smtp | 通用 SMTP（host/port/user/pass/tls: `none` 25 / `starttls` 587 / `tls` 465） | SMTP PLAIN/LOGIN（按服务器 EHLO advertised 机制协商：PLAIN 优先、仅 LOGIN 时用 LOGIN、未 advertise 回退 PLAIN；用户名空=匿名） | MIME multipart/mixed（text+html 并存时嵌套 multipart/alternative）+ base64 附件；Message-ID(`<smtp-<uuid>@主机名>`)/Date/Subject(=UTF-8 B 编码)/Reply-To/自定义头（跳过结构性保留头）；From/To/Cc/Reply-To 头经 net/mail Address.String()（非 ASCII 显示名走 RFC 2047 编码） | From=FromName\<FromAddr\>；envelope from=FromAddr；tags 无映射 | 无 → 生成 `<smtp-<uuid>@主机名>` |
| mock | 无网络请求 | — | — | 直接成功，延迟 50ms；渠道 APIKey 设为魔法值可模拟失败以验证轮换路径：`fail`→模拟 5xx（可重试）、`fail_rate_limit`→模拟 429（可重试+冷却）、`fail_auth`→模拟 401（自动禁用渠道）；其余/空=发送成功（首启预置的「示例渠道」行为不变） | `mock-<uuid>` |

通用约束：
- 渠道未配置 from 且请求也未提供 → invalid_request（smtp 渠道必须配置 from_email）
- 渠道配置 ProxyURL 时：HTTP 适配器统一经代理出站（支持 http://、https:// 与 socks5://，socks5 未写端口默认 1080）；smtp 适配器仅支持 socks5://（含 socks5h）；未配置则直连（不读环境变量代理）
- `tags` 透传口径：resend/brevo/smtp2go/mailgun 透传，mailjet/smtp/elasticemail/mailtrap/aliyun 不透传（"尽力透传"的尽力范围）
- 每个适配器文件风格一致；`headers` 为空时不发任何自定义头字段
- 非 2xx 响应统一分类：401/403 → DisableChannel；429 → Retryable+Cooldown；402/5xx → Retryable；其余 4xx → 参数类（三者皆否）；唯一例外：Brevo 额度用尽返回 HTTP 400 + `code=not_enough_credits`（官方错误码表），按限流同款 Retryable+Cooldown 处理（进冷却，避免继续把消息打到已无额度的渠道）；网络错误 → Retryable
- 错误信息提取：平台 message 字段深度优先提取（顶层与嵌套结构均兼容，最多下探 5 层——Mailjet `Messages[0].Errors[].ErrorMessage` 等嵌套形态可提取），错误信息截断（message 300 字符、body 2KB）；响应体读取上限 1MB（防超大响应撑爆内存）
- `defaults.go` 平台默认额度（0=不限，由平台报错兜底）：
  resend{100/日, 3000/月} · brevo{300/日, 0} · mailjet{200/日, 6000/月} · smtp2go{200/日, 1000/月} · mailgun{100/日, 0} · elasticemail{100/日, 3000/月} · mailtrap{150/日, 4000/月} · aliyun{200/日, 2000/月} · smtp{0, 0} · mock{0, 0}

---

## 4. 数据模型（GORM，SQLite | PostgreSQL 同构）

```go
User     {ID, Username unique(64), PasswordHash(bcrypt), CreatedAt,
          SessionEpoch int default:0,        // 按用户会话纪元（O10）：本人改密/被重置密码同事务 +1，
                                             // 该用户旧纪元会话全部失效；存量库补列旧行取 0，无需回填
          TOTPSecret string default:'',      // C4 两步验证：base32 密钥（setup 生成，enable 校验通过前置位）
          TOTPEnabled bool default:false,    // 两步验证启用位
          RecoveryCodes string default:''}   // JSON 数组，存每个恢复码 sha256 hex（明文仅 enable 响应一次性返回）
Setting  {ID=1, Rotation string, MaxAttempts int=3, CooldownSeconds int=60,
          ConsecutiveFailureThreshold int=5, LogRetentionDays int=90, // 0=永久保留
          SessionSecret string(首启随机生成32B hex),
          SessionEpoch int,                  // 已废弃（O10）：全局纪元代码路径已移除，列残留无害（gorm 不删列）
          NotifyInappEnabled bool default:false,   // 站内通知中心开关（O9）
          NotifyWebhookEnabled bool default:false, // Webhook 通知开关
          NotifyWebhookURL string default:'',      // webhook 地址（空=未配置）
          NotifyEvents string default:'',          // 事件白名单 csv 子集，空=全部 6 事件（5 基础事件 + bounce_rate_exceeded）
          LogBodyEnabled bool default:true,        // A4 日志正文隐私开关：false 时新日志正文存空串（只影响新日志）
          BackupDailyEnabled bool default:false,   // A2 每日自动备份开关（scheduler 每小时任务检查执行）
          SuppressionAutoEnabled bool default:true, // 抑制列表自动入库开关：hard bounce 与 complained 自动写入 suppressions
          BounceRateThreshold float64 default:5,   // 退信率告警阈值（百分比 0-100，0=关闭）：超阈值广播 bounce_rate_exceeded warn
          NotifyWebhookSecret string default:'',   // 通知 Webhook 签名密钥（32B hex；空=不签名不发头）——仅经
                                                   // POST /api/settings/notify/webhook-secret 生成/轮换，PUT /api/settings 不受理
          UpdatedAt}
ApiKey   {ID, Name, Key unique(64) "cm-...", Enabled bool,
          WebhookURL string default:'',      // D1 调用方回执（显式 opt-in，http(s)://）
          WebhookEvents string default:'',   // 回执事件白名单 csv（send_accepted|send_failed，空=不推送）
          DailyQuota int default:0,          // B5 调用方每日发送成功上限（status=sent 口径，0=不限）
          RatePerSec int default:0,          // B5 调用方令牌桶限速（封/秒，0=不限）
          WebhookSecret string default:'',   // 回执签名密钥（32B hex）：创建 Key 自动生成，POST /api/keys/:id/webhook-secret 轮换；
                                             // 回执投递据此生成 X-CatMail-Signature（空=不签名不发头）
          CreatedBy string(64) default:'',   // 创建者用户名：密码重置联动停用按此归属圈定；空=无主 Key 不受联动影响
          DisabledReason string default:'',  // password_reset=密码重置联动停用（管理页展示徽标，手动启用即清空）；空=常规停用
          LastUsedAt *time, CreatedAt}
Channel  {ID, Name unique(128), Provider string,     // 10 平台枚举见 §3
          Status int,             // 0=enabled 1=manual_disabled 2=auto_disabled 3=cooldown
          Priority int=0,
          APIKey string,          // brevo/mailjet(public)/mailgun/resend/smtp2go/elasticemail/mailtrap/aliyun(AccessKeyId) 共用
          APISecret string,       // mailjet private key / aliyun AccessKeySecret
                                  // ↑ 凭据静态加密（安全加固三件套）：APIKey/APISecret/SMTPPass/ProxyURL/WebhookSecret
                                  //   五字段挂 gorm serializer "encsecret"（AES-256-GCM，CATMAIL_SECRET_KEY），
                                  //   读写透明加解密、密文 enc:v1: 前缀；未配密钥明文兼容，启动期一次性迁移存量明文
          Domain string,          // mailgun 发送域名
          Region string,          // mailgun: "us"(默认)|"eu"
          ProxyURL string,        // 出站代理 http:// 或 socks5://（smtp 仅 socks5）；空=直连（可内嵌代理凭据，一并加密）
          AllowedDomains string,  // 允许的发件域名，逗号分隔；空=不限制（轮换按 from 域名过滤）
          FromEmail, FromName string,   // 渠道默认发件人（smtp/aliyun 渠道必填）
          SMTPHost string, SMTPPort int, SMTPUser, SMTPPass string, SMTPTLS string, // none|starttls|tls
          DailyLimitOverride int, MonthlyLimitOverride int,   // 0=用 provider 默认
          RateLimitPerSec int default:0, // 出站限速（封/秒，渠道级令牌桶，容量=max(1,rate)）；0=不限
          UsedDay int, DayAnchor string,   // YYYY-MM-DD
          UsedMonth int, MonthAnchor string, // YYYY-MM
          CooldownUntil *time,
          ConsecutiveFailures int, LastError string, AutoDisabledReason string,
          LastUsedAt *time, CreatedAt, UpdatedAt,
          WebhookToken string(64) idx default:'', // 送达事件入站 token（每渠道 256bit 随机 hex，创建/复制自动生成，
                                                  // POST /api/channels/:id/webhook-token 补生成/轮换）：公开端点
                                                  // /webhooks/mail-events/:token 据此定位渠道，对无签名平台兼作共享 secret
          WebhookSecret string default:''}        // 平台侧签名密钥（Resend whsec_ 签名串 / Mailgun Webhook Signing Key），
                                                  // 空串=该渠道仅按 URL token 校验；encsecret 加密字段
SendLog  {ID, MessageID(uuid) idx, ApiKeyID uint idx, ChannelID uint idx,
          ChannelName, Provider string,     // 冗余快照（已删渠道的历史发送仍可展示/聚合）
          IsTest bool,
          Status int idx,         // 0=pending 1=sent 2=failed
          TemplateID uint default:0, // 所用邮件模板（发送请求携带 template_id 时记录，渲染结果随本日志快照落库）；
                                     // 模板删除后保留该 ID 供追溯
          FromAddr string, ToAddrs string(json), Subject string,
          HTMLBody, TextBody string, // 正文落库（Setting.LogBodyEnabled=false 时新日志存空串）
          ProviderMessageID string, Error string, Attempts int, LatencyMs int64, CreatedAt idx}
IdempotencyRecord {ID,                     // E2 幂等键防重发档案（状态机见 §2）
          ApiKeyID uint + Key string(128) 组合唯一索引, // 同一调用方同 key 仅一条；并发首投由唯一索引兜底
          Status string,                   // pending | done
          ResponseStatus int, ResponseBody string, // 响应存档（done 后重放逐字节一致）
          MessageID string,                // 成功响应的内部 uuid（错误路径为空）
          CreatedAt idx}                   // 认领基准时间：TTL/僵尸判定锚；24h 懒惰清理，无定时任务
AuditLog {ID, Username string idx(64),      // E4 管理操作审计：只增不改不删（应用层），scheduler 裁剪至 5000 条
          Action string,                   // 25 枚举：channel_create/channel_update/channel_delete/channel_status/
                                           //   channel_duplicate/channel_webhook_token/key_create/key_update/key_delete/
                                           //   key_webhook_secret/user_create/user_reset_password/user_delete/
                                           //   password_reset_linkage/sessions_revoke/settings_update/
                                           //   settings_webhook_secret/purge_logs/backup_create/backup_delete/
                                           //   template_create/template_update/template_delete/
                                           //   suppression_create/suppression_delete
          Target string,                   // 形如 "channel#3 name=示例渠道"
          Detail string,                   // ≤500 字符截断；仅字段名/计数等非敏感摘要（秘钥/密码绝不入审计）
          IP string(64), CreatedAt idx}
Notification {ID, Event string,           // 6 枚举：channel_disabled|channel_cooldown|channel_recovered|send_failed|
                                        //   update_available(E8)|bounce_rate_exceeded（送达链路三件套，warn）
          Level string,           // info|warn|error
          Title, Body string,     // 事件文案由后端埋点生成（body 截断有界）
          ChannelID *uint idx,    // 关联渠道；与具体渠道无关的事件为空
          Read bool, CreatedAt}   // (read, created_at) 组合索引：已读过滤 + 倒序分页/未读计数；每小时清理至 1000 条
Template {ID, Name unique(128),          // 邮件模板（三发送入口经 template_id 引用，渲染语义见 §2）
          Subject string,                // 模板主题（纯文本语境渲染，非空 ≤998）
          HTML string default:'',        // 模板正文：各 ≤1MB 且至少一个非空
          Text string default:'',
          Variables string default:'',   // 变量名清单 JSON 数组：缺省由 subject/html/text 自动提取（派生数据）
          CreatedAt, UpdatedAt}
MailEvent {ID, ChannelID idx, ChannelName, Provider string, // 冗余快照（已删渠道历史事件仍可展示）
          Event string,                  // 归一化事件：sent|delivered|bounced|complained|raw（识别不了的一律 raw 存档不猜测）
          BounceType string,             // hard|soft（仅 bounced 填写；裸 bounce 按 soft 保守处理，防误入抑制列表）
          ProviderMessageID string idx(255), // 平台消息 ID（原始形态）
          MessageID string(64), SendLogID uint, // 尽力按 provider_message_id（去尖括号）反查本渠道 SendLog 关联，0=未解析
          Recipient string(320),         // 收件人（小写归一）
          Detail string(≤500),           // 原始事件名/退信原因/诊断码
          Raw string(text),              // 原始报文 JSON 存档（form 报文转 JSON）
          OccurredAt time,               // 平台事件时间（缺省取摄取时刻）
          CreatedAt idx}                 // 摄取时间（7 天送达侧健康度窗口过滤口径）
Suppression {ID, Email unique(320)（小写归一）, // 收件人维度全局抑制（跨渠道生效，命中即拒发）
          Reason string,                 // bounce（hard bounce 自动）|complaint（complained 自动）|manual（管理端手动）
          ChannelID uint, ChannelName string, // 首次命中来源渠道快照（0=手动添加未指定）
          Detail string, CreatedAt}      // hard bounce/complaint 自动入库（SuppressionAutoEnabled，默认开，幂等不覆盖）
Session {ID string(64) pk,               // 服务端会话注册表（安全加固三件套）：一行对应一次登录签发的 v3 token
          Username string idx(64),
          IP string(64), UserAgent string(512), // 登录来源快照（展示用）
          CreatedAt, LastSeenAt,         // requireSession 惰性刷新（节流 60s）
          ExpiresAt idx,                 // 与 token exp 一致（登录 +7d；重签顺延）
          RevokedAt *time idx}           // 非 nil = 已吊销（踢出/登出/重置联动）——requireSession 查注册表即拒，即时生效
WebhookDelivery {ID,                     // Webhook 投递 outbox（安全加固三件套）：通知与回执先落库再由 scheduler 投递
          EventType string idx(64),      // 事件名快照（通知 6 事件 / send_accepted|send_failed / test）
          Payload string(text),          // 原始 JSON 请求体（投递逐字节原样发送并参与签名）
          URL string(1024),              // 投递目标（入队时快照）
          SecretRef string(128) default:'', // 签名密钥引用（投递时现取现签）：settings:notify_webhook_secret |
                                            // api_key:<id>；空串=不签名不发头；引用失效（Key 已删）→ 转 dead
          Attempts int,                  // 已失败尝试次数（达 10 转 dead；成功投递不再递增）
          Status string(16) idx组合(status,next_retry_at), // pending | sent | dead
          NextRetryAt time, LastError string, CreatedAt, UpdatedAt}
                                         // 退避节奏：失败后 1m→5m→30m→2h→8h→24h 封顶；dead 保留最近 100 条供排查
```

存储：`effectiveDailyLimit(c) = c.DailyLimitOverride>0 ? override : providerDefault`；effectiveMonthlyLimit 同理。`data/` 目录自动创建。SQLite 打开参数：WAL + busy_timeout 5000 + TranslateError（唯一约束冲突映射 gorm.ErrDuplicatedKey，上层兜底 400）。PostgreSQL（D2）：`CATMAIL_DB=postgres://…` 时走 gorm postgres 驱动，AutoMigrate/CRUD/窗口函数聚合双方言同构（健康度 Top3 的 ROW_NUMBER 两方言均支持）；**备份族为 SQLite 专属**——`DBFilePath/CreateBackup/ListBackups/BackupFilePath/DeleteBackup` 在 PG 连接上统一返回 `store.ErrSQLiteOnly`，接口 400 透传（请用 pg_dump/pg_restore）。
所有新增列均带 gorm 默认值：存量库 AutoMigrate 补列时旧行取零值/安全默认（通知关、正文照记、额度不限），无需回填。
备份（A2）：`VACUUM INTO` 生成整库快照到 `<数据目录>/backups/catmail-backup-<时间戳>.db`，保留最近 10 份轮换；下载接口按文件名白名单校验（仅放行备份命名模式，防路径穿越）。
首启初始化（无数据时）：admin 用户 + Setting 默认行 + 一条 mock 示例渠道（名称「示例渠道」，status=enabled，默认发件人 sender@example.com /「CatMail 示例」——开箱即可测试发送，可删），开箱即可体验轮换。

---

## 5. 管理 API（Cookie 会话）与端点总表

会话：登录成功后 `Set-Cookie: catmail_session=<token>`（HMAC-SHA1 签名的 v3 格式 `v3|exp|username|user_epoch|session_id|sig`，7 天有效，secret 用 Setting.SessionSecret；user_epoch 为「按用户会话纪元」users.session_epoch（O10）——本人改密/被重置密码成功后该用户纪元 +1，该用户携带旧纪元的全部会话立即失效，其他用户会话不受影响；session_id 对应服务端会话注册表 sessions 表一行）。**版本前缀机制使 v2 与更早格式的存量 token 一律失效**，升级后重新登录一次即可，零数据迁移。签名与纪元校验之外，中间件再查注册表（未吊销、未过期、属主匹配），使「踢会话」成为服务端可执行动作（吊销即时生效，无需等 token 过期）；LastSeenAt 惰性刷新（节流 60s），scheduler 每小时清理过期行与吊销满 7 天的行。Cookie 属性 HttpOnly + SameSite=Lax + Path=/（`CATMAIL_COOKIE_SECURE` 开启时附加 Secure）。中间件校验除登录与登录第二步外所有 `/api/*`（并复核用户仍存在与用户纪元匹配），未登录 → 401。空 SessionSecret 一律拒绝签发/校验。
管理面 IP 白名单（E5）：`CATMAIL_ADMIN_CIDR` 配置后，`POST /api/auth/login`（含 login/totp）、全部会话内 `/api/*` 与 `/metrics` 先过白名单，白名单外来源未登录即 403 forbidden；`/api/health`、`/v1/send`、`/emails`、`/docs` 豁免（探针/调用方/文档可来自任意来源）。
登录限流：同 用户名+IP 组合 10 分钟内失败 ≥5 次锁定 15 分钟，期间一律 429「尝试次数过多，请稍后再试」，成功登录清零计数；另设仅用户名维度的独立计数（ClientIP 可被 X-Forwarded-For 伪造轮换，用户名维度保证单账号爆破始终受限）；不存在的用户同样计数，且走等时长的 bcrypt 哨兵比对（防按响应时间枚举用户名）。
两步验证（C4）：账号启用 TOTP 后，login 第一步凭据校验通过即返回 `{totp_required:true, ticket}`（`t|` 前缀 HMAC 短票据，5 分钟有效，与会话 cookie 双向隔离——verifySession 拒绝 t| 票据、login/totp 拒绝会话 cookie）；第二步 `POST /api/auth/login/totp {code|recovery_code}` 换正式会话（登录即写会话注册表）。恢复码共 8 个（A-Z2-7 免混淆字符集），每个一次性使用，仅 enable 响应明文返回一次。

### 端点总表（与 server.go 现状一致；完整请求/响应契约见内嵌 `/docs` 与 `backend/internal/docs/openapi.yaml`）

**公开（无会话）**

| 方法/路径 | 说明 |
|---|---|
| GET `/api/health` | 健康探针 → `{ok:true}`（不受白名单约束） |
| GET `/docs` | 内嵌 API 文档页（单文件 HTML，OpenAPI 3.1 规范渲染，零外部依赖；公开无鉴权——内容均为脱敏占位示例） |
| GET `/docs/openapi.yaml` | OpenAPI 规范原文（与 SPEC 同步维护；调用方接入指南另见 `docs/API.md`） |
| GET `/metrics` | Prometheus 文本格式（version 0.0.4，UTF-8）：`catmail_sends_total{channel_name,provider}`、`catmail_test_sends_total{…}`、`catmail_channels{id,name,provider,state}`、`catmail_notifications_unread` 四族；实时聚合不缓存；受管理 IP 白名单约束（抓取器不登录） |
| POST `/webhooks/mail-events/:token` | 送达事件入站 webhook（送达链路三件套，公开无会话鉴权、不受白名单约束）：`:token` 为每渠道独立 256bit URL token（创建/复制渠道自动生成，管理端可复制/轮换），据其定位渠道并对无签名机制的平台（Brevo/SMTP2GO/Mailjet/Elastic Email/Mailtrap/aliyun 等）兼作共享凭据；Resend（Svix 规范：whsec_ 密钥、`svix-*` 三头、±5min 容差）与 Mailgun（Webhook Signing Key、timestamp+token HMAC、±15min 容差）签名头/字段齐备即**必须验真**（验败 401 不回退 token），窗口内重放（svix-id/Mailgun token）进程内去重拒绝；事件报文归一化 sent/delivered/bounced(hard\|soft)/complained/其他 raw 存档进 mail_events（含原始报文 JSON 与 SendLog 关联）→ `{ok:true}`；hard bounce/complaint 自动入抑制列表（开关默认开）；bounced 触发退信率告警判定。token 无效/已轮换 → 404（统一错误体，不区分渠道不存在）。请求体上限 1MB |

**发送入口（Bearer ApiKey，见 §2）**：`POST /v1/send` · `POST /emails`（同构，含幂等键）

**会话与账号**

| 方法/路径 | 请求 → 响应 |
|---|---|
| POST `/api/auth/login` | `{username,password}` → 未启用两步验证：签发会话 cookie → `{username}`；已启用：`{totp_required:true, ticket}`（凭据校验通过才发票据）。首启无 User 记录用 env 默认创建；见上方登录限流 |
| POST `/api/auth/login/totp` | `{code}` 或 `{recovery_code}`（凭 `t|` 票据）→ 签发会话 cookie → `{username}` |
| POST `/api/auth/logout` | 清 cookie → `{ok:true}` |
| GET `/api/auth/me` | → `{id, username}` |
| PUT `/api/auth/password` | `{old_password,new_password}`（bcrypt 校验旧密码）；成功后本人 session_epoch+1 并**重签当前会话 Cookie**（当前会话保持登录），本人其他已签发会话全部失效需重新登录；改密+纪元递增事务化 |
| POST `/api/auth/totp/setup` | 生成 160bit base32 secret 存库（enabled 仍 false）→ `{secret, otpauth_url}`（绑定未完成不影响登录） |
| POST `/api/auth/totp/enable` | `{code}`（±1 窗口校验）→ enabled=true，生成 8 个恢复码 → `{recovery_codes:[…]}`（**明文仅此一次**，库存 sha256） |
| POST `/api/auth/totp/disable` | `{code|password}` 校验后关闭并清空 secret/恢复码 |
| GET `/api/users` | → `{items:[{id,username,created_at}]}`（登录用户均为管理员角色，全部可见）|
| POST `/api/users` | `{username,password}` → 新建账号（重名 400，上限 10 个）|
| PUT `/api/users/:id/password` | `{new_password}` 管理员重置任意账号密码；成功后**单事务联动三件套**（安全加固三件套，C）：目标用户 session_epoch+1 + 全部活跃会话吊销（sessions 表）+ **该用户名下全部 API Key 停用**（按 created_by 归属圈定，enabled=false + disabled_reason=password_reset，管理页展示「密码重置后停用，可手动启用」徽标，手动启用即恢复；存量无主 Key 不受影响）；操作者及其他用户会话不受影响；审计 user_reset_password + password_reset_linkage（detail 计数） |
| DELETE `/api/users/:id` | 删除账号（不能删自己；必须至少保留一个账号；事务内复核）|

**会话管理（安全加固三件套，C）**

| 方法/路径 | 请求 → 响应 |
|---|---|
| GET `/api/sessions` | → `{items:[{id,ip,user_agent,created_at,last_seen_at,expires_at,current}]}` 本人活跃（未吊销未过期）会话，按最后活跃倒序；`current:true` 标记当前会话 |
| DELETE `/api/sessions/:id` | 踢本人指定会话（吊销只写注册表，即时生效、不影响 API Key）；不能踢当前会话（→400，下线自己走登出）；非本人会话一律 404；审计 sessions_revoke |
| POST `/api/sessions/revoke-others` | 无请求体 → 下线本人除当前外全部活跃会话 → `{revoked}`（幂等）；审计 sessions_revoke |

**渠道管理**

| 方法/路径 | 请求 → 响应 |
|---|---|
| GET `/api/channels` | → `{items:[ChannelDTO]}`（**脱敏**：api_key/api_secret 只返回前4后4掩码，proxy_url 含凭据时掩码；含 effective_daily/monthly_limit、used_day/month、status、last_error、auto_disabled_reason、rate_limit_per_sec、webhook_token（送达事件入站 token，前端拼 `/webhooks/mail-events/<token>` 完整地址）、webhook_secret（平台签名密钥掩码）；**附带 `health`**：近 7 天健康度 A3——`{window_days:7, total, sent, failed, success_rate(0-100), last_success_at, last_failed_at, top_errors:[{reason(≤120字符), count}≤3]}`，无日志渠道为 null；**送达侧（送达链路三件套升级）**：`delivered/bounced/complained` 为 7 天窗口内事件计数，`delivery_rate/bounce_rate/complaint_rate`（0-100 一位小数）= 计数/终态事件合计（delivered+bounced+complained，避开 sent 事件可选上报的平台差异），窗口内无终态事件的渠道为 null）|
| POST `/api/channels` | ChannelDTO（含明文 key）→ 创建；name/provider 校验（provider ∈ 10 平台枚举）|
| PUT `/api/channels/:id` | 部分更新；省略字段 = 保留原值，秘钥字段（api_key/api_secret/proxy_url/smtp_pass/webhook_secret）传空 = 保留；可选 `clear_secrets`：字符串数组显式清除秘钥字段，合法值 ⊆ {"api_key","api_secret","proxy_url","smtp_pass","webhook_secret"}（去重、空白项忽略、含未知值 → 400）；语义优先级：clear_secrets 显式清除 > 指针字段保留语义；同字段同时传非空新值时新值生效；清除后的合并结果照常经渠道合法性校验；更新后 status 复位为 0、连续失败/auto_disabled_reason/last_error 清空、cooldown_until 置空；落库仅写表单可写集与复位字段，不回写运行时用量计数 |
| DELETE `/api/channels/:id` | 删除 |
| POST `/api/channels/:id/status` | `{action:"enable"\|"disable"}` → 手动启停（disable→1，enable→0 且清冷却/失败计数）|
| POST `/api/channels/:id/test` | `{to:"x@y.com"}`（合法邮箱，必填）+ 可选 `{template_id, variables}`（邮件模板渲染替换默认测试主题/正文，unknown_template/变量校验失败在预留额度前拒绝）→ 直发测试（不走轮换候选排序/failover），走 SMTP/HTTP 适配器；渠道未配置默认发件人 from_email → 400（测试固定用渠道默认发件人）；**发送前先原子预留额度**（rotator.ReserveQuota，与轮换路径同一套条件 UPDATE）：受理成功保留扣减、发送失败释放（预留/释放按周期锚点配对）；额度已用尽 → 429 统一错误体 `{error:{type:"quota_exhausted",message:"渠道当日/当月额度已用尽，测试发送未执行"}}` 且不落发送日志；测试失败不触发禁用/冷却/连续失败累计（人工诊断行为），停用/冷却渠道仍可测试；记 IsTest 日志 → 成功 `{ok:true,latency_ms}`、失败 `{ok:false,latency_ms,error:"平台错误文案"}`（HTTP 200：管理端直发诊断接口）|
| POST `/api/channels/:id/duplicate` | 复制渠道（A3）：全字段复制（含 secrets）→ 新记录命名为 `<原名>-副本`（重名依次 `-副本-2/-副本-3…`），status=manual_disabled、运行态（用量/失败计数/错误/冷却/最近使用）全部归零 → `{id,name}` |
| POST `/api/channels/:id/webhook-token` | 送达事件入站 token 补生成/轮换（送达链路三件套）：请求体可选 `{rotate:true}`（空 body = 仅 token 为空时补生成，存量渠道升级后首次调用即补齐）→ `{webhook_token, webhook_url}`（webhook_url 为 `/webhooks/mail-events/<token>` 相对路径，前端拼 origin）；轮换后旧 URL 立即失效，平台侧需更新 webhook 地址；审计 channel_webhook_token（ensure/rotate，token 值不入审计） |

**发送日志**

| 方法/路径 | 请求 → 响应 |
|---|---|
| GET `/api/logs` | `?page(≥1)&page_size(1-100,默认20)&status&channel_id&q(主题/收件人模糊，≤128字符)&sort` → `{items,total}`；status：`sent`/`failed`/`pending`（空=全部）；sort：`newest`（默认）/`oldest`/`latency_desc`，非法值 → 400；列表项不含正文大字段，id 兜底排序保证分页稳定 |
| GET `/api/logs/export` | 查询参数与 `/api/logs` 完全一致（忽略 page/page_size）→ CSV 下载（UTF-8 BOM、公式注入防护 =+-@ 前置单引号、行数上限 10000 并在备注列注明）；列：时间/状态/渠道/平台/发件人/收件人/主题/耗时ms/测试/错误信息/消息ID/备注 |
| GET `/api/logs/:id` | 完整详情（含正文；LogBodyEnabled=false 期间的新日志正文为空串） |
| POST `/api/logs/:id/resend` | 按日志快照（from/to/subject/html/text）原样重发一封（A3）：走轮换、落新日志、生成新 message_id；无正文/无收件人的历史日志 → 400；**30 秒防滥用**：同一日志 30s 内重复重发 → 429 quota_exhausted「该日志 30 秒内已重发过，请稍后再试」→ 成功响应同 `/v1/send` |
| POST `/api/logs/purge` | 手动清理发送日志（A2）：`{mode:"before_days"\|"keep_last"\|"all", days?, keep?}` 三模式，分批删除（500/批）→ `{deleted}`；落审计 purge_logs |

**送达事件（送达链路三件套，入站摄取在公开端点 `/webhooks/mail-events/:token`）**

| 方法/路径 | 请求 → 响应 |
|---|---|
| GET `/api/mail-events` | `?page(≥1)&page_size(1-100,默认20)&event(sent/delivered/bounced/complained/raw,空=全部)&bounce_type(hard/soft)&channel_id&q(收件人模糊,≤128字符)` → `{items,total}`（occurred_at 倒序、id 兜底稳定；列表不含 raw 大字段）|
| GET `/api/mail-events/:id` | 完整详情（含 raw 原始报文 JSON，前端可格式化展示）|

**抑制列表（送达链路三件套）**

| 方法/路径 | 请求 → 响应 |
|---|---|
| GET `/api/suppressions` | `?page&page_size(1-100,默认20)&reason(bounce/complaint/manual)&channel_id&q(email 模糊,≤128字符)` → `{items:[{id,email,reason,channel_id,channel_name,detail,created_at}],total}`（created_at 倒序）|
| POST `/api/suppressions` | `{email,detail?}` 手动添加：email 必填（合法邮箱 ≤320 字符，入库小写归一；重复 → 400）、reason 恒为 manual（bounce/complaint 仅由事件自动入库产生）；审计 suppression_create |
| DELETE `/api/suppressions/:id` | 解除（物理删除该行；同邮箱此后的 hard bounce 事件会重新自动入库——解除的是当前抑制状态，不是拉黑豁免）；审计 suppression_delete |

**邮件模板（三发送入口同口径引用，渲染语义见 §2）**

| 方法/路径 | 请求 → 响应 |
|---|---|
| GET `/api/templates` | → `{items:[{id,name,subject,variables,created_at,updated_at}]}`（全量列表 id 升序，不含正文大字段）|
| POST `/api/templates` | `{name,subject,html?,text?,variables?}`：name 1-128 字符（唯一，重名 400）、subject 非空 ≤998、html/text 各 ≤1MB 且至少一个非空；variables 显式提供（≤100 条，去重保序、空数组=清空）或缺省时按内容自动提取 → 详情响应；审计 template_create（仅记变量条数）|
| GET `/api/templates/:id` | 完整详情（含 html_body/text_body）|
| PUT `/api/templates/:id` | 指针语义部分更新（nil=保留；name/subject 显式空串视作未修改）；variables 未提供时按合并后内容重新提取；重名 400；审计 template_update（仅记变更字段名）|
| DELETE `/api/templates/:id` | 删除（历史 SendLog.template_id 保留追溯）；审计 template_delete |

**API Keys**

| 方法/路径 | 请求 → 响应 |
|---|---|
| GET `/api/keys` | → `{items:[{id,name,masked_key,enabled,webhook_url,webhook_events,webhook_secret,daily_quota,rate_per_sec,disabled_reason,total_sent,last_used_at,created_at}]}`（掩码 `cm-abcd…wxyz`；webhook_secret 明文回显供管理页查看/复制（/api 会话保护口径同设置页 notify_webhook_secret）；disabled_reason=password_reset 时前端展示联动停用徽标；total_sent=该 key 的 SendLog status=sent 总数，单条 GROUP BY 聚合，含测试发送）|
| POST `/api/keys` | `{name, webhook_url?, webhook_events?, daily_quota?, rate_per_sec?}`（name 1-64 字符；webhook_url 须 http(s):// 且 host 非空；webhook_events ⊆ {send_accepted,send_failed}；配额/限速 ≥0）→ `{id,name,key,…}`（**明文 key 仅此一次返回**；回执签名密钥 webhook_secret 创建时自动生成 32 字节 hex）|
| PUT `/api/keys/:id` | `{name?,enabled?,webhook_url?,webhook_events?,daily_quota?,rate_per_sec?}` 指针语义：nil=保留；联动停用（disabled_reason=password_reset）的 Key 手动启用即清空该标记 |
| DELETE `/api/keys/:id` | 删除 |
| POST `/api/keys/:id/webhook/test` | 回执连通性测试（D1）：按该 Key 当前配置向 webhook_url 推一条 `event:"test"` 回执（带签名，secret 未配置不签名）→ `{ok, status?, error?}` |
| POST `/api/keys/:id/webhook-secret` | 轮换回执签名密钥（安全加固三件套，B）→ `{webhook_secret}`（新值供管理页展示/复制）；轮换后存量未投递的 outbox 回执任务按新值签名；审计 key_webhook_secret |

**统计**

| 方法/路径 | 请求 → 响应 |
|---|---|
| GET `/api/stats/overview` | → `{today_sent, today_failed, month_sent, success_rate(0-100), channels:[{id,name,provider,status,used_day,effective_daily_limit,used_month,effective_monthly_limit,last_error}]}`（sent/failed 统计不含测试发送；用量按 anchor 归一化展示；合并聚合单条 SQL，凭据类列不下发）|
| GET `/api/stats/daily?days=30&channel_id=` | → `{items:[{date:"MM-DD",sent,failed}]}`（本地时区按天分桶，days 1-90 默认 30，缺数据日补 0；不含测试发送）|

**通知中心（O9）**

| 方法/路径 | 请求 → 响应 |
|---|---|
| GET `/api/notifications` | `?page&page_size(1-100,默认20)` → `{items:[{id,event,level,title,body,channel_id,read,created_at}],total,unread}`（created_at/id 倒序；event 6 枚举见 §4 Notification）|
| GET `/api/notifications/unread_count` | → `{count}`（顶栏未读数轮询） |
| POST `/api/notifications/read` | `{ids:number[]\|null}`（null/缺省=全部已读；空数组 no-op）→ `{ok,updated}`（幂等） |
| POST `/api/settings/notify/test` | 无请求体，按「已保存」设置实测双通道：站内恒落一条测试 Notification；webhook 未启用 → `webhook_ok=true`；已启用 → 实测地址（带签名，与 outbox 真实投递同一签名契约）→ `{inapp_ok,webhook_ok,webhook_error}` |
| POST `/api/settings/notify/webhook-secret` | 通知 Webhook 签名密钥生成/轮换（安全加固三件套，B）：`{rotate?}`（空 body/rotate=false = 仅未配置时生成）→ `{webhook_secret}`（32 字节随机 hex）；PUT `/api/settings` 不受理该字段（防整表单保存误清），变更仅经本端点；审计 settings_webhook_secret（secret 值不入审计） |

**设置 / 审计 / 数据管理 / 更新**

| 方法/路径 | 请求 → 响应 |
|---|---|
| GET `/api/settings` / PUT | Rotation("priority"/"round_robin")/MaxAttempts(1-10)/CooldownSeconds(≥1)/ConsecutiveFailureThreshold(3-20)/LogRetentionDays(0-365，0=永久)，越界 → 400；通知四字段 NotifyInappEnabled/NotifyWebhookEnabled/NotifyWebhookURL/NotifyEvents（URL http(s) 校验；notify_events 仅允许 6 事件 token 子集，空=全部）；`log_body_enabled`（A4，默认 true）；`backup_daily_enabled`（A2，默认 false）；`suppression_auto_enabled`（送达链路三件套，默认 true：hard bounce/complaint 自动入抑制列表）；`bounce_rate_threshold`（0-100，默认 5：渠道退信率告警阈值，0=关闭告警）；`notify_webhook_secret` GET 回显明文 / PUT 忽略（变更仅走 webhook-secret 端点）；GET 响应额外带 `weak_admin_password`（首启种子账号口令命中常见弱口令清单时为 true，bcrypt 比对结果按哈希缓存）；设置行缺失时 PUT 会补建并生成随机会话密钥 |
| GET `/api/audit` | E4 审计查询：`?page&page_size(1-100)&action=&username=` → `{items:[{id,username,action,target,detail,ip,created_at}],total}`（created_at/id 倒序；action 前缀匹配（action=channel 命中全部 channel_*），escapeLike 防注入；username 精确匹配）|
| GET `/api/webhook-deliveries` | Webhook 投递记录排查（安全加固三件套，B）：`?page(≥1)&page_size(1-100,默认20)&status(pending/sent/dead)&event_type` → `{items:[{id,event_type,url,secret_ref,attempts,status,last_error,next_retry_at,created_at,updated_at}],total}`（id 倒序；投递引擎见 §4 WebhookDelivery 与 §7 cron——payload 不回显，事件正文可能含收件人等敏感信息）|
| GET `/api/backups` | 备份列表 `{items:[{name,size,created_at}]}`（倒序；PG → 400 ErrSQLiteOnly）|
| POST `/api/backups` | VACUUM INTO 整库快照 + 保留 10 份轮换 → `{name,size}`；落审计 backup_create；PG → 400 |
| GET `/api/backups/:name/download` | 下载备份文件（文件名白名单校验防路径穿越；Content-Disposition attachment）|
| DELETE `/api/backups/:name` | 删除备份；落审计 backup_delete |
| GET `/api/update/check` | E8 更新检查：→ CheckResult `{current, latest?, update_available, release_url?, notes?, published_at?, checked_at, in_docker, error?}`（结果进程内缓存 1h；GitHub 失败如实入 error 字段且不缓存；未配置 CATMAIL_UPDATE_REPO → 400）|
| POST `/api/update/apply` | E8 落盘更新：下载当前平台资产 `catmail_<os>_<arch>` → **强制 SHA-256 校验（release 内 checksums.txt，缺失/不匹配一律拒绝且零落盘）** → 磁盘空间预检（≥2× 包大小）→ 同目录临时文件 + fsync + rename 原子替换 → `{ok:true, installed_version, hint:"重启 catmail 生效"}`；容器部署（/.dockerenv 或 CATMAIL_IN_DOCKER=1）与 Windows 恒拒绝（400，指引拉新镜像/手动替换）；进程内互斥防并发双更（ErrBusy）；需重启后生效。版本号单一来源 `internal/version.Version`（`-ldflags -X` 注入；Dockerfile 支持 `--build-arg VERSION=`，未注入显示 `dev`，doctor/启动日志/更新比较三处口径一致）。CI 的 tag 发布流水线（按 GOOS/GOARCH 产出 `catmail_<os>_<arch>` 与 sha256sum 格式 checksums.txt 并随 Release 上传）为 `ci.yml` 顶部 TODO——发布前需按该硬约定手动发布，否则 apply 因缺 checksums.txt 拒绝 |

`/v1/send` 鉴权：查 ApiKey（enabled）→ enforceKeyPolicy（先限速后日额度，见 §2）→ 命中后更新 LastUsedAt；失败 401。

---

## 6. 前端规格

- 纯 SPA，**双语界面（zh-CN 源语言 + en-US）**；构建 `npm run build` 产物写入 `../backend/internal/web/dist`（vite `outDir`，`emptyOutDir:true`，`base:'./'`）；路由级懒加载 + Suspense 骨架
- dev：`vite` 起 5173，proxy `/api`、`/v1` → `http://localhost:8080`
- 国际化：react-i18next（i18next v27），词典按领域拆 17 个命名空间（`src/i18n/locales/<lang>/<ns>.json`，`import.meta.glob` 自动收编）；语言解析 localStorage `catmail-lang` → 浏览器语言（zh*/en* 前缀）→ 默认 zh-CN；设置页「界面」卡下拉切换（即时全树重渲染，选项为语言自称）；`<html lang>` 与时间格式化（formatDateTime/formatRelativeTime 走 i18next.language 与 common.time.* 复数 key）随语言联动；TS key 全量校验（i18next.d.ts 以 zh-CN 词典推导，拼错 key 编译失败）；zh/en 同构校验脚本 `node scripts/check-i18n.mjs`；API 错误消息本期保持后端单语（中文），前端自有兜底文案（超时/网络错误等）已入词典
- Tailwind v4：`@import "tailwindcss"` + `@custom-variant dark (&:where(.dark, .dark *));`；主题三态「浅色/深色/跟随系统」（localStorage `catmail-theme` + matchMedia 监听 + 跨标签页同步，index.html 内联脚本防 FOUC，默认跟随系统）；`@theme` 定义语义 token
- 路由（react-router v7 BrowserRouter）：`/login`、`/`(仪表盘)、`/channels`、`/domains`(域名治理)、`/templates`(邮件模板)、`/logs`、`/events`(送达事件)、`/suppressions`(抑制列表)、`/keys`、`/users`、`/audit`(审计)、`/settings`；`<RequireAuth>` 包裹布局与全部登录后页面（`/api/auth/me` 返回 401 才跳 `/login`；任意管理 API 收到 401 时全局清缓存跳登录）
- 布局：左侧栏（猫头 CatMail logo + 导航 + 底部收起侧栏与退出登录），≥1024px 可折叠图标态（状态持久化）；顶栏含 ⌘K 命令面板、三态主题循环按钮与通知铃铛（未读数 30s 轮询，面板分页/一键全部已读）；⌘K/Ctrl+K 命令面板手写实现零依赖；<1024px 侧栏收纳为抽屉：顶栏菜单按钮开关（原生 dialog，路由切换自动关闭），抽屉内恒为展开态
- 设计系统：`components/ui/`（Button/Card/Modal/Input/Select/Table/Skeleton/EmptyState）；状态徽章语义色全局唯一映射；列表加载一律 Skeleton 骨架屏
- 状态徽章色：enabled=绿 · manual_disabled=灰 · auto_disabled=红 · cooldown=黄

| 页面 | 内容 |
|---|---|
| 登录 | 居中卡片：用户名/密码/错误提示/登录按钮；启用两步验证的账号：输入 6 位动态码或恢复码的第二步界面 |
| 仪表盘 | 4 统计卡（今日成功/今日失败/本月发送/成功率；大字号 + sparkline 迷你趋势）+ recharts 组合图（7/30/90 天切换 + 渠道筛选，同步 URL）+ 渠道用量表（含健康度列：近 7 天成功率、送达/退信/投诉率与失败 Top 原因）；统计卡与渠道行可下钻到日志页；首登三步引导清单（改默认口令 → 建渠道 → 建 Key，进度由接口数据实时推导，「不再显示」持久化） |
| 渠道管理 | 表格 + 筛选条 + 新建/编辑弹窗（平台下拉 10 项、rate_limit_per_sec 限速字段、api_secret/proxy_url/smtp_pass/webhook_secret 显式清除、编辑掩码）+ 测试发送 + 启停 + **复制** + 删除；渠道行展开/详情可见近 7 天健康度（受理 + 送达双侧）与**送达事件 webhook 地址**（每渠道独立 URL，可复制/轮换） |
| 域名治理 | 从渠道 allowed_domains 聚合「域名 × 渠道」矩阵：每域名一张汇总卡，按平台分节展示该平台要求的验证记录（SPF/DKIM/DMARC/MX/发件人验证）与配置指引（前端静态知识库，纯展示 + 文案复制，**不做 DNS 检测**）；未配置 allowed_domains 的渠道单独归入无限制列表 |
| 邮件模板 | 模板列表 + 新建/编辑弹窗（name/subject/html/text，变量占位符自动提取提示）+ **桌面/移动双预览**弹窗（样本变量 JSON 填充后 iframe sandbox 渲染：桌面自适应宽度、移动固定 375px 手机宽度并排）+ 删除 |
| 发送日志 | 筛选/排序/实时刷新(5s)/CSV 导出/分页/URL 同步/键盘导航；详情停靠面板（正文预览 iframe sandbox）；详情内「重发」按钮（30s 防重提示）；手动「清理日志」入口（三模式） |
| 送达事件 | 事件列表（事件类型/退信类型/渠道筛选 + 收件人关键词，occurred_at 倒序）+ 详情面板（归一化字段 + raw 原始报文格式化展示 + 关联发送日志跳转） |
| 抑制列表 | 列表（原因/渠道筛选 + email 搜索）+ 手动添加弹窗 + 解除（注明「解除的是当前抑制状态，不是拉黑豁免」） |
| API Keys | 列表（掩码、total_sent、daily_quota/rate_per_sec 列）+ 新建（明文一次性展示）+ 编辑（webhook_url/webhook_events/配额/限速）+ **回执签名密钥查看/复制/轮换** + 回执连通性测试 + 启停/删除（「密码重置后停用」徽标提示可手动启用） |
| 账号管理 | 用户列表 + 新建 + 重置密码 + 删除（不能删自己、至少保留一个） |
| 审计 | 操作审计列表（时间/操作者/动作徽章/目标/详情/IP；action 前缀与操作者筛选、分页） |
| 系统设置 | 轮换/尝试/冷却/阈值/日志保留天数 + 失败通知双通道（站内/Webhook/事件白名单/连通性测试/**签名密钥生成轮换**）+ 日志正文开关 + 每日自动备份开关 + 备份管理（列表/创建/下载/删除）+ 两步验证卡片（setup 二维码/enable/恢复码一次性展示/disable）+ **活跃会话卡片**（本人会话列表：IP/设备/最后活跃，踢指定会话/下线其余全部）+ 抑制自动入库开关与退信率阈值 + 界面主题 + **界面语言切换（zh-CN/en-US，即时生效 + 持久化）** + 管理员改密 |

- 数据层：@tanstack/react-query v5；`src/lib/api.ts` fetch 封装（401 跳登录）；`sonner` toast
- 验收：`npm run build`（含 `tsc -b`）零错误产出 dist；`npm run e2e`（Playwright 全量回归用例）

---

## 7. 后端补充要求

- 路由：`/api/health`、`/docs`、`/docs/openapi.yaml`、`/webhooks/mail-events/:token`（公开）+ `/v1/send`、`/emails`（Bearer）+ `/metrics`（白名单）+ `/api/*`（Cookie 会话）+ 静态：非上述前缀路径 → embed SPA（命中文件直出，index.html fallback）；`/api`、`/v1`、`/emails`、`/webhooks` 及其子路径下的未知路径 → 统一 JSON 404 错误体，不落 SPA fallback
- 启动顺序（安全加固三件套，A）：config.Load（含 CATMAIL_SECRET_KEY 校验，非法即退出）→ secretcrypt.SetGlobalKey → store.Open → `store.EnsureChannelEncryption`（一次性存量迁移：明文凭据加密回写，单事务幂等；库中有密文但未配密钥 → 拒绝启动；密钥与存量密文不匹配 → 拒绝启动）→ 其余初始化
- 优雅退出（顺序收口）：SIGINT/SIGTERM → HTTP Server Shutdown（5s 超时）→ SMTP 入口 Stop（在途会话有限等待）→ scheduler.Stop（最多 5s，等待在执行的 outbox 投递轮收尾）→ 关闭数据库（SQLite 自动 checkpoint WAL）；gin.Logger + panic Recovery（客户端统一 500 错误体）。通知/回执 webhook 投递已持久化到 outbox（webhook_deliveries 表），无进程内队列，重启后按 next_retry_at 续投（at-least-once）
- cron（robfig/cron v3，本地时区）：
  - `* * * * *` 每分钟：outbox 到期投递驱动（安全加固三件套，B）——捞取 pending 且 next_retry_at 到期的行（单轮 ≤100 条、单轮互斥、行级原子认领防并发重复投递），逐条签名投递（secret 引用现取现签，空 secret 不签名不发头）；失败按 1m→5m→30m→2h→8h→24h 封顶退避推进 next_retry_at，10 次失败或永久故障（URL 非法/secret 引用失效）转 dead
  - `0 * * * *` 每小时整点：冷却释放（逐条带全条件 UPDATE 复位，RowsAffected==1 才发 channel_recovered 通知，与并发路径天然互斥）→ 站内通知清理至最近 1000 条 → 审计日志裁剪至最近 5000 条（E4）→ outbox dead 行裁剪至最近 100 条 → 会话注册表清理（过期行 + 吊销满 7 天行）→ 每日自动备份检查（A2：开关开启且今天未备过则执行一次）
  - `30 4 * * *` 每日 04:30：按 LogRetentionDays 清理过期 SendLog（0=不清理；分批 500 条）
- `catmail doctor`（E7）：子命令自检后立即退出，8 大项只读体检——[1] 环境（Go 版本/OS/程序版本） [2] 配置（白名单/默认口令/COOKIE_SECURE/TRUSTED_PROXIES/**CATMAIL_SECRET_KEY 未配置 WARN**/**SMTP TLS 证书可读性与策略生效值**） [3] 数据目录（存在/可写/磁盘剩余，PG 跳过） [4] 数据库（Ping/quick_check/WAL 大小 >512MB WARN/逐表行数） [5] 渠道配置完整性（provider 注册与按类型必填字段，from_email 缺失 WARN；**凭据加密状态盘点**：有密文无密钥 → FAIL 提示服务将拒绝启动，配密钥有明文 → 提示待迁移） [6] 通知与回执（webhook URL 合法性） [7] 端口监听（含隐式 TLS 监听端口） [8] 汇总；任一 FAIL → 退出码 1；数据库打不开不终止，其余项照常输出
- 自动更新（E8）实现要点（internal/update）：受限 semver 比较（解析失败=不可比，如实 update_available=false 不猜测）；check 结果缓存 1h（GitHub 匿名 60 次/h/IP）；apply 实时重取 release、当前版本不低于最新即拒绝重复更新；资产 URL https 白名单；下载内存上限 256MB（Content-Length 与实际读取双检）；校验通过前零落盘
- 测试：rotator（额度/域名过滤/failover/限速跳过/抑制拦截）、适配器（httptest 假服务）、api 层集成（鉴权/限流/导出/测试发送额度/幂等/TOTP/备份/审计/白名单/metrics/模板/送达事件摄取/抑制/会话/outbox）、store（方言/备份/索引/凭据加密迁移）、smtpserver（含 STARTTLS/隐式 TLS/策略）、inbound（Svix/Mailgun 签名/归一化/退信率）、render、secretcrypt、httpsig、outbox、update、doctor、docs、notify、receipt、scheduler；测试不依赖网络与外部状态
- `go build ./... && go vet ./...` 零错误；`internal/web/dist/index.html` 占位保证 embed 编译（CI 同样处理）

## 8. 质量标准（功能完善、结构完整；全波次强制执行）

**后端**
- gofmt 全仓合规、go vet 零警告、包职责单一、关键逻辑有适度中文注释
- 输入校验完备：必填、长度上限、邮箱/URL 格式、枚举合法性、分页边界（page≥1、page_size≤100）
- 错误统一走 §2 错误体；内部错误不泄露堆栈/路径；SQLite 打开参数启用 WAL + busy_timeout
- 性能：SendLog 对 created_at/channel_id/status/api_key_id 建索引；notifications (read,created_at) 组合索引；audit_logs created_at+username 索引；idempotency_records (api_key_id,key) 组合唯一索引 + created_at；列表一律分页；聚合单条 GROUP BY（Key total_sent、statsOverview、渠道健康度两条聚合含窗口函数 Top3），回归测试锁定各端点 SQL 往返条数
- 安全：api_key/api_secret/smtp_pass/proxy_url/webhook_secret 响应一律脱敏（webhook_secret/notify_webhook_secret 管理页查看场景明文回显，/api 会话保护口径）；cookie HttpOnly+SameSite=Lax；密码 bcrypt；登录限流（用户名+IP 与用户名双维度、耗时对齐）；管理面 IP 白名单；TOTP 两步验证 + 恢复码哈希存储；审计日志不含敏感值；请求体大小上限；headers 拒 CRLF；CSV 导出公式注入防护；备份文件名白名单防路径穿越；自动更新强制 SHA-256；测试发送/日志重发防滥用口径明确；**渠道凭据 AES-256-GCM 静态加密；出站 webhook HMAC-SHA256 签名；SMTP TLS 入口；会话 v3 注册表**
- 关键路径单测：rotator、适配器、auth、发送入口、幂等、SMTP 入口、更新校验；测试不依赖网络与外部状态

**已落地加固项（2026-11-11 P0/P1/P2 优化轮）**
- P0/P1 并发与额度：原子额度预留/释放（含跨周期锚点配对与 panic 配对兜底）；连续失败原子递增；渠道/Key 编辑只写可写集；逐候选回退默认发件人
- P1 语义修复：Brevo not_enough_credits 冷却；Mailjet 嵌套错误深度提取（5 层）；SMTP PLAIN 成功后失败 bug；重置/改密事务化；测试发送 429 统一错误体；statsOverview 凭据列裁剪
- P2 安全加固：登录限流；会话纪元（v2 token，旧 token 全失效）；Cookie Secure；请求体上限；CRLF 防护；CSV 注入防护；唯一约束兜底 400；删账号事务复核

**2026-11-12 优化轮新增（本文档 v1.1 主体）**
- 功能：通知双通道（5 事件）、调用方回执（D1）、幂等键（E2）、Key 配额限速（B5）、渠道级出站限速（A1）、渠道健康度（A3）、渠道复制、日志重发、备份（A2）、日志清理三模式、日志正文开关（A4）、审计（E4）、管理 IP 白名单（E5）、/metrics（B4）、/docs（B2）、SMTP 入口（E1）、doctor（E7）、自动更新（E8）、TOTP（C4）、PostgreSQL（D2）、新平台 elasticemail/mailtrap/aliyun
- 性能（L5 实测，2026-11-12，测试环境：macOS arm64 本机 SQLite，SendLog 预置 10 万行，数据仅为该环境参考值）：日志列表/详情/导出/统计/渠道列表/Key 列表/通知列表/审计列表/仪表盘共 9 个接口响应 0.8–160ms（首字节，含 1 万行 CSV 导出上限场景）；24h 压测无内存泄漏；首屏 JS 经路由级代码分割较上轮 -54%
- 前端：审计页、通知铃铛、TOTP 设置卡与登录第二步、渠道健康度展示、备份管理 UI、Key 配额/回执编辑

**2026-11-14 两轮交付新增（本文档 v1.2 主体，两波均独立验收通过）**
- Wave 1（送达链路 + SMTP TLS + 邮件模板）：送达事件摄取（公开端点 `/webhooks/mail-events/:token`，每渠道 256bit token + Resend Svix/Mailgun 真实签名校验 + 时间戳容差与进程内去重防重放）；渠道健康度送达侧升级（近 7 天送达率/退信率/投诉率 + `bounce_rate_exceeded` 告警，1h/渠道去重）；抑制列表（hard bounce/complaint 自动入库 + 三发送入口统一拦截 `recipient_suppressed`，查询 fail-open）；SMTP TLS 入口（STARTTLS RFC 3207 + 隐式 TLS 监听 + optional/require 策略，doctor 4 项新检查）；邮件模板（templates 管理 + `template_id`/`variables` 渲染，HTML 语境转义无逃生门，`body_conflict`/`unknown_template`/`missing_variables` 全链路可见）；域名治理面板（/domains 域名×渠道矩阵 + 平台验证记录指引，静态知识库）
- Wave 2（安全加固三件套）：渠道凭据 AES-256-GCM 静态加密（`CATMAIL_SECRET_KEY`，`enc:v1:` 前缀，启动期一次性幂等迁移 + 拒绝带病启动）；Webhook 签名 + outbox（`X-CatMail-Signature: t=<unix秒>,v1=<hex>` 契约 ±5min 容差；通知与回执统一经 webhook_deliveries 落库投递，指数退避 1m→24h 封顶、10 次转 dead、每分钟投递驱动）；会话 v3 + 服务端注册表（GET/DELETE /api/sessions、revoke-others；管理员重置密码单事务联动吊销会话 + 停用名下全部 API Key）

**交付物**
- README / SPEC / PLAN / CHANGELOG 四文档同步；smoke.sh 集成冒烟（覆盖送达链路/SMTP TLS/邮件模板/安全加固全场景）；frontend e2e（Playwright）；CI（.github/workflows/ci.yml：gofmt/vet/test -race + 前端构建）
