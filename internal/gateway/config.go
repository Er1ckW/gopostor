// 本文件把 config.json 读进内存，并在启动阶段完成尽可能多的校验。
//
// 设计原则是"启动时失败，而不是运行时才出错"：
//   - 未知字段直接报错（通常是拼写错误，避免静默使用默认值）；
//   - 版本范围必须合法且互不重叠；
//   - publicIp 必须是 IPv4；
//   - httpEndpoint 必须是一个干净的 HTTP(S) 源地址。
package gateway

import (
	// bytes 把 []byte 包装成 io.Reader，供 json.Decoder 使用。
	"bytes"
	// encoding/binary 用于按小端序把 4 个 IP 字节组合成 uint32。
	"encoding/binary"
	// encoding/json 负责 JSON 解析。
	"encoding/json"
	// fmt 用于生成带上下文的错误信息。
	"fmt"
	// io 这里只用到 io.EOF：判断 JSON 后面是否还有多余内容。
	"io"
	// net/netip 是 Go 1.18 引入的 IP 地址类型，比老的 net.IP 更轻量、可比较。
	"net/netip"
	// net/url 解析并校验 httpEndpoint。
	"net/url"
	// os 读取配置文件。
	"os"
	// sort 按版本下限给后端排序。
	"sort"
	// strconv 解析 URL 中显式写出的端口号。
	"strconv"
	// strings 去除配置项两端的空白。
	"strings"
)

// Backend 描述一个 Impostor 后端实例。
//
// 前 6 个字段是导出字段（首字母大写），带 `json:"..."` 结构体标签，
// encoding/json 会按标签里的名字从 JSON 读取；没有标签的字段默认用字段名匹配。
//
// 后 3 个字段是小写开头的"未导出字段"，JSON 不会读取它们。
// 它们是解析配置时预先算好的派生数据，运行时直接复用，避免每次请求重复计算：
//   - min / max：版本范围转换后的整数；
//   - encodedIP：IPv4 按小端序打包成的 uint32，正是 Impostor HTTP 接口里 IP 字段的格式；
//   - addr：net/netip 的地址对象，用来判断回环、内网等。
type Backend struct {
	Name         string     `json:"name"`         // 后端名字，必须唯一，只用于日志和报错
	MinVersion   string     `json:"minVersion"`   // 版本下限（含），可写 "2023.10.1" 或原始整数，留空表示 1
	MaxVersion   string     `json:"maxVersion"`   // 版本上限（含），留空表示不设上限
	PublicIP     string     `json:"publicIp"`     // 玩家实际连接后端 UDP 的公网 IPv4
	PublicPort   int        `json:"publicPort"`   // 玩家实际连接后端 UDP 的公网端口
	HTTPEndpoint string     `json:"httpEndpoint"` // 网关访问该后端内部 HTTP 服务的源地址，如 http://127.0.0.1:23001
	min, max     int64      // 解析后的版本范围，min <= version <= max 表示命中
	encodedIP    uint32     // 小端序编码后的 PublicIP，用于直接返回给游戏客户端
	addr         netip.Addr // 解析后的 PublicIP，用于 IsLoopback/IsPrivate 等判断
}

// Config 对应 config.json 的顶层结构。
//
// 字段上的注释既解释配置含义，也解释默认值从哪里来（见 parseConfig 的初始值）。
// maxBackendTimeoutMS 是 backendTimeoutMs 允许的最大值（毫秒）。
//
// deploy/nginx 中的 proxy_read_timeout 是 15 秒：后端超时如果比它长，
// Nginx 会先返回 504，网关自己的 502 和健康记录都不会生效。
// 10 秒给 Nginx 和网关本身留出余量；调大这里时必须同时调大 Nginx 的超时。
const maxBackendTimeoutMS = 10000

