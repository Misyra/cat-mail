// Package store 负责数据库打开（SQLite / PostgreSQL 双方言，D2）、AutoMigrate
// 与首启初始化。
package store

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/glebarez/sqlite" // 纯 Go SQLite，无 CGO
	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/postgres" // PostgreSQL 方言（pgx stdlib，D2）
	"gorm.io/gorm"

	"catmail/internal/model"
)

// IsPostgresDSN 判断数据源字符串是否为 PostgreSQL 连接串：以 postgres:// 或
// postgresql:// 开头（libpq/pgx 认可的 URL 形式；scheme 按惯例小写，此处容忍
// 大小写混写）。其余取值一律按 SQLite 文件路径处理（含 query 参数形式）。
// 该判定是 CATMAIL_DB 双方言语义的唯一分叉点（D2），与 config 层共享同一口径。
func IsPostgresDSN(dsn string) bool {
	lower := strings.ToLower(dsn)
	return strings.HasPrefix(lower, "postgres://") || strings.HasPrefix(lower, "postgresql://")
}

// IsPostgres 判断 *gorm.DB 当前连接是否为 PostgreSQL（按 Dialector 名判定），
// 供备份族函数与调用方（api/scheduler）做方言分支，不需要重新读环境变量。
func IsPostgres(db *gorm.DB) bool {
	return db != nil && db.Dialector != nil && db.Dialector.Name() == "postgres"
}

