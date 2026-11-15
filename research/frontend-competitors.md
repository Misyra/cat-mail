# Snowmail 前端选型与竞品调研报告

> 调研日期：2026-11-11。数据来源：GitHub API（star 数、license、最近 push 时间均为当日实查）。

---

## 1. 前端框架对比（管理后台 / Dashboard 场景）

| 维度 | React 19 + Vite + Tailwind + shadcn/ui | Vue 3 + Vite + Naive UI / Element Plus | SvelteKit |
|---|---|---|---|
| 组件生态维护活跃度 | shadcn/ui（shadcn-ui/ui）**127.8k★ / MIT**，2026-11 仍在高频更新；TanStack Query/Router/Table、Recharts、zustand 全家桶成熟 | Element Plus **27.8k★ / MIT**（活跃）、Naive UI **18.6k★ / MIT**（活跃），表格/表单/弹窗开箱即用 | SvelteKit 20.9k★ / MIT，活跃；shadcn-svelte 可用但组件库与模板生态明显更薄 |
| Dashboard 场景成熟度 | 极高：大量高质量 shadcn 管理端模板（见 §2），图表/表格选型标准 | 极高：国内后台模板最密集的路线（Soybean、pure-admin 等） | 中等：可用但现成 admin 模板少（搜索仅见 small 项目如 modernstack-saas） |
| 与 Go 后端配合的常见做法 | 标准 SPA（Vite build 产物嵌入 Go binary，`go:embed` + 静态路由回退）；TanStack Query 对接 REST；new-api 已验证该组合 | 同样 SPA 嵌入 Go；Element Plus + axios 常见于 Go 后台项目（如各类 gin-admin） | 同样可 embed，但社区案例少 |
| 学习/招聘面 | 最大 | 大（国内尤甚） | 较小 |
| 2025-2026 趋势 | React 19 + Tailwind 4 + shadcn（Base UI primitives）是 new-api、Cal.com、unkey 等新一代开源后台的主流选择 | 稳定维持，Vue 3.5 + Vite 7 生态健康 | 增长但主要在产品站/中小应用 |

**推荐：React 19 + Vite + Tailwind 4 + shadcn/ui。**

理由：
1. 本项目 UI 的直接参照物 new-api 的管理后台正是这套栈（React 19 + shadcn + Base UI + Tailwind 4 + TanStack Router/Query + Zustand + Recharts，构建用 Rsbuild），照抄其信息架构成本最低；
2. shadcn 模板（shadcn-admin 等）直接给出登录页、侧边栏布局、表格页、设置页，只需替换业务字段；
3. Go 后端 `go:embed` 一个纯静态 SPA 是最简单的交付方式，无需 SSR，因此 Vite（而非 Next.js）更合适；
4. 图表选 Recharts，表格选 TanStack Table，请求层 TanStack Query + axios/fetch。

备选：若团队更熟 Vue，则 Vue 3 + Vite + Naive UI + Soybean Admin 是完全可行的等价方案（Naive UI 深色主题与密度控制更适合数据后台）。

---

## 2. Dashboard 模板推荐（均为当日实查状态）

### 首选：satnaing/shadcn-admin
- **16,643★ / MIT**，最近 push 2026-09，活跃维护
- 技术：**React + Vite** + TypeScript + Tailwind + shadcn/ui，TanStack Router/Query
- 自带：登录页、侧边栏/折叠布局、数据表格（含服务端分页示例）、用户管理 CRUD 页、设置页、深浅色主题——与 Snowmail 的登录、渠道管理、日志、设置需求几乎一一对应

### 备选 A（Vue 路线）：soybeanjs/soybean-admin
- **15,301★ / MIT**，最近 push 2026-10，活跃
- Vue3 + Vite + TS + Pinia + **Naive UI** + UnoCSS；权限路由、i18n、主题定制齐全

### 备选 B（Vue 路线）：pure-admin/vue-pure-admin
- **20,758★ / MIT**，最近 push 2026-10，活跃
- Vue3 + Vite + **Element Plus** + TS + Tailwind；功能最全的国内 admin（精简版 pure-admin-thin 适合二开）

