package main

import (
	"compress/gzip"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"catmail/internal/api"
	"catmail/internal/web"
)

// newStaticEngine 构造仅挂载静态兜底路由的引擎（registerStatic 是 cmd 层装配逻辑）。
// 全局中间件与 main.go 的交付层装配对齐（安全响应头 + gzip，W2.5/O45）；Logger 与
// Recovery 与本组断言无关，保持用例最小不挂。
func newStaticEngine(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(api.SecurityHeaders(), web.Gzip())
	registerStatic(r)
	return r
}

// do 执行请求并返回响应。
func do(t *testing.T, r http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestNoRouteAPIPrefixJSON404 /api、/v1、/emails（含子路径）未知路径返回统一
// 404 错误体，绝不落 SPA index.html（SPEC §7 / P2-12）。
func TestNoRouteAPIPrefixJSON404(t *testing.T) {
	r := newStaticEngine(t)
	for _, path := range []string{"/api", "/api/", "/api/nope", "/v1", "/v1/nope", "/emails", "/emails/nope"} {
		w := do(t, r, http.MethodGet, path)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s → %d, want 404", path, w.Code)
			continue
		}
		ct := w.Header().Get("Content-Type")
		if !strings.Contains(ct, "application/json") {
			t.Errorf("%s Content-Type = %q, want application/json", path, ct)
		}
		body := w.Body.String()
		if !strings.Contains(body, `"error"`) || strings.Contains(body, "<!doctype html>") {
			t.Errorf("%s 响应体应为统一错误体，got: %s", path, body)
		}
	}
}

// TestNoRouteSPAFallback 非 API 路径回退 index.html（SPA 前端路由如 /channels）。
func TestNoRouteSPAFallback(t *testing.T) {
	r := newStaticEngine(t)
	for _, path := range []string{"/", "/channels", "/logs?page=2", "/deep/nested/route"} {
		w := do(t, r, http.MethodGet, path)
		if w.Code != http.StatusOK {
			t.Errorf("%s → %d, want 200", path, w.Code)
			continue
		}
		ct := w.Header().Get("Content-Type")
		if !strings.Contains(ct, "text/html") {
			t.Errorf("%s Content-Type = %q, want text/html", path, ct)
		}
		if !strings.Contains(w.Body.String(), "<!doctype html>") {
			t.Errorf("%s 应回退 index.html", path)
		}
	}
}

// TestNoRouteServesEmbeddedAssets 命中 embed FS 的静态文件（assets/* 哈希名）
// 直接直出，不落 index.html。
func TestNoRouteServesEmbeddedAssets(t *testing.T) {
	r := newStaticEngine(t)
	matches, err := fs.Glob(web.FS(), "assets/*")
	if err != nil || len(matches) == 0 {
		t.Fatalf("dist/assets 为空，embed 产物缺失（glob err=%v）", err)
	}
	path := "/" + matches[0]
	w := do(t, r, http.MethodGet, path)
	if w.Code != http.StatusOK {
		t.Fatalf("%s → %d, want 200", path, w.Code)
	}
	if strings.Contains(w.Body.String(), "<!doctype html>") {
		t.Errorf("%s 应直出静态文件而非 index.html", path)
	}
}

// pickAsset 取 dist/assets 下第一个 ≥ minSize 的 .js/.css 产物（返回 FS 内相对
// 路径，如 assets/index-x.js）。跳过哈希小 chunk——不足 gzipMinSize 的响应会被
// 压缩中间件合法跳过，不适用于压缩激活路径的断言。
func pickAsset(t *testing.T, minSize int) string {
	t.Helper()
	for _, pattern := range []string{"assets/*.js", "assets/*.css"} {
		matches, err := fs.Glob(web.FS(), pattern)
		if err != nil {
			t.Fatalf("glob %s: %v", pattern, err)
		}
		for _, name := range matches {
			b, err := fs.ReadFile(web.FS(), name)
			if err != nil {
				t.Fatalf("read %s: %v", name, err)
			}
			if len(b) >= minSize {
				return name
			}
		}
	}
	t.Fatalf("dist/assets 无 ≥ %d 字节的 .js/.css 产物", minSize)
	return ""
}

