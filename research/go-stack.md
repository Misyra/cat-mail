# Snowmail 调研报告:Go 技术栈与架构分析

> 调研日期:2026-11-11。数据来源:GitHub REST API(repo/releases 元数据)、项目 README、one-api/new-api 关键源码(raw.githubusercontent.com)。所有版本号与日期均为当日实时抓取。

---

## 1. Web 框架选型:Gin vs Echo vs Fiber vs chi

### 1.1 现状数据(2026-11-11 抓取)

| 框架 | 最新版本 | 发布时间 | Stars | 最近代码推送 | 底层 |
|---|---|---|---|---|---|
| **Gin** | v1.12.0 | 2026-02-28 | 89,619 | 2026-10-10 | `net/http` |
| **Echo** | v5.4.4 | 2026-10-30 | 32,798 | 2026-10-30 | `net/http`(v5 为当前主版本) |
| **Fiber** | v3.7.0 | 2026-11-09 | 40,263 | 2026-11-10 | **fasthttp**(非 `net/http`) |
| **chi** | v5.5.0 | 2026-10-14 | 23,079 | 2026-11-07 | `net/http` |

四个项目全部处于活跃维护状态,没有明显的"停更"风险。Fiber 发布节奏最快(v3 系列迭代频繁),Gin 版本号保守但生态最大。

### 1.2 对比要点

- **Gin**:事实标准。中间件/文档/示例生态最大,几乎所有中文社区资料、第三方集成(如 GORM 配套、casbin、swagger)都以 Gin 为第一目标。one-api 与 new-api 后端均为 Gin(源生自 gin-template),借鉴它们的代码结构时零摩擦。`c.Request` 就是标准 `*http.Request`,做反向代理/流式转发完全兼容标准库。内置 `Static`/`StaticFS` + `NoRoute` 兜底,可顺带服务管理后台 SPA。
- **Echo**:API 设计干净,v5 已落地且仍在快速修 bug(2026-10-30 刚发 v5.4.4 修代理头问题)。生态略小于 Gin。
- **Fiber**:性能跑分好、开发活跃,但基于 fasthttp,**不是 `net/http`**。对"网关"型应用是硬伤:HTTP 反代、流式 body 复制、与标准库 http.Client/http.Handler 生态的互操作都要走适配层,且 fasthttp 在 header 处理、连接复用等边界行为上有差异。邮件网关大量工作是"收请求 → 改写 → 转发到上游 API",收益撑不起这个风险。
- **chi**:纯标准库路由,零依赖、API 稳定(v5.x 多年兼容),`http.Handler` 原生可组合,最适合"把管理后台和 API 装进一个 http.Server"的极简派。缺点:中间件生态和文档量小于 Gin,没有内置静态文件便捷方法(需自己写 `http.FileServer` + SPA fallback)。

### 1.3 推荐

**推荐 Gin(v1.12.0)。** 理由:
1. 与 one-api/new-api 同栈,渠道抽象、重试、计费等设计可以直接"对着源码抄结构",甚至能参考其 `middleware/distributor.go` 的写法;
2. `net/http` 兼容,适配"代理转发"型网关;
3. 生态最大,管理后台(Go embed 静态文件)+ API 一体化部署有大量现成模式(`gin.StaticFS` + `router.NoRoute` SPA fallback);
4. 中文资料最多,符合自部署项目的受众。

备选 chi(纯标准库极简派)。**不推荐 Fiber** 用于本项目(fasthttp 与代理转发场景不匹配)。

---

## 2. 参考架构:one-api 与 new-api

### 2.1 项目概况

| | one-api (songquanpeng/one-api) | new-api (QuantumNous/new-api) |
|---|---|---|
| Stars | 37.4k | 51.7k |
| 协议 | MIT | AGPLv3 + 附加 Section 7 条款(闭源商用需商业授权) |
| 栈 | Go + Gin,React 前端 | Go + Gin(RelayKit 独立模块),React 19 |
| 存储 | SQLite 默认,`SQL_DSN` 切 MySQL/PG,可选 Redis | SQLite/MySQL/PG,日志可选 ClickHouse,Redis |
| 定位 | LLM API 聚合中转,单二进制自部署 | one-api 活跃分支,功能更重 |

### 2.2 渠道(Channel)抽象(one-api `model/channel.go`)

