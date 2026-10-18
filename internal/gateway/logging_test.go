// 本文件测试访问日志：一行 JSON。-debug 模式包含完整请求和响应 body，
// 默认模式只包含脱敏后的内容。
package gateway

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type trackedRequestBody struct {
	reader io.Reader
	read   int
	closed bool
	err    error
}

func (b *trackedRequestBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	b.read += n
	if err == io.EOF && b.err != nil {
		return n, b.err
	}
	return n, err
}

func (b *trackedRequestBody) Close() error { b.closed = true; return nil }

func TestAccessLogRejectsOversizedBody(t *testing.T) {
	for _, path := range []string{"/api/user", "/api/games", "/healthz"} {
		t.Run(path, func(t *testing.T) {
			output := captureLog(t)
			c := testConfig(t, "http://127.0.0.1:1")
			c.ProbeBackendOnCreate = false
			body := &trackedRequestBody{reader: strings.NewReader(strings.Repeat("x", 4<<20))}
			r := httptest.NewRequest("PUT", path, nil)
			if path == "/api/user" {
				r.Method = "POST"
			}
			if path == "/healthz" {
				r.Method = "GET"
			}
			r.Body = body
			r.ContentLength = -1 // Chunked bodies must also be bounded.
			r.Header.Set("Authorization", "Bearer "+createToken("player", 50595550))
			w := httptest.NewRecorder()
			newDebugHandler(c).ServeHTTP(w, r)
			if w.Code != http.StatusRequestEntityTooLarge || body.read != maxRequestBodyBytes+1 || !body.closed {
				t.Fatalf("status=%d read=%d closed=%v", w.Code, body.read, body.closed)
			}
			entry := parseAccessLog(t, output.String())
			if len(entry.RequestBody) != maxRequestBodyBytes || entry.Status != w.Code || entry.ResponseBody != w.Body.String() {
				t.Fatal("oversized request or error response was not logged correctly")
			}
		})
	}
}

func TestAccessLogRejectsBodyReadError(t *testing.T) {
	output := captureLog(t)
	payload := `{"ClientVersion":50595550}`
	body := &trackedRequestBody{reader: strings.NewReader(payload), err: io.ErrUnexpectedEOF}
	r := httptest.NewRequest("POST", "/api/user", nil)
	r.Body = body
	w := httptest.NewRecorder()
	newDebugHandler(testConfig(t, "http://127.0.0.1:1")).ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest || !body.closed {
		t.Fatalf("status=%d closed=%v", w.Code, body.closed)
	}
	entry := parseAccessLog(t, output.String())
	if entry.RequestBody != payload || entry.Status != w.Code || entry.ResponseBody != w.Body.String() {
		t.Fatal("failed request or error response was not logged correctly")
	}
}

func TestAccessLogAcceptsBodyAtLimit(t *testing.T) {
	output := captureLog(t)
	payload := `{"ClientVersion":50595550}`
	payload += strings.Repeat(" ", maxRequestBodyBytes-len(payload))
	w := call(newDebugHandler(testConfig(t, "http://127.0.0.1:1")), "POST", "/api/user", payload, "")
	entry := parseAccessLog(t, output.String())
	if w.Code != http.StatusOK || entry.RequestBody != payload || entry.ResponseBody != w.Body.String() {
		t.Fatal("request at the limit must succeed and be logged in full")
	}
}

func TestAccessLogIncludesDecodedAuthorization(t *testing.T) {
	backend := hostBackend(t, 16777343, 22032)
	for _, method := range []string{"PUT", "POST"} {
		t.Run(method, func(t *testing.T) {
			output := captureLog(t)
			auth := createToken("player", 50663625)
			path := "/api/games"
			if method == "POST" {
				path += "?gameId=123"
			}
			w := call(newDebugHandler(testConfig(t, backend.URL)), method, path, "\x00", "Bearer "+auth)
			entry := parseAccessLog(t, output.String())
			want, _ := base64.StdEncoding.DecodeString(auth)
			if w.Code != http.StatusOK || string(entry.AuthorizationToken) != string(want) {
				t.Fatalf("status=%d authorization_token=%s; want %s", w.Code, entry.AuthorizationToken, want)
			}
		})
	}
}

func TestAccessLogOmitsUndecodableAuthorization(t *testing.T) {
	for _, auth := range []string{"", "Bearer !!!", "Bearer " + base64.StdEncoding.EncodeToString([]byte("not JSON")), "Bearer " + strings.Repeat("A", 16388)} {
		t.Run(auth[:min(len(auth), 30)], func(t *testing.T) {
			output := captureLog(t)
			w := call(newHandler(testConfig(t, "http://127.0.0.1:1")), "PUT", "/api/games", "", auth)
			entry := parseAccessLog(t, output.String())
			if w.Code != http.StatusUnauthorized || len(entry.AuthorizationToken) != 0 {
				t.Fatalf("status=%d authorization_token=%s", w.Code, entry.AuthorizationToken)
			}
		})
	}
}

