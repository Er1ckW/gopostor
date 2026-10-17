package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var requestSequence atomic.Uint64
var requestEpoch = time.Now().UnixNano()

const (
	levelInfo = iota
	levelWarn
	levelError
)

var levelNames = [...]string{"info", "warn", "error"}

type requestLog struct {
	http.ResponseWriter
	fields  map[string]any
	status  int
	bytes   int
	level   int
	leveled bool
	issues  []string
	hints   []string
	// serious and seriousHints only hold warnings and errors, they make up the readable line.
	serious      []string
	seriousHints []string
	quiet        bool
}

func (w *requestLog) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *requestLog) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += n
	if err != nil {
		w.fields["write_error"] = errorKind(err)
	}
	return n, err
}

func (w *requestLog) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func diagnostic(w http.ResponseWriter, key string, value any) {
	if rw, ok := w.(*requestLog); ok {
		rw.fields[key] = value
	}
}

// issue records a named problem of the request together with a hint telling the operator what to check.
// Warnings and errors are additionally printed as one readable line next to the JSON record.
func issue(w http.ResponseWriter, level int, code, hint string) {
	rw, ok := w.(*requestLog)
	if !ok {
		return
	}
	if !rw.leveled || level > rw.level {
		rw.level = level
	}
	rw.leveled = true
	rw.issues = append(rw.issues, code)
	if level >= levelWarn {
		rw.serious = append(rw.serious, code)
	}
	if hint != "" {
		rw.hints = append(rw.hints, hint)
		if level >= levelWarn {
			rw.seriousHints = append(rw.seriousHints, hint)
		}
	}
}

var shownOnce sync.Map

func firstTime(key string) bool {
	_, seen := shownOnce.LoadOrStore(key, true)
	return !seen
}

// issueOnce reports a repeating problem as a warning the first time and as info afterwards.
func issueOnce(w http.ResponseWriter, key, code, hint string) {
	if firstTime(key) {
		issue(w, levelWarn, code, hint+"（同类问题之后只记为 info）")
		return
	}
	issue(w, levelInfo, code, hint)
}

func quiet(w http.ResponseWriter) {
	if rw, ok := w.(*requestLog); ok {
		rw.quiet = true
	}
}

var errRedirect = errors.New("backend redirect refused")
var errInvalidPayload = errors.New("invalid backend payload")

func errorKind(err error) string {
	if errors.Is(err, errRedirect) {
		return "redirect_refused"
	}
	if errors.Is(err, errInvalidPayload) {
		return "invalid_payload"
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return "timeout"
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "dns_error"
	}
	var op *net.OpError
	if errors.As(err, &op) {
		if strings.Contains(strings.ToLower(op.Err.Error()), "refused") {
			return "connection_refused"
		}
		return "network_" + op.Op
	}
	var syntax *json.SyntaxError
	if errors.As(err, &syntax) {
		return "invalid_json"
	}
	return fmt.Sprintf("%T", err)
}

func backendHint(b *Backend, err error, timeoutMS int) string {
	prefix := fmt.Sprintf("后端 %s（%s）", b.Name, b.HTTPEndpoint)
	switch kind := errorKind(err); {
	case kind == "connection_refused" || kind == "network_dial":
		return prefix + " 拒绝连接：检查 Impostor 是否在运行、config.json 的 HttpServer.Enabled 是否为 true、HttpServer.ListenIp/ListenPort 是否与 httpEndpoint 一致"
	case kind == "timeout":
		return fmt.Sprintf("%s 在 %dms 内没有响应：后端可能卡死或过载、防火墙丢弃了连接，或 backendTimeoutMs 太小", prefix, timeoutMS)
	case kind == "dns_error":
		return prefix + " 的主机名无法解析：检查 httpEndpoint 拼写或 DNS"
	case kind == "redirect_refused":
		return prefix + " 返回了重定向：httpEndpoint 可能指向了 Nginx 的 HTTPS 跳转或其他站点，应直接指向 Impostor 的内部 HTTP 端口"
	case kind == "invalid_payload" || kind == "invalid_json":
		return prefix + " 返回的内容不是预期的 JSON：httpEndpoint 可能指向了其他服务，或后端版本与网关不匹配"
	default:
		return fmt.Sprintf("%s 请求失败（%s）", prefix, kind)
	}
}

func truncate(s string, n int) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// forwardedClient reads the client address set by a reverse proxy, only when the direct peer is local.
func forwardedClient(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return ""
	}
	if addr, err := netip.ParseAddr(host); err != nil || !addr.IsLoopback() {
		return ""
	}
	if v := strings.TrimSpace(r.Header.Get("X-Real-IP")); v != "" {
		return truncate(v, 64)
	}
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		return truncate(strings.TrimSpace(strings.Split(v, ",")[0]), 64)
	}
	return ""
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		id := fmt.Sprintf("%x-%d", requestEpoch, requestSequence.Add(1))
		w.Header().Set("X-Request-ID", id)
		rw := &requestLog{ResponseWriter: w, fields: map[string]any{
			"event": "http_request", "request_id": id, "method": r.Method,
			"path": r.URL.Path, "peer": r.RemoteAddr, "content_type": r.Header.Get("Content-Type"),
			"content_length": r.ContentLength, "authorization_present": r.Header.Get("Authorization") != "",
			"response_source": "gateway", "host": truncate(r.Host, 100), "user_agent": truncate(r.UserAgent(), 160),
		}}
		if client := forwardedClient(r); client != "" {
			rw.fields["forwarded_client"] = client
		}
		defer func() {
			if rw.status == 0 {
				rw.status = 200
			}
			if !rw.leveled {
				switch {
				case rw.status >= 500:
					rw.level = levelError
				case rw.status >= 400:
					rw.level = levelWarn
				}
			}
			rw.fields["status"] = rw.status
			rw.fields["level"] = levelNames[rw.level]
			rw.fields["response_bytes"] = rw.bytes
			rw.fields["duration_ms"] = time.Since(start).Milliseconds()
			if len(rw.issues) > 0 {
				rw.fields["issues"] = rw.issues
			}
			if len(rw.hints) > 0 {
				rw.fields["hint"] = strings.Join(rw.hints, "；")
			}
			stats.record(rw)
			if rw.quiet && rw.status < 400 {
				return
			}
			data, _ := json.Marshal(rw.fields)
			log.Print(string(data))
			if rw.level >= levelWarn {
				hint := strings.Join(rw.seriousHints, "；")
				if hint == "" {
					hint = "详见同一 request_id 的 JSON 记录"
				}
				codes := strings.Join(rw.serious, ",")
				if codes == "" {
					codes = "http_" + fmt.Sprint(rw.status)
				}
				log.Printf("[%s] %s %s %s -> %d %s: %s", strings.ToUpper(levelNames[rw.level]), id, r.Method, r.URL.Path, rw.status, codes, hint)
			}
		}()
		next.ServeHTTP(rw, r)
	})
}

func logVersion(w http.ResponseWriter, version int64) {
	diagnostic(w, "client_version", version)
	diagnostic(w, "normalized_version", normalizeVersion(version))
	if version >= minRawVersion && version <= maxVersion {
		diagnostic(w, "client_version_text", formatVersion(normalizeVersion(version)))
		if normalizeVersion(version) != version {
			diagnostic(w, "disable_server_authority_flag", true)
		}
	}
}

func logBackend(w http.ResponseWriter, b *Backend) {
	diagnostic(w, "backend", b.Name)
	diagnostic(w, "backend_endpoint", b.HTTPEndpoint)
	diagnostic(w, "public_ip", b.PublicIP)
	diagnostic(w, "public_port", b.PublicPort)
}
