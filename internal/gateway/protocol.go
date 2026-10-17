package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// Matchmaker error reasons (DisconnectReason values used by Impostor and Impostor.Http).
const (
	reasonGameNotFound     = 3
	reasonIncorrectVersion = 5
	reasonNotAuthorized    = 11
	reasonError            = 17
)

var gameIDPattern = regexp.MustCompile(`^-?\d+$`)

// flexibleInt accepts a JSON number or a number in a string, like ASP.NET Core does for Impostor.
func flexibleInt(raw json.RawMessage) (value int64, quoted, ok bool) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return 0, false, false
	}
	if strings.HasPrefix(s, `"`) {
		if json.Unmarshal(raw, &s) != nil {
			return 0, true, false
		}
		s, quoted = strings.TrimSpace(s), true
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, quoted, err == nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func errorBody(byCode bool, reasons ...any) map[string]any {
	list := make([]any, len(reasons))
	for i, r := range reasons {
		list[i] = map[string]any{"Reason": r}
	}
	body := map[string]any{"Errors": list}
	if byCode {
		body["Game"] = nil
	}
	return body
}

func failure(w http.ResponseWriter, status, reason int, message string) {
	diagnostic(w, "reason", reason)
	diagnostic(w, "message", message)
	writeJSON(w, status, map[string]any{"Errors": []any{map[string]any{"Reason": reason, "Message": message}}})
}

// gameFailure answers with the shape the requested endpoint uses, GET /api/games/{id} also carries "Game": null.
func gameFailure(w http.ResponseWriter, byCode bool, status, reason int, message string) {
	diagnostic(w, "reason", reason)
	diagnostic(w, "message", message)
	body := errorBody(byCode, reason)
	body["Errors"].([]any)[0].(map[string]any)["Message"] = message
	writeJSON(w, status, body)
}

func validGameID(s string) bool {
	if !gameIDPattern.MatchString(s) {
		return false
	}
	_, err := strconv.ParseInt(s, 10, 32)
	return err == nil
}

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

func rewrite(path string, payload any, b *Backend, legacy bool) error {
	switch {
	case legacy:
		host, ok := payload.(map[string]any)
		if !ok {
			return fmt.Errorf("invalid host")
		}
		host["Ip"] = b.ip
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

func rewriteListing(value any, b *Backend) error {
	listing, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("invalid listing")
	}
	listing["IP"] = b.ip
	listing["Port"] = b.PublicPort
	return nil
}
