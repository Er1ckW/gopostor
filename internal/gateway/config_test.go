// 本文件测试配置解析、版本范围、路由边界、严格字段检查和配置校验。
//
// 测试通过相对路径 ../../config.example.json 读取仓库中提交的示例配置。
// go test 会把当前包目录（internal/gateway）作为工作目录，所以相对路径是稳定的。
package gateway

import (
	// os 读取环境变量和示例配置文件。
	"os"
	// path/filepath 跨平台拼接外部 Impostor 源码路径。
	"path/filepath"
	// regexp 从 C# 源码中提取版本列表。
	"regexp"
	// strings 做字符串替换和包含判断。
	"strings"
	// testing 测试框架。
	"testing"
)

// exampleConfigPath 指向仓库根目录的示例配置。
const exampleConfigPath = "../../config.example.json"

// findMatchingBackend 只返回"精确命中"的后端，不包含超范围回退。
//
// 这个方法定义在 _test.go 中，因此只在测试编译时存在，不会进入生产二进制。
// 用它来断言范围边界，可以避免把 below/above 回退误当成命中。
func (c *Config) findMatchingBackend(version int64) *Backend {
	if r := c.route(version); r.kind == routeExact {
		return r.backend
	}
	return nil
}

// TestReleaseRanges 校验示例配置的两段版本范围：边界值（含 +25 标志）必须命中，
// 范围内的值必须命中，范围外的值不能精确命中。
func TestReleaseRanges(t *testing.T) {
	c, err := loadConfig(exampleConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Backends) != 2 {
		t.Fatal("expected two backends")
	}
	// 配置解析时已按 min 升序排序，所以这里可以直接按名字顺序断言。
	for i, release := range []string{"1.9.3", "1.10.9"} {
		if c.Backends[i].Name != release {
			t.Fatalf("expected backend %s, got %s", release, c.Backends[i].Name)
		}
		// 每个后端的 min 和 max 都是闭区间边界，必须能精确命中。
		for _, bound := range []string{c.Backends[i].MinVersion, c.Backends[i].MaxVersion} {
			n, err := encodeVersion(bound)
			if err != nil {
				t.Fatal(err)
			}
			// 客户端可能带 +25 的 disable-server-authority 标志，两种都要能路由。
			for _, flag := range []int64{0, 25} {
				b := c.findMatchingBackend(n + flag)
				if b == nil || b.Name != release {
					t.Fatalf("wrong route for %s: %v", release, bound)
				}
			}
		}
	}
	// 范围内部（不在边界上）的版本也必须命中。
	for _, s := range []string{"2023.10.2", "2024.2.2", "2025.1.1"} {
		v, _ := encodeVersion(s)
		if c.findMatchingBackend(v) == nil {
			t.Fatal("range gap rejected")
		}
	}
	// 范围外（边界前后一天）的版本不能精确命中，只能走回退路由。
	for _, s := range []string{"2023.9.30", "2024.2.4", "2026.7.22"} {
		v, _ := encodeVersion(s)
		if c.findMatchingBackend(v) != nil {
			t.Fatal("outside range accepted")
		}
	}
}

