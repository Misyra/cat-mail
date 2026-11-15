# 更新日志（Changelog）

本项目所有显著变更记录于此文件。格式基于 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，条目面向使用者、每条一行；实现细节与波次验收口径见 [PLAN.md](PLAN.md)，契约细节见 [SPEC.md](SPEC.md)。

> **[1.0.0]（2026-11-15 归档）** = W1–W5 首版交付 + 2026-11-12 优化轮（交付后加固与功能补全）+ 2026-11-13/14 前端现代化两轮 + 2026-11-14 送达链路/SMTP TLS/邮件模板与安全加固两轮 + 2026-11-15 开源就绪轮（社区文件/CI 门禁/镜像发布/README 改版）+ 发布前项目更名（Snowmail → CatMail，猫邮）的全部变更。

## [Unreleased]

暂无。

## [1.0.0] - 2026-11-15

### Added

- 项目更名 **Snowmail → CatMail（猫邮）**：发布前更名，无对外兼容包袱；Go module、环境变量前缀（`SNOWMAIL_*`→`CATMAIL_*`）、回执与通知签名头（`X-Snowmail-Signature`→`X-CatMail-Signature`）、Prometheus 指标前缀（`snowmail_*`→`catmail_*`）、API Key 前缀（`sm-`→`cm-`）、二进制与 Release 资产名（`snowmail_<os>_<arch>`→`catmail_<os>_<arch>`）、默认数据库文件名（`snowmail.db`→`catmail.db`）、前端 localStorage key（`snowmail-lang`/`snowmail-theme`→`catmail-lang`/`catmail-theme`）、镜像与克隆 URL 示例同步更名。
- 新增 **开源社区文件**：`SECURITY.md`（漏洞私密披露策略：GitHub Private Vulnerability Reporting 主渠道，响应承诺 48h 确认 / 7 天评估，凭据脱敏规范）、`CONTRIBUTING.md`（开发环境/go:embed 坑/测试与 i18n 门禁/契约优先约定）、`CODE_OF_CONDUCT.md`（Contributor Covenant v2.1 中文版）、GitHub issue 表单（bug/feature）与 PR 模板（自测清单含 SPEC/CHANGELOG 联动）、dependabot 三线（gomod/npm/github-actions）weekly 自动升级。
- 新增 **CI 门禁四道**：后端测试覆盖率测量与上报（artifact + Codecov tokenless）、golangci-lint（v2 配置 6 linter，排除规则见 `backend/.golangci.yml`）、govulncheck（失败即红）、前端 Playwright E2E 全量进 CI（webServer 自动构建前后端，端口 18100）；CI 增加最小权限（`contents: read`）与同 ref 并发取消。
- 新增 **Release Docker 多架构镜像**：推送 `v*` tag 时构建 linux/amd64 + linux/arm64 镜像发布到 ghcr.io（semver 全套 tag），附 SBOM 与 provenance attestation，镜像注入 VERSION 供 E8 自动更新校验；与二进制发布完全并行，互不阻塞。
- 新增 **README 改版**：管理后台截图（带演示数据，1600×900 @2x，`docs/screenshots/`）、Mermaid 架构图、静态 badge；长文拆分至 `docs/platforms.md`（渠道接入指南与平台清单）与 `docs/smtp.md`（SMTP 入口），发送 API 细节统一收口 `docs/API.md`；新增英文版 `README.en.md`。
- 新增 **管理后台国际化**：全站 16 页面与共享组件完成 react-i18next 双语化（zh-CN 源语言 + en-US，约 1050 条词典 key，按 17 个领域命名空间拆分）；语言跟随浏览器自动探测、设置页「界面语言」下拉手动切换（即时生效）、localStorage 持久化；时间显示与相对时间随语言本地化（英文单复数）；TS 全量 key 校验 + zh/en 词典同构校验脚本（`node scripts/check-i18n.mjs`）；后端 API 错误消息本期保持中文单语，后续独立一期。

- 新增 **管理后台国际化**：全站 16 页面与共享组件完成 react-i18next 双语化（zh-CN 源语言 + en-US，约 1050 条词典 key，按 17 个领域命名空间拆分）；语言跟随浏览器自动探测、设置页「界面语言」下拉手动切换（即时生效）、localStorage 持久化；时间显示与相对时间随语言本地化（英文单复数）；TS 全量 key 校验 + zh/en 词典同构校验脚本（`node scripts/check-i18n.mjs`）；后端 API 错误消息本期保持中文单语，后续独立一期。

