// 本文件负责访问日志的脱敏：不带 -debug 启动时，日志里不出现完整的玩家标识、
// 玩家名字和 token。
//
// 规则：
//   - JSON body：递归查找敏感字段（不区分大小写），Puid 这类 ID 只保留前 4 个字符，
//     Username/HostName 这类名字整段替换为 "***"，其余字段原样保留；
//   - 非 JSON body（例如 /api/user 返回的 token 文本）：只记录长度，不记录内容；
//   - 超过 maxRedactedBodyBytes 的 body：只记录长度，避免大房间列表占用内存和磁盘；
//   - Authorization token：解码后同样按 JSON 规则脱敏，保留 ClientVersion 便于排查路由。
//
// 带 -debug 启动时不会调用这里的函数，日志记录完整原始数据。
package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// maxRedactedBodyBytes 是非 debug 模式下单个 body 最多记录（和缓存）的字节数。
const maxRedactedBodyBytes = 16 << 10

// redactedIDKeys 是玩家 ID 类字段：保留前 4 个字符，方便在多行日志之间对照同一个玩家。
var redactedIDKeys = map[string]bool{"puid": true, "productuserid": true}

// redactedNameKeys 是玩家名字类字段：整段隐藏。
var redactedNameKeys = map[string]bool{"username": true, "hostname": true, "truehostname": true, "playername": true, "friendcode": true}

// redactBody 返回写入日志的脱敏 body。size 是 body 的真实总长度，
// data 可能只是其中的前一部分（响应 body 只缓存前 maxRedactedBodyBytes 字节）。
func redactBody(data []byte, size int) string {
	if size == 0 {
		return ""
	}
	if size > maxRedactedBodyBytes || len(data) < size {
		return fmt.Sprintf("[omitted: %d bytes]", size)
	}
	if redacted, ok := redactJSON(data); ok {
		return string(redacted)
	}
	return fmt.Sprintf("[redacted: %d bytes non-JSON]", size)
}

// redactJSON 把 data 当作 JSON 解析并脱敏；不是合法 JSON 时返回 false。
// UseNumber 保证大整数（例如超过 2^53 的字段）不会因为转成 float64 而失真。
func redactJSON(data []byte) ([]byte, bool) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return nil, false
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return nil, false
	}
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	// 不把 < > & 转义成 < 之类，日志更易读。
	encoder.SetEscapeHTML(false)
	if encoder.Encode(redactValue(value)) != nil {
		return nil, false
	}
	return bytes.TrimRight(out.Bytes(), "\n"), true
}

// redactValue 递归遍历 JSON 值，替换敏感字段。
// 注意：重新编码后对象的字段按字母顺序输出，与原始顺序可能不同。
func redactValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		for key, item := range v {
			switch lower := strings.ToLower(key); {
			case redactedIDKeys[lower]:
				v[key] = maskID(item)
			case redactedNameKeys[lower]:
				v[key] = "***"
			default:
				v[key] = redactValue(item)
			}
		}
	case []any:
		for i, item := range v {
			v[i] = redactValue(item)
		}
	}
	return value
}

// maskID 保留字符串 ID 的前 4 个字符（按 rune 计，避免截断多字节字符），其余替换为 ***。
// 太短的字符串和非字符串值整段隐藏；空字符串保持为空，便于看出客户端没有上报。
func maskID(value any) any {
	s, ok := value.(string)
	if !ok {
		return "***"
	}
	if s == "" {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= 4 {
		return "***"
	}
	return string(runes[:4]) + "***"
}