- `Channel` 核心字段:`Type int`(上游平台类型枚举)、`Key string`(凭证)、`Status int`(`Unknown=0 / Enabled=1 / ManuallyDisabled=2 / AutoDisabled=3`,**手动禁用与自动禁用分开建模**,自动禁用可被自动恢复)、`Priority *int64` 与 `Weight *uint`(指针 + nil 安全访问器,零值默认)、`BaseURL`、`Models`(逗号分隔)、`Group`、`ModelMapping`(JSON:请求模型名 → 上游模型名重定向)、`UsedQuota int64`、`Balance float64`、`ResponseTime/TestTime`(定期测试记录)。
- **安全细节**:列表/搜索/批量查询统一 `Omit("key")`,凭证不随管理接口批量出库。
- **Ability 表**(选路索引):每个 (group, model, channel) 三元组一行,仅含 enabled 渠道;Channel 的增删改通过 `AddAbilities/UpdateAbilities/DeleteAbilities` 同步,状态变更走 `UpdateAbilityStatus`。选路时按 group+model+enabled 过滤 abilities,再取最高优先级层内随机。

### 2.3 负载均衡与选路

- **one-api**(`model/ability.go` `GetRandomSatisfiedChannel`):SQL 内用相关子查询取该 (group, model) 的 `MAX(priority)`,限定同层候选后 `ORDER BY RANDOM()` **等概率**抽取(开源版没有按 weight 加权,weight 字段存在但主要在 pro/衍生版生效);`ignoreFirstPriority=true` 时放开优先级限制,用于重试时下沉到低优先级层。
- **new-api**(`service/channel_select.go` `SelectChannelForRequest`):优先级链为 **钉死渠道(如 `KEY-CHANNEL_ID` 指定)> 会话亲和(仅首次尝试)> `CacheGetRandomSatisfiedChannel(retry)`**;**retry 计数直接作为优先级层下标**:第 0 次重试取最高层,第 1 次取次层,以此类推;`auto` 分组下"每组先穷尽所有优先级层再换下一组"。渠道查询全部走缓存(`CacheGetChannel` 等)。

### 2.4 失败重试(one-api `relay/controller` 的 `Relay`)

- 重试次数 `config.RetryTimes`;`shouldRetry` 决定是否重试:钉死渠道不重试、2xx 与 400 不重试、429/5xx 及其他默认重试;
- 每次重试循环内**重新选渠道**,并记录 `lastFailedChannelId`,抽到同一个就跳过;
- 请求体提前整读缓存,重试时用 `c.Request.Body = io.NopCloser(bytes.NewBuffer(requestBody))` 复原后再次发送;
- 每次失败异步 `go processChannelRelayError(...)` 记录错误并判断是否禁用渠道。源码里自带 `// BUG: bizErr is in race condition` 注释——错误对象在 goroutine 与主流程间共享指针,是我们要避开的坑(错误聚合应在同一线性流程里做)。

### 2.5 自动禁用与恢复(one-api `monitor/`)

- **即时规则禁用**(`ShouldDisableChannel`):`AutomaticDisableChannelEnabled` 开关下,HTTP 401 一律立即禁用;按错误 type/code/消息子串匹配,如 `insufficient_quota`、`invalid_api_key`、`account_deactivated`、`api key expired`、"credit"/"balance"、"已欠费" 等。⚠️ 注意它对裸子串 `"credit"`、`"balance"` 的匹配过宽(误伤正常报错),我们应改为结构化错误分类而非子串匹配。
- **滑动窗口统计禁用**(`metric.go`,`ENABLE_METRIC` 开启):每渠道维护最近 `MetricQueueSize`(约 100)次成功/失败 rolling window;仅失败触发评估,**窗口未填满(预热期)不评估**;成功率低于阈值(README 默认 0.8)即禁用并清空窗口。异步 emit(带缓冲 channel 的生产者-消费者),不阻塞请求路径。
- **自动恢复**(`ShouldEnableChannel`):`AutomaticEnableChannelEnabled` 开启且定期测试成功(请求无错且业务错误为 nil)即自动启用。
- **定期测试**:`CHANNEL_TEST_FREQUENCY` 周期性测试全部渠道并回填 `ResponseTime`;另有渠道余额查询。

### 2.6 额度记账

- 计费公式:`分组倍率 × 模型倍率 ×(提示 token + 补全 token × 补全倍率)`,支持兑换码、按分组差异化倍率;
- **批量聚合更新**:高并发下开启 `BatchUpdateEnabled`,额度变更先进内存队列(`addNewRecord`),定期 flush,避免每请求一次 DB 写——对邮件网关的"每渠道已用配额"计数是直接可抄的模式;
- new-api 侧拆得更细:`billing_session`(请求级计费会话)、`tiered_settle`(分层结算)、表达式定价、订阅配额重置(`subscription_reset_task.go` 定时任务,即"每月重置"的现成参考)、多节点实例心跳(`system_instance.go`)。

