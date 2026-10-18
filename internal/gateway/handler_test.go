// 本文件是网关的端到端 HTTP 测试：从 /api/user 拿 token，再带 token 请求
// /api/games 系列接口，验证路由、地址改写、错误清理和健康检查。
//
// 测试使用 net/http/httptest 在本地启动模拟后端，不依赖真实 Impostor。
// 整个测试文件与生产代码同属 gateway 包，因此可以直接调用未导出函数。
package gateway

import (
	// context 用于健康检查的启动调用。
	"context"
	// base64 解码 token、构造畸形 token。
	"encoding/base64"
	// json 解析 token 内容。
	"encoding/json"
	// fmt 生成测试配置和请求体。
	"fmt"
	// io 向模拟后端写响应。
	"io"
	// http 构造模拟后端的 handler。
	"net/http"
	// httptest 提供模拟服务器、请求和响应记录器。
	"net/http/httptest"
	// strings 判断响应内容。
	"strings"
	// testing 测试框架。
	"testing"
	// time 控制模拟后端延迟和超时配置。
	"time"
)

// newHandler 是测试用的"完整网关"入口：包含访问日志和健康检查，
// 与 main -> Run -> newGateway().handler() 的组装方式一致。
func newHandler(c Config) http.Handler { return newGateway(c).handler() }

// testConfig 构造一个包含两个后端的配置：
//   - old：2023.10.1..2024.2.3，公网地址 203.0.113.10:22031；
//   - new：2024.3.1..2026.7.21，公网地址 127.0.0.1:22032。
//
// 203.0.113.0/24 是 RFC 5737 保留的文档示例网段，用它做测试不会误连真实主机。
// fmt.Appendf(nil, ...) 返回 []byte，%q 会给 endpoint 加上合法的 JSON 引号。
func testConfig(t *testing.T, endpoint string) Config {
	t.Helper()
	c, err := parseConfig(fmt.Appendf(nil, `{"backends":[{"name":"old","minVersion":"2023.10.1","maxVersion":"2024.2.3","publicIp":"203.0.113.10","publicPort":22031,"httpEndpoint":%q},{"name":"new","minVersion":"2024.3.1","maxVersion":"2026.7.21","publicIp":"127.0.0.1","publicPort":22032,"httpEndpoint":%q}]}`, endpoint, endpoint))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// hostBackend 模拟 Impostor 对 PUT /api/games 的响应，返回给定的 Ip/Port。
// Ip 是已经按小端序编码好的 uint32（例如 127.0.0.1 = 16777343）。
func hostBackend(t *testing.T, ip uint32, port int) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `{"Ip":%d,"Port":%d}`, ip, port)
	}))
	// 测试结束时自动关闭服务器，避免端口泄漏。
	t.Cleanup(s.Close)
	return s
}

// call 构造一个请求并直接调用 handler，返回记录下来的响应。
//
// httptest.NewRequest 创建请求对象；httptest.NewRecorder 实现 ResponseWriter
// 并把响应保存在内存里。整个过程不经过真实网络。
func call(h http.Handler, method, path, body, auth string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", auth)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// requestToken 调用 POST /api/user 获取 token，并校验响应是 200 + text/plain。
func requestToken(t *testing.T, h http.Handler, body string) string {
	t.Helper()
	w := call(h, "POST", "/api/user", body, "")
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
		t.Fatal(w.Code, w.Body.String())
	}
	return w.Body.String()
}

