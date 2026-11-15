# SMTP 入口（可选）

> 本文迁自 README，面向用 SMTP 协议接入 CatMail 的场景。HTTP API 侧的同口径契约——三种接入形态、AUTH 细节、状态码映射完整表、WordPress 配置示例、SMTP 环境变量表——见 [docs/API.md「SMTP 接入」](./API.md#10-smtp-接入)。


只支持 SMTP 协议的本地应用（老框架、设备、内网系统）无需改造，把 CatMail 当普通 SMTP 服务器即可：设置环境变量 `CATMAIL_SMTP_ADDR=:2525` 启用（默认关闭，不监听任何端口）。邮件经统一轮换路径发送，额度预留/渠道限速/Key 配额/failover/发送日志/失败通知与 HTTP API **完全同口径**；`CATMAIL_SMTP_MAX_SIZE_MB`（默认 25）控制单封邮件上限。

- **防开放中继**：按对端 IP 逐连接判定——loopback 来源（本机应用）免认证；远程来源必须以 `AUTH PLAIN|LOGIN` 认证，密码为某个启用的 API Key（用户名任意），未认证在 RCPT 前即 `530` 拒绝。
- **传输安全（TLS，可选）**：配置 `CATMAIL_SMTP_CERT_FILE`/`CATMAIL_SMTP_KEY_FILE` 后——明文监听广播 **STARTTLS**（RFC 3207，客户端在 EHLO 后、发信前协商升级，升级成功复位会话状态并要求重新 EHLO）；`CATMAIL_SMTP_TLS_ADDR`（如 `:465`）可增开 **465 风格隐式 TLS 监听**（accept 后立即 TLS 握手，防开放中继/AUTH/额度语义与明文入口完全一致）；`CATMAIL_SMTP_TLS_POLICY=require` 时明文监听必须先 STARTTLS 才允许 AUTH（否则 `530`），防明文密码泄露。未配证书行为与旧版完全一致（纯明文、不广播 STARTTLS），公网加密仍可由反代/隧道（stunnel、frp、cloudflared 等）承担 TLS 终结后转发到本机；证书/策略配置非法启动即退出。
- **邮件模板（报文头指令）**：DATA 报文携带 `X-CatMail-Template: <模板ID>`（可选 `X-CatMail-Template-Vars: <JSON>` 或 `base64:<base64(JSON)>`）即按后台模板渲染后发送，语义与 API 的 `template_id` + `variables` 完全同口径（变量形态与渲染语义详见 [docs/API.md「SMTP 使用邮件模板」](./API.md#smtp-使用邮件模板)）；模板不存在或与报文自带正文并存（`body_conflict`）按内容类永久失败 `501` 拒绝。
- **状态码映射**：受理成功 `250`；参数/内容问题（含抑制列表命中 `recipient_suppressed` 与模板错误）`501`（永久失败）；额度/限流 `452`；候选渠道穷尽/内部错误 `451`（临时失败，可重试）；体积超限 `552`；收件人超 100 个 `452`；未认证 `530`；TLS 策略要求升级 `530`。失败会按 HTTP API 同样规则触发冷却/禁用/通知（完整映射表见 [docs/API.md「SMTP 接入」](./API.md#10-smtp-接入)）。
## 在 Docker 部署中启用

Docker 部署需要同时映射 SMTP 端口并显式设置监听地址（镜像内该功能默认关闭）：

```bash
docker run -d --name catmail \
  -p 8080:8080 -p 2525:2525 \
  -v ./data:/data \
  -e CATMAIL_SMTP_ADDR=:2525 \
  -e CATMAIL_SMTP_MAX_SIZE_MB=25 \
  catmail:latest
```

docker compose 在 `environment` 中加 `CATMAIL_SMTP_ADDR: ":2525"`，并在 `ports` 中加 `- "2525:2525"` 即可。容器内应用属于非 loopback 来源，必须用 API Key 作为 SMTP 密码完成认证（见上文）；认证失败或未认证的连接一律拒绝转发，不会变成开放中继。

## 环境变量

全部 SMTP 环境变量（`CATMAIL_SMTP_ADDR` / `CATMAIL_SMTP_MAX_SIZE_MB` / `CATMAIL_SMTP_TLS_ADDR` / `CATMAIL_SMTP_CERT_FILE` / `CATMAIL_SMTP_KEY_FILE` / `CATMAIL_SMTP_TLS_POLICY`）见 [README「环境变量」](../README.md#环境变量)与 [docs/API.md](./API.md#10-smtp-接入) 的 SMTP 环境变量表；非法 TLS 组合（只配半套证书、`CATMAIL_SMTP_TLS_ADDR` 无证书、`require` 无证书等）一律**启动即退出**（快速失败），绝不带病启动。