// TestStaticGzipNegotiation 静态资产 gzip 协商（W2.5/O45）：带 Accept-Encoding:
// gzip 时 Content-Encoding: gzip 且 body 可解压为原始内容；不带时不压缩、body
// 原样；两种请求均带 Vary: Accept-Encoding（未压缩候选也标注，防共享缓存串味）。
func TestStaticGzipNegotiation(t *testing.T) {
	r := newStaticEngine(t)
	name := pickAsset(t, 1<<10) // ≥ 1KB，避开「已知小体积不压」分支
	raw, err := fs.ReadFile(web.FS(), name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	path := "/" + name

	// 带 gzip：压缩 + 可解压还原
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("%s(gzip) → %d, want 200", path, w.Code)
	}
	if ce := w.Header().Get("Content-Encoding"); ce != "gzip" {
		t.Errorf("%s(gzip) Content-Encoding = %q, want gzip", path, ce)
	}
	zr, err := gzip.NewReader(w.Body)
	if err != nil {
		t.Fatalf("%s(gzip) body 无法按 gzip 解压: %v", path, err)
	}
	got, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("%s(gzip) body 解压失败: %v", path, err)
	}
	if string(got) != string(raw) {
		t.Errorf("%s(gzip) 解压结果与原始产物不一致（%d vs %d 字节）", path, len(got), len(raw))
	}

	// 不带 gzip：不压缩，body 原样
	w2 := do(t, r, http.MethodGet, path)
	if w2.Code != http.StatusOK {
		t.Fatalf("%s → %d, want 200", path, w2.Code)
	}
	if ce := w2.Header().Get("Content-Encoding"); ce != "" {
		t.Errorf("%s（无 Accept-Encoding）Content-Encoding = %q, want 空", path, ce)
	}
	if w2.Body.String() != string(raw) {
		t.Errorf("%s 未压缩响应体应与原始产物一致（%d vs %d 字节）", path, w2.Body.Len(), len(raw))
	}

	// Vary：压缩与未压缩候选都要带
	for i, resp := range []*httptest.ResponseRecorder{w, w2} {
		if v := resp.Header().Get("Vary"); !strings.Contains(v, "Accept-Encoding") {
			t.Errorf("%s Vary = %q, want 含 Accept-Encoding（用例 %d）", path, v, i)
		}
	}
}

// TestStaticCacheControl assets/ 哈希产物一年 immutable 长缓存；index.html（含
// 直接请求 /index.html）与 SPA fallback 恒 no-cache（W2.5/O45）。
func TestStaticCacheControl(t *testing.T) {
	r := newStaticEngine(t)
	asset := "/" + pickAsset(t, 0)
	w := do(t, r, http.MethodGet, asset)
	cc := w.Header().Get("Cache-Control")
	if !strings.Contains(cc, "public") || !strings.Contains(cc, "max-age=31536000") || !strings.Contains(cc, "immutable") {
		t.Errorf("%s Cache-Control = %q, want public, max-age=31536000, immutable", asset, cc)
	}

	for _, path := range []string{"/", "/channels", "/logs?page=2", "/index.html"} {
		w := do(t, r, http.MethodGet, path)
		if cc := w.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("%s Cache-Control = %q, want no-cache", path, cc)
		}
	}
}

// TestSecurityHeaders 全局安全响应头（W2.5/O45）：静态直出、SPA fallback、API
// JSON 404 一律携带 nosniff / XFO DENY / Referrer-Policy / Permissions-Policy /
// X-Robots-Tag / CSP。
func TestSecurityHeaders(t *testing.T) {
	r := newStaticEngine(t)
	asset := "/" + pickAsset(t, 0)
	const csp = "default-src 'self'; script-src 'self' 'unsafe-inline'; " +
		"style-src 'self' 'unsafe-inline' https:; img-src 'self' data: https:; " +
		"font-src 'self' data:; connect-src 'self'; frame-src 'self' data: blob:; " +
		"object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'"
	want := map[string]string{
		"X-Content-Type-Options":  "nosniff",
		"X-Frame-Options":         "DENY",
		"Referrer-Policy":         "same-origin",
		"Permissions-Policy":      "camera=(), microphone=(), geolocation=()",
		"X-Robots-Tag":            "noindex, nofollow",
		"Content-Security-Policy": csp,
	}
	for _, path := range []string{"/", "/channels", "/api/nope", asset} {
		w := do(t, r, http.MethodGet, path)
		for k, v := range want {
			if got := w.Header().Get(k); got != v {
				t.Errorf("%s %s = %q, want %q", path, k, got, v)
			}
		}
	}
}

// TestRangeRequestNotCompressed Range 与 gzip 不可混用（W2.5/O45）：带 Range 头
// 的请求即使 Accept-Encoding: gzip 也不压缩，分段内容原样直出。
func TestRangeRequestNotCompressed(t *testing.T) {
	r := newStaticEngine(t)
	path := "/" + pickAsset(t, 200) // 分段 100 字节，须取更大的产物
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("Range", "bytes=0-99")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusPartialContent {
		t.Fatalf("%s(Range) → %d, want 206", path, w.Code)
	}
	if ce := w.Header().Get("Content-Encoding"); ce != "" {
		t.Errorf("%s(Range) Content-Encoding = %q, want 空（Range 不压缩）", path, ce)
	}
	if n := w.Body.Len(); n != 100 {
		t.Errorf("%s(Range) body = %d 字节, want 100", path, n)
	}
}
