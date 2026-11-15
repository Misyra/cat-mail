// middleware_adminip_test.go 管理后台 IP 白名单中间件测试（E5）：
// 中间件单元行为（命中/未命中/IPv6/多 CIDR/未配置全放行）+ 接线形态下的
// 生效范围（login 拦截，/api/health 与 /v1/send、/emails 不受影响）。
// 来源 IP 经 httptest 请求的 RemoteAddr 伪造（不设置 XFF，走 gin ClientIP 默认语义）。
package api

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"catmail/internal/config"
	"catmail/internal/store"
)

// reqFromIP 以指定 RemoteAddr 发起请求（body 非空时按 JSON 处理）。
func reqFromIP(r *gin.Engine, method, path, remoteAddr, body string) *httptest.ResponseRecorder {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.RemoteAddr = remoteAddr
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// errType 取统一错误体的 error.type，缺失即失败。
func errType(t *testing.T, body string) string {
	t.Helper()
	m := decode(t, body)
	e, ok := m["error"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺少统一错误体: %s", body)
	}
	typ, _ := e["type"].(string)
	return typ
}

// TestAdminIPAllowlistMiddleware 中间件单元行为：白名单内放行、白名单外
// 403 统一错误体、IPv6 CIDR、多 CIDR 任一命中、未配置（nil/空）全放行。
func TestAdminIPAllowlistMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)
	parse := func(t *testing.T, raw string) []*net.IPNet {
		t.Helper()
		cidrs, err := config.ParseAdminCIDRs(raw)
		if err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		return cidrs
	}

	cases := []struct {
		name   string
		cidrs  string // 空串 = 未配置
		remote string // 请求 RemoteAddr（httptest 形式 host:port，IPv6 加方括号）
		want   int
	}{
		{"白名单内 IPv4", "10.0.0.0/8", "10.1.2.3:443", http.StatusOK},
		{"白名单外 IPv4", "10.0.0.0/8", "11.1.2.3:443", http.StatusForbidden},
		{"/24 网段内", "192.168.1.0/24", "192.168.1.77:1000", http.StatusOK},
		{"/24 网段外", "192.168.1.0/24", "192.168.2.77:1000", http.StatusForbidden},
		{"IPv6 命中", "2001:db8::/32", "[2001:db8::1]:1234", http.StatusOK},
		{"IPv6 未命中", "2001:db8::/32", "[2001:db9::1]:1234", http.StatusForbidden},
		{"多 CIDR 命中第一条", "192.168.1.0/24,10.0.0.5/32", "192.168.1.5:9", http.StatusOK},
		{"多 CIDR 命中第二条", "192.168.1.0/24,10.0.0.5/32", "10.0.0.5:9", http.StatusOK},
		{"多 CIDR 全未命中", "192.168.1.0/24,10.0.0.5/32", "10.0.0.6:9", http.StatusForbidden},
		{"未配置 nil 全放行", "", "203.0.113.9:5", http.StatusOK},
		{"未配置（空白串）全放行", "  ,  ", "203.0.113.9:5", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probe := gin.New()
			probe.GET("/probe", AdminIPAllowlist(parse(t, tc.cidrs)), func(c *gin.Context) {
				c.String(http.StatusOK, "ok")
			})
			w := reqFromIP(probe, "GET", "/probe", tc.remote, "")
			if w.Code != tc.want {
				t.Fatalf("remote %s: status = %d, want %d (body=%s)", tc.remote, w.Code, tc.want, w.Body.String())
			}
			if tc.want == http.StatusForbidden {
				if typ := errType(t, w.Body.String()); typ != TypeForbidden {
					t.Errorf("error.type = %q, want %q", typ, TypeForbidden)
				}
				if msg := decode(t, w.Body.String())["error"].(map[string]any)["message"]; msg != "来源 IP 不在管理白名单内" {
					t.Errorf("error.message = %q, 不符契约文案", msg)
				}
			}
		})
	}
}