- 新增 **送达事件摄取**：每渠道分配独立的送达事件 webhook 地址（`POST /webhooks/mail-events/:token`，渠道详情可复制/轮换），接收各平台送达事件——Resend（Svix 规范 `whsec_` 签名，±5 分钟容差）与 Mailgun（Webhook Signing Key，±15 分钟容差）做真实签名校验，其余平台以 URL token 即凭据；事件归一化为 sent / delivered / bounced（hard|soft）/ complained / raw 存档，并尽力关联到发送日志；时间戳容差 + 进程内去重双重防重放。
- 新增 **抑制列表**：hard bounce 与投诉（spam 标记）事件自动入库（开关默认开，可手动添加/解除），命中收件人的后续发送在三个入口（HTTP API / Resend 兼容端点 / SMTP）统一拦截，返回明确错误码 `recipient_suppressed` 并列出命中的收件人，不进入轮换、不落发送日志。
- 新增 **渠道健康度送达侧升级**：渠道健康度在受理成功率与失败 Top 原因之外，补齐近 7 天送达率/退信率/投诉率（无事件数据显示「—」）；退信率超阈值（默认 5%，0-100 可调、0=关闭）自动告警（同渠道 1 小时去重），并新增 `bounce_rate_exceeded` 通知事件（可按事件白名单开关）。
- 新增 **SMTP TLS 入口**：配置证书（`CATMAIL_SMTP_CERT_FILE`/`CATMAIL_SMTP_KEY_FILE`）后，明文监听支持 STARTTLS 升级（RFC 3207，升级后复位会话状态），并可经 `CATMAIL_SMTP_TLS_ADDR` 增开 465 风格隐式 TLS 监听；`CATMAIL_SMTP_TLS_POLICY=require` 时明文监听必须先升级才允许 AUTH（否则 530），防明文密码泄露。未配证书行为与旧版一致，反代/隧道方案不受影响；`catmail doctor` 新增证书、策略与 TLS 端口检查项。
- 新增 **邮件模板**：管理端维护 `{{var}}` 占位符内容骨架（主题/HTML/纯文本，变量清单自动提取，桌面/移动双预览），三个发送入口经 `template_id` + `variables` 引用渲染——HTML 语境变量值自动转义（无逃生门），缺失变量留空并在成功响应 `missing_variables` 字段可见（不阻断发送）；与自带正文并存显式报错（`body_conflict`），模板不存在返回 `unknown_template`；发送日志记录所用模板 ID 供追溯。
- 新增 **域名治理面板**：按渠道发件域名聚合「域名 × 渠道」矩阵，逐平台展示 SPF/DKIM/DMARC/MX/发件人验证记录与配置指引（前端静态知识库，纯展示 + 文案复制，不做 DNS 检测）。
- 新增 **渠道凭据静态加密**：配置 `CATMAIL_SECRET_KEY`（base64 编码 32 字节，`openssl rand -base64 32` 生成）后，渠道的 API Key/Secret/SMTP 密码/代理地址/平台签名密钥以 AES-256-GCM 加密落库（`enc:v1:` 前缀密文），首次配置自动一次性迁移存量明文（幂等）；未配置保持明文兼容，但库中已有密文或密钥不匹配时拒绝启动；备份文件中的凭据天然为密文。
- 新增 **Webhook 签名**：通知 Webhook 与调用方回执统一附带 `X-CatMail-Signature: t=<unix秒>,v1=<hex>` 签名头（`v1 = hex(HMAC-SHA256(secret, t + "." + 原始请求体))`，±5 分钟容差），接收方可防伪造与重放；通知密钥在设置页生成/轮换、回执密钥创建 Key 自动生成/可轮换，**secret 未配置时不签名、不发头**（接收方校验示例须按此兼容）。
- 新增 **Webhook outbox 投递**：通知 Webhook 与调用方回执改为先落库（`webhook_deliveries` 表）再由后台任务每分钟投递——重启不丢任务（at-least-once），失败按指数退避重试（1m→5m→30m→2h→8h→24h 封顶），10 次失败转 dead（保留最近 100 条供排查）；后台新增投递记录查询端点 `GET /api/webhook-deliveries`。
- 新增 **会话管理**：登录会话落服务端注册表，「系统设置」页新增活跃会话卡片——查看本人会话（来源 IP/设备/最后活跃）、踢出指定会话、一键下线其余全部会话；吊销即时生效，不影响 API Key。
- 新增 **管理员重置密码联动吊销**：重置他人密码升级为单事务联动——目标用户全部活跃会话吊销 + 其名下全部 API Key 一并停用（管理页展示「密码重置后停用，可手动启用」徽标；升级前创建的无主 Key 不受影响）。
- 新增 **Elastic Email / Mailtrap / 阿里云邮件推送** 三家适配器，内置发送平台增至 10 家（Resend、Brevo、Mailjet、SMTP2GO、Mailgun、Elastic Email、Mailtrap、阿里云邮件推送、通用 SMTP、mock）。
- 新增 **SMTP 入口**（可选，`CATMAIL_SMTP_ADDR` 启用）：只支持 SMTP 的本地应用可把 CatMail 当普通 SMTP 服务器用，发送语义与 HTTP API 完全一致；loopback 免认证、远程必须用 API Key 认证，杜绝开放中继。
- 新增 **幂等键防重发**：发送请求可携带 `Idempotency-Key` 头，同键重放直接回放首次响应（含失败响应）、处理中返回 409、进程崩溃残留 5 分钟后自动认领、档案 24 小时懒惰清理；不带头则完全不启用。
- 新增 **API Key 级配额与限速**：`daily_quota`（每日发送上限）与 `rate_per_sec`（令牌桶限速），先限速后额度，被拒不发送、不落日志；HTTP 与 SMTP 入口同口径。
- 新增 **渠道级出站限速**：渠道可配置 `rate_limit_per_sec`（令牌桶），取令牌等待超时按「临时不可用」继续 failover。
- 新增 **失败通知双通道**：站内通知中心（顶栏铃铛）+ Webhook（自动适配飞书/钉钉/企业微信/Slack 机器人）双通道，覆盖渠道自动禁用、进入冷却、冷却恢复、发送失败、发现新版本 5 类事件，支持按事件开关与连通性测试。
- 新增 **调用方 Webhook 回执**：按 API Key 配置回执地址，推送 `send_accepted` / `send_failed` 事件，并提供连通性测试端点。
- 新增 **渠道健康度**：渠道列表附带近 7 天成功率与失败 Top3 原因，辅助判断该渠道是否值得保留。
- 新增 **渠道复制**：一键复制渠道全部配置（含秘钥），副本自动停用、运行态归零、重名自动加「-副本」后缀。
- 新增 **日志重发**：按历史日志快照一键重发（走轮换、落新日志），30 秒内防重复滥用。
- 新增 **手动日志清理三模式**：保留最近 N 天 / 保留最近 N 条 / 全部清空（与每日自动清理并存，均分批删除）。
- 新增 **日志正文隐私开关** `log_body_enabled`：关闭后新日志不再存储邮件正文（历史日志不受影响）。
- 新增 **SQLite 整库备份**：设置页一键创建 `VACUUM INTO` 一致性快照，自动保留最近 10 份，支持下载/删除与每日自动备份开关。
- 新增 **管理操作审计日志**：渠道/Key/账号/设置/备份等 15 类写操作记录「谁在何时从哪个 IP 做了什么」（不含秘钥密码），保留最近 5000 条，后台新增「审计」页。
- 新增 **TOTP 两步验证**：扫码绑定认证器，登录需 6 位动态码；启用时一次性返回 8 个恢复码（仅存哈希），找回登录能力不依赖后端。
- 新增 **管理面 IP 白名单**：`CATMAIL_ADMIN_CIDR` 限制登录/管理 API/`/metrics` 的来源网段；`CATMAIL_TRUSTED_PROXIES` 声明可信反代，保证反向代理部署下真实客户端 IP 可信。
- 新增 **`/metrics` Prometheus 指标**：发送/测试发送计数、渠道状态、未读通知数四族指标，实时聚合、支持白名单保护。
- 新增 **`/docs` 内嵌 API 文档页**：浏览器直接打开即可查阅完整 OpenAPI 3.1 文档（零外部依赖，示例脱敏），规范原文随 `/docs/openapi.yaml` 提供。
- 新增 **`catmail doctor` 自检命令**：8 大项只读体检（环境/配置/数据目录/数据库/渠道完整性/通知回执/端口/汇总），任一 FAIL 退出码 1，可接入监控脚本。
- 新增 **自动更新（实验性）**：配置 `CATMAIL_UPDATE_REPO` 后可在后台检查并一键应用 GitHub Release 新版本——强制 SHA-256 校验（缺 checksums.txt 拒绝且零落盘）、原子替换、容器/Windows 拒绝；可选启动时检查并经通知双通道广播。
- 新增 **PostgreSQL 支持**：`CATMAIL_DB=postgres://…` 即切双方言同构存储（备份等 SQLite 专属功能明确拒绝并指引 `pg_dump`）。
- 新增 **Docker 部署物**：多阶段 Dockerfile（非 root、healthcheck、支持 `--build-arg VERSION` 注入版本）、docker-compose.yml、GitHub Actions CI（gofmt/vet/test -race + 前端构建）。
- 新增 **前端自动化验证**：Playwright e2e 15 用例（`npm run e2e`，一次性后端自动拉起）、`scripts/smoke.sh` 集成冒烟 140 项断言（含并发额度压测、TOTP、备份、白名单、审计全场景）。
- 新增 **首启三步引导清单**（仪表盘展示：改默认口令 → 建渠道 → 建 Key，可跳过）与默认口令弱口令提醒（`weak_admin_password`）。
- 新增 mock 渠道 **故障注入魔法值**：API Key 填 `fail` / `fail_rate_limit` / `fail_auth` 分别模拟 5xx / 429 / 401，本地即可演练 failover、冷却与自动禁用。
- 新增 **移动端抽屉侧栏**：<1024px 视口侧栏收纳为抽屉，顶栏菜单按钮开关（原生 `<dialog>`，Esc/点击遮罩关闭），路由切换自动关闭，抽屉内恒为展开态；桌面（≥1024px）可折叠侧栏行为不变。
- 新增 **日志/审计表格移动端堆叠卡片布局**：容器宽度 ≤600px 时表格自动折叠为卡片（表头隐藏，列名经 `data-col` 注入行内），横向滚动让位为纵向堆叠；桌面（>600px）布局零变化。
- 新增 **前端现代化回归 E2E 用例**（15→22）：输入法合成态回车不跳页（日志/审计分页）、弹窗打开时 ⌘K 不叠加命令面板、chunk 加载失败自动兜底恢复、移动端抽屉导航。

