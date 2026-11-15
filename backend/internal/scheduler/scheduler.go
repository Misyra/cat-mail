// Package scheduler 实现 SPEC §7 的两项 cron 职责与 A2 定时备份、E4 审计裁剪、
// B outbox 投递驱动：每分钟驱动 webhook outbox 到期投递（安全加固三件套）；
// 每小时整点释放冷却到期渠道（附带清理站内通知/审计日志至保留上限、检查每日
// 自动备份）；每日 04:30 按 LogRetentionDays 清理发送日志，随后执行 PRAGMA
// optimize 刷新查询计划统计（L5 性能采纳，SQLite 专属）。
package scheduler

import (
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
	"gorm.io/gorm"

	"catmail/internal/model"
	"catmail/internal/notify"
	"catmail/internal/outbox"
	"catmail/internal/store"
)

// logDeleteBatch 日志清理单批删除条数：分批删除避免大事务长时间占用 SQLite 写锁。
const logDeleteBatch = 500

// notificationKeep 站内通知保留上限：每小时随冷却释放任务一并清理，仅保留最近
// 1000 条。通知只记录异常事件（量级远小于 SendLog），但渠道持续故障时仍可能
// 高频产生（如轮换全挂后的连续告警），设硬上限防通知表无限膨胀。
const notificationKeep = 1000

// auditLogKeep 审计日志保留上限（E4）：每小时随冷却释放任务一并清理，仅保留
// 最近 5000 条。审计按操作次数产生（管理面写操作，量级小），但脚本/误操作可能
// 短时间高频写库，设硬上限防 audit_logs 表无限膨胀；裁剪按 id 圈界删旧（见
// pruneAuditLogs），保留的恰是最近 5000 条。
const auditLogKeep = 5000

// outboxDeadKeep webhook 投递 dead 行保留上限（安全加固三件套，B）：每小时随
// 冷却释放任务一并裁剪，仅保留最近 100 条供排查（任务书口径）。
const outboxDeadKeep = outbox.DeadKeep

// stopWait 优雅退出时等待在执行任务收尾的最长时间。
const stopWait = 5 * time.Second

// Start 启动 cron 调度器（本地时区）并立即返回，不阻塞。
// 任务清单（SPEC §7 + A2 + B outbox）：
//   - "* * * * *"  每分钟：webhook outbox 到期投递驱动（安全加固三件套，B）——
//     通知 webhook 与调用方回执统一由本任务投递（指数退避/10 次失败转 dead，
//     见 internal/outbox；单轮互斥 + 行级认领防重叠投递）。
//   - "0 * * * *"  每小时整点：冷却释放——cooldown_until 已过期的渠道复位 ENABLED
//     （逐个复位并发射 EventChannelRecovered 通知）；随后清理站内通知至保留上限、
//     清理审计日志至保留上限（E4）、裁剪 outbox dead 行至保留上限（B）；再检查
//     每日自动备份（A2）：Setting.backup_daily_enabled 开启且今天还没备过则执行
//     一次整库备份（保留 10 份）。
//   - "30 4 * * *" 每日 04:30：日志清理——按 Setting.LogRetentionDays 删除过期 SendLog
//     （0=不清理）；随后 PRAGMA optimize 刷新查询计划统计（L5，PG 跳过）。
//
// 返回 *cron.Cron 供 main 优雅退出：defer scheduler.Stop(cron) 或直接 c.Stop()
// （Stop 会等待在执行的 outbox 投递轮收尾，与既有优雅关停口径一致）。
func Start(db *gorm.DB) *cron.Cron {
	c := cron.New(cron.WithLocation(time.Local))
	if _, err := c.AddFunc("* * * * *", func() {
		outbox.ProcessDue(db)
	}); err != nil {
		log.Printf("[scheduler] 注册 outbox 投递任务失败: %v", err)
	}
	if _, err := c.AddFunc("0 * * * *", func() {
		releaseExpiredCooldowns(db)
		pruneNotifications(db)
		pruneAuditLogs(db)
		pruneOutboxDead(db)
		pruneSessions(db)
		runScheduledBackup(db)
	}); err != nil {
		log.Printf("[scheduler] 注册冷却释放任务失败: %v", err)
	}
	if _, err := c.AddFunc("30 4 * * *", func() {
		cleanupExpiredLogs(db)
		optimizeSQLite(db)
	}); err != nil {
		log.Printf("[scheduler] 注册日志清理任务失败: %v", err)
	}
	c.Start()
	return c
}

