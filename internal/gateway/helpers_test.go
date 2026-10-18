// 本文件测试代理头解析等纯函数。
//
// 这些函数不依赖网络或全局状态，所以测试可以直接构造输入并比较输出，
// 不需要启动 HTTP 服务器。
package gateway

import (
	// httptest 提供轻量的测试请求对象 httptest.NewRequest。
	"net/http/httptest"
	// testing 测试框架。
	"testing"
)

// TestForwardedClient 验证"只在直连对端是本机代理时才信任转发头"的安全规则。
//
// 如果无条件相信 X-Real-IP，任何玩家都能伪造 IP 来污染日志；
// 因此远程地址不是回环时，即使带了头也必须返回空字符串。
func TestForwardedClient(t *testing.T) {
	cases := []struct {
		name       string
		remoteAddr string
		realIP     string
		forwarded  string
		want       string
	}{
		// 本机 Nginx 转发，优先使用 X-Real-IP。
		{"loopback real ip", "127.0.0.1:5000", "203.0.113.9", "", "203.0.113.9"},
		// 没有 X-Real-IP 时取 X-Forwarded-For 的第一段。
		{"loopback forwarded for", "127.0.0.1:5000", "", "203.0.113.9, 10.0.0.1", "203.0.113.9"},
		// 直连对端不是回环地址：不信任任何转发头，防止伪造。
		{"direct client cannot spoof", "198.51.100.7:5000", "203.0.113.9", "", ""},
		// 本机连接但没带头：没有可用的真实地址。
		{"loopback without headers", "127.0.0.1:5000", "", "", ""},
		// RemoteAddr 格式非法时安全返回空。
		{"invalid remote address", "not-an-address", "203.0.113.9", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// NewRequest 创建一个不经过网络的标准 *http.Request。
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = tc.remoteAddr
			if tc.realIP != "" {
				r.Header.Set("X-Real-IP", tc.realIP)
			}
			if tc.forwarded != "" {
				r.Header.Set("X-Forwarded-For", tc.forwarded)
			}
			if got := forwardedClient(r); got != tc.want {
				t.Fatalf("forwardedClient() = %q, want %q", got, tc.want)
			}
		})
	}
}
