// Package update 实现自动更新（E8）：从 GitHub Releases 检查新版本
// （GET /api/update/check）与下载替换本机二进制（POST /api/update/apply）。
//
// 设计要点：
//   - 更新源由 CATMAIL_UPDATE_REPO（"owner/repo"）开启，空 = 功能关闭；
//   - 版本比较为手写受限 semver（去 v 前缀、按 "." 分段数字比较、位数不齐补 0），
//     解析失败视为「不可比」并如实呈现 update_available=false，绝不猜测升级；
//   - apply 前强制 SHA-256 校验（release 内 checksums.txt），校验失败绝不落盘；
//   - 替换方式为「同目录临时文件 + fsync + rename 原子替换」，Windows 因无法对
//     运行中 exe rename 而整体拒绝（返回手动替换指引）；
//   - check 结果进程内缓存 1h；GitHub 网络/限流错误如实放入 Result.Error 且不缓存。
package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"

	"catmail/internal/notify"
)

// cacheTTL check 结果进程内缓存时长：GitHub API 匿名额度有限（60 次/h/IP），
// 检查频率必须收敛；/api/update/check 与启动检查共用同一份缓存。
const cacheTTL = time.Hour

// 常用超时：API 查询短超时；二进制下载放宽（受下载体积与带宽影响）。
const (
	apiTimeout      = 15 * time.Second
	downloadTimeout = 10 * time.Minute
)

// maxBinaryBytes 单资产下载上限（256MB）：防异常 release 打爆内存；
// 下载即校验 Content-Length 与实际读取双保险，超限报错。
const maxBinaryBytes int64 = 256 << 20

// ErrDisabled 更新源未配置（CATMAIL_UPDATE_REPO 为空）时由 Check 返回。
var ErrDisabled = errors.New("自动更新未启用（未配置 CATMAIL_UPDATE_REPO）")

// Checker 自动更新器：持有更新源配置、当前版本与 check 结果缓存。
// 零值不可用，经 New 构造；并发安全（缓存互斥 + apply 互斥独立）。
type Checker struct {
	repo       string // "owner/repo"（已在 config 层校验格式）
	prerelease bool   // 是否纳入 prerelease
	current    string // 当前程序版本（internal/version.Version）

	// apiBase GitHub API 根地址（默认 https://api.github.com；测试注入 httptest）。
	apiBase string
	// hc HTTP 客户端（不设全局 Timeout，逐请求 ctx 控制超时，兼顾下载长事务）。
	hc *http.Client
	// insecureAllowHTTP 允许 http:// 资产下载（仅测试注入 httptest 用；生产恒 false，
	// 资产 URL 一律强制 https，防明文链路替换二进制）。
	insecureAllowHTTP bool

	// exePath 当前可执行文件路径获取函数（apply 落盘目标；测试替换为临时文件）。
	exePath func() (string, error)

	mu       sync.Mutex
	cached   *CheckResult
	cachedAt time.Time

	// applyMu apply 互斥：TryLock 失败 = 已有更新在进行，拒绝并发双更。
	applyMu sync.Mutex
}

// New 构造更新器：repo 为 "owner/repo"（config 层已校验格式），current 为当前
// 程序版本（internal/version.Version）。
func New(repo string, prerelease bool, current string) *Checker {
	return &Checker{
		repo:       repo,
		prerelease: prerelease,
		current:    current,
		apiBase:    "https://api.github.com",
		hc:         &http.Client{},
		exePath:    defaultExePath,
	}
}

// NewForTest 测试构造（api 包集成测试注入 httptest 伪装 GitHub 用，生产一律 New）：
// apiBase 覆盖 API 根地址并放行 http:// 资产（测试服务器非 TLS）；exePath 指定
// apply 落盘目标（空串 = 真实可执行文件）。
func NewForTest(repo string, prerelease bool, current, apiBase, exePath string) *Checker {
	ck := New(repo, prerelease, current)
	ck.apiBase = apiBase
	ck.insecureAllowHTTP = true
	if exePath != "" {
		ck.exePath = func() (string, error) { return exePath, nil }
	}
	return ck
}

