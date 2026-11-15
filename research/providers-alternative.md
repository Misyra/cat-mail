# Snowmail 调研:非主流 / 新兴 / 国内可用邮件发送平台

> 调研日期:2026-11-11。与主报告(Resend/SendGrid/Mailgun/Brevo/Mailjet/Postmark/SES/MailerSend/SMTP2GO/Elastic Email)互补。
> 免费额度与政策以官网当日页面为准,接入前请复核。

## 总览表

| 平台 | 免费额度 | 注册门槛 | 域名验证 | Go SDK | API 风格 | 大陆访问 |
|---|---|---|---|---|---|---|
| Loops.so | 4,000 封/30 天滚动窗口(营销+事务共享),1,000 联系人,10 req/s | 邮箱注册,无需信用卡 | 必须验证自有域名 | 社区库(tilebox/loops-go 等),无官方 | REST JSON + Bearer,接近 Resend | 可访问(Cloudflare,偶慢) |
| Plunk | 1,000 封/月 | 邮箱注册 | 云版需验证域名;可自托管 | 无官方(REST 简单) | REST + Bearer(sk_*),POST /v1/send | 可访问(EU 托管) |
| Notifuse | 自托管:3 工作区免费、不限量(自带 SES/SMTP);云版从 $16/月起,无免费 | 自托管需自有 AWS/SMTP | 用后端提供商的域名规则 | 平台本身是 Go 编写,REST | REST + Bearer,自带多提供商路由 | 自托管即可内网;云版一般可访问 |
| EmailJS | 200 请求/月,2 模板,无需信用卡 | 仅邮箱注册 | **无需自有域名**(可绑 Gmail/个人邮箱作为 service) | 无(REST + JS SDK) | 自定义 REST,POST api/v1.0/email/send,认证放 body | 可访问 |
| SendPulse SMTP | 12,000 封/月(50 封/小时,2 发送域名) | 注册后需提交 SMTP 开通申请表 | 必须域名(配置 SPF/DKIM) | 无官方 Go | POST api.sendpulse.com/smtp/emails,html 需 Base64 | 可访问 |
| Zoho ZeptoMail (CPaaS) | 首个 credit 免费(1 credit = 10,000 封,6 个月有效) | 注册后有人工审核(仅事务邮件定位) | 必须验证域名 | 无官方 Go(Node/Python/PHP/Java/C#) | REST,POST cpaas.zoho.com/v1.1/email | 可访问(境外 DC,偏慢) |
| Netcore (Pepipost) | 免费试用(历史为 30 天 3 万封);现主打询价 | 注册+试用审核 | 域名 SPF/DKIM | 官方含 Go(宣称 10+ 语言) | REST,POST emailapi.netcorecloud.net/v5/mail/send,api_key header | 未充分验证 |
| JangoSMTP | 无免费档(18,000 封/月起,需询价) | 联系销售 | 自有域名 SPF/DKIM | 无 | SMTP + API(以报表为主) | 可访问但产品已边缘化 |
| Mailtrap | **4,000 封/月(150/天),1 域名** | 邮箱注册,无需信用卡 | 正式发送需验证域名;sandbox 端点免域名(仅发到自己收件箱) | 官方多语言 SDK | REST + Bearer,POST send.api.mailtrap.io/api/send,**几乎=Resend 格式** | 可访问 |
| SparkPost (已被 Bird 收购) | **1,000 封/月(50/天)** | 邮箱注册 | 域名验证 | 历史官方 go-sparkpost | REST + API key | 可访问 |
| Maileroo | **3,000 发送/月 + 1,000 收信 + 100 测试**,2 域名 | 邮箱注册,无需信用卡 | 域名 DNS 验证 | 官方多语言 SDK(REST/SMTP) | 原生 REST,POST /emails/send 等 | 可访问(EU) |
| TurboSMTP (serversmtp) | **200 封/天(≈6,000/月)**,含 SMTP+API | 邮箱注册 | 以验证发件邮箱为主,无需整域名 | 无 | REST + SMTP | 可访问 |
| EngageLab(极光系) | **50 封/天** 免费 | 邮箱注册;支持支付宝/微信/USDT 付款 | 域名验证(自定义发信域) | 官方多语言 SDK | RESTful + SMTP,SendGrid 式心智模型 | **稳(中国团队,面向大陆可用)** |
| useSend(开源) | 自托管不限;云版 beta(定价页未公开免费档) | 自托管 Docker | 用自己的域名/SES | 无官方 Go | REST(hono),Resend 替代品定位 | 自托管可控 |
| Reloop(开源) | 云版 **6,000 封/月(200/天)**;自托管 Apache-2.0 | 邮箱注册 | 域名验证 | 无官方 | REST + SMTP + CLI/MCP,自研 MTA(KumoMTA) | 可访问 |
| 阿里云 DirectMail | **总量 2,000 封免费,每日最多免费 200 封** | 阿里云账号+实名(个人即可);无需备案(邮件域名) | 必须发信域名(5 个)+发信地址(100 个) | **官方 Go SDK(alibabacloud-go/dm)** | API + SMTP + 控制台;签名调用(RPC 风格) | **极稳** |
| 腾讯云 SES | **每账号 1,000 封免费(不限时用完为止)**,之后 0.0019 元/封 | 腾讯云账号+实名认证;模板需审核 | 必须发信域名 DNS 配置 | **官方 Go SDK(tencentcloud-sdk-go/ses)** | API + SMTP + 控制台(TC3 签名) | **极稳** |
| SendCloud(武汉闪达) | 注册即建测试域名/API_USER(仅测试);正式额度需认证+充值,报价不公开 | 注册后需实名/企业认证 | 测试域名仅限测试;正式必须自有域名 | 无官方 Go(PHP/Java/Python 官方) | REST + SMTP(API_USER+API_KEY 签名) | **稳** |
| SubMail 赛邮 | 无明确邮件免费额度(注册送的是短信);预付费 10 万封 ¥760(¥0.0076/封) | 注册+实名;试用入口 | 自有域名(SPF/MX)验证 | **官方 Go SDK** | RESTful + SUBHOOK 回调 | **稳** |
| 百度智能云 SES | **未发现该产品**(官网/搜索均无公开邮件推送产品页) | — | — | — | — | — |

## 各平台详情

### 国际新兴平台

**Loops.so** — loops.so。免费 4,000 封/30 天(与营销邮件共享配额),10 封/秒限速,带 "Powered by Loops" 页脚;Growth $49/月起(5,000 联系人,1,000 封/秒)。API:`https://app.loops.so/api/v1/transactional-emails`,Bearer token,简单 JSON;但事务邮件须先在后台创建/发布模板,不支持内联 HTML 直发,这是聚合层要特殊处理的点。无 CORS(仅服务端)。文档:https://loops.so/docs/api-reference

**Plunk** — useplunk.com,AGPL-3.0 开源,Docker 自托管,云版 EU 免费层 1,000 封/月,$0.001/封 按量,联系人不限量。API:`POST https://next-api.useplunk.com/v1/send`,Bearer sk_*,body `{to, subject, body}` 极简;另有 /v1/track(事件触发工作流)、/v1/verify(邮箱校验)。限速 1,000 req/min。文档:https://docs.useplunk.com/api-reference/overview

**Notifuse** — notifuse.com,开源(GitHub 2,277★,Go 编写),自带多提供商路由(可接 SES 等)。自托管 3 工作区免费且不限邮件量;云版 $16/月起无免费层。定位接近"邮件版统一网关",其自身路由/模板/工作流设计与 Snowmail 有重叠,可参考其设计。文档:https://docs.notifuse.com(OpenAPI,Bearer 认证)

**EmailJS** — emailjs.com。免费 200 请求/月(2 模板,7 天数据保留,超限直接丢弃不扣费)。核心卖点:**无需自有域名**——把 Gmail/Outlook/任意 SMTP 作为 "service" 绑定即可,用平台代理发送。API:`POST https://api.emailjs.com/api/v1.0/email/send`,参数 service_id/template_id/user_id(公钥)/accessToken(私钥)/template_params,认证放在 body(非 Header),限 1 req/s。文档:https://www.emailjs.com/docs/rest-api/send/

**SendPulse SMTP** — 免费档 12,000 封/月,限 50 封/小时、2 个发送域名,注册不需信用卡但 **SMTP 服务要先提交申请表人工激活**,并要求域名 SPF/DKIM。API:`POST https://api.sendpulse.com/smtp/emails`,html 字段要求 Base64 编码(body 包在 `{"email":{...}}` 里),OAuth2 换 token。文档:https://sendpulse.com/integrations/api/smtp

**Zoho ZeptoMail(现 Zoho CPaaS Email)** — zoho.com/zeptomail。纯事务邮件定位(禁营销),注册有人工审核。计费为 credit:1 credit = 10,000 封,首个 credit 免费、6 个月有效,无月费。API:`POST https://cpaas.zoho.com/v1.1/email`(字段 from_address/to/htmlcode,custom 格式),官方 SDK 无 Go。必须验证发信域名。文档:https://www.zoho.com/cpaas/

**Netcore Cloud(原 Pepipost,印度)** — netcore.ai/email-api。免费试用存在但页面改为询价制(历史为 30 天 30,000 封)。API:`POST https://emailapi.netcorecloud.net/v5/mail/send`,header 传 api_key;SDK 宣称 10+ 语言**含 Go**。文档:https://emaildocs.netcore.ai

**JangoSMTP** — jangosmtp.com。已无免费档("Plans start at 18,000 emails/month",Call for Pricing),仅保留 SMTP+报表 API。列为低优先级,不建议纳入聚合。

**Mailtrap** — mailtrap.io。免费 **4,000 封/月、150 封/天**、1 域名、3 天日志,无需信用卡;Basic $15/月 10K 封起。API:`POST https://send.api.mailtrap.io/api/send`,Bearer,body `{from:{email,name}, to:[{email}], subject, text/html}` —— **与 Resend POST /emails 几乎同构,适配成本最低**;另有 sandbox 端点 `sandbox.api.mailtrap.io/api/send/{inbox_id}` 免域名(只投递到自己的沙箱收件箱,不能发外部真实收件人)。文档:https://docs.mailtrap.io

**SparkPost(现属 Bird)** — bird.com/pricing/email。免费 **1,000 封/月、50 封/天**;付费为预充值余额按量($0.20–0.45/千封)。REST + API key;历史官方 Go 库(go-sparkpost)。域名验证要求同 SendGrid 系。品牌迁移中,聚合时注意其控制台/密钥体系已并入 Bird。

**Maileroo** — maileroo.com(新平台)。免费 **3,000 发送/月 + 1,000 收信/月 + 100 测试**、2 域名,无需信用卡;服务器在德/荷,99.95% SLA。原生 REST(send / templated / bulk / schedule + Inbound Routing + 测试 sink),官方多语言 SDK。文档:https://maileroo.com/docs

**TurboSMTP(serversmtp.com)** — 免费 **200 封/天(约 6,000/月)**,含 SMTP relay + Web API + 2 个 API key + SPF/DKIM;定价页提示免费档"前 6 个月"(FAQ 又称永久免费,以后台为准)。注册仅邮箱。以验证发件邮箱为主,无需整域名验证——**是海外平台里"无自有域名"自由度较高的选项**。文档:https://www.serversmtp.com

**EngageLab(中国背景,极光/Aurora Mobile 旗下)** — engagelab.com。免费 **50 封/天**;按量付费,支持**支付宝/微信支付/USDT**。RESTful + SMTP + 模板 + webhook,面向"中国团队发国际邮件"场景,大陆访问稳。文档:https://www.engagelab.com/docs(域名验证同 SendGrid 流程)

### 开源可自托管(Resend 替代定位)

- **useSend**(usesend.com,GitHub usesend/useSend 4,794★,AGPLv3):底层 AWS SES,事务+营销,REST(hono),自托管 Docker;云版 beta。适合作为 Snowmail 自托管兜底或代码参考。
- **Reloop**(reloop.sh,191★,Apache-2.0):自研 MTA(KumoMTA),云版免费 **6,000 封/月(200/天)**,REST+SMTP+CLI+MCP,自带 inbound。
- **Notifuse**(见上):Go 编写、自带多后端路由,与本项目定位最接近的开源形态。
- 这三家均定位 "Resend alternative",但**均未官方承诺 API 与 Resend 完全兼容**;接入前需逐字段核对。

### 国内平台

**阿里云邮件推送 DirectMail** — 免费额度:**主账户共 2,000 封,每日最多 200 封免费**;日发送配额初始 2,000 封/天(随信誉等级上调),按量/资源包付费。发信域名上限 5 个、发信地址 100 个;附件 15MB(API/SMTP)。无需网站 ICP 备案(邮件域名不涉备案),个人实名即可开通。API 为阿里云 RPC 签名风格 + 标准 SMTP,**官方 Go SDK 存在**(alibabacloud dm 2017-06-06 版)。大陆访问极稳,适合做国内通道兜底。文档:https://help.aliyun.com/zh/direct-mail/(规格限制:product-overview/limits)

**腾讯云 SES(简单邮件服务)** — 免费额度:**每账号 1,000 封,不限有效期用完为止**;超出 0.0019 元/封(按日结算)。需实名认证、发信域名 DNS 配置、**发信模板需审核通过才能发送**。API/SMTP/控制台三种接入,**官方 Go SDK**(tencentcloud-sdk-go 的 ses 模块)。大陆访问极稳。文档:https://cloud.tencent.com/document/product/1288/47930(价格)

**SendCloud(武汉闪达科技,sendcloud.net)** — 国内老牌代发平台(2013 年,API/SMTP)。注册即自动建好**测试域名 + 测试 API_USER + 模板**(仅限测试,官方明确禁止用于真实业务);正式使用需实名/企业认证并充值,**价格不公开**(需注册控制台或联系商务)。REST(API_USER + 签名)+ SMTP。大陆访问稳。文档:https://www.sendcloud.net/doc/product_email/quickin/

**SubMail(赛邮,mysubmail.com)** — 事务+营销邮件。**无明确邮件免费额度**(注册赠送的是短信),预付费套餐 ¥0.0076/封(10 万封 ¥760)起,量越大越低(500 万封 ¥0.005/封)。RESTful API + SUBHOOK 回调,**官方 Go SDK**(PHP/Java/C#/Python/Go/Ruby)。需自有域名(SPF/MX)。大陆访问稳。文档:https://www.mysubmail.com/documents

**百度智能云 SES** — **未找到该产品**:官网产品目录与搜索引擎均无"百度智能云邮件推送/SES"公开产品页(早期资料亦未见独立邮件发送产品)。建议从候选清单中移除。

## 关键问题 1:哪些平台的发送 API 模仿 Resend / SendGrid 格式?

结论:**没有发现任何一家提供官方 "SendGrid v3 兼容端点"**;Resend 格式的模仿者则有一批,但多停在"风格接近"而非"路径+字段完全兼容"。

接近 Resend 格式(Bearer + 扁平 JSON,适配层最薄):
1. **Mailtrap** — `POST https://send.api.mailtrap.io/api/send`,Bearer,body `{from:{email,name}, to:[{email}], subject, html/text}`。与 Resend `POST https://api.resend.com/emails` 仅差:from 是对象而非字符串、路径多一段。一个 ~30 行的适配器即可复用。
2. **Loops.so** — Bearer + 简单 JSON,但路径为 `/api/v1/transactional-emails` 且**必须预先注册模板、不能内联 HTML**,语义差异大,只能走"模板池"适配。
3. **Plunk** — Bearer + `{to, subject, body}`,字段名不同(subject/body vs subject/html),转换极其简单。
4. **Reloop / useSend** — 自称 Resend alternative,REST+SMTP,自托管时甚至可改造为原生 Resend 兼容;云版兼容性未官方承诺。
5. **Maileroo / SparkPost / Mailtrap 同代平台** — 均为"扁平 JSON + Bearer/API key"流派,写统一中间格式很自然。

SendGrid 阵营:EngageLab、Netcore 等在产品形态(SPF/DKIM、模板、webhook 事件)上模仿 SendGrid,但 **API 请求体没有一家照抄 SendGrid v3 的 `personalizations[]/content[]` 嵌套结构**;SendGrid 兼容层只能自己实现(即:Snowmail 对外暴露 SendGrid v3 格式,内部翻译到各家)。反倒是**国内阿里云(阿里 RPC 签名)、腾讯云(TC3 签名)** 的签名机制特殊,适配层要单独处理签名而不是 JSON 字段。

工程建议:内部统一格式选 **Resend 扁平 JSON**(字段最简、主流新平台都长得像它);对 Mailtrap/Plunk 几乎零成本,对 Loops/ZeptoMail 需要模板预注册映射,对阿里云/腾讯云重点封签名逻辑。

## 关键问题 2:无需自有域名即可对外发送的方案

| 方案 | 是否可用 | 限制 |
|---|---|---|
| **EmailJS** | ✅ 最佳 | 绑定 Gmail/Outlook/任意个人 SMTP 作为 service 即可,无需域名。免费 200 封/月、1 req/s、走模板。发件人显示为所绑邮箱;量大需付费。 |
| **TurboSMTP** | ✅ 较好 | 以"验证发件邮箱地址"为主,无需整域名 DNS 验证;免费 200 封/天。适合临时/小量发任意收件人。 |
| **SendCloud** | ⚠️ 仅测试 | 注册即送测试域名/API_USER,可立即发信,但官方明确禁止用于真实业务、额度小;正式发送必须认证 + 自有域名。 |
| **Mailtrap sandbox** | ❌ 不可对外 | sandbox 端点免域名,但只投递到 Mailtrap 自己的沙箱收件箱,发不了真实外部收件人;正式发送需验证域名。 |
| Loops/Plunk/Maileroo/Mailtrap/SparkPost/ZeptoMail/Netcore/SendPulse/EngageLab | ❌ | 全部要求验证自有域名(SPF/DKIM)。 |
| 阿里云 DirectMail / 腾讯云 SES / SendCloud / SubMail(正式) | ❌ | 必须发信域名;国内平台普遍还要实名认证(个人实名即可,DirectMail/SES 不强制企业资质),腾讯 SES 模板还要审核。 |

补充:各平台对"域名"的最低要求差异很大——SendGrid 系/国内云厂要求完整 DNS(SPF+DKIM+MX);TurboSMTP 这类只需要点验证邮件确认发件地址。**Snowmail 若要支持"用户无域名"模式,组合方案是:EmailJS(免费 200/月)+ TurboSMTP(200/天)做无域名档,其余平台全部走自有域名池。**

## 对 Snowmail 聚合的落地建议

1. **免费额度最大化组合**(全部无需信用卡):Mailtrap 4,000/月 + Reloop 6,000/月 + TurboSMTP ~6,000/月 + Maileroo 3,000/月 + SparkPost 1,000/月 + Loops 4,000/月 + Plunk 1,000/月 + EmailJS 200/月 ≈ **2.5 万封/月可用额度**,再叠腾讯 SES 1,000 封与阿里 DirectMail 2,000 封。
2. **国内通道**:阿里 DirectMail(官方 Go SDK、200/天免费)为首选;腾讯 SES 备选(模板审核是额外延迟);SendCloud 仅当需要国内投递率调优时接入(价格不透明)。
3. **大陆访问稳定性**:国内云厂(阿里/腾讯/SendCloud/SubMail)最稳;EngageLab 次之(中国团队、支持支付宝);海外平台中 EmailJS/Mailtrap/Loops 走 Cloudflare 一般可达,自托管 Notifuse/useSend + 自备 SMTP 是完全可控的兜底。
4. **适配优先级**:Mailtrap(准 Resend)→ Plunk → Maileroo → Loops(需模板映射)→ 阿里/腾讯(需签名封装)。

## 主要参考链接

- Loops: https://loops.so/pricing , https://loops.so/docs/api-reference
- Plunk: https://useplunk.com , https://docs.useplunk.com/api-reference/overview
- Notifuse: https://www.notifuse.com/pricing , https://docs.notifuse.com
- EmailJS: https://www.emailjs.com/pricing/ , https://www.emailjs.com/docs/rest-api/send/
- SendPulse SMTP: https://sendpulse.com/en/pricing/smtp , https://sendpulse.com/integrations/api/smtp
- Zoho ZeptoMail/CPaaS: https://www.zoho.com/zeptomail/ , https://www.zoho.com/cpaas/pricing.html
- Netcore: https://netcore.ai/email-api/
- Mailtrap: https://mailtrap.io/pricing/ , https://docs.mailtrap.io
- SparkPost/Bird: https://bird.com/pricing/email
- Maileroo: https://maileroo.com/pricing , https://maileroo.com/docs
- TurboSMTP: https://www.serversmtp.com/en/pricing
- EngageLab: https://www.engagelab.com/ , https://www.engagelab.com/pricing
- useSend: https://github.com/usesend/useSend
- Reloop: https://reloop.sh/
- 阿里 DirectMail: https://www.aliyun.com/product/directmail , https://help.aliyun.com/zh/direct-mail/product-overview/limits
- 腾讯 SES: https://cloud.tencent.com/product/ses , https://cloud.tencent.com/document/product/1288/47930
- SendCloud: https://www.sendcloud.net/doc/product_email/quickin/
- SubMail: https://www.mysubmail.com/mail
