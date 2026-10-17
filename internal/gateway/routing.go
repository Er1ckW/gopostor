package gateway

import (
	"fmt"
	"net/http"
)

const (
	routeExact   = "exact"
	routeBelow   = "below_range"
	routeBetween = "between_ranges"
	routeAbove   = "above_range"
	routeInvalid = "invalid_version"
)

type route struct {
	backend    *Backend
	normalized int64
	kind       string
}

// route picks the backend for a client version. Versions outside every range go to the nearest
// backend below them (or the oldest one), so that Impostor itself tells the player whether the game
// or the server is outdated instead of the gateway failing with a generic error.
func (c *Config) route(version int64) route {
	if version < 1 || version > maxVersion {
		return route{kind: routeInvalid}
	}
	r := route{normalized: normalizeVersion(version)}
	for i := range c.Backends {
		b := &c.Backends[i]
		if r.normalized >= b.min && r.normalized <= b.max {
			r.backend, r.kind = b, routeExact
			return r
		}
	}
	if r.normalized < c.Backends[0].min {
		r.backend, r.kind = &c.Backends[0], routeBelow
		return r
	}
	for i := len(c.Backends) - 1; i >= 0; i-- {
		if c.Backends[i].min <= r.normalized {
			r.backend = &c.Backends[i]
			r.kind = routeBetween
			if i == len(c.Backends)-1 {
				r.kind = routeAbove
			}
			break
		}
	}
	return r
}

// findBackend returns the backend whose range contains the version, without any fallback.
func (c *Config) findBackend(version int64) *Backend {
	if r := c.route(version); r.kind == routeExact {
		return r.backend
	}
	return nil
}

func (c *Config) routeHint(r route) string {
	if r.kind == routeExact || r.kind == routeInvalid {
		return ""
	}
	v := versionText(r.normalized)
	first, last := &c.Backends[0], &c.Backends[len(c.Backends)-1]
	var hint string
	switch r.kind {
	case routeBelow:
		hint = fmt.Sprintf("客户端版本 %s 低于所有已配置范围（最低 %s），已转给 %s，玩家连接时会被后端提示更新游戏；如需支持该版本，请部署对应版本的 Impostor 并在 config.json 中添加范围", v, formatVersion(first.min), r.backend.Name)
	case routeAbove:
		hint = fmt.Sprintf("客户端版本 %s 高于所有已配置范围（最高 %s），可能是 Among Us 刚更新，已转给最新后端 %s；若该后端不支持此版本，玩家会看到服务器版本过旧。请升级后端，并把 maxVersion 调大或留空", v, formatVersion(last.max), r.backend.Name)
	case routeBetween:
		hint = fmt.Sprintf("客户端版本 %s 落在两个后端范围之间，已转给 %s；请确认该版本应由哪个后端处理并调整范围", v, r.backend.Name)
	}
	if c.RejectUnknownVersions {
		hint += "（rejectUnknownVersions=true：游戏请求将返回 Reason 5）"
	}
	return hint
}

func (g *gateway) logRoute(w http.ResponseWriter, r route) {
	diagnostic(w, "route", r.kind)
	if r.backend == nil {
		return
	}
	logBackend(w, r.backend)
	if r.kind != routeExact {
		diagnostic(w, "backend_range", r.backend.rangeText())
		issueOnce(w, "route:"+r.kind+":"+versionText(r.normalized), "version_"+r.kind, g.config.routeHint(r))
	}
}
