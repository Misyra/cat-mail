# 贡献指南

感谢关注 CatMail！动手写代码之前，请先读两份文档：

- [SPEC.md](SPEC.md) —— 行为契约。实现、测试、文档全部以它为准，改动前先看它怎么约定的。
- [README.md](README.md) —— 面向使用者的功能说明与部署文档。

安全漏洞请勿通过 issue 报告，走 [SECURITY.md](SECURITY.md) 的私密披露渠道。

## 开发环境

- Go **1.27+**（以 `backend/go.mod` 的 `go` 指令为准）
- Node.js **22**（与 CI 一致；Vite 7 的底线是 20.19+，建议直接用 22 LTS）
- Bash（运行 `scripts/` 下的脚本）

## 本地开发

**最大的坑：`go:embed` 依赖前端构建产物，必须先构建前端，再编译后端。** 后端通过 `go:embed` 打包前端产物，输入目录是 `backend/internal/web/dist`——该目录被 `.gitignore` 忽略（克隆后不存在），且 `//go:embed dist` 要求目录非空，否则编译直接报错。每次改动前端后都要重新 `npm run build`，改动才会进入二进制：

```bash
# 1. 构建前端（tsc 类型检查 + vite 打包，产物输出到 backend/internal/web/dist）
cd frontend
npm install
npm run build

# 2. 编译并运行后端（数据落 ./data，可用 CATMAIL_ADDR/CATMAIL_DB 换端口与库做本地隔离）
cd ../backend
go build -o catmail ./cmd/catmail
./catmail
```

E2E 不受陈旧产物影响：`scripts/e2e-server.sh` 每次都会重新构建前端再拉起一次性后端。

## 测试与检查（提交前全绿）

```bash
# 后端测试（CI 同款：-race 且 -count=1）
cd backend && go test -race ./...

# gofmt 与 go vet（CI 有同款门禁，gofmt -l 输出非空即失败）
cd backend && gofmt -l . && go vet ./...

# 前端构建（含 tsc -b 全量类型检查）
cd frontend && npm run build

# i18n 门禁：新增界面文案必须 zh/en 词典同构，否则此脚本报错
cd frontend && node scripts/check-i18n.mjs

# E2E（改动了前端或后端 UI 相关逻辑时必跑）
cd frontend && npm run e2e
```

E2E 说明：Playwright `webServer` 自动调 `scripts/e2e-server.sh` 拉起一次性后端——前端构建 + `go build` + 临时库，跑完自动清理。端口约定：**E2E 固定 18100**，smoke 集成冒烟 18080、走查 18081，三者不要同时跑，避免端口冲突；E2E 全程 mock 渠道、不出网。E2E 用例与截图类验收的执行口径见 [PLAN.md](PLAN.md) 的留档（每轮优化用什么验证、截图怎么落档都在里面）。

## 提交信息

仿现有提交风格：`<type>(<scope>): 中文描述`，type 主要是 `feat` / `fix` / `docs` / `chore`（`refactor` / `test` 酌情），scope 为 `frontend` / `backend` 或具体模块。描述写清"做了什么、为什么"，一行说不完用破折号接着写：

```text
feat(backend): 渠道凭据静态加密——AES-256-GCM 落库 + 启动期一次性迁移
feat(frontend): 移动端适配与 W3 收尾——侧栏抽屉(<lg)/表格堆叠卡片化(<600px)
fix(backend): 适配器对齐官方 API、并发额度原子化
docs: CHANGELOG/PLAN 同步——前端现代化两轮变更清单
chore: Docker 部署/CI 测试与发布流水线/smoke 集成冒烟
```

## 契约优先（本仓库的核心约定）

- **行为变更必须先改 [SPEC.md](SPEC.md)**。SPEC 是行为契约：先在 SPEC 落稿新行为（或修改旧契约），再实现、再补测试；PR 里 SPEC、实现、测试三者一致才算完成。只改实现不改 SPEC 的 PR 会被打回。
- **[CHANGELOG.md](CHANGELOG.md) 每条一行、面向使用者**：写"使用者的哪些体验/接口/配置发生了什么变化"，不写实现波次与内部验收细节；实现留档进 [PLAN.md](PLAN.md)。条目加到 `Unreleased` 下对应的 Added / Changed / Fixed / Removed 分组。
- 文档联动：使用者可见的功能、环境变量变更同步 [README.md](README.md)；接口契约变更同步 [docs/API.md](docs/API.md)。

## PR 流程

1. 从 `main` 拉分支开发（一个 PR 聚焦一件事）；
2. 按「测试与检查」跑完全部门禁，UI 变更附截图（深/浅色主题，移动端布局变更另附窄视口截图）；
3. 提 PR **目标分支 `main`**，标题与提交信息同风格（模板里有自测清单，逐项勾选后再请求 review）；
4. CI 必须绿（gofmt / vet / `go test -race` / 前端构建）才进入评审。

社区互动请遵循 [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md)。
