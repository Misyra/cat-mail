// handler_backup.go A2 数据管理：SQLite 整库备份（创建/列表/下载/删除）与
// 发送日志手动清理（分批删除）。全部接口挂在 /api/* 会话组内（danger 级操作
// 由前端二次确认，服务端记录操作审计日志）。D2：备份为 SQLite 专属功能，
// PostgreSQL 连接上四个备份接口统一 400 透传 store.ErrSQLiteOnly（不 500）。
package api

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"catmail/internal/model"
	"catmail/internal/store"
)

// logPurgeBatch 日志手动清理单批删除条数：与 scheduler 日志清理同口径（500 条），
// 分批删除避免大事务长时间占用 SQLite 写锁。
const logPurgeBatch = 500

// backupCreatedResp POST /api/backups 响应体。
type backupCreatedResp struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// createBackup POST /api/backups：VACUUM INTO 生成整库快照到 <数据目录>/backups/
// 并裁剪保留最近 10 份（store.CreateBackup）。恢复方法见该函数注释。
// PostgreSQL 连接（D2）：store.CreateBackup 返回 ErrSQLiteOnly，此处透传为
// 400（文案明示 PG 请用 pg_dump），不当内部 500。
func (s *Server) createBackup(c *gin.Context) {
	name, size, err := store.CreateBackup(s.db, time.Now())
	if err != nil {
		if errors.Is(err, store.ErrSQLiteOnly) {
			badRequest(c, err.Error())
			return
		}
		serverError(c, err)
		return
	}
	// 审计：谁+何时+结果（谁来自会话中间件注入的 ctxUsername）
	log.Printf("api: 数据备份: 用户 %s 于 %s 创建备份 %s（%d 字节）",
		c.GetString(ctxUsername), time.Now().Format("2006-01-02 15:04:05"), name, size)
	// 审计落库（E4）：仅成功路径记录
	s.audit(c, model.AuditBackupCreate,
		fmt.Sprintf("backup name=%s", name), fmt.Sprintf("size=%d", size))
	c.JSON(http.StatusOK, backupCreatedResp{Name: name, Size: size})
}