// Stop 停止 cron 调度器：不再触发新任务，并等待正在执行的任务收尾（最多 stopWait），
// 供 main 在 SIGTERM 等退出路径上调用。重复/nil 调用安全。
func Stop(c *cron.Cron) {
	if c == nil {
		return
	}
	ctx := c.Stop()
	select {
	case <-ctx.Done():
	case <-time.After(stopWait):
		log.Printf("[scheduler] 退出等待超时（%v），仍有任务在执行", stopWait)
	}
}

// releaseExpiredCooldowns 冷却释放：status==cooldown 且 cooldown_until 非空且已过期
// 的渠道复位 ENABLED、cooldown_until 置空。先圈定到期渠道，再逐条带全条件的 UPDATE
// 复位：RowsAffected==1 才视为本任务复位并发射 EventChannelRecovered 通知（O9）——
// 与并发路径（轮换惰性复位/手动启用）天然互斥，不重复发通知；整体幂等（重复执行无副作用）。
func releaseExpiredCooldowns(db *gorm.DB) {
	var expired []model.Channel
	if err := db.Where("status = ? AND cooldown_until IS NOT NULL AND cooldown_until <= ?",
		model.ChannelCooldown, time.Now()).Find(&expired).Error; err != nil {
		log.Printf("[scheduler] 冷却释放失败: %v", err)
		return
	}
	reset := 0
	for i := range expired {
		res := db.Model(&model.Channel{}).
			Where("id = ? AND status = ? AND cooldown_until IS NOT NULL AND cooldown_until <= ?",
				expired[i].ID, model.ChannelCooldown, time.Now()).
			Updates(map[string]any{
				"status":         model.ChannelEnabled,
				"cooldown_until": nil,
			})
		if res.Error != nil {
			log.Printf("[scheduler] 冷却释放失败（渠道 %s）: %v", expired[i].Name, res.Error)
			continue
		}
		if res.RowsAffected == 0 {
			continue // 已被并发请求复位：不重复计数、不发恢复通知
		}
		reset++
		cid := expired[i].ID
		notify.Emit(notify.EventChannelRecovered, notify.LevelInfo,
			fmt.Sprintf("渠道「%s」已恢复可用", expired[i].Name),
			"冷却结束，渠道重新参与轮换。", &cid)
	}
	if reset > 0 {
		log.Printf("[scheduler] 冷却释放: 已复位 %d 个渠道", reset)
	}
}

// pruneNotifications 站内通知保留上限清理（O9）：仅保留最近 notificationKeep 条，
// 防异常风暴撑爆通知表。与日志清理同风格：失败只记日志，不影响其他任务。
func pruneNotifications(db *gorm.DB) {
	deleted, err := notify.PruneOld(db, notificationKeep)
	if err != nil {
		log.Printf("[scheduler] 通知清理失败: %v", err)
		return
	}
	if deleted > 0 {
		log.Printf("[scheduler] 通知清理: 已删除 %d 条历史通知", deleted)
	}
}