### 其他参考
- Kiranism/next-shadcn-dashboard-starter（7,248★ / MIT，Next.js 16 + shadcn，活跃）——若未来想上 Next.js 再看
- arhamkhnz/next-shadcn-admin-dashboard（3,271★ / MIT，活跃）
- 「OpenAdmin」未检索到对应活跃开源模板（GitHub 搜索 0 结果），疑为商业化或旧项目，不建议作为基础

---

## 3. 竞品调研

### 3.1 直接竞品：「多供应商邮件发送聚合/轮换网关」

用 `multi-provider email gateway`、`email api aggregator`、`email relay rotation`、`email gateway sendgrid mailgun`、`smtp rotation sender`、`"email gateway" in:name` 等关键词搜索 GitHub，**没有发现任何成型的开源项目做过「one-api 式的邮件轮换网关」**。命中的只有三类：

| 类别 | 代表 | 状态 | 与本项目差距 |
|---|---|---|---|
| 个人玩具/脚本 | Sugamdeol/email-sender-api（1★，"multi-provider rotation"，Python）、ruomox/Outlook-OAuth2-Sender（3★） | 0-3★，无管理后台、无额度体系 | 只有轮换的雏形概念，无产品化 |
| 群发/营销桌面工具 | InboxKing-Pro、omnisend-pro 等（0-2★） | 个人项目 | 面向群发操作而非开发者 API |
| 单一插件/适配层 | developershohel/...（WordPress 多 SMTP 路由插件）、utopia-rs/messaging（0★，Rust 多 provider 适配器） | 无 star、无生态 | 无统一 Key、无配额仪表盘、无轮换策略 |

结论：**「one-api for email」这个生态位目前是空的**，没有现成竞品。

### 3.2 功能相近但定位不同的开源邮件/通知平台

| 项目 | Stars | License | 活跃度（push） | 语言 | 定位 | 与本项目（聚合轮换）的差距 |
|---|---|---|---|---|---|---|
| **novuhq/novu** | 40,314 | 自定义（NOASSERTION） | 2026-11，非常活跃 | TS | 多渠道通知基础设施（email/SMS/push/in-app），聚合 SendGrid/Mailgun/SES/Postmark/Resend 等 provider | 最接近「provider 聚合」，但面向通知工作流编排（订阅者、偏好、digest），体量重（需 Mongo 等），不做按渠道轮换/配额转售，License 非标准 OSS |
| **knadh/listmonk** | 24,240 | AGPL-3.0 | 2026-11，活跃 | Go | 自托管 newsletter / 邮件列表管理，单二进制 | 面向运营的 campaign 工具，无多 provider 聚合、无 API Key 体系 |
| **useplunk/plunk** | 5,596 | AGPL-3.0 | 2026-11，活跃 | TS | 开源邮件营销平台（automation、campaigns） | 营销自动化定位，发信依赖单一 SES/SMTP，无轮换 |
| **Notifuse/notifuse** | 2,277 | 自定义（NOASSERTION） | 2026-11，活跃 | Go | 开源 Mailchimp 替代：MJML 可视化编辑器 + Liquid 模板 + **7 个发送 provider** | provider 接入最多，但核心是 campaign 编辑器/订阅管理；API 是内部发送而非开发者统一 Key + 轮换 + 配额 |
| deeztek/Hermes-Secure-Email-Gateway | 142 | AGPL-3.0 | 活跃 | JS | 安全邮件网关（入站过滤/加密） | 完全不同领域（入站安全） |
| postalserver/postal / mailcow 等 | — | — | — | — | 邮件服务器/MTA | 解决「自己发信的投递基础设施」，不解决「多平台密钥聚合轮换」 |

### 3.3 差异化空间

现有开源的三个流派——**营销平台**（listmonk/Plunk/Notifuse，给运营人员用）、**通知编排**（Novu，给产品接入多渠道通知）、**邮件服务器**（Postal/mailcow，自己建 MTA）——都不覆盖以下场景：

> 开发者拿着一个 API Key，往 `POST /v1/email/send` 发请求，网关在 N 个第三方平台（SendGrid/Mailgun/SES/Resend/Postmark/阿里云邮推…）的渠道密钥之间按权重/故障/额度轮换，后台有渠道管理、额度仪表盘、发送日志、子 Key 管理。

