// audit_test.go 管理操作审计（E4）：插桩断言（成功才记/失败不记/操作者与 IP 正确、
// settings 不含敏感值）、查询端点分页筛选与鉴权。Detail 截断与变更字段比对为纯函数单测。
package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"

	"catmail/internal/model"
)

// ---------- 测试脚手架 ----------

// auditRows 按 id 正序取回全部审计行（审计只增不改，id 即写入顺序）。
func auditRows(t *testing.T, db *gorm.DB) []model.AuditLog {
	t.Helper()
	var rows []model.AuditLog
	if err := db.Order("id ASC").Find(&rows).Error; err != nil {
		t.Fatalf("query audit rows: %v", err)
	}
	return rows
}

// requireAuditCount 断言审计行总数。
func requireAuditCount(t *testing.T, db *gorm.DB, want int) {
	t.Helper()
	rows := auditRows(t, db)
	if len(rows) != want {
		t.Fatalf("audit rows: want %d got %d: %+v", want, len(rows), rows)
	}
}

// lastAudit 取最新一条审计行（无行时 Fatal）。
func lastAudit(t *testing.T, db *gorm.DB) model.AuditLog {
	t.Helper()
	rows := auditRows(t, db)
	if len(rows) == 0 {
		t.Fatalf("audit rows: want >=1 got 0")
	}
	return rows[len(rows)-1]
}

// testChannelBody 合法渠道创建请求体（mock 平台，免外部依赖）。
const testChannelBody = `{"name":"e2e-audit","provider":"mock","from_email":"a@b.com"}`

// ---------- 渠道插桩 ----------