type Config struct {
	// Source 是可选的文档字段，用来记录版本范围来自哪个 Impostor 源码。
	// 保留它是为了让旧示例配置仍能通过严格解析。
	Source string `json:"source,omitempty"`
	// ListenIP / ListenPort 是网关 HTTP 服务的监听地址。
	// 通常监听 127.0.0.1，由前面的 Nginx 负责 HTTPS 和公网入口。
	ListenIP   string `json:"listenIp"`
	ListenPort int    `json:"listenPort"`
	// BackendTimeoutMS 是单次后端请求的超时时间（毫秒），上限见 maxBackendTimeoutMS。
	BackendTimeoutMS int `json:"backendTimeoutMs"`
	// RejectUnknownVersions 控制"版本不在任何范围内"时如何处理游戏请求：
	//   false（默认）：仍转给最近的后端，由 Impostor 告诉玩家该更新游戏还是服务器过旧；
	//   true：网关直接返回 404 Reason 5（版本不匹配）。
	RejectUnknownVersions bool `json:"rejectUnknownVersions"`
	// ProbeBackendOnCreate 为 true 时，创建房间前先向后端发 PUT /api/games 探测；
	// 后端不可用就立即返回 502，玩家不用等到 UDP 连接超时才发现问题。
	ProbeBackendOnCreate   bool `json:"probeBackendOnCreate"`
	HealthCheckIntervalSec int  `json:"healthCheckIntervalSec"` // 后台健康检查间隔（秒），0 表示只在启动时检查
	// HealthFailureThreshold 是已经 up 的后端连续失败多少次才判定为 down。
	// 一次偶发超时不应让 /healthz 立刻变成 503；状态还是 unknown 时第一次失败就判 down。
	HealthFailureThreshold int `json:"healthFailureThreshold"`
	// StatsIntervalSec 已废弃：为兼容旧配置保留解析，当前版本不再输出统计汇总。
	StatsIntervalSec int       `json:"statsIntervalSec"`
	Backends         []Backend `json:"backends"` // 至少一个后端；解析后会按版本下限升序排序
}

// rangeText 把解析后的整数范围还原成可读文本，供测试和调试使用。
//
// 例如 min=1 显示成 "any..2024.2.3"；max 等于 maxVersion（不设上限）显示成 "2024.3.1..any"。
func (b *Backend) rangeText() string {
	low, high := formatVersion(b.min), formatVersion(b.max)
	if b.min <= 1 {
		low = "any"
	}
	if b.max == maxVersion {
		high = "any"
	}
	return low + ".." + high
}

