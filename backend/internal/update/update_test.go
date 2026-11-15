// update_test.go Check 缓存/GitHub 容忍/启动通知广播的测试：
// httptest 伪装 GitHub API，零真实外网。
package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"catmail/internal/model"
	"catmail/internal/notify"
	"catmail/internal/store"
)

// newTestChecker 构造指向 httptest 伪装 GitHub 的 Checker（允许 http 资产）。
func newTestChecker(t *testing.T, serverURL, current string, prerelease bool) *Checker {
	t.Helper()
	ck := New("owner/repo", prerelease, current)
	ck.apiBase = serverURL
	ck.insecureAllowHTTP = true
	return ck
}

// fakeGitHub 伪装 GitHub Releases API：mu 保护 hits 计数；releaseJSON 为返回体
// （nil = 返回 500，用于 5xx 容忍测试）。返回服务器与 hits 计数指针。
func fakeGitHub(t *testing.T, releaseJSON any) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/owner/repo/releases/latest", func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		writeFakeRelease(w, releaseJSON)
	})
	mux.HandleFunc("GET /repos/owner/repo/releases", func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		// prerelease 路径返回数组形态
		arr, ok := releaseJSON.([]ghRelease)
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(arr)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &hits
}

func writeFakeRelease(w http.ResponseWriter, releaseJSON any) {
	rel, ok := releaseJSON.(*ghRelease)
	if !ok {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rel)
}

