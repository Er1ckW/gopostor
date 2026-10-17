package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// send makes one request to a backend and feeds the result into the health monitor.
func (g *gateway) send(w http.ResponseWriter, r *http.Request, b *Backend, method, target string) (*http.Response, []byte, error) {
	start := time.Now()
	defer func() { diagnostic(w, "backend_duration_ms", time.Since(start).Milliseconds()) }()
	diagnostic(w, "backend_stage", "request_build")
	req, err := http.NewRequestWithContext(r.Context(), method, b.HTTPEndpoint+target, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept", "application/json")
	if auth := r.Header.Get("Authorization"); auth != "" {
		req.Header.Set("Authorization", auth)
	}
	req.Header.Set("X-Request-ID", w.Header().Get("X-Request-ID"))
	diagnostic(w, "backend_stage", "request_send")
	response, err := g.client.Do(req)
	if err != nil {
		if r.Context().Err() == nil {
			g.health.record(b, err, time.Since(start), "request")
		} else {
			diagnostic(w, "client_canceled", true)
		}
		return nil, nil, err
	}
	defer response.Body.Close()
	diagnostic(w, "backend_status", response.StatusCode)
	diagnostic(w, "backend_stage", "response_read")
	data, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	elapsed := time.Since(start)
	g.health.record(b, err, elapsed, "request")
	if err != nil {
		return nil, nil, err
	}
	if limit := time.Duration(g.config.BackendTimeoutMS) * time.Millisecond / 2; elapsed > limit {
		issue(w, levelWarn, "backend_slow", fmt.Sprintf("后端 %s 用时 %dms，超过超时时间 %dms 的一半；持续出现时检查后端负载和网络", b.Name, elapsed.Milliseconds(), g.config.BackendTimeoutMS))
	}
	return response, data, nil
}

func (g *gateway) backendFailure(w http.ResponseWriter, b *Backend, byCode bool, err error) {
	diagnostic(w, "error_kind", errorKind(err))
	issue(w, levelError, "backend_unavailable", backendHint(b, err, g.config.BackendTimeoutMS))
	gameFailure(w, byCode, 502, reasonError, "Matchmaking backend unavailable")
}

// serveCreate hands out the configured address of the backend, after checking that the backend accepts a new game.
func (g *gateway) serveCreate(w http.ResponseWriter, r *http.Request, b *Backend) {
	if g.config.ProbeBackendOnCreate {
		response, data, err := g.send(w, r, b, http.MethodPut, "/api/games")
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
				issue(w, levelError, "backend_rejected_create", fmt.Sprintf("后端 %s 对创建房间返回 HTTP %d 或无效内容，玩家无法创建房间：检查后端日志", b.Name, response.StatusCode))
				diagnostic(w, "error_kind", errorKind(err))
				gameFailure(w, false, 502, reasonError, "Matchmaking backend unavailable")
				return
			}
			g.backendFailure(w, b, false, err)
			return
		}
		g.checkReportedAddress(w, b, host.Ip, host.Port)
	} else if g.health.isDown(b) {
		issue(w, levelWarn, "backend_marked_down", fmt.Sprintf("后端 %s 最近一次检查不可达，但 probeBackendOnCreate=false，仍返回其地址；玩家可能在连接 UDP 时超时", b.Name))
	}
	diagnostic(w, "response_source", "gateway")
	writeJSON(w, 200, map[string]any{"Ip": b.ip, "Port": b.PublicPort})
}

// checkReportedAddress compares the PublicIp/PublicPort a backend reports with the gateway configuration.
func (g *gateway) checkReportedAddress(w http.ResponseWriter, b *Backend, ip, port json.Number) {
	reportedIP, ipErr := ip.Int64()
	reportedPort, portErr := port.Int64()
	if ipErr != nil || portErr != nil {
		return
	}
	address := fmt.Sprintf("%d.%d.%d.%d", reportedIP&0xff, reportedIP>>8&0xff, reportedIP>>16&0xff, reportedIP>>24&0xff)
	diagnostic(w, "backend_reported_address", fmt.Sprintf("%s:%d", address, reportedPort))
	if uint32(reportedIP) == b.ip && int(reportedPort) == b.PublicPort {
		return
	}
	issueOnce(w, fmt.Sprintf("address:%s:%s:%d", b.Name, address, reportedPort), "backend_public_address_mismatch",
		fmt.Sprintf("后端 %s 自报的公网地址 %s:%d 与网关 config.json 的 %s:%d 不一致；网关始终返回自己的配置，请确认玩家实际能用 %s:%d 连到这个后端的 UDP，并让两边保持一致",
			b.Name, address, reportedPort, b.PublicIP, b.PublicPort, b.PublicIP, b.PublicPort))
}

