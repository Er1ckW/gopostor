package gateway

const (
	routeExact   = "exact"
	routeBelow   = "below_range"
	routeBetween = "between_ranges"
	routeAbove   = "above_range"
	routeInvalid = "invalid_version"
)

type routeResult struct {
	backend    *Backend
	normalized int64
	kind       string
}

// route picks the backend for a client version. Versions outside every range go to the nearest
// backend below them (or the oldest one), so that Impostor itself tells the player whether the game
// or the server is outdated instead of the gateway failing with a generic error.
func (c *Config) route(version int64) routeResult {
	if len(c.Backends) == 0 || version < 1 || version > maxVersion {
		return routeResult{kind: routeInvalid}
	}
	r := routeResult{normalized: normalizeVersion(version)}
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
