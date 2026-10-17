package gateway

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

type tokenContent struct {
	PUID          string `json:"Puid"`
	ClientVersion int64  `json:"ClientVersion"`
	ExpiresAt     string `json:"ExpiresAt"`
}

type token struct {
	Content tokenContent `json:"Content"`
	Hash    string       `json:"Hash"`
}

func createToken(puid string, version int64) string {
	data, _ := json.Marshal(token{
		Content: tokenContent{PUID: puid, ClientVersion: version, ExpiresAt: "2012-12-21T00:00:00"},
		Hash:    "impostor_was_here",
	})
	return base64.StdEncoding.EncodeToString(data)
}

var bearerPattern = regexp.MustCompile(`(?i)^Bearer\s+([A-Za-z0-9+/]+={0,2})$`)

// parseTokenVersion reads the client version out of the matchmaking token. The problem code says why a token was refused.
// Only the version is required: Impostor itself never checks the Puid or the Hash.
func parseTokenVersion(header string) (int64, string, error) {
	if strings.TrimSpace(header) == "" {
		return 0, "missing_authorization", fmt.Errorf("missing token")
	}
	m := bearerPattern.FindStringSubmatch(strings.TrimSpace(header))
	if m == nil || len(m[1]) > 16384 {
		return 0, "not_bearer_base64", fmt.Errorf("invalid token")
	}
	data, err := base64.StdEncoding.Strict().DecodeString(m[1])
	if err != nil {
		return 0, "invalid_base64", err
	}
	var t struct {
		Content struct{ ClientVersion json.RawMessage }
	}
	if err = json.Unmarshal(data, &t); err != nil {
		return 0, "invalid_json", err
	}
	version, _, ok := flexibleInt(t.Content.ClientVersion)
	if !ok || version < 1 || version > maxVersion {
		return 0, "invalid_client_version", fmt.Errorf("invalid token payload")
	}
	return version, "", nil
}

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

func (g *gateway) serveToken(w *requestLog, r *http.Request) {
	if r.Method != "POST" {
		issue(w, levelInfo, "method_not_allowed", "")
		w.Header().Set("Allow", "POST")
		writeErrorResponse(w, 405, reasonError, "Use POST")
		return
	}
	var user struct {
		PUID          *string `json:"Puid"`
		Username      *string
		Language      json.RawMessage
		ClientVersion json.RawMessage
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 65536))
	if err != nil {
		setLogField(w, "validation_error", "request_body_read_failed")
		setLogField(w, "error_kind", classifyError(err))
		issue(w, levelWarn, "token_request_unreadable", hintTokenRequestUnreadable())
		writeErrorResponse(w, 400, reasonError, "Invalid token request")
		return
	}
	if err = json.Unmarshal(data, &user); err != nil {
		setLogField(w, "validation_error", "invalid_json_or_field_type")
		setLogField(w, "error_kind", classifyError(err))
		// Report only known schema field names, never raw JSON or values.
		if fieldError, ok := err.(*json.UnmarshalTypeError); ok {
			switch fieldError.Field {
			case "Puid", "Username", "Language", "ClientVersion":
				setLogField(w, "invalid_field", fieldError.Field)
			}
		}
		issue(w, levelWarn, "token_request_invalid_json", hintTokenRequestInvalidJSON())
		writeErrorResponse(w, 400, reasonError, "Invalid token request")
		return
	}
	version, quoted, ok := flexibleInt(user.ClientVersion)
	if quoted {
		setLogField(w, "client_version_quoted", true)
	}
	if !ok || version < 1 || version > maxVersion {
		setLogField(w, "validation_error", "missing_or_invalid_client_version")
		setLogField(w, "invalid_fields", []string{"ClientVersion"})
		issue(w, levelWarn, "token_request_invalid_version", hintTokenRequestInvalidVersion())
		writeErrorResponse(w, 400, reasonError, "Invalid token request")
		return
	}
	logVersion(w, version)

	// Older clients do not send Username or Language and some accounts have no name: none of them are needed.
	var missing []string
	if user.PUID == nil || strings.TrimSpace(*user.PUID) == "" {
		missing = append(missing, "Puid")
	}
	if user.Username == nil || strings.TrimSpace(*user.Username) == "" {
		missing = append(missing, "Username")
	}
	if language, _, ok := flexibleInt(user.Language); ok {
		setLogField(w, "language", language)
	} else {
		missing = append(missing, "Language")
	}
	if len(missing) > 0 {
		setLogField(w, "missing_fields", missing)
		issue(w, levelInfo, "token_fields_missing", hintTokenFieldsMissing(missing))
	}
	puid := ""
	if user.PUID != nil {
		puid = *user.PUID
	}
	g.logRoute(w, g.config.route(version))
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, createToken(puid, version))
}