### 2.7 可借鉴设计(结论)

1. **渠道抽象 + Ability 索引表 + 四态 Status**(区分手动/自动禁用、自动恢复):映射到邮件网关即 Provider/Account 抽象,模型维度换为"发信域名/发件人"维度;
2. **重试时按优先级层下沉 + 排除上次失败渠道 + 请求体缓冲复用**;错误处理保持线性流程,避免 one-api 的 bizErr 竞态;
3. **双层禁用判定**(结构化即时规则 + 滑动窗口成功率,预热期保护)+ 定期健康测试回填响应时间 + 批量聚合记账;
4. (new-api 额外)**渠道亲和**:按规则把同一身份的请求粘回同一渠道——对应邮件场景即"同一发件域/身份固定走同一账号",对发件域名信誉与限速友好,值得列入路线图。

---

## 3. 存储选型

### 3.1 SQLite 驱动

| | mattn/go-sqlite3 | modernc.org/sqlite |
|---|---|---|
| 最新版 | v1.14.53(2026-11-05) | v1.62.0(2026-11-07,内嵌 SQLite 3.54.1) |
| CGO | **需要** | **不需要**(C→Go 转译,纯 Go) |
| 性能 | 原生 C,最快 | CPU 密集查询慢 1.3~2.0 倍(linux/amd64 实测);IO 密集差距小 |
| 交叉编译 | 需目标平台 C 工具链 | `GOOS/GOARCH` 直接编,零工具链 |
| 注意 | — | 需与 go.mod 中的 `modernc.org/libc` 版本严格对齐(issue #177) |

**结论:默认用 modernc.org/sqlite(纯 Go)。** 邮件网关的写负载(额度计数、发送日志、健康检查结果)远低于 SQLite 瓶颈;真正的收益是"单二进制 + 一条命令交叉编译任意平台",这正是 one-api 式自部署项目的核心体验。需开启 WAL + `busy_timeout`。**何时换 Postgres**:多节点部署(SQLite 单写者)、日志/审计量大到需要独立扩容、已有运维团队。参考 one-api:启动时读 `SQL_DSN`,没有就落 SQLite——架构上从第一天就保持 SQL 可移植,换库零成本。

### 3.2 ORM:GORM vs sqlc

- **GORM v1.31.2**(2026-06-25):one-api/new-api 均用它;`AutoMigrate` 让单二进制自部署"零迁移步骤";动态查询(渠道选路的动态 SQL)写起来顺;缺点是隐藏 SQL 与生成语句质量一般。纯 Go SQLite 驱动可选社区版 `github.com/glebarez/sqlite`(GORM driver,基于 modernc)。
- **sqlc v1.31.1**(2026-04-22):类型安全、SQL 显式可审;但**没有运行时迁移方案**(要另配 golang-migrate/atlas,破坏自部署体验),且 SQLite/PG 双方言要维护两套 schema,渠道选路这种动态拼接 SQL 的场景反而不擅长。

**推荐 GORM**(核心 CRUD/迁移)+ 热点路径(渠道选路、批量记账 flush)手写原生 SQL,正是 one-api 的做法(`GetRandomSatisfiedChannel` 就是 GORM 连接上手写 SQL)。

---

## 4. 任务调度与配置热加载

- **robfig/cron**(14.2k stars,v3.x,MIT):单进程内嵌 cron,支持秒级精度与恢复包装器;最后一次实质代码推送是 2024-07,属于"稳定冻结"型库——对定时任务(每月额度重置、每日/每 N 分钟健康检查、批量记账 flush)完全够用,且**不引入任何外部依赖**。
- **hibiken/asynq**(v0.26.0,2026-02-03):Redis 分布式任务队列,重试/定时/去重齐全,但**强制依赖 Redis**,与"单二进制零依赖"目标冲突。仅当未来做多节点或异步任务很重时再引入。
- **推荐组合:robfig/cron + 进程内 goroutine。** 每月重置实现要点:用 `CRON_TZ` 时区表达式(每月 1 日 00:00 本地时区),或干脆记录 `last_reset_month` 字段惰性判断(实例跨月重启也不会漏);重置动作幂等。
- **配置热加载**常见三档:① `SIGHUP` 触发重读(最传统);② 管理后台保存配置后经 API 即时生效(网关类项目主流,one-api 的环境变量 + 管理页双轨);③ fsnotify 监听配置文件 + 防抖。**推荐 ②(+可选 ①)**:配置原子替换(载入新配置 → 校验 → `atomic.Value`/RWMutex 整体切换 → 失败则保留旧配置并报错),渠道/额度这类数据本来就在 DB,真正需要热加载的只有静态配置项。

---

## 5. Go 生态:多供应商邮件网关盘点

> 排除单平台 SDK;按"是否真正做多平台聚合/路由"评估。检索:GitHub search API,email gateway / email relay / failover 组合词,2026-11-11 抓取。

| 项目 | Stars | 活跃度 | 定位与缺口 |
|---|---|---|---|
| **craigmccaskill/posthorn** | 217 | 推送 2026-11-07,v2.0 2026-09 发布 | **同方向最近的活跃项目**。自部署出站邮件网关:HTTP 表单 + HTTP API(Bearer、幂等键)+ SMTP 监听三种入口;Transport 接口对接 Postmark/Resend/Mailgun/SES(手写 SigV4)/SMTP;SQLite 重试队列、退信抑制名单、webhook、/healthz、Prometheus。**没有**多供应商轮换/故障转移/配额管理——单一活跃 provider,切平台靠改配置 |
| soulteary/owlmail | 76 | 推送 2026-11-08 | 邮件测试网关(开发/CI 抓信),非生产发送 |
| muety/mailwhale | 371 | **已归档**(2023-12) | 自带 SMTP 的 REST 邮件中继 + Web UI |
| wiggin77/mailrelay | 88 | 推送 2026-07 | SMTP 跳板(无认证→认证 TLS),不做平台聚合 |
| blueimp/aws-smtp-relay | 86 | 推送 2024-01 | 仅 AWS(SES/Pinpoint) |
| chuccp/http2smtp | 12 | 推送 2026-10 | HTTP→SMTP,Web UI |
| nikoksr/notify | 3.9k | 推送 2026-11-09 | 多渠道**通知库**(含 SES/SendGrid 邮件),是库不是网关,无记账/轮换;Provider 适配器写法可参考 |
| 非 Go 参考:taltas/chain_mail(Ruby,31★,多供应商自动 failover)、thabot/thotsakan-mail(Bun/TS,14 供应商 failover)、Cuttlefish/Postal(Ruby) | — | — | 证明"多供应商 failover 邮件网关"需求存在,但 Go 生态是空白 |

**结论:Go 生态中不存在成熟的"多平台邮件聚合 + 额度轮换"网关。** posthorn 是唯一活跃的同赛道项目,它验证了 Transport 接口 + 单二进制的可行性,但刻意不做路由/轮换/配额——这正是 Snowmail 的差异化空间。架构上建议:入口 API(参照 posthorn 的幂等键、抑制名单、webhook)+ 轮换调度/记账/禁用(参照 one-api)。

### 附:主要平台 Go SDK 现状(2026-11-11)

| 平台 | 库 | Stars | 活跃度 |
|---|---|---|---|
| Mailgun | mailgun/mailgun-go(官方) | 743 | 推送 2026-11-09,活跃,内置 mock 测试服务器 |
| SendGrid | sendgrid/sendgrid-go(官方) | 1,063 | 推送 2026-10-29,维护中、节奏平缓 |
| Resend | resend/resend-go(官方) | 283 | 推送 2026-11-09,活跃 |
| MailerSend | mailersend/mailersend-go(官方) | 39 | 推送 2026-11-05 |
| AWS SES | aws-sdk-go-v2/service/sesv2(官方) | — | 标准维护 |
| Postmark | **无官方 Go SDK**,社区库为主 | — | 建议像 posthorn 一样手写薄 HTTP client |

主流平台 SDK 均可用;小平台(SparkPost gosparkpost、Mailjet mailjet-apiv3-go 等有库但活跃度一般)接入时可统一走"手写薄 client"路线,以网关自己的 `Transport` 接口为准,SDK 只作为参考实现。

---

## 6. 最终推荐组合

**Gin v1.12.0 + modernc.org/sqlite(经 glebarez/sqlite 接 GORM v1.31.2)+ robfig/cron v3**,单二进制自部署,SQL 层保持可移植(预留 `SQL_DSN` 切 Postgres);调度全部进程内 cron + goroutine,配置热加载走管理 API 原子替换(可选 SIGHUP);asynq/Redis 仅在多节点阶段引入。
