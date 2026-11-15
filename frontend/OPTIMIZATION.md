# 前端现代化执行计划（2026-11-13）

> 审计依据：`modern-web-guidance` legacy-scan（命中 2 条 IME 问题）+ 全量通读 + 指南检索
> （`dialog` / `responsive-table` / `forms` / `motion` / `dark-mode` / `responsive-design` /
> `form-factor-adaptation` / `ime-safe-enter-submit`）+ 生产构建体积核对。
>
> 范围：`frontend/src` + 静态资产交付层（`backend/cmd/catmail/main.go`）。
> 栈：React 19 + Vite 8 + Tailwind 4。
>
> 执行约定：**按 W0 → W5 顺序执行；每一波结束跑构建 + 该波验收，全绿再进下一波**。
> 任务编号保留审计批次（O20–O50），执行顺序按波次。E2E 一次性后端端口 18100；
> 前端构建产物进 `backend/internal/web/dist`（go:embed），改完前端必须先 `npm run build`
> 再跑 Go 构建/E2E。

---

## 波次总览

| 波次 | 内容 | 任务 | 风险 | 状态 |
|---|---|---|---|---|
| W0 | 基线校验 | O20 | — | ✅ 2026-11-13（Go 全绿 + Playwright 15/15） |
| W1 | P1 bug + 平台基础 | O21–O27、O34–O41 | 低 | ✅ 2026-11-13 |
| W2 | 原生 `<dialog>` + 图表/时间 | O28、O42–O44 | 中 | ✅ 2026-11-13（含 ChannelsPage 渠道详情面板同款迁移） |
| W2.5 | 静态资产交付（gzip/缓存/安全头） | O45 | 中 | ✅ 2026-11-13（含 4 个新 Go 测试） |
| W3 | 语义、对比度、表单细节 | O29–O31、O46–O49 | 低-中 | ✅ 2026-11-14（O29/O31/O47 已拍板执行；O30/O46/O48/O49 见第一轮） |
| W4 | 移动端适配（改 SPEC §482） | O32–O33 | 高 | ✅ 2026-11-14（抽屉 + 表格卡片化，13-mobile×3 E2E + 390px 实拍验收） |
| W5 | 可选性能项 | O50 | 低 | ⏳ 暂缓（路由预取/brotli/视图过渡） |

> **执行留档（2026-11-13）**：W0–W2.5 + W3 低风险项以 12 个后台子代理（并发 ≤4、按文件所有权切分、
> 完成一个补位一个）完成；验收四道门全绿——`npm run build` 零错误、Playwright **19/19**
> （15 存量 + 11-ime×2 + 12-resilience×2）、`go build/vet/test` 全绿、关键 diff 人工审查通过。
> 构建产物变化：`DailyActivityChart` 独立 chunk 5.6KB，Dashboard 落地页 JS 17.8→13.5KB（recharts 不再随首屏）。
> 偏差记录：ChannelDetailPanel 的 dialog 迁移由子代理自行扩大范围完成（方向正确、已人工复核）；
> ErrorBoundary 的 `key={pathname}` 会使路由树随导航重挂载（副作用已核实无害）。

---

## W0 — 基线（O20）

- [ ] `cd frontend && npm run build`
- [ ] `cd frontend && npx playwright test`（15 用例，setup 登录态）
- [ ] `cd backend && go build ./... && go vet ./... && go test ./...`
- [ ] 记录基线结果；任一失败先修基线，不叠加新改动。
- [ ] 若 18100 有残留进程：`lsof -ti tcp:18100 | xargs kill -9`

**已完成（2026-11-13 审计时）**：`npm run build` 零错误。产物：
CSS 57.84KB / gzip 9.98KB；`index` 86.64KB / gzip 26.15KB；
`react-vendor` 285.07KB / gzip 90.20KB；`recharts` 385.61KB / gzip 109.74KB。

**完成定义**：三项全绿，记录当前 E2E 用时/用例数。

---

## W1 — P1 bug + 平台基础（O21–O27、O34–O41）

### O21 中文输入法回车防护（S，必须）

**问题**：分页输入框只判断 `e.key === 'Enter'`。拼音/五笔组词时按回车确认候选，会直接跳页。
`CommandPalette` 已有正确防护，可对齐。

- [ ] `src/pages/AuditPage.tsx` 第 362 行附近
- [ ] `src/pages/LogsPage.tsx` 第 754 行附近
- [ ] 统一改成：
      ```tsx
      onKeyDown={(e) => {
        if (e.nativeEvent.isComposing || e.keyCode === 229) return
        if (e.key === 'Enter') handlePageJump()
      }}
      ```
- [ ] `grep -rn "key === 'Enter'" src` 复查其余输入类 Enter 处理器（`<tr>` 上的方向键导航不涉及 IME，保留）
- [ ] 加回归用例：`frontend/e2e/11-ime.spec.ts`，对日志/审计分页输入框
      `dispatchEvent('keydown', { key: 'Enter', keyCode: 229, isComposing: true })`，断言 URL 的 `page` 参数未变；
      再发一次 `isComposing: false` 的 Enter，断言跳页成功。

