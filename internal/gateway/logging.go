// 本文件只做一件事：为每个 HTTP 请求输出一行简单的访问日志。
//
// 日志字段固定为：时间、来源、请求方法/路径/查询参数/UA、请求 body、
// 解码后的 Authorization token、响应状态/Content-Type/body、耗时。不再记录 issues、hint、route、
// backend_stage、warn-once、统计汇总等诊断信息。
//
// 注意：请求 body 和响应 body 会原样写入日志。对于 /api/user，响应 body
// 就是 token，请求 body 可能包含 Puid/Username，因此日志文件属于敏感数据，
// 必须限制访问权限。
package gateway

import (
	// bytes 用于保存响应 body，以及在读取请求 body 后重新构造 r.Body。
	"bytes"
	// encoding/json 把访问记录序列化成一行 JSON。
	"encoding/json"
	"errors"
	// io 读取请求 body。
	"io"
	// log 输出最终的一行日志。
	"log"
	// net 拆分 RemoteAddr 的 host:port。
	"net"
	// net/http 提供 ResponseWriter、Request。
	"net/http"
	// net/netip 判断直连对端是否为回环地址。
	"net/netip"
	// strings 处理代理头和 X-Forwarded-For。
	"strings"
	// time 记录时间戳和耗时。
	"time"
)

// accessLog 是每请求唯一的一条日志记录。
type accessLog struct {
	Time                string          `json:"time"`
	ClientIP            string          `json:"client_ip"`
	Peer                string          `json:"peer"`
	Method              string          `json:"method"`
	Path                string          `json:"path"`
	Query               string          `json:"query"`
	UserAgent           string          `json:"user_agent"`
	RequestContentType  string          `json:"request_content_type"`
	RequestBody         string          `json:"request_body"`
	AuthorizationToken  json.RawMessage `json:"authorization_token,omitempty"`
	Status              int             `json:"status"`
	ResponseContentType string          `json:"response_content_type"`
	ResponseBody        string          `json:"response_body"`
	DurationMS          int64           `json:"duration_ms"`
}

// loggingResponseWriter 包装真正的 ResponseWriter，记录状态码和完整响应 body。
type loggingResponseWriter struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

// WriteHeader 只接受第一次写入的状态码，并转发给底层 ResponseWriter。
func (w *loggingResponseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

// Write 把响应同时写给客户端和内存缓冲区。
func (w *loggingResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(p)
	// 只记录真正写出的部分，避免底层写失败时日志与客户端收到的内容不一致。
	w.body.Write(p[:n])
	return n, err
}

// Unwrap 让 http.ResponseController 能拿到底层 ResponseWriter。
func (w *loggingResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

const maxRequestBodyBytes = 64 << 10

// readRequestBody 最多读取 64KB；成功时恢复 body，失败时由调用方拒绝请求。
// 失败请求的日志只保留已读取的部分，避免为了记日志而继续读取超大 body。
func readRequestBody(w http.ResponseWriter, r *http.Request) (string, error) {
	if r.Body == nil {
		return "", nil
	}
	body := http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	data, err := io.ReadAll(body)
	_ = body.Close()
	if err != nil {
		return string(data), err
	}
	r.Body = io.NopCloser(bytes.NewReader(data))
	return string(data), nil
}

// clientIP 返回日志中的 client_ip：优先使用可信代理传来的真实 IP，
// 否则使用 TCP 直连对端的 IP。
func clientIP(r *http.Request) string {
	if forwarded := forwardedClient(r); forwarded != "" {
		return forwarded
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// forwardedClient 读取反向代理设置的真实客户端 IP。
//
// 只有当直连对端是本机回环地址时才信任 X-Real-IP / X-Forwarded-For，
// 否则玩家可以伪造这些头来污染日志。
func forwardedClient(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return ""
	}
	if addr, err := netip.ParseAddr(host); err != nil || !addr.IsLoopback() {
		return ""
	}
	if v := strings.TrimSpace(r.Header.Get("X-Real-IP")); v != "" {
		return v
	}
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		return strings.TrimSpace(strings.Split(v, ",")[0])
	}
	return ""
}

// logRequests 返回包装了访问日志的 http.Handler。
//
// 流程：
//  1. 读取并恢复请求 body；
//  2. 用 loggingResponseWriter 记录响应状态和 body；
//  3. 调用 g.serve 处理业务；
//  4. 请求结束后输出一行 JSON。
func (g *gateway) logRequests() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		requestBody, readErr := readRequestBody(w, r)
		lrw := &loggingResponseWriter{ResponseWriter: w}

		// 即使业务处理 panic，也先写下这条访问日志；panic 随后仍由 net/http 处理。
		defer func() {
			if lrw.status == 0 {
				lrw.status = http.StatusOK
			}
			entry := accessLog{
				Time:                time.Now().Format(time.RFC3339Nano),
				ClientIP:            clientIP(r),
				Peer:                r.RemoteAddr,
				Method:              r.Method,
				Path:                r.URL.Path,
				Query:               r.URL.RawQuery,
				UserAgent:           r.UserAgent(),
				RequestContentType:  r.Header.Get("Content-Type"),
				RequestBody:         requestBody,
				Status:              lrw.status,
				ResponseContentType: lrw.Header().Get("Content-Type"),
				ResponseBody:        lrw.body.String(),
				DurationMS:          time.Since(start).Milliseconds(),
			}
			if payload, err := decodeTokenPayload(r.Header.Get("Authorization")); err == nil {
				entry.AuthorizationToken = payload
			}
			data, _ := json.Marshal(entry)
			log.Print(string(data))
		}()

		if readErr != nil {
			status, message := http.StatusBadRequest, "Invalid request body"
			var tooLarge *http.MaxBytesError
			if errors.As(readErr, &tooLarge) {
				status, message = http.StatusRequestEntityTooLarge, "Request body exceeds 64KB"
			}
			isGameLookup := strings.HasPrefix(r.URL.Path, "/api/games/") && r.URL.Path != "/api/games/filtered"
			writeGameErrorResponse(lrw, isGameLookup, status, reasonError, message)
			return
		}
		g.serve(lrw, r)
	})
}
