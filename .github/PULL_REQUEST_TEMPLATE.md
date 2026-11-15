<!-- 标题与提交信息同风格：<type>(<scope>): 中文描述，例如 feat(backend): 渠道凭据静态加密。 -->

## 变更内容（简述）

<!-- 做了什么、为什么；安全漏洞修复请勿在标题与描述中泄露可复现细节。 -->

## 关联

<!-- 关联 issue / SPEC.md 章节（如 SPEC §X）；无则写「无」。 -->

## 变更类型（勾选全部适用项）

- [ ] `feat` 新功能
- [ ] `fix` 缺陷修复
- [ ] `docs` 文档
- [ ] `refactor` / `test` / `chore` 重构、测试、构建或杂项
- [ ] **行为变更**（影响对外契约：API 语义 / 环境变量 / 数据模型 / 日志与错误码——勾中即触发下方契约优先项）

## 自测清单（提交前逐项确认，不适用项划掉并注明原因）

- [ ] 后端测试通过：`cd backend && go test -race ./...`
- [ ] `cd backend && gofmt -l .` 无输出，`go vet ./...` 通过
- [ ] 前端构建通过：`cd frontend && npm run build`（改动了前端时必做——`go:embed` 吃的就是这份产物）
- [ ] i18n 门禁通过：`cd frontend && node scripts/check-i18n.mjs`（新增界面文案时必做，zh/en 词典同构）
- [ ] E2E 通过：`cd frontend && npm run e2e`（改动了前端或后端 UI 相关逻辑时必做；端口 18100，勿与 smoke 18080 并行）
- [ ] **SPEC.md 已同步**：行为变更先落契约再实现，SPEC / 实现 / 测试三者一致（无行为变更可跳过）
- [ ] **CHANGELOG.md 已加条目**：加到 `Unreleased` 对应分组，每条一行、面向使用者
- [ ] UI 变更已附截图（深/浅色主题；移动端布局变更另附窄视口截图）
- [ ] 文档联动：README.md / docs/API.md 中涉及的使用者可见内容已更新
