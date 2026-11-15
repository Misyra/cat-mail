// Package scheduler 提供 SPEC §7 的两项 cron 职责：每小时整点冷却到期自动释放、
// 每日 04:30 按 LogRetentionDays 清理发送日志（见 scheduler.go）。
package scheduler
