// docs_test.go B2 内嵌文档测试：/docs 页面内容、/docs/openapi.yaml 可解析性，
// 以及 gin 路由表与规范路径的一致性对照（防新增端点漏文档）。
// 外部测试包（docs_test）：api 包经 server.go 依赖本包，反向引用需外层包。
package docs_test

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"

	"catmail/internal/api"
	"catmail/internal/store"
)

// newEngine 装配与 cmd 层一致的路由（临时 SQLite + api.Register），
// 路由一致性测试以此为「实现真值」。
func newEngine(t *testing.T) *gin.Engine {
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
	api.Register(r, db)
	return r
}

func doGet(r *gin.Engine, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// parseSpec 从实际路由取回规范原文并解析（同时验证端点可服务）。
func parseSpec(t *testing.T, r *gin.Engine) map[string]any {
	t.Helper()
	w := doGet(r, "/docs/openapi.yaml")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /docs/openapi.yaml: status=%d", w.Code)
	}
	var spec map[string]any
	if err := yaml.Unmarshal(w.Body.Bytes(), &spec); err != nil {
		t.Fatalf("openapi.yaml 不能被 yaml.v3 解析: %v", err)
	}
	return spec
}

// TestSpecServedYAML 规范原文可访问、可被 yaml.v3 解析，且包含统一错误体
// 与关键结构（openapi 版本、paths、components.schemas.Error）。
func TestSpecServedYAML(t *testing.T) {
	r := newEngine(t)
	w := doGet(r, "/docs/openapi.yaml")
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "yaml") {
		t.Errorf("Content-Type = %q, want 含 yaml", ct)
	}
	spec := parseSpec(t, r)
	if v, ok := spec["openapi"].(string); !ok || !strings.HasPrefix(v, "3.1") {
		t.Errorf("openapi = %v, want 3.1.x", spec["openapi"])
	}
	paths, ok := spec["paths"].(map[string]any)
	if !ok || len(paths) == 0 {
		t.Fatalf("paths 缺失或为空")
	}
	components, ok := spec["components"].(map[string]any)
	if !ok {
		t.Fatalf("components 缺失")
	}
	schemas, _ := components["schemas"].(map[string]any)
	if _, ok := schemas["Error"]; !ok {
		t.Errorf("components.schemas.Error（统一错误体）缺失")
	}
}

// TestDocsPage HTML 文档页 200 且包含关键端点字样、版本说明与内嵌 JSON；
// 零外部依赖（不引用任何外部脚本/样式）。
func TestDocsPage(t *testing.T) {
	r := newEngine(t)
	w := doGet(r, "/docs")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /docs: status=%d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	body := w.Body.String()
	for _, want := range []string{
		"CatMail API 文档",
		"与 SPEC.md 同步维护",       // 版本/同步说明
		"application/json",     // 内嵌 JSON 数据块
		"prefers-color-scheme", // 深浅色自适应
		"/v1/send",             // 关键端点字样（内嵌 JSON 中）
		"/api/auth/login",
		"/api/channels/{id}/test",
		"/api/channels/{id}/duplicate",
		"/api/logs/export",
		"/api/logs/{id}/resend",
		"/api/keys",
		"/api/settings/notify/test",
		"/api/backups/{name}/download",
		"/metrics",
		"catmail_session", // 会话 Cookie 鉴权说明
		"invalid_request", // 统一错误体错误类型
	} {
		if !strings.Contains(body, want) {
			t.Errorf("文档页缺少关键内容 %q", want)
		}
	}
	// 零外部 CDN 依赖：不允许任何外部 src/href 引用
	for _, banned := range []string{`src="http`, `href="http`, `src='http`, `href='http`} {
		if strings.Contains(body, banned) {
			t.Errorf("文档页引用了外部资源 %q（要求零外部依赖）", banned)
		}
	}
}

// ginPathToSpec gin 的 :param 参数风格转 OpenAPI {param} 风格。
func ginPathToSpec(p string) string {
	if i := strings.IndexByte(p, ':'); i >= 0 {
		var b strings.Builder
		for i := 0; i < len(p); i++ {
			if p[i] == ':' {
				j := i + 1
				for j < len(p) && p[j] != '/' {
					j++
				}
				b.WriteByte('{')
				b.WriteString(p[i+1 : j])
				b.WriteByte('}')
				i = j - 1
				continue
			}
			b.WriteByte(p[i])
		}
		return b.String()
	}
	return p
}

// specRouteSet 解析规范 paths 为 "METHOD 路径" 集合（方法大写）。
func specRouteSet(t *testing.T, r *gin.Engine) map[string]bool {
	t.Helper()
	spec := parseSpec(t, r)
	paths, _ := spec["paths"].(map[string]any)
	set := map[string]bool{}
	for path, item := range paths {
		pi, ok := item.(map[string]any)
		if !ok {
			continue
		}
		for method := range pi {
			switch strings.ToUpper(method) {
			case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete,
				http.MethodPatch, http.MethodHead, http.MethodOptions:
				set[strings.ToUpper(method)+" "+path] = true
			}
		}
	}
	if len(set) == 0 {
		t.Fatal("规范中未解析到任何路由")
	}
	return set
}

// TestRoutesCoveredBySpec gin 实际路由表与规范一一对应（双向）：
// 新增端点未写文档、或文档写了未实现的路由，均在本测试失败。
func TestRoutesCoveredBySpec(t *testing.T) {
	r := newEngine(t)
	spec := specRouteSet(t, r)

	actual := map[string]bool{}
	for _, route := range r.Routes() {
		if route.Path == "" {
			continue
		}
		actual[route.Method+" "+ginPathToSpec(route.Path)] = true
	}

	var missing, extra []string
	for route := range actual {
		if !spec[route] {
			missing = append(missing, route)
		}
	}
	for route := range spec {
		if !actual[route] {
			extra = append(extra, route)
		}
	}
	if len(missing) > 0 {
		t.Errorf("以下端点已注册但未写入 openapi.yaml（漏文档）: %v", missing)
	}
	if len(extra) > 0 {
		t.Errorf("以下端点写入了 openapi.yaml 但未注册（幽灵文档）: %v", extra)
	}
	if len(actual) != len(spec) {
		t.Errorf("路由数不一致: 实际 %d, 文档 %d", len(actual), len(spec))
	}
	t.Logf("路由数一致: 实际 %d = 文档 %d", len(actual), len(spec))
}

// TestDocsRoutePublic 文档页与规范原文公开可达：不携带任何凭据也应 200，
// 且不落入 SPA/JSON 404 兜底。
func TestDocsRoutePublic(t *testing.T) {
	r := newEngine(t)
	for _, path := range []string{"/docs", "/docs/openapi.yaml"} {
		w := doGet(r, path)
		if w.Code != http.StatusOK {
			t.Errorf("GET %s 无凭据访问: status=%d, want 200", path, w.Code)
		}
	}
}
