// apply.go 更新落盘（E8）：下载 release 资产 → SHA-256 强校验 → 同目录临时文件
// + fsync + rename 原子替换。校验失败绝不落盘；容器/Windows 环境整体拒绝。
package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// minFreeBytesMul 磁盘空间预检倍数：目标分区剩余空间须 ≥ 2× 二进制大小
// （旧文件尚未删除，需容纳新文件本身 + 落盘余量）。
const minFreeBytesMul = 2

// applyHint 更新成功后的固定提示（与响应契约一致）。
const applyHint = "重启 catmail 生效，可用 catmail doctor 验证"

// 业务性失败（HTTP 400 类）：文案面向管理员，直接透传到响应。
var (
	// ErrInDocker 容器部署：请拉取新镜像升级。
	ErrInDocker = &HTTPError{Status: 400, Msg: "检测到容器部署，请通过拉取新镜像升级（docker pull 后重建容器），不支持文件级自更新"}
	// ErrNoChecksum release 缺少 checksums.txt。
	ErrNoChecksum = &HTTPError{Status: 400, Msg: "缺少校验文件 checksums.txt，拒绝更新"}
	// ErrChecksumMismatch SHA-256 不匹配。
	ErrChecksumMismatch = &HTTPError{Status: 400, Msg: "SHA-256 校验失败，拒绝更新（资产与 checksums.txt 不一致或被篡改）"}
	// ErrBusy 已有更新任务进行中（进程内互斥防并发双更）。
	ErrBusy = &HTTPError{Status: 400, Msg: "已有更新任务正在进行，请稍后再试"}
)

// HTTPError 携带建议 HTTP 状态码的业务性错误：api 层按 Status 透传文案，
// 其余（IO/网络等意外错误）一律 500 通用文案。
type HTTPError struct {
	Status int
	Msg    string
}

func (e *HTTPError) Error() string { return e.Msg }

// ApplyResult POST /api/update/apply 成功响应体。
type ApplyResult struct {
	OK               bool   `json:"ok"`
	InstalledVersion string `json:"installed_version"`
	Hint             string `json:"hint"`
}

// InDocker 判定容器部署：/.dockerenv 存在或 CATMAIL_IN_DOCKER=1。
// 容器内二进制在镜像层，rename 替换随容器重建即失效（且通常非 root 无写权限），
// 统一拒绝并指引走镜像升级。
func InDocker() bool {
	if os.Getenv("CATMAIL_IN_DOCKER") == "1" {
		return true
	}
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	return false
}

// defaultExePath 当前可执行文件真实路径（测试经 ck.exePath 替换为临时文件）。
func defaultExePath() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(p)
}

// Apply 执行自更新并返回结果。业务性失败（容器/Windows/资产缺失/校验失败/
// 磁盘不足/并发）返回 *HTTPError（api 层按状态码透传），意外 IO 错误原样返回。
// 全程持有进程内互斥：并发第二次调用得到 ErrBusy，不做排队。
func (ck *Checker) Apply(ctx context.Context) (*ApplyResult, error) {
	if ck.repo == "" {
		return nil, &HTTPError{Status: 400, Msg: ErrDisabled.Error()}
	}
	if !ck.applyMu.TryLock() {
		return nil, ErrBusy
	}
	defer ck.applyMu.Unlock()

	if InDocker() {
		return nil, ErrInDocker
	}
	// Windows 无法对运行中 exe rename（文件被锁）：整体拒绝并给手动替换指引，
	// 用 runtime.GOOS 判定（而非等 rename 报错），口径明确可测试。
	if runtime.GOOS == "windows" {
		return nil, &HTTPError{Status: 400, Msg: fmt.Sprintf(
			"Windows 平台不支持运行中自更新（运行中的可执行文件无法被替换）。请手动升级：下载 %s/releases 中的 %s 与 checksums.txt，校验后覆盖当前程序文件再重启",
			ck.repo, assetName(runtime.GOOS, runtime.GOARCH))}
	}

	// 实时取 release（不走 check 缓存：apply 需要资产列表，且下载前重取保证
	// 所装版本与 release 元数据一致）。
	rel, err := ck.latestRelease(ctx)
	if err != nil {
		return nil, &HTTPError{Status: 400, Msg: "获取最新版本失败: " + err.Error()}
	}
	// 当前版本不低于最新发布（cmp ≥ 0）即拒绝重复更新；不可比（cerr 非 nil）时
	// 不拦——后续 SHA-256 校验与落盘安全性不受版本号影响，由管理员自行判断。
	if cmp, cerr := Compare(ck.current, rel.TagName); cerr == nil && cmp >= 0 {
		return nil, &HTTPError{Status: 400, Msg: fmt.Sprintf("当前已是最新版本（current=%s latest=%s）", ck.current, rel.TagName)}
	}

	name := assetName(runtime.GOOS, runtime.GOARCH)
	asset := findAsset(rel, name)
	if asset == nil {
		return nil, &HTTPError{Status: 400, Msg: fmt.Sprintf("release %s 缺少当前平台资产 %s，拒绝更新", rel.TagName, name)}
	}
	sumAsset := findAsset(rel, checksumAssetName)
	if sumAsset == nil {
		// 安全取舍：没有校验文件就没有下载完整性依据（GitHub 传输层之外无保证），
		// 宁可拒绝更新也不能把未知内容写到自己的可执行文件位置。
		return nil, ErrNoChecksum
	}

	// 先下 checksums.txt（小文件），再下二进制并校验；校验通过前零落盘。
	sums, err := ck.download(ctx, sumAsset.BrowserDownloadURL, 1<<20)
	if err != nil {
		return nil, &HTTPError{Status: 400, Msg: "下载 checksums.txt 失败: " + err.Error()}
	}
	want, err := checksumFor(sums, name)
	if err != nil {
		return nil, &HTTPError{Status: 400, Msg: err.Error()}
	}
	bin, err := ck.download(ctx, asset.BrowserDownloadURL, maxBinaryBytes)
	if err != nil {
		return nil, &HTTPError{Status: 400, Msg: "下载更新包失败: " + err.Error()}
	}
	got := sha256.Sum256(bin)
	if !strings.EqualFold(hex.EncodeToString(got[:]), want) {
		log.Printf("[update] 校验失败: release=%s asset=%s want=%s got=%x", rel.TagName, name, want, got)
		return nil, ErrChecksumMismatch
	}

	target, err := ck.exePath()
	if err != nil {
		return nil, fmt.Errorf("定位当前可执行文件失败: %w", err)
	}
	// 磁盘空间预检（目标分区剩余 < 2× 二进制大小 → 拒绝）：写一半失败会把
	// 目录留在「临时文件已占空间」的脏状态，事前检查比事后清理更稳。
	if err := diskFreeCheck(filepath.Dir(target), int64(len(bin))); err != nil {
		return nil, err
	}
	if err := replaceBinary(target, bin); err != nil {
		return nil, err
	}
	log.Printf("[update] 已安装更新: %s -> %s（%s，%d 字节）", ck.current, rel.TagName, target, len(bin))
	return &ApplyResult{OK: true, InstalledVersion: rel.TagName, Hint: applyHint}, nil
}

