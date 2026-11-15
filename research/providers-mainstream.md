# 国际主流事务性邮件发送平台调研报告

> 调研日期：2026-11-11（信息时效已在每节标注，免费额度政策变动频繁，接入前请以官方页面为准）
> 用途：Snowmail 聚合网关——统一 API + 按各平台剩余额度自动轮换路由

## 总览对比表

| 平台 | 免费额度 | 免费需绑卡 | API 认证风格 | 请求体 | 官方 Go SDK | 域名验证 | 注册门槛 |
|---|---|---|---|---|---|---|---|
| Resend | 3,000/月，100/天 | 否 | `Bearer re_xxx` | JSON | ✅ resend-go | 测试域名仅发给自己 | 极低 |
| SendGrid (Twilio) | 试用 60 天，100/天（无永久免费） | 否 | `Bearer SG.xxx` | JSON | ✅ sendgrid-go | 发件人验证即可发；域名认证推荐 | 低 |
| Mailgun | 100/天（沙盒域名仅限授权收件人） | 否（升级需卡） | Basic `api:KEY` | form-encoded | ✅ mailgun-go | 沙盒限授权收件人，生产须验证域名 | 中 |
| Brevo | 300/天（≈9,000/月） | 否 | `api-key: xxx` 头 | JSON | ❌ 官方无（Node/Py/PHP） | 发件人验证即可发；域名 DKIM/SPF 推荐 | 低 |
| Mailjet | 200/天，6,000/月 | 否 | Basic `KEY:SECRET` | JSON | ✅ mailjet-apiv3-go | 发件人+域名验证 | 低 |
| Postmark | 100/月（永久，Developer） | 否 | `X-Postmark-Server-Token` 头 | JSON | ❌ 官方无（社区库） | 新账号仅可发给自己，需验证发件人/域名 | 中（有审核） |
| Amazon SES | 3,000/月 × 12 个月（新账号）+ $200 AWS 通用额度 | 是（AWS 账号需绑卡） | AWS SigV4 签名 | JSON / MIME | ✅ aws-sdk-go-v2 | 默认沙盒：仅可发已验证地址，需人工申请生产权限 | 高 |
| MailerSend | 500/月（原 3,000，已缩减），100 API 请求/天 | **是** | `Bearer xxx` | JSON | ✅ mailersend-go | 须验证发件人；自定义域名推荐 | 中 |
| SMTP2GO | 1,000/月，200/天 | 否 | JSON 体 `api_key` 或 `X-Smtp2go-Api-Key` 头 | JSON | ✅ smtp2go-oss/go | 需验证发件人；未验域名 25 封/小时 | 中（强制手机短信验证） |
| Elastic Email | 3,000/月，100/天（API 产品独立账号） | 否 | `X-ElasticEmail-ApiKey` 头 | JSON | ✅ ElasticEmail/elasticemail-go | 需验证发件人域名/地址 | 低 |

> 注：Resend、Mailgun、MailerSend、Elastic Email 等均有各自的 sandbox/测试发件地址，但对"发给别人"普遍有限制，详见下文各节。

---

## 1. Resend

**信息日期：2026-11，resend.com/pricing、resend.com/setup.md、resend.com/docs/api-reference**

### 免费额度
- **3,000 封/月，100 封/天**（唯一有日上限的套餐）——信息源：resend.com/pricing（2026-11 实测抓取）
- 无需信用卡，官方 FAQ 明确"无试用，但有永久免费套餐"
- 超额行为：Free 套餐**没有**按量付费（PAYG），超出 3,000/月或 100/天后发送直接失败，需升级 Pro（$20/月起，50K 封）
- 附赠：1 个 webhook、10,000 次自动化运行、30 天数据保留、Broadcasts（营销群发）不计入事务性配额

