// addr_test.go loopback 判定的表驱动测试（防开放中继策略的判定核心）。
package smtpserver

import "testing"

func TestIsLoopbackAddr(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		// 非 loopback：通配监听、外网 IP、域名、空串
		{":2525", false},
		{"0.0.0.0:25", false},
		{"[::]:25", false},
		{"192.168.1.10:2525", false},
		{"example.com:25", false},
		{"1.2.3.4", false},
		{"", false},
		{"localhosty:25", false}, // 精确匹配 localhost，前缀不算
		// loopback：整个 127.0.0.0/8、::1、localhost（不分大小写/方括号/缺端口）
		{"127.0.0.1:25", true},
		{"127.8.8.8:2525", true},
		{"[::1]:25", true},
		{"[::1]", true},
		{"::1", true},
		{"localhost:8080", true},
		{"LOCALHOST:25", true},
		{"localhost", true},
	}
	for _, c := range cases {
		if got := IsLoopbackAddr(c.in); got != c.want {
			t.Errorf("IsLoopbackAddr(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestRequireAuthDecision(t *testing.T) {
	// 按对端 IP 逐连接判定：环回免认证、外网必须、解析失败从严。
	s := &Server{}
	for _, c := range []struct {
		ip      string
		require bool
		force   bool
	}{
		{"127.0.0.1", false, false},
		{"::1", false, false},
		{"192.168.1.5", true, false},
		{"", true, false},        // 非 IP 形态（unix socket 等）从严
		{"10.0.0.1", true, true}, // forceAuth 注入恒要求（等价非环回对端）
		{"127.0.0.1", true, true},
	} {
		s.forceAuth = c.force
		if got := s.requireAuth(c.ip); got != c.require {
			t.Errorf("requireAuth(ip=%q, force=%v) = %v, want %v", c.ip, c.force, got, c.require)
		}
	}
}
