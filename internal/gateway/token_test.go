// 本文件测试 token 的线上格式、解析健壮性和性能。
//
// Go 测试的基础约定：
//   - 文件名必须以 _test.go 结尾；
//   - 测试函数名以 Test 开头，参数是 *testing.T；
//   - 基准函数名以 Benchmark 开头，参数是 *testing.B；
//   - 模糊测试函数名以 Fuzz 开头，参数是 *testing.F；
//   - 执行 go test ./... 时，Go 自动发现并运行这些函数，不需要额外注册。
package gateway

import (
	// base64 用于把 token 解码回 JSON，验证线上格式。
	"encoding/base64"
	// testing 提供 T、B、F 等测试类型。
	"testing"
)

// TestCreateTokenWireFormat 把 token 的 JSON 载荷逐字节固定下来。
//
// 它故意不经过 token/tokenContent 结构体反序列化再比较，而是直接比较原始字符串：
// 这样即使有人改了结构体字段顺序或标签，测试也会立刻失败。
// 对客户端来说 token 是不透明的 Base64，但真正的兼容性取决于里面的 JSON 形状。
func TestCreateTokenWireFormat(t *testing.T) {
	encoded := createToken("player", 50663625)
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		// t.Fatal 记录错误并立即终止当前测试函数。
		t.Fatal(err)
	}
	// 反引号字符串可以包含双引号，不需要转义。
	want := `{"Content":{"Puid":"player","ClientVersion":50663625,"ExpiresAt":"2012-12-21T00:00:00"},"Hash":"impostor_was_here"}`
	if string(data) != want {
		// t.Fatalf 支持格式化参数，%s 输出实际值，便于定位差异。
		t.Fatalf("token payload = %s; want %s", data, want)
	}
}

// FuzzParseTokenVersion 是 Go 的模糊测试目标。
//
// f.Add 提供初始种子（空头、畸形头、合法 token）；go test 会在此基础上
// 自动变异出大量随机输入。测试断言是一个"不变量"：
//
//	只要函数返回 err == nil，解析出的版本就必须在 1..maxVersion 之间。
//
// 模糊测试特别适合解析器：它不需要你手写所有畸形输入。
func FuzzParseTokenVersion(f *testing.F) {
	f.Add("")
	f.Add("Bearer !!!")
	f.Add("Bearer " + createToken("player", 50595550))
	f.Fuzz(func(t *testing.T, header string) {
		version, err := parseTokenVersion(header)
		if err == nil && (version < 1 || version > maxVersion) {
			t.Fatalf("accepted invalid version %d", version)
		}
	})
}

// BenchmarkParseTokenVersion 测量解析一个合法 token 的耗时和内存分配。
//
// b.Loop 由测试框架控制迭代次数：它会不断增加迭代次数，直到结果稳定。
// 只有 go test -bench=. 才会运行基准测试，普通 go test 会跳过。
func BenchmarkParseTokenVersion(b *testing.B) {
	header := "Bearer " + createToken("player", 50595550)
	// ReportAllocs 让输出包含每次操作的分配次数和字节数。
	b.ReportAllocs()
	for b.Loop() {
		if _, err := parseTokenVersion(header); err != nil {
			b.Fatal(err)
		}
	}
}
