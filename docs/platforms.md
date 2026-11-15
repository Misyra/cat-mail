# 渠道接入指南

> 本文迁自 README，面向把 CatMail 渠道接上真实邮件平台的用户。发送 API 的调用方式见 [docs/API.md](./API.md)；部署与快速开始见 [README](../README.md#快速开始)。


## 支持的平台与默认额度

| 平台 | 需要的凭据 | 是否需要验证发送域名 | 默认额度（0 = 不限） |
|---|---|---|---|
| Resend | API Key | 是（SPF/DKIM） | 100 封/日，3000 封/月 |
| Brevo | API Key | 是（SPF/DKIM） | 300 封/日 |
| Mailjet | API Key + Secret | 是（SPF/DKIM） | 200 封/日，6000 封/月 |
| SMTP2GO | API Key | 是（SPF/DKIM） | 200 封/日，1000 封/月 |
| Mailgun | API Key + 发送域名（region：us/eu） | 是（SPF/DKIM/接收 MX） | 100 封/日 |
| Elastic Email | API Key（Email API 产品，与营销产品账号不通用） | 是（验证发件人/域名，SPF/DKIM） | 100 封/日，3000 封/月 |
| Mailtrap | API Token | 是（正式发送需验证域名；沙箱端点免域名但仅发给自己收件箱） | 150 封/日，4000 封/月 |
| 阿里云邮件推送 | AccessKeyId + AccessKeySecret + 发信地址 | 是（发信域名 + 发信地址均需在控制台验证） | 200 封/日，2000 封/月（免费总量；超额转按量计费） |
| SMTP | 任意 SMTP 服务（host/port/user/pass/TLS） | 取决于服务商 | 不限 |
| Mock | 无（可选：API Key 填魔法值模拟失败） | 否 | 本地模拟，用于测试；API Key 设为 `fail` / `fail_rate_limit` / `fail_auth` 可分别模拟 5xx（可重试）/ 429（冷却）/ 401（自动禁用），便于本地验证 failover |

上表额度仅为 CatMail 的**调度兜底默认值**（写入 `backend/internal/provider/defaults.go`），可在渠道表单中覆盖；实际以平台侧限制为准。

## 接入步骤

1. **平台注册并获取凭据**：在所选平台注册账号（每平台一个），在控制台创建 API Key；Mailjet 另有 Secret Key，Mailgun 需添加并验证发送域名，Elastic Email 请认准 Email API 产品（与 Email Marketing 账号不通用），Mailtrap 渠道填 API Token，阿里云邮件推送填 AccessKeyId/AccessKeySecret 并在控制台验证发信域名与发信地址。
2. **管理后台新建渠道**：「渠道管理」→「新建渠道」，填写：
   - 名称（轮换与 `channel` 参数用）、平台类型、**优先级**（数字越小越优先）
   - API Key（Mailjet 填 Secret、Mailgun 填域名与 region）
   - 默认发件人 `from_email` / `from_name`（可选；请求未带 `from` 且该渠道配置了默认发件人时生效——轮换路径与显式指定 `channel` 均适用）
   - 额度覆盖 `daily_limit_override` / `monthly_limit_override`（0 = 用平台默认）
   - 可选：允许的发件域名 `allowed_domains`（逗号分隔）、出站代理 `proxy_url`（`http://` 或 `socks5://`）
3. **测试发送**：渠道列表「测试」按钮，输入收件邮箱，直发该渠道（不走轮换），返回耗时与错误详情。测试与轮换路径同口径**先预留额度**：平台受理成功则保留扣减、失败自动返还；渠道当日/当月额度已用尽时测试不执行（提示 429，不落日志）。测试失败不会触发渠道禁用/冷却。注意：测试固定使用渠道的默认发件人，故渠道需先配置 `from_email`。
4. **创建 API Key**：「API Keys」页新建，明文 key **仅显示一次**，用于调用发送 API。

## 发送域名与 DNS（SPF/DKIM）

多数平台要求验证发信域名（配置该平台指定的 SPF/DKIM DNS 记录）。建议**为每个平台分配一个独立子域**，例如：

- `r.mail.example.com` → Resend
- `b.mail.example.com` → Brevo
- `m.mail.example.com` → Mailgun

这样各平台的 DNS 记录互不冲突（SPF 单域记录数有限、DKIM 密钥各不相同），同时可在 CatMail 中把渠道的 `allowed_domains` 绑定到对应子域，避免发件域名与渠道不匹配被平台拒信。

> 常见事务性平台中，仅 **TurboSMTP**（验证发件邮箱即可）与 **EmailJS**（无需自有域名）支持免域名发信，当前 CatMail 未内置这两个平台的适配器；其余内置平台均建议完成域名验证后使用。

## 接入真实平台清单

本节面向第一次把 mock 渠道换成真实 API Key 的用户：先用首启自带的 mock 渠道走通全流程，再接第一个真实平台验证，最后配齐多平台享受 failover。各平台免费额度政策可能随时变化（见[README「合规与使用须知」](../README.md#合规与使用须知)），下文额度为调研时（2026-11）的口径，接入前请以官方页面为准。

### 推荐接入顺序

**第 1 步：用 mock 渠道走通全流程（无需任何平台账号）**

首启自带的 mock 示例渠道「示例渠道」本地模拟发送、不产生真实网络请求（默认发件人 `sender@example.com`）。只需：

1. 「API Keys」页创建一个 Key（`cm-` 开头，明文仅显示一次）；
2. 按 [docs/API.md「快速开始」](./API.md#2-快速开始) 的 curl 示例调用 `POST /v1/send`（`from` 可省略，回退渠道默认发件人）。

验收点：

- 响应 `"accepted": true`，`provider_message_id` 为 `mock-` 开头；
- 「发送日志」出现该条记录（详情面板可预览正文），「渠道管理」中示例渠道当日用量 +1。

可选 failover 演练——把 mock 渠道的 API Key 换成魔法值即可模拟平台故障（见「支持的平台与默认额度」表 Mock 行）：

- 新建第二个 mock 渠道，API Key 填 `fail_auth`、优先级数字调小（更优先），再发一封：请求仍成功（自动换到「示例渠道」），`fail_auth` 渠道徽章变为红色「自动禁用」，原因显示平台认证/授权失败（HTTP 401）；
- API Key 填 `fail_rate_limit` → 渠道徽章变为黄色「冷却」，默认 60 秒后自动恢复。

**第 2 步：接入第一个真实平台**

推荐从 **Resend** 开始（邮箱注册即可、无需信用卡）。注意未验证域名前只能用 `onboarding@resend.dev` 发件且**仅能发给你自己的注册邮箱**——首次验证时收件邮箱填注册邮箱即可。

1. Resend 控制台「API Keys」页创建 API Key；
2. 「渠道管理」→「新建渠道」：平台选 Resend、填 API Key、配置默认发件人 `from_email`（测试发送必需），保存；
3. 渠道列表点「测试」，输入收件邮箱直发该渠道（不走轮换）。

验收点：

- 测试返回成功与耗时；收件箱收到邮件，且 **Resend 控制台的发送记录（Logs）页可见该封**；
- 「发送日志」出现带「测试」标记的记录；渠道当日用量 +1（测试发送与正式发送同口径计额度）；
- 再用 `/v1/send` 不带 `channel` 参数发一封，确认响应的 `channel` 为该渠道名——轮换路径也打通了。

**第 3 步：配齐多平台，验证 failover 轮换**

按同样流程再接 1–2 个平台（Brevo / Mailjet / SMTP2GO 同样无需信用卡），发信域名按上文「发送域名与 DNS」的建议为每个平台分配独立子域，并在渠道上绑定 `allowed_domains`；优先级按「主用数字小、备用数字大」设置。

验收点（故意制造额度耗尽，观察轮换切换）：

- 编辑主渠道，把「日额度覆盖」改为 `1`，连续调用两次 `/v1/send`：第 2 封响应的 `channel` 应变为下一优先级渠道，「发送日志」两条记录的渠道不同，主渠道用量显示 1/1；
- 把额度覆盖改回后，主渠道重新回到轮换首位；
- 也可继续用 mock 渠道复现 429 冷却 / 401 自动禁用（见第 1 步），确认徽章与恢复行为符合预期。

### 逐平台接入要点

「免费额度（官方）」为调研时免费档口径；「默认额度」为 CatMail 的调度兜底值（与 `backend/internal/provider/defaults.go` 一致，可在渠道表单覆盖）。所有 HTTP 平台共用同一套错误分类：**401/403 → 自动禁用**；**429 → 冷却（默认 60 秒）并换渠道**；402/5xx/网络错误 → 记连续失败（默认阈值 5 次禁用）并换渠道；其余 4xx 按参数错误返回调用方（不换渠道、不改渠道状态）。

| 平台 | Key 获取入口 | 域名 / 发件人验证 | from 地址约束 | 免费额度（官方）↔ 默认额度 | 失败 / 限流在本系统的表现 |
|---|---|---|---|---|---|
| Resend | 控制台「API Keys」页创建（`re_` 开头，仅显示一次）；[API 文档](https://resend.com/docs/api-reference) | 控制台「Domains」添加域名并按指引配置 SPF/DKIM DNS 记录；未验证域名只能用 `onboarding@resend.dev` 且仅发给自己 | 必须是已验证域名下的地址（测试期 `onboarding@resend.dev` 仅限发给自己） | 100/日、3,000/月 ↔ 100/日、3000/月 | 限流（默认 10 req/s）返回 429 → 冷却；401/403（Key 失效或被封禁）→ 自动禁用 |
| Brevo | 账户菜单「SMTP & API」页创建 API Key；[API 文档](https://developers.brevo.com) | 必须先在控制台验证发件人邮箱；域名 DKIM/SPF/DMARC 推荐配置（不强制，但未验证投递率差） | 必须是控制台已验证的发件人邮箱（或已验证域名下地址） | 300/日（月度无硬上限）↔ 300/日、月不限 | **额度耗尽返回 400 + `not_enough_credits`，本系统按限流同款处理**：进入冷却并换渠道；429 → 冷却 |
| Mailjet | 控制台「Account」页同时查看 API Key 与 Secret Key，分别填入渠道的 API Key 与 Secret 输入框；[API 文档](https://dev.mailjet.com) | 必须验证发件人地址；域名 SPF/DKIM 强烈推荐；免费档邮件带 Mailjet 页脚 | 必须是已验证的发件人地址（或已验证域名下地址） | 200/日、6,000/月 ↔ 200/日、6000/月 | 401（Key/Secret 错误）→ 自动禁用；429 → 冷却 |
| SMTP2GO | 控制台「Settings → API Keys」创建；[API 文档](https://developers.smtp2go.com) | 必须验证发件邮箱；**未验证发件域名时额外限 25 封/小时**，验证域名（DKIM/SPF）后解锁；注册强制手机验证且拒绝公共邮箱 | 必须是已验证发件邮箱或已验证域名下地址 | 200/日、1,000/月 ↔ 200/日、1000/月 | 401 → 自动禁用；429 / 超限 → 冷却 |
| Mailgun | 控制台「Settings → API Keys」查看；渠道需同时填发送域名与 region（us/eu）；[API 文档](https://documentation.mailgun.com/docs/mailgun/api-reference/) | 新账号默认在沙盒域名（`xxx.mailgun.org`）发信，**仅能发给已验证的 Authorized Recipients**；正式发送必须添加并验证自定义域名（SPF/DKIM TXT） | 必须是已验证发送域名下的地址（沙盒阶段仅限授权收件人） | 100/日 ↔ 100/日、月不限 | 401 → 自动禁用；429 → 冷却；域名/收件人未验证等 4xx 按参数错误返回调用方 |
| Elastic Email | 控制台「Settings → API Keys」创建（Email API 产品，与营销产品账号不通用）；[API 文档](https://elasticemail.com/developers/api-documentation) | 需验证发件人；自定义域名支持 SPF/DKIM/DMARC（自研 MTA，配置完善） | 必须是已验证发件人/域名下地址 | 100/日、3,000/月（Email API 免费档）↔ 100/日、3000/月 | 401/403 → 自动禁用；429 → 冷却；免费档 100/日限速明显，超额 4xx 按参数错误返回 |
| Mailtrap | 控制台「Sending → API Tokens」创建；[API 文档](https://docs.mailtrap.io) | 正式发送必须验证发送域名（SPF/DKIM）；沙箱端点免域名但只投递到自己的沙箱收件箱（CatMail 使用正式端点） | 必须是已验证域名下地址 | 150/日、4,000/月、1 域名 ↔ 150/日、4000/月 | 401（Token 无效）→ 自动禁用；429 → 冷却；域名未验证等 4xx 按参数错误返回 |
| 阿里云邮件推送 | 控制台创建发信域名与发信地址，AccessKey 管理页获取 AccessKeyId/AccessKeySecret（渠道 API Key 填前者、Secret 填后者）；[API 文档](https://help.aliyun.com/document_detail/294597.html) | 必须验证发信域名（SPF 等 DNS 记录）与发信地址；需阿里云账号实名（个人即可，邮件域名无需备案） | 必须是控制台验证过的发信地址（渠道需配置 `from_email`，适配器以其为 AccountName） | 每日 200 封免费、免费总量 2,000 封；超额转按量计费 ↔ 200/日、2000/月 | 签名/凭据错误 → 自动禁用；发信地址未验证等业务错 4xx 按参数错误返回；**不支持附件**（headers 丢弃、tags 需控制台预创建不透传），带附件请求会被平台拒收按参数错误返回 |
| 通用 SMTP | 无统一入口：取决于你的 SMTP 服务商（邮局控制台 / 自建 MTA），渠道填 host/port/用户名/密码/TLS 模式 | 取决于服务商（一般要求发信域名 SPF/DKIM，或使用授权码） | 由服务商决定（多数要求与账号绑定域名一致） | 不限 ↔ 不限（0 = 不限，由平台报错兜底） | 530/534/535 认证失败 → 自动禁用；429 → 冷却；其余 5xx 拒收按参数错误返回；网络错误 / 4xx 临时错误 → 换渠道重试 |

### 通用注意事项

- **AllowedDomains 与 from 过滤的配合**：请求带 `from` 时，轮换按发件域名过滤候选渠道（配置了 `allowed_domains` 且域名不匹配的渠道被跳过）；请求不带 `from` 时不做过滤，实际发件人由候选渠道的默认发件人回退。因此每个渠道的默认发件人域名务必落在该渠道已在平台验证的域名内，`allowed_domains` 建议与平台侧验证的子域一一对应（见上文「发送域名与 DNS」）。
- **渠道级出站代理**：当部署服务器无法直连某平台 API（例如服务器在境内、部分海外平台接口访问不稳定）时，可为该渠道单独配置 `proxy_url`（`http://` 或 `socks5://`；SMTP 渠道仅支持 `socks5://`）。代理只影响该渠道的出站请求，不影响其他渠道。
- **测试发送会扣额度**：渠道「测试」与轮换发送同口径先预留额度——平台受理成功计 1 封、失败自动返还；渠道额度用尽时测试不执行（提示 429，不落日志）。测试失败不会触发禁用 / 冷却 / 连续失败累计（属人工诊断行为）。
- **发送日志落正文（隐私）**：发送日志完整存储邮件正文、收件人、主题与错误信息，请把 `data/` 目录与 SQLite 文件当敏感数据保护；隐私敏感场景可调低「日志保留天数」（详见 [README「安全注意事项」](../README.md#安全注意事项)）。
- **送达率基础**：正式启用前为每个发信域名配齐 **SPF / DKIM**，建议再加 **DMARC**（多平台共用主域时注意 SPF 单条记录的 include 合并，或按上文建议使用独立子域规避）；各平台控制台均有 DNS 配置指引与验证状态页，以其官方文档为准。