// TestCheckCacheHit 首查写入缓存，TTL 内第二次 Check 不再出网（hits=1）。
func TestCheckCacheHit(t *testing.T) {
	pub := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	srv, hits := fakeGitHub(t, &ghRelease{
		TagName: "v1.2.0", HTMLURL: "https://github.com/owner/repo/releases/tag/v1.2.0",
		Body: "release notes", PublishedAt: &pub,
	})
	ck := newTestChecker(t, srv.URL, "v1.0.0", false)
	for range 2 {
		res, err := ck.Check(context.Background())
		if err != nil || res.Error != "" {
			t.Fatalf("Check: err=%v res.Error=%q", err, res.Error)
		}
		if !res.UpdateAvailable || res.Latest != "v1.2.0" || res.Notes != "release notes" {
			t.Fatalf("unexpected result: %+v", res)
		}
		if res.ReleaseURL == "" || res.PublishedAt == nil || res.CheckedAt.IsZero() || res.Current != "v1.0.0" {
			t.Fatalf("missing fields: %+v", res)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1（应命中缓存）", hits.Load())
	}
}

// TestCheckGitHub5xxNotCached GitHub 5xx：error 如实返回、不缓存（两次 Check
// 两次出网），且失败结果绝不伪装成「已是最新」。
func TestCheckGitHub5xxNotCached(t *testing.T) {
	srv, hits := fakeGitHub(t, nil) // 500
	ck := newTestChecker(t, srv.URL, "v1.0.0", false)
	for range 2 {
		res, err := ck.Check(context.Background())
		if err != nil {
			t.Fatalf("Check err: %v", err)
		}
		if res.Error == "" {
			t.Fatal("5xx 应如实返回 error 字段")
		}
		if res.UpdateAvailable || res.Latest != "" {
			t.Fatalf("失败结果不得携带更新判断: %+v", res)
		}
		if res.Current != "v1.0.0" || res.CheckedAt.IsZero() {
			t.Fatalf("失败结果仍应含 current/checked_at: %+v", res)
		}
	}
	if hits.Load() != 2 {
		t.Fatalf("hits = %d, want 2（错误不缓存）", hits.Load())
	}
}

// TestCheckPrerelease 开启 prerelease 时走列表端点取首个非草稿（含 prerelease
// 标记的发布；tag 仍须为纯 vX.Y.Z 才可比较——带 -rc.1 后缀的 tag 按设计不可比）。
func TestCheckPrerelease(t *testing.T) {
	srv, _ := fakeGitHub(t, []ghRelease{
		{TagName: "v2.1.0", Prerelease: true, Draft: false, HTMLURL: "https://github.com/owner/repo/releases/tag/v2.1.0"},
		{TagName: "v1.9.0", Draft: false},
	})
	ck := newTestChecker(t, srv.URL, "v1.0.0", true)
	res, err := ck.Check(context.Background())
	if err != nil || res.Error != "" {
		t.Fatalf("Check: err=%v res.Error=%q", err, res.Error)
	}
	if res.Latest != "v2.1.0" || !res.UpdateAvailable {
		t.Fatalf("prerelease 应被纳入比较: %+v", res)
	}
	// 未开启 prerelease：走 /releases/latest 端点，fake 未挂该形态（releaseJSON
	// 为数组）→ 500 → error 如实返回，证明两条路径确实分开。
	ck2 := newTestChecker(t, srv.URL, "v1.0.0", false)
	res2, err := ck2.Check(context.Background())
	if err != nil {
		t.Fatalf("Check err: %v", err)
	}
	if res2.Error == "" {
		t.Fatalf("prerelease 关闭时应走 latest 端点（fake 未提供），got %+v", res2)
	}
}

// TestCheckIncomparable 版本不可比（如 current=dev）：如实 update_available=false
// 且给出 error 说明，不猜测升级。
func TestCheckIncomparable(t *testing.T) {
	srv, _ := fakeGitHub(t, &ghRelease{TagName: "v1.2.0", HTMLURL: "https://github.com/owner/repo"})
	ck := newTestChecker(t, srv.URL, "dev", false)
	res, err := ck.Check(context.Background())
	if err != nil {
		t.Fatalf("Check err: %v", err)
	}
	if res.Error == "" || res.UpdateAvailable {
		t.Fatalf("不可比应如实返回: %+v", res)
	}
}

// TestCheckDisabled 未配置 repo → ErrDisabled。
func TestCheckDisabled(t *testing.T) {
	ck := New("", false, "v1.0.0")
	if _, err := ck.Check(context.Background()); err == nil {
		t.Fatal("未配置 repo 应返回 ErrDisabled")
	}
}

// TestCheckAndNotify 发现新版本 → notify 双通道落 update_available 事件（info 级，
// 标题含新版本号、正文含当前版本与 release_url）。
func TestCheckAndNotify(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() {
		if sqlDB, cerr := db.DB(); cerr == nil {
			_ = sqlDB.Close()
		}
	}()
	if err := store.EnsureSeed(db, "admin", "admin123"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// 开启站内通道（种子默认关闭，双通道按 Setting 开关分流）
	if err := db.Model(&model.Setting{}).Where("id = 1").Update("notify_inapp_enabled", true).Error; err != nil {
		t.Fatalf("enable inapp: %v", err)
	}
	notifier := notify.NewNotifier(db)
	notify.SetDefault(notifier)
	defer notify.SetDefault(nil)

	srv, _ := fakeGitHub(t, &ghRelease{TagName: "v2.0.0", HTMLURL: "https://github.com/owner/repo/releases/tag/v2.0.0"})
	ck := newTestChecker(t, srv.URL, "v1.0.0", false)
	ck.CheckAndNotify()

	var rows []struct {
		Event string
		Level string
		Title string
		Body  string
	}
	if err := db.Table("notifications").Find(&rows).Error; err != nil {
		t.Fatalf("query notifications: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("notifications 行数 = %d, want 1", len(rows))
	}
	r := rows[0]
	if r.Event != notify.EventUpdateAvailable || r.Level != notify.LevelInfo {
		t.Fatalf("event/level = %s/%s, want update_available/info", r.Event, r.Level)
	}
	for _, want := range []string{"v2.0.0", "v1.0.0", "https://github.com/owner/repo/releases/tag/v2.0.0"} {
		if !strings.Contains(r.Title+r.Body, want) {
			t.Errorf("通知内容缺少 %q: title=%q body=%q", want, r.Title, r.Body)
		}
	}
}

// TestCheckAndNotifyNoUpdate 无更新/失败时绝不发通知。
func TestCheckAndNotifyNoUpdate(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() {
		if sqlDB, cerr := db.DB(); cerr == nil {
			_ = sqlDB.Close()
		}
	}()
	if err := store.EnsureSeed(db, "admin", "admin123"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// 开启站内通道（种子默认关闭，双通道按 Setting 开关分流）
	if err := db.Model(&model.Setting{}).Where("id = 1").Update("notify_inapp_enabled", true).Error; err != nil {
		t.Fatalf("enable inapp: %v", err)
	}
	notifier := notify.NewNotifier(db)
	notify.SetDefault(notifier)
	defer notify.SetDefault(nil)

	srv, _ := fakeGitHub(t, &ghRelease{TagName: "v0.1.0", HTMLURL: "https://github.com/owner/repo"})
	ck := newTestChecker(t, srv.URL, "v1.0.0", false) // 当前已比 latest 新
	ck.CheckAndNotify()

	srv2, _ := fakeGitHub(t, nil) // 500
	ck2 := newTestChecker(t, srv2.URL, "v1.0.0", false)
	ck2.CheckAndNotify()

	var n int64
	if err := db.Table("notifications").Count(&n).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("无更新/失败不应发通知, got %d 条", n)
	}
}

// TestAssetName 资产命名约定：catmail_<os>_<arch>，Windows 追加 .exe。
func TestAssetName(t *testing.T) {
	cases := map[[2]string]string{
		{"linux", "amd64"}:   "catmail_linux_amd64",
		{"darwin", "arm64"}:  "catmail_darwin_arm64",
		{"windows", "amd64"}: "catmail_windows_amd64.exe",
		{"linux", "arm64"}:   "catmail_linux_arm64",
	}
	for in, want := range cases {
		if got := assetName(in[0], in[1]); got != want {
			t.Errorf("assetName(%s,%s) = %q, want %q", in[0], in[1], got, want)
		}
	}
}

// TestChecksumFor checksums.txt 解析：sha256sum 两种分隔形态、大小写 hex、
// 畸形行忽略、缺条目报错、非法哈希报错。
func TestChecksumFor(t *testing.T) {
	sum := sha256.Sum256([]byte("NEWBIN"))
	hexSum := hex.EncodeToString(sum[:])

	good := hexSum + "  catmail_linux_amd64\n" + "deadbeef  other.bin\n"
	if got, err := checksumFor([]byte(good), "catmail_linux_amd64"); err != nil || got != hexSum {
		t.Errorf("标准形态解析失败: %q %v", got, err)
	}
	binary := hexSum + " *catmail_linux_amd64\n"
	if got, err := checksumFor([]byte(binary), "catmail_linux_amd64"); err != nil || got != hexSum {
		t.Errorf("二进制形态解析失败: %q %v", got, err)
	}
	upper := "DEADBEEF" + hexSum[8:] + "  catmail_linux_amd64\n"
	if got, err := checksumFor([]byte(upper), "catmail_linux_amd64"); err != nil {
		t.Errorf("大写 hex 应可解析: %q %v", got, err)
	}
	if _, err := checksumFor([]byte("deadbeef  catmail_linux_amd64\n"), "catmail_linux_amd64"); err == nil {
		t.Error("哈希长度非法应报错")
	}
	if _, err := checksumFor([]byte("deadbeef  other.bin\n"), "catmail_linux_amd64"); err == nil {
		t.Error("缺条目应报错")
	}
}

// TestValidateAssetURL 资产地址白名单：https 恒可；http 仅测试注入时放行。
func TestValidateAssetURL(t *testing.T) {
	ck := New("owner/repo", false, "v1.0.0")
	if err := ck.validateAssetURL("https://github.com/x/y"); err != nil {
		t.Errorf("https 应放行: %v", err)
	}
	if err := ck.validateAssetURL("http://github.com/x/y"); err == nil {
		t.Error("生产形态 http 必须拒绝")
	}
	if err := ck.validateAssetURL("file:///etc/passwd"); err == nil {
		t.Error("file scheme 必须拒绝")
	}
	if err := ck.validateAssetURL("https://"); err == nil {
		t.Error("无 host 必须拒绝")
	}
	ck.insecureAllowHTTP = true
	if err := ck.validateAssetURL("http://127.0.0.1:8080/bin"); err != nil {
		t.Errorf("测试注入后 http 应放行: %v", err)
	}
}
