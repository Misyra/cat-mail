// audit.go 管理操作审计 helper（E4）：写操作成功后追加一条审计日志，并提供
// 审计明细截断与变更字段名比对工具。
package api

import (
	"log"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"catmail/internal/model"
)

// auditDetailMax 审计 Detail 字段截断上限（字符数，按 Unicode 码点计）。
const auditDetailMax = 500

// audit 记录一条管理操作审计日志（E4）。**仅在操作成功后调用**——失败的请求
// （400/404/500 等）不产生审计行，审计记录的是「实际发生的管理变更」而非尝试。
//
// 尽力而为语义：审计落库失败仅记服务端日志、不影响业务响应——审计缺失可事后
// 从服务端日志/备份侧补救，而让一次已成功的危险操作（如清空日志）因审计表故障
// 对用户报 500 属本末倒置；调用方无需检查返回值。
//
// 敏感值红线：target/detail 只允许字段名、计数、资源名等非敏感摘要；密码、
// API Key 明文、渠道秘钥（api_key/api_secret/smtp_pass）、代理凭据、会话密钥
// 一律不入审计（settings 更新只记变更字段名清单，同理）。
func (s *Server) audit(c *gin.Context, action, target, detail string) {
	entry := model.AuditLog{
		Username: c.GetString(ctxUsername),
		Action:   action,
		Target:   target,
		Detail:   truncateAuditDetail(detail),
		IP:       c.ClientIP(),
	}
	if err := s.db.Create(&entry).Error; err != nil {
		log.Printf("api: audit write failed (user=%s action=%s target=%s): %v",
			entry.Username, action, target, err)
	}
}

// truncateAuditDetail 审计明细按 Unicode 码点截断至 auditDetailMax 字符
// （utf8.RuneCountInString 口径，与各输入校验一致；中文按 1 个字符计）。
func truncateAuditDetail(detail string) string {
	if utf8.RuneCountInString(detail) <= auditDetailMax {
		return detail
	}
	runes := []rune(detail)
	return string(runes[:auditDetailMax])
}

// changedSettingFields 比较设置更新前后的差异，返回发生变更的字段名列表
// （json 名顺序固定）。只输出字段名、绝不输出值——notify_webhook_url 等地址
// 可能内嵌 token，密码类字段同理，统一不入审计。
func changedSettingFields(old, new settingsDTO) []string {
	type field struct {
		name    string
		changed bool
	}
	fields := []field{
		{"rotation", old.Rotation != new.Rotation},
		{"max_attempts", old.MaxAttempts != new.MaxAttempts},
		{"cooldown_seconds", old.CooldownSeconds != new.CooldownSeconds},
		{"consecutive_failure_threshold", old.ConsecutiveFailureThreshold != new.ConsecutiveFailureThreshold},
		{"log_retention_days", old.LogRetentionDays != new.LogRetentionDays},
		{"notify_inapp_enabled", old.NotifyInappEnabled != new.NotifyInappEnabled},
		{"notify_webhook_enabled", old.NotifyWebhookEnabled != new.NotifyWebhookEnabled},
		{"notify_webhook_url", old.NotifyWebhookURL != new.NotifyWebhookURL},
		{"notify_events", old.NotifyEvents != new.NotifyEvents},
		{"log_body_enabled", old.LogBodyEnabled != new.LogBodyEnabled},
		{"backup_daily_enabled", old.BackupDailyEnabled != new.BackupDailyEnabled},
		{"suppression_auto_enabled", old.SuppressionAutoEnabled != new.SuppressionAutoEnabled},
		{"bounce_rate_threshold", old.BounceRateThreshold != new.BounceRateThreshold},
	}
	var changed []string
	for _, f := range fields {
		if f.changed {
			changed = append(changed, f.name)
		}
	}
	return changed
}

// joinAuditFields 字段名列表 → 审计 detail（"fields=a,b,c"）；空列表返回空串
// （无任何变更的 PUT 不记变更清单）。
func joinAuditFields(fields []string) string {
	if len(fields) == 0 {
		return ""
	}
	return "fields=" + strings.Join(fields, ",")
}
