// 本文件负责协议版本的"整数编码"和解析。
//
// 游戏客户端在 HTTP 请求里传的是一个整数，而不是 "2023.10.1" 这样的字符串。
// Among Us / Impostor 使用如下编码：
//
//	版本整数 = 年*25000 + 月*1800 + 日*50 + revision
//
// 例如 2023.11.15 编码为 2023*25000 + 11*1800 + 15*50 = 50595550。
// 网关配置允许写可读版本，也允许直接写整数，两者最终都转换成同一个 int64 再比较。
package gateway

import (
	// fmt 生成错误信息和格式化后的版本文本。
	"fmt"
	// regexp 编译版本字符串的正则表达式。
	"regexp"
	// strconv 把字符串片段解析成整数。
	"strconv"
	// strings 去除配置值两端空白。
	"strings"
)

const (
	// 协议版本按"年*25000 + 月*1800 + 日*50 + revision"编码。
	// 这些常量必须与 Impostor 保持一致，否则路由会选错后端。
	yearFactor  = 25000
	monthFactor = 1800
	dayFactor   = 50
	// 客户端可能给版本加上 +25 的"禁用服务器权威（disable server authority）"标志。
	// 这个标志只影响游戏内行为，路由比较前必须去掉，否则会落不到任何版本范围。
	authorityFlag = 25

	// maxVersion 是 int32 的最大值 2147483647。
	// 客户端协议版本字段是 32 位有符号整数，超过它既不可能出现，也会导致编码溢出。
	maxVersion = 2147483647

	// minRawVersion 是 2000.1.1 的编码值。
	// 配置里直接写整数时，小于它的值（例如 2023、-1）不可能是真实客户端版本，直接拒绝。
	minRawVersion = 2000*yearFactor + monthFactor + dayFactor
)

// versionPattern 匹配 "年.月.日" 或 "年.月.日.revision" 两种写法。
// ^ 和 $ 表示必须匹配整个字符串；最后一段 (?:\.(\d+))? 是可选的 revision。
// 正则里有 4 组括号，FindStringSubmatch 会返回"整体 + 4 个分组"共 5 个元素。
var versionPattern = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)(?:\.(\d+))?$`)

// encodeVersion 把 "2023.10.1" 这样的可读版本转换成协议整数。
func encodeVersion(s string) (int64, error) {
	// FindStringSubmatch 返回 nil 表示格式不匹配（例如 "19.0"、"2026.13" 之外的情况）。
	parts := versionPattern.FindStringSubmatch(s)
	if parts == nil {
		return 0, fmt.Errorf("invalid protocol version: %q", s)
	}
	// v[0..3] 分别保存年、月、日、revision；数组长度固定为 4。
	var v [4]int64
	for i := range v {
		// parts[0] 是整个匹配，parts[1..4] 才是各分组。
		// 可选分组没写时是空字符串，跳过即可（revision 保持默认 0）。
		if parts[i+1] == "" {
			continue
		}
		n, err := strconv.ParseInt(parts[i+1], 10, 64)
		if err != nil {
			return 0, err
		}
		v[i] = n
	}
	// 按协议公式合成一个整数。
	n := v[0]*yearFactor + v[1]*monthFactor + v[2]*dayFactor + v[3]
	// 逐项检查字段范围：
	//   - 年上限 85899 把量级限制在 int32 附近，最后再用 n > maxVersion 兜底；
	//   - 月 1..12、日 1..31 是日历范围（不检查具体月份天数，协议本身也不检查）；
	//   - revision 0..24，因为 +25 被保留给"禁用服务器权威"标志；
	//   - 最后再兜底检查一次总编码是否超出 int32。
	if v[0] < 1 || v[0] > 85899 || v[1] < 1 || v[1] > 12 || v[2] < 1 || v[2] > 31 || v[3] > 24 || n > maxVersion {
		return 0, fmt.Errorf("invalid protocol version: %q", s)
	}
	return n, nil
}

// parseVersionBound 解析配置里的版本边界，支持三种写法：
//
//	"2023.10.1" -> 按可读版本编码；
//	"50595550"  -> 直接当作已有整数；
//	""          -> 返回调用者给的默认值 empty（下限默认 1，上限默认 maxVersion）。
func parseVersionBound(s string, empty int64) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return empty, nil
	}
	// 先尝试按十进制整数解析。注意 "2023.10.1" 会因小数点解析失败，
	// 从而进入下面的 encodeVersion；而 "2023" 能解析成整数，但它小于
	// minRawVersion，会被判为非法，避免把年份误当成完整版本。
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		if n < minRawVersion || n > maxVersion {
			return 0, fmt.Errorf("invalid protocol version: %q", s)
		}
		return n, nil
	}
	return encodeVersion(s)
}

// formatVersion 做 encodeVersion 的逆运算，把整数还原成可读版本，用于配置和错误信息。
// 余数会依次分解出月、日、revision。revision 为 0 时省略最后一段。
func formatVersion(n int64) string {
	year, rest := n/yearFactor, n%yearFactor
	month, rest := rest/monthFactor, rest%monthFactor
	day, revision := rest/dayFactor, rest%dayFactor
	if revision == 0 {
		return fmt.Sprintf("%d.%d.%d", year, month, day)
	}
	return fmt.Sprintf("%d.%d.%d.%d", year, month, day, revision)
}

// normalizeVersion 去掉客户端版本里的 +25 标志。
//
// 编码中日的系数是 50，而 revision 只允许 0..24，所以正常情况下
// version % 50 就是 revision；带 +25 标志时余数会变成 25..49。
// 判断余数 >= 25 就减掉 25，得到真正用于路由比较的版本。
// 注意 token 里保留原始版本，只有路由比较使用去掉标志的值。
func normalizeVersion(version int64) int64 {
	if version%dayFactor >= authorityFlag {
		return version - authorityFlag
	}
	return version
}