func (g *gateway) proxy(w http.ResponseWriter, r *http.Request, b *Backend, legacy, byCode bool) {
	target := r.URL.EscapedPath()
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	response, data, err := g.send(w, r, b, r.Method, target)
	if err != nil {
		g.backendFailure(w, b, byCode, err)
		return
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		g.backendError(w, r, b, response.StatusCode, data, legacy, byCode)
		return
	}
	payload, err := decodeJSON(data)
	if err == nil {
		diagnostic(w, "backend_stage", "response_rewrite")
		err = rewrite(r.URL.Path, payload, b, legacy)
	}
	if err != nil {
		g.backendFailure(w, b, byCode, fmt.Errorf("%w: %v", errInvalidPayload, err))
		return
	}
	diagnostic(w, "backend_stage", "complete")
	diagnostic(w, "response_source", "backend")
	writeJSON(w, 200, payload)
}

// backendError answers a non-success backend response. Only the error reasons are passed on: Impostor puts
// exception text into Message. Endpoints an older backend does not have are answered by the gateway.
func (g *gateway) backendError(w http.ResponseWriter, r *http.Request, b *Backend, status int, data []byte, legacy, byCode bool) {
	diagnostic(w, "backend_stage", "http_error")
	diagnostic(w, "backend_body_bytes", len(data))
	var reasons []any
	var messages []string
	if payload, err := decodeJSON(data); err == nil {
		if object, ok := payload.(map[string]any); ok {
			list, _ := object["Errors"].([]any)
			for _, item := range list {
				entry, _ := item.(map[string]any)
				if reason, ok := entry["Reason"].(json.Number); ok {
					reasons = append(reasons, reason)
				}
				if message, ok := entry["Message"].(string); ok && message != "" {
					messages = append(messages, truncate(message, 160))
				}
			}
		}
	}
	if len(reasons) > 0 {
		diagnostic(w, "backend_reasons", reasons)
		if len(messages) > 0 {
			diagnostic(w, "backend_message", strings.Join(messages, " | "))
		}
		if status == 404 && len(reasons) == 1 && reasons[0] == json.Number(strconv.Itoa(reasonGameNotFound)) {
			issue(w, levelInfo, "game_not_found", fmt.Sprintf("后端 %s 上没有这个房间：房间已关闭、房间码输错，或房主属于另一个版本范围的后端", b.Name))
		} else {
			issue(w, levelWarn, "backend_error", fmt.Sprintf("后端 %s 返回 HTTP %d（Reason %v）：%s", b.Name, status, reasons, strings.Join(messages, " | ")))
		}
		diagnostic(w, "response_source", "backend")
		writeJSON(w, status, errorBody(byCode, reasons...))
		return
	}
	// ASP.NET answers a route it does not have with an empty 404/405, an HTML page means something else answered.
	if status == 404 || status == 405 {
		body, code, known := missingEndpointAnswer(r.URL.Path, legacy, byCode)
		if known || len(bytes.TrimSpace(data)) == 0 {
			hint := fmt.Sprintf("后端 %s 没有 %s %s 接口（Impostor 1.9.x 不支持 16.0 及之后客户端使用的接口），网关已代为返回空结果或房间不存在", b.Name, r.Method, endpointName(r.URL.Path))
			if !known {
				body, code = errorBody(byCode, reasonError), status
				hint = fmt.Sprintf("后端 %s 没有 %s %s 接口（Impostor 1.9.3 和 1.10.9 都没有实现它），网关照旧返回 404（Reason 17），与之前的版本行为相同", b.Name, r.Method, endpointName(r.URL.Path))
			}
			issueOnce(w, "missing:"+b.Name+":"+r.Method+":"+endpointName(r.URL.Path), "backend_endpoint_missing", hint)
			diagnostic(w, "response_source", "gateway")
			writeJSON(w, code, body)
			return
		}
	}
	issue(w, levelError, "backend_error_unparsed", fmt.Sprintf("后端 %s 返回 HTTP %d，响应不是 Impostor 的错误格式（%d 字节）：可能是后端异常，或 httpEndpoint 指向了 Nginx/其他服务", b.Name, status, len(data)))
	if status < 400 {
		status = 502
	}
	writeJSON(w, status, errorBody(byCode, reasonError))
}

func endpointName(path string) string {
	if strings.HasPrefix(path, "/api/games/") && path != "/api/games/filtered" {
		return "/api/games/{gameId}"
	}
	return path
}

func missingEndpointAnswer(path string, legacy, byCode bool) (any, int, bool) {
	switch {
	case byCode:
		return errorBody(true, reasonGameNotFound), 404, true
	case legacy:
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
