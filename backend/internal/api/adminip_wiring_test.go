// adminip_wiring_test.go E5 白名单真实接线回归（Register + WithAdminAllowlist）：
// 配置 CIDR 后 login 白名单外 403、白名单内正常登录；管理接口先白名单后会话；
// 未配置全放行（默认现状不变）；/api/health、/v1/send、/emails 豁免面不变。
// 与 middleware_adminip_test.go 的手工路由形态互补，本文件走真实 Register 装配。
package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"catmail/internal/config"
	"catmail/internal/store"
)

// newWiredEnv 走真实 Register 接线（Option 注入白名单），返回引擎与库句柄。
func newWiredEnv(t *testing.T, cidrEnv string) (*gin.Engine, *gorm.DB) {
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
	cidrs, err := config.ParseAdminCIDRs(cidrEnv)
	if err != nil {
		t.Fatalf("parse cidrs: %v", err)
	}
	r := gin.New()
	r.Use(gin.Recovery())
	Register(r, db, WithAdminAllowlist(cidrs))
	return r, db
}

// loginFromIP 从指定来源 IP 登录并返回会话 cookie 值（失败即 Fatal）。
func loginFromIP(t *testing.T, r *gin.Engine, ip, username, password string) string {
	t.Helper()
	w := reqFromIP(r, "POST", "/api/auth/login", ip,
		`{"username":"`+username+`","password":"`+password+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("login from %s: status=%d body=%s", ip, w.Code, w.Body.String())
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookieName && c.Value != "" {
			return c.Value
		}
	}
	t.Fatalf("login from %s: session cookie missing", ip)
	return ""
}

// reqWithCookieAndIP 携带会话 Cookie 与指定来源 IP 发起请求。
func reqWithCookieAndIP(r *gin.Engine, method, path, ip, cookie string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = ip
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookie})
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestAdminAllowlistWiringViaRegister 真实接线回归：配 CIDR 后 login 白名单外
// 403、白名单内正常登录并携带会话访问管理接口；白名单先于会话校验。
func TestAdminAllowlistWiringViaRegister(t *testing.T) {
	r, _ := newWiredEnv(t, "192.168.1.0/24,10.0.0.5/32")
	denied, allowed := "203.0.113.7:1000", "192.168.1.10:1000"

	// login：白名单外未登录即 403（防外网爆破），错误体符契约
	w := reqFromIP(r, "POST", "/api/auth/login", denied,
		`{"username":"admin","password":"admin123"}`)
	if w.Code != http.StatusForbidden || errType(t, w.Body.String()) != TypeForbidden {
		t.Fatalf("白名单外 login: status = %d, want 403 forbidden (body=%s)", w.Code, w.Body.String())
	}

	// login：白名单内正常登录，会话可访问管理接口
	cookie := loginFromIP(t, r, allowed, "admin", "admin123")
	if w := reqWithCookieAndIP(r, "GET", "/api/users", allowed, cookie); w.Code != http.StatusOK {
		t.Fatalf("白名单内已登录 /api/users: status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}

	// 管理组：白名单外 403（先于会话校验）；白名单内未登录 401（白名单已放行）
	if w := reqWithCookieAndIP(r, "GET", "/api/users", denied, ""); w.Code != http.StatusForbidden ||
		errType(t, w.Body.String()) != TypeForbidden {
		t.Fatalf("白名单外 /api/users: status = %d, want 403 forbidden (body=%s)", w.Code, w.Body.String())
	}
	if w := reqWithCookieAndIP(r, "GET", "/api/users", allowed, ""); w.Code != http.StatusUnauthorized ||
		errType(t, w.Body.String()) != TypeUnauthorized {
		t.Fatalf("白名单内未登录 /api/users: status = %d, want 401 unauthorized (body=%s)", w.Code, w.Body.String())
	}

	// 豁免面：/api/health 任意来源 200；/v1/send、/emails 白名单外可达
	// （无 key 401，证明请求穿过白名单到达 handler，而非被 403 拦截）
	if w := reqFromIP(r, "GET", "/api/health", denied, ""); w.Code != http.StatusOK {
		t.Fatalf("白名单外 /api/health: status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	for _, path := range []string{"/v1/send", "/emails"} {
		w := reqFromIP(r, "POST", path, denied, `{}`)
		if w.Code != http.StatusUnauthorized || errType(t, w.Body.String()) != TypeUnauthorized {
			t.Fatalf("白名单外 POST %s: status = %d, want 401 unauthorized（白名单不应拦截）(body=%s)",
				path, w.Code, w.Body.String())
		}
	}

	// /metrics 属管理面（B4）：随白名单受限，白名单外 403、白名单内免登录可抓取
	if w := reqFromIP(r, "GET", "/metrics", denied, ""); w.Code != http.StatusForbidden ||
		errType(t, w.Body.String()) != TypeForbidden {
		t.Fatalf("白名单外 /metrics: status = %d, want 403 forbidden (body=%s)", w.Code, w.Body.String())
	}
	if w := reqFromIP(r, "GET", "/metrics", allowed, ""); w.Code != http.StatusOK {
		t.Fatalf("白名单内 /metrics: status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
}

// TestAdminAllowlistUnconfiguredDefault 未配置 CIDR 全放行（默认现状不变）：
// 任意来源可登录、可访问管理接口（未登录则照旧 401）、可抓取 /metrics。
func TestAdminAllowlistUnconfiguredDefault(t *testing.T) {
	r, _ := newWiredEnv(t, "")
	// 任意来源登录成功（reqFromIP body 非 200 即 Fatal）
	loginFromIP(t, r, "203.0.113.9:1000", "admin", "admin123")
	if w := reqFromIP(r, "GET", "/metrics", "198.51.100.7:1000", ""); w.Code != http.StatusOK {
		t.Fatalf("未配置白名单 /metrics: status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
}
