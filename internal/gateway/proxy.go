// 本文件负责“网关 -> Impostor 后端”的 HTTP 交互：
// 发送请求、改写地址、转换错误，以及对老后端缺失的接口代答。
//
// 日志已经移出业务逻辑：这里只返回响应，不再记录 backend_stage、
// error_kind、issue 等诊断字段。
package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// 两个哨兵错误：调用方用 errors.Is 判断具体失败原因。
var errRedirect = errors.New("backend redirect refused")

var errInvalidPayload = errors.New("invalid backend payload")

// failureKind 是健康状态里保存的失败分类。
type failureKind string

const (
	kindRedirect          failureKind = "redirect_refused"
	kindInvalidPayload    failureKind = "invalid_payload"
	kindTimeout           failureKind = "timeout"
	kindDNS               failureKind = "dns_error"
	kindConnectionRefused failureKind = "connection_refused"
	kindNetworkDial       failureKind = "network_dial"
	kindInvalidJSON       failureKind = "invalid_json"
)

// classifyError 把任意 error 归类成稳定的 failureKind。
func classifyError(err error) failureKind {
	if errors.Is(err, errRedirect) {
		return kindRedirect
	}
	if errors.Is(err, errInvalidPayload) {
		return kindInvalidPayload
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return kindTimeout
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return kindDNS
	}
	var op *net.OpError
	if errors.As(err, &op) {
		if strings.Contains(strings.ToLower(op.Err.Error()), "refused") {
			return kindConnectionRefused
		}
		return failureKind("network_" + op.Op)
	}
	var syntax *json.SyntaxError
	if errors.As(err, &syntax) {
		return kindInvalidJSON
	}
	return failureKind(fmt.Sprintf("%T", err))
}

// sendBackendRequest 向后端发送一次请求，并把结果同步给健康检查器。
func (g *gateway) sendBackendRequest(r *http.Request, b *Backend, method, target string) (*http.Response, []byte, error) {
	start := time.Now()
	req, err := http.NewRequestWithContext(r.Context(), method, b.HTTPEndpoint+target, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept", "application/json")
	if auth := r.Header.Get("Authorization"); auth != "" {
		req.Header.Set("Authorization", auth)
	}

	response, err := g.client.Do(req)
	if err != nil {
		// 玩家主动断开时不把失败记到后端健康状态上。
		if r.Context().Err() == nil {
			g.health.record(b, err, time.Since(start))
		}
		return nil, nil, err
	}
	defer response.Body.Close()

	data, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	g.health.record(b, err, time.Since(start))
	if err != nil {
		return nil, nil, err
	}
	return response, data, nil
}

// backendFailure 对客户端统一返回 502。
func (g *gateway) backendFailure(w http.ResponseWriter, isGameLookup bool) {
	writeGameErrorResponse(w, isGameLookup, 502, reasonError, "Matchmaking backend unavailable")
}

// serveCreateGame 处理 PUT /api/games（创建房间）。
// 网关返回配置中的公网地址，而不是后端自报的地址。
func (g *gateway) serveCreateGame(w http.ResponseWriter, r *http.Request, b *Backend) {
	if g.config.ProbeBackendOnCreate {
		response, data, err := g.sendBackendRequest(r, b, http.MethodPut, "/api/games")
		if err == nil && (response.StatusCode < 200 || response.StatusCode >= 300) {
			err = fmt.Errorf("%w: status %d", errInvalidPayload, response.StatusCode)
		}
		var host struct{ Ip, Port json.Number }
		if err == nil {
			decoder := json.NewDecoder(bytes.NewReader(data))
			decoder.UseNumber()
			if decodeErr := decoder.Decode(&host); decodeErr != nil || host.Ip == "" || host.Port == "" {
				err = fmt.Errorf("%w: host server", errInvalidPayload)
			}
		}
		if err != nil {
			if response != nil {
				writeGameErrorResponse(w, false, 502, reasonError, "Matchmaking backend unavailable")
				return
			}
			g.backendFailure(w, false)
			return
		}
	}

	writeJSON(w, 200, map[string]any{"Ip": b.encodedIP, "Port": b.PublicPort})
}

// proxy 把查询类请求转发给后端，并改写成功响应里的房间地址。
func (g *gateway) proxy(w http.ResponseWriter, r *http.Request, b *Backend, legacyJoin, isGameLookup bool) {
	target := r.URL.EscapedPath()
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}

	response, data, err := g.sendBackendRequest(r, b, r.Method, target)
	if err != nil {
		g.backendFailure(w, isGameLookup)
		return
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		g.backendError(w, r, response.StatusCode, data, legacyJoin, isGameLookup)
		return
	}

	payload, err := decodeJSON(data)
	if err == nil {
		err = rewrite(r.URL.Path, payload, b, legacyJoin)
	}
	if err != nil {
		g.backendFailure(w, isGameLookup)
		return
	}
	writeJSON(w, 200, payload)
}

// backendError 处理后端返回的非 2xx 响应。
// 只把 Errors[].Reason 转给客户端，丢弃 Impostor 的异常文本。
func (g *gateway) backendError(w http.ResponseWriter, r *http.Request, status int, data []byte, legacyJoin, isGameLookup bool) {
	reasons := parseBackendErrors(data)
	if len(reasons) > 0 {
		writeJSON(w, status, errorBody(isGameLookup, reasons...))
		return
	}

	// 老版本 Impostor 对不存在的路由返回空 404/405。
	if status == 404 || status == 405 {
		body, code, known := missingEndpointAnswer(r.URL.Path, legacyJoin, isGameLookup)
		if known || len(bytes.TrimSpace(data)) == 0 {
			if !known {
				body, code = errorBody(isGameLookup, reasonError), status
			}
			writeJSON(w, code, body)
			return
		}
	}

	if status < 400 {
		status = 502
	}
	writeJSON(w, status, errorBody(isGameLookup, reasonError))
}

// parseBackendErrors 只提取 Errors[].Reason，忽略 Message。
func parseBackendErrors(data []byte) []any {
	payload, err := decodeJSON(data)
	if err != nil {
		return nil
	}
	object, ok := payload.(map[string]any)
	if !ok {
		return nil
	}
	list, _ := object["Errors"].([]any)
	var reasons []any
	for _, item := range list {
		entry, _ := item.(map[string]any)
		if reason, ok := entry["Reason"].(json.Number); ok {
			reasons = append(reasons, reason)
		}
	}
	return reasons
}

// missingEndpointAnswer 决定“后端没有这个接口”时网关如何代答。
func missingEndpointAnswer(path string, legacyJoin, isGameLookup bool) (any, int, bool) {
	switch {
	case isGameLookup:
		return errorBody(true, reasonGameNotFound), 404, true
	case legacyJoin:
		return errorBody(false, reasonGameNotFound), 404, true
	case path == "/api/games":
		return []any{}, 200, true
	case path == "/api/games/filtered":
		return map[string]any{"games": []any{}, "metadata": map[string]any{"allGamesCount": 0, "matchingGamesCount": 0}}, 200, true
	case path == "/api/filters":
		return map[string]any{"filters": []any{}}, 200, true
	}
	return nil, 0, false
}
