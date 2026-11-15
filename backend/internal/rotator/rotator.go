// Package rotator 实现轮换调度引擎：额度惰性重置、域名过滤、指定渠道语义、
// failover、自动禁用与冷却（SPEC §2 轮换算法）。
//
// 公开入口 Send(ctx, db, in)：输入已解析的 provider.Message 与可选指定渠道，
// 输出成功结果 Output 或带 HTTP 状态映射信息的 Err，供 internal/api 直接返回。
package rotator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"catmail/internal/model"
	"catmail/internal/notify"
	"catmail/internal/provider"
)

// 错误类型常量（SPEC §2 统一错误体的 error.type）。
const (
	ErrInvalidRequest = "invalid_request" // 400：参数/指定渠道语义错误，不换渠道
	ErrQuotaExhausted = "quota_exhausted" // 429：无可用候选渠道
	ErrProviderError  = "provider_error"  // 502：候选全部尝试失败
	ErrServerError    = "server_error"    // 500：设置/渠道读取等内部错误
)

// 设置兜底默认值（Setting 行缺失时使用，与 SPEC §4 种子数据一致）。
const (
	defaultRotation      = "priority"
	defaultMaxAttempts   = 3
	defaultCooldown      = 60
	defaultFailThreshold = 5
)

const (
	dayLayout   = "2006-01-02"
	monthLayout = "2006-01"
)

// Input 一次发送请求的输入。Message 由 API 层解析为规范形态；
// Resolver 供测试/内嵌替换适配器查找，nil 时使用 provider.Get。
type Input struct {
	Message     *provider.Message
	ChannelName string // 非空 = 指定渠道（精确匹配渠道名）
	APIKeyID    uint
	IsTest      bool
	// TemplateID 所用邮件模板 ID（邮件模板功能包）：模板渲染在 API/SMTP 入口
	// 完成（渲染结果已替换 Message 的 subject/html/text），轮换器只负责随
	// SendLog 落库追溯；0 = 未使用模板。
	TemplateID uint
	Resolver   func(name string) (provider.Transport, bool)
}

// Output 发送成功结果（平台已受理）。
type Output struct {
	MessageID         string // 内部 uuid，与 SendLog.MessageID 一致
	ChannelID         uint
	ChannelName       string
	Provider          string
	ProviderMessageID string
	LatencyMs         int64
}

// Err 发送失败结果，HTTPStatus/Type/Message 供 API 层直接映射统一错误体。
// Code 为可选机器可读错误码（如 recipient_suppressed，抑制列表拦截，见
// suppress.go）：空串 = 无附加错误码，API 层错误体不含 code 字段（既有调用方
// 契约不变）。
type Err struct {
	HTTPStatus int
	Type       string
	Message    string
	Code       string
}