// TestTokenAndRouting 覆盖完整流程：登录拿 token -> 解析 token -> 创建房间，
// 并验证两个后端的四个版本边界以及 +25 标志。
func TestTokenAndRouting(t *testing.T) {
	// 两个后端都指向同一个模拟 host 后端；但网关返回的 Port 应来自配置。
	c := testConfig(t, hostBackend(t, 16777343, 22032).URL)
	h := newHandler(c)
	for _, row := range []struct {
		version string
		port    int
	}{{"2023.10.1", 22031}, {"2024.2.3", 22031}, {"2024.3.1", 22032}, {"2026.7.21", 22032}} {
		v, _ := encodeVersion(row.version)
		// 每个边界都测试带/不带 +25 两种情况。
		for _, flag := range []int64{0, 25} {
			tokenText := requestToken(t, h, fmt.Sprintf(`{"Puid":"用户","Username":"Player","Language":0,"ClientVersion":%d}`, v+flag))
			data, err := base64.StdEncoding.DecodeString(tokenText)
			if err != nil {
				t.Fatal(err)
			}
			// 用生产代码里的 token 结构体解析，验证字段值。
			var tok token
			if err = json.Unmarshal(data, &tok); err != nil {
				t.Fatal(err)
			}
			// PUID 原样保留（包括中文），版本保留原始值（含 +25），
			// ExpiresAt/Hash 使用 Impostor 的固定值。
			if tok.Content.PUID != "用户" || tok.Content.ClientVersion != v+flag || tok.Content.ExpiresAt != "2012-12-21T00:00:00" || tok.Hash != "impostor_was_here" {
				t.Fatal(tok)
			}
			// 创建房间：返回配置中的公网端口，而不是 host 后端自报的 22032。
			host := call(h, "PUT", "/api/games", "", "Bearer "+tokenText)
			if host.Code != 200 || !strings.Contains(host.Body.String(), fmt.Sprintf(`"Port":%d`, row.port)) {
				t.Fatal(host.Body.String())
			}
		}
	}
}

// TestLenientTokenRequest 验证 /api/user 的宽松兼容行为：
// 缺失 Puid/Username/Language、字段名大小写不同、数字写成字符串都应成功；
// 缺少 ClientVersion 或类型错误则必须 400。
func TestLenientTokenRequest(t *testing.T) {
	h := newHandler(testConfig(t, hostBackend(t, 16777343, 22032).URL))
	for _, body := range []string{
		`{"Puid":"x","ClientVersion":50663600}`,                           // 只有必需字段
		`{"ClientVersion":50663600}`,                                      // 连 Puid 也没有
		`{"Puid":"","Username":"","Language":0,"ClientVersion":50663600}`, // 空字符串
		`{"puid":"x","clientversion":"50663600","language":"3"}`,          // 小写键 + 字符串数字
	} {
		tokenText := requestToken(t, h, body)
		if w := call(h, "PUT", "/api/games", "", "Bearer "+tokenText); w.Code != 200 || !strings.Contains(w.Body.String(), `"Port":22032`) {
			t.Fatal(body, w.Code, w.Body.String())
		}
	}
	for _, body := range []string{`{"Puid":"x"}`, `{"ClientVersion":"abc"}`, `{"ClientVersion":0}`, `{"Puid":1,"ClientVersion":50663600}`} {
		if w := call(h, "POST", "/api/user", body, ""); w.Code != 400 {
			t.Fatal(body, w.Code)
		}
	}
}

