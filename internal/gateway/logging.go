package gateway

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

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
		w.fields["write_error"] = classifyError(err)
	}
	return n, err
}

func (w *requestLog) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func setLogField(w *requestLog, key string, value any) {
	w.fields[key] = value
}

// issue records a named problem of the request together with a hint telling the operator what to check.
// Warnings and errors are additionally printed as one readable line next to the JSON record.
func issue(w *requestLog, level int, code, hint string) {
	if !w.leveled || level > w.level {
		w.level = level
	}
	w.leveled = true
	w.issues = append(w.issues, code)
	if level >= levelWarn {
		w.serious = append(w.serious, code)
	}
	if hint != "" {
		w.hints = append(w.hints, hint)
		if level >= levelWarn {
			w.seriousHints = append(w.seriousHints, hint)
		}
	}
}

func (g *gateway) firstTime(key string) bool {
	_, seen := g.shownOnce.LoadOrStore(key, true)
	return !seen
}

// warnOnceThenInfo reports a repeating problem as a warning the first time and as info afterwards.
func (g *gateway) warnOnceThenInfo(w *requestLog, key, code, hint string) {
	if g.firstTime(key) {
		issue(w, levelWarn, code, hint+warnOnceNote)
		return
	}
	issue(w, levelInfo, code, hint)
}

func quiet(w *requestLog) {
	w.quiet = true
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

func (g *gateway) logRequests() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		id := fmt.Sprintf("%x-%d", g.requestEpoch, g.requestSeq.Add(1))
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
			g.stats.record(rw)
			if rw.quiet && rw.status < 400 {
				return
			}
			logEvent(rw.fields)
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
		g.serve(rw, r)
	})
}

func logVersion(w *requestLog, version int64) {
	setLogField(w, "client_version", version)
	setLogField(w, "normalized_version", normalizeVersion(version))
	if version >= minRawVersion && version <= maxVersion {
		setLogField(w, "client_version_text", formatVersion(normalizeVersion(version)))
		if normalizeVersion(version) != version {
			setLogField(w, "disable_server_authority_flag", true)
		}
	}
}

func logBackend(w *requestLog, b *Backend) {
	setLogField(w, "backend", b.Name)
	setLogField(w, "backend_endpoint", b.HTTPEndpoint)
	setLogField(w, "public_ip", b.PublicIP)
	setLogField(w, "public_port", b.PublicPort)
}

func (g *gateway) logRoute(w *requestLog, r routeResult) {
	setLogField(w, "route", r.kind)
	if r.backend == nil {
		return
	}
	logBackend(w, r.backend)
	if r.kind != routeExact {
		setLogField(w, "backend_range", r.backend.rangeText())
		g.warnOnceThenInfo(w, "route:"+r.kind+":"+versionText(r.normalized), "version_"+r.kind, hintRoute(g.config, r))
	}
}

// logEvent writes one JSON object with a timestamp field. Run disables the standard
// log prefix, so collectors can parse every line as JSON.
func logEvent(fields map[string]any) {
	fields["time"] = time.Now().Format(time.RFC3339Nano)
	data, _ := json.Marshal(fields)
	log.Print(string(data))
}