即「one-api/new-api 在 LLM 领域做的事，在事务性邮件领域没人做」。Snowmail 的差异化：
1. 渠道级轮换 + 健康检查 + 故障转移（one-api 的 channel 模型移植到 email provider）；
2. 按 Key/渠道的额度配额与用量仪表盘（额度转售/分发给下游）；
3. 简单统一的 send API + Webhook 回调（送达/退信事件归一化）。

---

## 4. one-api / new-api 前端风格参照

### one-api（37,396★ / MIT，Go + 多主题前端）
- `web/default`：**React 18 + Create React App + Semantic UI**（semantic-ui-react）+ recharts + axios + i18next
- `web/berry`：React 18 + **MUI v5（Berry Admin 模板）** + Redux + ApexCharts
- `web/air`：第三主题
- 视觉：经典左侧边栏 + 顶栏 + 卡片式统计（额度/请求数图表）+ 密集数据表格 + 弹窗表单；中英双语。整体偏「传统后台」，视觉较旧

### new-api（51,699★ / AGPL-3.0，Go，2026-11 极活跃）
- 前端（`web/`）：**React 19 + Rsbuild + TanStack Router/Query/Table + shadcn/ui（Base UI primitives）+ Tailwind CSS 4 + Zustand + Recharts/VChart + sonner/cmdk/vaul**
- 视觉：现代 shadcn 风——圆角卡片、侧边栏 + 数据表格 + 抽屉/弹窗表单、深浅色主题、渐变仪表盘首页；信息密度接近 Vercel/Dashboard 风格

### 对 Snowmail 的启示
- 直接以 **new-api 的前端栈与信息架构**为模板：侧边栏导航（仪表盘 / 渠道 / API Key / 日志 / 设置）+ 首页统计卡片 + 表格页 + 表单弹窗；
- one-api 的「渠道（channel）管理」页交互（列表 + 测试连通性按钮 + 权重/分组字段）可以近乎原样映射为 Snowmail 的「邮件渠道管理」；
- License 提醒：new-api 是 AGPL-3.0，参考其 UI 布局没问题（布局思想不受版权保护），但**不要直接复制其代码**到 Snowmail，除非接受 AGPL；shadcn-admin（MIT）作为代码起点则无此顾虑。

---

## 5. 最终推荐组合

```
React 19 + Vite + TypeScript + Tailwind CSS 4 + shadcn/ui
+ TanStack Router / TanStack Query / TanStack Table + Recharts + Zustand
+ 模板基座：satnaing/shadcn-admin（MIT）
交付形态：纯静态 SPA，go:embed 进 Go 后端二进制
```

备选（团队 Vue 背景）：Vue 3 + Vite + Naive UI + Soybean Admin（均 MIT）。

---

## 附：当日实查数据一览

| 仓库 | Stars | License | 最近 push |
|---|---|---|---|
| shadcn-ui/ui | 127,828 | MIT | 2026-11-10 |
| QuantumNous/new-api | 51,699 | AGPL-3.0 | 2026-11-10 |
| songquanpeng/one-api | 37,396 | MIT | 2026-01-09 |
| novuhq/novu | 40,314 | 自定义 | 2026-11-10 |
| knadh/listmonk | 24,240 | AGPL-3.0 | 2026-11-10 |
| pure-admin/vue-pure-admin | 20,758 | MIT | 2026-10-12 |
| sveltejs/kit | 20,905 | MIT | 2026-11-10 |
| tusen-ai/naive-ui | 18,607 | MIT | 2026-10-26 |
| satnaing/shadcn-admin | 16,643 | MIT | 2026-09-10 |
| soybeanjs/soybean-admin | 15,301 | MIT | 2026-10-21 |
| element-plus/element-plus | 27,847 | MIT | 2026-11-10 |
| useplunk/plunk | 5,596 | AGPL-3.0 | 2026-11-09 |
| Notifuse/notifuse | 2,277 | 自定义 | 2026-11-10 |
| Kiranism/next-shadcn-dashboard-starter | 7,248 | MIT | 2026-11-08 |
