package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testConfig(t *testing.T, endpoint string) Config {
	t.Helper()
	c, err := parseConfig([]byte(fmt.Sprintf(`{"backends":[{"name":"old","minVersion":"2023.10.1","maxVersion":"2024.2.3","publicIp":"203.0.113.10","publicPort":22031,"httpEndpoint":%q},{"name":"new","minVersion":"2024.3.1","maxVersion":"2026.7.21","publicIp":"127.0.0.1","publicPort":22032,"httpEndpoint":%q}]}`, endpoint, endpoint)))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// hostBackend answers PUT /api/games like Impostor does, reporting the given public address.
func hostBackend(t *testing.T, ip uint32, port int) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `{"Ip":%d,"Port":%d}`, ip, port)
	}))
	t.Cleanup(s.Close)
	return s
}

func call(h http.Handler, method, path, body, auth string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", auth)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func requestToken(t *testing.T, h http.Handler, body string) string {
	t.Helper()
	w := call(h, "POST", "/api/user", body, "")
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
		t.Fatal(w.Code, w.Body.String())
	}
	return w.Body.String()
}

func TestTokenAndRouting(t *testing.T) {
	c := testConfig(t, hostBackend(t, 16777343, 22032).URL)
	h := newHandler(c)
	for _, row := range []struct {
		version string
		port    int
	}{{"2023.10.1", 22031}, {"2024.2.3", 22031}, {"2024.3.1", 22032}, {"2026.7.21", 22032}} {
		v, _ := encodeVersion(row.version)
		for _, flag := range []int64{0, 25} {
			tokenText := requestToken(t, h, fmt.Sprintf(`{"Puid":"用户","Username":"Player","Language":0,"ClientVersion":%d}`, v+flag))
			data, err := base64.StdEncoding.DecodeString(tokenText)
			if err != nil {
				t.Fatal(err)
			}
			var tok token
			if err = json.Unmarshal(data, &tok); err != nil {
				t.Fatal(err)
			}
			if tok.Content.Puid != "用户" || tok.Content.ClientVersion != v+flag || tok.Content.ExpiresAt != "2012-12-21T00:00:00" || tok.Hash != "impostor_was_here" {
				t.Fatal(tok)
			}
			host := call(h, "PUT", "/api/games", "", "Bearer "+tokenText)
			if host.Code != 200 || !strings.Contains(host.Body.String(), fmt.Sprintf(`"Port":%d`, row.port)) {
				t.Fatal(host.Body.String())
			}
		}
	}
}