// Send 执行一次带轮换 failover 的发送（SPEC §2 轮换算法）。
//
// 流程：抑制列表拦截（送达链路三件套的唯一 hook 点，见 suppress.go）→ 读取设置
// → 汇总候选（状态过滤/指定渠道语义/域名过滤/额度惰性重置与过滤）
// → 排序（priority 或 round_robin）→ 逐家尝试：先取渠道级出站令牌（A1 令牌桶限速，
// 等待超时/ctx 取消跳过该候选继续 failover）、再原子预留额度（条件 UPDATE 抢占，
// 抢不到视为超额跳过），请求未带 from 时用候选渠道默认发件人回退，再发送；
// 受理成功预留转最终记账、失败释放预留（401/403 立即禁用、429 进冷却、可重试错误
// 原子累计失败达阈值禁用、参数类 4xx 直接返回）→ 全部耗尽 502。
// 铁律：平台受理后绝不换渠道重发；额度净增只发生在受理时（预留→受理，或预留→释放）。
func Send(ctx context.Context, db *gorm.DB, in *Input) (*Output, *Err) {
	start := time.Now()
	if in == nil || in.Message == nil {
		return nil, invalidRequest("缺少消息内容")
	}
	messageID := uuid.NewString()
	now := time.Now()

	// 收件人抑制拦截（统一 hook 点）：/v1/send、/emails、SMTP 入口、日志重发
	// 全部经本函数出站，一处拦截三处生效。命中 → 400 invalid_request（code=
	// recipient_suppressed），不进轮换、不预留额度、不落发送日志（拒绝在门外）。
	if serr := checkSuppressions(db, in.Message); serr != nil {
		return nil, serr
	}

	setting, rerr := loadSettings(db)
	if rerr != nil {
		return nil, rerr
	}
	rotation, maxAttempts, cooldownSeconds, threshold := normalizeSettings(setting)

	candidates, rerr := collectCandidates(db, in, now)
	if rerr != nil {
		return nil, rerr
	}
	sortCandidates(candidates, rotation)

	attempts := maxAttempts
	if len(candidates) < attempts {
		attempts = len(candidates)
	}

	var (
		tried     int
		lastTried *model.Channel
		lastMsg   *provider.Message // 最后一次实际尝试的消息（含默认发件人回退后的副本）
		lastPerr  *provider.ProviderError
		// 因出站限速等待超时/ctx 取消而跳过的候选数：全部候选都因此跳过时
		// （tried==0）返回 429 出站限速文案，与额度耗尽文案区分。
		rateLimitedSkips int
	)
	// 额度预留失败的候选（并发竞态下被其他请求抢先占满）不算一次尝试，继续看下一候选。
	for i := 0; i < len(candidates) && tried < attempts; i++ {
		ch := &candidates[i]

		transport, ok := resolveTransport(in, ch.Provider)
		if !ok {
			// 适配器缺失视为可重试故障：累计失败后换下一家（未预留额度，无需释放）。
			tried++
			lastTried, lastMsg = ch, in.Message
			perr := &provider.ProviderError{
				Code:      "provider_missing",
				Message:   fmt.Sprintf("平台适配器未注册: %s", ch.Provider),
				Retryable: true,
			}
			lastPerr = perr
			applyRetryableFailure(db, ch, perr, threshold)
			continue
		}

		// 出站限速（A1 渠道级令牌桶）：先取令牌、再预留额度、再发送——避免等待
		// 期间占用已预留的额度。等待超时/ctx 取消 → 跳过该候选：不计 tried、
		// 不失败计数、不预留（限速视作临时不可用，这是刻意的 failover 语义）。
		if !tryAcquire(ctx, ch) {
			rateLimitedSkips++
			continue
		}

		// 发送前原子预留额度（P1-1）：条件 UPDATE 抢占，失败即超额跳过。
		if !reserveQuota(db, ch, now) {
			continue
		}
		tried++
		lastTried = ch

		// panic 安全（O8）：预留成功后 defer 配对兜底——transport.Send panic 时
		// （api 层 Recovery 兜底进程不崩）预留不再静默多计。quotaSettled 标记本
		// 候选预留是否已按正常路径结算（受理成功保留 / 失败已显式释放），仅
		// panic 未结算时 defer 补释放，正常路径不双重释放。
		quotaSettled := false
		defer func() {
			if !quotaSettled {
				releaseQuota(db, ch, now)
			}
		}()

		// 每候选独立回退渠道默认发件人（P1-4）：作用于本候选的消息副本。
		attemptMsg := messageForCandidate(in.Message, ch)
		lastMsg = attemptMsg

		result, perr := transport.Send(ctx, ch, attemptMsg)
		lastPerr = perr
		if perr == nil {
			// 平台已受理：预留转为最终记账并返回，绝不换渠道重发（SPEC 铁律）。
			acceptChannel(db, ch, now)
			quotaSettled = true // 受理成功：预留转最终记账（保留扣减），不再释放
			writeSendLog(db, messageID, in, ch, attemptMsg, model.SendStatusSent, tried, start, result, "")
			out := &Output{
				MessageID:         messageID,
				ChannelID:         ch.ID,
				ChannelName:       ch.Name,
				Provider:          ch.Provider,
				ProviderMessageID: result.ProviderMessageID,
				LatencyMs:         time.Since(start).Milliseconds(),
			}
			// D1 调用方回执：受理成功后推送 send_accepted（未配置 webhook 即 no-op）
			receiptAccepted(in, out, attemptMsg)
			return out, nil
		}

		// 发送失败：先释放预留额度（锚点口径见 releaseQuota），再按错误类型处置
		// （额度净增只发生在受理成功时）。
		releaseQuota(db, ch, now)
		quotaSettled = true // 已显式释放：panic 兜底 defer 不重复释放
		switch {
		case perr.DisableChannel: // 平台 401/403：立即自动禁用，不消耗额度
			disableChannel(db, ch, perr)
		case perr.Cooldown: // 平台 429：进入冷却，到期由轮换/定时任务复位
			startCooldown(db, ch, cooldownSeconds, perr, now)
		case perr.Retryable: // 网络错误/5xx/402：累计连续失败，达阈值自动禁用
			applyRetryableFailure(db, ch, perr, threshold)
		default: // 参数类 4xx：调用方问题，不换渠道、不改渠道状态
			writeSendLog(db, messageID, in, ch, attemptMsg, model.SendStatusFailed, tried, start, nil, perr.Error())
			return nil, invalidRequest(fmt.Sprintf("渠道「%s」拒绝了请求：%s", ch.Name, perr.Message))
		}
	}

	// 一个候选都没实际尝试 → 429：全部候选都因出站限速等待跳过时限速文案，
	// 否则（全部在预留环节输给并发请求）沿用额度耗尽文案。
	if tried == 0 {
		if rateLimitedSkips == len(candidates) {
			msg := "所有候选渠道处于出站限速等待中（rate_limit_per_sec 限制）"
			notifySendFailed(in, messageID, 0, msg)
			receiptFailed(in, messageID, 0, msg) // D1 调用方回执：429 限速终态
			return nil, quotaErr(msg)
		}
		notifySendFailed(in, messageID, 0, "所有候选渠道的额度已耗尽")
		receiptFailed(in, messageID, 0, "所有候选渠道的额度已耗尽") // D1 调用方回执：429 额度终态
		return nil, quotaErr("所有候选渠道的额度已耗尽")
	}
	// 候选全部尝试耗尽 → 502 provider_error，message 附最后一次平台错误。
	errText := "所有候选渠道均发送失败"
	if lastPerr != nil {
		errText = lastPerr.Error()
	}
	if lastTried != nil {
		writeSendLog(db, messageID, in, lastTried, lastMsg, model.SendStatusFailed, tried, start, nil, errText)
	}
	notifySendFailed(in, messageID, tried, errText)
	receiptFailed(in, messageID, tried, errText) // D1 调用方回执：502 候选穷尽终态
	return nil, &Err{
		HTTPStatus: 502,
		Type:       ErrProviderError,
		Message:    fmt.Sprintf("已尝试 %d 个渠道均发送失败：%s", tried, errText),
	}
}