**验收**：`npm run build` 零错误；新 E2E 通过；手动用拼音输入法在输入框里回车确认候选不跳页。

---

### O22 iOS 输入框聚焦不缩放（S，必须）

**问题**：`inputClasses` 是 `text-sm`（14px），紧凑控件覆盖成 `text-xs`（12px）。
iOS Safari 对 `<16px` 的输入控件聚焦时强制放大视口。

- [ ] `src/components/ui/Input.tsx` 的 `inputClasses`：`text-sm` → `text-base sm:text-sm`
- [ ] 紧凑控件显式改成 `text-base sm:text-xs`（确保移动端 16px）：
  - [ ] `src/pages/LogsPage.tsx` 每页条数 `Select`、跳页 `Input`
  - [ ] `src/pages/AuditPage.tsx` 每页条数 `Select`、跳页 `Input`
  - [ ] `src/components/charts/DailyActivityChart.tsx` 渠道筛选 `Select`
  - [ ] `grep -rn "className=.*text-xs" src/pages src/components | grep -E "Input|Select|Textarea"` 复查遗漏
- [ ] 检查 `Field` 的 label/hint 不受影响；桌面端保持 14px 视觉不变。

**验收**：Playwright 视口 `390×844`，聚焦登录用户名/日志筛选输入框，
`getComputedStyle(input).fontSize` ≥ `16px`；桌面 `1280` 下为 `14px`；`npm run build`。

---

### O23 动态视口单位（S）

**问题**：`h-screen` / `85vh` / `min-h-screen` / `12vh` 在移动端地址栏收放时跳动。

- [ ] `src/components/layout/AppLayout.tsx`：`h-screen` → `h-dvh`
- [ ] `src/pages/LoginPage.tsx`：`min-h-screen` → `min-h-dvh`
- [ ] `src/components/RequireAuth.tsx`（2 处）：`min-h-screen` → `min-h-dvh`
- [ ] `src/components/ui/Modal.tsx`：`max-h-[85vh]` → `max-h-[85dvh]`
- [ ] `src/components/CommandPalette.tsx`：`pt-[12vh]` → `pt-[12dvh]`
- [ ] `grep -rn "h-screen\|min-h-screen\|vh\]" src` 复查

**验收**：`npm run build`；移动视口截图登录页/日志页，地址栏模拟收放时无内容跳动。

---

### O24 主题元信息（S）

**问题**：`index.html` 缺 `color-scheme` 与 `theme-color`。深色首屏可能白闪，移动端浏览器 UI 不跟随主题。

- [ ] `index.html` `<head>` 加：
      ```html
      <meta name="color-scheme" content="light dark" />
      <meta id="theme-color-meta" name="theme-color" content="#f1f5f9" />
      ```
- [ ] 首屏内联脚本按 `dark` 布尔值同步 meta：`dark ? '#020617' : '#f1f5f9'`
- [ ] `src/lib/theme.ts` 的 `applyTheme` / `commitTheme` 同步更新 `#theme-color-meta`
      （手动切换浅/深/系统时都要更新；system 跟随 `matchMedia` 变化已有监听）
- [ ] 不引入两套 media 版 theme-color，避免手动切换与系统偏好冲突。

**验收**：`npm run build`；切换三态主题时 `document.querySelector('meta[name=theme-color]').content` 随之变化；深色首屏无白闪（DevTools 慢速网络）。

---

### O25 尊重 `prefers-reduced-motion`（S）

**问题**：弹层/侧栏/骨架动画无条件运行，系统「减少动态效果」未生效。

- [ ] `src/index.css` 加：
      ```css
      @media (prefers-reduced-motion: reduce) {
        .animate-pop-in,
        .animate-dock-in-right {
          animation: none;
        }
      }
      ```
- [ ] `src/components/layout/Sidebar.tsx`：宽度过渡加 `motion-reduce:transition-none`
- [ ] `src/components/ui/Skeleton.tsx`：`animate-pulse` 加 `motion-reduce:animate-none`
- [ ] `src/pages/LogsPage.tsx` 实时刷新按钮的 `animate-pulse` 加 `motion-reduce:animate-none`
- [ ] 保留 `animate-spin`（功能性加载指示，不属装饰性位移）
- [ ] W2 的原生 dialog 入场动画同样要在 `prefers-reduced-motion: reduce` 下关闭

**验收**：Playwright `emulateMedia({ reducedMotion: 'reduce' })`，打开弹层/命令面板，
`getComputedStyle(el).animationName === 'none'`；正常模式动画仍在。

---

### O26 表格可访问名 + 键盘横向滚动（M）

**问题**：四个表格页无 caption/可访问名；横向滚动容器不可键盘聚焦。