// TestReleaseCompatibility 可选地用外部 Impostor 源码核对示例版本范围。
//
// 未设置 IMPOSTOR_SOURCE_ROOT 时 t.Skip 跳过，所以普通 go test 不依赖外部仓库。
// 设置后要求目录里同时有 Impostor-1.9.3 和 Impostor-1.10.9，
// 并解析 CompatibilityManager.cs 中的 GameVersion 列表逐一验证路由。
func TestReleaseCompatibility(t *testing.T) {
	sourceRoot := os.Getenv("IMPOSTOR_SOURCE_ROOT")
	if sourceRoot == "" {
		t.Skip("set IMPOSTOR_SOURCE_ROOT to the directory containing Impostor-1.9.3 and Impostor-1.10.9")
	}
	c, err := loadConfig(exampleConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	for i, release := range []string{"1.9.3", "1.10.9"} {
		source, err := os.ReadFile(filepath.Join(sourceRoot, "Impostor-"+release, "src", "Impostor.Server", "Net", "Manager", "CompatibilityManager.cs"))
		if err != nil {
			t.Fatal(err)
		}
		// 版本列表写在 "private readonly" 之前；只解析这一段，避免把后面无关的
		// GameVersion 构造也算进来。
		block := strings.Split(string(source), "private readonly")[0]
		versions := regexp.MustCompile(`new GameVersion\((\d+),\s*(\d+),\s*(\d+)\)`).FindAllStringSubmatch(block, -1)
		// 两个 Impostor 版本的兼容列表项数不同，写死数量可以在源码结构变化时报警。
		expected := 4
		if i == 1 {
			expected = 22
		}
		if len(versions) != expected {
			t.Fatal(len(versions))
		}
		for _, v := range versions {
			n, err := encodeVersion(v[1] + "." + v[2] + "." + v[3])
			if err != nil {
				t.Fatal(err)
			}
			for _, flag := range []int64{0, 25} {
				b := c.findMatchingBackend(n + flag)
				if b == nil || b.Name != release {
					t.Fatalf("wrong route: %v", v)
				}
			}
		}
	}
}

// TestOpenRangesAndFallback 覆盖三种情况：原始整数边界、开放上限、超范围回退。
func TestOpenRangesAndFallback(t *testing.T) {
	// minVersion 直接写整数 50593050；maxVersion 留空表示不设上限。
	c, err := parseConfig([]byte(`{"backends":[
		{"name":"a","minVersion":"50593050","maxVersion":"2024.2.3","publicIp":"203.0.113.1","publicPort":1,"httpEndpoint":"http://127.0.0.1:1"},
		{"name":"b","minVersion":"2024.6.1","maxVersion":"","publicIp":"203.0.113.2","publicPort":2,"httpEndpoint":"http://127.0.0.1:2"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Backends[0].min != 50593050 || c.Backends[1].max != maxVersion || c.Backends[1].rangeText() != "2024.6.1..any" {
		t.Fatal(c.Backends)
	}
	// 每个用例都额外 +25，验证路由前会去掉标志；normalized 应等于原始 v。
	for _, row := range []struct{ version, backend, kind string }{
		{"2021.6.30", "a", routeBelow},  // 低于所有范围 -> 最老后端
		{"2023.10.1", "a", routeExact},  // 精确命中 a
		{"2024.4.1", "a", routeBetween}, // 落在 a/b 之间 -> a
		{"2024.6.1", "b", routeExact},   // 精确命中 b
		{"2099.1.1", "b", routeExact},   // 开放上限，命中 b
	} {
		v, _ := encodeVersion(row.version)
		r := c.route(v + 25)
		if r.backend == nil || r.backend.Name != row.backend || r.kind != row.kind || r.normalized != v {
			t.Fatalf("%s: %+v", row.version, r)
		}
	}
	// 非法版本 0 返回 invalid，而不是 panic。
	if r := c.route(0); r.backend != nil || r.kind != routeInvalid {
		t.Fatal(r)
	}
	// 空配置也必须安全返回 invalid（route 里有 len 检查）。
	if r := (&Config{}).route(1); r.backend != nil || r.kind != routeInvalid {
		t.Fatal("empty config must not panic or route")
	}
}

// TestConfigRejectsUnknownFields 验证严格 JSON：
// 顶层拼写错误、后端未知字段、尾部多余 JSON 都必须报错。
func TestConfigRejectsUnknownFields(t *testing.T) {
	valid := `{"backends":[{"name":"a","minVersion":"2023.10.1","maxVersion":"2024.2.3","publicIp":"203.0.113.1","publicPort":22031,"httpEndpoint":"http://127.0.0.1:23001"}]}`
	if _, err := parseConfig([]byte(valid)); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		// 顶层字段拼写错误（backendTimeoutMillis 不是已定义字段）。
		strings.Replace(valid, `"backends"`, `"backendTimeoutMillis":1000,"backends"`, 1),
		// 后端对象里多了一个未知字段。
		strings.Replace(valid, `"name":"a"`, `"name":"a","unknown":1`, 1),
		// 合法 JSON 后面又跟了一个 JSON 对象。
		valid + `{}`,
	} {
		if _, err := parseConfig([]byte(body)); err == nil {
			t.Fatalf("accepted invalid config: %s", body)
		}
	}
}

// BenchmarkRoute 测量一次版本路由的耗时；路由是每个游戏请求都会走的热路径。
func BenchmarkRoute(b *testing.B) {
	c, err := loadConfig(exampleConfigPath)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if r := c.route(50663600); r.backend == nil {
			b.Fatal("no backend")
		}
	}
}

// TestConfigValidation 覆盖版本解析和配置校验的各种非法输入。
func TestConfigValidation(t *testing.T) {
	// 作为原始整数传入时：2026 太小（不是完整版本），-1 为负，
	// 99999999999 超出 int32 协议上限。
	for _, v := range []string{"2026", "-1", "99999999999"} {
		if _, err := parseVersionBound(v, 0); err == nil {
			t.Fatal(v)
		}
	}
	// 作为可读版本传入时：少一段、月份 13、revision 25（25 被 +25 标志占用）、
	// 年份大到溢出 int32，都必须拒绝。
	for _, v := range []string{"19.0", "2026.13.1", "2026.7.20.25", "99999999999999999999.1.1"} {
		if _, err := encodeVersion(v); err == nil {
			t.Fatal(v)
		}
	}
	// 127.0.0.1 按小端序编码应等于 16777343，验证字节序没有写反。
	c := testConfig(t, "http://localhost:23001")
	if c.Backends[1].encodedIP != 16777343 {
		t.Fatal("incorrect IPv4 byte order")
	}
	data, err := os.ReadFile(exampleConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	// 在示例配置上逐项制造非法配置：范围重叠、端口 0、IPv6、
	// httpEndpoint 带路径、后端重名。每一项都必须被拒绝。
	// 以及后端超时超过 Nginx 能等的上限、健康失败阈值为 0。
	for _, pair := range [][2]string{{`"2024.3.1"`, `"2024.2.3"`}, {`22031`, `0`}, {`"127.0.0.1"`, `"::1"`}, {`"http://127.0.0.1:23001"`, `"http://localhost/path"`}, {`"1.10.9"`, `"1.9.3"`}, {`"backendTimeoutMs": 5000`, `"backendTimeoutMs": 10001`}, {`"healthCheckIntervalSec": 30`, `"healthCheckIntervalSec": 30, "healthFailureThreshold": 0`}} {
		modified := strings.ReplaceAll(string(data), pair[0], pair[1])
		// 先确认替换真的匹配到了内容，否则测试会因为"没改到"而误判通过。
		if modified == string(data) {
			t.Fatalf("test replacement did not match: %v", pair)
		}
		if _, err := parseConfig([]byte(modified)); err == nil {
			t.Fatalf("accepted invalid config: %v", pair)
		}
	}
}
