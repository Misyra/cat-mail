// api_update_test.go E8 自动更新端点测试：会话保护、功能关闭 400、
// 接线后 check/apply 走 httptest 伪装 GitHub、notify_events 五事件枚举往返。
package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"catmail/internal/store"
	"catmail/internal/update"
)

// newUpdateTestEnv 装配带/不带 update checker 的测试环境（临时 SQLite + gin）。
func newUpdateTestEnv(t *testing.T, ck *update.Checker) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := store.EnsureSeed(db, "admin", "admin123"); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	r := gin.New()
	r.Use(gin.Recovery())
	if ck != nil {
		Register(r, db, WithUpdateChecker(ck))
	} else {
		Register(r, db)
	}
	return r
}

// TestUpdateEndpointsDisabled 未配置 CATMAIL_UPDATE_REPO：两端点保持注册，
// 会话内访问返回 400（功能关闭），未登录 401（会话组保护）。
func TestUpdateEndpointsDisabled(t *testing.T) {
	r := newUpdateTestEnv(t, nil)
	cookie := login(t, r, "admin", "admin123")

	w := doReq(r, "GET", "/api/update/check", cookie, "", "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("check disabled: %d body=%s", w.Code, w.Body.String())
	}
	if m := decode(t, w.Body.String()); m["error"] == nil {
		t.Fatalf("应返回统一错误体: %s", w.Body.String())
	}
	w = doReq(r, "POST", "/api/update/apply", cookie, "", "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("apply disabled: %d", w.Code)
	}
	w = doReq(r, "GET", "/api/update/check", "", "", "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("未登录应为 401, got %d", w.Code)
	}
}