// ---------- 候选渠道汇总 ----------

// collectCandidates 汇总本次请求的候选渠道（SPEC §2）：
//   - 指定 channel：仅该渠道；不存在/域名不匹配 → 400，停用/冷却未到/超额 → 429；
//   - 未指定：全部 ENABLED 渠道 + 冷却已到期渠道（立即复位 ENABLED），
//     经域名过滤与额度过滤（惰性重置后）筛选；为空 → 429 quota_exhausted。
func collectCandidates(db *gorm.DB, in *Input, now time.Time) ([]model.Channel, *Err) {
	today := now.Format(dayLayout)
	month := now.Format(monthLayout)

	if in.ChannelName != "" {
		return specifiedCandidates(db, in, now, today, month)
	}

	var pool []model.Channel
	if err := db.Where("status = ?", model.ChannelEnabled).Find(&pool).Error; err != nil {
		return nil, serverError(fmt.Sprintf("查询渠道失败：%v", err))
	}
	// 冷却到期（cooldown_until <= now）的渠道视为可用并立即复位为 ENABLED。
	var cooling []model.Channel
	if err := db.Where("status = ?", model.ChannelCooldown).Find(&cooling).Error; err != nil {
		return nil, serverError(fmt.Sprintf("查询渠道失败：%v", err))
	}
	for i := range cooling {
		if cooling[i].CooldownUntil != nil && !cooling[i].CooldownUntil.After(now) {
			resetFromCooldown(db, &cooling[i])
			pool = append(pool, cooling[i])
		}
	}
	if len(pool) == 0 {
		return nil, quotaErr("没有可用渠道：全部处于停用或冷却状态")
	}

	fromDomain := domainOf(in.Message.FromAddr)
	var candidates []model.Channel
	for i := range pool {
		ch := pool[i]
		// 请求未提供 from 时不做域名过滤（P1-4）：实际发送域名将由候选渠道的
		// 默认发件人决定（回退后的域名天然匹配该渠道绑定的 AllowedDomains），
		// 不能用空域名把配置了 AllowedDomains 的渠道全部误剔除。
		if fromDomain != "" && !domainAllowed(ch.AllowedDomains, fromDomain) {
			continue
		}
		lazyResetQuota(db, &ch, today, month)
		if quotaBlocked(&ch) {
			continue
		}
		candidates = append(candidates, ch)
	}
	if len(candidates) == 0 {
		return nil, quotaErr("所有可用渠道的额度已耗尽（或发件域名不匹配）")
	}
	return candidates, nil
}

// specifiedCandidates 指定渠道的候选语义：仅该渠道，语义错误直接返回给调用方。
func specifiedCandidates(db *gorm.DB, in *Input, now time.Time, today, month string) ([]model.Channel, *Err) {
	var ch model.Channel
	err := db.Where("name = ?", in.ChannelName).First(&ch).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, invalidRequest(fmt.Sprintf("指定渠道不存在：%s", in.ChannelName))
	}
	if err != nil {
		return nil, serverError(fmt.Sprintf("查询渠道失败：%v", err))
	}
	fromDomain := domainOf(in.Message.FromAddr)
	// 请求未提供 from 时同样放宽域名过滤（P1-4，口径与轮换路径一致）：
	// 实际发送域名由渠道默认发件人决定，空域名不应判为不匹配。
	if fromDomain != "" && !domainAllowed(ch.AllowedDomains, fromDomain) {
		return nil, invalidRequest(fmt.Sprintf("指定渠道「%s」不允许发件域名 %s", ch.Name, fromDomain))
	}
	switch ch.Status {
	case model.ChannelManualDisabled, model.ChannelAutoDisabled:
		return nil, quotaErr(fmt.Sprintf("指定渠道「%s」已停用", ch.Name))
	case model.ChannelCooldown:
		if ch.CooldownUntil == nil || ch.CooldownUntil.After(now) {
			return nil, quotaErr(fmt.Sprintf("指定渠道「%s」冷却中", ch.Name))
		}
		resetFromCooldown(db, &ch) // 冷却到期：立即复位并可用
	}
	lazyResetQuota(db, &ch, today, month)
	if quotaBlocked(&ch) {
		return nil, quotaErr(fmt.Sprintf("指定渠道「%s」额度已用尽", ch.Name))
	}
	return []model.Channel{ch}, nil
}