// Open 打开（必要时创建）数据库并执行 AutoMigrate，驱动按数据源形态自动选择（D2）：
//   - postgres:// / postgresql:// 前缀 → PostgreSQL（gorm.io/driver/postgres，
//     底层 pgx stdlib；连接串语法见 libpq，如
//     postgres://user:pass@host:5432/dbname?sslmode=disable）；
//   - 其余 → SQLite 文件路径（glebarez 纯 Go 驱动）：父目录自动创建，并追加
//     WAL 与 busy_timeout pragma（SPEC §8）提升并发读写稳定性——pragma 仅对
//     SQLite 有意义，PostgreSQL 路径不追加任何 DSN 参数（连接参数走连接串本身）。
//
// 两种方言共用同一套 AutoMigrate 与 TranslateError：唯一约束冲突均翻译为
// gorm.ErrDuplicatedKey（glebarez 与 gorm postgres 驱动均实现 ErrorTranslator，
// 后者按 SQLSTATE 23505 翻译），上层据此把并发重名兜底为 400，不感知方言。
func Open(dsn string) (*gorm.DB, error) {
	var dialector gorm.Dialector
	if IsPostgresDSN(dsn) {
		dialector = postgres.Open(dsn)
	} else {
		path := dsn
		if dir := filepath.Dir(path); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("create data dir: %w", err)
			}
		}
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		dialector = sqlite.Open(path + sep + "_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	}
	db, err := gorm.Open(dialector, &gorm.Config{
		// P2-13：开启错误翻译——唯一约束冲突映射为 gorm.ErrDuplicatedKey，
		// 上层据此把并发重名兜底为 400（两种方言驱动均实现 ErrorTranslator）
		TranslateError: true,
	})
	if err != nil {
		if IsPostgresDSN(dsn) {
			return nil, fmt.Errorf("open postgres: %w", err)
		}
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if err := db.AutoMigrate(
		&model.User{},
		&model.Setting{},
		&model.ApiKey{},
		&model.Channel{},
		&model.SendLog{},
		&model.Notification{},
		// E2 幂等键防重发档案：存量库 AutoMigrate 建表即可，无历史数据回填
		&model.IdempotencyRecord{},
		// E4 管理操作审计日志：存量库 AutoMigrate 建表即可，仅记录升级后的新操作
		&model.AuditLog{},
		// 邮件模板（邮件模板功能包）：存量库 AutoMigrate 建表即可，无历史数据回填
		&model.Template{},
		// 送达链路三件套：mail_events（入站送达事件存档）与 suppressions（收件人
		// 抑制列表）。存量库 AutoMigrate 建表即可，无历史数据回填。
		&model.MailEvent{},
		&model.Suppression{},
		// 安全加固三件套：sessions（服务端会话注册表，存量用户重新登录一次即入表）
		// 与 webhook_deliveries（webhook 投递 outbox，升级后新事件才开始落表）。
		// 均无历史数据回填。
		&model.Session{},
		&model.WebhookDelivery{},
	); err != nil {
		return nil, fmt.Errorf("auto migrate: %w", err)
	}
	// 通知功能存量库升级：Setting 新增的 notify_* 列均带 gorm 默认值，旧行取默认零值
	// （bool=false、string=空），语义即「通知关闭/未配置」，属安全默认，无需回填。
	// 按用户会话纪元（O10）：User 新增 session_epoch 列（默认 0），存量行取 0 与
	// 升级后首轮签发 token 的纪元一致，无需回填；旧全局纪元列 settings.session_epoch
	// 的读写代码路径已全部移除，gorm AutoMigrate 不删列，存量库残留该列无害。
	return db, nil
}

// EnsureSeed 首启初始化（无数据时）：admin 用户（bcrypt）+ Setting 默认行
// （SessionSecret 随机 32B hex）+ 一条 mock 示例渠道「示例渠道」。幂等，可重复调用。
func EnsureSeed(db *gorm.DB, adminUser, adminPass string) error {
	return db.Transaction(func(tx *gorm.DB) error {
		// Setting 默认行（ID=1）
		var settingCount int64
		if err := tx.Model(&model.Setting{}).Count(&settingCount).Error; err != nil {
			return err
		}
		if settingCount == 0 {
			secret, err := randomHex(32)
			if err != nil {
				return err
			}
			if err := tx.Create(&model.Setting{
				ID:                          1,
				Rotation:                    "priority",
				MaxAttempts:                 3,
				CooldownSeconds:             60,
				ConsecutiveFailureThreshold: 5,
				LogRetentionDays:            90,
				SessionSecret:               secret,
			}).Error; err != nil {
				return err
			}
		}

		// admin 用户（无任何 User 时才创建）
		var userCount int64
		if err := tx.Model(&model.User{}).Count(&userCount).Error; err != nil {
			return err
		}
		if userCount == 0 {
			hash, err := bcrypt.GenerateFromPassword([]byte(adminPass), bcrypt.DefaultCost)
			if err != nil {
				return err
			}
			if err := tx.Create(&model.User{Username: adminUser, PasswordHash: string(hash)}).Error; err != nil {
				return err
			}
		}

		// mock 示例渠道（开箱即可体验轮换，可删）
		var channelCount int64
		if err := tx.Model(&model.Channel{}).Count(&channelCount).Error; err != nil {
			return err
		}
		if channelCount == 0 {
			if err := tx.Create(&model.Channel{
				Name:     "示例渠道",
				Provider: "mock",
				Status:   model.ChannelEnabled,
				// 默认发件人：开箱即可「测试发送」，无需再补 from
				FromEmail: "sender@example.com",
				FromName:  "CatMail 示例",
			}).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// randomHex 生成 n 字节随机数的 hex 编码（32B → 64 字符）。
func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// ---------- A2 数据备份 ----------
// D2 方言边界：备份族（DBFilePath/CreateBackup/ListBackups/BackupFilePath/
// DeleteBackup）依赖 SQLite 专属能力——PRAGMA database_list 定位数据目录、
// VACUUM INTO 生成单文件一致性快照、备份产物为普通 SQLite 文件。PostgreSQL
// 连接上统一返回 ErrSQLiteOnly，调用方（api/scheduler）透传给用户，不得 500。

// ErrSQLiteOnly 备份族功能在 PostgreSQL 连接上的统一拒绝错误（D2）。
// 调用方以 errors.Is 判定并映射 4xx（文案明示 PG 请用 pg_dump/pg_restore）。
var ErrSQLiteOnly = errors.New("备份功能仅支持 SQLite（PostgreSQL 请使用 pg_dump）")

// backupNameLayout 备份文件名内嵌时间戳的格式（秒级）。
const backupNameLayout = "20060102-150405"

// BackupKeep 备份保留份数：超出时按文件名（内嵌时间戳）从旧到新清理。
const BackupKeep = 10

// BackupFileNameRe 备份文件名严格白名单：仅允许 catmail-backup-YYYYMMDD-HHMMSS.db。
// 下载/删除接口在拼接路径前据此校验 :name 参数——正则不含「/」「\」「..」，
// 路径穿越在白名单一步即被拒绝。
var BackupFileNameRe = regexp.MustCompile(`^catmail-backup-\d{8}-\d{6}\.db$`)

// ErrInvalidBackupName 备份文件名未通过白名单校验（调用方映射 400）。
var ErrInvalidBackupName = errors.New("无效的备份文件名")

// BackupInfo 单个备份文件信息（GET /api/backups 列表项）。
type BackupInfo struct {
	Name      string    `json:"name"`
	Size      int64     `json:"size"`
	CreatedAt time.Time `json:"created_at"`
}

// DBFilePath 主库文件实际路径：PRAGMA database_list 的 main 行 file 列。
// 该值为连接真实打开的路径（相对 DSN 已由驱动解析为绝对路径），比重新拼
// config 的 DBPath 更可靠——api/scheduler 层只持有 *gorm.DB，不必再透传路径。
// PostgreSQL 不适用（D2）：PRAGMA 为 SQLite 专属，返回 ErrSQLiteOnly。
func DBFilePath(db *gorm.DB) (string, error) {
	if IsPostgres(db) {
		return "", ErrSQLiteOnly
	}
	rows, err := db.Raw("PRAGMA database_list").Rows()
	if err != nil {
		return "", fmt.Errorf("pragma database_list: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var seq int
		var name, file string
		if err := rows.Scan(&seq, &name, &file); err != nil {
			return "", fmt.Errorf("scan database_list: %w", err)
		}
		if name == "main" && file != "" {
			return file, nil
		}
	}
	return "", errors.New("pragma database_list: main 库路径不可得（内存库或连接异常）")
}

// backupDir <数据目录>/backups（数据目录 = 主库文件所在目录）。
func backupDir(db *gorm.DB) (string, error) {
	dbPath, err := DBFilePath(db)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(dbPath), "backups"), nil
}

// CreateBackup 用 VACUUM INTO 生成整库快照到 <数据目录>/backups/，并裁剪只保留
// 最近 BackupKeep 份，返回 (name, size)。
//
// VACUUM INTO 实测：glebarez/sqlite（底层 modernc.org/sqlite，SQLite ≥3.27）原生
// 支持（见 store/backup_test.go 的合法性与裁剪用例），故无需退回 Online Backup API
// 或 litestream 式逐页复制——VACUUM INTO 单条语句即产出紧凑的单文件一致性快照
// （执行时对主库加读锁、自动合并 WAL），是本依赖形态下最稳妥且代码量最小的方案。
//
// 恢复方法（整库替换，注释明示以便运维）：停服 → 用备份文件替换 data/catmail.db
// （同时删除残留的 catmail.db-wal/-shm）→ 起服。备份是普通 SQLite 文件，也可直接
// 用 sqlite3 打开按表抽取数据。
//
// PostgreSQL（D2）：VACUUM INTO 不存在，返回 ErrSQLiteOnly——请用
// pg_dump -Fc "$CATMAIL_DB" > catmail.dump（恢复 pg_restore）。
func CreateBackup(db *gorm.DB, now time.Time) (string, int64, error) {
	if IsPostgres(db) {
		return "", 0, ErrSQLiteOnly
	}
	dir, err := backupDir(db)
	if err != nil {
		return "", 0, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", 0, fmt.Errorf("create backup dir: %w", err)
	}
	// 同名文件已存在时 VACUUM INTO 报错：同一秒内重复备份（手动+定时撞车）按秒
	// 推进时间戳重试；文件名受 BackupFileNameRe 白名单约束，无注入面。
	var lastErr error
	for i := 0; i < 10; i++ {
		name := "catmail-backup-" + now.Add(time.Duration(i)*time.Second).Format(backupNameLayout) + ".db"
		target := filepath.Join(dir, name)
		if _, err := os.Stat(target); err == nil {
			lastErr = fmt.Errorf("备份 %s 已存在", name)
			continue
		}
		if err := db.Exec("VACUUM INTO ?", target).Error; err != nil {
			_ = os.Remove(target) // 半成品清理（失败可忽略）
			return "", 0, fmt.Errorf("vacuum into %s: %w", name, err)
		}
		st, err := os.Stat(target)
		if err != nil {
			return "", 0, fmt.Errorf("stat backup %s: %w", name, err)
		}
		// 裁剪失败不掩盖备份已成功的事实，但作为错误返回给调用方记录
		if err := trimBackups(dir); err != nil {
			return name, st.Size(), fmt.Errorf("trim backups: %w", err)
		}
		return name, st.Size(), nil
	}
	return "", 0, lastErr
}

// trimBackups 旧备份清理：按文件名倒序（=时间倒序）保留前 BackupKeep 份，其余删除。
func trimBackups(dir string) error {
	infos, err := listBackupInfos(dir)
	if err != nil {
		return err
	}
	if len(infos) <= BackupKeep {
		return nil
	}
	for _, info := range infos[BackupKeep:] {
		if err := os.Remove(filepath.Join(dir, info.Name)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// listBackupInfos 列出目录内白名单命中的备份，按文件名倒序；目录不存在视为空。
func listBackupInfos(dir string) ([]BackupInfo, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []BackupInfo{}, nil
		}
		return nil, err
	}
	out := make([]BackupInfo, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !BackupFileNameRe.MatchString(e.Name()) {
			continue
		}
		st, err := os.Stat(filepath.Join(dir, e.Name()))
		if err != nil {
			continue // 并发删除等竞态：跳过该条
		}
		out = append(out, BackupInfo{Name: e.Name(), Size: st.Size(), CreatedAt: st.ModTime()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name })
	return out, nil
}

// ListBackups 备份列表（created_at 取文件修改时间，倒序）。PostgreSQL 返回 ErrSQLiteOnly。
func ListBackups(db *gorm.DB) ([]BackupInfo, error) {
	if IsPostgres(db) {
		return nil, ErrSQLiteOnly
	}
	dir, err := backupDir(db)
	if err != nil {
		return nil, err
	}
	return listBackupInfos(dir)
}

// BackupFilePath 校验 name 白名单后返回备份文件完整路径（非法名返回
// ErrInvalidBackupName，绝不拼接非白名单路径）。PostgreSQL 返回 ErrSQLiteOnly。
func BackupFilePath(db *gorm.DB, name string) (string, error) {
	if IsPostgres(db) {
		return "", ErrSQLiteOnly
	}
	if !BackupFileNameRe.MatchString(name) {
		return "", ErrInvalidBackupName
	}
	dir, err := backupDir(db)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

// DeleteBackup 删除指定备份（name 先过白名单）。PostgreSQL 返回 ErrSQLiteOnly。
func DeleteBackup(db *gorm.DB, name string) error {
	path, err := BackupFilePath(db, name)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return nil
}