### API 接入
- REST，Base URL `https://api.resend.com`（仅 HTTPS，无版本化方案）
- 认证：`Authorization: Bearer re_xxxxxxxxx`
- 请求体：JSON（`POST /emails`，字段 `from/to/subject/html`）；批量 `POST /emails/batch`（单次最多 100 封）
- 速率限制：默认 **10 req/s/team**，超限返回 429；需要 User-Agent 头（缺失返回 403 错误码 1010）
- 官方 Go SDK：**github.com/resend/resend-go**（官方维护，resend.com/llms.txt 2026-11 确认）
- 另有 SMTP 接入方式和 CLI
- 文档：https://resend.com/docs/api-reference

### 域名/发件人验证
- 未验证域名前：只能用 `onboarding@resend.dev` 作为发件人，且**只能发给自己注册邮箱或官方测试地址**——信息源：resend.com/setup.md（2026-11）
- 验证自定义域名需加 SPF/DKIM（建议同时加 DMARC）DNS 记录；支持 Vercel/GoDaddy 一键自动配置
- 全部套餐均可配置 DKIM/SPF/DMARC 与 open/click tracking

### 注册门槛
- 邮箱注册即可，无需信用卡、无手机验证、无人工审核（社区口碑：新域名首月进垃圾箱概率高，官方有专门指南）

### 特别限制
- 100/天日上限对聚合轮换是天然节流阀
- 营销（Broadcasts）与事务性邮件配额分开计
- 新账号 + 未验证域名 + 大量外发是典型风控触发点；官方建议新域名先预热（参考 guides/email/new-domain-emails-going-to-spam.md）

---

## 2. SendGrid (Twilio)

**信息日期：2026-11，twilio.com/en-us/products/email-api/pricing、twilio.com/docs/sendgrid**

### 免费额度 ⚠️ 已变
- **永久免费套餐已取消**：现在是 **60 天试用，100 封/天**（信息源：twilio.com SendGrid Email API pricing 页，2026-11）
- 注册无需信用卡（"Free trial / No credit card required"）
- 试用到期后必须升级（Essentials $19.95/月起，约 5 万封/月）
- 超额：试用期内日上限到达后当天不能再发；付费套餐超出按 overage 费率计费

### API 接入
- REST v3，Base URL `https://api.sendgrid.com/v3`
- 认证：`Authorization: Bearer SG.xxxx.yyyy`
- 请求体：JSON（`POST /mail/send`，`personalizations` 结构较复杂）；支持单请求多收件人
- 官方 Go SDK：**github.com/sendgrid/sendgrid-go**（官方，7 语言 SDK 之一）——信息源：twilio.com/docs（2026-11）
- 文档：https://www.twilio.com/docs/sendgrid/api-reference

### 域名/发件人验证
- 发件人验证（Sender Verification，验证单个邮箱）即可发送，不强制域名认证
- 域名认证（Domain Authentication）需加 CNAME/MX/TXT（automated security 开启时 3 CNAME + 1 TXT DMARC）；不做域名认证邮件会带 "via sendgrid.net" 且投递率差
- From 域名必须与已认证域名匹配

### 注册门槛
- 邮箱注册，无需信用卡；无需手机验证
- 新账号有一次性审核流程（通常自动通过）；免费试用若日发量异常可能触发人工复核

### 特别限制
- 试用 60 天后无法继续白嫖，这对聚合器是**重大缺点**（池子里账号会批量过期）
- 营销（Marketing Campaigns）与事务性（Mail Send）是两个产品线，配额分开
- unsubscribe 链接：事务性邮件不强制；营销邮件必须
- 发送速率：v3 API 无公开固定 RPS，超量返回 429

---

## 3. Mailgun (Sinch)

**信息日期：2026-11，mailgun.com/pricing、documentation.mailgun.com**

### 免费额度
- **100 封/天**（即约 3,000/月），1 个自定义发送域名、1 个 API key、1 天日志保留、单用户
- 信息源：mailgun.com/pricing（2026-11）
- 注册无需信用卡；付费套餐（Basic $15/1 万封、Foundation $35/5 万封、Scale $90/10 万封）有 30 天试用
- 超额：免费套餐无 PAYG；付费套餐超额 $1.10–$1.80/千封，月底出账