// pruneAuditLogs 审计日志保留上限清理（E4）：仅保留最近 auditLogKeep 条，防
// 管理操作高频（脚本/误操作风暴）撑爆审计表。与 notify.PruneOld 同款 id 圈界法：
// 自增 id 与 created_at 单调一致（审计行只增不改），按 id 倒序取第 auditLogKeep
// 条的 id 为边界，删除边界之前的全部行——恰好保留最近 auditLogKeep 条，且无需
// 依赖 created_at 排序。失败只记日志，不影响其他任务。
func pruneAuditLogs(db *gorm.DB) {
	var boundary []uint // 第 auditLogKeep 条新审计的 id：其之前的全部删除，恰好保留 auditLogKeep 条
	if err := db.Model(&model.AuditLog{}).
		Order("id DESC").Limit(1).Offset(auditLogKeep-1).Pluck("id", &boundary).Error; err != nil {
		log.Printf("[scheduler] 审计清理失败: %v", err)
		return
	}
	if len(boundary) == 0 {
		return // 不足 auditLogKeep 条
	}
	res := db.Where("id < ?", boundary[0]).Delete(&model.AuditLog{})
	if res.Error != nil {
		log.Printf("[scheduler] 审计清理失败: %v", res.Error)
		return
	}
	if res.RowsAffected > 0 {
		log.Printf("[scheduler] 审计清理: 已删除 %d 条历史审计日志（保留最近 %d 条）",
			res.RowsAffected, auditLogKeep)
	}
}

// pruneOutboxDead webhook 投递 dead 行裁剪（安全加固三件套，B）：仅保留最近
// outboxDeadKeep 条供排查，防持续故障 + 长期停投（如 webhook 地址长期 4xx/5xx）
// 时 webhook_deliveries 表无限膨胀。与 pruneAuditLogs 同款 id 圈界法；失败只记
// 日志，不影响其他任务。
func pruneOutboxDead(db *gorm.DB) {
	deleted, err := outbox.PruneDead(db, outboxDeadKeep)
	if err != nil {
		log.Printf("[scheduler] outbox dead 行清理失败: %v", err)
		return
	}
	if deleted > 0 {
		log.Printf("[scheduler] outbox 清理: 已删除 %d 条 dead 投递记录（保留最近 %d 条）",
			deleted, outboxDeadKeep)
	}
}

// sessionRevokedGrace 已吊销会话行的保留宽限（安全加固三件套，C）：吊销后保留
// 24h 再清理——「活跃会话」列表不展示已吊销行，宽限仅为排查留痕；过期会话行
// 直接清理（expires_at 已过即无复活可能，requireSession 先过期判定）。
const sessionRevokedGrace = 24 * time.Hour

// pruneSessions 会话注册表清理（安全加固三件套，C）：删除已过期（expires_at <=
// now）与吊销超过宽限（revoked_at <= now-24h）的行，防登录/踢出高频时 sessions
// 表无限膨胀。失败只记日志，不影响其他任务。双方言通用（无方言专属语句）。
func pruneSessions(db *gorm.DB) {
	now := time.Now()
	res := db.Where("expires_at <= ? OR (revoked_at IS NOT NULL AND revoked_at <= ?)",
		now, now.Add(-sessionRevokedGrace)).Delete(&model.Session{})
	if res.Error != nil {
		log.Printf("[scheduler] 会话清理失败: %v", res.Error)
		return
	}
	if res.RowsAffected > 0 {
		log.Printf("[scheduler] 会话清理: 已删除 %d 条过期/吊销超时的会话记录", res.RowsAffected)
	}
}

