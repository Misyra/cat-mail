// addr.go 地址的 loopback 判定（防开放中继策略的判定核心，纯函数便于表驱动测试）。
package smtpserver

import (
	"net"
	"strings"
)

// IsLoopbackAddr 判定 host:port 形式的地址是否指向 loopback：
//   - host 为 "localhost"（不分大小写）→ true；
//   - host 是 IP 且 IsLoopback()（整个 127.0.0.0/8 与 ::1）→ true；
//   - 其余（0.0.0.0、::、普通域名、空串等）→ false。
//
// 兼容缺端口与 IPv6 方括号写法（"::1"、"[::1]:25"）；解析失败按非环回处理
// （从严，宁可多要求一次 AUTH）。供 CATMAIL_SMTP_ADDR 监听地址判定与对端
// RemoteAddr 判定共用（会话路径最终以对端判定为准，见 requireAuth）。
func IsLoopbackAddr(addr string) bool {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	return isLoopbackHost(strings.Trim(host, "[]"))
}

// isLoopbackHost 主机名/IP 字符串的 loopback 判定（IsLoopbackAddr 的底层，
// 接受裸 host，供已拆分出主机部分的调用方复用）。
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// remoteIPOf 从 net.Addr 提取 IP 主机部分（TCP RemoteAddr 为 "ip:port" 或
// "[ipv6]:port"）；非 IP 形态（unix socket 等）返回空串，requireAuth 从严要求 AUTH。
func remoteIPOf(addr net.Addr) string {
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return ""
	}
	return host
}
