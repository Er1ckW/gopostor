package gateway

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
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
	encodedIP    uint32
	addr         netip.Addr
}

type Config struct {
	// Source is optional documentation for where the version ranges came from.
	// It is accepted so configurations copied from older examples keep working.
	Source           string `json:"source,omitempty"`
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
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&c); err != nil {
		return c, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return c, fmt.Errorf("unexpected trailing JSON")
	}
	c.ListenIP = strings.TrimSpace(c.ListenIP)
	if c.ListenIP == "" {
		return c, fmt.Errorf("listenIp must not be empty")
	}
	if c.ListenPort < 1 || c.ListenPort > 65535 {
		return c, fmt.Errorf("listenPort must be between 1 and 65535")
	}
	if c.BackendTimeoutMS < 1 || c.BackendTimeoutMS > 120000 {
		return c, fmt.Errorf("backendTimeoutMs must be between 1 and 120000")
	}
	if len(c.Backends) == 0 {
		return c, fmt.Errorf("at least one backend is required")
	}
	if c.HealthCheckIntervalSec < 0 || c.StatsIntervalSec < 0 {
		return c, fmt.Errorf("healthCheckIntervalSec and statsIntervalSec must not be negative")
	}
	names := map[string]bool{}
	for i := range c.Backends {
		b := &c.Backends[i]
		b.Name = strings.TrimSpace(b.Name)
		if b.Name == "" || names[b.Name] {
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
		b.PublicIP = strings.TrimSpace(b.PublicIP)
		ip, err := netip.ParseAddr(b.PublicIP)
		if err != nil || !ip.Is4() {
			return c, fmt.Errorf("%s: publicIp must be IPv4", b.Name)
		}
		b.addr = ip
		octets := ip.As4()
		b.encodedIP = binary.LittleEndian.Uint32(octets[:])
		b.HTTPEndpoint = strings.TrimSpace(b.HTTPEndpoint)
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
			warnings = append(warnings, hintConfigLoopback(b))
		case b.addr.IsPrivate():
			warnings = append(warnings, hintConfigPrivate(b))
		}
		if i > 0 && b.min > c.Backends[i-1].max+1 {
			notes = append(notes, hintConfigGap(c, i))
		}
	}
	last := &c.Backends[len(c.Backends)-1]
	if last.max != maxVersion {
		text := hintConfigMax(c, last)
		if c.RejectUnknownVersions {
			warnings = append(warnings, text)
		} else {
			notes = append(notes, text)
		}
	}
	return warnings, notes
}
