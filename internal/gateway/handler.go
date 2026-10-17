package gateway

import (
	"io"
	"net/http"
	"strings"
	"time"
)

type gateway struct {
	config *Config
	client *http.Client
	health *healthMonitor
}

func newGateway(c Config) *gateway {
	client := &http.Client{Timeout: time.Duration(c.BackendTimeoutMS) * time.Millisecond, CheckRedirect: func(*http.Request, []*http.Request) error { return errRedirect }}
	g := &gateway{config: &c, client: client}
	g.health = newHealthMonitor(g.config, client)
	return g
}

func newHandler(c Config) http.Handler { return newGateway(c).handler() }

func (g *gateway) handler() http.Handler { return logRequests(g) }

func (g *gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	isRead := r.Method == http.MethodGet || r.Method == http.MethodHead
	if path == "/" && isRead {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "Impostor matchmaking gateway is running.")
		return
	}
	if path == "/healthz" && isRead {
		quiet(w)
		states, healthy := g.health.snapshot()
		status := http.StatusOK
		if !healthy {
			status = http.StatusServiceUnavailable
			issue(w, levelWarn, "healthz_backend_down", "健康检查报告有后端不可达，详见启动后的 [ERROR] 后端 ... 不可达 日志")
		}
		writeJSON(w, status, map[string]any{"healthy": healthy, "backends": states})
		return
	}
	if path == "/api/user" {
		g.serveToken(w, r)
		return
	}
	allowed := "GET"
	byCode := false
	switch {
	case path == "/api/games":
		allowed = "GET, POST, PUT"
	case path == "/api/games/filtered" || path == "/api/filters" || path == "/api/filtertags":
	case strings.HasPrefix(path, "/api/games/") && !strings.Contains(strings.TrimPrefix(path, "/api/games/"), "/") && strings.TrimPrefix(path, "/api/games/") != "":
		byCode = true
		if !validGameID(strings.TrimPrefix(path, "/api/games/")) {
			issue(w, levelInfo, "invalid_game_id", "")
			gameFailure(w, true, 400, reasonGameNotFound, "Invalid game ID")
			return
		}
	default:
		issue(w, levelInfo, "unknown_endpoint", "")
		failure(w, 404, reasonError, "Unknown endpoint")
		return
	}
	if !strings.Contains(", "+allowed+", ", ", "+r.Method+", ") {
		issue(w, levelInfo, "method_not_allowed", "")
		w.Header().Set("Allow", allowed)
		failure(w, 405, reasonError, "Method not allowed")
		return
	}
	legacy := path == "/api/games" && r.Method == "POST"
	if legacy && !validGameID(r.URL.Query().Get("gameId")) {
		issue(w, levelInfo, "invalid_game_id", "")
		failure(w, 400, reasonGameNotFound, "Invalid game ID")
		return
	}
	version, problem, err := tokenVersion(r.Header.Get("Authorization"))
	if err != nil {
		diagnostic(w, "validation_error", "invalid_matchmaking_token")
		diagnostic(w, "token_problem", problem)
		issue(w, levelWarn, "invalid_token", tokenHint(problem))
		gameFailure(w, byCode, 401, reasonNotAuthorized, "Missing or invalid matchmaking token")
		return
	}
	logVersion(w, version)
	rt := g.config.route(version)
	g.logRoute(w, rt)
	if rt.kind != routeExact && g.config.RejectUnknownVersions {
		gameFailure(w, byCode, 404, reasonIncorrectVersion, "Unsupported client version")
		return
	}
	b := rt.backend
	if path == "/api/games" && r.Method == "PUT" {
		g.serveCreate(w, r, b)
		return
	}
	g.proxy(w, r, b, legacy, byCode)
}