- [ ] `src/components/ui/Table.tsx`
  - [ ] `Table` 增加 `label?: string`，渲染 `<caption className="sr-only">{label}</caption>`
  - [ ] 新增 `TableScroll`：`<div role="region" aria-label={label} tabIndex={0} className="overflow-x-auto focus-visible:outline-2 focus-visible:outline-offset-[-2px] focus-visible:outline-sky-500">`
  - [ ] `TH` 默认 `scope="col"`（允许 props 覆盖）
- [ ] 替换四处 `overflow-x-auto` 包裹层 + 给 Table 传 label：
  - [ ] `src/pages/LogsPage.tsx`（发送日志）
  - [ ] `src/pages/AuditPage.tsx`（审计日志）
  - [ ] `src/pages/KeysPage.tsx`（API Key 列表）
  - [ ] `src/pages/UsersPage.tsx`（账号列表）
- [ ] 不做粘性表头：`overflow-x-auto` 会同时成为纵向滚动容器，`thead` sticky 不会相对页面滚动生效；要做得重排布局，收益不抵成本。
- [ ] 移动端卡片化（容器查询）留到 W4。

**验收**：`getByRole('region', { name: '发送日志' })` 可见；Tab 能聚焦滚动容器并出现焦点环；
`getByRole('table', { name: '发送日志' })` 可定位；`npm run build`。

---

### O27 表单错误播报（M）

**问题**：`Field` 错误只是普通 `<span>`；输入框无 `aria-invalid` / `aria-describedby`；
登录页/TOTP 全局错误无 `role="alert"`。

- [ ] `src/components/ui/Input.tsx` 的 `Field`：
  - [ ] `useId()` 生成控件 id / error id / hint id
  - [ ] 结构从 `<label>` 包裹改为 `<div>` + `<label htmlFor>` + 控件
  - [ ] 单个合法 React 元素子节点用 `cloneElement` 注入：
        `id`（尊重子元素已有 id）、`aria-invalid={error ? true : undefined}`、
        `aria-describedby={error ? errorId : hint ? hintId : undefined}`
  - [ ] 子节点不是单个元素时安全降级（不 clone，保留原样）
  - [ ] 错误 `<span id={errorId} role="alert">`；hint `<span id={hintId}>`
  - [ ] 注意 E2E 选择器目前用 `/^中文锚点/` 正则，改成显式 label 后仍匹配；
        原本「可访问名含 hint 文字」的行为会变成「name + description」，属于修正
- [ ] `src/pages/LoginPage.tsx` 两处全局错误 `<p>` 加 `role="alert"`
- [ ] `src/components/settings/TotpCard.tsx` 全局错误同上
- [ ] `src/components/QueryState.tsx` 的 `QueryError` 错误文案加 `role="alert"`
- [ ] 可选：提交失败时把焦点移到第一个错误控件（需要各表单补 ref，工作量大，本轮不做）

**验收**：E2E 登录失败 `getByRole('alert')` 能取到错误；所有 `Field` 表单仍能用
`getByLabel(/^中文锚点/)` 定位；`npm run build`。

---

### O34 Button `loading` 被 `disabled` 覆盖（S，必须）

**问题**：`Button` 先展开受控的 `disabled: nativeRest.disabled || loading`，随后 `{...rest}`
又展开一次；调用方传 `disabled={false}` 时会把 loading 的禁用覆盖掉。实测命中：

- `src/pages/KeysPage.tsx` Webhook 测试按钮：`loading={isPending} disabled={url === ''}`
  → URL 非空时 pending 期间仍可点
- `src/pages/SettingsPage.tsx` 保存设置按钮：`loading={isPending} disabled={events.size === 0}`
  → 有事件时 pending 期间仍可点

- [ ] `src/components/ui/Button.tsx` 调整 spread 顺序：先 `{...rest}`，再展开 `type` / `disabled`
- [ ] `disabled: nativeRest.disabled || loading` 保留；加载期间 `aria-busy={loading || undefined}`
- [ ] `as={Link}` 等非原生元素不要注入 `disabled` / `type`
- [ ] E2E 可选：拦截 PUT `/api/settings` 延迟 1s，点击保存后断言按钮 `disabled`

**验收**：pending 期间按钮不可点；`npm run build`；相关 E2E 通过。

---

### O35 全局 ⌘K 与弹层冲突 / 长按重复（S，必须）

**问题**：`src/components/layout/AppLayout.tsx` 的全局快捷键：

- 不判断 `e.repeat`：按住 K 会反复开关面板
- 不判断当前是否有弹窗：Modal 打开时按 ⌘K 会在其上再开命令面板；两者都用
  `window` keydown 监听，Esc 会同时关闭两层

- [ ] `if (e.repeat) return`
- [ ] 打开前检查 `document.querySelector('dialog[open]:not(#command-palette), [aria-modal="true"]')`；
      存在则忽略（W2 迁移后统一查 `dialog[open]`）
