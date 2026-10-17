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
	ClientVersion int64
	ExpiresAt     string
}

type token struct {
	Content tokenContent
	Hash    string
}

func createToken(puid string, version int64) string {
	data, _ := json.Marshal(token{tokenContent{puid, version, "2012-12-21T00:00:00"}, "impostor_was_here"})
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

func tokenHint(problem string) string {
	switch problem {
	case "missing_authorization":
		return "请求没有 Authorization 头：Nginx 可能未转发它（需要 proxy_set_header Authorization $http_authorization），或客户端没有先成功调用 /api/user"
	case "invalid_client_version":
		return "token 中没有有效的 ClientVersion：token 不是 Impostor/本网关签发的"
	default:
		return "token 格式不是 Impostor/本网关签发的 Base64 JSON：客户端可能沿用了官方服务器或其他私服的 token，让玩家重启游戏后重新选择该区域"
	}
}

func (g *gateway) serveToken(w http.ResponseWriter, r *http.Request) {
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
		setLogField(w, "error_kind", errorKind(err))
		issue(w, levelWarn, "token_request_unreadable", "读取 /api/user 请求体失败或超过 64KB：客户端中途断开，或代理截断了请求")
		writeErrorResponse(w, 400, reasonError, "Invalid token request")
		return
	}
	if err = json.Unmarshal(data, &user); err != nil {
		setLogField(w, "validation_error", "invalid_json_or_field_type")
		setLogField(w, "error_kind", errorKind(err))
		// Report only known schema field names, never raw JSON or values.
		if fieldError, ok := err.(*json.UnmarshalTypeError); ok {
			switch fieldError.Field {
			case "Puid", "Username", "Language", "ClientVersion":
				setLogField(w, "invalid_field", fieldError.Field)
			}
		}
		issue(w, levelWarn, "token_request_invalid_json", "/api/user 请求体不是预期的 JSON：确认请求来自 Among Us 客户端，且 Nginx 没有改写请求体")
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
		issue(w, levelWarn, "token_request_invalid_version", "/api/user 缺少有效的 ClientVersion，无法确定后端：请求很可能不是来自 Among Us 客户端")
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
		issue(w, levelInfo, "token_fields_missing", "缺少 "+strings.Join(missing, "/")+"，通常是旧版客户端或无名称账号；网关照常签发 token")
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
