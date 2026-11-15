# CatMail API 用户指南

本指南面向**调用 CatMail 发送能力与集成能力**的开发者：统一发送 API、Resend SDK 兼容端点、邮件模板、幂等键、送达回执与送达事件 Webhook、SMTP 入口，以及错误码全集与重试语义。

> 事实来源与同步说明：本文与内嵌 OpenAPI 规范（服务运行后访问 `GET /docs/openapi.yaml`，或仓库内 `backend/internal/docs/openapi.yaml`）保持同一契约。文中所有示例的密钥均为脱敏占位样式（`cm-abcd…wxyz`），请替换为你自己的凭据。

## 目录

- [1. 简介](#1-简介)
- [2. 快速开始](#2-快速开始)
- [3. 统一发送 API](#3-统一发送-api)
  - [认证](#认证)
  - [请求字段](#请求字段)
  - [响应](#响应)
  - [channel 参数与轮换语义](#channel-参数与轮换语义)
  - [调用方限速与日额度](#调用方限速与日额度)
- [4. Resend SDK 兼容接入](#4-resend-sdk-兼容接入)
- [5. 邮件模板](#5-邮件模板)
- [6. 幂等键防重发](#6-幂等键防重发)
- [7. 错误处理与重试语义](#7-错误处理与重试语义)
- [8. 送达回执 Webhook（CatMail 推送到你的服务）](#8-送达回执-webhookcatmail-推送到你的服务)
- [9. 送达事件回传（邮件平台推送到 CatMail）](#9-送达事件回传邮件平台推送到-catmail)
- [10. SMTP 接入](#10-smtp-接入)
- [11. 运维与观测](#11-运维与观测)
- [12. 常见问题（FAQ）](#12-常见问题faq)
- [附录：管理 API 端点一览](#附录管理-api-端点一览)

---

## 1. 简介

**CatMail** 是一个面向个人与小团队的多供应商事务性邮件 **failover 网关**：对内暴露一套统一的发送 API（HTTP + 可选 SMTP 入口），对外连接 Resend、Brevo、Mailjet、SMTP2GO、Mailgun、Elastic Email、Mailtrap、阿里云邮件推送、通用 SMTP 或本地 mock 渠道。

你只需要调用**一个发送接口**（`POST /v1/send`）：

- 网关按**优先级/轮询策略**在多个渠道间自动选路；
- 某个渠道**额度用尽、被限流（429）、认证失效（401/403）或网络故障**时，自动 failover 到下一候选渠道；
- 平台**受理成功之后绝不换渠道重发**（同一封邮件不会被平台投递两次）。

除 HTTP API 外，CatMail 还提供：**Resend SDK 兼容端点**（`POST /emails`，把现有 Resend SDK 的 base URL 指过来即可）、**邮件模板**（`template_id` + 变量渲染）、**幂等键防重发**（`Idempotency-Key`）、**送达回执 Webhook**（send_accepted/send_failed 推送到你的服务）、**送达事件回传**（各平台的 delivered/bounced/complained 事件汇聚到 CatMail，联动抑制列表），以及可选的 **SMTP 入口**。

管理与观测（渠道/日志/模板/抑制列表等）通过内置中文 Web 后台完成，完整管理端点见 [`SPEC.md` §5](../SPEC.md) 与内嵌文档页 `GET /docs`。

---

## 2. 快速开始

前提：CatMail 已部署并运行（构建与部署见 [README](../README.md#快速开始)），管理后台 `http://localhost:8080` 可登录。首次启动自带一条 **mock 示例渠道「示例渠道」**（本地模拟发送、不产生真实网络请求，默认发件人 `sender@example.com`），开箱即可跑通全流程。

**第 1 步：创建 API Key**

管理后台「API Keys」页 → 新建。明文 key **仅显示一次**，形如：

```
cm-abcd1234efgh5678ijkl9012mnop3456
```

**第 2 步：发送第一封邮件（curl）**

```bash
curl -X POST http://localhost:8080/v1/send \
  -H "Authorization: Bearer cm-abcd1234efgh5678ijkl9012mnop3456" \
  -H "Content-Type: application/json" \
  -d '{
    "to": "you@example.com",
    "subject": "CatMail 第一封",
    "text": "如果你收到这封邮件，说明链路已打通。"
  }'
```

请求省略了 `from` 与 `channel`：网关自动落到「示例渠道」，并回退使用渠道默认发件人。成功响应：

```json
{
  "id": "9c1e8f4a-3f2b-4c5d-8e9f-a1b2c3d4e5f6",
  "channel": "示例渠道",
  "provider": "mock",
  "provider_message_id": "mock-0d1e2f3a-4b5c-6d7e-8f9a-0b1c2d3e4f5a",
  "accepted": true
}
```

**第 3 步：查发送日志**

- 管理后台「发送日志」页可按状态/渠道/关键词筛选，详情面板可预览正文与平台消息 ID；
- 或调用管理 API（Cookie 会话，供后台/脚本使用）：

```bash
# 先登录拿会话 Cookie，再查询日志
curl -s -c cookies.txt -X POST http://localhost:8080/api/auth/login \
  -H "Content-Type: application/json" \
  -d '{"username":"admin","password":"你的密码"}'

curl -s -b cookies.txt "http://localhost:8080/api/logs?page=1&page_size=20&status=sent"
```

> 接入真实平台（Resend/Brevo/Mailgun 等）与验证 failover 的完整步骤，见 [docs/platforms.md「接入真实平台清单」](./platforms.md#接入真实平台清单)。

---

## 3. 统一发送 API

### 认证

所有发送请求携带 `Authorization: Bearer <ApiKey>` 头：

```
Authorization: Bearer cm-abcd1234efgh5678ijkl9012mnop3456
```

- Key 必须处于**启用**状态；缺失、无效或停用 → `401`（错误体见[错误处理](#7-错误处理与重试语义)）。
- Key 上可配置**每秒限速**（`rate_per_sec`）与**每日额度**（`daily_quota`），见[调用方限速与日额度](#调用方限速与日额度)。

### 请求字段

```
POST /v1/send
Content-Type: application/json        # 请求体上限 30MB（容纳 base64 附件）
Authorization: Bearer cm-...
Idempotency-Key: <可选，见幂等键章节>
```

请求体为 JSON（Resend 风格扁平结构）：

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
  "attachments": [ { "filename": "a.pdf", "content_base64": "JVBERi0xLjcK…" } ],
  "tags": [ { "name": "campaign", "value": "nov" } ],
  "channel": "resend主号",
  "template_id": 1,
  "variables": { "name": "张三" }
}
```

| 字段 | 类型 | 必填 | 说明与限制 |
|---|---|---|---|
| `from` | string | 是（带回退） | `"Name <addr>"` 或纯地址，≤1024 字符。未提供时：指定了 `channel` → 用该渠道默认发件人（渠道也未配置 → 400）；未指定渠道 → 留空放行，由轮换按各候选渠道的默认发件人逐候选回退 |
| `to` | string 或 string[] | 是 | 统一按数组处理；单项 ≤320 字符且须为合法邮箱；`to`+`cc`+`bcc` 合计 ≤100 |
| `subject` | string | 是 | ≤998 字符（RFC 单头行上限）。**使用 `template_id` 时可省略**（由模板渲染结果整体替换） |
| `html` | string | 二选一 | HTML 正文，各 ≤1048576 字符（1MB），`html`/`text` 至少一个非空。**使用 `template_id` 时必须省略**（否则 400 `body_conflict`） |
| `text` | string | 二选一 | 纯文本正文，限制同上 |
| `cc` / `bcc` | string 或 string[] | 否 | 形态同 `to` |
| `reply_to` | string 或 string[] | 否 | 形态同 `to`；接受 camelCase 别名 `replyTo`（`reply_to` 优先） |
| `headers` | object | 否 | string→string 自定义邮件头，≤50 个；键非空 ≤256 字符、值 ≤4096 字符；**键值不得包含换行符**（防头注入）→ 400 |
| `attachments` | array | 否 | ≤10 个；每项 `{filename, content_base64}`（`content` 为等价别名），`filename` 必填 ≤255 字符，base64 须可解码（容忍中间换行/空格） |
| `tags` | array | 否 | ≤20 个 `{name, value}`，`name` 必填；适配器尽力透传（resend/brevo/smtp2go/mailgun 透传，其余平台不透传） |
| `channel` | string | 否 | 指定使用的渠道名（精确匹配，≤128 字符），见[下文](#channel-参数与轮换语义) |
| `template_id` | integer | 否 | 引用管理端邮件模板（见[邮件模板](#5-邮件模板)）：渲染结果整体替换 `subject`/`html`/`text` |
| `variables` | object | 否 | 模板渲染变量：键数量 ≤100、变量名 ≤128 字符；支持嵌套对象点路径取值；未携带 `template_id` 时被忽略 |

参数校验失败一律返回 `400 invalid_request`（不换渠道重试，详见[错误处理](#7-错误处理与重试语义)）。

### 响应

成功 `200`：

```json
{
  "id": "9c1e8f4a-3f2b-4c5d-8e9f-a1b2c3d4e5f6",
  "channel": "resend主号",
  "provider": "resend",
  "provider_message_id": "e30ea7c1-0b2d-4f3e-9a8b-1c2d3e4f5a6b",
  "accepted": true
}
```

| 字段 | 说明 |
|---|---|
| `id` | CatMail 内部 message_id（uuid），贯穿发送日志与送达事件关联 |
| `channel` | 实际使用的渠道名 |
| `provider` | 渠道的平台类型 |
| `provider_message_id` | 平台返回的消息 ID（后续送达事件按它关联回这条日志） |
| `accepted` | 恒为 `true`（受理成功） |
| `missing_variables` | **仅使用模板且有未提供变量时出现**：缺失变量名数组（占位符已留空），见[邮件模板](#5-邮件模板) |

> `accepted: true` 表示平台已**受理**，不等于最终送达收件箱；投递结果通过[送达事件回传](#9-送达事件回传邮件平台推送到-catmail)或各平台控制台跟踪。

Node.js 调用示例（内置 `fetch`，无需依赖）：

```js
async function sendMail(payload) {
  const res = await fetch("http://localhost:8080/v1/send", {
    method: "POST",
    headers: {
      "Authorization": "Bearer cm-abcd1234efgh5678ijkl9012mnop3456",
      "Content-Type": "application/json",
      // 可选：幂等键（建议用业务唯一号），见「幂等键防重发」章节
      "Idempotency-Key": "order-20261114-000123",
    },
    body: JSON.stringify(payload),
  });
  const body = await res.json();
  if (!res.ok) {
    // 统一错误体：{ error: { type, message, code? } }
    throw new Error(`send failed (${res.status}): ${body.error?.type} ${body.error?.message}`);
  }
  return body; // { id, channel, provider, provider_message_id, accepted }
}

sendMail({
  from: "CatMail <noreply@mail.example.com>",
  to: ["user@example.com"],
  subject: "验证你的邮箱",
  html: "<p>验证码：<strong>123456</strong>，5 分钟内有效。</p>",
});
```

Python 调用示例（`requests`）：

```python
import requests

def send_mail(payload: dict) -> dict:
    res = requests.post(
        "http://localhost:8080/v1/send",
        headers={
            "Authorization": "Bearer cm-abcd1234efgh5678ijkl9012mnop3456",
            "Idempotency-Key": "order-20261114-000123",  # 可选幂等键
        },
        json=payload,
        timeout=30,
    )
    data = res.json()
    if res.status_code != 200:
        err = data.get("error", {})
        raise RuntimeError(f"send failed ({res.status_code}): {err.get('type')} {err.get('message')}")
    return data  # {"id": ..., "channel": ..., "provider": ..., "provider_message_id": ..., "accepted": True}

send_mail({
    "from": "CatMail <noreply@mail.example.com>",
    "to": ["user@example.com"],
    "subject": "验证你的邮箱",
    "html": "<p>验证码：<strong>123456</strong>，5 分钟内有效。</p>",
})
```

### channel 参数与轮换语义

**不指定 `channel`**（默认）：候选为所有**已启用**的渠道（冷却到期渠道自动复位参与）。

1. 若请求提供了 `from`，先按发件域名过滤：渠道配置了 `allowed_domains` 且 `from` 的域名不在列表 → 该渠道被跳过；请求未提供 `from` 时不做域名过滤（实际发件人由候选渠道的默认发件人决定）。
2. 剔除当日/当月额度已用尽的渠道；仍无候选 → `429 quota_exhausted`。
3. 按「系统设置」中的策略排序：`priority`（默认，优先级数字小者先行，同级按最久未用）或 `round_robin`（仅按最久未用轮询）。
4. 逐个尝试（默认最多 `max_attempts=3` 个渠道）：
   - 平台 **401/403** → 该渠道**自动禁用**，换下一家；
   - 平台 **429**（含 Brevo 额度耗尽的 `400 + not_enough_credits`）→ 该渠道进入**冷却**（默认 60 秒，到期自动恢复），换下一家；
   - 网络错误 / 5xx / 402 → 记**连续失败**并换下一家（连续失败达阈值，默认 5 次，自动禁用）；
   - 其余 4xx（参数类）→ 直接返回 `400 invalid_request`，**不换渠道**。

**指定 `channel`**：候选仅该渠道。渠道不存在或发件域名不匹配 → `400`；渠道停用/冷却未到/额度用尽 → `429 quota_exhausted`。

**铁律**：平台**受理（accepted）成功之后绝不换渠道重发**——同一封邮件不会被重复投递；额度只在受理成功时 +1（发送前原子预留、失败自动返还）。

### 调用方限速与日额度

API Key 可配置两个独立维度（管理后台「API Keys」页，均可为 0 = 不限），强制点在发送入口鉴权处，**先限速后额度**，被拒请求不进入发送流程、不落发送日志：

| 维度 | 行为 | 触发后的响应 |
|---|---|---|
| `rate_per_sec` > 0 | 按 Key 维度令牌桶限速；取得令牌的等待超过 3 秒 → 拒绝 | `429 rate_limited`「请求过于频繁（rate_per_sec 限制）」 |
| `daily_quota` > 0 | 统计该 Key 今日发送成功（`status=sent`）条数，达到上限 → 拒绝 | `429 quota_exhausted`「ApiKey 今日额度已用尽（daily_quota=N）」 |

Key 日额度与 SMTP 入口发送**共用同一额度池**（SMTP 发送同样按 Key 归因落库）。

---

## 4. Resend SDK 兼容接入

`POST /emails` 与 `POST /v1/send` **完全同构**：同样的 Bearer ApiKey 认证、请求/响应/错误体、幂等键、30MB 请求体上限，两条路径共用同一处理逻辑。这是为「把现有 Resend SDK 的 base URL 指向 CatMail 后零改造接入」准备的兼容端点。

把 Resend SDK 的 `baseUrl` 指向 CatMail 根地址，ApiKey 换成后台创建的 `cm-` 密钥即可：

Node.js（`resend` 官方 SDK）：

```ts
import { Resend } from "resend";

const resend = new Resend("cm-abcd1234efgh5678ijkl9012mnop3456", {
  baseUrl: "http://localhost:8080",   // 指向你的 CatMail 地址
});

await resend.emails.send({
  from: "CatMail <noreply@mail.example.com>",
  to: ["user@example.com"],
  subject: "验证你的邮箱",
  html: "<p>验证码：<strong>123456</strong></p>",
});
```

Python（`resend` 官方 SDK）：

```python
import resend

resend.api_key = "cm-abcd1234efgh5678ijkl9012mnop3456"
resend.base_url = "http://localhost:8080"  # 指向你的 CatMail 地址

resend.Emails().send({
    "from": "CatMail <noreply@mail.example.com>",
    "to": ["user@example.com"],
    "subject": "验证你的邮箱",
    "html": "<p>验证码：<strong>123456</strong></p>",
})
```

SDK 兼容细节：

- `to`/`cc`/`bcc`/`reply_to` 既接受单字符串也接受数组；`replyTo`（camelCase）为等价别名；
- 附件既接受 `{filename, content_base64}` 也接受 Resend SDK 实际发送的 `{filename, content}`；
- 邮件模板扩展字段（`template_id`/`variables`）在 `/emails` 与 `/v1/send` 上同样可用（代码两路径共用同一解析结构；OpenAPI 规范当前仅在 `/emails` 的 schema 中列出这两个字段——待收口确认，不影响使用）；
- SDK 忽略响应中的未知字段（如 `channel`），不影响既有解析。

curl 示例（HTML 邮件 + 附件，走兼容端点）：

```bash
curl -X POST http://localhost:8080/emails \
  -H "Authorization: Bearer cm-abcd1234efgh5678ijkl9012mnop3456" \
  -H "Content-Type: application/json" \
  -d '{
    "from": "CatMail <noreply@mail.example.com>",
    "to": ["user@example.com"],
    "subject": "月度账单",
    "html": "<p>您好，11 月账单请见附件。</p>",
    "attachments": [
      { "filename": "invoice.pdf", "content_base64": "JVBERi0xLjcK..." }
    ]
  }'
```

---

## 5. 邮件模板

邮件模板是可复用的 subject/html/text 内容骨架，占位符风格 `{{var}}`。在管理后台创建维护，发送时以 `template_id` 引用并携带 `variables` 渲染。

### 占位符语法与渲染语义

- 占位符 `{{var}}`，容忍两侧空白（`{{ var }}` 等价）；变量名支持**点路径**取嵌套对象值（`{{user.name}}` ← `variables: {"user": {"name": "张三"}}`）；中文键合法。
- **转义规则**：`html` 中的变量值做 HTML 转义（`<>&'"`，无关闭转义的逃生门）；`subject` 与 `text` 属纯文本语境，变量原样替换。
- **缺失变量不报错**：未提供的变量占位符替换为**空串**，变量名收集后：
  - 成功响应附带 `missing_variables: ["变量名", …]` 字段（仅模板发送且有缺失时出现）；
  - 服务端日志同步记录。
  - 这样可选变量天然支持、缺参不阻断事务性发送；渲染结果整体随发送日志落库，可在日志详情面板核对。

### 管理模板（Cookie 会话 API）

| 端点 | 说明 |
|---|---|
| `GET /api/templates` | 全量列表（id 升序，不含正文大字段）→ `{items:[{id,name,subject,variables,created_at,updated_at}]}` |
| `POST /api/templates` | 新建。`name`（1-128 字符，唯一）与 `subject`（非空 ≤998 字符）必填；`html`/`text` 至少一个非空（各 ≤1MB 字符）；`variables` 为显式变量名清单（可选，缺省时按 subject/html/text 中的 `{{var}}` 占位符自动提取；≤100 条、单项 ≤128 字符）。重名 → 400「模板名称已存在」 |
| `GET /api/templates/:id` | 完整详情（含 `html_body`/`text_body`） |
| `PUT /api/templates/:id` | 部分更新：省略字段 = 保留原值（`name`/`subject` 显式空串视作未修改；`html`/`text` 显式空串 = 清空）；`variables` 省略 = 按合并后的内容重新提取，显式提供（含空数组）= 以清单为准 |
| `DELETE /api/templates/:id` | 删除 → `{ok:true}`。历史发送日志保留 `template_id` 供追溯（模板删除后日志页显示为 ID） |

```bash
# 创建模板（会话 Cookie 见快速开始）
curl -s -b cookies.txt -X POST http://localhost:8080/api/templates \
  -H "Content-Type: application/json" \
  -d '{
    "name": "邮箱验证码",
    "subject": "您的验证码：{{code}}",
    "html": "<p>您好 {{user.name}}，验证码为 <strong>{{code}}</strong>，5 分钟内有效。</p>",
    "text": "您好 {{user.name}}，验证码为 {{code}}，5 分钟内有效。"
  }'
```

### 发送时使用模板（`template_id` + `variables`）

`POST /v1/send` 与 `POST /emails` 均支持：

```bash
curl -X POST http://localhost:8080/v1/send \
  -H "Authorization: Bearer cm-abcd1234efgh5678ijkl9012mnop3456" \
  -H "Content-Type: application/json" \
  -d '{
    "to": "user@example.com",
    "template_id": 1,
    "variables": { "user": { "name": "张三" }, "code": "123456" }
  }'
```

语义要点：

- **渲染时机**：渲染发生在幂等检查之后、实际发送之前——渲染结果即发送快照（发送日志存的是渲染后的正文）。
- **模板字段整体替换**：模板渲染结果替换请求中的 `subject`/`html`/`text`；使用模板时请求可两者皆缺。
- **`body_conflict`（400）**：请求同时携带 `template_id` 与非空 `html`/`text` → 显式报错（错误体 `type: "body_conflict"`），不静默覆盖——事务性邮件发错内容比发不出去代价更高。`subject` 不参与冲突判定（Resend SDK 的 subject 必填，迁移模板的 SDK 调用方仍会带着 subject；其值会被模板渲染结果覆盖）。
- **`unknown_template`（400）**：`template_id` 不存在 → 错误体 `type: "unknown_template"`。
- **幂等键重放语义不变**：重放直接回放存档响应，不重新渲染。

### SMTP 入口使用模板

本地 SMTP 应用经**报文头**携带模板指令，语义与 API 完全同口径（见 [SMTP 接入](#10-smtp-接入)）：

- `X-CatMail-Template: <模板 ID>`（十进制正整数）；
- `X-CatMail-Template-Vars: <JSON 对象>`，或 `base64:<base64(JSON)>`（UTF-8 变量值在纯 ASCII SMTP 链路上的稳妥携带形态；两种形态均可）。

报文自带非空正文时指定模板 → `body_conflict`，SMTP 应答 `501`（永久失败）；模板不存在 → `unknown_template`，同样 `501`。

---

## 6. 幂等键防重发

网络超时后的盲目重试是「重复发信」的头号来源。发送入口（`POST /v1/send` 与 `POST /emails`）支持可选请求头 `Idempotency-Key` 启用去重。

**基本规则**

- **头缺失 = 完全不启用去重**，行为与无幂等完全一致（零额外开销）；
- 作用域绑定当前 ApiKey——不同调用方使用相同键互不干扰；
- 键为去首尾空白后 **1–128 字符的可见 ASCII**（键内空格拒绝）；显式携带非法键 → `400 invalid_request`，**不静默降级为无键**。

**状态机**

| 场景 | 行为 |
|---|---|
| 首次请求 | 正常执行发送，响应（成功**或失败**）被存档 |
| 同键重放（首次已完成） | 直接**逐字节回放**存档响应（不再发送），响应头附 `Idempotent-Replay: true` |
| 同键并发（首次仍在处理中） | `409`（错误体 `type: "invalid_request"`）「相同的 Idempotency-Key 请求正在处理中」 |
| 处理中超过 5 分钟 | 视为进程崩溃残留，自动认领重新发送 |
| 存档超过 24 小时 | 懒惰清理，同键视为全新请求 |

**取舍说明**：失败响应（400/429/502）同样存档重放——带同键重试会拿到**同一错误**而不是触发二次发送。想换参数重试，请换一个键（或不带键）。

```bash
curl -X POST http://localhost:8080/v1/send \
  -H "Authorization: Bearer cm-abcd1234efgh5678ijkl9012mnop3456" \
  -H "Idempotency-Key: order-20261114-000123" \
  -H "Content-Type: application/json" \
  -d '{ "to": "user@example.com", "subject": "订单已创建", "text": "..." }'
```

---

## 7. 错误处理与重试语义

### 错误体结构

所有接口统一错误体：

```json
{ "error": { "type": "invalid_request", "message": "人类可读原因" } }
```

部分错误额外携带机器可读 `code` 字段（加法扩展，不影响既有 `type`/`message` 解析）：

```json
{ "error": { "type": "invalid_request", "code": "recipient_suppressed", "message": "收件人已被抑制列表拦截：a@example.com（bounce）" } }
```

### 错误码总表

`error.type` 全集（从源码核对）：

| type | HTTP | 含义 | 调用方处理建议 |
|---|---|---|---|
| `invalid_request` | 400 | 参数错误：缺字段、格式非法、超限、指定渠道不存在/域名不匹配、模板正文冲突等。**不换渠道重试**，视为调用方问题 | 修正请求参数后重试 |
| `invalid_request` | 404 | 未知路径（`/api`、`/v1`、`/emails` 及其子路径下的未注册路径）「接口不存在」，或资源不存在（日志/模板/抑制记录等详情类端点） | 检查路径与资源 ID |
| `invalid_request` | 409 | 同 Idempotency-Key 请求仍在处理中（`code` 字段缺失，靠状态码区分） | 稍候重试同键（会重放或拿到结果），勿换键 |
| `unauthorized` | 401 | ApiKey 缺失、无效或已停用 | 检查 Bearer 头；到后台确认 Key 状态 |
| `forbidden` | 403 | 管理面来源不在 `CATMAIL_ADMIN_CIDR` 白名单（仅影响 login/管理 API/`/metrics`，**不影响发送入口**） | 从白名单网段访问 |
| `rate_limited` | 429 | Key 配置了 `rate_per_sec` 且取得令牌等待超 3 秒 | 降低调用频率后重试 |
| `quota_exhausted` | 429 | 所有可用渠道额度耗尽；指定渠道停用/冷却/超额；Key 当日 `daily_quota` 用尽；或渠道级出站限速等待超时 | 查看后台渠道用量与状态；稍后重试或提升额度 |
| `provider_error` | 502 | 已尝试全部候选渠道仍失败，`message` 附最后一次平台错误（形如「已尝试 N 个渠道均发送失败：<平台错误>」） | 查看后台渠道「最近错误」；可稍后重试 |
| `server_error` | 500 | 服务器内部错误（客户端仅见「服务器内部错误」，详情入服务端日志） | 稍后重试；排查服务端日志 |
| `unknown_template` | 400 | 发送请求携带的 `template_id` 不存在（模板已删除或 ID 错误） | 修正模板 ID |
| `body_conflict` | 400 | 请求同时携带 `template_id` 与非空 `html`/`text` | 二选一：去掉正文字段，或去掉 `template_id` |

`error.code` 附加码（随 `type` 一并返回）：

| code | HTTP / type | 含义 | 调用方处理建议 |
|---|---|---|---|
| `recipient_suppressed` | 400 / `invalid_request` | 收件人（to/cc/bcc 任一）命中**抑制列表**，被拒在门外：不进入轮换、不预留额度、不落发送日志。`message` 会列出命中地址（最多列 5 个，超出以计数注记） | 该地址大概率已退信/投诉过；先到后台「抑制列表」核对原因，确属误伤再解除（见[送达事件回传](#9-送达事件回传邮件平台推送到-catmail)） |

> 409 只出现在带幂等键的并发场景；HTTP 层面它是「冲突」，错误体 `type` 仍为 `invalid_request`。

### 失败重试语义（重要）

- **参数类 400（含 `body_conflict`/`unknown_template`/`recipient_suppressed`）**：原样重试必然原样失败，先修参数/先解除抑制。
- **429（`rate_limited`/`quota_exhausted`）**：可安全重试（退避）。重试时可携带**新的** Idempotency-Key——同键重试只会重放首次的错误存档。
- **502（`provider_error`）**：说明所有候选渠道都试过且失败，可稍后重试；重试会重新走轮换（此时可能有渠道已恢复）。
- **受理成功（200）后不存在「换渠道重发」**：`accepted: true` 的邮件不会因后续平台投递失败而换渠道重发；投递失败的处理路径是[送达事件回传](#9-送达事件回传邮件平台推送到-catmail)与抑制列表，而不是 API 层重试。

---

## 8. 送达回执 Webhook（CatMail 推送到你的服务）

如果你需要无人值守的自动化归档/告警，可为每个 API Key 配置**回执 Webhook**：发送受理成功（`send_accepted`）与终态失败（`send_failed`）时，CatMail 向你配置的 URL 推送固定格式 JSON。

### 配置

管理后台「API Keys」页编辑 Key：

- `webhook_url`：`http(s)://` 地址，host 非空；
- `webhook_events`：事件白名单，仅 `send_accepted` / `send_failed` 子集。**空 = 未订阅任何事件 = 不推送**。

可用 `POST /api/keys/:id/webhook/test`（管理 API）向该地址推一条 `event: "test"` 的连通性测试回执（恒 200，返回 `{ok:true}` 或 `{ok:false,error:"…"}`）。

### 推送格式

统一外层 envelope：

```json
{
  "source": "catmail",
  "event": "send_accepted",
  "api_key_id": 1,
  "data": { … }
}
```

`send_accepted` 的 `data`（平台受理成功快照）：

```json
{
  "message_id": "9c1e8f4a-3f2b-4c5d-8e9f-a1b2c3d4e5f6",
  "provider": "resend",
  "channel": "resend主号",
  "provider_message_id": "e30ea7c1-…",
  "to": ["user@example.com"],
  "subject": "订单已创建",
  "latency_ms": 342,
  "accepted_at": "2026-11-14T09:00:00+08:00"
}
```

`send_failed` 的 `data`（终态失败快照；**仅在 429 额度/限速与 502 候选穷尽两种终态推送**，参数类 400 不推——同步响应里当场已见错误）：

```json
{
  "message_id": "9c1e8f4a-…",
  "error": "已尝试 3 个渠道均发送失败：<平台错误>",
  "tried": 3,
  "failed_at": "2026-11-14T09:00:05+08:00"
}
```

推送行为：单次 5 秒超时；失败间隔 1 秒重试 1 次；仍失败仅记服务端日志——回执通道故障**绝不影响发送主链路**。回执按 Key 维度 opt-in，与管理员侧的失败通知（站内 + Webhook 双通道）相互独立。

### 签名校验

若 CatMail 侧配置了回执/通知 Webhook 的**签名密钥**，每个回执请求携带请求头：

```
X-CatMail-Signature: t=<unix秒>,v1=<hex>
```

- `t` 为发送时刻的 Unix 秒级时间戳；
- `v1 = hex(HMAC-SHA256(secret, "<t>." + <原始请求体>))`——签名内容是**时间戳、点号、未经任何再序列化的原始 body 字节**；
- **时间戳容差 ±5 分钟**：超窗应拒绝（防重放）；窗口内的重放由调用方自行按 `t` + body 摘要去重兜底；
- **secret 未配置时，请求不携带 `X-CatMail-Signature` 头**——调用方按「无头即跳过校验」处理，不要把缺头当失败。

Node.js 完整校验示例（Express）：

```js
const crypto = require("crypto");

const WEBHOOK_SECRET = process.env.CATMAIL_WEBHOOK_SECRET; // 与 CatMail 侧一致；未配置则跳过校验

function verifyCatMailSignature(req) {
  const header = req.get("X-CatMail-Signature");
  if (!header) return !WEBHOOK_SECRET; // 无头：仅当服务端未配置签名时放行

  const parts = Object.fromEntries(header.split(",").map((kv) => kv.split("=")));
  const t = Number(parts.t);
  const sig = parts.v1;
  if (!t || !sig) return false;

  // 时间戳容差 ±5 分钟（300 秒）
  if (Math.abs(Date.now() / 1000 - t) > 300) return false;

  // "<t>." + 原始请求体（必须用 raw body，不能 re-marshal）
  const raw = req.rawBody; // 见下方 express.raw 挂载
  const expected = crypto
    .createHmac("sha256", WEBHOOK_SECRET)
    .update(`${t}.${raw}`)
    .digest("hex");
  return crypto.timingSafeEqual(Buffer.from(sig), Buffer.from(expected));
}

// 关键：用 raw body 校验签名
app.post("/catmail/receipt",
  express.raw({ type: "*/*", limit: "1mb" }),
  (req, res) => {
    if (!verifyCatMailSignature(req)) return res.status(401).end();
    const payload = JSON.parse(req.body);
    // payload: { source:"catmail", event:"send_accepted"|"send_failed"|"test", api_key_id, data }
    res.json({ ok: true });
  });
```

Python 完整校验示例（Flask）：

```python
import hmac, hashlib, time
from flask import Flask, request, jsonify

app = Flask(__name__)
WEBHOOK_SECRET = None  # 与 CatMail 侧一致；None 表示未配置签名，跳过校验

def verify_signature(raw_body: bytes) -> bool:
    header = request.headers.get("X-CatMail-Signature")
    if header is None:
        return WEBHOOK_SECRET is None  # 无头：仅当服务端未配置签名时放行
    fields = dict(kv.split("=", 1) for kv in header.split(","))
    t, sig = int(fields["t"]), fields["v1"]
    if abs(time.time() - t) > 300:  # ±5 分钟容差
        return False
    expected = hmac.new(
        WEBHOOK_SECRET.encode(), f"{t}.".encode() + raw_body, hashlib.sha256
    ).hexdigest()
    return hmac.compare_digest(sig, expected)

@app.post("/catmail/receipt")
def receipt():
    raw = request.get_data()  # 原始字节，未再序列化
    if not verify_signature(raw):
        return "", 401
    payload = request.get_json()
    # payload: {"source":"catmail","event":"send_accepted"|"send_failed"|"test","api_key_id":..,"data":{..}}
    return jsonify(ok=True)
```

> 签名密钥的生成与查看入口以 CatMail 管理后台实际界面为准（该能力正按上述契约收口实现）；在密钥可用前，回执请求不带签名头。

---

## 9. 送达事件回传（邮件平台推送到 CatMail）

「平台受理」不等于「最终送达」。把各平台的 **Webhook** 指回 CatMail，即可在站内看到 delivered/bounced/complained 的完整投递轨迹，并让**抑制列表**自动生效。

### 接入形态：每渠道一个 URL

每个渠道拥有独立的入站事件地址：

```
POST http://<你的 CatMail 地址>/webhooks/mail-events/<token>
```

- `<token>` 为 32 位 hex 的**每渠道独立凭据**，渠道创建时自动生成；老版本渠道可在后台「渠道管理」中补生成/轮换（对应管理 API `POST /api/channels/:id/webhook-token`，`{"rotate":true}` 强制轮换，旧 URL 立即失效）；
- token 明文回显在渠道列表响应的 `webhook_token` 字段；
- 请求体为平台事件报文（JSON；兼容 Mailgun legacy 的 form-urlencoded），上限 1MB；摄取成功返回 `200 {"ok":true}`。

### 鉴权分层

| 层 | 适用平台 | 校验方式 |
|---|---|---|
| URL token（基础层） | 所有平台 | token 唯一索引精确定位渠道；无效 → `404`「webhook 地址无效或已轮换」 |
| 平台签名（增强层，有则必验） | Resend（Svix 规范）、Mailgun | 见下表；验败 → `401`「webhook 签名校验失败」，**不会**回退为仅 token 放行 |

签名细节：

| 平台 | 渠道侧需配置 | 校验算法 | 时间戳容差 |
|---|---|---|---|
| Resend | 渠道 `webhook_secret` 填 **Svix 签名串**（`whsec_…`，Resend 控制台创建 webhook endpoint 时获得） | Svix 规范：签名内容 `{svix-id}.{timestamp}.{payload}`，HMAC-SHA256 后 base64；兼容 `svix-*` 与 `webhook-*` 两套头名；窗口内按 `svix-id` 去重防重放 | ±5 分钟 |
| Mailgun | 渠道 `webhook_secret` 填 **Webhook Signing Key** | `hex(HMAC-SHA256(key, timestamp + token))` 与报文 `signature` 恒等比对；按 `token` 去重防重放 | ±15 分钟 |
| 其余平台 | 无需配置（不支持自定义请求头） | 仅 URL token 即共享凭据 | — |

签名校验失败 / 窗口内重放 → `401`；token 无效 → `404`；其余成功路径一律 `200`（识别不了的事件也会按 `raw` 存档，不猜测）。

### 各平台配置指引

| 平台 | 平台侧配置 | CatMail 侧 |
|---|---|---|
| Resend | 控制台「Webhooks」添加 endpoint，URL 填 CatMail 事件地址；订阅 `email.sent` / `email.delivered` / `email.bounced` / `email.complained` 等事件 | 渠道 `webhook_secret` 填 Svix 的 `whsec_…` 签名串 |
| Mailgun | 控制台「Webhooks」把 delivered / permanent fail / spam complaints 等事件指向 CatMail 事件地址 | 渠道 `webhook_secret` 填 Webhook Signing Key |
| Brevo | 控制台「Transactional → Webhooks」配置事件 URL | 无需密钥（token 即凭据） |
| Mailjet | 控制台配置 Event API（webhook）回调 URL | 无需密钥 |
| SMTP2GO / Elastic Email / Mailtrap / 阿里云邮件推送 | 平台支持 webhook/事件回调的按控制台指引配置；不支持的平台事件侧无回传 | 无需密钥；不支持的渠道无事件数据（健康度送达侧为 null） |

### 事件类型与归一化

所有平台事件统一归一化为 5 类存档（原始报文全量存档在详情 `raw` 字段，可查证）：

| 事件 | 含义 | 关联字段 |
|---|---|---|
| `sent` | 平台受理（可选上报） | — |
| `delivered` | 送达 | — |
| `bounced` | 退信；`bounce_type` = `hard`（永久，地址无效等）或 `soft`（临时，邮箱满等） | `bounce_type` |
| `complained` | 被收件方标记为垃圾邮件（投诉） | — |
| `raw` | 其他/未识别事件（opened/clicked/deferred…）原样存档，不计入送达率分母 | — |

摄取时尽力按平台消息 ID（去尖括号比对）反查本渠道的发送日志，命中则回填 `message_id`/`send_log_id`——后台「送达事件」页可从事件跳转定位到发送日志。管理端查询端点：`GET /api/mail-events`（`?page&page_size&event&bounce_type&channel_id&q`）、`GET /api/mail-events/:id`（含 `raw`）。

### 抑制列表联动（重要）

- **自动入库**：摄取到 **hard bounce** 或 **complained** 事件时，收件地址自动写入抑制列表（设置项 `suppression_auto_enabled`，默认开启；入库原因分别为 `bounce` / `complaint`）。抑制是**收件人维度、跨渠道生效**：同一地址在任何渠道退信过，就不再向它发送。
- **拦截**：此后所有发送入口（`/v1/send`、`/emails`、SMTP 入口、日志重发）对该地址的发送在轮换开始前即被拒绝：`400`，`error.code = "recipient_suppressed"`（见[错误码总表](#错误码总表)）。
- **退信率告警**：`bounced` 事件触发渠道近 7 天退信率判定，超过设置项 `bounce_rate_threshold`（百分比，默认 5，0 = 关闭）时经通知双通道广播 `bounce_rate_exceeded` 告警（每渠道每小时最多一次，防风暴）。
- **管理**：`GET /api/suppressions`（`?reason=bounce|complaint|manual` 筛选）、`POST /api/suppressions`（手动添加，`reason` 恒为 `manual`）、`DELETE /api/suppressions/:id`（解除）。注意：解除的是**当前抑制状态**而非拉黑豁免——同邮箱之后再收到 hard bounce/complaint 事件仍会自动重新入库。

---

## 10. SMTP 接入

只支持 SMTP 协议的本地应用（老框架、设备、内网系统）无需改造，把 CatMail 当普通 SMTP 服务器即可。设置 `CATMAIL_SMTP_ADDR`（如 `:2525`）启用（默认关闭，不监听任何端口）。邮件经统一轮换路径发送：额度预留、渠道限速、Key 配额、failover、发送日志、失败通知与 HTTP API **完全同口径**。

### 三种接入形态

| 形态 | 启用方式 | 适用场景 |
|---|---|---|
| **明文入口** | 仅设 `CATMAIL_SMTP_ADDR=:2525` | 内网/本机直连（默认形态）。loopback 来源免认证；非 loopback 来源必须 AUTH。明文链路**不要**跨公网使用 |
| **STARTTLS（587 风格）** | `CATMAIL_SMTP_ADDR=:587` + 证书对（`CATMAIL_SMTP_CERT_FILE`/`CATMAIL_SMTP_KEY_FILE`） | 明文监听广播 `STARTTLS` 扩展，客户端在 EHLO 后、发信前协商升级为 TLS |
| **TLS 直连（465 风格）** | `CATMAIL_SMTP_ADDR=:2525` + `CATMAIL_SMTP_TLS_ADDR=:465` + 证书对 | accept 后立即 TLS 握手的隐式 TLS 监听，与经典 SMTPS 客户端兼容 |

反向代理终结方案：不想让 CatMail 直接持有证书时，可用 stunnel / frp / cloudflared / nginx stream 等在前面终结 TLS，把解密后的明文流量转发到明文入口（未配证书时 CatMail 维持纯明文，不广播 STARTTLS）。

### 认证（AUTH）

- **凭据模型「API Key 即密码」**：`AUTH PLAIN` / `AUTH LOGIN` 的 **password 填某个启用的 ApiKey**（`cm-` 开头），**username 任意**（仅留日志）。命中后该连接的发送按此 Key 归因落库，并受其 `rate_per_sec`/`daily_quota` 约束（与 `/v1/send` 共用同一额度池）。
- **防开放中继**（按对端 IP 逐连接判定）：loopback（127.0.0.0/8、::1、localhost）**免认证**；非 loopback 来源必须 AUTH，未认证在 `RCPT` 前即 `530` 拒绝。监听 0.0.0.0 时两种策略并存，不会意外变成开放中继。
- `CATMAIL_SMTP_TLS_POLICY=require` 时，明文连接必须先 `STARTTLS` 成功才允许 AUTH（否则 `530 5.7.0`，防明文密码泄露）；TLS 直连监听天然豁免。

### 状态码映射（rotator 结果 → SMTP 应答）

| 场景 | SMTP 应答 |
|---|---|
| 受理成功 | `250 2.0.0 OK queued as <message_id>`（message_id 即响应 JSON 里的内部 ID） |
| 参数/内容问题（400，含 `body_conflict`/`unknown_template`/`recipient_suppressed`） | `501 5.6.0`（永久失败：客户端需改内容/参数重试） |
| 额度/限速（429） | `452 4.2.1`（临时失败：可稍后重试） |
| 候选渠道穷尽/内部错误（502/500） | `451 4.3.0`（临时失败：可重试） |
| MAIL SIZE / DATA 载荷超限 | `552 5.3.4` |
| 收件人超过 100 | `452 4.5.3` |
| 非 loopback 未认证 | `530`（RCPT 前拦截） |

其他协议参数：EHLO 扩展 `PIPELINING`、`SIZE`（= `CATMAIL_SMTP_MAX_SIZE_MB`，默认 25）、`8BITMIME`、`AUTH PLAIN LOGIN`、`STARTTLS`（证书已配置时）；单行 64KB 上限；单命令 60s / 单连接 5 分钟超时；同一连接可连续发多封。

### 典型应用配置示例（WordPress）

「WP Mail SMTP」或同类插件中把 SMTP 指向 CatMail：

| 插件字段 | 值（明文入口 / STARTTLS / TLS 直连） |
|---|---|
| SMTP Host | CatMail 所在主机（如 `catmail.internal`） |
| Encryption | None（内网明文） / STARTTLS（`TLS_POLICY=optional` + 证书） / SSL/TLS（指向 465 隐式 TLS 端口） |
| Port | `2525`（明文） / `587`（STARTTLS） / `465`（TLS 直连） |
| Username | 任意非空值（如 `catmail`）——CatMail 不校验 username |
| Password | 你的 API Key：`cm-abcd1234efgh5678ijkl9012mnop3456` |
| Authentication | 开启（ON） |

> WordPress 部署在**另一台机器**上时，来源非 loopback，必须填写 API Key 认证；与本机同机部署可免认证（用户名/密码留空），但仍然建议配置认证以便日志归因与配额管理。

### SMTP 环境变量表

| 环境变量 | 说明 | 默认值 |
|---|---|---|
| `CATMAIL_SMTP_ADDR` | 明文入口监听地址（如 `:2525`）；空 = SMTP 功能整体关闭 | 未设置（关闭） |
| `CATMAIL_SMTP_MAX_SIZE_MB` | 单封邮件 DATA 载荷上限（MB，正整数）；EHLO SIZE 广播与 `552` 拒收共用 | `25` |
| `CATMAIL_SMTP_TLS_ADDR` | TLS 直连（隐式 TLS，465 风格）监听地址；**必须先配置证书对**，否则启动即失败 | 未设置 |
| `CATMAIL_SMTP_CERT_FILE` | TLS 证书路径（PEM）；须与私钥**成对**配置，启动时校验存在/可读/可加载 | 未设置 |
| `CATMAIL_SMTP_KEY_FILE` | TLS 私钥路径（PEM）；同上 | 未设置 |
| `CATMAIL_SMTP_TLS_POLICY` | 明文入口的 STARTTLS 策略：`optional`（默认，广播但不强制）/ `require`（AUTH 前必须升级 STARTTLS；同样要求先配证书） | `optional` |

非法 TLS 组合（只配半套证书、`CATMAIL_SMTP_TLS_ADDR` 无证书、`require` 无证书等）一律**启动即退出**（快速失败），绝不带病启动。

### SMTP 使用邮件模板

报文头携带模板指令（语义与 API 的 `template_id`+`variables` 完全同口径，见[邮件模板](#5-邮件模板)）：

```
X-CatMail-Template: 1
X-CatMail-Template-Vars: {"user":{"name":"张三"},"code":"123456"}
```

变量值含非 ASCII（中文等）时推荐 `base64:` 前缀形态（后跟 base64 编码的 JSON 对象，两种形态均可）：

```
X-CatMail-Template-Vars: base64:eyJ1c2VyIjp7Im5hbWUiOiLlvKDkuIkifSwiY29kZSI6IjEyMzQ1NiJ9
```

指令非法、模板不存在（`unknown_template`）或报文自带非空正文（`body_conflict`）均为永久失败 → `501`。

---

## 11. 运维与观测

**内嵌文档页**：浏览器打开 `http://<地址>/docs` 查看完整 OpenAPI 3.1 渲染文档（单文件 HTML、零外部依赖、示例均为脱敏占位值）；规范原文在 `GET /docs/openapi.yaml`。两者公开无鉴权。

**健康探针**：`GET /api/health`（公开）→ `{"ok":true}`，供负载均衡/容器健康检查。

**Prometheus 指标**：`GET /metrics`（文本格式），四族指标：

| 指标 | 类型 | 说明 |
|---|---|---|
| `catmail_sends_total{channel_name,provider}` | counter | 发送成功计数（不含测试发送） |
| `catmail_test_sends_total{channel_name,provider}` | counter | 渠道测试发送计数 |
| `catmail_channels{id,name,provider,state}` | gauge | 渠道状态（enabled/manual_disabled/auto_disabled/cooldown） |
| `catmail_notifications_unread` | gauge | 站内通知未读数 |

实时聚合、不缓存；配置 `CATMAIL_ADMIN_CIDR` 后抓取器需在白名单网段内。

**自检**：`./catmail doctor` 执行 8 大项只读体检后立即退出——环境（Go/OS/版本）、配置（白名单/默认口令/Cookie Secure/可信代理/`CATMAIL_SECRET_KEY` 状态/SMTP TLS 证书与策略）、数据目录（可写/磁盘剩余）、数据库（连通/WAL 大小/逐表行数）、渠道配置完整性（按平台必填字段 + 凭据加密状态盘点）、通知与回执（Webhook URL 合法性）、端口监听（含隐式 TLS 端口）、汇总；任一 FAIL 退出码为 1，适合挂进部署脚本与监控。

**备份**：管理后台「系统设置 → 备份管理」或 `POST /api/backups`：`VACUUM INTO` 生成整库一致性快照（服务不停机），自动保留最近 10 份，支持下载/删除；可开启每日自动备份。PostgreSQL 数据源时备份接口返回 400（请用 `pg_dump/pg_restore`）。全部业务数据默认在 SQLite 单文件（`CATMAIL_DB` 指向路径，运行时伴随 `-wal`/`-shm`），迁移即备份该目录。

**发送日志治理**：按「日志保留天数」（默认 90，0 = 永久）每日自动清理；后台支持三模式手动清理（保留最近 N 天 / 最近 N 条 / 全部）。隐私敏感场景可关闭「日志正文落库」（`log_body_enabled`，仅影响新日志）。

**自动更新（实验性）**：配置 `CATMAIL_UPDATE_REPO=owner/repo`（GitHub Releases）后，「系统设置」页可检查并一键应用新版本：下载对应平台资产 `catmail_<os>_<arch>` → **强制 SHA-256 校验**（release 内 `checksums.txt`，缺失/不匹配一律拒绝且零落盘）→ 磁盘空间预检 → 临时文件 + fsync + rename 原子替换，重启进程生效。容器部署与 Windows 恒拒绝（指引拉新镜像/手动替换）；`CATMAIL_UPDATE_CHECK_ON_START=1` 可在启动时异步检查并经通知双通道广播新版本，`CATMAIL_UPDATE_PRERELEASE` 纳入预发布版本。未配置 `CATMAIL_UPDATE_REPO` 时两个端点返回 400（功能关闭）；发布侧打 `v*` tag 即由 `.github/workflows/release.yml` 自动产出资产与 checksums.txt。

---

## 12. 常见问题（FAQ）

**Q：发送返回 429 `quota_exhausted`，怎么办？**
按顺序排查：① 指定了 `channel`？→ 该渠道可能停用/冷却/当日额度用尽（后台渠道页看用量与状态徽章）；② 未指定 → 所有候选渠道额度耗尽或全部被域名过滤/停用；③ Key 配置了 `daily_quota` 且今日已用尽（错误 message 会写明 `daily_quota=N`）。渠道额度跨日/跨月惰性重置（下次发送时自动清零重新计数），也可在渠道上覆盖 `daily_limit_override`/`monthly_limit_override`（0 = 用平台默认）。

**Q：发送返回 429 `rate_limited`？**
Key 配置了 `rate_per_sec` 令牌桶限速且等待超 3 秒。降低调用频率，或调高/清零 Key 的限速配置。

**Q：发送返回 400 `code=recipient_suppressed`？**
收件人命中抑制列表（曾 hard bounce 或投诉，或被手动拉黑）。到后台「抑制列表」按邮箱筛选核对入库原因：确属误伤（如软退被误标硬退、旧客户回流）→ 解除该条记录；否则不要重发该地址（继续发会伤害域名信誉）。注意解除后若再次 hard bounce 会自动重新入库。

**Q：发送返回 400 `body_conflict` / `unknown_template`？**
`body_conflict`：请求同时带了 `template_id` 和非空 `html`/`text`——用模板时正文必须由模板渲染，二选一。`unknown_template`：`template_id` 不存在（被删除或 ID 传错），到「模板」页核对。

**Q：为什么模板发送的正文里变量是空的？**
未提供的变量占位符替换为空串（不报错）。检查响应中的 `missing_variables` 字段与服务端日志；嵌套变量注意点路径（`{{user.name}}` 对应 `variables: {"user": {"name": "…"}}`）。

**Q：响应 `accepted: true` 但收件箱没收到？**
`accepted` 是「平台已受理」。投递结果看：① CatMail「送达事件」页（需按[第 9 节](#9-送达事件回传邮件平台推送到-catmail)接入平台 Webhook）；② 各平台控制台的发送记录；③ 检查发件域名 SPF/DKIM 配置（建议每个平台一个独立子域，见 [docs/platforms.md「发送域名与 DNS」](./platforms.md#发送域名与-dnsspfdkim)）；④ 收件人是否在垃圾箱。

**Q：渠道被自动禁用（红色徽章）/冷却（黄色徽章）？**
红色 = 平台返回 401/403（Key 失效/被封）或连续失败达阈值（默认 5 次）——查「最近错误」与「自动禁用原因」，修复后手动「启用」。黄色 = 平台 429 限流退避，默认 60 秒自动恢复，无需人工干预。

**Q：带 `from` 发送报 400「指定渠道…不允许发件域名」？**
渠道配置了 `allowed_domains` 且 `from` 的域名不在列表。要么调整渠道的允许域名，要么改用该渠道已验证域名下的发件地址；不指定渠道时同理按域名过滤候选。

**Q：通过代理部署，平台 API 连不上？**
为该渠道单独配置出站代理 `proxy_url`（`http://`、`https://` 或 `socks5://`；SMTP 渠道仅 `socks5://`）。代理只影响该渠道的出站请求。CatMail 本身**不读取**环境变量里的全局代理配置。

**Q：SMTP 客户端认证失败（535）？**
password 必须是某个**启用中**的 ApiKey（`cm-` 开头），username 任意。另外确认来源 IP：非 loopback 来源必须认证；`CATMAIL_SMTP_TLS_POLICY=require` 时明文连接要先 STARTTLS 才能 AUTH。

**Q：幂等键重试拿到的还是上次的错误？**
这是刻意设计：失败响应同样存档重放，避免「重试反而造成二次发送」。想换参数重试，换一个新的 Idempotency-Key（或不带键）。24 小时后旧档案懒惰清理，同键视为全新请求。

**Q：收到的回执 Webhook 没有签名头？**
CatMail 侧未配置 Webhook 签名密钥时不携带 `X-CatMail-Signature` 头，属正常现象；配置后按[第 8 节](#8-送达回执-webhookcatmail-推送到你的服务)校验。

---

## 附录：管理 API 端点一览

管理 API（`/api/*`）使用 Cookie 会话（登录后签发，7 天有效；改密/被重置密码后该用户旧会话全部失效），供内置管理后台使用；请求/响应与错误体的完整契约见 [SPEC.md §5](../SPEC.md) 与内嵌文档页 `/docs`。端点一览：

| 分组 | 端点 |
|---|---|
| 健康/文档/指标 | `GET /api/health`（公开，返回 `{ok:true}`）· `GET /docs`、`GET /docs/openapi.yaml`（公开内嵌文档）· `GET /metrics`（Prometheus） |
| 会话与两步验证 | `POST /api/auth/login`（启用 TOTP 时返回 `{totp_required,ticket}`）· `POST /api/auth/login/totp` · `POST /api/auth/logout` · `GET /api/auth/me` · `PUT /api/auth/password`（改密后本人旧会话失效）· `POST /api/auth/totp/setup|enable|disable` |
| 账号管理 | `GET/POST /api/users` · `PUT /api/users/:id/password`（重置后联动吊销目标用户会话并停用其名下 API Key）· `DELETE /api/users/:id` |
| 会话管理 | `GET /api/sessions`（本人活跃会话）· `DELETE /api/sessions/:id`（踢指定会话）· `POST /api/sessions/revoke-others`（下线其余全部） |
| 渠道管理 | `GET/POST /api/channels` · `PUT/DELETE /api/channels/:id` · `POST /api/channels/:id/status`（启停）· `POST /api/channels/:id/test`（直发测试，先预留额度）· `POST /api/channels/:id/duplicate`（复制）· `POST /api/channels/:id/webhook-token`（送达事件 webhook 地址补生成/轮换） |
| 发送日志 | `GET /api/logs`（分页筛选）· `GET /api/logs/export`（CSV 下载）· `GET /api/logs/:id`（详情含正文）· `POST /api/logs/:id/resend`（快照重发，30s 防滥用）· `POST /api/logs/purge`（三模式清理） |
| 送达事件 | `GET /api/mail-events`（分页筛选）· `GET /api/mail-events/:id`（详情含原始报文）；入站摄取端点 `POST /webhooks/mail-events/:token`（公开，供邮件平台回调） |
| 抑制列表 | `GET/POST /api/suppressions` · `DELETE /api/suppressions/:id`（解除） |
| 邮件模板 | `GET/POST /api/templates` · `GET/PUT/DELETE /api/templates/:id` |
| API Keys | `GET/POST /api/keys`（含 `daily_quota`/`rate_per_sec`/回执配置）· `PUT/DELETE /api/keys/:id` · `POST /api/keys/:id/webhook/test`（回执连通性测试）· `POST /api/keys/:id/webhook-secret`（回执签名密钥轮换） |
| 统计 | `GET /api/stats/overview` · `GET /api/stats/daily?days=&channel_id=` |
| 通知中心 | `GET /api/notifications` · `GET /api/notifications/unread_count` · `POST /api/notifications/read` · `POST /api/settings/notify/test`（双通道连通性测试）· `POST /api/settings/notify/webhook-secret`（通知 Webhook 签名密钥生成/轮换） |
| 系统设置 | `GET/PUT /api/settings`（含通知开关、`log_body_enabled`、`backup_daily_enabled`、`suppression_auto_enabled`、`bounce_rate_threshold`、`weak_admin_password` 提示） |
| Webhook 投递 | `GET /api/webhook-deliveries`（outbox 投递记录排查：状态/尝试次数/最后错误/下次重试时间） |
| 审计 | `GET /api/audit`（按动作前缀/操作者筛选，分页） |
| 备份与数据 | `GET/POST /api/backups` · `GET /api/backups/:name/download` · `DELETE /api/backups/:name`（SQLite 专属，PG 返回 400） |
| 自动更新 | `GET /api/update/check` · `POST /api/update/apply`（未配置 `CATMAIL_UPDATE_REPO` 时返回 400） |

> 发送接口错误体对管理 API 同样适用（`{error:{type,message}}`）；未登录访问一律 401，配置白名单后白名单外来源 403。

---

*本文与 `backend/internal/docs/openapi.yaml`、`SPEC.md` 同步维护；发现不一致时以仓库内规范与源码为准并欢迎提 issue。*
