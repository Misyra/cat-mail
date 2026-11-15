# Snowmail 可行性与风险分析报告

> 调研日期：2026-11-11。范围：API 兼容性、域名验证工程、轮换与可靠性设计、ToS/合规风险、送达率。
> 各平台免费额度数字由其他代理负责，本报告不重复。
> 所有条款引用均来自 2026-11-11 实际抓取的官方页面（来源链接附后），个别不可达来源（Google 域名）已注明。

---

## 0. 结论（TL;DR）

**技术上完全可行；合规与商业模式上属于灰色地带 → 总体判定：有条件可行。**

- 四家平台的发送 API 形态差异明确、可枚举、可封装，统一 Schema + 适配器是成熟模式（one-api 已验证同类形态）。
- 一个自有域名同时接入多个发送平台**在工程上可行且是业界常规做法**（DKIM selector 共存 + 每平台独立 envelope 子域），但需要正确的 DNS 架构，否则 SPF PermError 会让全部邮件被拒。
- 主要风险不在技术，而在于：免费额度的性质是**获客/试用导向**，多家条款已明确针对"绕过限制/多免费账户"设防（Mailgun 明文限制免费账户数，Resend 明文禁止多账户绕配额，Twilio 禁止 bypass service limitations）。**每家注册一个账号、正常使用其免费层**是安全的；**批量注册/宣传"叠加白嫖"**则踩线。
- 开源发布需将项目定位为"多供应商邮件网关 / failover 路由器"而非"免费额度聚合器"。

**主要风险前三名：**
1. **平台合规风险**——免费额度被视作试用配额，条款普遍禁止绕过；开源传播会放大风险。
2. **送达率风险**——免费共享 IP 池 + 多平台基础设施跳变，信誉碎片化，Outlook 尤其敏感。
3. **工程可靠性风险**——四家异步语义/错误码/附件编码不一致，failover 判定困难，重复发送风险真实存在。

---

## 1. API 兼容性：四平台发送 API 对比

### 1.1 总览

| 维度 | Resend | SendGrid v3 | Mailgun | Brevo |
|---|---|---|---|---|
| 端点 | `POST /emails` | `POST /v3/mail/send` | `POST /v3/{domain}/messages` | `POST /v3/smtp/email` |
| 认证 | `Authorization: Bearer` | `Authorization: Bearer` | HTTP Basic（api:key） | `api-key` 自定义头 |
| 请求体 | JSON | JSON | **multipart/form-data** | JSON |
| 发件人 | `from` 字符串 | `from` 对象 `{email,name}` | `from` 字符串 | `sender` 对象 `{email,name,id}` |
| 收件人 | `to/cc/bcc` 字符串或数组（≤50） | `personalizations[].to/cc/bcc` 对象数组（≤1000） | `to/cc/bcc` 字符串数组（RFC 5322，逗号分隔可批量） | `to/cc/bcc` 对象数组（name ≤70 字符） |
| 正文 | `html` + `text`（缺省自动转 text） | `content: [{type,value}]` 数组 | `text` + `html` + `amp-html` | `htmlContent` + `textContent` |
| 附件 | `attachments: [{content(base64/buffer), filename, path, content_type}]` | `attachments: [{content(base64), filename, type, disposition, content_id}]` | `attachment`/`inline` **二进制 multipart 字段**（可重复出现） | `attachment: [{url 或 content(base64), name}]` |
| 自定义头 | `headers` 对象 | `headers` 对象（有受保护头清单） | `h:X-Custom` 前缀字段 | `headers` 对象（强制 Title-Case） |
| 元数据/追踪 | `tags: [{name,value}]` | `custom_args` + `categories` | `v:*` 变量、`o:tag` | `tags: [string]` |
| 定时发送 | `scheduled_at`（自然语言/ISO 8601） | `send_at`（Unix 秒，≤72h） | `o:deliverytime`（RFC 2822，3–7 天） | `scheduledAt`（UTC，仅 ≤5 分钟延迟） |
| 幂等支持 | 原生 `Idempotency-Key`（24h） | 无原生（可用 batch_id + custom_args） | 无 | 原生 `Idempotency-Key`（返回 duplicate_request） |
| 成功响应 | `200 {id}` | **`202 Accepted`**（异步，无 body） | `200`（消息 id 表单返回） | `201 {messageId, messageIds}` |
| 特色 | `react`、`topic_id` | `personalizations` 批量、`ip_pool_name`、沙箱模式 | `o:testmode`、`o:dkim`、`o:secondary-dkim`、`recipient-variables` | `templateId`、`messageVersions`（≤2000 收件人/请求） |

已验证的细节备注：