// newDebugHandler 和 newHandler 一样，但打开 -debug：访问日志记录完整数据。
func newDebugHandler(c Config) http.Handler {
	g := newGateway(c)
	g.debug = true
	return g.handler()
}

// captureLog 把标准 log 的输出重定向到内存缓冲区，测试结束后恢复。
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var output bytes.Buffer
	oldWriter, oldFlags := log.Writer(), log.Flags()
	log.SetOutput(&output)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(oldWriter); log.SetFlags(oldFlags) })
	return &output
}

// parseAccessLog 从捕获的日志中解析出唯一一条访问记录。
func parseAccessLog(t *testing.T, output string) accessLog {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		var entry accessLog
		if json.Unmarshal([]byte(line), &entry) == nil && entry.Path != "" {
			return entry
		}
	}
	t.Fatal("no access log found:", output)
	return accessLog{}
}

// TestAccessLogCapturesFullRequestAndResponse 验证时间、来源、请求 body 和响应 body 都被记录。
func TestAccessLogCapturesFullRequestAndResponse(t *testing.T) {
	output := captureLog(t)
	h := newDebugHandler(testConfig(t, hostBackend(t, 16777343, 22032).URL))

	body := `{"Puid":"用户","Username":"Player","Language":0,"ClientVersion":50595550}`
	r := httptest.NewRequest("POST", "/api/user?x=1", strings.NewReader(body))
	r.Header.Set("User-Agent", "AmongUs/Test")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	entry := parseAccessLog(t, output.String())
	if entry.Time == "" {
		t.Fatal("missing time")
	}
	if entry.ClientIP != "192.0.2.1" || entry.Peer != "192.0.2.1:1234" {
		t.Fatalf("source = %q / %q", entry.ClientIP, entry.Peer)
	}
	if entry.Method != "POST" || entry.Path != "/api/user" || entry.Query != "x=1" {
		t.Fatalf("request line = %s %s?%s", entry.Method, entry.Path, entry.Query)
	}
	if entry.UserAgent != "AmongUs/Test" || entry.RequestContentType != "application/json" {
		t.Fatalf("request metadata = %q / %q", entry.UserAgent, entry.RequestContentType)
	}
	if entry.RequestBody != body {
		t.Fatalf("request body = %q; want %q", entry.RequestBody, body)
	}
	if entry.Status != 200 {
		t.Fatalf("status = %d", entry.Status)
	}
	if entry.ResponseContentType != "text/plain; charset=utf-8" {
		t.Fatalf("response content type = %q", entry.ResponseContentType)
	}
	if entry.ResponseBody != w.Body.String() {
		t.Fatalf("response body = %q; want %q", entry.ResponseBody, w.Body.String())
	}
	if entry.DurationMS < 0 {
		t.Fatalf("duration = %d", entry.DurationMS)
	}
}

// TestAccessLogRecordsErrorResponse 验证错误响应也会记录状态码和完整响应体。
func TestAccessLogRecordsErrorResponse(t *testing.T) {
	output := captureLog(t)
	h := newHandler(testConfig(t, "http://127.0.0.1:1"))

	w := call(h, "PUT", "/api/games", "", "")
	entry := parseAccessLog(t, output.String())

	if entry.Status != w.Code || entry.Status != 401 {
		t.Fatalf("status = %d, response = %d", entry.Status, w.Code)
	}
	if entry.RequestBody != "" {
		t.Fatalf("request body = %q", entry.RequestBody)
	}
	if !strings.Contains(entry.ResponseBody, `"Reason":11`) {
		t.Fatalf("response body = %q", entry.ResponseBody)
	}
}

// TestAccessLogUsesTrustedForwardedClient 验证本机代理传来的 X-Real-IP 会成为 client_ip。
func TestAccessLogUsesTrustedForwardedClient(t *testing.T) {
	output := captureLog(t)
	h := newHandler(testConfig(t, "http://127.0.0.1:1"))

	r := httptest.NewRequest("GET", "/healthz", nil)
	r.RemoteAddr = "127.0.0.1:5000"
	r.Header.Set("X-Real-IP", "203.0.113.9")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	entry := parseAccessLog(t, output.String())
	if entry.ClientIP != "203.0.113.9" || entry.Peer != "127.0.0.1:5000" {
		t.Fatalf("source = %q / %q", entry.ClientIP, entry.Peer)
	}
}