// ---------- 方言分支（D2） ----------

// nullSafeNotEqualFrag 「列与绑定参数 NULL 安全不等」的方言片段（D2）：
//   - SQLite：`col IS NOT ?`——IS/IS NOT 是 SQLite 的 NULL 安全比较，右侧可为
//     任意表达式（原实现，行为不变）；
//   - PostgreSQL：`col IS DISTINCT FROM ?`——标准 SQL 的 NULL 安全不等；PG 的
//     IS NOT 只接受 NULL/TRUE/FALSE/UNKNOWN/DISTINCT FROM，`col IS NOT ?` 是
//     语法错误，必须换 IS DISTINCT FROM。
//
// 两者语义一致：col 为 NULL → 不等成立；col 与参数相等 → 不成立。参数占位仍按
// 原 ORDER 追加，调用方绑定顺序不受分支影响。rotator 内唯一用到该语法的位置是
// lazyResetQuota 的 WHERE（全仓原始 SQL 审计 D2 逐条核对过）。
func nullSafeNotEqualFrag(db *gorm.DB, col string) string {
	if db != nil && db.Dialector != nil && db.Dialector.Name() == "postgres" {
		return col + " IS DISTINCT FROM ?"
	}
	return col + " IS NOT ?"
}

// lazyResetQuota 惰性额度重置（P1-1 配套）：仅当 day/month anchor 仍是过期值时
// 条件清零（单条条件 UPDATE）。不能用请求快照无条件 reset——晚到的过期快照会把
// 并发请求刚完成预留/累计的 used_day 清零，破坏原子预留。条件失败（anchor 已被
// 并发请求重置或推进到当前周期）即为 no-op；额度判定本身交给预留的条件 UPDATE。
// 方言（D2）：WHERE 的 NULL 安全不等按 driver 分支（nullSafeNotEqualFrag——
// SQLite 用 IS NOT、PG 用 IS DISTINCT FROM）；SET/其余 WHERE 的 CASE WHEN 与
// 等值比较两种方言语义一致（anchor 为 NULL 时 `day_anchor = ?` 均判假，走
// ELSE 清零分支），PG 9.5+ 完整支持。anchor 为文本列（YYYY-MM-DD），无时区差异。
func lazyResetQuota(db *gorm.DB, ch *model.Channel, today, month string) {
	db.Exec(`UPDATE channels SET
		used_day = CASE WHEN day_anchor = ? THEN used_day ELSE 0 END,
		day_anchor = ?,
		used_month = CASE WHEN month_anchor = ? THEN used_month ELSE 0 END,
		month_anchor = ?
		WHERE id = ? AND (`+nullSafeNotEqualFrag(db, "day_anchor")+` OR `+nullSafeNotEqualFrag(db, "month_anchor")+`)`,
		today, today, month, month, ch.ID, today, month)
	// 同步内存快照（供本请求的额度过滤判断；权威判定在预留的条件 UPDATE）。
	if ch.DayAnchor != today {
		ch.UsedDay, ch.DayAnchor = 0, today
	}
	if ch.MonthAnchor != month {
		ch.UsedMonth, ch.MonthAnchor = 0, month
	}
}

// quotaBlocked 判断渠道当日/当月额度是否已用尽（0=不限；override>0 优先于平台默认）。
func quotaBlocked(ch *model.Channel) bool {
	quota := provider.DefaultQuotaFor(ch.Provider)
	if daily := effectiveLimit(ch.DailyLimitOverride, quota.Daily); daily > 0 && ch.UsedDay >= daily {
		return true
	}
	if monthly := effectiveLimit(ch.MonthlyLimitOverride, quota.Monthly); monthly > 0 && ch.UsedMonth >= monthly {
		return true
	}
	return false
}

// domainOf 提取发件邮箱的域名（小写）；无法提取时返回空串。
func domainOf(addr string) string {
	addr = strings.TrimSpace(addr)
	at := strings.LastIndex(addr, "@")
	if at < 0 || at == len(addr)-1 {
		return ""
	}
	return strings.ToLower(addr[at+1:])
}

// domainAllowed 判断发件域名是否在渠道 AllowedDomains（逗号分隔，忽略大小写）内；
// 渠道未配置（空）时不限制。
func domainAllowed(allowed, domain string) bool {
	allowed = strings.TrimSpace(allowed)
	if allowed == "" {
		return true
	}
	if domain == "" {
		return false
	}
	for _, d := range strings.Split(allowed, ",") {
		if strings.EqualFold(strings.TrimSpace(d), domain) {
			return true
		}
	}
	return false
}

// effectiveLimit 生效额度：override>0 时用 override，否则用平台默认。
func effectiveLimit(override, def int) int {
	if override > 0 {
		return override
	}
	return def
}