- [ ] 命令面板自身加 `id="command-palette"`，允许开着时再按 ⌘K 关闭
- [ ] 加 E2E：新建渠道弹窗打开时按 Ctrl+K，断言命令面板不出现

**验收**：长按不抖动；弹窗打开时 ⌘K 不叠加；Esc 只关当前层。

---

### O36 路由级 ErrorBoundary + chunk 加载失败恢复（M，必须）

**问题**：全仓无 `ErrorBoundary`。懒加载路由 chunk 在发版后 404 / 网络中断时，
React.lazy 的 rejection 会直接炸到根，白屏且无恢复入口。go:embed 单二进制发版后
旧 index.html 缓存引用旧哈希 chunk 是真实场景。

- [ ] 新增 `src/components/ErrorBoundary.tsx`（class 组件）：
  - `getDerivedStateFromError` + `componentDidCatch`
  - 卡片式兜底：标题、错误信息、「重新加载」按钮（`window.location.reload()`）、
    「返回首页」链接
- [ ] `src/App.tsx`：包住 `<Routes>`（或在 `PageSuspense` 外层按路由 key 重置）
- [ ] `src/main.tsx` 监听 Vite 的 chunk 加载失败事件：
      ```ts
      window.addEventListener('vite:preloadError', (event) => {
        event.preventDefault()
        // 每会话最多自动重载一次，防止坏网络下无限循环
        if (!sessionStorage.getItem('catmail-chunk-reload')) {
          sessionStorage.setItem('catmail-chunk-reload', '1')
          window.location.reload()
        }
      })
      ```
- [ ] 可选 E2E：`page.route('**/assets/*.js', route => route.abort())`，断言兜底卡片出现

**验收**：人为阻断 chunk 后页面显示可恢复的错误卡而非白屏；正常路由不受影响。

---

### O37 fetch 默认超时（S，必须）

**问题**：`src/lib/api.ts` 与 `downloadLogsCsv` 的 fetch 都不带超时。后端假死/网络半开时
请求永久 pending，React Query 一直 `isPending`，用户只能刷新。

- [ ] `src/lib/api.ts` 增加 `timeoutMs?: number`（默认 30_000）
- [ ] 用 `AbortController` + `setTimeout` 合并调用方 `signal`；超时抛
      `ApiError(0, '请求超时，请稍后重试')`，调用方主动取消则原样 rethrow
- [ ] `src/lib/update.ts` 的 `applyUpdate` 单独传长超时（下载二进制，建议 10 分钟）
- [ ] `src/lib/logs.ts` 的 CSV 导出传 120_000（1 万行上限，留足时间）
- [ ] 统一清理 timer，避免泄漏

**验收**：模拟 30s 不返回的请求（Playwright route 延迟），页面出现超时错误而非一直转圈；
更新/导出不被默认超时误杀。

---

### O38 剪贴板统一降级（S）

**问题**：`KeysPage` 有 `execCommand` 降级，`TotpCard` 直接 `navigator.clipboard.writeText`。
自托管常见 http://LAN 非安全上下文，恢复码复制会直接失败。

- [ ] 抽 `src/lib/clipboard.ts`：优先 `navigator.clipboard.writeText`，失败/不存在时
      创建临时 textarea + `execCommand('copy')`，finally 移除
- [ ] `src/pages/KeysPage.tsx`、`src/components/settings/TotpCard.tsx`、
      `src/pages/LogsPage.tsx`（复制 message id）统一调用
- [ ] 失败文案保持「复制失败，请手动选择复制」

**验收**：http 非安全源下恢复码/Key/Message ID 均可复制；https 下走原生 API。

---

### O39 通知面板语义（S）

**问题**：面板是 `role="dialog"`，但打开时不移动焦点、不做焦点限制、不是模态；
语义与行为不一致。铃铛按钮缺 `aria-controls`，角标数字会被读屏重复播报。

- [ ] `src/components/layout/NotificationBell.tsx`：
  - 面板改 `role="region"` + `aria-label="通知列表"`（或实现真正的 dialog 焦点管理）
  - 面板加 `id="notification-panel"`；按钮加 `aria-controls="notification-panel"`
  - 角标 `<span aria-hidden>`（未读数已在按钮 `aria-label` 中）
  - 保持现有 Esc / 点击外部关闭
- [ ] 可选：打开时把焦点移到面板标题或第一条，关闭还原到铃铛

**验收**：读屏不重复播报角标；`aria-expanded`/`aria-controls` 成对；键盘 Tab 可进入面板。

---

### O40 管理后台禁止被索引（S）

- [ ] `index.html` 加 `<meta name="robots" content="noindex, nofollow" />`
- [ ] 可选：后端所有 SPA 响应加 `X-Robots-Tag: noindex, nofollow`
- [ ] 不加 `robots.txt` 也行，meta 足够；重点是别让搜索引擎收录登录页和内页

