// apply_test.go 更新落盘流程测试：httptest 伪装 release + checksums.txt + 资产，
// 真实替换临时目录内的假二进制并断言内容。覆盖：校验通过/缺失/不匹配三态、
// Docker 检测、apply 互斥锁、下载大小上限、已是最新拒绝。
package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// 伪装的新旧二进制内容。
const (
	oldBin = "#!/bin/sh\necho old-catmail\n"
	newBin = "#!/bin/sh\necho new-catmail-v2\n"
)

// applyFixture apply 测试环境。
type applyFixture struct {
	ck     *Checker
	target string // 伪装的当前可执行文件路径
}

// newApplyFixture 装配 apply 测试环境：
//   - httptest 伪装 GitHub：/bin 返回 binContent、/sums 返回 sumsContent、
//     /repos/owner/repo/releases/latest 返回 mutate 改造后的 release；
//   - 临时目录内写入假当前二进制（oldBin，0o755），ck.exePath 指向它；
//   - 强制非 Docker 环境（t.Setenv 空值）。
//
// 默认：资产与 checksums.txt 齐备且哈希正确（即成功路径）。
func newApplyFixture(t *testing.T, binContent, sumsContent string, mutate func(rel *ghRelease)) *applyFixture {
	t.Helper()
	t.Setenv("CATMAIL_IN_DOCKER", "")

	asset := assetName(runtime.GOOS, runtime.GOARCH)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /bin", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(binContent)))
		_, _ = w.Write([]byte(binContent))
	})
	mux.HandleFunc("GET /sums", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(sumsContent))
	})
	mux.HandleFunc("GET /repos/owner/repo/releases/latest", func(w http.ResponseWriter, req *http.Request) {
		rel := ghRelease{TagName: "v2.0.0", HTMLURL: "https://github.com/owner/repo/releases/tag/v2.0.0"}
		// 默认资产齐备（checksums.txt 在内）；mutate 可整体改写。
		base := req.Host // 资产下载地址指向本测试服务器
		scheme := "http"
		rel.Assets = []ghAsset{
			{Name: asset, BrowserDownloadURL: scheme + "://" + base + "/bin", Size: int64(len(binContent))},
			{Name: checksumAssetName, BrowserDownloadURL: scheme + "://" + base + "/sums", Size: int64(len(sumsContent))},
		}
		if mutate != nil {
			mutate(&rel)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(rel); err != nil {
			t.Errorf("encode release: %v", err)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	target := filepath.Join(dir, "catmail")
	if err := os.WriteFile(target, []byte(oldBin), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}

	ck := newTestChecker(t, srv.URL, "v1.0.0", false)
	ck.exePath = func() (string, error) { return target, nil }
	return &applyFixture{ck: ck, target: target}
}

// validSums newBin 的标准 checksums.txt 内容（sha256sum 形态）。
func validSums(t *testing.T) string {
	t.Helper()
	sum := sha256.Sum256([]byte(newBin))
	return hex.EncodeToString(sum[:]) + "  " + assetName(runtime.GOOS, runtime.GOARCH) + "\nother-file  deadbeef\n"
}

// TestApplySuccess 端到端：下载 → SHA-256 校验 → 临时文件 + rename 原子替换，
// 假二进制内容变为 newBin、权限保持可执行、目录无临时文件残留。
func TestApplySuccess(t *testing.T) {
	f := newApplyFixture(t, newBin, validSums(t), nil)
	res, err := f.ck.Apply(context.Background())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !res.OK || res.InstalledVersion != "v2.0.0" || res.Hint != applyHint {
		t.Fatalf("unexpected result: %+v", res)
	}
	got, err := os.ReadFile(f.target)
	if err != nil || string(got) != newBin {
		t.Fatalf("二进制未替换: content=%q err=%v", got, err)
	}
	if fi, _ := os.Stat(f.target); fi.Mode()&0o111 == 0 {
		t.Error("替换后失去可执行权限")
	}
	entries, _ := os.ReadDir(filepath.Dir(f.target))
	if len(entries) != 1 || entries[0].Name() != "catmail" {
		t.Fatalf("目录应仅剩目标文件（无临时文件残留）: %v", entries)
	}
}

// TestApplyChecksumMismatch SHA-256 不匹配 → 400（ErrChecksumMismatch），且绝不
// 落盘：原二进制内容原样保留。
func TestApplyChecksumMismatch(t *testing.T) {
	bogus := sha256.Sum256([]byte("TAMPERED"))
	bogusSums := hex.EncodeToString(bogus[:]) + "  " + assetName(runtime.GOOS, runtime.GOARCH) + "\n"
	f := newApplyFixture(t, newBin, bogusSums, nil)

	res, err := f.ck.Apply(context.Background())
	if res != nil || !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("want ErrChecksumMismatch, got res=%v err=%v", res, err)
	}
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != 400 {
		t.Fatalf("校验失败应映射 400, got %v", err)
	}
	if got, rerr := os.ReadFile(f.target); rerr != nil || string(got) != oldBin {
		t.Fatalf("校验失败绝不落盘（原文件被改动）: content=%q err=%v", got, rerr)
	}
}

// TestApplyNoChecksum release 缺 checksums.txt → 400 拒绝更新，不落盘。
func TestApplyNoChecksum(t *testing.T) {
	asset := assetName(runtime.GOOS, runtime.GOARCH)
	f := newApplyFixture(t, newBin, validSums(t), func(rel *ghRelease) {
		rel.Assets = []ghAsset{{Name: asset, BrowserDownloadURL: "http://placeholder/bin"}} // 仅二进制资产
	})

	res, err := f.ck.Apply(context.Background())
	if res != nil || !errors.Is(err, ErrNoChecksum) {
		t.Fatalf("want ErrNoChecksum, got res=%v err=%v", res, err)
	}
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != 400 {
		t.Fatalf("缺校验文件应映射 400, got %v", err)
	}
	if got, rerr := os.ReadFile(f.target); rerr != nil || string(got) != oldBin {
		t.Fatalf("拒绝更新不得落盘: content=%q err=%v", got, rerr)
	}
}

// TestApplyChecksumEntryMissing checksums.txt 存在但无当前平台条目 → 拒绝。
func TestApplyChecksumEntryMissing(t *testing.T) {
	f := newApplyFixture(t, newBin, "deadbeef  some-other-binary\n", nil)
	res, err := f.ck.Apply(context.Background())
	if res != nil || err == nil {
		t.Fatalf("缺条目应拒绝, got res=%v err=%v", res, err)
	}
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != 400 {
		t.Fatalf("应映射 400, got %v", err)
	}
}

// TestApplyAssetMissing release 无当前平台资产 → 400。
func TestApplyAssetMissing(t *testing.T) {
	f := newApplyFixture(t, newBin, validSums(t), func(rel *ghRelease) {
		rel.Assets = []ghAsset{
			{Name: "catmail_plan9_amd64", BrowserDownloadURL: "http://placeholder/bin"},
			{Name: checksumAssetName, BrowserDownloadURL: "http://placeholder/sums"},
		}
	})
	res, err := f.ck.Apply(context.Background())
	if res != nil || err == nil {
		t.Fatalf("缺平台资产应报错, got res=%v err=%v", res, err)
	}
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != 400 {
		t.Fatalf("应映射 400, got %v", err)
	}
}

// TestApplyDockerDetection CATMAIL_IN_DOCKER=1 → 容器拒绝（ErrInDocker，400）。
// /.dockerenv 分支在 macOS/Linux 容器外不可稳定构造，以环境变量分支覆盖判定逻辑。
func TestApplyDockerDetection(t *testing.T) {
	f := newApplyFixture(t, newBin, validSums(t), nil)
	t.Setenv("CATMAIL_IN_DOCKER", "1")
	if !InDocker() {
		t.Fatal("CATMAIL_IN_DOCKER=1 应判定为容器部署")
	}
	res, err := f.ck.Apply(context.Background())
	if res != nil || !errors.Is(err, ErrInDocker) {
		t.Fatalf("want ErrInDocker, got res=%v err=%v", res, err)
	}
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != 400 {
		t.Fatalf("容器拒绝应映射 400, got %v", err)
	}
}

// TestInDockerEnvOff 未设置环境变量且无 /.dockerenv 时判定为非容器（CI/本机默认）。
func TestInDockerEnvOff(t *testing.T) {
	t.Setenv("CATMAIL_IN_DOCKER", "")
	if _, err := os.Stat("/.dockerenv"); err != nil && InDocker() {
		t.Fatal("无容器标志时不应判定为容器部署")
	}
}

// TestApplyBusy 进程内互斥：锁被占用时第二次 Apply 立即得到 ErrBusy（400），
// 不排队不阻塞——防并发双更写坏二进制。
func TestApplyBusy(t *testing.T) {
	f := newApplyFixture(t, newBin, validSums(t), nil)
	f.ck.applyMu.Lock() // 模拟一次进行中的更新
	defer f.ck.applyMu.Unlock()

	res, err := f.ck.Apply(context.Background())
	if res != nil || !errors.Is(err, ErrBusy) {
		t.Fatalf("want ErrBusy, got res=%v err=%v", res, err)
	}
}

// TestApplyAlreadyLatest 当前版本不低于 release → 400 拒绝重复更新。
func TestApplyAlreadyLatest(t *testing.T) {
	f := newApplyFixture(t, newBin, validSums(t), nil)
	f.ck.current = "v9.9.9"
	res, err := f.ck.Apply(context.Background())
	if res != nil || err == nil {
		t.Fatalf("已是最新应拒绝, got res=%v err=%v", res, err)
	}
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != 400 {
		t.Fatalf("应映射 400, got %v", err)
	}
}

// TestApplyDownloadTooBig 下载体积超上限：Content-Length 预检与实际读取双检。
func TestApplyDownloadTooBig(t *testing.T) {
	f := newApplyFixture(t, newBin, validSums(t), nil)
	// Content-Length 谎报超限 → 下载前即拒绝
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "999999999999")
		_, _ = w.Write([]byte("x"))
	}))
	defer srv.Close()
	if _, err := f.ck.download(context.Background(), srv.URL, maxBinaryBytes); err == nil {
		t.Fatal("超大 Content-Length 应被拒绝")
	}
	// 实际读取超限（无 Content-Length）→ LimitReader 截断后按超限拒绝
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(maxBinaryBytes+1))
		_, _ = w.Write(make([]byte, maxBinaryBytes+1))
	}))
	defer srv2.Close()
	if _, err := f.ck.download(context.Background(), srv2.URL, maxBinaryBytes); err == nil {
		t.Fatal("实际读取超限应被拒绝")
	}
}
