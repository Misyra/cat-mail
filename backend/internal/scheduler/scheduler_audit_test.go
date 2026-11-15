// scheduler_audit_test.go 审计日志保留上限裁剪（E4）：5000 条边界——不足不动、
// 超出删旧、恰好保留最近 auditLogKeep 条（id 圈界法）。
package scheduler

import (
	"fmt"
	"testing"

	"gorm.io/gorm"

	"catmail/internal/model"
)

// putAuditRows 批量写入 n 条审计行（id 自增；分批 100 行避免超长参数语句）。
func putAuditRows(t *testing.T, db *gorm.DB, n int) {
	t.Helper()
	rows := make([]model.AuditLog, n)
	for i := range rows {
		rows[i] = model.AuditLog{
			Username: "admin",
			Action:   model.AuditChannelCreate,
			Target:   fmt.Sprintf("channel#%d name=x", i+1),
		}
	}
	for start := 0; start < n; start += 100 {
		end := min(start+100, n)
		if err := db.Create(rows[start:end]).Error; err != nil {
			t.Fatalf("seed audit rows: %v", err)
		}
	}
}

// auditStat 统计审计行数与最小 id（空表 minID=0）。
func auditStat(t *testing.T, db *gorm.DB) (total int64, minID int64) {
	t.Helper()
	if err := db.Model(&model.AuditLog{}).Count(&total).Error; err != nil {
		t.Fatalf("count audit: %v", err)
	}
	var row struct {
		MinID int64
	}
	if err := db.Model(&model.AuditLog{}).
		Select("COALESCE(MIN(id), 0) AS min_id").Scan(&row).Error; err != nil {
		t.Fatalf("min audit id: %v", err)
	}
	return total, row.MinID
}

// TestPruneAuditLogs 裁剪边界：恰好 5000 条不动；新增后裁剪恰好删掉最早的
// 超出部分，保留最近 5000 条；重复执行幂等。
func TestPruneAuditLogs(t *testing.T) {
	db := newDB(t)

	// 恰好 5000 条：不动任何数据
	putAuditRows(t, db, auditLogKeep)
	total, minID := auditStat(t, db)
	if total != auditLogKeep || minID != 1 {
		t.Fatalf("seed: total=%d minID=%d", total, minID)
	}
	pruneAuditLogs(db)
	total, minID = auditStat(t, db)
	if total != auditLogKeep || minID != 1 {
		t.Fatalf("prune at boundary: total=%d minID=%d, want 不变", total, minID)
	}

	// 再加 20 条 → 删最早的 20 条（id 1-20），保留最近 5000 条
	putAuditRows(t, db, 20)
	pruneAuditLogs(db)
	total, minID = auditStat(t, db)
	if total != auditLogKeep {
		t.Fatalf("after prune: total=%d, want %d", total, auditLogKeep)
	}
	if minID != 21 {
		t.Fatalf("after prune: minID=%d, want 21（最早 20 条已删）", minID)
	}

	// 幂等：重复执行无副作用
	pruneAuditLogs(db)
	total, minID = auditStat(t, db)
	if total != auditLogKeep || minID != 21 {
		t.Fatalf("idempotent prune: total=%d minID=%d", total, minID)
	}
}