### API 接入
- REST，Base URL `https://api.mailgun.net/v3`（EU 区 `https://api.eu.mailgun.net/v3`）
- 认证：**HTTP Basic，username=`api`，password=API Key**（curl `--user 'api:KEY'`）；另有仅限单域发送的 Domain Sending Keys
- 请求体：**form-encoded**（`POST /v3/{domain}/messages`，字段 from/to/subject/text/html），返回 JSON
- 官方 Go SDK：**github.com/mailgun/mailgun-go**（官方，quickstart 中列出 Go 示例）——信息源：documentation.mailgun.com（2026-11）
- 速率限制：存在内部限流，超限 429；文档未公开具体数字
- 文档：https://documentation.mailgun.com/docs/mailgun/api-reference/

### 域名/发件人验证
- 新账号默认在**沙盒域名**（xxx.mailgun.org）上发送，且**只能发给 Authorized Recipients（需邮件点击验证的收件人）**——信息源：docs/quickstart（2026-11）
- 正式发送必须验证自定义域名（SPF/DKIM TXT 记录），EU 域名数据隔离在 EU 区
- 未加付款方式的试用账号历史上同样受 Authorized Recipients 限制（帮助中心文章已迁移，此条建议接入前实测）

### 注册门槛
- 邮箱注册，无信用卡可完成沙盒配置；升级生产需绑卡
- 有账号激活引导（guided setup）生成首个 API key

### 特别限制
- form-encoded 请求体与其它家 JSON 风格差异大，适配层要单独写
- 100/天偏少但域名验证流程成熟、对开发者友好
- 免费 1 天日志保留，重试/补发逻辑需自建事件落库

---

## 4. Brevo（原 Sendinblue）

**信息日期：2026-09/11，brevo.com/pricing.md（官方机器可读定价，生成于 2026-09-29）、developers.brevo.com**

### 免费额度
- **300 封/天**（≈9,000 封/月），无需信用卡（FAQ 原话："只需要邮箱和公司名"）
- 信息源：brevo.com/pricing 与官方 pricing.md（2026-09-29 生成）
- 付费：Starter $9/月（5,000 封）、Standard $18/月、Professional $499/月起
- 超额：免费套餐当日 300 封用完即停止（次日重置）；Starter 超额可购买不外流的 email credits（PAYG credits 不过期）

### API 接入
- REST，Base URL `https://api.brevo.com/v3`
- 认证：**自定义头 `api-key: YOUR_API_KEY`**（非 Bearer）；也支持 OAuth2
- 请求体：JSON（`POST /v3/smtp/email`）
- 速率限制（官方 api-limits 页，2026-11 抓取）：事务发送 `POST /smtp/email` 一般档 **1,000 RPS / 360 万 RPH**，专业档更高——是所有平台里最宽松的
- **官方 SDK 仅 Node/Python/PHP，无官方 Go SDK**（cURL/自写 HTTP 封装即可，接口简单）
- 文档：https://developers.brevo.com

### 域名/发件人验证
- 必须先验证发件人邮箱；自定义域名验证（DKIM/SPF/DMARC）所有套餐可用，强烈建议
- 未验证域名也可发，但投递率和品牌信任差

### 注册门槛
- 邮箱注册即可，无信用卡、无强制手机验证
- 免费套餐有营销联系人上限（免费 100,000 contacts 条目出现在定价 API 中，实际免费档通常受限，以 dashboard 为准）

### 特别限制
- 300/天是全局日限（含营销+事务），对聚合器来说免费池容量尚可但不可突发
- 营销邮件必须含退订链接（免费套餐邮件页脚有 Brevo logo）
- 超额不会自动扣费，适合"只白嫖"的池子成员
- 限流极宽松，适合突发事务流量

---

## 5. Mailjet

**信息日期：2026-11，mailjet.com/pricing、dev.mailjet.com**

### 免费额度
- **200 封/天，6,000 封/月**，1,000 联系人，无需信用卡
- 信息源：mailjet.com/pricing（2026-11）
- 付费：Starter $9/月（8,000 封，无日限）、Essential $19/月（15,000 封）
- 超额：可继续发送但按"每 1,000 封"加价出账；未用完的月度额度不结转