func TestLenientTokenRequest(t *testing.T) {
	h := newHandler(testConfig(t, hostBackend(t, 16777343, 22032).URL))
	for _, body := range []string{
		`{"Puid":"x","ClientVersion":50663600}`,
		`{"ClientVersion":50663600}`,
		`{"Puid":"","Username":"","Language":0,"ClientVersion":50663600}`,
		`{"puid":"x","clientversion":"50663600","language":"3"}`,
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

func TestOutOfRangeVersions(t *testing.T) {
	old := hostBackend(t, 184120523, 22031)
	c := testConfig(t, old.URL)
	c.Backends[1].HTTPEndpoint = hostBackend(t, 16777343, 22032).URL
	h := newHandler(c)
	for _, row := range []struct {
		version string
		port    int
	}{{"2022.12.14", 22031}, {"2024.2.20", 22031}, {"2026.10.1", 22032}} {
		v, _ := encodeVersion(row.version)
		tokenText := requestToken(t, h, fmt.Sprintf(`{"Puid":"x","Username":"x","Language":0,"ClientVersion":%d}`, v))
		if w := call(h, "PUT", "/api/games", "", "Bearer "+tokenText); w.Code != 200 || !strings.Contains(w.Body.String(), fmt.Sprintf(`"Port":%d`, row.port)) {
			t.Fatal(row.version, w.Code, w.Body.String())
		}
	}
	c.RejectUnknownVersions = true
	h = newHandler(c)
	v, _ := encodeVersion("2026.10.1")
	tokenText := requestToken(t, h, fmt.Sprintf(`{"Puid":"x","ClientVersion":%d}`, v))
	if w := call(h, "PUT", "/api/games", "", "Bearer "+tokenText); w.Code != 404 || !strings.Contains(w.Body.String(), `"Reason":5`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := call(h, "GET", "/api/games/123", "", "Bearer "+tokenText); w.Code != 404 || !strings.Contains(w.Body.String(), `"Game":null`) {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestCreateProbe(t *testing.T) {
	auth := "Bearer " + createToken("test", 50663600)
	down := testConfig(t, "http://127.0.0.1:1")
	if w := call(newHandler(down), "PUT", "/api/games", "", auth); w.Code != 502 || !strings.Contains(w.Body.String(), `"Reason":17`) {
		t.Fatal(w.Code, w.Body.String())
	}
	down.ProbeBackendOnCreate = false
	if w := call(newHandler(down), "PUT", "/api/games", "", auth); w.Code != 200 || !strings.Contains(w.Body.String(), `"Port":22032`) {
		t.Fatal(w.Code, w.Body.String())
	}
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer broken.Close()
	if w := call(newHandler(testConfig(t, broken.URL)), "PUT", "/api/games", "", auth); w.Code != 502 {
		t.Fatal(w.Code)
	}
	// A backend reporting another public address still gets the configured one handed out.
	mismatch := testConfig(t, hostBackend(t, 1, 1).URL)
	if w := call(newHandler(mismatch), "PUT", "/api/games", "", auth); w.Code != 200 || !strings.Contains(w.Body.String(), `"Ip":16777343,"Port":22032`) {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestProxy(t *testing.T) {
	const query = "?filter=%7B%22FilterSets%22%3A%5B%5D%7D"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
				if r.URL.Query().Get("gameId") != "-123" {
					t.Error("game ID changed")
				}
				_, _ = io.WriteString(w, `{"Ip":0,"Port":1}`)
			} else {
				_, _ = io.WriteString(w, `[{"IP":0,"Port":1}]`)
			}
		case "/api/games/-404":
			w.WriteHeader(404)
			_, _ = io.WriteString(w, `{"Errors":[{"Reason":3}]}`)
		case "/api/games/-450":
			w.WriteHeader(400)
			_, _ = io.WriteString(w, `{"Errors":[{"Reason":113,"Message":"Unknown exception caught in filterSystem.Exception: at Impostor.Server"}]}`)
		case "/api/games/-500":
			w.WriteHeader(503)
			_, _ = io.WriteString(w, "broken")
		case "/api/games/-600":
			_, _ = io.WriteString(w, "invalid JSON")
		case "/api/games/-700":
			time.Sleep(60 * time.Millisecond)
			_, _ = io.WriteString(w, `{}`)
		case "/api/games/-800":
			http.Redirect(w, r, "/api/games", 302)
		case "/api/filters", "/api/filtertags":
			_, _ = io.WriteString(w, `{"filters":["keep"]}`)
		default:
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
		{"GET", "/api/games", 200, `"IP":16777343`},
		{"POST", "/api/games?gameId=-123", 200, `"Ip":16777343`},
		{"GET", "/api/games/filtered" + query, 200, `"Options":"keep"`},
		{"GET", "/api/games/-123", 200, `"Large":9007199254740993`},
		{"GET", "/api/games/-404", 404, `{"Errors":[{"Reason":3}],"Game":null}`},
		{"GET", "/api/games/-450", 400, `{"Errors":[{"Reason":113}],"Game":null}`},
		{"GET", "/api/games/-500", 503, `"Reason":17`},
		{"GET", "/api/games/-600", 502, "unavailable"},
		{"GET", "/api/games/-800", 502, "unavailable"},
		{"GET", "/api/filters", 200, `"keep"`},
		{"GET", "/api/filtertags", 200, `"keep"`},
	} {
		t.Run(row.path, func(t *testing.T) {
			w := call(h, row.method, row.path, "", auth)
			if w.Code != row.status || !strings.Contains(w.Body.String(), row.contains) || strings.Contains(w.Body.String(), "Exception") {
				t.Fatal(w.Code, w.Body.String())
			}
		})
	}
	c.BackendTimeoutMS = 10
	if w := call(newHandler(c), "GET", "/api/games/-700", "", auth); w.Code != 502 {
		t.Fatal(w.Code)
	}
}

// Impostor 1.9.x has no endpoints of Among Us 16.0 and answers them with an empty 404.
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
		{"GET", "/api/filtertags", 404, `{"Errors":[{"Reason":17}]}`},
	} {
		w := call(h, row.method, row.path, "", auth)
		if w.Code != row.status || strings.TrimSpace(w.Body.String()) != row.body || w.Header().Get("Content-Type") != "application/json" {
			t.Fatal(row.path, w.Code, w.Body.String())
		}
	}
}

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
		{"GET", "/api/games/2147483648", "", auth, 400}, {"GET", "/api/games", "", "Bearer !!!", 401},
		{"PUT", "/api/games", "", "", 401}, {"PUT", "/api/games", "", "Bearer " + base64.StdEncoding.EncodeToString([]byte(`{"Content":{}}`)), 401},
	} {
		w := call(h, row.method, row.path, row.body, row.auth)
		if w.Code != row.status {
			t.Errorf("%s %s: %d", row.method, row.path, w.Code)
		}
	}
}

func TestHealthz(t *testing.T) {
	up := httptest.NewServer(http.NotFoundHandler())
	defer up.Close()
	c := testConfig(t, up.URL)
	g := newGateway(c)
	g.health.checkAll(context.Background(), "startup")
	if w := call(g.handler(), "GET", "/healthz", "", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"healthy":true`) {
		t.Fatal(w.Code, w.Body.String())
	}
	c.Backends[1].HTTPEndpoint = "http://127.0.0.1:1"
	g = newGateway(c)
	g.health.checkAll(context.Background(), "startup")
	if w := call(g.handler(), "GET", "/healthz", "", ""); w.Code != 503 || !strings.Contains(w.Body.String(), `"state":"down"`) {
		t.Fatal(w.Code, w.Body.String())
	}
}

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
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"HostName":"`+row.name+`"`) {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}