// newAllowlistEnv 按 E5 接线形态构造真实路由（与 Register 相同的 handler，
// 白名单中间件只挂在 login 与 /api/* 管理组，health/v1/send/emails 豁免）。
func newAllowlistEnv(t *testing.T, cidrEnv string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	if err := store.EnsureSeed(db, "admin", "admin123"); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	allowed, err := config.ParseAdminCIDRs(cidrEnv)
	if err != nil {
		t.Fatalf("parse cidrs: %v", err)
	}
	mw := AdminIPAllowlist(allowed)
	s := &Server{db: db, limiter: newLoginLimiter()}
	r := gin.New()
	r.GET("/api/health", s.health)
	r.POST("/v1/send", bodyLimit(maxSendBodyBytes), s.send)
	r.POST("/emails", bodyLimit(maxSendBodyBytes), s.send)
	// 接线点（E5）：白名单套用在 login 路由与管理路由组
	r.POST("/api/auth/login", mw, bodyLimit(maxAdminBodyBytes), s.login)
	api := r.Group("/api", mw, s.requireSession, bodyLimit(maxAdminBodyBytes))
	api.GET("/users", s.listUsers)
	return r
}

// TestAdminIPAllowlistScope 生效范围：login 未登录也拦；管理组先过白名单再过
// 会话；/api/health 与 /v1/send、/emails 任意来源可达（探针/调用方豁免）。
func TestAdminIPAllowlistScope(t *testing.T) {
	r := newAllowlistEnv(t, "192.168.1.0/24,10.0.0.5/32")
	denied, allowed := "203.0.113.7:1000", "192.168.1.10:1000"
	loginBody := `{"username":"admin","password":"admin123"}`

	// login：白名单外来源未登录即 403（防外网爆破），错误体符契约
	w := reqFromIP(r, "POST", "/api/auth/login", denied, loginBody)
	if w.Code != http.StatusForbidden {
		t.Fatalf("白名单外 login: status = %d, want 403 (body=%s)", w.Code, w.Body.String())
	}
	if typ := errType(t, w.Body.String()); typ != TypeForbidden {
		t.Errorf("login error.type = %q, want %q", typ, TypeForbidden)
	}
	if msg := decode(t, w.Body.String())["error"].(map[string]any)["message"]; msg != "来源 IP 不在管理白名单内" {
		t.Errorf("login error.message = %q, 不符契约文案", msg)
	}

	// login：任一白名单 CIDR 命中即可正常登录（第二个 CIDR /32 命中）
	for _, ip := range []string{allowed, "10.0.0.5:1000"} {
		w := reqFromIP(r, "POST", "/api/auth/login", ip, loginBody)
		if w.Code != http.StatusOK {
			t.Fatalf("白名单内 login from %s: status = %d, want 200 (body=%s)", ip, w.Code, w.Body.String())
		}
	}

	// 管理组：白名单外 403（先于会话校验）；白名单内但未登录 401（白名单已放行）
	w = reqFromIP(r, "GET", "/api/users", denied, "")
	if w.Code != http.StatusForbidden || errType(t, w.Body.String()) != TypeForbidden {
		t.Fatalf("白名单外 /api/users: status = %d, want 403 forbidden (body=%s)", w.Code, w.Body.String())
	}
	w = reqFromIP(r, "GET", "/api/users", allowed, "")
	if w.Code != http.StatusUnauthorized || errType(t, w.Body.String()) != TypeUnauthorized {
		t.Fatalf("白名单内未登录 /api/users: status = %d, want 401 unauthorized (body=%s)", w.Code, w.Body.String())
	}

	// 豁免面：/api/health 任意来源 200；/v1/send、/emails 白名单外可达（无 key 401，
	// 证明请求穿过白名单到达 handler，而非被 403 拦截）
	w = reqFromIP(r, "GET", "/api/health", denied, "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Fatalf("白名单外 /api/health: status = %d, want 200 ok (body=%s)", w.Code, w.Body.String())
	}
	for _, path := range []string{"/v1/send", "/emails"} {
		w := reqFromIP(r, "POST", path, denied, `{}`)
		if w.Code != http.StatusUnauthorized || errType(t, w.Body.String()) != TypeUnauthorized {
			t.Fatalf("白名单外 POST %s: status = %d, want 401 unauthorized（白名单不应拦截）(body=%s)",
				path, w.Code, w.Body.String())
		}
	}
}