// TestAuditChannelActions 渠道五类操作的审计：成功才记、失败不记、
// username/IP/target/detail 正确；测试直发（test）不产生审计行。
func TestAuditChannelActions(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")

	// 创建成功 → channel_create（种子已含示例渠道 #1，新渠道自增为 #2）
	mustStatus(t, doReq(r, "POST", "/api/channels", cookie, "", testChannelBody), http.StatusOK)
	row := lastAudit(t, db)
	if row.Username != "admin" || row.Action != model.AuditChannelCreate {
		t.Fatalf("create audit: username=%q action=%q", row.Username, row.Action)
	}
	if row.Target != "channel#2 name=e2e-audit" {
		t.Fatalf("create target = %q", row.Target)
	}
	if row.IP != "192.0.2.1" {
		t.Fatalf("create ip = %q, want httptest 默认来源 192.0.2.1", row.IP)
	}
	if !strings.Contains(row.Detail, "provider=mock") {
		t.Fatalf("create detail = %q, want 含 provider=mock", row.Detail)
	}

	// 创建失败（缺 name → 400）→ 不记
	mustStatus(t, doReq(r, "POST", "/api/channels", cookie, "", `{"provider":"mock"}`), http.StatusBadRequest)
	requireAuditCount(t, db, 1)

	// 重名失败（400）→ 不记
	mustStatus(t, doReq(r, "POST", "/api/channels", cookie, "", testChannelBody), http.StatusBadRequest)
	requireAuditCount(t, db, 1)

	// 更新成功 → channel_update（秘钥值不入审计）
	mustStatus(t, doReq(r, "PUT", "/api/channels/2", cookie, "",
		`{"priority":5,"api_key":"secret-key-value"}`), http.StatusOK)
	row = lastAudit(t, db)
	if row.Action != model.AuditChannelUpdate || !strings.Contains(row.Detail, "provider=mock") {
		t.Fatalf("update audit: action=%q detail=%q", row.Action, row.Detail)
	}
	if strings.Contains(row.Detail, "secret-key-value") || strings.Contains(row.Target, "secret-key-value") {
		t.Fatalf("渠道秘钥泄漏进审计: %+v", row)
	}

	// 更新失败（渠道不存在 → 404）→ 不记
	mustStatus(t, doReq(r, "PUT", "/api/channels/999", cookie, "", `{"priority":5}`), http.StatusNotFound)
	requireAuditCount(t, db, 2)

	// 启停成功 → channel_status
	mustStatus(t, doReq(r, "POST", "/api/channels/2/status", cookie, "", `{"action":"disable"}`), http.StatusOK)
	row = lastAudit(t, db)
	if row.Action != model.AuditChannelStatus || row.Detail != "action=disable" {
		t.Fatalf("status audit: action=%q detail=%q", row.Action, row.Detail)
	}
	// 启停失败（非法 action → 400）→ 不记
	mustStatus(t, doReq(r, "POST", "/api/channels/2/status", cookie, "", `{"action":"nope"}`), http.StatusBadRequest)
	requireAuditCount(t, db, 3)

	// 复制成功 → channel_duplicate（target 为副本，detail 指向来源）
	mustStatus(t, doReq(r, "POST", "/api/channels/2/duplicate", cookie, "", ""), http.StatusOK)
	row = lastAudit(t, db)
	if row.Action != model.AuditChannelDuplicate {
		t.Fatalf("duplicate audit: action=%q", row.Action)
	}
	if row.Target != "channel#3 name=e2e-audit-副本" {
		t.Fatalf("duplicate target = %q", row.Target)
	}
	if row.Detail != "from=channel#2 name=e2e-audit" {
		t.Fatalf("duplicate detail = %q", row.Detail)
	}

	// 渠道测试直发不在插桩清单（E4）：不产生审计行
	mustStatus(t, doReq(r, "POST", "/api/channels/2/test", cookie, "", `{"to":"x@y.com"}`), http.StatusOK)
	requireAuditCount(t, db, 4)

	// 删除失败（不存在 → 404）→ 不记
	mustStatus(t, doReq(r, "DELETE", "/api/channels/999", cookie, "", ""), http.StatusNotFound)
	requireAuditCount(t, db, 4)

	// 删除成功 → channel_delete
	mustStatus(t, doReq(r, "DELETE", "/api/channels/2", cookie, "", ""), http.StatusOK)
	row = lastAudit(t, db)
	if row.Action != model.AuditChannelDelete || row.Target != "channel#2 name=e2e-audit" {
		t.Fatalf("delete audit: action=%q target=%q", row.Action, row.Target)
	}
	requireAuditCount(t, db, 5)
}

// ---------- Key 插桩 ----------

