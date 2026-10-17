package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var errRedirect = errors.New("backend redirect refused")

var errInvalidPayload = errors.New("invalid backend payload")

// failureKind classifies why a backend request failed. It is a distinct type so
// hints and logs cannot silently disagree about a string constant.
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

// sendBackendRequest makes one request to a backend and feeds the result into the health monitor.
func (g *gateway) sendBackendRequest(w *requestLog, r *http.Request, b *Backend, method, target string) (*http.Response, []byte, error) {
	start := time.Now()
	defer func() { setLogField(w, "backend_duration_ms", time.Since(start).Milliseconds()) }()
	setLogField(w, "backend_stage", "request_build")
	req, err := http.NewRequestWithContext(r.Context(), method, b.HTTPEndpoint+target, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept", "application/json")
	if auth := r.Header.Get("Authorization"); auth != "" {
		req.Header.Set("Authorization", auth)
	}
	req.Header.Set("X-Request-ID", w.Header().Get("X-Request-ID"))
	setLogField(w, "backend_stage", "request_send")
	response, err := g.client.Do(req)
	if err != nil {
		if r.Context().Err() == nil {
			g.health.record(b, err, time.Since(start), "request")
		} else {
			setLogField(w, "client_canceled", true)
		}
		return nil, nil, err
	}
	defer response.Body.Close()
	setLogField(w, "backend_status", response.StatusCode)
	setLogField(w, "backend_stage", "response_read")
	data, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	elapsed := time.Since(start)
	g.health.record(b, err, elapsed, "request")
	if err != nil {
		return nil, nil, err
	}
	if limit := time.Duration(g.config.BackendTimeoutMS) * time.Millisecond / 2; elapsed > limit {
		issue(w, levelWarn, "backend_slow", hintBackendSlow(b, elapsed, g.config.BackendTimeoutMS))
	}
	return response, data, nil
}

func (g *gateway) backendFailure(w *requestLog, b *Backend, isGameLookup bool, err error) {
	setLogField(w, "error_kind", classifyError(err))
	issue(w, levelError, "backend_unavailable", hintBackendUnavailable(b, err, g.config.BackendTimeoutMS))
	writeGameErrorResponse(w, isGameLookup, 502, reasonError, "Matchmaking backend unavailable")
}

// serveCreateGame hands out the configured address of the backend, after checking that the backend accepts a new game.
func (g *gateway) serveCreateGame(w *requestLog, r *http.Request, b *Backend) {
	if g.config.ProbeBackendOnCreate {
		response, data, err := g.sendBackendRequest(w, r, b, http.MethodPut, "/api/games")
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
				issue(w, levelError, "backend_rejected_create", hintBackendRejectedCreate(b, response.StatusCode))
				setLogField(w, "error_kind", classifyError(err))
				writeGameErrorResponse(w, false, 502, reasonError, "Matchmaking backend unavailable")
				return
			}
			g.backendFailure(w, b, false, err)
			return
		}
		g.checkReportedAddress(w, b, host.Ip, host.Port)
	} else if g.health.isDown(b) {
		issue(w, levelWarn, "backend_marked_down", hintBackendMarkedDown(b))
	}
	setLogField(w, "response_source", "gateway")
	writeJSON(w, 200, map[string]any{"Ip": b.encodedIP, "Port": b.PublicPort})
}

// checkReportedAddress compares the PublicIp/PublicPort a backend reports with the gateway configuration.
func (g *gateway) checkReportedAddress(w *requestLog, b *Backend, ip, port json.Number) {
	reportedIP, ipErr := ip.Int64()
	reportedPort, portErr := port.Int64()
	if ipErr != nil || portErr != nil {
		return
	}
	address := fmt.Sprintf("%d.%d.%d.%d", reportedIP&0xff, reportedIP>>8&0xff, reportedIP>>16&0xff, reportedIP>>24&0xff)
	setLogField(w, "backend_reported_address", fmt.Sprintf("%s:%d", address, reportedPort))
	if uint32(reportedIP) == b.encodedIP && int(reportedPort) == b.PublicPort {
		return
	}
	g.warnOnceThenInfo(w, fmt.Sprintf("address:%s:%s:%d", b.Name, address, reportedPort), "backend_public_address_mismatch",
		hintBackendAddressMismatch(b, address, reportedPort))
}