// cleanupExpiredLogs 日志清理：按 Setting.LogRetentionDays 删除 created_at 早于
// now-N 天的 SendLog；0（或设置行缺失）=永久保留，直接跳过。
// 分批删除（每批 logDeleteBatch 条）直到清完，避免单次大事务。
func cleanupExpiredLogs(db *gorm.DB) {
	var setting model.Setting
	// Find（而非 First）：设置行缺失不报错，零值 LogRetentionDays=0 走永久保留分支。
	if err := db.Limit(1).Find(&setting, 1).Error; err != nil {
		log.Printf("[scheduler] 读取设置失败，跳过日志清理: %v", err)
		return
	}
	if setting.LogRetentionDays <= 0 {
		return // 0=永久保留
	}
	cutoff := time.Now().AddDate(0, 0, -setting.LogRetentionDays)
	var total int64
	for {
		// SQLite 不支持 DELETE ... LIMIT，用 id 子查询圈定本批删除范围。
		// 方言（D2）：PG 同样不支持 DELETE ... LIMIT，id IN (SELECT ...) 子查询
		// 两种方言通用（PG 上子查询先于删除求值，语义一致）——同一写法跨方言。
		batch := db.Model(&model.SendLog{}).
			Select("id").
			Where("created_at < ?", cutoff).
			Limit(logDeleteBatch)
		res := db.Where("id IN (?)", batch).Delete(&model.SendLog{})
		if res.Error != nil {
			log.Printf("[scheduler] 日志清理失败（已删 %d 条）: %v", total, res.Error)
			return
		}
		total += res.RowsAffected
		if res.RowsAffected < logDeleteBatch {
			break
		}
	}
	if total > 0 {
		log.Printf("[scheduler] 日志清理: 已删除 %d 条 %s 之前的发送日志",
			total, cutoff.Format("2006-01-02 15:04:05"))
	}
}

// optimizeSQLite 每日 04:30 随日志清理执行 PRAGMA optimize（L5 性能采纳）：
// 让 SQLite planner 依据近期查询/写入刷新 sqlite_stat1 统计——长期运行的库缺
// 统计时 overview/health 聚合会选错索引（L5 实测约 7 倍扫描行差），每日一次
// 成本可忽略。D2：PRAGMA 为 SQLite 专属，PostgreSQL 连接直接跳过（统计刷新
// 由 PG 自身 autovacuum/ANALYZE 机制承担）；失败只记日志，不影响日志清理结果。
func optimizeSQLite(db *gorm.DB) {
	if store.IsPostgres(db) {
		return
	}
	if err := db.Exec("PRAGMA optimize").Error; err != nil {
		log.Printf("[scheduler] PRAGMA optimize 失败: %v", err)
	}
}

// ---------- A2 每日自动备份 ----------

// dailyBackupState 定时备份的当日去重状态（进程内存态即可）：lastDate 记录最近
// 一次成功自动备份的日期（本地时区 YYYY-MM-DD）。重启后清零——开启自动备份的
// 进程重启当天会立即补备一次，多备一份无害（保留策略自动裁剪到 10 份）。
var dailyBackupState struct {
	mu       sync.Mutex
	lastDate string
}

// runScheduledBackup 每小时任务的每日备份检查：Setting.backup_daily_enabled 开启
// 且今天尚未备份过时，执行与手动备份（POST /api/backups）完全相同的
// store.CreateBackup（VACUUM INTO 快照 + 保留 10 份裁剪）。失败仅记日志、不更新
// lastDate，下个整点自动重试。
// D2：备份是 SQLite 专属功能，PostgreSQL 连接直接跳过（不每小时刷错误日志；
// 管理员侧手动备份在 PG 上由 api 层 400 透传 store.ErrSQLiteOnly 拒绝）。
func runScheduledBackup(db *gorm.DB) {
	if store.IsPostgres(db) {
		return
	}
	var setting model.Setting
	// Find（而非 First）：设置行缺失不报错，零值 BackupDailyEnabled=false 直接跳过
	if err := db.Limit(1).Find(&setting, 1).Error; err != nil {
		log.Printf("[scheduler] 读取设置失败，跳过定时备份检查: %v", err)
		return
	}
	if !setting.BackupDailyEnabled {
		return
	}
	today := time.Now().Format("2006-01-02")
	dailyBackupState.mu.Lock()
	if dailyBackupState.lastDate == today {
		dailyBackupState.mu.Unlock()
		return // 今天已备过
	}
	dailyBackupState.mu.Unlock()

	name, _, err := store.CreateBackup(db, time.Now())
	if err != nil {
		log.Printf("[scheduler] 定时备份失败: %v", err)
		return
	}
	dailyBackupState.mu.Lock()
	dailyBackupState.lastDate = today
	dailyBackupState.mu.Unlock()
	log.Printf("[scheduler] 定时备份完成: 已生成 %s（每日自动备份，保留最近 10 份）", name)
}