### API 接入
- REST，Send API v3.1，Base URL `https://api.mailjet.com/v3.1`
- 认证：**HTTP Basic，username=API Key，password=Secret Key**
- 请求体：JSON（`POST /send`，`Messages[]` 数组，单请求最多 50 条消息）
- 官方 Go SDK：**github.com/mailjet/mailjet-apiv3-go**（官方长期维护）
- 文档：https://dev.mailjet.com

### 域名/发件人验证
- 必须验证发件人地址；自定义域名 SPF/DKIM 验证强烈推荐（法国公司，EU 数据）
- 免费档邮件有 Mailjet 品牌 footer

### 注册门槛
- 邮箱注册，无信用卡（预充值账户需 $50 起）；付款方式为信用卡或电汇
- 法国主体，注册流程无手机验证要求；新账号可能有人工风控审核（社区反馈偶发）

### 特别限制
- 200/天日限 + 免费档营销功能弱（定位偏营销，事务性也支持）
- 营销邮件需退订链接（有内置 unsubscribe 处理）
- 未验证域名/新账号发送量骤增会触发人工审核

---

## 6. Postmark

**信息日期：2026-11，postmarkapp.com/pricing、postmarkapp.com/developer/api/overview**

### 免费额度
- **Developer 免费：100 封/月，永不过期**（明确"no trial time limit"），无需信用卡
- 信息源：postmarkapp.com/pricing（2026-11）
- 超额：免费档**不允许 overage**，用完即止；付费 Basic $15/月（1 万封，超量 $1.80/千封）、Pro $16.50/月、Platform $18/月（无限域名/服务器）
- 无 100–10,000 封之间的中间档

### API 接入
- REST，Base URL `https://api.postmarkapp.com`
- 认证：**自定义头 `X-Postmark-Server-Token: xxx`**（发送）；账户级操作用 `X-Postmark-Account-Token`
- 请求体：JSON（`POST /email`，单请求 ≤10MB；batch ≤500 封/请求）；`POSTMARK_API_TEST` 作为 token 可做沙盒测试不真实发送
- 错误处理规范：422 输入错误 + `ErrorCode` 数字码，429 限流
- **无官方 Go SDK**，官方列出社区 Go 库（community libraries 页）——信息源：developer/api/overview（2026-11）
- 文档：https://postmarkapp.com/developer

### 域名/发件人验证
- 每个 Server 必须先配置发件人签名（Sender Signature，验证单邮箱）或验证发送域名（DKIM/SPF）
- 新账号/未确认（Unconfirmed）状态下发送受限（历史上仅可发给账号自身邮箱；生产前需完成 onboarding），接入前以控制台提示为准
- 主打事务性邮件，对营销内容态度严格

### 注册门槛
- 邮箱注册，无信用卡
- 有账号确认/审核流程（新账号发送量突增会有人工审查），审核响应较快但存在

### 特别限制
- 100/月免费额度最小，但对"聚合池最后一个备用节点"仍可用
- 明确的事务性专注：禁止群发营销（TOS 层面），营销退订链接不适用
- 批量上限 500 封/请求，错误码体系清晰，轮换网关实现错误处理简单

---

## 7. Amazon SES

**信息日期：2026-11，aws.amazon.com/ses/pricing、docs.aws.amazon.com/ses/latest/dg/request-production-access.html**

### 免费额度
- 新账号：**每月 3,000 条消息免费，为期 12 个月**（自开始使用 SES 起算）——信息源：AWS SES FAQ（2026-11）
- 2025-07-15 起新 AWS 账号另有 $200 AWS Free Tier 通用额度可用于 SES（免费方案限开通后 6 个月）
- 旧的"62,000 封/月（EC2 内发送）"永久免费额度已不在现行文档中（老账号可能保留）
- 超额/价格：按量 **$0.10/千封** + 附件数据传输 $0.12/GB（约 32KB/封再摊一点），仍是全场最便宜；新定价页另出现 Essentials/Pro/Enterprise 套餐模式（Essentials $0.16/千封）
- **AWS 账号必须绑定信用卡**才能创建