// CheckResult GET /api/update/check 响应体（同时供启动检查内部使用）。
// CheckedAt 恒有值；GitHub 侧失败时 Error 非空且其余字段（除 Current/InDocker）
// 为零值——错误如实返回、绝不缓存，也绝不把失败伪装成「已是最新」。
type CheckResult struct {
	Current         string     `json:"current"`
	Latest          string     `json:"latest,omitempty"`
	UpdateAvailable bool       `json:"update_available"`
	ReleaseURL      string     `json:"release_url,omitempty"`
	Notes           string     `json:"notes,omitempty"`
	PublishedAt     *time.Time `json:"published_at,omitempty"`
	CheckedAt       time.Time  `json:"checked_at"`
	// InDocker 容器部署标记（/.dockerenv 或 CATMAIL_IN_DOCKER=1）：供前端隐藏
	// 一键更新按钮并改示「拉取新镜像」提示；apply 在该环境恒拒绝。
	InDocker bool `json:"in_docker"`
	// Error GitHub 网络/限流/响应异常文案（人类可读），如实返回不缓存。
	Error string `json:"error,omitempty"`
}

// Check 检查更新：优先返回 1h 内的缓存结果；否则实时查询 GitHub Releases。
// 返回值恒非 nil（失败形态见 CheckResult.Error），error 仅在功能关闭时非 nil。
func (ck *Checker) Check(ctx context.Context) (*CheckResult, error) {
	if ck.repo == "" {
		return nil, ErrDisabled
	}
	ck.mu.Lock()
	if ck.cached != nil && time.Since(ck.cachedAt) < cacheTTL {
		res := ck.cached
		ck.mu.Unlock()
		return res, nil
	}
	ck.mu.Unlock()

	// 缓存外查询（不加锁，防慢网络长时间持锁拖住并发请求；写回时再持锁）。
	res := ck.queryGitHub(ctx)
	res.Current = ck.current
	res.InDocker = InDocker()

	if res.Error == "" {
		ck.mu.Lock()
		ck.cached, ck.cachedAt = res, time.Now()
		ck.mu.Unlock()
	}
	return res, nil
}

// CheckAndNotify 启动检查入口（CATMAIL_UPDATE_CHECK_ON_START）：检查一次，
// 发现新版本时经 notify.Emit 广播 update_available 事件（info 级，站内通知 +
// Webhook 双通道按 Setting 开关分流）。GitHub 失败/不可比/无更新一律静默
// （只记日志）——启动检查是尽力而为的增值功能，绝不阻塞或影响常驻服务。
func (ck *Checker) CheckAndNotify() {
	ctx, cancel := context.WithTimeout(context.Background(), apiTimeout+downloadTimeout)
	defer cancel()
	res, err := ck.Check(ctx)
	if err != nil {
		return // 功能关闭：静默
	}
	if res.Error != "" {
		log.Printf("[update] 启动检查失败（不影响服务）: %s", res.Error)
		return
	}
	if !res.UpdateAvailable {
		return
	}
	log.Printf("[update] 启动检查发现新版本: %s -> %s (%s)", res.Current, res.Latest, res.ReleaseURL)
	notify.Emit(notify.EventUpdateAvailable, notify.LevelInfo,
		fmt.Sprintf("发现新版本 %s", res.Latest),
		fmt.Sprintf("当前版本 %s，最新版本 %s。查看更新内容与升级说明: %s",
			res.Current, res.Latest, res.ReleaseURL),
		nil)
}

// ---------- GitHub Releases 查询 ----------

// ghRelease GitHub Release API 响应（仅取所需字段）。
type ghRelease struct {
	TagName     string     `json:"tag_name"`
	HTMLURL     string     `json:"html_url"`
	Body        string     `json:"body"`
	Draft       bool       `json:"draft"`
	Prerelease  bool       `json:"prerelease"`
	PublishedAt *time.Time `json:"published_at"`
	Assets      []ghAsset  `json:"assets"`
}

// ghAsset GitHub Release 资产（二进制与 checksums.txt 均为资产）。
type ghAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

