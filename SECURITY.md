# Security Policy / 安全策略

CatMail（猫邮）是多供应商事务性邮件 failover 网关，自身托管着三类高价值敏感信息：**渠道凭据**（各邮件平台 API Key/Secret、SMTP 凭据）、**管理员会话**（v3 session token、TOTP 密钥与恢复码）与**发送数据**（日志含邮件正文与收件人）。如果你发现的问题可能涉及这些数据的机密性或完整性，请务必走下面的私密披露渠道，不要使用公开 issue。

---

## English

### Supported Versions

We only maintain the **latest version**. Older releases do not receive security patches — please upgrade before reporting.

| Version | Supported |
| --- | --- |
| latest release (or `main` tip, before the first tag is cut) | yes |
| any older release / commit | no |

> The repository has not been tagged yet. Until the first release is published, "latest" means the tip of `main`; this table will list the newest `vX.Y.Z` afterwards.

### Reporting a Vulnerability

**Primary channel: GitHub Private Vulnerability Reporting.** Open the repository page → **Security** tab → **Report a vulnerability**. This works as soon as the repository is pushed to GitHub and requires no email; reports and discussion stay private by default.

**Backup channel: none yet.** <!-- TODO(上线后补充)：如需加密邮箱或其他备用渠道，在此补充具体地址；当前不提供，也不设占位地址，避免留下看似可用的假联系方式。 -->

Please include:

- Version (tag / image tag / commit SHA) and deployment mode (Docker / binary / from source);
- Database in use (SQLite / PostgreSQL) and relevant environment variables — **redacted**;
- Reproduction steps and, where applicable, a PoC (request/response, minimal script);
- Log excerpts — **sanitized** (see below).

### Never disclose credentials — anywhere

The project's surface is full of live secrets. In screenshots, logs, issue text and even private reports:

- Do **not** paste real channel API keys/secrets, SMTP credentials, admin session tokens, TOTP seeds or recovery codes, or `CATMAIL_SECRET_KEY`. If a value is essential to the report, show only a short prefix plus its length (e.g. `re_<12 more chars>`).
- If you leaked a real credential while investigating, **rotate it immediately** (disable/re-issue the channel key in the admin UI, rotate keys on the provider side), then continue the report.

### Response Commitments

- **Acknowledgement within 48 hours** of your report;
- **Assessment and a fix plan within 7 days** (severity, affected scope, ETA) — complex fixes may take longer, and we will keep you updated;
- Coordinated disclosure: we will credit you in the release notes unless you prefer to stay anonymous.

### Scope

**In scope:** the CatMail project itself — backend, admin frontend, embedded docs, official Dockerfile / compose file, CI and release pipeline.

**Out of scope:**

- Security issues of **self-deployed environments** (unchanged default password `admin/admin123`, admin panel exposed to the public internet without `CATMAIL_COOKIE_SECURE` / `CATMAIL_ADMIN_CIDR`, unprotected `data/` directory, etc.). These are deployment configuration problems; the recommendations live in [README.md](README.md#安全注意事项) and are considered intended behavior.
- Compromise of your accounts on the upstream email platforms.
- Vulnerabilities in third-party dependencies — please report those upstream first. If CatMail pins a version with a known flaw, that is in scope.

Behavior that contradicts the documented contract counts as a vulnerability even without an explicit "security" label. The intended behavior is documented in [README.md](README.md) (合规与使用须知 → 安全注意事项) and [SPEC.md](SPEC.md) (§7 后端补充要求的安全条目与 §4 数据模型；凭据加密、Webhook 签名、会话 v3、登录限流、TOTP、审计日志等设计均以此为准)。

Good-faith research following this policy is authorized and will not be pursued as a violation, as long as you avoid privacy violations, data destruction and service degradation.

---

## 中文

### 支持版本

仅维护**最新版本**，旧版本不提供安全补丁，请在升级到最新版后再确认问题是否存在。

| 版本 | 是否支持 |
| --- | --- |
| 最新发布版（首个 tag 发布前 = `main` 分支最新提交） | 是 |
| 其他更早版本 / 提交 | 否 |

> 仓库尚未打正式版本 tag；首个版本发布后，本表以最新 `vX.Y.Z` 为准。

### 报告漏洞

**主渠道：GitHub Private Vulnerability Reporting（私密漏洞报告）。** 仓库页面 → **Security** 标签页 → **Report a vulnerability**。仓库推送到 GitHub 后即可使用，无需邮箱；报告内容与讨论默认保持私密。

**备用渠道：暂无。** <!-- TODO(上线后补充)：如需加密邮箱或其他备用渠道，在此补充具体地址；当前不提供，也不设占位地址，避免留下看似可用的假联系方式。 -->

报告请尽量包含：

- 版本号（tag / 镜像 tag / commit SHA）与部署方式（Docker / 二进制 / 源码）；
- 数据库类型（SQLite / PostgreSQL）及相关环境变量——**须脱敏**；
- 复现步骤，尽量附 PoC（请求/响应、最小脚本）；
- 日志片段——**须脱敏**（见下）。

### 任何场合都不要披露真实凭据

本项目的数据面遍布可用密钥。无论截图、日志、issue 正文还是私密报告：

- 请**不要**粘贴真实的渠道 API Key/Secret、SMTP 凭据、管理员会话 token、TOTP 密钥或恢复码、`CATMAIL_SECRET_KEY`。确需展示时，只给短前缀 + 长度（例如 `re_<后 12 位省略>`）。
- 调查过程中若已泄露真实凭据，请**立即轮换**（在后台停用并重建渠道 Key、到对应平台轮换密钥），再继续完成报告。

### 响应承诺

- 收到报告后 **48 小时内确认**；
- **7 天内完成评估并给出修复计划**（严重程度、影响范围、时间表）——复杂修复可能更久，期间会持续同步进展；
- 采用协同披露，修复发布时在变更日志中致谢（可按你的要求匿名）。

### 范围说明

**范围内：** CatMail 项目本身——后端、管理前端、内嵌文档页、官方 Dockerfile / compose、CI 与发布流水线。

**范围外：**

- **用户自部署环境的配置问题**：未修改默认口令 `admin/admin123`、管理后台直接暴露公网且未设置 `CATMAIL_COOKIE_SECURE` / `CATMAIL_ADMIN_CIDR`、`data/` 目录未妥善保护等。这些属于部署运维问题，官方建议见 [README.md](README.md#安全注意事项)，按建议部署即视为预期行为。
- 各邮件平台侧账号被盗等平台自身安全问题。
- 第三方依赖的通用漏洞——请优先提交上游；若 CatMail 锁定了已知有漏洞的依赖版本，则属于范围内。

行为与文档契约不符即视为漏洞，无需带"安全"标签。预期行为以 [README.md](README.md)（合规与使用须知 → 安全注意事项）与 [SPEC.md](SPEC.md)（§7 后端补充要求的安全条目与 §4 数据模型；凭据 AES-256-GCM 静态加密、Webhook HMAC 签名、会话 v3 注册表、登录限流、TOTP、审计日志等设计均以此为准）为准。

按本策略善意开展的安全研究属于授权行为，不会被追究——前提是不造成隐私侵犯、数据破坏或服务降级。