**验收**：`view-source` 可见；curl 检查 SPA 响应头（若做后端项）。

---

### O41 邮件预览 iframe 隐私（S）

**问题**：`LogsPage` 的 HTML 正文 iframe 已正确使用 `sandbox=""`（脚本/表单隔离，这点没问题），
但远程图片仍会加载，且请求会带当前后台地址作为 Referer。

- [ ] iframe 加 `referrerPolicy="no-referrer"`
- [ ] 可选 `loading="lazy"`
- [ ] 不注入 CSP：`srcdoc` 会继承父页 CSP，容易把邮件自带样式/图片一起挡掉；
      若要做，单独评估 `img-src https: data:` 与 `style-src 'unsafe-inline' https:`
- [ ] 保持 `sandbox=""` 不变

**验收**：预览正常；DevTools 里图片请求无 Referer。

---

## W2 — 原生 `<dialog>` + 图表/时间（O28、O42–O44）

**目标**：用浏览器原生模态能力替换手写遮罩/focus trap/Esc/焦点还原/滚动锁。
收益：top layer、背景 inert、Esc/返回手势、焦点归还、`::backdrop`、嵌套弹窗天然支持。

**涉及**：`src/components/ui/Modal.tsx`、`src/components/CommandPalette.tsx`、
`src/pages/LogsPage.tsx` 的 `LogDetailPanel`、`src/lib/dialog.ts`、`src/index.css`。

### O28.1 公共 CSS 与行为（M）

- [ ] `src/index.css` 增加 `dialog.app-dialog` 基础样式：
  - 面板本体：`margin: auto; padding: 0; border: 0; background: transparent; max-width/max-height: none; color: inherit;`
  - `::backdrop`：`rgb(15 23 42 / .5)` + `backdrop-filter: blur(4px)`
  - `dialog:modal { overscroll-behavior: contain; }`
  - `dialog::backdrop { overflow: hidden; overscroll-behavior: contain; }`（防止背景滚动链）
  - 入场用 `@starting-style` + `animation`；出场若做，需 `transition-behavior: allow-discrete` + `overlay`，否则直接关闭
  - `prefers-reduced-motion: reduce` 下关闭入场动画
- [ ] 新增 `src/lib/useNativeDialog.ts`（或组件内 hook）：
  - `open` 变化 → `showModal()` / `close()`
  - `cancel` 事件 `preventDefault()` + `onClose()`，保证受控状态同步
  - `close` 事件（Esc/返回手势/表单关闭）→ 通知外部
  - 点击遮罩关闭：优先 `closedby="any"`；Safari 不支持时用 guide 的 pointerdown/pointerup
    坐标判定兜底（不要用 `event.target`）
  - 不手写 focus trap、不写 `role="dialog"` / `aria-modal`、不写 `inert`
- [ ] 决定是否保留 `lockScroll(main)`：原生 modal 已 inert 背景；先去掉，用
      `overscroll-behavior` + 移动端实测确认；若 Safari 仍能滚背景，再只锁 `main`
      （不要锁 `html`/`body`）。

### O28.2 Modal（M）

- [ ] 移除 `role="dialog"` / `aria-modal` / `tabIndex={-1}` / 遮罩 div / `useDialogBehavior`
- [ ] `<dialog ref className="app-dialog ...">` + `<h2 id={titleId}>` + `aria-labelledby={titleId}`
- [ ] 关闭按钮仍可见；`autofocus` 放在首个输入或安全操作上
- [ ] 保留 `max-h-[85dvh]`、内部滚动、`X` 关闭、footer 布局
- [ ] E2E：`06-keys` / `03-channels` / `04-channels-test-send` / `07-users`
      的 `getByRole('dialog', { name })` 应继续通过（原生 dialog 隐式 role）

### O28.3 ⌘K 命令面板（M）

- [ ] 改用 `<dialog>`，输入框 `autoFocus`
- [ ] 补 combobox 语义：
  - 输入框 `role="combobox"`、`aria-expanded`、`aria-controls={listboxId}`、
    `aria-autocomplete="list"`、`aria-activedescendant={activeOptionId}`
  - 结果容器 `role="listbox"`、每条 `role="option"` + `aria-selected`
  - 当前项 id 与 `aria-activedescendant` 对应；键盘仍由输入框处理
- [ ] 保留 ↑↓ 循环、Enter 执行、Esc 关闭、搜索过滤、滚动进视口
- [ ] 关闭时重置 query/active（现有 effect 保留）

### O28.4 日志详情停靠面板（M）

- [ ] 改用 `<dialog>`（modal）；`inset-y-0 right-0` 布局改成 dialog 自身定位：
      `margin: 0 0 0 auto; height: 100dvh; width: min(520px, 90vw);`
- [ ] 保留 `animate-dock-in-right` 的视觉效果（用 `@starting-style` + translate）
- [ ] 移除 `useDialogBehavior(!resendConfirm, ...)` 的嵌套 hack：
      重发确认弹窗是另一个 modal dialog，原生 top layer 自动叠放；
      Esc 只关最上层；关闭后焦点回到「重发」按钮