// TestUpdateCheckWired 接线后 check：返回 current/latest/update_available/
// release_url/checked_at/in_docker 契约字段，update_available 方向正确。
func TestUpdateCheckWired(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"v2.0.0","html_url":"https://github.com/owner/repo/releases/tag/v2.0.0","body":"notes"}`))
	}))
	t.Cleanup(srv.Close)

	ck := update.NewForTest("owner/repo", false, "v1.0.0", srv.URL, "")
	r := newUpdateTestEnv(t, ck)
	cookie := login(t, r, "admin", "admin123")

	w := doReq(r, "GET", "/api/update/check", cookie, "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("check: %d body=%s", w.Code, w.Body.String())
	}
	m := decode(t, w.Body.String())
	if m["current"] != "v1.0.0" || m["latest"] != "v2.0.0" || m["update_available"] != true {
		t.Fatalf("check 契约字段异常: %v", m)
	}
	if m["release_url"] == nil || m["checked_at"] == nil || m["in_docker"] == nil {
		t.Fatalf("缺 release_url/checked_at/in_docker: %v", m)
	}
}

// TestUpdateCheckWiredError GitHub 失败 → 200 + error 字段如实返回（非 500）。
func TestUpdateCheckWiredError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	ck := update.NewForTest("owner/repo", false, "v1.0.0", srv.URL, "")
	r := newUpdateTestEnv(t, ck)
	cookie := login(t, r, "admin", "admin123")
	w := doReq(r, "GET", "/api/update/check", cookie, "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GitHub 失败应 200+error 字段, got %d", w.Code)
	}
	m := decode(t, w.Body.String())
	if m["error"] == nil || m["update_available"] != false {
		t.Fatalf("应如实携带 error 且不假装最新: %v", m)
	}
}

// TestUpdateApplyWired 接线后 apply：伪装 release+checksums+资产，经端点真实
// 替换临时目录假二进制（NewForTest 指定 exePath 落盘目标）。
func TestUpdateApplyWired(t *testing.T) {
	newBin := "NEW-BINARY-CONTENT"
	sum := sha256.Sum256([]byte(newBin))
	asset := fmt.Sprintf("catmail_%s_%s", runtime.GOOS, runtime.GOARCH)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /bin", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(newBin))
	})
	mux.HandleFunc("GET /sums", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(hex.EncodeToString(sum[:]) + "  " + asset + "\n"))
	})
	mux.HandleFunc("GET /repos/owner/repo/releases/latest", func(w http.ResponseWriter, req *http.Request) {
		rel := map[string]any{
			"tag_name": "v2.0.0",
			"assets": []map[string]any{
				{"name": asset, "browser_download_url": "http://" + req.Host + "/bin"},
				{"name": "checksums.txt", "browser_download_url": "http://" + req.Host + "/sums"},
			},
		}
		_ = json.NewEncoder(w).Encode(rel)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	target := filepath.Join(t.TempDir(), "catmail")
	if err := os.WriteFile(target, []byte("OLD"), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	ck := update.NewForTest("owner/repo", false, "v1.0.0", srv.URL, target)
	t.Setenv("CATMAIL_IN_DOCKER", "")

	r := newUpdateTestEnv(t, ck)
	cookie := login(t, r, "admin", "admin123")
	w := doReq(r, "POST", "/api/update/apply", cookie, "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("apply: %d body=%s", w.Code, w.Body.String())
	}
	m := decode(t, w.Body.String())
	if m["ok"] != true || m["installed_version"] != "v2.0.0" || m["hint"] == nil {
		t.Fatalf("apply 响应契约异常: %v", m)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != newBin {
		t.Fatalf("二进制未替换: %q %v", got, err)
	}
}

// TestUpdateApplyDocker400 容器环境（CATMAIL_IN_DOCKER=1）→ 400 容器升级指引，
// 且不出网。
func TestUpdateApplyDocker400(t *testing.T) {
	ck := update.NewForTest("owner/repo", false, "v1.0.0", "http://127.0.0.1:1", "")
	t.Setenv("CATMAIL_IN_DOCKER", "1")
	r := newUpdateTestEnv(t, ck)
	cookie := login(t, r, "admin", "admin123")
	w := doReq(r, "POST", "/api/update/apply", cookie, "", "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("容器 apply 应 400, got %d body=%s", w.Code, w.Body.String())
	}
	m := decode(t, w.Body.String())
	errObj, _ := m["error"].(map[string]any)
	if errObj == nil || errObj["message"] == nil {
		t.Fatalf("应返回统一错误体: %v", m)
	}
}

// TestUpdateApplyNoChecksum400 缺 checksums.txt → 400「缺少校验文件，拒绝更新」。
func TestUpdateApplyNoChecksum400(t *testing.T) {
	asset := fmt.Sprintf("catmail_%s_%s", runtime.GOOS, runtime.GOARCH)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/owner/repo/releases/latest", func(w http.ResponseWriter, _ *http.Request) {
		rel := map[string]any{
			"tag_name": "v2.0.0",
			"assets":   []map[string]any{{"name": asset, "browser_download_url": "http://127.0.0.1:1/bin"}},
		}
		_ = json.NewEncoder(w).Encode(rel)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	ck := update.NewForTest("owner/repo", false, "v1.0.0", srv.URL, "")
	t.Setenv("CATMAIL_IN_DOCKER", "")
	r := newUpdateTestEnv(t, ck)
	cookie := login(t, r, "admin", "admin123")
	w := doReq(r, "POST", "/api/update/apply", cookie, "", "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("缺校验文件应 400, got %d body=%s", w.Code, w.Body.String())
	}
	if body := w.Body.String(); !strings.Contains(body, "缺少校验文件") {
		t.Fatalf("错误文案应明示缺少校验文件: %s", body)
	}
}

// TestNotifyEventsIncludeUpdateAvailable notify_events 五事件枚举往返（E8）：
// update_available 可通过校验、空串规范化为五 token 全集、未知 token 仍 400。
func TestNotifyEventsIncludeUpdateAvailable(t *testing.T) {
	r := newUpdateTestEnv(t, nil)
	cookie := login(t, r, "admin", "admin123")

	// 单独订阅 update_available 可通过校验并原样回显
	body := `{"rotation":"priority","max_attempts":3,"cooldown_seconds":60,` +
		`"consecutive_failure_threshold":5,"log_retention_days":90,` +
		`"notify_inapp_enabled":true,"notify_webhook_enabled":false,"notify_webhook_url":"",` +
		`"notify_events":"update_available"}`
	w := doReq(r, "PUT", "/api/settings", cookie, "", body)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT update_available: %d body=%s", w.Code, w.Body.String())
	}
	if m := decode(t, w.Body.String()); m["notify_events"] != "update_available" {
		t.Fatalf("notify_events echo: %v", m["notify_events"])
	}
	w = doReq(r, "GET", "/api/settings", cookie, "", "")
	if m := decode(t, w.Body.String()); m["notify_events"] != "update_available" {
		t.Fatalf("GET notify_events: %v", m["notify_events"])
	}

	// 空串 → 五 token 全集（含 update_available）
	body = `{"rotation":"priority","max_attempts":3,"cooldown_seconds":60,` +
		`"consecutive_failure_threshold":5,"log_retention_days":90,"notify_events":""}`
	w = doReq(r, "PUT", "/api/settings", cookie, "", body)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT empty events: %d", w.Code)
	}
	// 送达链路三件套新增 bounce_rate_exceeded 事件 token
	allEvents := "channel_disabled,channel_cooldown,channel_recovered,send_failed,update_available,bounce_rate_exceeded"
	if m := decode(t, w.Body.String()); m["notify_events"] != allEvents {
		t.Fatalf("empty normalized: %v", m["notify_events"])
	}

	// 未知 token 仍拒绝
	body = `{"rotation":"priority","max_attempts":3,"cooldown_seconds":60,` +
		`"consecutive_failure_threshold":5,"log_retention_days":90,"notify_events":"bogus"}`
	w = doReq(r, "PUT", "/api/settings", cookie, "", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("未知 token 应 400, got %d", w.Code)
	}
}
