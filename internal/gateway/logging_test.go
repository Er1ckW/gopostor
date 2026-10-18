// 本文件测试简化后的访问日志：一行 JSON，包含完整请求和响应 body。
package gateway

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http/httptest"
	"strings"
	"testing"
)

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
