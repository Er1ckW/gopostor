// 本文件负责构造 JSON 响应，以及改写后端返回的房间地址。
//
// 日志不再由响应层记录：这里只关心状态码和响应体。
package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Matchmaker 错误原因码，对应 Among Us / Impostor 的 DisconnectReason。
const (
	reasonGameNotFound     = 3
	reasonIncorrectVersion = 5
	reasonNotAuthorized    = 11
	reasonError            = 17
)

// writeJSON 是网关写 JSON 响应的统一入口。
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// errorBody 构造 Impostor 风格的最小错误体。
func errorBody(isGameLookup bool, reasons ...any) map[string]any {
	list := make([]any, len(reasons))
	for i, r := range reasons {
		list[i] = map[string]any{"Reason": r}
	}
	body := map[string]any{"Errors": list}
	if isGameLookup {
		body["Game"] = nil
	}
	return body
}

// writeErrorResponse 返回带 Message 的错误体。
func writeErrorResponse(w http.ResponseWriter, status, reason int, message string) {
	writeJSON(w, status, map[string]any{"Errors": []any{map[string]any{"Reason": reason, "Message": message}}})
}

// writeGameErrorResponse 根据接口形状决定是否补 "Game": null。
func writeGameErrorResponse(w http.ResponseWriter, isGameLookup bool, status, reason int, message string) {
	body := map[string]any{"Errors": []any{map[string]any{"Reason": reason, "Message": message}}}
	if isGameLookup {
		body["Game"] = nil
	}
	writeJSON(w, status, body)
}

// decodeJSON 把后端响应解析成通用 Go 值。
// UseNumber 保留大整数的原始文本，避免 float64 丢精度。
func decodeJSON(data []byte) (any, error) {
	var payload any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return nil, err
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return nil, fmt.Errorf("invalid trailing JSON")
	}
	return payload, nil
}

// rewrite 根据请求路径，把后端返回的房间地址改写成配置的公网地址。
func rewrite(path string, payload any, b *Backend, legacyJoin bool) error {
	switch {
	case legacyJoin:
		host, ok := payload.(map[string]any)
		if !ok {
			return fmt.Errorf("invalid host")
		}
		host["Ip"] = b.encodedIP
		host["Port"] = b.PublicPort
	case path == "/api/games":
		return rewriteList(payload, b)
	case path == "/api/games/filtered":
		result, ok := payload.(map[string]any)
		if !ok {
			return fmt.Errorf("invalid filtered response")
		}
		return rewriteList(result["games"], b)
	case strings.HasPrefix(path, "/api/games/"):
		result, ok := payload.(map[string]any)
		if !ok {
			return fmt.Errorf("invalid game response")
		}
		if result["Game"] != nil {
			return rewriteListing(result["Game"], b)
		}
	}
	return nil
}

// rewriteList 遍历房间数组，改写每个元素的地址。
func rewriteList(value any, b *Backend) error {
	list, ok := value.([]any)
	if !ok {
		return fmt.Errorf("invalid game list")
	}
	for _, item := range list {
		if err := rewriteListing(item, b); err != nil {
			return err
		}
	}
	return nil
}

// rewriteListing 改写单个房间对象的 IP/Port。
func rewriteListing(value any, b *Backend) error {
	listing, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("invalid listing")
	}
	listing["IP"] = b.encodedIP
	listing["Port"] = b.PublicPort
	return nil
}