// ---------- 排序 ----------

// sortCandidates 按 settings.rotation 排序（SPEC §2）：
//
//	priority（默认）→ priority 升序，同级按 last_used_at 升序；
//	round_robin → 仅按 last_used_at 升序（忽略 priority）。
//
// last_used_at 为空视为最久未用排最前；同级以 ID 兜底保证结果稳定。
func sortCandidates(cs []model.Channel, rotation string) {
	byPriority := rotation != "round_robin"
	sort.Slice(cs, func(i, j int) bool {
		a, b := &cs[i], &cs[j]
		if byPriority && a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		if c := compareLastUsed(a.LastUsedAt, b.LastUsedAt); c != 0 {
			return c < 0
		}
		return a.ID < b.ID
	})
}

// compareLastUsed 比较 last_used_at（nil 视为最早）。
func compareLastUsed(a, b *time.Time) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return -1
	case b == nil:
		return 1
	case a.Before(*b):
		return -1
	case b.Before(*a):
		return 1
	default:
		return 0
	}
}

// ---------- 渠道状态记账 ----------
// 以下写库均为单渠道计数/状态更新，失败不阻断发送流程（受理后的记账不可回滚发送）。

// reserveQuota 发送前对候选渠道做原子额度预留（P1-1 并发修复）：
// 判定不再依赖请求开始时的读快照，而是单条条件 UPDATE 抢占——SQLite 单写者
// （busy_timeout 5000）保证两个并发请求不可能同时把 used 从 limit-1 抢到 limit。
//
//	预留成功（RowsAffected==1）→ 发送；三种路径：
//	  受理成功 → 预留即最终记账（acceptChannel 仅补记 last_used_at 等）；
//	  发送失败 → releaseQuota 释放预留；
//	  预留失败（RowsAffected==0，或写库异常 fail-closed）→ 该候选视为超额跳过。
//	可观测语义与 SPEC 铁律一致：静止状态下额度只因受理成功净增
//	（预留+1 → 受理保留 / 失败 -1 释放）。
//
// WHERE 同时处理日/月两个维度与惰性重置：anchor 不是当前周期时按 0 参与比较
// （等价于先重置再判断），SET 的 CASE 同口径起算（重置后 +1 = 1）。
// 生效额度 = override>0 ? override : 平台默认（默认值在 Go 侧查表后内联进条件，0=不限）。
//
// 方言（D2）：无 SQLite 专属语法。CASE WHEN 条件更新 PG 9.5+ 完整支持；原子性
// 在 SQLite 靠单写者（busy_timeout）保证，在 PG 靠行锁 + READ COMMITTED 的
// EvalPlanQual——并发 UPDATE 等锁醒来后 WHERE 条件按最新已提交行重评，仍不可能
// 两个请求同时把 used 从 limit-1 抢到 limit，预留语义跨方言一致。
func reserveQuota(db *gorm.DB, ch *model.Channel, now time.Time) bool {
	quota := provider.DefaultQuotaFor(ch.Provider)
	effDaily := effectiveLimit(ch.DailyLimitOverride, quota.Daily)
	effMonthly := effectiveLimit(ch.MonthlyLimitOverride, quota.Monthly)
	today := now.Format(dayLayout)
	month := now.Format(monthLayout)

	res := db.Exec(`UPDATE channels SET
		used_day = CASE WHEN day_anchor = ? THEN used_day + 1 ELSE 1 END,
		day_anchor = ?,
		used_month = CASE WHEN month_anchor = ? THEN used_month + 1 ELSE 1 END,
		month_anchor = ?
		WHERE id = ?
		  AND (? = 0 OR (CASE WHEN day_anchor = ? THEN used_day ELSE 0 END) < ?)
		  AND (? = 0 OR (CASE WHEN month_anchor = ? THEN used_month ELSE 0 END) < ?)`,
		today, today, month, month, ch.ID,
		effDaily, today, effDaily,
		effMonthly, month, effMonthly)
	return res.Error == nil && res.RowsAffected > 0
}

// releaseQuota 发送失败后释放预留额度（P1-1）：日/月各自回收 1，逐列 CASE 防御
// 减为负（与并发的周期重置/其他请求的预留交错时也不可能出现负计数）。
//
// 跨周期锚点口径（O8）：仅当渠道当前 day/month anchor 仍等于本次预留的周期锚点
// （即 used 计数仍包含本笔预留的 +1）才回收；预留发生在 23:59:59、释放在 00:00
// 后执行（或期间惰性重置已翻页）时，计数归属新周期、不含本笔预留，不得多减。
// 不匹配的维度保持原值（锚点为 NULL 时等值比较不成立，同样不动——宁可少减）。
// now 必须传与配对 reserveQuota 相同的时刻（轮换路径为同一次 Send 的 now），
// 据此还原预留时的 today/month 锚点。
// 方言（D2）：仅 CASE WHEN 与等值比较，SQLite/PG（9.5+）语义一致，无分支。
func releaseQuota(db *gorm.DB, ch *model.Channel, now time.Time) {
	db.Exec(`UPDATE channels SET
		used_day = CASE WHEN day_anchor = ? AND used_day > 0 THEN used_day - 1 ELSE used_day END,
		used_month = CASE WHEN month_anchor = ? AND used_month > 0 THEN used_month - 1 ELSE used_month END
		WHERE id = ?`, now.Format(dayLayout), now.Format(monthLayout), ch.ID)
}

