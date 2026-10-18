// 本文件负责 Among Us 的 /api/user 登录接口，以及 token 的签发和解析。
//
// 日志已经移出业务逻辑：serveToken 只负责解析请求、生成 token、写响应，
// 不再记录字段、问题代码或提示文案。
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

// tokenContent 是 token 的 Content 部分，字段名和 JSON 标签必须与 Impostor 一致。
type tokenContent struct {
	PUID          string `json:"Puid"`
	ClientVersion int64  `json:"ClientVersion"`
	ExpiresAt     string `json:"ExpiresAt"`
}

// token 是完整的 token 结构。
type token struct {
	Content tokenContent `json:"Content"`
	Hash    string       `json:"Hash"`
}

// createToken 按 Impostor 的格式签发 token：Base64(JSON)。
// ExpiresAt 和 Hash 使用固定值，保持与 Impostor 的兼容性。
func createToken(puid string, version int64) string {
	data, _ := json.Marshal(token{
		Content: tokenContent{PUID: puid, ClientVersion: version, ExpiresAt: "2012-12-21T00:00:00"},
		Hash:    "impostor_was_here",
	})
	return base64.StdEncoding.EncodeToString(data)
}

// bearerPattern 匹配 "Bearer <base64>"。
var bearerPattern = regexp.MustCompile(`(?i)^Bearer\s+([A-Za-z0-9+/]+={0,2})$`)

// parseTokenVersion 从 Authorization 头中读出客户端版本。
// 只要求 ClientVersion 合法；不校验 Puid 和 Hash。
func parseTokenVersion(header string) (int64, error) {
	if strings.TrimSpace(header) == "" {
		return 0, fmt.Errorf("missing token")
	}
	m := bearerPattern.FindStringSubmatch(strings.TrimSpace(header))
	if m == nil || len(m[1]) > 16384 {
		return 0, fmt.Errorf("invalid token")
	}
	data, err := base64.StdEncoding.Strict().DecodeString(m[1])
	if err != nil {
		return 0, err
	}
	var t struct {
		Content struct{ ClientVersion json.RawMessage }
	}
	if err = json.Unmarshal(data, &t); err != nil {
		return 0, err
	}
	version, ok := flexibleInt(t.Content.ClientVersion)
	if !ok || version < 1 || version > maxVersion {
		return 0, fmt.Errorf("invalid token payload")
	}
	return version, nil
}

// flexibleInt 接受 JSON 数字或数字字符串，返回解析出的整数和是否成功。
func flexibleInt(raw json.RawMessage) (int64, bool) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return 0, false
	}
	if strings.HasPrefix(s, `"`) {
		if json.Unmarshal(raw, &s) != nil {
			return 0, false
		}
		s = strings.TrimSpace(s)
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil
}

// serveToken 处理 POST /api/user：校验请求体、签发 token。
//
// Puid/Username 缺失也照常签发 token，与 Impostor 保持一致。
func (g *gateway) serveToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		w.Header().Set("Allow", "POST")
		writeErrorResponse(w, 405, reasonError, "Use POST")
		return
	}

	// Username 字段本身不再使用，但保留在这里是为了保持原有的类型校验：
	// 如果客户端把 Username 传成数字，json.Unmarshal 仍会返回类型错误。
	var user struct {
		PUID          *string `json:"Puid"`
		Username      *string
		ClientVersion json.RawMessage
	}

	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 65536))
	if err != nil {
		writeErrorResponse(w, 400, reasonError, "Invalid token request")
		return
	}
	if err = json.Unmarshal(data, &user); err != nil {
		writeErrorResponse(w, 400, reasonError, "Invalid token request")
		return
	}

	version, ok := flexibleInt(user.ClientVersion)
	if !ok || version < 1 || version > maxVersion {
		writeErrorResponse(w, 400, reasonError, "Invalid token request")
		return
	}

	puid := ""
	if user.PUID != nil {
		puid = *user.PUID
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, createToken(puid, version))
}