// TestOutOfRangeVersions 验证超范围版本的回退路由，以及 rejectUnknownVersions 的行为。
func TestOutOfRangeVersions(t *testing.T) {
	// old 后端自报一个与配置不同的地址（184120523），
	// 但网关仍应返回配置里的 203.0.113.10:22031。
	old := hostBackend(t, 184120523, 22031)
	c := testConfig(t, old.URL)
	c.Backends[1].HTTPEndpoint = hostBackend(t, 16777343, 22032).URL
	h := newHandler(c)
	for _, row := range []struct {
		version string
		port    int
		// 三个用例：低于所有范围 -> 最老后端；两段范围之间 -> 前一个后端；
		// 高于所有范围 -> 最新后端。
	}{{"2022.12.14", 22031}, {"2024.2.20", 22031}, {"2026.10.1", 22032}} {
		v, _ := encodeVersion(row.version)
		tokenText := requestToken(t, h, fmt.Sprintf(`{"Puid":"x","Username":"x","Language":0,"ClientVersion":%d}`, v))
		if w := call(h, "PUT", "/api/games", "", "Bearer "+tokenText); w.Code != 200 || !strings.Contains(w.Body.String(), fmt.Sprintf(`"Port":%d`, row.port)) {
			t.Fatal(row.version, w.Code, w.Body.String())
		}
	}
	// 开启拒绝未知版本后：/api/user 仍签发 token，但游戏请求返回 404 Reason 5。
	c.RejectUnknownVersions = true
	h = newHandler(c)
	v, _ := encodeVersion("2026.10.1")
	tokenText := requestToken(t, h, fmt.Sprintf(`{"Puid":"x","ClientVersion":%d}`, v))
	if w := call(h, "PUT", "/api/games", "", "Bearer "+tokenText); w.Code != 404 || !strings.Contains(w.Body.String(), `"Reason":5`) {
		t.Fatal(w.Code, w.Body.String())
	}
	// 单房间查询的错误体还必须带 "Game":null。
	if w := call(h, "GET", "/api/games/123", "", "Bearer "+tokenText); w.Code != 404 || !strings.Contains(w.Body.String(), `"Game":null`) {
		t.Fatal(w.Code, w.Body.String())
	}
}

// TestCreateProbe 验证创建房间前的后端探测：
// 后端不可用时立即 502；关闭探测后即使后端 down 也仍返回配置地址；
// 后端返回 500 视为拒绝创建；后端自报地址不一致不影响返回配置地址。
func TestCreateProbe(t *testing.T) {
	auth := "Bearer " + createToken("test", 50663600)
	// 127.0.0.1:1 没有服务，连接会立即被拒绝。
	down := testConfig(t, "http://127.0.0.1:1")
	if w := call(newHandler(down), "PUT", "/api/games", "", auth); w.Code != 502 || !strings.Contains(w.Body.String(), `"Reason":17`) {
		t.Fatal(w.Code, w.Body.String())
	}
	// 关闭探测后不再访问后端，直接返回配置地址。
	down.ProbeBackendOnCreate = false
	if w := call(newHandler(down), "PUT", "/api/games", "", auth); w.Code != 200 || !strings.Contains(w.Body.String(), `"Port":22032`) {
		t.Fatal(w.Code, w.Body.String())
	}
	// 后端可达但返回 500：探测失败，仍应 502。
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer broken.Close()
	if w := call(newHandler(testConfig(t, broken.URL)), "PUT", "/api/games", "", auth); w.Code != 502 {
		t.Fatal(w.Code)
	}
	// 后端返回 Impostor 格式的错误原因：和查询接口一样透传 Reason，丢弃 Message。
	rejected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = io.WriteString(w, `{"Errors":[{"Reason":5,"Message":"Exception at Impostor.Server"}]}`)
	}))
	defer rejected.Close()
	if w := call(newHandler(testConfig(t, rejected.URL)), "PUT", "/api/games", "", auth); w.Code != 400 || w.Body.String() != `{"Errors":[{"Reason":5}]}`+"\n" {
		t.Fatal(w.Code, w.Body.String())
	}
	// 后端自报 0.0.0.1:1，与配置不一致；网关只记录警告，仍返回配置地址。
	mismatch := testConfig(t, hostBackend(t, 1, 1).URL)
	if w := call(newHandler(mismatch), "PUT", "/api/games", "", auth); w.Code != 200 || !strings.Contains(w.Body.String(), `"Ip":16777343,"Port":22032`) {
		t.Fatal(w.Code, w.Body.String())
	}
}

