# CatMail 实施计划（波次派发 · 主代理验收）

> **状态：W1–W5 全部交付（2026-11-12 验收留档）；2026-11-12 优化轮（交付后加固 + 功能补全，11-11 启动、11-12 收官）追加完成，见文末「2026-11-12 优化轮」；2026-11-13 前端现代化轮追加完成，见文末「2026-11-13 前端现代化轮」。** 下表保留波次规划与验收口径留档。

规则：后台子代理并发 ≤ 5；单代理任务中等粒度；**每波完成 → 主代理验收（构建/测试/冒烟）→ 通过才派下一波**；全波次强制执行 SPEC §8 质量标准。
网络：Go modules → goproxy.cn 优先，npm → npmmirror 优先；克隆 GitHub 仓库 → `https_proxy=http://127.0.0.1:7890`。

## 波次表

| 波次 | 任务 | 范围 | 验收标准 | 状态 |
|---|---|---|---|---|
| W1 | **B1** 后端骨架+数据层 | go.mod、目录、config、model（含 ProxyURL/AllowedDomains/正文落库）、store（admin+设置+示例渠道）、provider 契约+mock、main | `go build/vet/test` 零错误 + 可启动 | ✅ 已交付 |
| W1 | **F1** 前端骨架+布局+登录 | Vite7+React19+Tailwind4、router（6 页路由）、query、api 封装、布局、登录页 | `npm run build` 零错误 + dev 可起 | ✅ 已交付 |
| W2 | **B2** rotator 轮换引擎 | 惰性额度重置、双策略排序、域名过滤、指定 channel、failover/冷却/自动禁用 + 单测 | `go test ./internal/rotator/` 全过 | ✅ 已交付 |
| W2 | **B3** HTTP 平台适配器 | resend/brevo/mailjet/smtp2go/mailgun（SPEC §3 逐字段，含代理出站）+ resend httptest 单测 | 构建零错误 + 单测全过 | ✅ 已交付 |
| W2 | **B4** SMTP 适配器 | 通用 SMTP（MIME multipart、B 编码主题、socks5 代理） | 构建零错误 | ✅ 已交付 |
| W2 | **F2** 仪表盘+渠道管理页 | 统计卡+recharts 图+用量进度条+渠道表格/弹窗表单（含 ProxyURL/AllowedDomains）/测试发送 | 构建零错误 | ✅ 已交付 |
| W3 | **B5** API 层 | 全部 /api/*（含 users 账号管理）+ /v1/send + /emails 兼容层 + 会话中间件 | 构建 + curl 冒烟 | ✅ 已交付 |
| W3 | **F3** 日志+Keys+账号+设置页 | 日志详情含正文预览（iframe sandbox）、账号管理 CRUD | 构建零错误 | ✅ 已交付 |
| W4 | **B6** 集成件 | go:embed SPA fallback + scheduler（冷却释放/90 天日志清理） | 单二进制可跑 | ✅ 已交付 |
| W4 | **F4** 前端终验 | 对照 SPEC §6 逐条自查修补、最终构建 | build 零错误 + 自查表 | ✅ 已交付 |
| W4.5 | **R1** 后端质量打磨 | 对照 SPEC §8 后端项全仓审查：gofmt/校验/错误体/索引/WAL/脱敏/单测补漏，直接修复 | build/vet/test 全绿 + 修复清单 | ✅ 已交付 |
| W4.5 | **R2** 前端质量打磨 | 对照 SPEC §8 前端项全页审查：三态/校验/防重复提交/URL query 同步/深浅色/细节打磨，直接修复 | build 零错误 + 修复清单 | ✅ 已交付 |
| W5 | **I1** 联调验收（主代理） | 单二进制构建、mock 渠道全链路 curl 冒烟（含指定 channel/域名过滤/额度轮换场景） | SPEC §2/§5 全端点走通 | ✅ 已交付 |
| W5 | **I2** 浏览器走查（子代理） | 实际打开网页：视觉检查 + 功能全走查（登录/建渠道/发信/日志/账号/设置，web-gui-tester） | 走查报告 + 问题修复闭环 | ✅ 已交付 |
| W5 | **I3** 文档交付（子代理） | README（failover 合规叙事）+ MIT LICENSE + 本地 git init 分阶段提交 | 交付报告 | ✅ 已交付 |

## 已锁定的产品决策（2026-11-11，用户拍板）

1. 首发渠道：Resend / Brevo / Mailjet / SMTP2GO / Mailgun + 通用 SMTP + mock
2. 支持 Resend SDK 兼容端点 `POST /emails`
3. 定位：先自用、预留开源（MIT，README 按"多供应商 failover 网关"叙事）
4. 前端：React 19 + Vite + Tailwind 4（不用 shadcn CLI）
5. 管理员默认 admin/admin123（env 可覆盖，后台可改密）
6. 日志默认保留 90 天（设置页可调，0=永久）
7. 端口 8080；SQLite；v1 不做 Docker/webhook 回执/Postgres
8. UI 中文、雪蓝/天青色系
9. 渠道级出站代理（HTTP/SOCKS5，默认留空直连）
10. 渠道 AllowedDomains 域名绑定 + 轮换按 from 域名过滤候选
11. 发送日志落正文（html/text）
12. 多管理员账号管理页（新建/重置密码/删除）
13. `/v1/send` 可选 `channel` 参数指定渠道
14. 首启预置一条 mock 示例渠道；完工后浏览器实际走查；纯本地 git

## 2026-11-12 优化轮（交付后加固 + 功能补全，实际执行清单）

> W1–W5 交付后的两阶段集中优化（2026-11-11 启动、2026-11-12 收官）：O1–O10 为修复与加固阶段，O11–O19 为功能/安全/运维补全与交付验证阶段。逐项契约细节已同步进 SPEC.md（v1.1，§2/§3/§4/§5/§7/§8），用户视角变更清单见 CHANGELOG.md「Unreleased」。

| 波次 | 任务 | 状态 |
|---|---|---|
| O1 额度 bug 修复（P0/P1） | 发送前原子额度预留/失败释放（rotator.ReserveQuota/ReleaseQuota 条件 UPDATE，杜绝并发超卖与丢单）；连续失败原子递增（UPDATE...RETURNING）；渠道/Key 编辑只写可写集（不覆盖并发累计用量）；逐候选回退渠道默认发件人（未带 from 时不误剔域名绑定渠道） | ✅ 已交付 |
| O2 适配器审查 | SMTP2GO 附件字段对齐官方（fileblob+mimetype，reply_to 经 custom_headers）；Brevo 额度耗尽（HTTP 400 + not_enough_credits）按冷却分类；mock 支持失败模拟魔法值（fail/fail_rate_limit/fail_auth）；SMTP AUTH LOGIN 按 EHLO advertised 协商 + 地址头 RFC 2047；HTTP 响应体 1MB 读取上限 | ✅ 已交付 |
| O3 调度与存储加固 | 日志清理分批删除（500/批，防大事务占写锁）；优雅关停统一收口（HTTP Shutdown → scheduler.Stop → 关库 WAL checkpoint）；冷却释放单条 UPDATE 幂等；历史库 SessionEpoch 回填 | ✅ 已交付 |
| O4 前端审查 | 对照 SPEC §6 全页审查修补：路由级懒加载+Suspense 骨架、401 全局拦截跳登录、日志页键盘导航与 URL 同步、渠道表单清除秘钥语义对齐、构建零错误 | ✅ 已交付 |
| O5 安全加固（P2） | 登录失败限流（用户名+IP 与用户名双维度、耗时对齐防枚举）；会话纪元（改密全体会话失效）；Cookie Secure 开关；请求体上限（30MB/1MB）；headers 拒 CRLF；CSV 公式注入防护；/emails/* JSON 404；唯一约束冲突兜底 400；删账号事务内复核 | ✅ 已交付 |
| O6 性能 | SendLog 增 api_key_id 索引（Key total_sent 聚合走索引）；Key 列表单条 GROUP BY 聚合去 N+1；统计单条聚合 | ✅ 已交付 |
| O7 冒烟验收 | `go build ./... && go vet ./... && go test ./...` 全绿、`npm run build` 零错误（文档校准时复验通过）；/api/health 契约 `{ok:true}` 保持 | ✅ 已交付 |
| O8 修复波 | 渠道测试发送 429 对齐统一错误体（quota_exhausted，SPEC §2）并去掉成功体恒空的 `error:""`；panic 安全：rotator.Send / 测试直发预留额度 defer 配对释放（transport panic 不再静默多计）；releaseQuota 按预留周期锚点回收（跨日/跨月释放不多减新周期计数）；changeOwnPassword 改密+纪元递增事务化（与 resetUserPassword 口径对齐）；gofmt 收尾；SPEC/前端注释同步 | ✅ 已交付 |
| O9 失败通知 | notify 基建（EventEnabled 白名单 / Notifier 双通道 / TestWebhook / PruneOld）+ rotator/scheduler 埋点四类事件：渠道自动禁用（401/403 与连续失败达阈值，error）、429 进入冷却（warn）、冷却恢复（info）、终态发送失败（429 额度耗尽与 502 候选穷尽，error，body 含 message_id/主题截 100 字/收件人前 3 个/已试渠道数/最后错误）；通知中心四端点（分页列表/未读数/标记已读/通道连通性测试）；设置四字段（站内与 Webhook 开关、URL http(s) 校验、事件白名单空=全部）；每小时任务清理站内通知至最近 1000 条；优雅关停 HTTP Shutdown → scheduler.Stop → notifier.Stop（flush webhook 队列）→ 关库 | ✅ 已交付 |
| O10 会话纪元按用户重构 | 浏览器走查 P2 根因：全局纪元（Setting.SessionEpoch）导致自己改密把自己踢下线、重置他人密码误踢操作者。改为 users.session_epoch 按用户纪元；会话 token 升级 v2 格式 `v2\|exp\|username\|user_epoch\|sig`（版本前缀使存量旧 token 全员失效，重新登录一次即可，零迁移）；本人改密事务内 bump 纪元 + 重签当前会话 Cookie（当前会话保持登录，其他会话失效）；重置他人密码仅 bump 目标用户（操作者会话不再受影响）；全局纪元缓存/回填等代码路径全部移除（settings.session_epoch 列残留无害，gorm 不删列） | ✅ 已交付 |
| O11 调用方治理（B5+D1+E2+A1） | ApiKey 四字段 `daily_quota`/`rate_per_sec`/`webhook_url`/`webhook_events`：先限速后额度，/v1/send、/emails、SMTP 入口三处同口径，被拒不发送不落日志（429 rate_limited / quota_exhausted 语义区分）；调用方 Webhook 回执（send_accepted/send_failed，receipt 包 + 连通性测试端点）；幂等键 `Idempotency-Key`（IdempotencyRecord 档案状态机：pending/done、同键处理中 409、5min 僵尸认领、24h 懒惰清理、**失败响应同样存档重放**、`Idempotent-Replay: true` 头）；渠道级出站限速令牌桶 `rate_limit_per_sec`（等待超 5s 跳过候选不计数，全候选限速等待 429 文案） | ✅ 已交付 |
| O12 渠道与日志增强（A3+A4+A2 部分） | 渠道健康度：近 7 天成功率 + 失败 Top3 原因（窗口函数聚合，两方言同构，无日志渠道为 null）；渠道复制（全字段含 secrets，`<原名>-副本` 重名递增，status=manual_disabled、运行态归零）；日志重发（按快照重发走轮换，30s 防滥用 429）；日志清理三模式（before_days/keep_last/all，分批 500/批）；日志正文隐私开关 `log_body_enabled`（false 时新日志正文存空串） | ✅ 已交付 |
| O13 备份与数据管理（A2+D2 配套） | SQLite `VACUUM INTO` 整库快照备份族（列表/创建/下载/删除，文件名白名单防路径穿越，保留最近 10 份轮换）；每日自动备份开关（scheduler 每小时检查执行）；PostgreSQL 连接上备份族统一 `ErrSQLiteOnly` → 接口 400（指引 pg_dump/pg_restore）；备份/清理操作落审计 | ✅ 已交付 |
| O14 安全轮扩展（C4+E4+E5） | TOTP 两步验证（C4）：setup/enable/disable 三端点，`t|` HMAC 短票据 + 8 个一次性恢复码（仅存 sha256），登录第二步 `/api/auth/login/totp`；管理面 IP 白名单（E5）：`CATMAIL_ADMIN_CIDR` 作用于 login/会话内 `/api/*`/`/metrics`，health/发送入口/docs 豁免；`CATMAIL_TRUSTED_PROXIES` 可信反代 CIDR（gin SetTrustedProxies，否则 XFF 不可信）；管理操作审计日志（E4）：15 类动作、detail ≤500 字符不含敏感值、scheduler 裁剪至 5000 条、前端审计页（动作前缀/操作者筛选）；登录限流补用户名维度 + 不存在用户 bcrypt 哨兵等时比对（防枚举，承接 O5） | ✅ 已交付 |
| O15 SMTP 入口（E1） | `CATMAIL_SMTP_ADDR` 启用（默认关闭）：DATA 后解析 MIME 转交 rotator，额度/限速/配额/failover/日志/通知与 API 完全同口径；防开放中继（loopback 免认证、远程必须 AUTH PLAIN/LOGIN 凭 ApiKey，RCPT 前 530）；SMTP 状态码映射（250/451/452/501/552/452 收件人超限/530 未认证）；EHLO PIPELINING/SIZE/8BITMIME/AUTH、60s/5min 超时、panic recover、优雅关停；边界如实标注：仅明文 TCP 无 TLS，公网由反代/隧道终结 | ✅ 已交付 |
| O16 运维与观测（B4+B2+E7+E8） | `/metrics` Prometheus 文本协议四族指标（发送/测试发送计数、渠道状态 gauge、未读通知）+ 管理 IP 白名单；`/docs` 内嵌文档页 + openapi.yaml（43 路径/50 操作，与 SPEC 同步）；`catmail doctor` 子命令 8 大项只读体检（任一 FAIL 退出码 1）；自动更新（E8）：`/api/update/check|apply`、`CATMAIL_UPDATE_REPO/PRERELEASE/CHECK_ON_START`、受限 semver、checksums.txt 强制 SHA-256（缺失/不匹配零落盘）、磁盘预检 + 原子替换、Docker/Windows 拒绝、`internal/version` + Dockerfile `--build-arg VERSION` ldflags 注入、新版本经通知双通道广播（update_available 第 5 事件）；CI 发布流水线（产出资产 + checksums.txt）留 TODO 于 ci.yml 顶部 | ✅ 已交付 |
| O17 PostgreSQL 双方言（D2） | `CATMAIL_DB=postgres://…` 走 gorm postgres 驱动（pgx stdlib），AutoMigrate/CRUD/窗口函数聚合双方言同构（含方言测试与 stats 时间分桶方言测试）；备份族 SQLite 专属（PG 统一 400）；config 层 DSN 前缀自动选型 + doctor 跳过 PG 数据目录检查 | ✅ 已交付 |
| O18 新适配器三家 | elasticemail（X-ElasticEmail-ApiKey，v4 transactional）、mailtrap（Api-Token，与 Resend 同构 body）、aliyun（DirectMail RPC + HMAC-SHA1 POP 签名，AccountName=渠道发信地址，不支持附件）+ 各自默认额度与 httptest 单测；内置平台增至 **10 家** | ✅ 已交付 |
| O19 性能轮（L5）+ 交付验证 | SendLog created_at/channel_id/status/api_key_id 索引 + Key total_sent/统计总览/健康度 Top3 单条聚合（回归测试锁定 SQL 往返条数）；首屏路由级代码分割 JS -54%；10 万行日志下 9 个读接口 0.8–160ms 实测（macOS arm64/SQLite 测试环境标注）+ 24h 压测无泄漏；Playwright e2e 15 用例（C1，一次性后端 + storageState）；smoke.sh 集成冒烟 140 项全 PASS；CI workflow（gofmt/vet/test -race + 前端构建）；四文档同步（SPEC v1.1 / README / CHANGELOG 新建 / 本表全 ✅） | ✅ 已交付 |

## 2026-11-13 前端现代化轮（O20–O50，两轮交付 · 实际执行清单）

> 依据 `frontend/OPTIMIZATION.md`（modern-web-guidance legacy-scan 审计 + 指南检索 + 构建体积核对产出的 O20–O50 执行清单）执行：以 **12+ 个后台子代理流水线**派发（并发 ≤4、**按文件所有权切分**、完成一个补位一个），分两轮交付——第一轮（2026-11-13，W0–W2.5 + W3 低风险项）为 P1 修复、平台基础、原生 `<dialog>` 迁移与静态资产交付层；第二轮（2026-11-13/14）为移动端适配与对比度/交互扫尾。每波验收四道门全绿：`npm run build` 零错误、Playwright 全过（15→22 用例）、`go build/vet/test` 全绿、关键 diff 人工审查。逐项契约与执行留档见 frontend/OPTIMIZATION.md（O32 已同步 SPEC §482），用户视角变更清单见 CHANGELOG.md「Unreleased」。

| 波次 | 任务 | 状态 |
|---|---|---|
| W0 基线校验（O20） | 三套验证基线全绿（Go build/vet/test、前端构建零错误、Playwright 15/15），记录构建产物体积基线（recharts chunk 385KB，为 W2 懒加载提供对照） | ✅ 已交付 |
| W1 P1 bug + 平台基础（O21–O27、O34–O41） | 中文输入法回车防护（日志/审计分页）、iOS 输入 16px、dvh 视口单位、color-scheme/theme-color 元信息三态同步、prefers-reduced-motion、表格 caption/region 键盘横向滚动、表单 aria-invalid/aria-describedby/role="alert"、Button loading 禁用覆盖修复、⌘K 长按与弹窗互斥防护、路由级 ErrorBoundary + chunk 加载失败自动恢复、fetch 默认 30s 超时（更新 10 分钟/CSV 导出 120 秒/备份与清理 5 分钟）、剪贴板统一降级（http 非安全源可用）、通知面板语义、noindex、邮件预览 iframe no-referrer | ✅ 已交付 |
| W2 原生 `<dialog>` + 图表/时间（O28、O42–O44） | Modal/⌘K 命令面板/日志与渠道详情停靠面板迁移原生 dialog（删除手写 focus trap 与滚动锁，命令面板补 combobox 语义）；图表 role="img" 可访问摘要 + KPI 趋势 sr-only 文本；RelativeTime 模块级共享 30s 时钟 + `<time>` 语义；recharts 独立 chunk 懒加载（Dashboard 落地 JS 17.8→13.5KB） | ✅ 已交付 |
| W2.5 静态资产交付层（O45） | gzip 响应压缩（text/*、js/json/svg，Vary，Range 跳过）；assets/ 哈希文件 immutable 一年缓存、index.html 恒 no-cache；安全响应头（nosniff/XFO DENY/Referrer-Policy same-origin/Permissions-Policy/X-Robots-Tag）与 CSP 基线（script/style 'unsafe-inline' 暂留，img-src https: 保邮件预览）；含 4 个新 Go 测试 | ✅ 已交付 |
| W3 语义、对比度、表单细节（O29–O31、O46–O49） | 第一轮部分交付：O30 语义细节（`<search>`/enterKeyHint/autoCorrect/skip link）、O46 表单 name（密码管理器识别）、O48 `@types/qrcode` 归位 devDependencies、O49 ⌘K 提示按平台显示；第二轮补齐：O29 浅色对比度抬档（约 60 处，含占位符）、O31 触屏 44px 命中区、O47 日志行键盘模型方案 A（行内「详情」按钮为键盘入口） | ✅ 已交付（第二轮补齐） |
| W4 移动端适配（O32–O33） | <1024px 侧栏收纳为抽屉（顶栏菜单按钮开关、原生 dialog、路由切换自动关闭、抽屉内恒展开态，SPEC §482 已同步）；日志/审计表格容器 ≤600px 切换堆叠卡片布局（`data-col` + `::before` 注入列名，桌面零变化）；新增移动端抽屉 E2E 3 用例（19→22） | ✅ 已交付 |
| W5 可选性能项（O50） | Sidebar 路由 chunk 预取、静态资源 brotli 预压缩、页面级 View Transitions——收益/风险比一般，本轮不做 | ⏳ 暂缓 |