- Resend：附件计入后总大小 ≤40MB（base64 后）；`to` ≤50；`text` 缺省时 HTML 自动转文本（传空字符串可关闭）。来源：[send-email API](https://resend.com/docs/api-reference/emails/send-email)。
- SendGrid：`personalizations` ≤1000、cc/bcc 各 ≤1000；受保护头（不可覆盖）包括 `dkim-signature`、`Content-Type`、`To/From/Subject/Reply-To` 等；`413` 响应存在。来源：[Mail Send API](https://www.twilio.com/docs/sendgrid/api-reference/mail-send/mail-send)。
- Mailgun：`o:`/`h:`/`v:`/`t:` 前缀字段合计 **≤16KB**；`o:testmode` 消息照常计费；支持 `o:secondary-dkim`（`domain/selector` 格式）做多签名。来源：[Send an email](https://documentation.mailgun.com/docs/mailgun/api-reference/send/mailgun/messages/post-v3--domain-name--messages)。
- Brevo：`messageVersions` 单请求总收件人 ≤2000、每版本 ≤99、单版本 `params` ≤100KB；错误码枚举里直接有 `not_enough_credits`、`duplicate_request`、**`DMARC policy requires domain authentication`**、`DNS records not properly configured` ——这些对路由器判定很有用。来源：[Send a transactional email](https://developers.brevo.com/reference/send-transac-email)。

### 1.2 关键差异点（适配器必须处理的）

1. **请求体格式分裂**：Mailgun 是 multipart/form-data（附件是二进制 part），其余三家是 JSON（附件是 base64 字符串）。统一层收到 base64 后，Mailgun 适配器需解码成二进制再挂 multipart part。
2. **收件人表达**：统一 Schema 建议用 `[{email, name}]` 对象数组作为规范形态：
   - → Resend/Mailgun：序列化为 `"Name <a@b.c>"` 字符串；
   - → SendGrid/Brevo：直接对象数组。
3. **附件编码**：统一 `content_base64` + `filename` + `content_type` + 可选 `content_id`（内联）。Resend/SendGrid/Brevo 近乎同构，可共享一条转换链；Mailgun 单独处理。
4. **同步/异步语义不一致**：SendGrid 返回 202 即接受（失败靠后续 `dropped` 事件告知）；Mailgun/Brevo/Resend 会在事件流里再发 `accepted`/`request`/`sent`。**"平台已接受"≠"已送达"**，路由器的状态机必须区分 `accepted` 与 `delivered`。
5. **错误码不可比**：429/限速/额度不足的返回结构各家不同（Brevo 有 `not_enough_credits` 错误码可直接映射；Resend 429；SendGrid 429 + Retry-After；Mailgun 429/600s 限速）。适配器应统一翻译为内部错误枚举：`RATE_LIMITED / QUOTA_EXCEEDED / AUTH_FAILED / INVALID_REQUEST / PROVIDER_DOWN`。

### 1.3 统一发送 Schema 建议

```jsonc
// POST /v1/send —— Snowmail 统一接口
{
  "from":    { "email": "noreply@example.com", "name": "Snowmail" },
  "to":      [{ "email": "user@host.com", "name": "U" }],   // 规范形态：对象数组
  "cc":      [], "bcc": [], "reply_to": [],
  "subject": "...",
  "html":    "...", "text": "...",                           // text 缺省时适配器自行决定
  "attachments": [{ "filename": "x.pdf", "content_base64": "...",
                    "content_type": "application/pdf",
                    "content_id": "img1" }],                 // content_id => inline
  "headers": { "X-Trace-Id": "..." },
  "tags":    { "stream": "transactional", "user_id": "42" }, // 统一为 k/v；映射到
                                                             // Resend tags / SG custom_args /
                                                             // Mailgun v: / Brevo tags
  "meta": {
    "idempotency_key": "uuid",       // 透传 Resend/Brevo 原生头；其余靠库内去重
    "scheduled_at": "ISO8601|null",  // SendGrid/Mailgun/Brevo 精度差异须在文档中声明
    "preferred_provider": "resend"   // 路由提示
  }
}
```

约束规则：
- 字段能力有交集才进统一 Schema；只有单家有的能力（如 `ip_pool_name`、`o:testmode`）放 `provider_options` 透传对象，各适配器自行取舍。
- 文档必须列出能力矩阵（如 Brevo 定时只有 5 分钟窗口、Mailgun o:/h:/v: 16KB 上限），对超出能力的请求在网关层预校验并返回明确错误，而不是让适配器静默降级。

### 1.4 哪些平台可共享适配逻辑

- **第一梯队（高度相似）**：Resend ↔ Brevo。两者都是"扁平 JSON + 对象数组收件人 + base64 附件 + 原生幂等键"，一个中间表示（IR）可以同时喂两家，预计 70% 代码共享。
- **第二梯队**：SendGrid。JSON 形态接近，但 `personalizations` 包装层、`content[]`、受保护头清单需要独立转换逻辑；批量发送反而它最强（1000 收件人/请求）。
- **第三梯队**：Mailgun。multipart + 前缀字段体系是独立物种，但其字段语义（from/subject/text/html/attachment）与其他家一一对应，IR→form 的映射仍可复用统一 Schema 的解析层，只是序列化器不同。
- 结论：**一个 IR + 3 个序列化器**（JSON-flat、JSON-personalizations、multipart-form）即可覆盖四家；签名校验（webhook 侧）四个各写各的（见 §4.3）。

---

## 2. 域名验证工程：一个域名接多家平台

### 2.1 结论先行

**可行，且是行业标准做法。** 关键在于三点：DKIM selector 天然共存、envelope-from（Return-Path）按平台隔离到子域、DMARC 依赖 DKIM 对齐而非 SPF 对齐。唯一的硬性工程约束是根域 SPF 的 10 次 DNS 查询上限。

### 2.2 SPF：10 次查询上限

依据 RFC 7208（经 [dmarcian 语法表](https://dmarcian.com/spf-syntax-table/) 整理）：

- 计入查询的机制：`include:`、`a`、`mx`、`ptr`、`exists:`、`redirect=`；`ip4:`/`ip6:` 不计。
- 超过 10 次 → **PermError**；dmarcian 明确指出 "Some mail receivers will reject based on a PermError"——即**不只冒名的邮件被拒，你的全部邮件都可能被拒**。
- 若把四家 include 全堆在根域：`include:amazonses.com`（Resend 底层是 SES）+ `include:sendgrid.net` + `include:mailgun.org` + `include:spf.sendinblue.com`（Brevo）+ 自身 `a`/`mx`，加上各 include 内部的嵌套查询（sendgrid.net、mailgun.org、sendinblue.com 各自还有 2–3 层嵌套 include），总计通常落在 8–12 次之间，**处于临界或超限**，不可靠。

**解法（dmarcian 原话："The single most practical solution"）**：把 SPF 按平台拆到各自的子域。这恰好是各家默认/推荐架构：

| 平台 | 验证记录形态（已从官方文档核实） | SPF 落点 |
|---|---|---|
| Resend | 子域 `send.example.com`：MX → `feedback-smtp.{region}.amazonses.com`，TXT `v=spf1 include:amazonses.com ~all`；DKIM：`resend._domainkey` TXT | **子域**，根域零污染。来源：[Cloudflare 指南](https://resend.com/docs/guides/dns/cloudflare.md) |
| SendGrid（Automated Security ON，默认） | `em[XXXX].example.com` CNAME → `[uNNNN].[wlNNN].sendgrid.net`（SPF 走 CNAME 继承）；DKIM：`s1._domainkey` / `s2._domainkey` CNAME | **子域**。来源：[Domain Authentication](https://www.twilio.com/docs/sendgrid/ui/account-and-settings/how-to-set-up-domain-authentication) |
| SendGrid（AS OFF） | `em[XXXX].example.com` TXT `v=spf1 include:sendgrid.net ~all` + MX `mx.sendgrid.net`；DKIM：`m1._domainkey` TXT | 子域 |
| Mailgun | 官方建议用 `mg.example.com` 之类子域收发；DKIM 在该子域下（如 `k1._domainkey.mg...`）；支持 `o:secondary-dkim` 多签名 | 子域。来源：[DNS 最佳实践](https://documentation.mailgun.com/docs/mailgun/email-best-practices/dns) |
| Brevo | DKIM：`mail._domainkey` TXT（`k=rsa;p=...`，经 API 实测返回）；所有权验证 `brevo-code` TXT；另要求 DMARC 记录 | **Brevo 的 SPF include 通常加在根域**（`spf.sendinblue.com`），是四家中唯一可能占根域查询名额的 |

**Snowmail 的工程建议**：按"每平台一个子域"接入（`resend.example.com` / `sg.example.com` / `mg.example.com` / `brevo.example.com`，或复用各家默认子域），根域 SPF 保持干净（只放自有服务器 + Brevo 一家，必要时把 Brevo 也压到子域）。子域各有独立 10 次配额，"effectively boundless"（dmarcian 语）。

### 2.3 DKIM：多 selector 共存

DKIM 的 selector 机制天然支持一个域名上多个签名方：每家用自己的 selector，互不冲突，均为已核实事实：

- Resend：`resend._domainkey`（TXT）
- SendGrid：`s1._domainkey` / `s2._domainkey`（CNAME），或手动 `m1._domainkey`，支持自定义 selector
- Mailgun：自有 selector（如 `k1`/`smtp`），且支持单消息开关（`o:dkim`）与第二签名（`o:secondary-dkim`）
- Brevo：`mail._domainkey`（新版 API 返回），旧文档亦有 `brevo1key._domainkey` 形态

**DNS 层面零冲突，无需协调。** 这就是为什么"一域名多平台"在业界普遍存在。

### 2.4 DMARC：一条记录管全家

- DMARC 记录是域名级的：`_dmarc.example.com TXT "v=DMARC1; p=..."`，与接入几家平台无关。
- **DMARC 判定靠 DKIM 或 SPF 任一对齐即可**。各平台完成 domain authentication 后都会用你的域名做 DKIM 签名（d= your domain）→ DKIM 对齐成立 → DMARC 通过。SPF 对齐（envelope-from 必须与 From 同域）在子域架构下通常不成立，但无关紧要。
- 策略推进建议：`p=none`（观察 ≥2 周，收集 rua 报告）→ `p=quarantine` → `p=reject`。Brevo 甚至把 "DMARC policy requires domain authentication" 做成了发送错误码——域名认证是其发信的硬前提。
- 注意：`p=reject` 之后，任何一家配置不良（如 DKIM 没配全）会导致该平台邮件全灭。多平台切换上线前必须逐家验过 DMARC 报告。

### 2.5 多平台共用域名会互相影响送达率吗？

会，但方式是**域名信誉共享**而非 DNS 层面：

- Mailgun 官方明确："Your email reputation is not only tied to your IP, but your domain name as well"（[DNS 最佳实践](https://documentation.mailgun.com/docs/mailgun/email-best-practices/dns)）。多个平台用同一域名 = 多个平台共享同一段域名信誉，一家出事（高投诉率）全站受损。
- Mailgun/Resend/SendGrid 三家文档均建议**按邮件流拆子域**（transactional / marketing 分离）——同样的思路可以推广为**按平台拆子域**：信誉隔离 + 事件归因清晰 + SPF 配额隔离，一举三得。
- 其他实际影响：DKIM 签名者不同、发送 IP 池不同、跳变的基础设施（Received 链路）对 Outlook 这类"finicky"（Mailgun 原话）的接收方略有负面影响；正文指纹一致但来源频繁切换的模式理论上可被风控关注，低量事务邮件场景影响可忽略。

---

## 3. 轮换与可靠性设计

### 3.1 额度跟踪数据模型

```sql
providers(
  id, adapter,                     -- resend | sendgrid | mailgun | brevo
  status,                          -- active | cooling | suspended | disabled
  monthly_quota, monthly_used,
  quota_reset_anchor,              -- 每家重置日不同：Brevo 按月度周年日重置（ToS 明文：
                                   --   "Unused quotas are reset (cancelled) at each monthly
                                   --    anniversary"），Resend 等按自然月/注册日
  daily_limit, daily_used,         -- SendGrid 免费层日额（100/日）→ 日计数器必须独立
  hourly_rate,                     -- 适配器本地限速，防触发 429
  cooldown_until,                  -- 连续失败/429 后的熔断冷却
  health_weight                    -- 滑动窗口成功率，路由打分用
)

usage_counters(provider_id, period_key, kind, used)   -- (provider, '2026-11', 'send') 唯一
messages(id, idempotency_key UNIQUE, provider_id, provider_msg_id,
         state,        -- queued → dispatched → accepted → delivered / deferred
                       --        → soft_bounced / hard_bounced / failed
         attempts, created_at)
events(message_id, provider, type, raw_json, occurred_at)  -- webhook 落库 + 归一化视图
```

要点：

- **配额以"平台接受"为准记账**（accepted 即扣减），不是以送达为准；Brevo 的 ToS 明文规定配额按"发送行为"计（"regardless of the content or the actual delivery"），SendGrid 202 同理。
- **日限速与月额度是两个独立维度**：SendGrid 免费层的硬约束是"每天 N 封"而非每月总量；Resend/Brevo 是月额度。路由器应支持 `monthly_quota` 与 `daily_limit` 双闸门，任一触顶即摘除该平台。
- **重置时间不可统一假设**：Brevo 按"月度周年日"重置、Resend 等另有锚点，数据模型必须支持每家独立 `quota_reset_anchor`，并提供"手动同步真实额度"接口（各家有账户/额度查询 API 时优先拉真值，本地计数只做缓存与预判）。
- 本地计数永远可能偏离真值（webhook 丢失、重试），要预留对账任务：定期用平台统计 API（如 Brevo `get-aggregated-smtp-report`、Mailgun Metrics）校准。

### 3.2 Failover 与重试策略

分层处理，**核心原则：只有在平台"接受消息之前"才允许跨平台 failover；接受之后绝不换平台重发。**

| 故障类型 | 判定依据 | 动作 |
|---|---|---|
| 限速/额度 | 429、Brevo `not_enough_credits`、本地计数触顶 | 摘除该平台（冷却 60–300s），**立即**换下一家重发（消息尚未被任何平台接受，安全） |
| 平台故障 | 5xx、超时、TLS 失败 | 指数退避重试同平台 1–2 次，再 failover |
| 认证/配置错误 | 401/403、`DNS records not properly configured` | **不 failover**（换平台同样会失败的概率低，且多为人工修复问题），告警并摘除 |
| 请求非法 | 400、附件超限、收件人格式 | **不 failover**（垃圾进垃圾出），直接返回客户端 |
| 接受后被丢弃 | SendGrid `dropped`、Brevo `blocked/invalid_email`、Resend `email.failed` | 记录 + 归因；**默认不自动重发**（易重复投递），关键邮件可走人工/带标记的补偿队列 |

幂等与重复投递防护：

- Resend、Brevo 有原生 Idempotency-Key → 透传；SendGrid 用 `custom_args.snowmail_id` + `batch_id`；Mailgun 用 `v:snowmail_id` 自定义变量。**统一幂等键在网关层生成，随消息走遍所有平台。**
- 重试链路所有入口（客户端重试、适配器重试、failover 重试）必须以 `idempotency_key` 做库内去重。
- 附件/正文需在"queued"状态持久化完整消息体（Mailgun 适配器在重试时要从 base64 重构建 multipart）。

其他设计要点：

- **事务邮件保持平台粘性**（affinity）：同一业务流尽量固定一家，减少基础设施跳变，利于域名/子域信誉稳定（呼应 §2.5、§6）；failover 只在必要时介入，恢复后回归首选。
- 熔断：滑动窗口错误率 > 阈值（如 20%/5 分钟）→ cooling；人工或自动探活恢复。

### 3.3 Webhook 回执归一化

四家事件体系差异较大，但可归一到统一枚举：

**统一事件枚举**：`accepted / delivered / deferred / soft_bounce / hard_bounce / dropped / blocked / spam / unsubscribed / opened / clicked / failed`

| 统一事件 | Resend | SendGrid | Mailgun | Brevo |
|---|---|---|---|---|
| accepted | `email.sent` | `processed` | `accepted` | `request` |
| delivered | `email.delivered` | `delivered` | `delivered` | `delivered` |
| deferred | `email.delivery_delayed` | `deferred` | `temporary_fail` | `deferred` |
| soft bounce | （bounce.type=Temporary） | `bounce`(type=blocked) / `deferred` | `temporary_fail` | `soft_bounce` |
| hard bounce | `email.bounced`(Permanent) | `bounce`(type=bounce) | `permanent_fail` | `hard_bounce` |
| dropped | `email.failed` / `email.suppressed` | `dropped` | `permanent_fail` | `invalid_email` / `blocked` |
| spam 投诉 | `email.complained` | `spamreport` | `complained` | `spam` |
| unsubscribed | —（营销广播侧） | `unsubscribe`/`group_unsubscribe` | `unsubscribed` | `unsubscribed` |
| opened / clicked | `email.opened` / `email.clicked` | `open` / `click` | `opened` / `clicked` | `opened` / `clicked`（+ `proxy_open` 变体） |

已核实的各家 payload 形态：

- **Resend**：`{ type, created_at, data{ email_id, message_id, from, to[], subject, tags{}, bounce{ type, subType, diagnosticCode[], message } } }`。签名验证用 **Svix**（`svix-id` / `svix-timestamp` / `svix-signature` 头，官方库 `svix.verify`），重试保留同一 `svix-id` 可去重。来源：[Event Types](https://resend.com/docs/webhooks/event-types.md)、[email.bounced](https://resend.com/docs/webhooks/emails/bounced.md)。
- **SendGrid**：**JSON 数组**（一次 POST 多条事件）；字段 `email, timestamp, smtp-id, sg_message_id, sg_event_id, event, reason, status, bounce_classification, attempt, response, url, useragent`；安全机制为 Signed Event Webhook（ECDSA 公钥验签）或 OAuth 2.0。来源：[Event Webhook Reference](https://www.twilio.com/docs/sendgrid/for-developers/tracking-events/event)。
- **Mailgun**：按域名按事件类型配置 webhook（`accepted/delivered/opened/clicked/unsubscribed/complained/temporary_fail/permanent_fail`）；payload 带 `signature{timestamp, token, signature}`，验签为 HMAC-SHA256(timestamp+token, signing_key)。来源：[Get domain webhooks](https://documentation.mailgun.com/docs/mailgun/api-reference/send/mailgun/domain-webhooks/get-v3-domains--domain--webhooks)。
- **Brevo**：单一 JSON 对象，`event` 字段 + `email` + `message-id` + `X-Mailin-custom`（发送时自定义头会**原样回传**——这是把统一 trace id 带回来的官方通道）；时间字段三套：`ts_event`（秒 UTC）、`ts_epoch`（毫秒 UTC）、`date`（**CET/CEST 时区**，坑点）。来源：[Transactional webhooks](https://developers.brevo.com/docs/transactional-webhooks.md)。

归一化实现建议：

1. 每平台一个 webhook 入口（路径含 provider 名），验签后先落 raw 库再异步归一化——归一化逻辑可随版本演进重放。
2. **关联主键用统一幂等键**（Brevo 从 `X-Mailin-custom` 取、SendGrid 从 `custom_args` 取、Mailgun 从 `v:` 变量取、Resend 从 `tags` 取），provider 自身的 message id 作为辅助索引。注意 SendGrid 异步 bounce 可能不带 sg_message_id（官方注明 delayed/asynchronous bounce 场景），幂等键是唯一可靠关联。
3. 时间统一 UTC 存储；Brevo 的 `date` 字段丢弃不用。

---

## 4. 风险与合规（如实调查，不回避）

### 4.1 各平台条款中的"红线"原文

以下引文全部来自 2026-11-11 实际抓取的官方文本：

**Resend**（[AUP](https://resend.com/legal/acceptable-use)，2026-10-13 版）：

> "Users are expressly forbidden from creating or using an account or multiple accounts with the aim of circumventing any quotas or limits imposed by our service."

> "Your complaint rate must be lower than 0.08%. Your bounce rate must be lower than 4%. If complaint or bounce rates go above these thresholds, your account may be shut down without warning."

注意 AUP 还把 **cold outreach（陌生开发信）本身列为 Prohibited Sending Purposes**——这在四家中最严格。

Resend ToS（[Free Tier 条款](https://resend.com/legal/terms-of-service)）：

> "At any time and without notice, ... reserves the right to (i) modify the features, limits, or availability of the Free Tier, (ii) cancel or discontinue the Free Tier, or (iii) convert Free Tier accounts to paid Subscriptions if usage exceeds Free Tier limits."

**Twilio/SendGrid**（[Twilio AUP](https://www.twilio.com/en-us/legal/aup)，"No Service Integrity Violations"节）：

> "Bypassing Service Limitations. Attempting to bypass, exploit, defeat, or disable limitations or restrictions placed on the Services."

> "Falsification of Identity or Origin. Creating a false identity or any attempt to mislead others as to the identity of the sender or the origin of any data or communications."

**Mailgun/Sinch**（[AUP](https://www.mailgun.com/legal/aup/)）——条款最直白：

> "You may not sign up or use more than one of our free accounts for each of our respective email platforms regardless of your corporate structure, or actual or intended use case without our prior written consent."

> "…to assist another person in an activity that would violate this AUP if performed by you is also a violation."

发送质量阈值：Bounces ≤5%、Spam Complaints ≤0.08%、Unsubscribes ≤1.4%、Blocks <20%。

**Brevo**（[ToS](https://www.brevo.com/legal/termsofuse/)）：

> "You are only allowed to create and use one account."（§3.1）

> "This Plan is limited to one (1) User only."（Free Plan，附录 1）

> Brevo "may suspend, limit, or terminate the free Services for any reason at any time without notice."（§15.3）；违约时 "with immediate effect without prior notice" 且不退已付费用（§15.2）。

配额规则：按发送行为计费（无论实际送达）、触顶即停发、未用完额度按"月度周年日"清零（附录 1）。另有 6 个月无活动可删号（§3.4）。Brevo AUP（[antispampolicy](https://www.brevo.com/legal/antispampolicy/)）禁止抓取/购买名单，部分行业（crypto、dating、loan 等）需先经客服审核。

### 4.2 这些条款对 Snowmail 意味着什么

1. **"每家注册一个账号"是安全的。** 所有红线条款针对的都是"在同一平台创建/使用多个账户来绕过该平台的配额"。Snowmail 的用法（Resend 一个号、Brevo 一个号、正常使用各家免费层）不在任何一条的字面范围内——这是各自平台欢迎的标准开发者行为。
2. **"聚合以最大化免费额度"是灰色地带。** Twilio 的 "bypass / defeat limitations" 与 Resend 的 "circumventing any quotas" 用词宽泛；如果项目的对外叙事是"帮你把所有免费额度叠加起来白嫖"，平台风控/法务按最宽泛解读的风险真实存在，尤其 Mailgun 还有 "assist another person" 条款——**开源项目教唆/工具化批量注册多账号会直接踩中它**。
3. **免费额度的真实意图是获客试用**，四家 ToS 都保留了随时修改/取消免费层、强制转付费的权利（Resend ToS 明文；Brevo §15.3）。**额度聚合的价值建立在随时可能被单方面改写的基础之上**——架构上必须假设"某天某平台免费层消失/收紧"是常态事件，路由器本身就是对冲这个风险的正确形态。
4. **常见封号原因**（综合条款与公开社区反馈）：高退信率/高投诉率（阈值见上）；发送未经同意的列表（购买/抓取名单，各家明文禁止）；新号突击大批量发送触发人工审核；行业类别受限（crypto/loan/dating/cold outreach）；注册信息不实（Mailgun 明文禁 false name/身份遮蔽）；多账号指纹关联。**Snowmail 项目本身不增加前三类风险，但如果它的存在促使用户批量造号，会显著增加后两类。**

### 4.3 个人/小团队自用 vs 开源发布

| 场景 | 风险评估 | 建议 |
|---|---|---|
| **个人/小团队自用** | **低**。每家一账号、正常免费层用量、真实发信场景——这是所有平台的目标用户画像。自动化轮换只是自己账号之间的调度，不触碰任何条款字面红线。风险集中在送达率与额度对账，而非合规。 | 正常用。保持高质量发信（双退信/投诉阈值），定期对账。 |
| **开源发布** | **中**。工具本身中立（类似 one-api 之于 LLM API），但传播后会放大两类风险：(a) Mailgun 的 "assist another person" 条款下，若社区用法演变为批量注册免费账号，项目可能被平台点名；(b) README 若主打"叠加免费额度"，等于自我定性为规避工具。 | ① 定位为 **"multi-provider email gateway / failover router"**，叙事强调可靠性、供应商冗余、统一 API；② 不提供、不文档化、不优化任何批量注册/多账号路径；③ 内置发信质量护栏（退信/投诉计数、阈值自动熔断）——这既是送达率工具也是合规保护；④ 在 README 附各平台 AUP 链接与"合规使用须知"。one-api 的长期存活证明中立网关定位是可以被容忍的。 |

通用合规提醒（对任何场景都适用）：CAN-SPAM/GDPR/CASL 的同意与退订义务始终由最终发信人承担；向 Gmail/Yahoo 大量发信（>5,000 封/日）须满足其 2024 年起执行的大发件人要求——SPF+DKIM+DMARC 齐备、营销邮件支持 RFC 8058 一键退订、投诉率 <0.3%（来源：Google/Yahoo 官方 Sender Guidelines，Google 域名在本调研网络不可达，数字为公开通行的官方要求）。

---

## 5. 送达率分析

### 5.1 共享 IP 池 vs 自有域名信誉

- 免费层全部使用**共享 IP 池**：Resend 底层为 AWS SES（官方 DNS 记录 `include:amazonses.com` 已证实）、SendGrid/Brevo/Mailgun 亦为共享池。IP 信誉与邻居租户共享，其他用户的劣质流量会外溢到你头上。
- **域名信誉是你唯一可控的部分**：Mailgun 官方表述"reputation is not only tied to your IP, but your domain name as well"。完成 domain authentication（DKIM 签自己域名）后，域名信誉跨平台可携带——这是多平台架构下最重要的信誉资产。
- 容量分界（Mailgun 官方）：日发送 <5,000 封共享池够用；>50,000 封/周才需要独享 IP。**Snowmail 的目标场景（免费额度轮换，必然是低量）恰好完全落在共享池合理区间**，共享池不是这个项目的劣势。

### 5.2 多平台轮换对送达率的特殊影响

- **信誉碎片化**：同一收件人时而从 SES 时而从 Brevo 收到你的信，接收方对每条基础链路分别累积信任，冷启动期每次切换都像新发件人。对策：事务邮件保持平台粘性（§3.2），按平台划分子域（§2.5）让信誉各自沉淀、互不污染。
- **Outlook/Hotmail 是最挑剔的接收方**（Mailgun 官方点名 "especially finicky"，From 域与发送域不一致就倾向进垃圾箱）；新域名 + 共享池 + 基础设施跳变的组合对 Outlook 最危险，Gmail 相对宽容。测试送达率时优先盯 Outlook。

### 5.3 新域名预热建议

- **事务邮件不需要严格的日计划预热**（SendGrid 官方："If you send transactional emails, don't focus on a strict IP warmup schedule"），但需要**平稳的量级爬坡**：Mailgun 官方警告 "Large volume spikes can be mistaken for spam"。
- 若发营销/批量邮件，参考 SendGrid 自动预热表：**Day 0 每小时 20 封起，约每日 ×1.4 递增**（Day 10 = 579 封/小时，持续约 30–45 天爬满）（[Automated IP warmup](https://www.twilio.com/docs/sendgrid/ui/sending-email/warming-up-an-ip-address)）。
- Snowmail 特有建议：**不要在轮换逻辑里把"换平台"当作热身手段**——每个平台×子域组合都要独立经历低量起步；新接入一家时路由器应设"渐入权重"（首周限制其承接比例），而不是立即全量分流。
- 新域名基础设施清单：WHOIS 公开（Mailgun 警告代理隐私注册可能被视为 spam 信号）、MX 有效（少数接收方校验）、SPF/DKIM/DMARC 三件套齐备、发送量稳定。

### 5.4 免费额度平台的送达率口碑

- 低量、已验证域名、纯事务流：四家免费层送达率都足够好，这也是它们作为获客手段的卖点；口碑差异主要体现在**共享池邻居质量**与**对 Outlook 的表现**上，且随时间波动，不宜写入架构决策。
- 真正决定成败的是 §2 的认证配置与 §4.2 的高质量发信纪律（退信 ≤4–5%、投诉 ≤0.08%），而不是选哪家。

---

## 6. 总体结论

| 维度 | 判定 | 依据 |
|---|---|---|
| 统一 API + 适配器 | **可行，工程量中等** | 差异可枚举；Resend↔Brevo 高度同构；一个 IR + 3 个序列化器覆盖四家 |
| 一域名多平台 | **可行，需正确 DNS 架构** | DKIM selector 共存 + 每平台子域隔离 SPF/信誉；根域 SPF 查询预算是唯一硬约束 |
| 额度轮换 + failover | **可行，需精细状态机** | accepted/delivered 两阶段、幂等键贯穿、接收后禁跨平台重发 |
| Webhook 归一化 | **可行** | 四家事件可映射统一枚举；Svix/ECDSA/HMAC 三种验签各写一次 |
| 合规 | **灰色但可管理** | 单账号聚合不触线；叙事与社区治理决定是否升级为风险 |
| 送达率 | **可控** | 低量事务场景共享池合理；平台粘性 + 子域隔离 + 质量护栏 |

**最终判定：有条件可行。** 条件为：(1) 每平台仅注册一个账号，定位为多供应商可靠性网关；(2) DNS 按"每平台独立子域"架构落地并逐家验证 DMARC；(3) 路由器区分 accepted/delivered 两阶段并贯穿幂等键；(4) 内置退信/投诉阈值护栏（Resend <0.08%/4%，Mailgun ≤5%/0.08%）。

### 主要风险前三名

1. **平台合规风险（最高）**：免费额度是试用/获客导向，Resend/Twilio/Mailgun 条款均直接针对"绕过限制/多免费账户"设防；开源传播若诱导批量注册将触碰 Mailgun "assist another person" 条款。可控手段：中立定位 + 拒绝多账号路径 + README 合规须知。
2. **送达率风险**：共享 IP 池邻居效应 + 多平台基础设施跳变导致信誉碎片化，Outlook 最敏感。可控手段：平台粘性路由、按平台子域隔离、渐入权重、退信投诉护栏。
3. **工程可靠性风险**：四家异步语义（SendGrid 202 即接受）、错误码、附件编码、时区（Brevo `date` 为 CET）不一致；failover 判定错误会造成重复投递或漏发。可控手段：两阶段状态机 + 统一幂等键 + 原始 webhook 落库可重放。

### 主要参考来源

- Resend：[Send email API](https://resend.com/docs/api-reference/emails/send-email) · [Webhook event types](https://resend.com/docs/webhooks/event-types.md) · [email.bounced](https://resend.com/docs/webhooks/emails/bounced.md) · [AUP](https://resend.com/legal/acceptable-use) · [ToS](https://resend.com/legal/terms-of-service) · [Cloudflare DNS 指南](https://resend.com/docs/guides/dns/cloudflare.md)
- SendGrid：[Mail Send API](https://www.twilio.com/docs/sendgrid/api-reference/mail-send/mail-send) · [Event Webhook Reference](https://www.twilio.com/docs/sendgrid/for-developers/tracking-events/event) · [Domain Authentication](https://www.twilio.com/docs/sendgrid/ui/account-and-settings/how-to-set-up-domain-authentication) · [IP Warmup](https://www.twilio.com/docs/sendgrid/ui/sending-email/warming-up-an-ip-address) · [Twilio AUP](https://www.twilio.com/en-us/legal/aup)
- Mailgun：[Send an email API](https://documentation.mailgun.com/docs/mailgun/api-reference/send/mailgun/messages/post-v3--domain-name--messages) · [Domain webhooks](https://documentation.mailgun.com/docs/mailgun/api-reference/send/mailgun/domain-webhooks/get-v3-domains--domain--webhooks) · [DNS 最佳实践](https://documentation.mailgun.com/docs/mailgun/email-best-practices/dns) · [IP 预热](https://documentation.mailgun.com/docs/mailgun/email-best-practices/ip_address) · [AUP](https://www.mailgun.com/legal/aup/)
- Brevo：[Send a transactional email](https://developers.brevo.com/reference/send-transac-email) · [Transactional webhooks](https://developers.brevo.com/docs/transactional-webhooks.md) · [Domain authentication](https://developers.brevo.com/docs/domain-authentication-and-verification.md) · [ToS](https://www.brevo.com/legal/termsofuse/) · [AUP](https://www.brevo.com/legal/antispampolicy/)
- SPF：[dmarcian SPF Syntax Table](https://dmarcian.com/spf-syntax-table/)（RFC 7208 的 10 次查询上限与 PermError 后果）