// TestProxy 覆盖查询类接口的转发、地址改写和错误清理。
//
// 模拟后端根据路径返回不同响应，用来触发网关的各个分支。
func TestProxy(t *testing.T) {
	// 预先编码好的过滤查询参数，用于验证查询参数原样转发。
	const query = "?filter=%7B%22FilterSets%22%3A%5B%5D%7D"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 网关必须转发 Authorization 和 Accept，并保留原查询参数。
		if r.Header.Get("Authorization") != "Bearer "+createToken("test", 50663600) || r.Header.Get("Accept") != "application/json" {
			t.Error("forwarded headers")
		}
		switch r.URL.Path {
		case "/api/games/filtered":
			if r.URL.RawQuery != strings.TrimPrefix(query, "?") {
				t.Error("query changed")
			}
			_, _ = io.WriteString(w, `{"games":[{"IP":0,"Port":1,"Options":"keep"}],"metadata":{"matchingGamesCount":1}}`)
		case "/api/games":
			if r.Method == "POST" {
				// 旧版加入房间：gameId 查询参数必须原样保留（包括负号）。
				if r.URL.Query().Get("gameId") != "-123" {
					t.Error("game ID changed")
				}
				_, _ = io.WriteString(w, `{"Ip":0,"Port":1}`)
			} else {
				_, _ = io.WriteString(w, `[{"IP":0,"Port":1}]`)
			}
		case "/api/games/-404":
			// 房间不存在：标准 Impostor 错误，只有 Reason。
			w.WriteHeader(404)
			_, _ = io.WriteString(w, `{"Errors":[{"Reason":3}]}`)
		case "/api/games/-450":
			// 后端把异常文本塞进 Message；网关必须丢弃它。
			w.WriteHeader(400)
			_, _ = io.WriteString(w, `{"Errors":[{"Reason":113,"Message":"Unknown exception caught in filterSystem.Exception: at Impostor.Server"}]}`)
		case "/api/games/-500":
			// 非 Impostor 格式的错误响应。
			w.WriteHeader(503)
			_, _ = io.WriteString(w, "broken")
		case "/api/games/-600":
			// 2xx 但内容不是 JSON。
			_, _ = io.WriteString(w, "invalid JSON")
		case "/api/games/-700":
			// 延迟 60ms，用于测试 backendTimeoutMs=10 时的超时。
			time.Sleep(60 * time.Millisecond)
			_, _ = io.WriteString(w, `{}`)
		case "/api/games/-800":
			// 3xx 重定向；网关的 client 配置为拒绝跟随。
			http.Redirect(w, r, "/api/games", 302)
		case "/api/filters", "/api/filtertags":
			_, _ = io.WriteString(w, `{"filters":["keep"]}`)
		default:
			// 单房间查询：Large 是超过 2^53 的整数，用于验证 UseNumber 不丢精度。
			_, _ = io.WriteString(w, `{"Errors":null,"Game":{"IP":0,"Port":1,"GameId":-123,"Options":"keep","Large":9007199254740993}}`)
		}
	}))
	defer upstream.Close()
	c := testConfig(t, upstream.URL)
	h := newHandler(c)
	auth := "Bearer " + createToken("test", 50663600)
	for _, row := range []struct {
		method, path string
		status       int
		contains     string
	}{
		{"GET", "/api/games", 200, `"IP":16777343`},                     // 列表地址被改写
		{"POST", "/api/games?gameId=-123", 200, `"Ip":16777343`},        // 旧版加入地址被改写
		{"GET", "/api/games/filtered" + query, 200, `"Options":"keep"`}, // 过滤列表保留其他字段
		{"GET", "/api/games/-123", 200, `"Large":9007199254740993`},     // 大整数精度
		{"GET", "/api/games/-404", 404, `{"Errors":[{"Reason":3}],"Game":null}`},
		{"GET", "/api/games/-450", 400, `{"Errors":[{"Reason":113}],"Game":null}`},
		{"GET", "/api/games/-500", 503, `"Reason":17`},
		{"GET", "/api/games/-600", 502, "unavailable"}, // 无效 JSON -> 502
		{"GET", "/api/games/-800", 502, "unavailable"}, // 重定向 -> 502
		{"GET", "/api/filters", 200, `"keep"`},
		{"GET", "/api/filtertags", 200, `"keep"`},
	} {
		t.Run(row.path, func(t *testing.T) {
			w := call(h, row.method, row.path, "", auth)
			// 除了状态码和内容，还要确保后端异常文本没有泄漏给客户端。
			if w.Code != row.status || !strings.Contains(w.Body.String(), row.contains) || strings.Contains(w.Body.String(), "Exception") {
				t.Fatal(w.Code, w.Body.String())
			}
		})
	}
	// 把超时改成 10ms，再请求会睡 60ms 的接口，验证超时返回 502。
	c.BackendTimeoutMS = 10
	if w := call(newHandler(c), "GET", "/api/games/-700", "", auth); w.Code != 502 {
		t.Fatal(w.Code)
	}
}