- [ ] `<lg` 占满全屏的样式保留

### O28.5 清理与回归（S）

- [ ] `grep -rn "useDialogBehavior\|lib/dialog" src`，确认无引用后删除 `src/lib/dialog.ts`
      （若 W4 抽屉还需要滚动锁，再抽 `lockScroll` 到独立工具）
- [ ] 全量 E2E + 手动键盘走查：
  - Tab 不逃出弹层；Shift+Tab 反向循环
  - Esc 关闭并还原焦点到触发按钮
  - 点击遮罩关闭（含 Safari 兜底）
  - 嵌套确认弹窗 Esc 只关最上层
  - 背景页面不能滚动/点击
  - 控制台无 React `dialog` 相关警告

**W2 验收**：`npm run build` + `npx playwright test` 全绿；上述手动走查通过；无 `aria-modal` 手写残留。

---

### O42 KPI 趋势与图表可访问性（M）

**问题**：

- KPI 卡的趋势只通过 Sparkline 颜色表达（绿=好/红=坏），Sparkline 本身 `aria-hidden`，
  读屏用户完全拿不到趋势；色觉障碍用户只能看线形，无法区分「好/坏」
- `DailyActivityChart` 没有 `role="img"` / 可访问摘要，读屏只会读到零散的轴刻度文字

- [ ] `src/pages/DashboardPage.tsx` KpiCard：根据 trend 输出 sr-only 文本
      （「近 30 日趋势：上升 / 下降 / 持平」），或加一个小箭头 + 文本
- [ ] `src/components/charts/DailyActivityChart.tsx`：图表容器加 `role="img"` +
      `aria-label`（如「近 7 天发送 123 封，失败 4 封，成功率 96.9%」），
      内部 SVG 对 AT 隐藏
- [ ] 可选：追加一个 `sr-only` 数据表（日期/发送/失败/成功率），给需要明细的读屏用户
- [ ] 保持现有 `isAnimationActive={false}`

**验收**：读屏能读到趋势与图表摘要；视觉不变。

---

### O43 RelativeTime 共享时钟（M）

**问题**：每个 `<RelativeTime>` 实例各自 `setInterval(30s)`。Keys/Channels/Audit 页面
峰值可能同时挂 50–100 个定时器；`LogsPage` 表格又直接用 `formatRelativeTime`，
不随分钟推进刷新（只有实时刷新时才更新），同一产品里两套行为。

- [ ] `src/lib/format.ts` 的 `formatRelativeTime(iso, now = Date.now())` 增加可选时间基准
- [ ] `src/components/RelativeTime.tsx` 改用模块级单一时钟 + `useSyncExternalStore`：
      每 30s 更新一次快照，所有实例共享；`visibilitychange` 隐藏时暂停
- [ ] `src/pages/LogsPage.tsx` 时间列改用 `<RelativeTime>`（同时获得 `<time>` 语义）
- [ ] 保留 `title` 绝对时间

**验收**：页面只有 1 个 30s 定时器（Performance 面板确认）；日志表格时间随分钟推进；
构建/E2E 通过。

---

### O44 仪表盘图表懒加载（S，性能大头）

**现状**：`recharts` chunk 385KB（gzip 110KB），Dashboard 是登录后的落地页，
`DailyActivityChart` 静态 import 导致这 385KB 与 KPI 卡一起首屏加载。

- [ ] `src/pages/DashboardPage.tsx` 把 `DailyActivityChart` 改为 `React.lazy` +
      `<Suspense fallback={<QueryLoading skeleton="chart" />}>`
- [ ] 确认 Vite 仍把 recharts 单独成 chunk；Dashboard 初始 JS 不再依赖它
- [ ] 图表查询仍在页面挂载时发起（数据请求与 chunk 并行，不串行）
- [ ] 可选：Sidebar hover 时预取目标路由 chunk（需把路由 import 集中到 `routes.ts`，
      当前 `App.tsx` 内联 lazy；收益中等，先不做也行）

**验收**：`npm run build` 输出确认；Dashboard 首屏 KPI 先出现、图表骨架随后替换；
E2E 02-dashboard 通过。

---

## W2.5 — 静态资产交付（O45）

**现状**：`backend/cmd/catmail/main.go` 的 `registerStatic` 直接 `http.ServeFileFS`：

- 没有 gzip/brotli：构建产物原始体积约 757KB（gzip 后约 226KB），直连部署全量传输
- 没有 `Cache-Control`：哈希资产每次访问都可能条件请求，白白多一跳
- 没有任何安全响应头：`X-Content-Type-Options` / `X-Frame-Options` / `Referrer-Policy` 均缺失，
  管理后台可被 iframe 嵌套（点击劫持）
- 没有 CSP

