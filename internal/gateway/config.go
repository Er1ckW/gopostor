package gateway

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
)

type Backend struct {
	Name         string `json:"name"`
	MinVersion   string `json:"minVersion"`
	MaxVersion   string `json:"maxVersion"`
	PublicIP     string `json:"publicIp"`
	PublicPort   int    `json:"publicPort"`
	HTTPEndpoint string `json:"httpEndpoint"`
	min, max     int64
	ip           uint32
	addr         netip.Addr
}

type Config struct {
	ListenIP         string `json:"listenIp"`
	ListenPort       int    `json:"listenPort"`
	BackendTimeoutMS int    `json:"backendTimeoutMs"`
	// RejectUnknownVersions answers game requests of versions outside every range with Reason 5
	// instead of forwarding them to the nearest backend.
	RejectUnknownVersions bool `json:"rejectUnknownVersions"`
	// ProbeBackendOnCreate asks the backend before handing out its address for a new game.
	ProbeBackendOnCreate   bool      `json:"probeBackendOnCreate"`
	HealthCheckIntervalSec int       `json:"healthCheckIntervalSec"`
	StatsIntervalSec       int       `json:"statsIntervalSec"`
	Backends               []Backend `json:"backends"`
}

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

func parseConfig(data []byte) (Config, error) {
	c := Config{ListenIP: "127.0.0.1", ListenPort: 24000, BackendTimeoutMS: 5000, ProbeBackendOnCreate: true, HealthCheckIntervalSec: 30, StatsIntervalSec: 600}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, err
	}
	if strings.TrimSpace(c.ListenIP) == "" || c.ListenPort < 1 || c.ListenPort > 65535 || c.BackendTimeoutMS < 1 || c.BackendTimeoutMS > 120000 || len(c.Backends) == 0 {
		return c, fmt.Errorf("invalid listen address, timeout or empty backends")
	}
	if c.HealthCheckIntervalSec < 0 || c.StatsIntervalSec < 0 {
		return c, fmt.Errorf("healthCheckIntervalSec and statsIntervalSec must not be negative")
	}
	names := map[string]bool{}
	for i := range c.Backends {
		b := &c.Backends[i]
		if strings.TrimSpace(b.Name) == "" || names[b.Name] {
			return c, fmt.Errorf("empty or duplicate backend name: %q", b.Name)
		}
		names[b.Name] = true
		var err error
		if b.min, err = parseVersionBound(b.MinVersion, 1); err != nil {
			return c, fmt.Errorf("%s: %w", b.Name, err)
		}
		if b.max, err = parseVersionBound(b.MaxVersion, maxVersion); err != nil {
			return c, fmt.Errorf("%s: %w", b.Name, err)
		}
		if b.min > b.max || b.PublicPort < 1 || b.PublicPort > 65535 {
			return c, fmt.Errorf("%s: invalid range or port", b.Name)
		}
		ip, err := netip.ParseAddr(b.PublicIP)
		if err != nil || !ip.Is4() {
			return c, fmt.Errorf("%s: publicIp must be IPv4", b.Name)
		}
		b.addr = ip
		octets := ip.As4()
		b.ip = binary.LittleEndian.Uint32(octets[:])
		u, err := url.Parse(b.HTTPEndpoint)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
			return c, fmt.Errorf("%s: httpEndpoint must be an HTTP(S) origin", b.Name)
		}
		b.HTTPEndpoint = u.Scheme + "://" + u.Host
		if u.Port() != "" {
			port, err := strconv.Atoi(u.Port())
			if err != nil || port < 1 || port > 65535 {
				return c, fmt.Errorf("%s: invalid HTTP port", b.Name)
			}
		}
	}
	sort.Slice(c.Backends, func(i, j int) bool { return c.Backends[i].min < c.Backends[j].min })
	for i := 1; i < len(c.Backends); i++ {
		if c.Backends[i].min <= c.Backends[i-1].max {
			return c, fmt.Errorf("overlapping version ranges: %s and %s", c.Backends[i-1].Name, c.Backends[i].Name)
		}
	}
	return c, nil
}

func loadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	return parseConfig(data)
}

// review lists configuration choices that are valid but likely to cause player-visible problems
// (warnings), and choices that only decide how versions outside every range are routed (notes).
func (c *Config) review() (warnings, notes []string) {
	for i := range c.Backends {
		b := &c.Backends[i]
		switch {
		case b.addr.IsLoopback() || b.addr.IsUnspecified():
			warnings = append(warnings, fmt.Sprintf("后端 %s 的 publicIp=%s 是回环/未指定地址，只有本机客户端能连上 UDP；正式部署请改成服务器公网 IPv4", b.Name, b.PublicIP))
		case b.addr.IsPrivate():
			warnings = append(warnings, fmt.Sprintf("后端 %s 的 publicIp=%s 是内网地址，外网玩家创建/加入房间后会连接超时；仅局域网部署时可忽略", b.Name, b.PublicIP))
		}
		if i > 0 && b.min > c.Backends[i-1].max+1 {
			notes = append(notes, fmt.Sprintf("版本 %s 与 %s 之间没有后端：落在间隙内的客户端将转给 %s%s", formatVersion(c.Backends[i-1].max), formatVersion(b.min), c.Backends[i-1].Name, c.rejectNote()))
		}
	}
	last := &c.Backends[len(c.Backends)-1]
	if last.max != maxVersion {
		text := fmt.Sprintf("最高版本上限为 %s：Among Us 更新后的新客户端将转给 %s%s；可以把最后一个后端的 maxVersion 留空表示不设上限", formatVersion(last.max), last.Name, c.rejectNote())
		if c.RejectUnknownVersions {
			warnings = append(warnings, text)
		} else {
			notes = append(notes, text)
		}
	}
	return warnings, notes
}

func (c *Config) rejectNote() string {
	if c.RejectUnknownVersions {
		return "，但 rejectUnknownVersions=true，其游戏请求会被拒绝（Reason 5）"
	}
	return "，由该后端在 UDP 握手时判断版本"
}