// TestMissingBackendEndpoints 模拟 Impostor 1.9.x：对未知路由返回空 404。
// 网关应根据接口代答空列表或房间不存在，而不是把空 404 直接透传。
func TestMissingBackendEndpoints(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	defer upstream.Close()
	h := newHandler(testConfig(t, upstream.URL))
	auth := "Bearer " + createToken("test", 50595550)
	for _, row := range []struct {
		method, path string
		status       int
		body         string
	}{
		{"GET", "/api/games/123", 404, `{"Errors":[{"Reason":3}],"Game":null}`},
		{"GET", "/api/games/filtered?filter=x", 200, `{"games":[],"metadata":{"allGamesCount":0,"matchingGamesCount":0}}`},
		{"GET", "/api/filters", 200, `{"filters":[]}`},
		{"GET", "/api/games", 200, `[]`},
		{"POST", "/api/games?gameId=1", 404, `{"Errors":[{"Reason":3}]}`},
		// /api/filtertags 两个 Impostor 版本都没实现，网关返回 404 Reason 17。
		{"GET", "/api/filtertags", 404, `{"Errors":[{"Reason":17}]}`},
	} {
		w := call(h, row.method, row.path, "", auth)
		// 这个测试要求响应体完全相等（去掉末尾换行后），并检查 Content-Type。
		if w.Code != row.status || strings.TrimSpace(w.Body.String()) != row.body || w.Header().Get("Content-Type") != "application/json" {
			t.Fatal(row.path, w.Code, w.Body.String())
		}
	}
}

// TestInvalidRequests 集中验证方法、路径、房间码和 token 的校验。
func TestInvalidRequests(t *testing.T) {
	h := newHandler(testConfig(t, "http://127.0.0.1:1"))
	auth := "Bearer " + createToken("test", 50663600)
	for _, row := range []struct {
		method, path, body, auth string
		status                   int
	}{
		{"GET", "/", "", "", 200}, {"HEAD", "/", "", "", 200}, {"GET", "/other", "", auth, 404},
		{"GET", "/api/user", "", auth, 405}, {"DELETE", "/api/games", "", auth, 405},
		{"POST", "/api/user", "{", "", 400}, {"POST", "/api/user", `{"Puid":"x","Username":"x","Language":0}`, "", 400},
		{"GET", "/api/games/not-id", "", auth, 400}, {"POST", "/api/games", "", auth, 400},
		// 2147483648 超出 int32，validGameID 应拒绝。
		{"GET", "/api/games/2147483648", "", auth, 400}, {"GET", "/api/games", "", "Bearer !!!", 401},
		// 完全缺 token / token 里没有 ClientVersion 都返回 401。
		{"PUT", "/api/games", "", "", 401}, {"PUT", "/api/games", "", "Bearer " + base64.StdEncoding.EncodeToString([]byte(`{"Content":{}}`)), 401},
	} {
		w := call(h, row.method, row.path, row.body, row.auth)
		// 这里用 Errorf 而不是 Fatalf：一个用例失败后仍继续跑其余用例，便于一次看到全部问题。
		if w.Code != row.status {
			t.Errorf("%s %s: %d", row.method, row.path, w.Code)
		}
	}
}