### API 接入
- REST + AWS SDK 风格：**AWS Signature V4 签名**（不是简单 key 头），JSON 请求体（SESv2 `POST /v2/email/outbound-emails`）或原始 MIME（SendRawEmail）
- 也支持 SMTP 接口（用户名密码形式，便于复用 SMTP 逻辑）
- 官方 Go SDK：**github.com/aws/aws-sdk-go-v2/service/sesv2**（官方一等公民，聚合网关用它最顺）
- 文档：https://docs.aws.amazon.com/ses/

### 域名/发件人验证
- **新账号默认沙盒**：只能发给已验证的邮箱/域名 + SES 邮箱模拟器，限 200 封/24h、1 封/秒
- 退出沙盒需申请生产权限：填写邮件类型（Transactional/Marketing）、网站 URL、退信/投诉处理承诺；**人工审核，首次响应 ≤96 小时**——信息源：docs（2026-11）
- 发件 From 必须是已验证 identity；自定义域名验证 = DNS TXT/CNAME（DKIM）

### 注册门槛
- AWS 账号：信用卡 + 手机验证（OTP）必需；部分地区账号注册审核偶发
- SES 生产权限人工审核（多数一天内，最长 96h+）

### 特别限制
- 无内置退订管理（营销退订需自建或用 VDM/配置集）
- 事务/营销在申请生产权限时需声明类型；发送行为与声明不符会被限制
- 弹性发送配额（sending quota）随信誉爬升，初始生产额度可能较低
- 接入复杂度最高（SigV4/region/凭证链），但对聚合器来说免费额度可靠、单价极低，值得做"高流量兜底"

---

## 8. MailerSend

**信息日期：2026-11，mailersend.com/pricing、developers.mailersend.com**

### 免费额度 ⚠️ 已缩水
- **500 封/月**（此前长期为 3,000/月，2025-2026 年间已下调），限制：1 域名、1 模板、1 API token、1 webhook、100 API 请求/天、1 天日志
- **免费套餐也要求绑信用卡**（官方解释为防滥用，不扣费）——信息源：mailersend.com/pricing FAQ（2026-11）
- 另有 14 天全功能试用（500 封/月，到期自动降为 Free）
- 超额：免费档为硬上限，需升级；付费 Hobby $7/月（5,000 封）、Starter $68/月（10 万封）档超额 $1.30/千封继续发
- 每月配额按 30 天滚动重置

### API 接入
- REST，Base URL `https://api.mailersend.com/v1`
- 认证：`Authorization: Bearer xxx`（token 按发送域生成，可带 scope）
- 请求体：JSON（`POST /v1/email`）；批量 `POST /v1/bulk-email`
- 速率限制（官方文档，2026-11）：发送 **120 req/分钟**，其他端点 60 req/分钟，bulk 10 req/分钟；另有每日 API 请求配额（Free 档 100/天）+ 月度 overage 上限（触顶返回 422）
- 官方 Go SDK：**github.com/mailersend/mailersend-go**（官方）——信息源：developers.mailersend.com（2026-11）
- 文档：https://developers.mailersend.com

### 域名/发件人验证
- 必须验证发件人；自定义域名 DKIM/SPF/DMARC 验证可用
- 试用/免费档只能给 1 个域名，规模受限

### 注册门槛
- 邮箱注册 + **信用卡**（免费档也要）；无手机验证
- MailerSend 是 Brevo 旗下事务性品牌，风控相对成熟

### 特别限制
- 免费 500/月 + 100 API 请求/天双重节流，聚合价值有限
- 事务性定位（与 Brevo 营销互补），营销邮件不鼓励
- Bearer + JSON + 官方 Go SDK，接入成本低

---

## 9. SMTP2GO

**信息日期：2026-11，smtp2go.com/pricing、developers.smtp2go.com**

### 免费额度
- **1,000 封/月，200 封/天**，永久免费（"for as long as you like"），无需信用卡
- 信息源：smtp2go.com/pricing + developers.smtp2go.com rate-limiting 文档（2026-11）
- 未验证发件域名时另有 **25 封/小时** 限制
- 超额：免费档到顶即止（无 overage）；付费 Starter $10/月（1 万封，超量 $1/千封）、Professional $75/月（10 万封）
- 免费档电话/在线客服支持仅 14 天