### Changed

- 许可证 **MIT → AGPL-3.0**：发布前变更（无历史许可负担），唯一版权人 Misyra；README/README.en badge 与 License 行同步，PLAN.md 早期记录保留原文。
- Go 工具链 1.27.3 → **1.27.4**：修复 stdlib `net/http` 可达漏洞 GO-2026-6859（HTTP/1 server MaxBytesReader header race），govulncheck 门禁转绿。
- **会话 token 升级 v3 格式**（新增服务端会话注册表）：v2 及更早格式的存量 token 一律失效，升级后需重新登录一次（零数据迁移；本人改密后当前会话保持登录的口径不变）。
- 管理员重置他人密码升级为**单事务联动**：目标用户全部会话吊销 + 其名下全部 API Key 停用（见 Added），被盗账号的持久化凭据一并失效。
- 通知 Webhook 与调用方回执的投递从「内存队列 + 失败 1s 重试 1 次」改为 **outbox 落库异步投递**（见 Added）：优雅关停不再有队列排空步骤，重启后按任务续投。
- 渠道编辑的秘钥字段扩展 `webhook_secret`（平台侧签名密钥：Resend 的 `whsec_` 签名串 / Mailgun 的 Webhook Signing Key），同样支持传空保留与 `clear_secrets` 显式清除。
- 通知事件白名单扩展至 6 类（新增 `bounce_rate_exceeded`）；设置项新增 `suppression_auto_enabled`（默认开）与 `bounce_rate_threshold`（默认 5，0=关闭告警）。
- `catmail doctor` 检查扩展：配置项新增 CATMAIL_SECRET_KEY 状态（未配置 WARN 建议配置）、SMTP TLS 证书可读性与策略生效值；渠道完整性新增凭据加密状态盘点（有密文无密钥 → FAIL）；端口监听含隐式 TLS 端口。
- 发送额度改为**先原子预留、失败自动返还**：并发下不超卖、不丢单；预留/释放按周期锚点配对，跨日/跨月释放不再误扣新周期计数。
- Brevo 额度耗尽（HTTP 400 + `not_enough_credits`）改按**限流同款冷却**处理，不再把消息反复打到已无额度的渠道。
- 修改密码后**当前会话保持登录**、本人其他会话失效；重置他人密码仅目标用户会话失效（旧版会把操作者也踢下线）；会话 token 升级 v2 格式，存量旧 token 全部失效，需重新登录一次。
- 渠道/Key 编辑仅写表单可写集（不再覆盖并发累计用量）；渠道秘钥字段传空 = 保留原值，显式清除用 `clear_secrets`。
- 统计总览响应裁剪凭据类列，后台接口响应一律脱敏展示。
- 日志清理改为分批删除（500 条/批），避免大事务长时间占用 SQLite 写锁。
- 优雅关停顺序收口：HTTP → SMTP 入口 → 定时任务（等待在执行的 outbox 投递轮收尾）→ 关库 checkpoint；webhook 投递已落库持久化，停机/重启不丢在途通知与回执。
- 平台错误信息提取增强：支持嵌套结构深度下探 5 层（如 Mailjet `Messages[0].Errors[]`），错误文案不再为空。
- 管理后台弹层迁移浏览器**原生 `<dialog>`**：Modal、⌘K 命令面板、日志与渠道详情停靠面板改用原生模态（top layer、背景惰性，焦点/Esc/返回手势交给浏览器），删除手写 focus trap 与滚动锁；命令面板补齐 combobox 语义（`aria-activedescendant` + listbox/option）。
- 表格与表单**可访问性增强**：四个表格页补 sr-only caption 与列头 `scope`、横向滚动容器键盘可聚焦（region 语义）；表单控件注入 `aria-invalid`/`aria-describedby`，错误与提示经 `role="alert"` 播报；通知面板改 region 语义，未读角标不再被读屏重复播报；仪表盘图表附一句话可访问摘要、KPI 趋势方向提供读屏文本；新增 skip link 直达主内容。
- 表单补 `name` 属性与移动输入细节：登录/账号/Key/设置等表单可被密码管理器识别保存填充，标识类输入补 `autoCorrect`/`autoCapitalize`/`spellCheck`，登录与验证码补 `enterKeyHint`；⌘K 快捷键提示按平台显示（Mac 为 ⌘K，其余为 Ctrl K）。
- 触屏设备（`pointer: coarse`）下顶栏图标按钮、侧栏导航与分页控件命中区提升到 **44px**，精确指针（桌面鼠标）密度不变。
- 发送日志表格行不再整体可聚焦：键盘用户 Tab 至行内「详情」按钮打开详情面板，表格回归原生语义（屏幕阅读器按原生表格播报），行点击仅服务鼠标。
- 浅色主题弱化文本对比度整体抬档：`text-slate-400 dark:text-slate-500` 成对调整为 `text-slate-500 dark:text-slate-400`（全站约 60 处，含输入框占位符），达到 WCAG AA 4.5:1；图标/装饰元素保持原色。
- `@types/qrcode` 从 dependencies 移至 devDependencies（仅构建期类型，运行时零影响）。