- [ ] gzip 中间件（`compress/gzip` + `sync.Pool`，或 `gin-contrib/gzip`）：
      仅压缩 `text/*` / `application/javascript` / `application/json` / `image/svg+xml`，
      加 `Vary: Accept-Encoding`，跳过 `HEAD`、已压缩响应、<1KB 响应
- [ ] `registerStatic`：
  - `assets/` 下哈希文件：`Cache-Control: public, max-age=31536000, immutable`
  - `index.html` 与 SPA fallback：`Cache-Control: no-cache`
  - `X-Content-Type-Options: nosniff`
  - `X-Frame-Options: DENY`
  - `Referrer-Policy: same-origin`
  - `Permissions-Policy: camera=(), microphone=(), geolocation=()`
  - 可选 `X-Robots-Tag: noindex, nofollow`
- [ ] CSP 分两步：
  - 先上 `default-src 'self'; img-src 'self' data: https:; style-src 'self' 'unsafe-inline';
    script-src 'self' 'unsafe-inline'; frame-src 'self' data:; object-src 'none';
    base-uri 'self'; form-action 'self'; frame-ancestors 'none'`
  - 再考虑把 `index.html` 内联主题脚本外置或用 sha256 hash，去掉 `script-src 'unsafe-inline'`
  - 注意 `srcdoc` iframe 继承父页 CSP：`img-src https: data:` 与
    `style-src 'unsafe-inline' https:` 是邮件预览不破的前提
- [ ] 若部署在 Caddy/Nginx 后，确认反代是否已 gzip；直连单二进制场景必须做

**验收**：
```bash
curl -sI -H 'Accept-Encoding: gzip' http://127.0.0.1:8080/assets/<hash>.js | grep -i content-encoding
curl -sI http://127.0.0.1:8080/assets/<hash>.js | grep -i cache-control
curl -sI http://127.0.0.1:8080/ | grep -iE 'x-frame-options|x-content-type-options|referrer-policy'
```
E2E / smoke 全绿；邮件预览仍正常。

---

## W3 — 语义、对比度、表单细节（O29–O31、O46–O49）

### O29 浅色对比度（M，视觉会变，执行前确认）

**问题**：104 处 `text-slate-400`，白底 2.94:1，低于 AA 4.5:1；常见组合
`text-slate-400 dark:text-slate-500` 在深色底也不足。

- [ ] 方案 A（推荐）：把「文本用」的 `text-slate-400 dark:text-slate-500` 成对替换为
      `text-slate-500 dark:text-slate-400`；图标/装饰保留原色
- [ ] 方案 B：统一切到语义 token `text-text-muted`（已是 slate-500 / slate-400）
- [ ] 逐页视觉复核：时间戳、hint、占位符、分页信息、空态说明
- [ ] 占位符单独检查 `::placeholder`（当前 `placeholder:text-slate-400`）
- [ ] 不做全站无差别替换，避免图标/分隔线被压暗

**验收**：抽查文本对比度 ≥ 4.5:1（大字号 ≥ 3:1）；亮/暗两套截图复核无层级塌陷。

---

### O30 语义细节（S）

- [ ] `src/components/RelativeTime.tsx`：`<span>` → `<time dateTime={iso}>`（保留 title）
- [ ] 日志/审计/渠道筛选区用 `<search>` 包裹（`forms` guide）
- [ ] 标识类输入补 `autoCorrect="off" autoCapitalize="none" spellCheck={false}`：
      用户名、搜索、API Key 名称、Webhook URL 等
- [ ] 登录页用户名 `enterKeyHint="next"`，密码/验证码 `enterKeyHint="done"`
- [ ] `AppLayout` 加 skip link（`href="#main-content"`，`sr-only focus:not-sr-only`），
      `<main id="main-content">`
- [ ] 通知铃铛按钮补 `aria-controls`，面板补 id（`aria-expanded` 已有；O39 已覆盖）
- [ ] 可选：`OnboardingChecklist` 的 `<span aria-label>` 加 `role="img"` 或改成
      visually-hidden 文本，避免部分 AT 忽略 generic span 的 aria-label

---

### O31 触摸目标（S，需设计确认）

- [ ] `@media (pointer: coarse)` 下把顶栏图标按钮、侧栏导航、分页按钮提升到 44px
- [ ] 表格行内紧凑按钮不放大（避免行高膨胀）
- [ ] 用媒体查询隔离，桌面密度不变

---

### O46 表单 `name` 与表单可访问名（S）

- [ ] 登录页用户名/密码/验证码/恢复码补 `name`（`username` / `password` / `code` /
      `recovery_code`），帮助密码管理器识别（`autocomplete` 已有）
- [ ] Users/Keys/Channels/Settings 的输入补稳定 `name`（至少认证与密钥类）
- [ ] `SettingsPage` 三个表单补 `aria-label`（「轮换与重试」「通知设置」「修改密码」），
      页面同时存在多个 form，读屏需要区分