### API 接入
- REST，Base URL `https://api.smtp2go.com/v3`（`POST /email/send`）
- 认证：**JSON 请求体内嵌 `api_key` 字段**，或自定义头 `X-Smtp2go-Api-Key`（非 Bearer/Basic）
- 请求体：JSON（senders/recipients/content 等）；另支持 MIME 字符串与批量
- 官方 Go 库：**github.com/smtp2go-oss/smtp2go-go**（官方 OSS）——信息源：official-libraries 文档（2026-11）
- 限流：`/activity/search` 60 req/分钟；错误请求过多会封 IP ≥1 分钟；可在 API key 上自定义发送速率上限
- 文档：https://developers.smtp2go.com

### 域名/发件人验证
- 必须验证发件邮箱；验证发件域名（DKIM/SPF 自动化配置）后解锁 25/小时的限制
- 全部套餐含 DKIM/SPF 自动化

### 注册门槛 ⚠️
- **强制手机短信验证码** + 邮箱激活
- **拒绝公共邮箱注册**（Gmail/Yahoo 等直接被拒），必须是自有域名的工作邮箱
- 30 天不满意全额退款承诺

### 特别限制
- 新付费账号审核期内临时限 1,000 封/天，超出排队
- 事务/营销均可；退订管理可用其 unsubscribe 功能
- 手机验证 + 拒绝公共邮箱意味着"批量开号建池"在此平台不可行，只适合正规接入 1-2 个账号

---

## 10. Elastic Email

**信息日期：2026-11，elasticemail.com/pricing、elasticemail.com/email-api**

### 免费额度
- **Email API 产品（与 Email Marketing 分开注册）：3,000 封/月，100 封/天**，无需信用卡（仅需邮箱验证）
- 信息源：elasticemail.com/email-api（2026-11）
- 付费：Starter $19/月（5 万封，≈$0.38/千封），订阅制无 PAYG；超出必须升档
- Email Marketing（另一个产品线）免费档为 1,000 联系人 / 3,000 封/月，两产品**账号不通用**

### API 接入
- REST，Base URL `https://api.elasticemail.com/v4`（`POST /emails/transactional`）
- 认证：**自定义头 `X-ElasticEmail-ApiKey: xxx`**（"apikey" 授权方案）
- 请求体：JSON
- 官方 Go SDK：**github.com/ElasticEmail/elasticemail-go**（官方，GitHub org 下还有 C#/Python/PHP/Java/JS/Ruby 等共 12 语言）——信息源：api-documentation 页（2026-11）
- 文档：https://elasticemail.com/developers/api-documentation

### 域名/发件人验证
- 需验证发件人地址；自定义域名 SPF/DKIM/DMARC 支持完善（自研 MTA）
- 免费档日志保留 3 天

### 注册门槛
- 邮箱验证即开，无信用卡、无手机验证，"instant setup"
- 低价定位，风控相对宽松，但垃圾投诉会快速封号

### 特别限制
- 营销产品必须含退订链接；API 产品的事务性发送不强制
- 免费档 100/天限速；自动升档（auto-increase）默认可能触发付费，注意关闭
- 单价便宜的口碑带来一定的送达率/信誉参差评价，聚合器中适合作为低价付费兜底而非主路由

---

## 补充发现（调研过程中确认的横向信息）

