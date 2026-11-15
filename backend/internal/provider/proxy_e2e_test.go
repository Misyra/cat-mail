// proxy_e2e_test.go 适配器代理出站端到端测试：本地最小 SOCKS5 假代理（RFC 1928 子集，
// 无认证 CONNECT），验证渠道配置 socks5:// 后流量真实经代理转发（此前仅覆盖 BuildClient
// 构造、从不实际拨号，DialContext 接线错误无法被发现）。
package provider

import (
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"catmail/internal/model"
)

// fakeSocks5Server 最小 SOCKS5 服务端：握手（无认证 METHOD=0x00）→ 读 CONNECT 目标 →
// 记录目标地址并转发到真实目标，双向搬运直至断开。
type fakeSocks5Server struct {
	t *testing.T

	mu     sync.Mutex
	dstAll []string
}

func startFakeSocks5(t *testing.T, srv *fakeSocks5Server) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("socks5 listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go srv.serve(conn)
		}
	}()
	return ln.Addr().String()
}

func (s *fakeSocks5Server) dsts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.dstAll...)
}

func (s *fakeSocks5Server) serve(conn net.Conn) {
	defer conn.Close()
	// 方法协商：VER NMETHODS METHODS → VER=5 METHOD=0（无认证）
	head := make([]byte, 2)
	if _, err := io.ReadFull(conn, head); err != nil || head[0] != 5 {
		return
	}
	if head[1] > 0 {
		methods := make([]byte, head[1])
		if _, err := io.ReadFull(conn, methods); err != nil {
			return
		}
	}
	if _, err := conn.Write([]byte{5, 0}); err != nil {
		return
	}
	// CONNECT 请求：VER CMD RSV ATYP DST.ADDR DST.PORT
	req := make([]byte, 4)
	if _, err := io.ReadFull(conn, req); err != nil || req[1] != 1 { // 仅支持 CONNECT
		return
	}
	var host string
	switch req[3] {
	case 1: // IPv4
		b := make([]byte, 4)
		if _, err := io.ReadFull(conn, b); err != nil {
			return
		}
		host = net.IP(b).String()
	case 3: // 域名
		n := make([]byte, 1)
		if _, err := io.ReadFull(conn, n); err != nil {
			return
		}
		b := make([]byte, n[0])
		if _, err := io.ReadFull(conn, b); err != nil {
			return
		}
		host = string(b)
	default: // IPv6 不支持
		_, _ = conn.Write([]byte{5, 8, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	port := make([]byte, 2)
	if _, err := io.ReadFull(conn, port); err != nil {
		return
	}
	dst := net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(port))))
	s.mu.Lock()
	s.dstAll = append(s.dstAll, dst)
	s.mu.Unlock()
	upstream, err := net.Dial("tcp", dst)
	if err != nil {
		_, _ = conn.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0}) // 连接失败
		return
	}
	_, _ = conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0}) // 成功应答（绑定地址占位）
	go func() {
		_, _ = io.Copy(upstream, conn)
		_ = upstream.(*net.TCPConn).CloseWrite()
	}()
	_, _ = io.Copy(conn, upstream)
}

// TestAdapterSendThroughSocks5Proxy 适配器走 socks5 出站：请求经代理转发到假平台并成功，
// 代理记录到的 CONNECT 目标 == 平台地址。修复锁定：DialContext 接线（SA1019 修复点）
// 错误时流量不会经过代理，本测试直接失败。
func TestAdapterSendThroughSocks5Proxy(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"proxy-ok"}`))
	}))
	t.Cleanup(backend.Close)
	backendAddr := backend.Listener.Addr().String()

	srv := &fakeSocks5Server{t: t}
	proxyAddr := startFakeSocks5(t, srv)

	old := resendBaseURL
	resendBaseURL = "http://" + backendAddr
	t.Cleanup(func() { resendBaseURL = old })

	ch := &model.Channel{APIKey: "rk", ProxyURL: "socks5://" + proxyAddr}
	res, perr := Resend{}.Send(t.Context(), ch, testMessage())
	if perr != nil {
		t.Fatalf("expected success via socks5 proxy, got %+v", perr)
	}
	if res.ProviderMessageID != "proxy-ok" {
		t.Fatalf("provider id = %q, want proxy-ok", res.ProviderMessageID)
	}
	dsts := srv.dsts()
	if len(dsts) != 1 || dsts[0] != backendAddr {
		t.Fatalf("代理 CONNECT 目标 = %v, want [%s]（流量必须经代理）", dsts, backendAddr)
	}
}