### Fixed

- 修复 golangci-lint 首轮报出的 3 处代码小问题：无效赋值（`api_notifications_test.go`）、注释错字（`aliyun_test.go`）、SMTP dot-stripping 改用 `strings.TrimPrefix`（语义等价，`session.go`）。
- 修复并发额度超卖/丢单：发送前原子条件 UPDATE 预留，失败路径 defer 配对释放，transport panic 也不再静默多计额度。
- 修复连续失败计数并发丢失（改为原子递增），达到阈值可靠触发自动禁用。
- 修复 SMTP 适配器 PLAIN 认证成功后发送失败的 bug（AUTH 机制按 EHLO advertised 协商）。
- 修复渠道测试发送：429 对齐统一错误体（quota_exhausted），与轮换发送同口径预留/返还额度；测试失败不再误触发禁用/冷却。
- 修复改密/重置密码与纪元递增非同事务导致的会话状态不一致。
- 修复跨日/跨月后失败释放额度误减新周期计数的问题。
- 修复中文输入法组词回车误触跳页：日志/审计分页输入框忽略输入法合成态的 Enter（`isComposing`/keyCode 229），确认候选不再直接跳页。
- 修复 iOS Safari 聚焦输入框强制缩放视口：移动端输入控件字号提升至 16px（桌面 14px 视觉不变）。
- 修复移动端地址栏收放时布局跳动：`h-screen`/`vh` 等改用 `dvh` 动态视口单位；深色主题首屏白闪随 color-scheme/theme-color 元信息预置并三态同步一并消除。
- 修复 Button 加载态禁用被调用方显式 `disabled={false}` 覆盖的问题：pending 期间按钮始终禁用并带 `aria-busy`（Webhook 测试、设置保存等）。
- 修复发版后旧 chunk 404/网络中断导致的整页白屏：新增路由级 ErrorBoundary 兜底卡（可重载/返回首页），chunk 加载失败每会话自动重载一次。
- 修复后端假死/网络半开时请求永久 pending：API 请求默认 30 秒超时并给出错误提示（自动更新 10 分钟、CSV 导出 120 秒、备份与日志清理 5 分钟不被误杀）。
- 修复 ⌘K 长按反复开关命令面板、弹窗打开时在其上叠加打开命令面板的问题：忽略按键长按重复，存在弹层时不叠加，Esc 只关当前层。
- 修复 http 非安全源下复制失败：剪贴板操作统一封装降级（原生 API 不可用时回退 `execCommand`），恢复码/API Key/Message ID 在 http 部署下均可复制。