func (g *gateway) proxy(w *requestLog, r *http.Request, b *Backend, legacyJoin, isGameLookup bool) {
	target := r.URL.EscapedPath()
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	response, data, err := g.sendBackendRequest(w, r, b, r.Method, target)
	if err != nil {
		g.backendFailure(w, b, isGameLookup, err)
		return
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		g.backendError(w, r, b, response.StatusCode, data, legacyJoin, isGameLookup)
		return
	}
	payload, err := decodeJSON(data)
	if err == nil {
		setLogField(w, "backend_stage", "response_rewrite")
		err = rewrite(r.URL.Path, payload, b, legacyJoin)
	}
	if err != nil {
		g.backendFailure(w, b, isGameLookup, fmt.Errorf("%w: %v", errInvalidPayload, err))
		return
	}
	setLogField(w, "backend_stage", "complete")
	setLogField(w, "response_source", "backend")
	writeJSON(w, 200, payload)
}

// backendError answers a non-success backend response. Only the error reasons are passed on: Impostor puts
// exception text into Message. Endpoints an older backend does not have are answered by the gateway.
func (g *gateway) backendError(w *requestLog, r *http.Request, b *Backend, status int, data []byte, legacyJoin, isGameLookup bool) {
	setLogField(w, "backend_stage", "http_error")
	setLogField(w, "backend_body_bytes", len(data))
	reasons, messages := parseBackendErrors(data)
	if len(reasons) > 0 {
		setLogField(w, "backend_reasons", reasons)
		if len(messages) > 0 {
			setLogField(w, "backend_message", strings.Join(messages, " | "))
		}
		if status == 404 && len(reasons) == 1 && reasons[0] == json.Number(strconv.Itoa(reasonGameNotFound)) {
			issue(w, levelInfo, "game_not_found", hintGameNotFound(b))
		} else {
			issue(w, levelWarn, "backend_error", hintBackendError(b, status, reasons, messages))
		}
		setLogField(w, "response_source", "backend")
		writeJSON(w, status, errorBody(isGameLookup, reasons...))
		return
	}
	// ASP.NET answers a route it does not have with an empty 404/405, an HTML page means something else answered.
	if status == 404 || status == 405 {
		endpoint := endpointName(r.URL.Path)
		body, code, known := missingEndpointAnswer(r.URL.Path, legacyJoin, isGameLookup)
		if known || len(bytes.TrimSpace(data)) == 0 {
			if !known {
				body, code = errorBody(isGameLookup, reasonError), status
			}
			g.warnOnceThenInfo(w, "missing:"+b.Name+":"+r.Method+":"+endpoint, "backend_endpoint_missing", hintMissingEndpoint(b, r.Method, endpoint, known))
			setLogField(w, "response_source", "gateway")
			writeJSON(w, code, body)
			return
		}
	}
	issue(w, levelError, "backend_error_unparsed", hintBackendErrorUnparsed(b, status, len(data)))
	if status < 400 {
		status = 502
	}
	writeJSON(w, status, errorBody(isGameLookup, reasonError))
}

// parseBackendErrors extracts only the fields the gateway forwards and logs: Reason values and
// the first line of Message, which is never sent to the client.
func parseBackendErrors(data []byte) (reasons []any, messages []string) {
	payload, err := decodeJSON(data)
	if err != nil {
		return nil, nil
	}
	object, ok := payload.(map[string]any)
	if !ok {
		return nil, nil
	}
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
	return reasons, messages
}

func endpointName(path string) string {
	if strings.HasPrefix(path, "/api/games/") && path != "/api/games/filtered" {
		return "/api/games/{gameId}"
	}
	return path
}

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