// TestHealthz 验证 /healthz 的 200/503 分支。
func TestHealthz(t *testing.T) {
	// 返回 404 也算"可达"：Impostor 1.9.x 的根路径本来就没有 handler。
	up := httptest.NewServer(http.NotFoundHandler())
	defer up.Close()
	c := testConfig(t, up.URL)
	g := newGateway(c)
	// 手动执行一次启动检查，避免依赖后台定时器。
	g.health.checkAll(context.Background())
	if w := call(g.handler(), "GET", "/healthz", "", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"healthy":true`) {
		t.Fatal(w.Code, w.Body.String())
	}
	// 把 new 后端指向一个没有服务的端口，重新检查后应返回 503。
	c.Backends[1].HTTPEndpoint = "http://127.0.0.1:1"
	g = newGateway(c)
	g.health.checkAll(context.Background())
	if w := call(g.handler(), "GET", "/healthz", "", ""); w.Code != 503 || !strings.Contains(w.Body.String(), `"state":"down"`) {
		t.Fatal(w.Code, w.Body.String())
	}
}

// TestHealthThreshold 验证已经 up 的后端要连续失败 threshold 次才判 down，
// 5xx 计为失败，成功一次立即恢复。
func TestHealthThreshold(t *testing.T) {
	c := testConfig(t, "http://127.0.0.1:1")
	m := newHealthMonitor(&c, nil)
	b := &c.Backends[0]
	state := func() backendState { states, _ := m.snapshot(); return states[0] }

	// unknown 时第一次失败直接判 down（从来没成功过）。
	m.record(b, serverStatusError(500), 0)
	if s := state(); s.State != stateDown || s.LastError != string(kindServerStatus) {
		t.Fatalf("%+v", s)
	}
	m.record(b, nil, 0)
	if s := state(); s.State != stateUp || s.Failures != 0 {
		t.Fatalf("%+v", s)
	}
	// up 之后前两次失败仍是 up，第三次（默认阈值 3）才判 down。
	for i := 1; i <= 3; i++ {
		m.record(b, errInvalidPayload, 0)
		if want := map[bool]string{true: stateDown, false: stateUp}[i == 3]; state().State != want {
			t.Fatalf("after %d failures: %+v", i, state())
		}
	}
}

// TestHealthCountsServerErrors 验证后台检查和真实请求遇到 5xx 都记为失败，4xx 仍算可达。
func TestHealthCountsServerErrors(t *testing.T) {
	status := 503
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
	defer upstream.Close()
	g := newGateway(testConfig(t, upstream.URL))
	g.health.checkAll(context.Background())
	if states, healthy := g.health.snapshot(); healthy || states[1].LastError != string(kindServerStatus) {
		t.Fatalf("%+v", states)
	}
	status = 404
	g.health.checkAll(context.Background())
	if _, healthy := g.health.snapshot(); !healthy {
		t.Fatal("404 must count as reachable")
	}
	// 真实请求收到 5xx：连续 3 次后判 down。
	status = 500
	for range 3 {
		call(g.handler(), "GET", "/api/games", "", "Bearer "+createToken("test", 50663600))
	}
	if states, healthy := g.health.snapshot(); healthy || states[1].Failures != 3 {
		t.Fatalf("%+v", states)
	}
}

// TestSeparateBackends 验证不同版本确实被转发到不同后端，而不是总用第一个。
func TestSeparateBackends(t *testing.T) {
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `[{"IP":0,"Port":0,"HostName":"old"}]`)
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `[{"IP":0,"Port":0,"HostName":"new"}]`)
	}))
	defer second.Close()
	c := testConfig(t, first.URL)
	c.Backends[1].HTTPEndpoint = second.URL
	h := newHandler(c)
	for _, row := range []struct{ version, name string }{{"2023.10.1", "old"}, {"2026.7.20", "new"}} {
		v, _ := encodeVersion(row.version)
		w := call(h, "GET", "/api/games", "", "Bearer "+createToken("test", v))
		// 用后端返回的 HostName 判断请求实际发到了哪个模拟服务器。
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"HostName":"`+row.name+`"`) {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}