### Security

- **渠道凭据静态加密**：渠道 API Key/Secret/SMTP 密码/代理地址/平台签名密钥支持 AES-256-GCM 加密落库（`CATMAIL_SECRET_KEY`，见 Added）；代理地址相关错误信息不再回显原文，防内嵌代理凭据泄露；加密状态下备份文件天然只含密文。
- **出站 Webhook 全量签名**：通知 Webhook 与调用方回执附带 `X-CatMail-Signature`（HMAC-SHA256，±5 分钟容差防重放），接收方可验证请求确实来自 CatMail 且未被篡改。
- **SMTP 入口 TLS**：STARTTLS（RFC 3207）+ 隐式 TLS 监听 + `require` 策略（AUTH 前必须升级），SMTP 认证凭据不再强制明文传输；非法 TLS 配置启动即拒绝，绝不带病降级。
- **密码重置联动吊销**：管理员重置他人密码时单事务联动吊销该用户全部会话并停用其名下全部 API Key，泄露的密码不再能维系既有访问与发送通道。
- 登录失败限流升级：用户名+IP 与用户名双维度独立计数（X-Forwarded-For 伪造轮换 IP 无法绕过用户名维度），不存在的用户走等时长 bcrypt 哨兵比对，响应时间不泄露用户名是否存在。
- 请求体大小上限（发送入口 30MB / 管理 API 1MB）；自定义 headers 拒绝 CRLF（防 SMTP/MIME 头注入）。
- CSV 导出公式注入防护（`=+-@` 前置单引号）；`/emails/*` 未知路径返回 JSON 404，不再落入 SPA 页面。
- 会话 Cookie 支持 `CATMAIL_COOKIE_SECURE` 开关（HTTPS 部署附加 Secure）；会话签名密钥首启随机生成。
- 审计日志不含任何秘钥/密码等敏感值；备份下载文件名白名单校验防路径穿越；自动更新强制 SHA-256 校验，防二进制替换。
- 静态资产交付层安全响应头基线：新增 **CSP**（`default-src 'self'` 等；script/style `'unsafe-inline'` 暂保留、`img-src https:` 保邮件预览远程图）、`X-Content-Type-Options: nosniff`、`X-Frame-Options: DENY` 与 `frame-ancestors 'none'` 双保险防点击劫持、`Referrer-Policy: same-origin`、`Permissions-Policy` 关闭摄像头/麦克风/地理位置。
- 管理后台全面禁止搜索引擎索引：页面 meta robots noindex + 全部响应附加 `X-Robots-Tag`；邮件 HTML 预览 iframe 增加 `no-referrer`，远程图片请求不再携带后台地址作为 Referer。

### Performance

- 首屏 JS 经路由级代码分割较上轮下降约 **54%**（图表等重组件懒加载 + 骨架屏）。
- 发送日志四列索引 + 关键聚合单条化（Key total_sent / 统计总览 / 健康度 Top3），10 万行日志下 9 个读接口实测 0.8–160ms（测试环境：macOS arm64 本机 SQLite，数据仅供参考）；连续 24 小时压测无内存泄漏。
- 静态资产 gzip 响应压缩（text/*、JS/JSON/SVG，附 `Vary: Accept-Encoding`，Range/HEAD 跳过）；`assets/` 哈希文件名资产 `immutable` 一年强缓存，index.html 恒 `no-cache` 保证发版后立取新入口。
- recharts 图表拆分独立 chunk 并懒加载：仪表盘落地页 JS **17.8→13.5KB**（图表以骨架屏占位，数据请求与 chunk 并行加载）。
- 相对时间组件改为模块级共享单一 30 秒时钟（`useSyncExternalStore`，页面隐藏时暂停）：百实例页面的定时器从上百个降为 1 个；日志表格时间列改用 `<time>` 语义并随分钟推进自动刷新。