// parseConfig 解析并校验 JSON 配置。
//
// 它接收 []byte 而不是文件路径，因此测试可以直接传入 JSON 字符串，
// 不需要在磁盘上创建临时文件。loadConfig 只是它外面薄薄的一层文件读取。
func parseConfig(data []byte) (Config, error) {
	// 先填入默认值。JSON 中缺省的字段会保留这里的值；
	// 显式写了 0/false 的字段则会被覆盖成 0/false（所以校验要区分"不写"和"写错"）。
	c := Config{ListenIP: "127.0.0.1", ListenPort: 24000, BackendTimeoutMS: 5000, ProbeBackendOnCreate: true, HealthCheckIntervalSec: 30, HealthFailureThreshold: 3}
	// bytes.NewReader 把字节切片包装成流；json.NewDecoder 从流里逐个读取 JSON 值。
	decoder := json.NewDecoder(bytes.NewReader(data))
	// 严格模式：JSON 里出现结构体没有定义的字段就返回错误。
	// 这样 "backendTimeoutMillis" 这类拼写错误会在启动时暴露，而不是静默失效。
	decoder.DisallowUnknownFields()
	// Decode(&c) 把 JSON 填充进 c。传 &c（指针）是因为需要修改调用者的变量。
	if err := decoder.Decode(&c); err != nil {
		return c, err
	}
	// 再尝试解码一个值：正常情况下流已经结束，会返回 io.EOF。
	// 如果不是 EOF，说明文件里有两段 JSON 或尾部有垃圾内容，视为错误。
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return c, fmt.Errorf("unexpected trailing JSON")
	}
	// 去除监听地址两端空白；空地址无法监听，直接报错。
	c.ListenIP = strings.TrimSpace(c.ListenIP)
	if c.ListenIP == "" {
		return c, fmt.Errorf("listenIp must not be empty")
	}
	// 端口必须落在 TCP 合法范围内。
	if c.ListenPort < 1 || c.ListenPort > 65535 {
		return c, fmt.Errorf("listenPort must be between 1 and 65535")
	}
	// 后端超时限制在 1ms 到 120s 之间，避免配置成 0（永不超时）或过大。
	if c.BackendTimeoutMS < 1 || c.BackendTimeoutMS > maxBackendTimeoutMS {
		return c, fmt.Errorf("backendTimeoutMs must be between 1 and %d", maxBackendTimeoutMS)
	}
	// 没有任何后端时，所有游戏请求都无处可转，启动就没有意义。
	if len(c.Backends) == 0 {
		return c, fmt.Errorf("at least one backend is required")
	}
	// 健康检查间隔允许为 0（表示只在启动时检查），但不能为负数。
	if c.HealthCheckIntervalSec < 0 {
		return c, fmt.Errorf("healthCheckIntervalSec must not be negative")
	}

	if c.HealthFailureThreshold < 1 || c.HealthFailureThreshold > 100 {
		return c, fmt.Errorf("healthFailureThreshold must be between 1 and 100")
	}
	// 用一个 map 记录已出现的名字，顺便检查空名和重名。
	names := map[string]bool{}
	// 这里必须用下标取地址（&c.Backends[i]），才能修改切片里的元素；
	// 若写成 for _, b := range，b 只是副本，改它不会影响 c.Backends。
	for i := range c.Backends {
		b := &c.Backends[i]
		b.Name = strings.TrimSpace(b.Name)
		if b.Name == "" || names[b.Name] {
			return c, fmt.Errorf("empty or duplicate backend name: %q", b.Name)
		}
		names[b.Name] = true
		// 声明 err 供下面两次赋值复用；:= 在第一次出现时创建变量。
		var err error
		// 版本下限留空时取 1（相当于"不限下限"）；上限留空时取 maxVersion。
		if b.min, err = parseVersionBound(b.MinVersion, 1); err != nil {
			return c, fmt.Errorf("%s: %w", b.Name, err)
		}
		if b.max, err = parseVersionBound(b.MaxVersion, maxVersion); err != nil {
			return c, fmt.Errorf("%s: %w", b.Name, err)
		}
		// 区间必须非空（min <= max），公网端口必须合法。
		if b.min > b.max || b.PublicPort < 1 || b.PublicPort > 65535 {
			return c, fmt.Errorf("%s: invalid range or port", b.Name)
		}
		// publicIp 必须是可解析的 IPv4。游戏客户端的 UDP 连接只支持 IPv4，
		// IPv6 会导致玩家连接失败，所以在启动时就拒绝。
		b.PublicIP = strings.TrimSpace(b.PublicIP)
		ip, err := netip.ParseAddr(b.PublicIP)
		if err != nil || !ip.Is4() {
			return c, fmt.Errorf("%s: publicIp must be IPv4", b.Name)
		}
		b.addr = ip
		// As4 返回 [4]byte（数组，不是切片）。binary.LittleEndian 按小端序
		// 把这 4 个字节解释成 uint32，与 Impostor/Among Us HTTP 协议中的 IP 表示一致。
		// 例如 127.0.0.1 会编码成 16777343。
		octets := ip.As4()
		b.encodedIP = binary.LittleEndian.Uint32(octets[:])
		// 校验并规范化后端 HTTP 地址。
		// 合法的 httpEndpoint 必须"只有协议 + 主机 + 可选的端口"，不能带路径、查询参数、
		// 用户名密码或片段，否则拼接 /api/games 时会产生错误地址。
		b.HTTPEndpoint = strings.TrimSpace(b.HTTPEndpoint)
		u, err := url.Parse(b.HTTPEndpoint)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
			return c, fmt.Errorf("%s: httpEndpoint must be an HTTP(S) origin", b.Name)
		}
		// 统一去掉结尾的 "/"，后续直接拼接 "/api/games"。
		b.HTTPEndpoint = u.Scheme + "://" + u.Host
		// 如果显式写了端口，检查它是否在合法范围内。
		if u.Port() != "" {
			port, err := strconv.Atoi(u.Port())
			if err != nil || port < 1 || port > 65535 {
				return c, fmt.Errorf("%s: invalid HTTP port", b.Name)
			}
		}
	}
	// 按版本下限升序排序。routing.go 中的"找最近后端"和 overlap 检查都依赖这个顺序。
	// sort.Slice 接收比较函数，返回 true 表示 i 应排在 j 前面。
	sort.Slice(c.Backends, func(i, j int) bool { return c.Backends[i].min < c.Backends[j].min })
	// 检查相邻范围是否重叠。因为已按 min 排序，只需比较后一个的 min 与前一个的 max。
	// 端点包含在范围内，所以相邻范围也必须满足 next.min > prev.max。
	for i := 1; i < len(c.Backends); i++ {
		if c.Backends[i].min <= c.Backends[i-1].max {
			return c, fmt.Errorf("overlapping version ranges: %s and %s", c.Backends[i-1].Name, c.Backends[i].Name)
		}
	}
	return c, nil
}

// loadConfig 读取文件内容后交给 parseConfig。
// 把"读文件"和"解析校验"分开，测试就能只测解析逻辑，不依赖真实文件。
func loadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	return parseConfig(data)
}
