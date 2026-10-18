// 本文件测试 proxy.go 里的错误分类函数 classifyError。
//
// 错误分类是健康状态的基础：同一个底层错误必须稳定地映射到
// 同一个 failureKind，/healthz 才能给出稳定的 last_error。
package gateway

import (
	// context 提供 DeadlineExceeded，它实现了 net.Error 且 Timeout()=true。
	"context"
	// json 提供 *json.SyntaxError。
	"encoding/json"
	// errors 创建普通错误、判断错误链。
	"errors"
	// fmt 演示用 %w 包装错误。
	"fmt"
	// net 提供 DNSError、OpError 等网络错误类型。
	"net"
	// testing 测试框架。
	"testing"
)

// TestClassifyError 使用表驱动测试覆盖每种 failureKind。
//
// 表驱动测试（table-driven test）是 Go 最常见的写法：
// 把输入和期望放进切片，再循环执行，新增用例只需加一行。
func TestClassifyError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want failureKind
	}{
		// 网关自己产生的哨兵错误：错误链里包含它就算命中。
		{"redirect", errRedirect, kindRedirect},
		// %w 把 errInvalidPayload 包进新错误；errors.Is 仍能识别出内层哨兵。
		{"wrapped invalid payload", fmt.Errorf("probe: %w", errInvalidPayload), kindInvalidPayload},
		// 后端返回 5xx。
		{"server status", serverStatusError(503), kindServerStatus},
		// context.DeadlineExceeded 满足 net.Error 接口，Timeout() 返回 true。
		{"timeout", context.DeadlineExceeded, kindTimeout},
		// *net.DNSError 表示主机名解析失败。
		{"dns", &net.DNSError{Err: "no such host", Name: "backend.invalid"}, kindDNS},
		// net.OpError 的底层错误文本包含 refused，归类为连接被拒绝。
		{"connection refused", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")}, kindConnectionRefused},
		// 其他网络操作错误保留操作名，例如 network_read。
		{"other network op", &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset")}, failureKind("network_read")},
		// JSON 语法错误。
		{"json syntax", &json.SyntaxError{Offset: 1}, kindInvalidJSON},
		// 未知错误回退到 Go 的类型名。
		{"unknown", errors.New("boom"), failureKind("*errors.errorString")},
	}
	for _, tc := range cases {
		// tc 是循环变量；Go 1.22 起每次迭代都会创建新变量，子测试里直接使用是安全的。
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyError(tc.err); got != tc.want {
				// %q 给字符串加引号，空字符串和空格差异会更容易看出来。
				t.Fatalf("classifyError() = %q, want %q", got, tc.want)
			}
		})
	}
}