// TestAuditKeyActions Key 增删改的审计：明文 key 绝不入审计；webhook 连通性
// 测试（key webhook/test）与渠道测试同属实测动作，不产生审计行。
func TestAuditKeyActions(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")

	// 创建成功 → key_create
	mustStatus(t, doReq(r, "POST", "/api/keys", cookie, "", `{"name":"audit-key"}`), http.StatusOK)
	row := lastAudit(t, db)
	if row.Action != model.AuditKeyCreate || row.Target != "key#1 name=audit-key" {
		t.Fatalf("key create audit: action=%q target=%q", row.Action, row.Target)
	}
	if !strings.Contains(row.Detail, "daily_quota=0") || !strings.Contains(row.Detail, "rate_per_sec=0") {
		t.Fatalf("key create detail = %q", row.Detail)
	}
	// 明文 key（cm- 前缀 32 位 hex）不得出现在任何审计列
	for _, col := range []string{row.Target, row.Detail} {
		if strings.Contains(col, "cm-") {
			t.Fatalf("key 明文泄漏进审计: %+v", row)
		}
	}

	// 创建失败（空名称 → 400）→ 不记
	mustStatus(t, doReq(r, "POST", "/api/keys", cookie, "", `{"name":"  "}`), http.StatusBadRequest)
	requireAuditCount(t, db, 1)

	// 更新成功 → key_update，detail 仅字段名（地址指向本地拒绝端口，零外部网络）
	mustStatus(t, doReq(r, "PUT", "/api/keys/1", cookie, "",
		`{"enabled":false,"webhook_url":"http://127.0.0.1:1/hook?token=topsecret"}`), http.StatusOK)
	row = lastAudit(t, db)
	if row.Action != model.AuditKeyUpdate || row.Detail != "fields=enabled,webhook_url" {
		t.Fatalf("key update audit: action=%q detail=%q", row.Action, row.Detail)
	}
	if strings.Contains(row.Target+row.Detail, "topsecret") {
		t.Fatalf("webhook 地址（含 token）值泄漏进审计: %+v", row)
	}

	// webhook 连通性测试不记审计（恒 200，成败经 ok 字段上报）
	mustStatus(t, doReq(r, "POST", "/api/keys/1/webhook/test", cookie, "", ""), http.StatusOK)
	requireAuditCount(t, db, 2)

	// 删除失败（不存在 → 404）→ 不记
	mustStatus(t, doReq(r, "DELETE", "/api/keys/999", cookie, "", ""), http.StatusNotFound)
	requireAuditCount(t, db, 2)

	// 删除成功 → key_delete
	mustStatus(t, doReq(r, "DELETE", "/api/keys/1", cookie, "", ""), http.StatusOK)
	row = lastAudit(t, db)
	if row.Action != model.AuditKeyDelete || row.Target != "key#1 name=audit-key" {
		t.Fatalf("key delete audit: action=%q target=%q", row.Action, row.Target)
	}
	requireAuditCount(t, db, 3)
}

// ---------- 账号插桩 ----------

// TestAuditUserActions 账号增删与重置密码的审计：密码明文/哈希绝不入审计。
func TestAuditUserActions(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")
	const password = "super-secret-pw-42"

	// 创建成功 → user_create
	mustStatus(t, doReq(r, "POST", "/api/users", cookie, "",
		fmt.Sprintf(`{"username":"auditor","password":%q}`, password)), http.StatusOK)
	row := lastAudit(t, db)
	if row.Action != model.AuditUserCreate || row.Target != "user#2 name=auditor" {
		t.Fatalf("user create audit: action=%q target=%q", row.Action, row.Target)
	}
	if row.Detail != "" {
		t.Fatalf("user create detail = %q, want 空", row.Detail)
	}
	// 创建失败（重名 → 400）→ 不记
	mustStatus(t, doReq(r, "POST", "/api/users", cookie, "",
		fmt.Sprintf(`{"username":"auditor","password":%q}`, password)), http.StatusBadRequest)
	requireAuditCount(t, db, 1)

	// 重置密码成功 → user_reset_password + password_reset_linkage（安全加固三件套，
	// C：联动结果单独落一条）两条；新密码不出现在任何审计列
	mustStatus(t, doReq(r, "PUT", "/api/users/2/password", cookie, "",
		fmt.Sprintf(`{"new_password":%q}`, password)), http.StatusOK)
	row = lastAudit(t, db)
	if row.Action != model.AuditPasswordResetLinkage || row.Target != "user#2 name=auditor" {
		t.Fatalf("reset linkage audit: action=%q target=%q", row.Action, row.Target)
	}
	if row.Detail != "sessions_revoked=0 api_keys_disabled=0" {
		t.Fatalf("reset linkage detail = %q", row.Detail)
	}
	rows := []model.AuditLog{}
	if err := db.Where("action = ?", model.AuditUserResetPassword).Order("id DESC").Limit(1).Find(&rows).Error; err != nil || len(rows) != 1 {
		t.Fatalf("reset audit rows: n=%d err=%v", len(rows), err)
	}
	if rows[0].Target != "user#2 name=auditor" {
		t.Fatalf("reset audit target=%q", rows[0].Target)
	}
	for _, r2 := range append(rows, row) {
		for _, col := range []string{r2.Target, r2.Detail} {
			if strings.Contains(col, password) {
				t.Fatalf("密码泄漏进审计: %+v", r2)
			}
		}
	}

	// 删除成功 → user_delete
	mustStatus(t, doReq(r, "DELETE", "/api/users/2", cookie, "", ""), http.StatusOK)
	row = lastAudit(t, db)
	if row.Action != model.AuditUserDelete || row.Target != "user#2 name=auditor" {
		t.Fatalf("user delete audit: action=%q target=%q", row.Action, row.Target)
	}
	requireAuditCount(t, db, 4)
}