// ---------- 导出额度记账入口（供绕过轮换的发送路径复用） ----------

// ReserveQuota 导出包装：渠道测试直发等绕过轮换的发送路径，在发送前用与轮换
// 路径完全相同的条件 UPDATE 原子预留额度（日 used_day +1、月 used_month +1），
// 含惰性周期重置口径——anchor 非当前周期时按 0 参与判定并以新周期起算（重置后 +1）。
// 返回 false = 写库失败或当日/当月额度已用尽（fail-closed），调用方不得发送。
// 禁止在包外另写额度 SQL：一切预留/释放都走这一对入口，避免与轮换口径漂移。
func ReserveQuota(db *gorm.DB, ch *model.Channel, now time.Time) bool {
	return reserveQuota(db, ch, now)
}

// ReleaseQuota 导出包装：发送失败后释放预留额度（日/月各回收 1，防御减为负）。
// 与 ReserveQuota 配对使用：预留后平台未受理必须释放，保证额度净增只发生在受理成功时。
// 仅回收额度，不改渠道状态机（禁用/冷却/连续失败累计仍由轮换路径专属处置）。
// now 必须传与配对 ReserveQuota 相同的时刻：释放按预留时的周期锚点对齐回收，
// 锚点已翻页（跨日/跨月后执行）的维度不动，不把新周期计数多减 1。
func ReleaseQuota(db *gorm.DB, ch *model.Channel, now time.Time) {
	releaseQuota(db, ch, now)
}

// acceptChannel 受理成功后的记账收尾（P1-1）：日/月额度已在发送前原子预留，
// 受理即将预留转为最终记账——此处仅补记 last_used_at 并清零连续失败与 last_error。
func acceptChannel(db *gorm.DB, ch *model.Channel, now time.Time) {
	db.Model(&model.Channel{}).Where("id = ?", ch.ID).Updates(map[string]any{
		"last_used_at":         now,
		"consecutive_failures": 0,
		"last_error":           "",
	})
}

// messageForCandidate 构造本候选渠道实际发送用的消息（P1-4）：请求未提供 from 且
// 该候选渠道配置了默认发件人（from_email）时用渠道默认填充。回退逐候选独立生效
// （不同渠道默认发件人不同），且填充作用在浅拷贝上——不污染共享 msg，避免下一
// 候选继承上一候选的 from。回退后的发件域名即渠道绑定域名，天然匹配其 AllowedDomains。
func messageForCandidate(msg *provider.Message, ch *model.Channel) *provider.Message {
	if msg.FromAddr != "" || ch.FromEmail == "" {
		return msg // 请求已提供 from（请求优先），或渠道无默认：原样使用
	}
	dup := *msg
	dup.FromAddr = ch.FromEmail
	if ch.FromName != "" {
		dup.FromName = ch.FromName
	}
	return &dup
}

// disableChannel 平台 401/403：立即 AUTO_DISABLED，写入原因与最后错误；不消耗额度。
// 同时清掉可能残留的 cooldown_until（P2-6）：置 AUTO_DISABLED 后旧冷却时间戳
// 不应继续存在，避免渠道恢复启用后仍带着过期冷却状态。
func disableChannel(db *gorm.DB, ch *model.Channel, perr *provider.ProviderError) {
	reason := "平台认证/授权失败"
	if perr.HTTPStatus > 0 {
		reason = fmt.Sprintf("平台认证/授权失败（HTTP %d）", perr.HTTPStatus)
	}
	db.Model(&model.Channel{}).Where("id = ?", ch.ID).Updates(map[string]any{
		"status":               model.ChannelAutoDisabled,
		"auto_disabled_reason": reason,
		"last_error":           perr.Error(),
		"cooldown_until":       nil,
	})
	notifyChannelDisabled(ch, reason, perr.Error())
}

// startCooldown 平台 429：COOLDOWN + cooldown_until = now + settings.cooldown_seconds。
func startCooldown(db *gorm.DB, ch *model.Channel, seconds int, perr *provider.ProviderError, now time.Time) {
	db.Model(&model.Channel{}).Where("id = ?", ch.ID).Updates(map[string]any{
		"status":         model.ChannelCooldown,
		"cooldown_until": now.Add(time.Duration(seconds) * time.Second),
		"last_error":     perr.Error(),
	})
	notifyChannelCooldown(ch, seconds)
}

