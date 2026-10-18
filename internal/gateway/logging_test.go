// 本文件测试简化后的访问日志：一行 JSON，包含完整请求和响应 body。
package gateway

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
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
			newHandler(c).ServeHTTP(w, r)
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
	newHandler(testConfig(t, "http://127.0.0.1:1")).ServeHTTP(w, r)
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
	w := call(newHandler(testConfig(t, "http://127.0.0.1:1")), "POST", "/api/user", payload, "")
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
			w := call(newHandler(testConfig(t, backend.URL)), method, path, "\x00", "Bearer "+auth)
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
	h := newHandler(testConfig(t, hostBackend(t, 16777343, 22032).URL))

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