// queryGitHub 实时查询 GitHub Releases 并组装结果（不含 Current/InDocker，
// 由 Check 补齐）。任何网络/HTTP/解析/比较异常都归入 Result.Error，不向上抛。
func (ck *Checker) queryGitHub(ctx context.Context) *CheckResult {
	res := &CheckResult{CheckedAt: time.Now()}
	rel, err := ck.latestRelease(ctx)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.Latest = rel.TagName
	res.ReleaseURL = rel.HTMLURL
	res.Notes = rel.Body
	res.PublishedAt = rel.PublishedAt

	// 版本比较：任一侧解析失败 = 不可比 → 如实 update_available=false（不猜测）。
	// cmp = Compare(current, latest) < 0 即当前版本落后于最新发布 → 有更新。
	if cmp, cerr := Compare(ck.current, rel.TagName); cerr != nil {
		res.Error = fmt.Sprintf("版本号不可比较（current=%q latest=%q）: %v", ck.current, rel.TagName, cerr)
	} else {
		res.UpdateAvailable = cmp < 0
	}
	return res
}

// latestRelease 取「应作为最新版本」的 Release：
//   - 未开启 prerelease：GET /releases/latest（GitHub 语义即最新正式版）；
//   - 开启 prerelease：GET /releases?per_page=10 取首个非草稿（含 prerelease，
//     GitHub 的 latest 端点会跳过 prerelease，故需列表端点）。
func (ck *Checker) latestRelease(ctx context.Context) (*ghRelease, error) {
	if !ck.prerelease {
		var rel ghRelease
		if err := ck.getJSON(ctx, fmt.Sprintf("%s/repos/%s/releases/latest", ck.apiBase, ck.repo), &rel); err != nil {
			return nil, err
		}
		return &rel, nil
	}
	var rels []ghRelease
	if err := ck.getJSON(ctx, fmt.Sprintf("%s/repos/%s/releases?per_page=10", ck.apiBase, ck.repo), &rels); err != nil {
		return nil, err
	}
	for i := range rels {
		if !rels[i].Draft {
			return &rels[i], nil
		}
	}
	return nil, fmt.Errorf("仓库 %s 暂无可用发布版本", ck.repo)
}

// getJSON GET 一个 GitHub API 端点并解码 JSON：网络错误、非 2xx（含 403 限流，
// 按 X-RateLimit-Remaining=0 识别给出人话）与 JSON 解析失败均转为错误。
func (ck *Checker) getJSON(ctx context.Context, endpoint string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("构造请求失败: %w", err)
	}
	// GitHub API 建议 Accept 头；UA 缺失时 GitHub 会拒绝裸请求。
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "CatMail/auto-update")

	resp, err := ck.hc.Do(req)
	if err != nil {
		return fmt.Errorf("请求 GitHub 失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden && resp.Header.Get("X-RateLimit-Remaining") == "0" {
		return errors.New("GitHub API 限流（匿名额度已用尽，约 1 小时后恢复）")
	}
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("仓库 %s 不存在或无发布版本", ck.repo)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("GitHub API 返回 %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("GitHub 响应解析失败: %w", err)
	}
	return nil
}

// validateAssetURL 资产下载地址白名单：必须为完整 http(s) URL 且 host 非空；
// 生产强制 https（insecureAllowHTTP 仅测试注入 httptest 时为 true）。资产内容
// 完整性由 checksums.txt 的 SHA-256 兜底，此处是第一道来源约束。
func (ck *Checker) validateAssetURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && !(ck.insecureAllowHTTP && u.Scheme == "http")) || u.Host == "" {
		return fmt.Errorf("资产下载地址非法: %q", raw)
	}
	return nil
}

// findAsset 按名字在 release 资产中查找，未找到返回 nil。
func findAsset(rel *ghRelease, name string) *ghAsset {
	for i := range rel.Assets {
		if rel.Assets[i].Name == name {
			return &rel.Assets[i]
		}
	}
	return nil
}

// checksumAssetName release 内校验文件约定名。
const checksumAssetName = "checksums.txt"

// assetName 当前平台的发布资产名约定：catmail_<os>_<arch>，Windows 追加 .exe
// （与发布流水线的 go build -o 产物命名一一对应，如 catmail_linux_amd64、
// catmail_darwin_arm64、catmail_windows_amd64.exe）。
func assetName(goos, goarch string) string {
	name := fmt.Sprintf("catmail_%s_%s", goos, goarch)
	if goos == "windows" {
		name += ".exe"
	}
	return name
}