// applyRetryableFailure 网络错误/5xx/402：连续失败原子 +1（P1-3 并发修复：
// 各并发失败各自读副本自增再写回会互相覆盖丢计数、永远达不到阈值，改为
// UPDATE ... RETURNING 单语句递增并取回新值），随后立即按取回的新计数判定，
// 达阈值（settings.consecutive_failure_threshold）则置 AUTO_DISABLED（原因=连续失败N次，
// N 与计数一致；递增与判定为先后两步，禁用重复置位幂等无害）。
// 方言（D2）：UPDATE ... RETURNING SQLite（modernc 内嵌 ≥3.35）与 PG（9.5+）
// 均原生支持，无需分支；PG 上该 UPDATE 由行锁串行化，并发递增不丢计数。
func applyRetryableFailure(db *gorm.DB, ch *model.Channel, perr *provider.ProviderError, threshold int) {
	var newCount int
	err := db.Raw(
		`UPDATE channels SET consecutive_failures = consecutive_failures + 1, last_error = ?
		 WHERE id = ? RETURNING consecutive_failures`,
		perr.Error(), ch.ID,
	).Scan(&newCount).Error
	if err != nil {
		// 退路：驱动不支持 RETURNING 时用表达式递增后再读回（读回窗口存在并发
		// 误差，可接受——最坏延迟一次禁用，不会丢失计数）。
		db.Model(&model.Channel{}).Where("id = ?", ch.ID).Updates(map[string]any{
			"consecutive_failures": gorm.Expr("consecutive_failures + 1"),
			"last_error":           perr.Error(),
		})
		var cur model.Channel
		if e := db.First(&cur, ch.ID).Error; e == nil {
			newCount = cur.ConsecutiveFailures
		}
	}
	ch.ConsecutiveFailures = newCount
	if newCount >= threshold {
		reason := fmt.Sprintf("连续失败 %d 次", newCount)
		db.Model(&model.Channel{}).Where("id = ?", ch.ID).Updates(map[string]any{
			"status":               model.ChannelAutoDisabled,
			"auto_disabled_reason": reason,
		})
		notifyChannelDisabled(ch, reason, perr.Error())
	}
}

// resetFromCooldown 冷却到期复位为 ENABLED（与 scheduler 整点释放逻辑幂等一致）。
func resetFromCooldown(db *gorm.DB, ch *model.Channel) {
	db.Model(&model.Channel{}).Where("id = ?", ch.ID).Updates(map[string]any{
		"status":         model.ChannelEnabled,
		"cooldown_until": nil,
	})
	ch.Status = model.ChannelEnabled
	ch.CooldownUntil = nil
}

// ---------- 日志与设置 ----------

// logBodyEnabled 读取「记录邮件正文」开关（A4，settings.log_body_enabled）。
// 每次落日志读一行 Setting：日志落库本就是低频 DB 写，附带一次主键单行读开销
// 可接受；读取失败或设置行缺失时按开启处理（保持既有行为，正文漏记不可恢复）。
func logBodyEnabled(db *gorm.DB) bool {
	var st model.Setting
	if err := db.First(&st, 1).Error; err != nil {
		return true
	}
	return st.LogBodyEnabled
}

// writeSendLog 落库一条 SendLog（成功/失败都要记）：uuid 贯穿、渠道冗余快照、
// 收件人与正文按 SPEC §4 快照存储。msg 为该次实际尝试的消息（含默认发件人回退），
// from 按其记录。落库失败不影响发送结果，但不再静默（P2-3）：记入服务端日志
// 便于排障（含 message_id，可关联调用方拿到的响应）。
// A4 隐私开关：log_body_enabled=false 时新日志正文存空串（元数据照常）；只影响
// 新日志，存量已落正文的历史行原样保留——刻意取舍，关闭开关不承诺抹除历史。
func writeSendLog(db *gorm.DB, messageID string, in *Input, ch *model.Channel, msg *provider.Message, status, attempts int, start time.Time, result *provider.Result, errText string) {
	toAddrs := "[]"
	if b, e := json.Marshal(msg.To); e == nil && len(msg.To) > 0 {
		toAddrs = string(b)
	}
	var providerMessageID string
	if result != nil {
		providerMessageID = result.ProviderMessageID
	}
	htmlBody, textBody := msg.HTML, msg.Text
	if !logBodyEnabled(db) {
		htmlBody, textBody = "", ""
	}
	lg := model.SendLog{
		MessageID:         messageID,
		ApiKeyID:          in.APIKeyID,
		ChannelID:         ch.ID,
		ChannelName:       ch.Name,
		Provider:          ch.Provider,
		TemplateID:        in.TemplateID,
		IsTest:            in.IsTest,
		Status:            status,
		FromAddr:          msg.FromAddr,
		ToAddrs:           toAddrs,
		Subject:           msg.Subject,
		HTMLBody:          htmlBody,
		TextBody:          textBody,
		ProviderMessageID: providerMessageID,
		Error:             errText,
		Attempts:          attempts,
		LatencyMs:         time.Since(start).Milliseconds(),
	}
	if err := db.Create(&lg).Error; err != nil {
		log.Printf("rotator: writeSendLog failed (message_id=%s, channel=%s): %v", messageID, ch.Name, err)
	}
}