// replaceBinary 原子替换二进制：同目录临时文件（写满 + fsync 保证数据落盘）→
// rename（同文件系统内原子）。调用方已保证非 Windows（运行中 exe 不可 rename）。
// 失败清理临时文件；rename 失败时原文件未动，服务仍以旧版本运行。
func replaceBinary(target string, bin []byte) error {
	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, ".catmail-update-*")
	if err != nil {
		return &HTTPError{Status: 400, Msg: "创建临时文件失败（目录不可写？）: " + err.Error()}
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = tmp.Close(); _ = os.Remove(tmpName) }

	if _, err := tmp.Write(bin); err != nil {
		cleanup()
		return fmt.Errorf("写入临时文件失败: %w", err)
	}
	if err := tmp.Chmod(0o755); err != nil { // 可执行位（Windows 分支不会走到这里）
		cleanup()
		return fmt.Errorf("设置可执行权限失败: %w", err)
	}
	if err := tmp.Sync(); err != nil { // fsync：断电也不留半个新二进制
		cleanup()
		return fmt.Errorf("刷盘失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("关闭临时文件失败: %w", err)
	}
	if err := os.Rename(tmpName, target); err != nil {
		_ = os.Remove(tmpName)
		return &HTTPError{Status: 400, Msg: "替换可执行文件失败: " + err.Error()}
	}
	return nil
}

// checksumFor 从 checksums.txt（sha256sum 格式：<64位hex><空格|空格+星号><文件名>）
// 中取出 name 对应的哈希；文件缺失条目视为校验文件不完整，拒绝更新。
func checksumFor(sums []byte, name string) (string, error) {
	for _, line := range strings.Split(string(sums), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		idx := strings.IndexAny(line, " \t")
		if idx <= 0 {
			continue // 畸形行忽略
		}
		sum, file := line[:idx], strings.TrimLeft(strings.TrimSpace(line[idx:]), "*")
		if file != name {
			continue
		}
		if len(sum) != 64 || !isHex(sum) {
			return "", fmt.Errorf("checksums.txt 中 %s 的哈希格式非法", name)
		}
		return sum, nil
	}
	return "", fmt.Errorf("checksums.txt 中缺少 %s 的条目，拒绝更新", name)
}

// isHex 判断 64 位哈希是否全为十六进制字符。
func isHex(s string) bool {
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

// download 下载一个资产到内存：https 白名单（见 validateAssetURL）、大小上限
// （Content-Length 与实际读取双检）、逐请求超时。返回全量内容（校验通过后才落盘，
// 天然满足「校验失败不落盘」）。
func (ck *Checker) download(ctx context.Context, rawURL string, maxBytes int64) ([]byte, error) {
	if err := ck.validateAssetURL(rawURL); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "CatMail/auto-update")

	resp, err := ck.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("下载返回 %d", resp.StatusCode)
	}
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		if n, perr := strconv.ParseInt(cl, 10, 64); perr == nil && n > maxBytes {
			return nil, fmt.Errorf("资产大小 %d 字节超过上限 %d", n, maxBytes)
		}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("资产超过大小上限 %d 字节", maxBytes)
	}
	return data, nil
}

// diskFreeCheck 磁盘空间预检：目标所在分区剩余 < minFreeBytesMul×size 时拒绝。
// 平台不支持（见 disk_free_other.go）时跳过检查：无法检查不应阻断更新，
// 完整性与落盘安全已由 SHA-256 + fsync + rename 保证。
func diskFreeCheck(dir string, size int64) error {
	free, err := diskFreeBytes(dir)
	if err != nil {
		if errors.Is(err, errDiskUnsupported) {
			return nil
		}
		return fmt.Errorf("检查磁盘剩余空间失败: %w", err)
	}
	if free < uint64(size)*minFreeBytesMul {
		return &HTTPError{Status: 400, Msg: fmt.Sprintf("磁盘剩余空间不足（%d 字节，需至少 %d 字节 = 2× 更新包大小），拒绝更新", free, size*minFreeBytesMul)}
	}
	return nil
}