// TestAccessLogRedactsByDefault 验证不带 -debug 时请求 body、响应 token 和
// Authorization 中的 Puid/Username 都不会以原文出现在日志里。
func TestAccessLogRedactsByDefault(t *testing.T) {
	output := captureLog(t)
	h := newHandler(testConfig(t, hostBackend(t, 16777343, 22032).URL))

	body := `{"Puid":"0123456789abcdef","Username":"Player","Language":0,"ClientVersion":50595550}`
	w := call(h, "POST", "/api/user", body, "")
	token := w.Body.String()
	entry := parseAccessLog(t, output.String())
	if entry.RequestBody != `{"ClientVersion":50595550,"Language":0,"Puid":"0123***","Username":"***"}` {
		t.Fatalf("request body = %q", entry.RequestBody)
	}
	if strings.Contains(entry.ResponseBody, token) || !strings.HasPrefix(entry.ResponseBody, "[redacted: ") {
		t.Fatalf("token leaked into response body log: %q", entry.ResponseBody)
	}

	output.Reset()
	call(h, "PUT", "/api/games", "", "Bearer "+token)
	entry = parseAccessLog(t, output.String())
	if got := string(entry.AuthorizationToken); strings.Contains(got, "0123456789abcdef") || !strings.Contains(got, `"Puid":"0123***"`) || !strings.Contains(got, `"ClientVersion":50595550`) {
		t.Fatalf("authorization_token = %s", got)
	}
	if strings.Contains(output.String(), "0123456789abcdef") || strings.Contains(output.String(), "Player") {
		t.Fatal("sensitive value leaked:", output.String())
	}
}

// TestAccessLogRedactsListings 验证房间列表里的房主名字被隐藏，其他字段保留。
func TestAccessLogRedactsListings(t *testing.T) {
	output := captureLog(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `[{"IP":0,"Port":1,"HostName":"Alice","TrueHostName":"Alice","GameId":-123}]`)
	}))
	defer upstream.Close()
	call(newHandler(testConfig(t, upstream.URL)), "GET", "/api/games", "", "Bearer "+createToken("player", 50663600))
	entry := parseAccessLog(t, output.String())
	if strings.Contains(entry.ResponseBody, "Alice") || !strings.Contains(entry.ResponseBody, `"GameId":-123`) || !strings.Contains(entry.ResponseBody, `"HostName":"***"`) {
		t.Fatalf("response body = %q", entry.ResponseBody)
	}
}

// TestAccessLogOmitsLargeBodies 验证默认模式下超过 16KB 的 body 只记录长度。
func TestAccessLogOmitsLargeBodies(t *testing.T) {
	output := captureLog(t)
	large := `{"ClientVersion":50595550,"Pad":"` + strings.Repeat("x", maxRedactedBodyBytes) + `"}`
	call(newHandler(testConfig(t, "http://127.0.0.1:1")), "POST", "/api/user", large, "")
	entry := parseAccessLog(t, output.String())
	if entry.RequestBody != fmt.Sprintf("[omitted: %d bytes]", len(large)) {
		t.Fatalf("request body = %q", entry.RequestBody)
	}
}

// TestRedactBody 覆盖脱敏函数的各个分支。
func TestRedactBody(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"empty", "", ""},
		{"non JSON", "eyJhYmMi", "[redacted: 8 bytes non-JSON]"},
		{"trailing JSON", `{} {}`, "[redacted: 5 bytes non-JSON]"},
		{"case insensitive keys", `{"puid":"abcdefgh","USERNAME":"x"}`, `{"USERNAME":"***","puid":"abcd***"}`},
		{"short and empty id", `[{"Puid":"abc"},{"Puid":""},{"Puid":12}]`, `[{"Puid":"***"},{"Puid":""},{"Puid":"***"}]`},
		{"multibyte id", `{"Puid":"用户标识符号"}`, `{"Puid":"用户标识***"}`},
		{"nested and big numbers", `{"games":[{"HostName":"a<b","Large":9007199254740993}]}`, `{"games":[{"HostName":"***","Large":9007199254740993}]}`},
		{"html not escaped", `{"Message":"a<b"}`, `{"Message":"a<b"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := redactBody([]byte(tc.in), len(tc.in)); got != tc.want {
				t.Fatalf("redactBody(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
	// 只缓存了一部分（data 比 size 短）时不能尝试解析。
	if got := redactBody([]byte(`{"a":1}`), 100); got != "[omitted: 100 bytes]" {
		t.Fatal(got)
	}
}
