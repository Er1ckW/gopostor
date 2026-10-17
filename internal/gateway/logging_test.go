package gateway

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var output bytes.Buffer
	oldWriter, oldFlags := log.Writer(), log.Flags()
	log.SetOutput(&output)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(oldWriter); log.SetFlags(oldFlags) })
	return &output
}

// requestRecord returns the JSON record of the request among the lines that were logged.
func requestRecord(t *testing.T, output string) map[string]any {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) == nil && record["event"] == "http_request" {
			return record
		}
	}
	t.Fatal("no request record:", output)
	return nil
}

func TestDiagnosticLogs(t *testing.T) {
	output := captureLog(t)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Request-ID") == "" {
			t.Error("missing upstream request ID")
		}
		w.WriteHeader(400)
		_, _ = w.Write([]byte("secret-backend-body"))
	}))
	defer backend.Close()
	h := newHandler(testConfig(t, backend.URL))
	cases := []struct {
		method, path, body, auth, field string
		value                           any
	}{
		{"POST", "/api/user", `{"Puid":"secret-puid","Username":"secret-name","Language":0,"ClientVersion":50595550}`, "Bearer secret-eos", "backend", "old"},
		{"POST", "/api/user", `{"Puid":"secret-puid","Username":"secret-name","Language":0,"ClientVersion":50595550}`, "", "client_version_text", "2023.11.15"},
		{"POST", "/api/user", `{"Puid":"secret-puid","Username":"secret-name","Language":0,"ClientVersion":1}`, "", "route", routeBelow},
		{"POST", "/api/user", `{"Puid":"secret-puid","Username":"secret-name","Language":"secret-value","ClientVersion":50595550}`, "", "level", "info"},
		{"POST", "/api/user", `{"Puid":"secret-puid","Username":"secret-name","Language":0,"ClientVersion":"secret-value"}`, "", "validation_error", "missing_or_invalid_client_version"},
		{"POST", "/api/user", `{"Puid":1,"ClientVersion":50595550}`, "", "invalid_field", "Puid"},
		{"PUT", "/api/games", "", "Bearer secret-token", "token_problem", "not_bearer_base64"},
		{"PUT", "/api/games", "", "", "token_problem", "missing_authorization"},
		{"PUT", "/api/games", "\x00", "Bearer " + createToken("secret-puid", 50595550), "public_port", float64(22031)},
		{"GET", "/api/games?token=secret-query", "", "Bearer " + createToken("secret-puid", 50595550), "backend_status", float64(400)},
	}
	ids := map[string]bool{}
	for _, tc := range cases {
		output.Reset()
		w := call(h, tc.method, tc.path, tc.body, tc.auth)
		record := requestRecord(t, output.String())
		if record[tc.field] != tc.value {
			t.Fatalf("%s: %v", tc.field, record)
		}
		if record["time"] == nil {
			t.Fatal("missing time field")
		}
		id := w.Header().Get("X-Request-ID")
		if id == "" || ids[id] || record["request_id"] != id {
			t.Fatal("request ID mismatch")
		}
		ids[id] = true
		if record["status"] != float64(w.Code) || record["duration_ms"] == nil || record["level"] == nil {
			t.Fatal("missing completion fields")
		}
		if strings.Contains(output.String(), "secret-") || strings.Contains(output.String(), createToken("secret-puid", 50595550)) {
			t.Fatal("sensitive data in log:", output.String())
		}
	}
}

func TestReadableProblemLines(t *testing.T) {
	output := captureLog(t)
	g := newGateway(testConfig(t, "http://127.0.0.1:1"))
	g.shownOnce.Range(func(key, _ any) bool { g.shownOnce.Delete(key); return true })
	h := g.handler()
	call(h, "PUT", "/api/games", "", "Bearer "+createToken("x", 50663600))
	text := output.String()
	for _, want := range []string{`"level":"error"`, `"issues":["backend_unavailable"]`, "[ERROR] 后端 new 不可达", "PUT /api/games -> 502 backend_unavailable", "HttpServer.Enabled"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}
	// A backend that stays down is not reported again on every request.
	output.Reset()
	call(h, "PUT", "/api/games", "", "Bearer "+createToken("x", 50663600))
	if strings.Contains(output.String(), "不可达（") {
		t.Fatal("repeated state change:", output.String())
	}

	// Versions outside every range warn once, later requests of that version are info.
	output.Reset()
	body := `{"Puid":"x","ClientVersion":50676850}` // 2027.1.1
	call(h, "POST", "/api/user", body, "")
	if !strings.Contains(output.String(), "[WARN]") || !strings.Contains(output.String(), "高于所有已配置范围") {
		t.Fatal(output.String())
	}
	output.Reset()
	call(h, "POST", "/api/user", body, "")
	if strings.Contains(output.String(), "[WARN]") || !strings.Contains(output.String(), "version_above_range") {
		t.Fatal(output.String())
	}
}

func TestQuietHealthzAndStats(t *testing.T) {
	output := captureLog(t)
	g := newGateway(testConfig(t, "http://127.0.0.1:1"))
	h := g.handler()
	g.stats.flush(time.Minute)
	output.Reset()
	call(h, "GET", "/healthz", "", "")
	if output.Len() != 0 {
		t.Fatal("healthy /healthz was logged:", output.String())
	}
	call(h, "POST", "/api/user", `{"Puid":"x","ClientVersion":50595550}`, "")
	output.Reset()
	g.stats.flush(time.Minute)
	if !strings.Contains(output.String(), `"event":"stats"`) || !strings.Contains(output.String(), "2023.11.15×1") {
		t.Fatal(output.String())
	}
	output.Reset()
	g.stats.flush(time.Minute)
	if output.Len() != 0 {
		t.Fatal("empty window was logged")
	}
}
