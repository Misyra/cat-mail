// store_pg_test.go D2 PostgreSQL 方言支持测试：
//   - CATMAIL_DB 驱动探测（IsPostgresDSN）与连接方言判定（IsPostgres）——
//     无需真实 PG，用未连接的 postgres 方言器构造 *gorm.DB；
//   - 备份族函数在 PG 连接上的统一拒绝（ErrSQLiteOnly）；
//   - live PG 集成回归（Open/AutoMigrate/EnsureSeed/读写）——设置
//     CATMAIL_TEST_PG_DSN 才运行，未设置时 Skip（本机无 PG）。
package store

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"catmail/internal/model"
)

// newPostgresDialectorDB 构造一个挂 postgres 方言器的 *gorm.DB，不建立真实连接：
// gorm postgres 驱动 v1.6 的 Initialize 仅做 pgx.ParseConfig 与惰性 sql.Open
// （无网络往返），DisableAutomaticPing 跳过 gorm.Open 收尾的默认 Ping——
// 唯一会触发真实连接的入口。仅用于方言分支与拒绝逻辑单测。
func newPostgresDialectorDB(t *testing.T) *gorm.DB {
	t.Helper()
	dialector := postgres.New(postgres.Config{
		DSN: "postgres://catmail:catmail@127.0.0.1:5432/catmail_test?sslmode=disable",
	})
	db, err := gorm.Open(dialector, &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		t.Fatalf("open postgres dialector: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

// TestIsPostgresDSN CATMAIL_DB 驱动探测：postgres:// / postgresql:// 前缀
// （大小写不敏感）判定为 PG，其余一切取值（含带 query 参数的路径）均按 SQLite。
func TestIsPostgresDSN(t *testing.T) {
	cases := []struct {
		dsn  string
		want bool
	}{
		{"postgres://u:p@host:5432/db", true},
		{"postgres://u:p@host:5432/db?sslmode=disable", true},
		{"postgresql://u:p@host/db", true},
		{"Postgres://u:p@host/db", true},   // scheme 大小写不敏感
		{"POSTGRESQL://u:p@host/db", true}, // scheme 大小写不敏感
		{"./data/catmail.db", false},
		{"/abs/path/catmail.db", false},
		{"data/catmail.db?_pragma=busy_timeout(5000)", false},
		{":memory:", false},
		{"file::memory:?cache=shared", false},
		{"mysql://u@host/db", false},
		{" sqlite://x.db", false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsPostgresDSN(c.dsn); got != c.want {
			t.Errorf("IsPostgresDSN(%q) = %v, want %v", c.dsn, got, c.want)
		}
	}
}

// TestIsPostgresDialect 连接方言判定：postgres 方言器 → true，sqlite 连接 → false；
// nil 安全（防御调用方传空指针）。
func TestIsPostgresDialect(t *testing.T) {
	if IsPostgres(nil) {
		t.Error("IsPostgres(nil) = true, want false")
	}
	pg := newPostgresDialectorDB(t)
	if !IsPostgres(pg) {
		t.Error("IsPostgres(postgres dialector) = false, want true")
	}
	sq := openSeeded(t)
	if IsPostgres(sq) {
		t.Error("IsPostgres(sqlite) = true, want false")
	}
}

// TestOpenPostgresDSNPrefixRoutesToPostgres Open 的路由行为：postgres:// 前缀的
// DSN 走 postgres 方言器（对不可达地址报错信息含 open postgres，而非 sqlite 目录
// 创建/pragma 错误），验证分支真的分出去了（编译期依赖 + 运行期错误归因双保险）。
func TestOpenPostgresDSNPrefixRoutesToPostgres(t *testing.T) {
	_, err := Open("postgres://catmail:catmail@127.0.0.1:1/nope?sslmode=disable&connect_timeout=1")
	if err == nil {
		t.Fatal("Open(不可达 PG DSN) 应报错")
	}
	if !strings.Contains(err.Error(), "open postgres") {
		t.Errorf("错误信息应归因 postgres 分支，got: %v", err)
	}
}

// TestBackupFamilyRejectedOnPostgres 备份族五函数在 PG 连接上统一返回
// ErrSQLiteOnly（errors.Is 可判定，调用方透传 4xx 不 500）。
func TestBackupFamilyRejectedOnPostgres(t *testing.T) {
	db := newPostgresDialectorDB(t)
	name := "catmail-backup-20260101-000000.db"

	if _, err := DBFilePath(db); !errors.Is(err, ErrSQLiteOnly) {
		t.Errorf("DBFilePath err = %v, want ErrSQLiteOnly", err)
	}
	if _, _, err := CreateBackup(db, time.Now()); !errors.Is(err, ErrSQLiteOnly) {
		t.Errorf("CreateBackup err = %v, want ErrSQLiteOnly", err)
	}
	if _, err := ListBackups(db); !errors.Is(err, ErrSQLiteOnly) {
		t.Errorf("ListBackups err = %v, want ErrSQLiteOnly", err)
	}
	if _, err := BackupFilePath(db, name); !errors.Is(err, ErrSQLiteOnly) {
		t.Errorf("BackupFilePath err = %v, want ErrSQLiteOnly", err)
	}
	if err := DeleteBackup(db, name); !errors.Is(err, ErrSQLiteOnly) {
		t.Errorf("DeleteBackup err = %v, want ErrSQLiteOnly", err)
	}
	// 拒绝先于白名单校验：PG 上即使非法名也返回 ErrSQLiteOnly（文案统一）
	if _, err := BackupFilePath(db, "../evil"); !errors.Is(err, ErrSQLiteOnly) {
		t.Errorf("BackupFilePath(非法名) err = %v, want ErrSQLiteOnly", err)
	}
}

// TestBackupFamilyWorksOnSQLite 反向锁定：同一套函数在 SQLite 上不受 D2 改造影响
// （拒绝分支只在 PG 生效）。
func TestBackupFamilyWorksOnSQLite(t *testing.T) {
	db := openSeeded(t)
	if _, err := DBFilePath(db); err != nil {
		t.Fatalf("DBFilePath(sqlite): %v", err)
	}
	if _, _, err := CreateBackup(db, time.Now()); err != nil {
		t.Fatalf("CreateBackup(sqlite): %v", err)
	}
	infos, err := ListBackups(db)
	if err != nil || len(infos) != 1 {
		t.Fatalf("ListBackups(sqlite) = %v, %v; want 1 份备份", infos, err)
	}
}

// TestPostgresLiveIntegration live PostgreSQL 集成回归（D2）：设 CATMAIL_TEST_PG_DSN
// 指向可写的空库才运行。自测命令（一次性容器，宿主机需 docker）：
//
//	docker run --rm -d --name catmail-pg -e POSTGRES_USER=catmail \
//	  -e POSTGRES_PASSWORD=catmail -e POSTGRES_DB=catmail_test -p 5432:5432 postgres:16
//	CATMAIL_TEST_PG_DSN='postgres://catmail:catmail@127.0.0.1:5432/catmail_test?sslmode=disable' \
//	  go test -count=1 -run TestPostgresLive ./internal/store/ ./internal/rotator/
func TestPostgresLiveIntegration(t *testing.T) {
	dsn := os.Getenv("CATMAIL_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("CATMAIL_TEST_PG_DSN 未设置：跳过 live PostgreSQL 集成测试（本机无 PG）")
	}
	db, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open(pg): %v", err)
	}
	if !IsPostgres(db) {
		t.Fatal("Open(pg DSN) 应回到 postgres 方言")
	}
	if err := EnsureSeed(db, "admin", "admin123"); err != nil {
		t.Fatalf("EnsureSeed(pg): %v", err)
	}
	var userCount int64
	if err := db.Model(&model.User{}).Count(&userCount).Error; err != nil || userCount == 0 {
		t.Fatalf("seed 用户缺失: n=%d err=%v", userCount, err)
	}

	// 唯一约束冲突翻译（与 sqlite 同口径，P2-13）：重复用户名 → gorm.ErrDuplicatedKey
	dup := model.User{Username: "admin", PasswordHash: "x"}
	if err := db.Create(&dup).Error; !errors.Is(err, gorm.ErrDuplicatedKey) {
		t.Errorf("PG 唯一冲突应翻译为 gorm.ErrDuplicatedKey, got %v", err)
	}

	// 基本读写往返（时间/布尔列口径）：created_at 读回 time.Time、bool 原样
	ch := model.Channel{Name: pgUniqueName("cm-live"), Provider: "mock", Status: model.ChannelEnabled}
	if err := db.Create(&ch).Error; err != nil {
		t.Fatalf("create channel(pg): %v", err)
	}
	var got model.Channel
	if err := db.First(&got, ch.ID).Error; err != nil {
		t.Fatalf("reload channel(pg): %v", err)
	}
	if got.CreatedAt.IsZero() {
		t.Error("created_at 未按 time.Time 读回")
	}

	// 备份族拒绝（真实 PG 连接上同样生效）
	if _, _, err := CreateBackup(db, time.Now()); !errors.Is(err, ErrSQLiteOnly) {
		t.Errorf("CreateBackup(pg) err = %v, want ErrSQLiteOnly", err)
	}
}

// pgUniqueName 生成测试专用唯一名（live PG 库跨运行持久，避免唯一索引冲突）。
func pgUniqueName(prefix string) string {
	return prefix + "-" + time.Now().Format("20060102-150405.000000000")
}