// ---------- 设置/清理/备份插桩 ----------

// TestAuditSettingsUpdate settings PUT 审计：detail 仅变更字段名清单，
// 任何字段值（notify_webhook_url 可能内嵌 token）不入审计。
func TestAuditSettingsUpdate(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")

	body := `{"rotation":"round_robin","max_attempts":3,"cooldown_seconds":60,` +
		`"consecutive_failure_threshold":5,"log_retention_days":90,` +
		`"notify_inapp_enabled":true,"notify_webhook_enabled":false,` +
		`"notify_webhook_url":"https://hook.example.com/secret-token-xyz",` +
		`"notify_events":"","log_body_enabled":true,"backup_daily_enabled":false,` +
		`"suppression_auto_enabled":true,"bounce_rate_threshold":5}`
	mustStatus(t, doReq(r, "PUT", "/api/settings", cookie, "", body), http.StatusOK)
	row := lastAudit(t, db)
	if row.Action != model.AuditSettingsUpdate || row.Target != "settings#1" {
		t.Fatalf("settings audit: action=%q target=%q", row.Action, row.Target)
	}
	// notify_events 空串被规范化为全事件串，同样计为变更字段
	if row.Detail != "fields=rotation,notify_inapp_enabled,notify_webhook_url,notify_events" {
		t.Fatalf("settings detail = %q", row.Detail)
	}
	if strings.Contains(row.Detail, "secret-token-xyz") || strings.Contains(row.Detail, "round_robin") {
		t.Fatalf("settings 字段值泄漏进审计: %+v", row)
	}

	// 无任何变更的 PUT → 审计仍记（操作成功），detail 为空
	mustStatus(t, doReq(r, "PUT", "/api/settings", cookie, "", body), http.StatusOK)
	row = lastAudit(t, db)
	if row.Action != model.AuditSettingsUpdate || row.Detail != "" {
		t.Fatalf("no-change settings audit: action=%q detail=%q", row.Action, row.Detail)
	}

	// 校验失败（400）→ 不记
	mustStatus(t, doReq(r, "PUT", "/api/settings", cookie, "", `{"rotation":"bogus"}`), http.StatusBadRequest)
	requireAuditCount(t, db, 2)
}

