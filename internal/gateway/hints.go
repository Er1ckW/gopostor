package gateway

import (
	"fmt"
	"strings"
	"time"
)

// Operator-facing text lives here, away from routing and proxy decisions, so that
// changing or translating a hint does not require touching request handling.

const warnOnceNote = "（同类问题之后只记为 info）"

func hintHealthzBackendDown() string {
	return "健康检查报告有后端不可达，详见启动后的 [ERROR] 后端 ... 不可达 日志"
}

func hintTokenProblem(problem string) string {
	switch problem {
	case "missing_authorization":
		return "请求没有 Authorization 头：Nginx 可能未转发它（需要 proxy_set_header Authorization $http_authorization），或客户端没有先成功调用 /api/user"
	case "invalid_client_version":
		return "token 中没有有效的 ClientVersion：token 不是 Impostor/本网关签发的"
	default:
		return "token 格式不是 Impostor/本网关签发的 Base64 JSON：客户端可能沿用了官方服务器或其他私服的 token，让玩家重启游戏后重新选择该区域"
	}
}

func hintTokenRequestUnreadable() string {
	return "读取 /api/user 请求体失败或超过 64KB：客户端中途断开，或代理截断了请求"
}

func hintTokenRequestInvalidJSON() string {
	return "/api/user 请求体不是预期的 JSON：确认请求来自 Among Us 客户端，且 Nginx 没有改写请求体"
}

func hintTokenRequestInvalidVersion() string {
	return "/api/user 缺少有效的 ClientVersion，无法确定后端：请求很可能不是来自 Among Us 客户端"
}

func hintTokenFieldsMissing(missing []string) string {
	return "缺少 " + strings.Join(missing, "/") + "，通常是旧版客户端或无名称账号；网关照常签发 token"
}

func hintBackendUnavailable(b *Backend, err error, timeoutMS int) string {
	prefix := fmt.Sprintf("后端 %s（%s）", b.Name, b.HTTPEndpoint)
	switch kind := classifyError(err); {
	case kind == kindConnectionRefused || kind == kindNetworkDial:
		return prefix + " 拒绝连接：检查 Impostor 是否在运行、config.json 的 HttpServer.Enabled 是否为 true、HttpServer.ListenIp/ListenPort 是否与 httpEndpoint 一致"
	case kind == kindTimeout:
		return fmt.Sprintf("%s 在 %dms 内没有响应：后端可能卡死或过载、防火墙丢弃了连接，或 backendTimeoutMs 太小", prefix, timeoutMS)
	case kind == kindDNS:
		return prefix + " 的主机名无法解析：检查 httpEndpoint 拼写或 DNS"
	case kind == kindRedirect:
		return prefix + " 返回了重定向：httpEndpoint 可能指向了 Nginx 的 HTTPS 跳转或其他站点，应直接指向 Impostor 的内部 HTTP 端口"
	case kind == kindInvalidPayload || kind == kindInvalidJSON:
		return prefix + " 返回的内容不是预期的 JSON：httpEndpoint 可能指向了其他服务，或后端版本与网关不匹配"
	default:
		return fmt.Sprintf("%s 请求失败（%s）", prefix, kind)
	}
}

func hintBackendSlow(b *Backend, elapsed time.Duration, timeoutMS int) string {
	return fmt.Sprintf("后端 %s 用时 %dms，超过超时时间 %dms 的一半；持续出现时检查后端负载和网络", b.Name, elapsed.Milliseconds(), timeoutMS)
}

func hintBackendRejectedCreate(b *Backend, status int) string {
	return fmt.Sprintf("后端 %s 对创建房间返回 HTTP %d 或无效内容，玩家无法创建房间：检查后端日志", b.Name, status)
}

func hintBackendMarkedDown(b *Backend) string {
	return fmt.Sprintf("后端 %s 最近一次检查不可达，但 probeBackendOnCreate=false，仍返回其地址；玩家可能在连接 UDP 时超时", b.Name)
}

func hintBackendAddressMismatch(b *Backend, address string, reportedPort int64) string {
	return fmt.Sprintf("后端 %s 自报的公网地址 %s:%d 与网关 config.json 的 %s:%d 不一致；网关始终返回自己的配置，请确认玩家实际能用 %s:%d 连到这个后端的 UDP，并让两边保持一致",
		b.Name, address, reportedPort, b.PublicIP, b.PublicPort, b.PublicIP, b.PublicPort)
}

func hintGameNotFound(b *Backend) string {
	return fmt.Sprintf("后端 %s 上没有这个房间：房间已关闭、房间码输错，或房主属于另一个版本范围的后端", b.Name)
}

func hintBackendError(b *Backend, status int, reasons []any, messages []string) string {
	return fmt.Sprintf("后端 %s 返回 HTTP %d（Reason %v）：%s", b.Name, status, reasons, strings.Join(messages, " | "))
}

func hintMissingEndpoint(b *Backend, method, endpoint string, known bool) string {
	if known {
		return fmt.Sprintf("后端 %s 没有 %s %s 接口（Impostor 1.9.x 不支持 16.0 及之后客户端使用的接口），网关已代为返回空结果或房间不存在", b.Name, method, endpoint)
	}
	return fmt.Sprintf("后端 %s 没有 %s %s 接口（Impostor 1.9.3 和 1.10.9 都没有实现它），网关照旧返回 404（Reason 17），与之前的版本行为相同", b.Name, method, endpoint)
}

func hintBackendErrorUnparsed(b *Backend, status, bodyBytes int) string {
	return fmt.Sprintf("后端 %s 返回 HTTP %d，响应不是 Impostor 的错误格式（%d 字节）：可能是后端异常，或 httpEndpoint 指向了 Nginx/其他服务", b.Name, status, bodyBytes)
}

func hintRoute(c *Config, r routeResult) string {
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

func hintConfigLoopback(b *Backend) string {
	return fmt.Sprintf("后端 %s 的 publicIp=%s 是回环/未指定地址，只有本机客户端能连上 UDP；正式部署请改成服务器公网 IPv4", b.Name, b.PublicIP)
}

func hintConfigPrivate(b *Backend) string {
	return fmt.Sprintf("后端 %s 的 publicIp=%s 是内网地址，外网玩家创建/加入房间后会连接超时；仅局域网部署时可忽略", b.Name, b.PublicIP)
}

func hintConfigGap(c *Config, i int) string {
	return fmt.Sprintf("版本 %s 与 %s 之间没有后端：落在间隙内的客户端将转给 %s%s", formatVersion(c.Backends[i-1].max), formatVersion(c.Backends[i].min), c.Backends[i-1].Name, hintConfigRejectNote(c))
}

func hintConfigMax(c *Config, last *Backend) string {
	return fmt.Sprintf("最高版本上限为 %s：Among Us 更新后的新客户端将转给 %s%s；可以把最后一个后端的 maxVersion 留空表示不设上限", formatVersion(last.max), last.Name, hintConfigRejectNote(c))
}

func hintConfigRejectNote(c *Config) string {
	if c.RejectUnknownVersions {
		return "，但 rejectUnknownVersions=true，其游戏请求会被拒绝（Reason 5）"
	}
	return "，由该后端在 UDP 握手时判断版本"
}