- [ ] 不改变现有 `noValidate` + 自定义中文校验

**验收**：密码管理器可保存/填充；读屏表单列表可区分；E2E 标签定位不变。

---

### O47 日志表格行键盘模型（S，执行前确认）

**问题**：`LogsPage` 给 `<tr>` 加 `tabIndex` 和 ↑↓/Enter，但行没有 `role="grid"` /
`aria-selected`，读屏不会把它当成可操作控件；每行已经有一个「详情」按钮。

- [ ] 方案 A（推荐）：移除 `TRow` 的 `tabIndex` / `onKeyDown`，键盘用户 Tab 到「详情」按钮；
      行点击仅服务鼠标，表格保持原生语义
- [ ] 方案 B：保留 ↑↓ 导航，升级为 `role="grid"` + `aria-selected` + `aria-activedescendant`，
      工作量明显更大
- [ ] 顺带：`DashboardPage` 的 `ErrorHint` 仅 hover 显示全文，键盘/触摸不可达；
      改 `tabIndex={0}` + focus 显示，或用 `<details>`

**验收**：屏幕阅读器按原生表格播报；键盘可打开详情；不再有「可聚焦但无角色」的行。

---

### O48 `@types/qrcode` 归位（XS）

- [ ] `package.json`：`@types/qrcode` 从 `dependencies` 移到 `devDependencies`
      （仅类型，运行时不需要）
- [ ] `npm install` 后 build 不受影响

---

### O49 ⌘K 提示按平台显示（XS）

- [ ] `AppLayout` 顶栏 kbd 当前恒显示 `⌘K`；Windows/Linux 显示 `Ctrl K`
      （`navigator.userAgentData?.platform` 或 `navigator.platform`，按 Mac 判断）
- [ ] 按钮加 `aria-keyshortcuts="Meta+K Control+K"`
- [ ] 命令面板底部提示保持通用

---

## W4 — 移动端适配（O32–O33；执行前确认，需改 SPEC §482）

### O32 侧栏改抽屉（L）

**现状**：侧栏固定 15rem，390px 视口下内容区仅约 150px；SPEC §482 目前写「窄屏恒为展开态」。

- [ ] 改 SPEC §482：≥1024px 保留可折叠侧栏；<1024px 改为抽屉
- [ ] `<lg` 隐藏侧栏，顶栏加菜单按钮；抽屉打开时遮罩 + 焦点管理 + Esc 关闭
- [ ] 路由切换自动关闭抽屉
- [ ] 抽屉可用原生 `<dialog popover>` 或现有 dialog 基础；注意 safe-area
- [ ] E2E：390×844 视口打开/关闭/导航

### O33 表格移动端卡片化（L）

- [ ] 用 `@container` 在卡片宽度 <600px 时切换堆叠布局
- [ ] `thead` 隐藏、每格 `::before` 注入表头标签（CSS 变量映射）
- [ ] 优先 Logs / Audit；Keys / Users 可后置
- [ ] 保留横向滚动作为降级路径
- [ ] 屏幕阅读器复核：隐藏 thead 后单元格仍需有可理解上下文

---

## W5 — 可选性能项（O50）

- [ ] Sidebar hover/focus 预取目标路由 chunk（需把 lazy import 集中到 `routes.ts`）
- [ ] 静态资源 brotli 预压缩（需构建插件 + 后端优先 `.br`，收益相对 gzip 约 15%）
- [ ] 页面级 View Transitions（E2E 截图不确定性高，暂缓）

---

## 不做 / 暂缓（避免过度工程）

- [ ] 通知面板改 Popover API：锚定定位（anchor positioning）在 Safari 支持不齐，现有 pointerdown + Esc 足够
- [ ] 粘性表头：与横向滚动容器冲突，需要重排表格布局
- [ ] 原生表单校验重写：现有 `noValidate` + 自定义中文错误文案是产品选择
- [ ] 全局字体替换/引入 Web Font：当前系统字体栈已够，避免 FOIT/体积
- [ ] CSP 直接去掉 `'unsafe-inline'`：先上基础 CSP，hash/外置脚本单独一轮

---

## 统一验收命令

```bash
# 前端构建（零错误）
cd frontend && npm run build

# E2E（脚本内部会重新构建前端 + go build + 临时库，端口 18100）
cd frontend && npx playwright test

# 后端
cd backend && go build ./... && go vet ./... && go test ./...

# 集成冒烟（可选，140 项）
cd .. && bash scripts/smoke.sh
```

**注意**：
- 前端构建会写入 `backend/internal/web/dist`，go:embed 二进制需重新 `go build` 才生效。
- E2E 前确认 18100 无残留：`lsof -ti tcp:18100 | xargs kill -9`。
- W1 一个 commit，W2 一个 commit，W2.5 一个 commit；W3/W4 执行前确认后另开。
- `frontend/OPTIMIZATION.md` 是唯一执行清单，后续调整直接改本文件。