// TestAuditPurgeAndBackup 日志清理与备份的审计：purge detail 含 mode 与删除数；
// 备份创建/删除各记一条。
func TestAuditPurgeAndBackup(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")

	// 造 2 条渠道 #7 的发送日志后按渠道清理
	for i := 0; i < 2; i++ {
		if err := db.Create(&model.SendLog{
			MessageID: fmt.Sprintf("m-%d", i), ChannelID: 7,
			Status: model.SendStatusSent, CreatedAt: time.Now(),
		}).Error; err != nil {
			t.Fatalf("seed send log: %v", err)
		}
	}
	mustStatus(t, doReq(r, "POST", "/api/logs/purge", cookie, "", `{"mode":"channel","channel_id":7}`), http.StatusOK)
	row := lastAudit(t, db)
	if row.Action != model.AuditPurgeLogs || row.Target != "send_logs" {
		t.Fatalf("purge audit: action=%q target=%q", row.Action, row.Target)
	}
	if row.Detail != "mode=channel channel_id=7 deleted=2" {
		t.Fatalf("purge detail = %q", row.Detail)
	}

	// 清理失败（非法 mode → 400）→ 不记
	mustStatus(t, doReq(r, "POST", "/api/logs/purge", cookie, "", `{"mode":"nope"}`), http.StatusBadRequest)
	requireAuditCount(t, db, 1)

	// 备份创建成功 → backup_create（SQLite 临时库支持 VACUUM INTO）
	mustStatus(t, doReq(r, "POST", "/api/backups", cookie, "", ""), http.StatusOK)
	row = lastAudit(t, db)
	if row.Action != model.AuditBackupCreate {
		t.Fatalf("backup create audit: action=%q", row.Action)
	}
	name := strings.TrimPrefix(row.Target, "backup name=")
	if !strings.HasPrefix(row.Target, "backup name=catmail-backup-") || !strings.Contains(row.Detail, "size=") {
		t.Fatalf("backup create: target=%q detail=%q", row.Target, row.Detail)
	}

	// 备份删除失败（非法名 → 400）→ 不记
	mustStatus(t, doReq(r, "DELETE", "/api/backups/bogus.db", cookie, "", ""), http.StatusBadRequest)
	requireAuditCount(t, db, 2)

	// 备份删除成功 → backup_delete
	mustStatus(t, doReq(r, "DELETE", "/api/backups/"+name, cookie, "", ""), http.StatusOK)
	row = lastAudit(t, db)
	if row.Action != model.AuditBackupDelete || row.Target != "backup name="+name {
		t.Fatalf("backup delete audit: action=%q target=%q", row.Action, row.Target)
	}
	requireAuditCount(t, db, 3)
}

// ---------- 查询端点 ----------

// seedAudit 直接写入 n 条指定动作/操作者/时间的审计行（id 自增）。
func seedAudit(t *testing.T, db *gorm.DB, username, action string, n int, base time.Time) {
	t.Helper()
	rows := make([]model.AuditLog, n)
	for i := range rows {
		rows[i] = model.AuditLog{
			Username: username, Action: action,
			Target: fmt.Sprintf("%s#%d name=x", strings.SplitN(action, "_", 2)[0], i+1),
			IP:     "192.0.2.9", CreatedAt: base.Add(time.Duration(i) * time.Minute),
		}
	}
	// 分批写入（每批 100 行），避免超长参数语句
	for start := 0; start < n; start += 100 {
		end := min(start+100, n)
		if err := db.Create(rows[start:end]).Error; err != nil {
			t.Fatalf("seed audit: %v", err)
		}
	}
}