// listBackups GET /api/backups：备份列表（name/size/created_at，按时间倒序）。
// 尚无备份（backups 目录未创建）时返回空列表。PostgreSQL 返回 ErrSQLiteOnly
// 透传为 400（D2，与 createBackup 同口径）。
func (s *Server) listBackups(c *gin.Context) {
	items, err := store.ListBackups(s.db)
	if err != nil {
		if errors.Is(err, store.ErrSQLiteOnly) {
			badRequest(c, err.Error())
			return
		}
		serverError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// downloadBackup GET /api/backups/:name/download：c.FileAttachment 直出文件。
// name 先过 store.BackupFileNameRe 严格白名单（只允许
// catmail-backup-\d{8}-\d{6}\.db）再拼路径——正则不含「/」「\」「..」，
// 路径穿越在白名单一步即被拒绝；文件名即白名单原名，Content-Disposition 无注入面。
func (s *Server) downloadBackup(c *gin.Context) {
	name := c.Param("name")
	path, err := store.BackupFilePath(s.db, name)
	if err != nil {
		if errors.Is(err, store.ErrSQLiteOnly) {
			badRequest(c, err.Error()) // D2：PG 无备份目录概念，透传拒绝文案
			return
		}
		badRequest(c, "无效的备份文件名")
		return
	}
	if _, err := os.Stat(path); err != nil {
		notFound(c, "备份文件不存在")
		return
	}
	c.FileAttachment(path, name)
}

// deleteBackup DELETE /api/backups/:name：删除指定备份（name 同下载白名单）。
func (s *Server) deleteBackup(c *gin.Context) {
	name := c.Param("name")
	if err := store.DeleteBackup(s.db, name); err != nil {
		switch {
		case errors.Is(err, store.ErrSQLiteOnly):
			badRequest(c, err.Error()) // D2：PG 透传拒绝文案，不 500
		case errors.Is(err, store.ErrInvalidBackupName):
			badRequest(c, "无效的备份文件名")
		case os.IsNotExist(err):
			notFound(c, "备份文件不存在")
		default:
			serverError(c, err)
		}
		return
	}
	log.Printf("api: 数据备份: 用户 %s 于 %s 删除备份 %s",
		c.GetString(ctxUsername), time.Now().Format("2006-01-02 15:04:05"), name)
	// 审计落库（E4）：仅成功路径记录
	s.audit(c, model.AuditBackupDelete,
		fmt.Sprintf("backup name=%s", name), "")
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ---------- 日志手动清理（POST /api/logs/purge） ----------

// purgeLogsReq POST /api/logs/purge 请求体：mode 三选一，
// before_days 需 days（1-3650）、channel 需 channel_id、all 无附加参数。
type purgeLogsReq struct {
	Mode      string `json:"mode"` // before_days | channel | all
	Days      *int   `json:"days"`
	ChannelID *uint  `json:"channel_id"`
}

// purgeLogs POST /api/logs/purge：手动清理发送日志（danger 级，前端红色确认弹窗
// 二次确认）。三种模式：before_days=删 created_at 早于 now-days 的日志、
// channel=删该渠道全部日志、all=清空全部（保留表结构）。与 scheduler 日志清理
// 同口径分批删除（每批 logPurgeBatch 条，SQLite 不支持 DELETE ... LIMIT，用 id
// 子查询圈定本批范围循环到删完；PG 原生支持 DELETE ... WHERE 且同样接受 id IN
// (SELECT ...) 子查询，同一写法跨方言可用——D2），避免单次大事务长时间占用写锁。
// 操作与结果进服务端日志（谁+何时+删了多少）。
func (s *Server) purgeLogs(c *gin.Context) {
	var req purgeLogsReq
	if !bindJSON(c, &req) {
		return
	}
	var scope func(q *gorm.DB) *gorm.DB // 本批删除的范围条件
	var desc string                     // 审计日志用的人类可读描述
	switch req.Mode {
	case "before_days":
		if req.Days == nil || *req.Days < 1 || *req.Days > 3650 {
			badRequest(c, "days 取值范围 1-3650")
			return
		}
		cutoff := time.Now().AddDate(0, 0, -*req.Days)
		scope = func(q *gorm.DB) *gorm.DB { return q.Where("created_at < ?", cutoff) }
		desc = fmt.Sprintf("mode=before_days days=%d", *req.Days)
	case "channel":
		if req.ChannelID == nil || *req.ChannelID == 0 {
			badRequest(c, "channel_id 必填")
			return
		}
		cid := *req.ChannelID
		scope = func(q *gorm.DB) *gorm.DB { return q.Where("channel_id = ?", cid) }
		desc = fmt.Sprintf("mode=channel channel_id=%d", cid)
	case "all":
		scope = func(q *gorm.DB) *gorm.DB { return q }
		desc = "mode=all"
	default:
		badRequest(c, "mode 仅支持 before_days/channel/all")
		return
	}

	deleted, err := purgeBatchedDelete(s.db, scope)
	if err != nil {
		log.Printf("api: 日志清理中止: 用户 %s 执行 %s，已删 %d 条后失败: %v",
			c.GetString(ctxUsername), desc, deleted, err)
		serverError(c, err)
		return
	}
	log.Printf("api: 日志清理: 用户 %s 于 %s 执行 %s，删除 %d 条发送日志",
		c.GetString(ctxUsername), time.Now().Format("2006-01-02 15:04:05"), desc, deleted)
	// 审计落库（E4）：仅成功路径记录；desc 已含 mode（含删除条数 0 的空清理）
	s.audit(c, model.AuditPurgeLogs, "send_logs",
		fmt.Sprintf("%s deleted=%d", desc, deleted))
	c.JSON(http.StatusOK, gin.H{"deleted": deleted})
}

// purgeBatchedDelete 分批删除发送日志：每批以 id 子查询圈定 logPurgeBatch 条
// 删除，循环到清完，返回总删除数（与 scheduler cleanupExpiredLogs 同口径）。
func purgeBatchedDelete(db *gorm.DB, scope func(*gorm.DB) *gorm.DB) (int64, error) {
	var total int64
	for {
		batch := scope(db.Model(&model.SendLog{}).Select("id")).Limit(logPurgeBatch)
		res := db.Where("id IN (?)", batch).Delete(&model.SendLog{})
		if res.Error != nil {
			return total, res.Error
		}
		total += res.RowsAffected
		if res.RowsAffected < logPurgeBatch {
			return total, nil
		}
	}
}