1. **SendGrid 免费永久套餐已死**（2026-11 官方定价页）：新签只有 60 天试用 100/天——老攻略里"SendGrid 100/day forever"已失效。
2. **MailerSend 免费从 3,000/月 缩到 500/月** 且免费档也要绑卡（2026-11 定价页）。
3. **AWS Free Tier 2025-07 重构**：老 SES 免费额度体系被替换为"$200 通用额度 + SES 前 12 个月每月 3,000 条"，老账号的 62,000/月（EC2）是否保留取决于账号时点。
4. 各家对"未验证域名只能发给自己"的沙盒设计高度一致（Resend / Mailgun / SES / Postmark），Snowmail 的域名引导流程必须是一级功能。
5. 认证风格三种流派：Bearer（Resend/SendGrid/MailerSend）、自定义头（Brevo api-key / Postmark X-Postmark-Server-Token / Elastic X-ElasticEmail-ApiKey / SMTP2go）、Basic 或签名（Mailgun api:KEY、Mailjet KEY:SECRET、SES SigV4）。统一网关的适配层按"凭证 → 头构造器"抽象即可全覆盖。
6. 请求体两种流派：JSON 主流；**Mailgun 是 form-encoded** 例外，需单独序列化。
7. 官方 Go SDK 覆盖：Resend ✅ / SendGrid ✅ / Mailgun ✅ / Mailjet ✅ / SES ✅（aws-sdk-go-v2）/ MailerSend ✅ / SMTP2GO ✅（OSS）/ Elastic Email ✅ / Brevo ❌ / Postmark ❌（仅社区库）。Brevo 与 Postmark 接口简单，自封装 HTTP client 约 100-200 行即可。

---

## 适合 Snowmail 聚合的 Top 5 推荐

**推荐名单（按优先级）：Resend、Brevo、Mailgun、Resend 之外的第二梯队 → SMTP2GO、Mailjet**

1. **Resend** —— 3,000/月 + 100/天、零门槛注册、官方 resend-go、10 req/s 限流友好、onboarding@resend.dev 测试域名让"先跑通再绑域名"成为可能。风控宽松、开发者口碑最好，是免费池的首选主力。
2. **Brevo** —— 300/天（≈9,000/月）全场最大免费日额度、无信用卡、1,000 RPS 的宽松限流、超额自动停止（不会产生费用）。缺点是没有官方 Go SDK，但 API 极简（api-key 头 + JSON），自封装成本低。
3. **Mailgun** —— 100/天 + 成熟的域名验证与 API 体系、官方 mailgun-go、US/EU 双区域。form-encoded 是适配成本，但引擎成熟、事件 webhooks 完善，免费池里做稳定节点。
4. **SMTP2GO** —— 1,000/月、200/天，额度比 Resend 月度少但日限更松（200/天），官方 OSS Go SDK。缺点是手机验证 + 拒绝公共邮箱，无法批量建号，只能作为正规接入的单节点；但服务稳定性口碑好。
5. **Mailjet** —— 200/天 + 6,000/月、无信用卡、官方 mailjet-apiv3-go、JSON + Basic 认证接入简单。作为第四/五节点补足每日额度池。

**备选/不建议作为免费池主力：**
- **Amazon SES**：注册+生产审核门槛最高（绑卡+96h 人工审核），不适合做"免费快池"，但建议作为**付费兜底路由**（$0.10/千封 + 官方 aws-sdk-go-v2），在所有免费池耗尽时接管。
- **Elastic Email**：3,000/月 + 官方 Go SDK，注册零门槛，可作第六节点；但送达率口碑一般、超额即升档付费，注意关 auto-increase。
- **SendGrid**：60 天试用到期即死，不适合长期免费池；付费档价格也偏高。
- **Postmark**：100/月太小，仅适合做"最后一个救命节点"；其对营销内容的严格 TOS 要求聚合器明确标注事务性用途。
- **MailerSend**：500/月 + 100 API 请求/天 + 免费档绑卡，三重限制下聚合价值最低。

**轮换路由设计提示**（基于以上调研）：
- 日限型平台（Resend 100/天、Brevo 300/天、Mailjet 200/天、SMTP2GO 200/天）用"日窗口计数器"轮换；月限型（Mailgun 100/天也偏日限、Postmark 100/月、MailerSend 500/月）用"月配额余量表"。
- 统一错误处理：429 + Retry-After（MailerSend/Brevo/SES）是标准退避信号；422（MailerSend overage / Postmark 输入）要区分"配额尽"与"请求错"。
- 域名验证引导：给用户一个"一次配置，全平台分发 DNS 记录"的向导（SPF 多平台合并是坑点：SPF 只能一条记录，多平台需 include 合并；DKIM 各自独立 CNAME/TXT 无冲突）。
