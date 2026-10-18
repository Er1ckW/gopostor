// 本文件是 HTTP 请求的总入口：判断路径、方法、token 和路由，
// 然后把请求交给 token.go 或 proxy.go。
//
// 日志不再参与业务函数：g.serve 及以下只接收 http.ResponseWriter，
// 不知道日志字段、问题代码或提示文案的存在。
package gateway

import (
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// gameIDPattern 匹配房间码：可选负号 + 一位或多位数字。
var gameIDPattern = regexp.MustCompile(`^-?\d+$`)

// gateway 汇集处理请求所需的共享状态。
// 它只被创建一次，之后被所有请求 goroutine 并发使用。
type gateway struct {
	config *Config
	client *http.Client
	health *healthMonitor
	// debug 为 true 时访问日志记录完整数据；默认 false，只记录脱敏后的内容。
	debug bool
}

// newGateway 创建后端 HTTP 客户端，并组装 gateway。
//
// 参数 c 是值传递，内部保存 &c；之后修改调用者的 Config 不会影响运行中的网关。
func newGateway(c Config) *gateway {
	client := &http.Client{
		Timeout: time.Duration(c.BackendTimeoutMS) * time.Millisecond,
		// 后端返回 3xx 时拒绝跟随，避免 PUT 被重定向到错误的服务。
		CheckRedirect: func(*http.Request, []*http.Request) error { return errRedirect },
	}
	g := &gateway{config: &c, client: client}
	g.health = newHealthMonitor(g.config, client)
	return g
}

// handler 返回包了访问日志的 http.Handler。
func (g *gateway) handler() http.Handler { return g.logRequests() }

// serve 完成单个请求的业务分发。
func (g *gateway) serve(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	isRead := r.Method == http.MethodGet || r.Method == http.MethodHead

	// 根路径：最简单的存活检查。
	if path == "/" && isRead {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "Impostor matchmaking gateway is running.")
		return
	}

	// /healthz：返回所有后端的健康状态；有后端 down 时返回 503。
	if path == "/healthz" && isRead {
		states, healthy := g.health.snapshot()
		status := http.StatusOK
		if !healthy {
			status = http.StatusServiceUnavailable
		}
		writeJSON(w, status, map[string]any{"healthy": healthy, "backends": states})
		return
	}

	// /api/user 不需要 token，由 token.go 处理。
	if path == "/api/user" {
		g.serveToken(w, r)
		return
	}

	// 剩余的都是 /api/games 系列接口。
	allowed := "GET"
	isGameLookup := false
	switch {
	case path == "/api/games":
		allowed = "GET, POST, PUT"
	case path == "/api/games/filtered" || path == "/api/filters" || path == "/api/filtertags":
	case strings.HasPrefix(path, "/api/games/") && !strings.Contains(strings.TrimPrefix(path, "/api/games/"), "/") && strings.TrimPrefix(path, "/api/games/") != "":
		isGameLookup = true
		if !validGameID(strings.TrimPrefix(path, "/api/games/")) {
			writeGameErrorResponse(w, true, 400, reasonGameNotFound, "Invalid game ID")
			return
		}
	default:
		writeErrorResponse(w, 404, reasonError, "Unknown endpoint")
		return
	}

	// 方法校验：把 allowed 和方法都补成 ", X, " 形式再判断子串。
	if !strings.Contains(", "+allowed+", ", ", "+r.Method+", ") {
		w.Header().Set("Allow", allowed)
		writeErrorResponse(w, 405, reasonError, "Method not allowed")
		return
	}

	// 旧版加入房间是 POST /api/games?gameId=...
	legacyJoin := path == "/api/games" && r.Method == "POST"
	if legacyJoin && !validGameID(r.URL.Query().Get("gameId")) {
		writeErrorResponse(w, 400, reasonGameNotFound, "Invalid game ID")
		return
	}

	// 从 Authorization: Bearer <base64 token> 中解析客户端版本。
	version, err := parseTokenVersion(r.Header.Get("Authorization"))
	if err != nil {
		writeGameErrorResponse(w, isGameLookup, 401, reasonNotAuthorized, "Missing or invalid matchmaking token")
		return
	}

	// 根据版本选择后端。
	rt := g.config.route(version)

	// 非精确命中且配置要求拒绝未知版本时，游戏请求直接返回 404 Reason 5。
	if rt.kind != routeExact && g.config.RejectUnknownVersions {
		writeGameErrorResponse(w, isGameLookup, 404, reasonIncorrectVersion, "Unsupported client version")
		return
	}

	b := rt.backend

	// 创建房间：返回配置的公网地址，而不是后端自报的地址。
	if path == "/api/games" && r.Method == "PUT" {
		g.serveCreateGame(w, r, b)
		return
	}

	// 其余请求转发给后端，并改写响应中的 IP/Port。
	g.proxy(w, r, b, legacyJoin, isGameLookup)
}

// validGameID 校验房间码：可选负号 + 数字，并且必须能放进 int32。
func validGameID(s string) bool {
	if !gameIDPattern.MatchString(s) {
		return false
	}
	_, err := strconv.ParseInt(s, 10, 32)
	return err == nil
}