// loadSettings 读取全局设置（ID=1）；行缺失时返回默认值（与种子数据一致）。
func loadSettings(db *gorm.DB) (model.Setting, *Err) {
	var s model.Setting
	err := db.First(&s, 1).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return model.Setting{
			Rotation:                    defaultRotation,
			MaxAttempts:                 defaultMaxAttempts,
			CooldownSeconds:             defaultCooldown,
			ConsecutiveFailureThreshold: defaultFailThreshold,
		}, nil
	}
	if err != nil {
		return s, serverError(fmt.Sprintf("读取系统设置失败：%v", err))
	}
	return s, nil
}

// normalizeSettings 归一化设置：rotation 仅认 round_robin（其余按 priority 默认），
// max_attempts 至少 1，失败阈值兜底默认 5。
func normalizeSettings(s model.Setting) (rotation string, maxAttempts, cooldownSeconds, threshold int) {
	rotation = s.Rotation
	if rotation != "round_robin" {
		rotation = defaultRotation
	}
	maxAttempts = s.MaxAttempts
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	cooldownSeconds = s.CooldownSeconds
	if cooldownSeconds < 0 {
		cooldownSeconds = 0
	}
	threshold = s.ConsecutiveFailureThreshold
	if threshold < 1 {
		threshold = defaultFailThreshold
	}
	return rotation, maxAttempts, cooldownSeconds, threshold
}

// resolveTransport 按平台名取适配器：优先使用注入的 Resolver，否则全局注册表。
func resolveTransport(in *Input, name string) (provider.Transport, bool) {
	if in.Resolver != nil {
		return in.Resolver(name)
	}
	return provider.Get(name)
}

// ---------- 失败通知埋点（O9） ----------
// 经包级 notify.Emit 发射（双通道：站内落库 + Webhook 异步）；notify 默认实例
// 未接线时 Emit 自动 no-op，通知故障绝不影响发送主链路。

// notifyChannelDisabled 渠道被自动禁用（401/403 或连续失败达阈值）→
// EventChannelDisabled / error：title 渠道名 + body 禁用原因与最后错误（截 300 字）。
func notifyChannelDisabled(ch *model.Channel, reason, lastErr string) {
	notify.Emit(notify.EventChannelDisabled, notify.LevelError,
		fmt.Sprintf("渠道「%s」已自动禁用", ch.Name),
		fmt.Sprintf("原因：%s\n最后错误：%s", reason, truncateRunes(lastErr, 300)),
		&ch.ID)
}

// notifyChannelCooldown 平台限流进入冷却 → EventChannelCooldown / warn。
func notifyChannelCooldown(ch *model.Channel, seconds int) {
	notify.Emit(notify.EventChannelCooldown, notify.LevelWarn,
		fmt.Sprintf("渠道「%s」进入冷却", ch.Name),
		fmt.Sprintf("平台限流，冷却 %d 秒", seconds),
		&ch.ID)
}

// notifySendFailed 一次请求重试穷尽后的终态失败（429 额度耗尽 / 502 候选穷尽）→
// EventSendFailed / error：body 含 message_id、主题（截 100 字）、收件人前 3 个、
// 已试渠道数与最后错误。channel_id 为空：失败是请求级事件（跨渠道穷尽），不指向单渠道。
func notifySendFailed(in *Input, messageID string, tried int, lastErr string) {
	notify.Emit(notify.EventSendFailed, notify.LevelError, "邮件发送失败",
		fmt.Sprintf("message_id: %s\n主题: %s\n收件人: %s\n已试渠道: %d 个\n最后错误: %s",
			messageID,
			truncateRunes(in.Message.Subject, 100),
			recipientsPreview(in.Message.To, 3),
			tried, lastErr),
		nil)
}

// recipientsPreview 收件人预览：前 n 个逗号连接；超出时以「等 N 个」注记总数。
func recipientsPreview(to []string, n int) string {
	if len(to) == 0 {
		return "-"
	}
	head := to
	if len(to) > n {
		head = to[:n]
	}
	preview := strings.Join(head, ", ")
	if len(to) > n {
		preview += fmt.Sprintf(" 等 %d 个", len(to))
	}
	return preview
}

// truncateRunes 按 rune 截断到 max 字，超长以「…」结尾（通知 body 防超长刷屏）。
func truncateRunes(s string, max int) string {
	if max <= 0 || utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max]) + "…"
}

// invalidRequest / quotaErr / serverError 统一错误构造。
func invalidRequest(msg string) *Err {
	return &Err{HTTPStatus: 400, Type: ErrInvalidRequest, Message: msg}
}
func quotaErr(msg string) *Err    { return &Err{HTTPStatus: 429, Type: ErrQuotaExhausted, Message: msg} }
func serverError(msg string) *Err { return &Err{HTTPStatus: 500, Type: ErrServerError, Message: msg} }