// TestAuditListPaginationAndFilters GET /api/audit：未鉴权 401、分页参数校验、
// created_at 倒序、action 前缀筛选与 username 精确筛选。
func TestAuditListPaginationAndFilters(t *testing.T) {
	r, db := newTestEnv(t)
	cookie := login(t, r, "admin", "admin123")

	// 未鉴权 → 401
	mustStatus(t, doReq(r, "GET", "/api/audit", "", "", ""), http.StatusUnauthorized)

	base := time.Now().Add(-time.Hour)
	seedAudit(t, db, "admin", "channel_create", 3, base)
	seedAudit(t, db, "admin", "key_create", 2, base.Add(10*time.Minute))
	seedAudit(t, db, "ops", "channel_delete", 1, base.Add(20*time.Minute))

	// 全量：created_at 倒序（最早写入的 channel_create 在最后）
	w := mustStatus(t, doReq(r, "GET", "/api/audit?page=1&page_size=10", cookie, "", ""), http.StatusOK)
	res := decode(t, w)
	if res["total"].(float64) != 6 {
		t.Fatalf("total = %v, want 6", res["total"])
	}
	items := res["items"].([]any)
	first := items[0].(map[string]any)
	if first["action"] != "channel_delete" || first["username"] != "ops" {
		t.Fatalf("first item = %v, want ops/channel_delete（最新）", first)
	}
	if first["ip"] != "192.0.2.9" {
		t.Fatalf("first ip = %v", first["ip"])
	}
	if first["created_at"] == "" {
		t.Fatalf("created_at 缺失: %v", first)
	}

	// 分页：page_size=2 第 2 页为倒数第 3、4 条（key_create 最新一条 + channel_create 最新一条）
	w = mustStatus(t, doReq(r, "GET", "/api/audit?page=2&page_size=2", cookie, "", ""), http.StatusOK)
	res = decode(t, w)
	items = res["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("page2 len = %d, want 2", len(items))
	}
	if items[0].(map[string]any)["action"] != "key_create" || items[1].(map[string]any)["action"] != "channel_create" {
		t.Fatalf("page2 = %v, want [key_create channel_create]", items)
	}

	// action 前缀筛选：channel 命中 channel_*（3+1 条），不命中 key_*
	w = mustStatus(t, doReq(r, "GET", "/api/audit?action=channel", cookie, "", ""), http.StatusOK)
	res = decode(t, w)
	if res["total"].(float64) != 4 {
		t.Fatalf("action=channel total = %v, want 4", res["total"])
	}

	// LIKE 通配符转义：% 不当通配符（前缀 "channel%" 只匹配字面量开头）
	w = mustStatus(t, doReq(r, "GET", "/api/audit?action="+strings.ReplaceAll("channel%", "%", "%25"), cookie, "", ""), http.StatusOK)
	if got := decode(t, w)["total"].(float64); got != 0 {
		t.Fatalf("action=channel%%25 total = %v, want 0（通配符已转义）", got)
	}

	// username 精确筛选
	w = mustStatus(t, doReq(r, "GET", "/api/audit?username=ops", cookie, "", ""), http.StatusOK)
	res = decode(t, w)
	if res["total"].(float64) != 1 {
		t.Fatalf("username=ops total = %v, want 1", res["total"])
	}

	// 分页参数非法 → 400
	mustStatus(t, doReq(r, "GET", "/api/audit?page=0", cookie, "", ""), http.StatusBadRequest)
	mustStatus(t, doReq(r, "GET", "/api/audit?page_size=101", cookie, "", ""), http.StatusBadRequest)
	mustStatus(t, doReq(r, "GET", "/api/audit?page_size=abc", cookie, "", ""), http.StatusBadRequest)
}

// ---------- 纯函数：截断与变更比对 ----------

// TestTruncateAuditDetail 明细按 Unicode 码点截断至 500（中文按 1 字符计）。
func TestTruncateAuditDetail(t *testing.T) {
	if got := truncateAuditDetail(strings.Repeat("中", 500)); len([]rune(got)) != 500 {
		t.Fatalf("500 字边界被截断: %d", len([]rune(got)))
	}
	if got := truncateAuditDetail(strings.Repeat("中", 501)); len([]rune(got)) != 500 {
		t.Fatalf("501 字未截断到 500: %d", len([]rune(got)))
	}
	// 截断不产生非法 UTF-8（逐码点截取）
	if got := truncateAuditDetail(strings.Repeat("雪mail❄", 120)); !utf8.ValidString(got) {
		t.Fatalf("截断结果非法 UTF-8: %q", got)
	}
}

// TestChangedSettingFields 变更字段名比对：仅输出差异字段、固定 json 名顺序。
func TestChangedSettingFields(t *testing.T) {
	old := settingsDTO{Rotation: "priority", MaxAttempts: 3, LogRetentionDays: 90}
	new := old
	new.Rotation = "round_robin"
	new.LogRetentionDays = 30
	got := joinAuditFields(changedSettingFields(old, new))
	if got != "fields=rotation,log_retention_days" {
		t.Fatalf("changed fields = %q", got)
	}
	if got := joinAuditFields(changedSettingFields(old, old)); got != "" {
		t.Fatalf("no-change fields = %q, want 空串", got)
	}
}
