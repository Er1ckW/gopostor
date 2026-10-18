// 本文件只做一件事：根据客户端协议版本选择后端。
//
// 配置中的后端已经按版本下限从小到大排好序，且范围互不重叠（见 config.go）。
// 路由结果除了"选哪个后端"，还带有 kind 字段说明属于哪种情况，供调用方判断。
package gateway

const (
	// routeExact 表示版本正好落在某个后端的范围内——这是唯一"完全正常"的路由。
	routeExact = "exact"
	// routeBelow 表示版本低于所有后端范围，会转给最老的后端。
	routeBelow = "below_range"
	// routeBetween 表示版本落在两个后端范围之间的空隙，会转给下限不高于它的最近后端。
	routeBetween = "between_ranges"
	// routeAbove 表示版本高于所有范围，会转给最新的后端。
	routeAbove = "above_range"
	// routeInvalid 表示版本本身不合法（例如 0、负数或超出 int32），无法路由。
	routeInvalid = "invalid_version"
)

// routeResult 是 route 的返回值：选中的后端、去掉 +25 标志后的版本、以及路由类型。
// backend 在 routeInvalid 时是 nil，调用方必须先判断 kind 或 backend 是否为空。
type routeResult struct {
	backend    *Backend
	normalized int64
	kind       string
}

// route 根据客户端版本选择后端。
//
// 关键设计：版本超出所有范围时**不报错**，而是转给"最近"的后端。
// 原因是 Among Us 更新后，新客户端会先到达网关；把它交给最新后端后，
// Impostor 可以在 UDP 握手阶段明确告诉玩家"游戏需要更新"或"服务器版本过旧"，
// 这比网关返回一个笼统的 HTTP 错误对玩家更友好。
//
// 具体规则：
//  1. 落在某个范围内：转给该后端（exact）；
//  2. 低于所有范围：转给最老的后端（below_range）；
//  3. 高于所有范围：转给最新的后端（above_range）；
//  4. 落在两个范围之间：转给下限不高于它的最近后端（between_ranges）。
func (c *Config) route(version int64) routeResult {
	// 防御性检查：空配置直接返回 invalid，避免下面访问 c.Backends[0] 时 panic。
	// version 是 int64，但协议字段是 32 位有符号整数，超出范围的值不可能来自真实客户端。
	if len(c.Backends) == 0 || version < 1 || version > maxVersion {
		return routeResult{kind: routeInvalid}
	}
	// 先把 +25 标志去掉，路由比较只使用 normalized；原始 version 仍保留在 token 中。
	r := routeResult{normalized: normalizeVersion(version)}
	// 第一轮：找精确命中。由于范围已排序且不重叠，找到第一个即可返回。
	for i := range c.Backends {
		b := &c.Backends[i]
		if r.normalized >= b.min && r.normalized <= b.max {
			r.backend, r.kind = b, routeExact
			return r
		}
	}
	// 第二轮：低于最老后端的下限，交给最老的后端。
	if r.normalized < c.Backends[0].min {
		r.backend, r.kind = &c.Backends[0], routeBelow
		return r
	}
	// 第三轮：从最新后端往前找，第一个 min <= 当前版本的后端就是"最近的下方后端"。
	// 此时已排除 exact，所以结果必然落在某个范围之后（或两段之间的空隙）。
	for i := len(c.Backends) - 1; i >= 0; i-- {
		if c.Backends[i].min <= r.normalized {
			r.backend = &c.Backends[i]
			r.kind = routeBetween
			// 如果这个后端就是最后一个（版本高于它的上限），归类为 above_range；
			// 否则说明版本落在它和下一个后端之间，归类为 between_ranges。
			if i == len(c.Backends)-1 {
				r.kind = routeAbove
			}
			break
		}
	}
	return r
}
