# gopostor 源码阅读指南

这份指南配合源码中的中文注释使用，目标是让刚接触 Go 的读者按一条清晰的路线读完整个项目，而不是从第一个文件开始逐行硬啃。

建议的读法是：**先看整体流程图，再按"启动 → 配置/版本 → 登录 → 分发 → 转发 → 运维"的顺序读，每读完一个文件就去看对应的测试。** 测试是"可执行的规格说明"，能告诉你这段代码到底承诺了什么行为。

---

## 1. 先用一句话理解项目

gopostor 是一个用 Go 标准库写的 Among Us / Impostor **HTTP 匹配网关**。

玩家在游戏里只填一个服务器地址。客户端先调用网关的 `/api/user` 拿 token，再调用 `/api/games` 系列接口创建/加入/查询房间。网关根据客户端上报的 `ClientVersion`，把请求转给对应版本的 Impostor 后端，并把后端返回的**内网地址**改写成配置里的**公网 IP + UDP 端口**。之后真正的游戏流量直接连到那个后端，不再经过网关。

所以整个程序其实只做四件事：

1. **版本路由**：这个客户端该去哪个 Impostor？
2. **地址改写**：玩家该连哪个公网 IP/端口？
3. **兼容与错误处理**：老后端没有新接口时怎么办？后端报错时给玩家看什么？
4. **可运维性**：访问日志、健康检查。

---

## 2. 一张图看清请求怎么走

```text
玩家客户端
   │
   │  POST /api/user  { ClientVersion, Puid, ... }
   ▼
┌─────────────────────────────────────────────────────┐
│ logRequests 中间件（logging.go）                     │
│ 读取请求 body、包装 ResponseWriter、开始计时          │
└─────────────────────────────────────────────────────┘
   │
   ▼
serve（handler.go）判断路径和方法
   │
   ├── GET/HEAD /          → 返回固定存活文本
   ├── GET/HEAD /healthz   → 汇总后端健康状态（有 down 则 503）
   ├── POST /api/user      → serveToken（token.go）
   │        ├─ 校验 ClientVersion
   │        └─ 返回 Base64(JSON token)，text/plain
   │
   └── /api/games 系列
            │
            ├─ parseTokenVersion：从 Authorization 读出 ClientVersion
            ├─ route(version)：选后端（routing.go）
            ├─ PUT /api/games → serveCreateGame（proxy.go）
            │        ├─ 可选：先 PUT 后端探测
            │        └─ 返回配置的 publicIp/publicPort
            │
            └─ 其他 → proxy（proxy.go）
                     ├─ 原样转发路径、查询参数、Authorization
                     ├─ 后端 2xx：decodeJSON → rewrite IP/Port → 返回 200
                     └─ 后端非 2xx：只保留 Errors[].Reason
   │
   ▼
响应写回客户端；logRequests 的 defer 输出一行访问日志
   │
   ▼
玩家拿到后端公网 IP + UDP 端口，游戏流量直接连后端
```

记住这个顺序，读代码时就不容易迷路：**中间件包住 handler，handler 分发，token 决定版本，route 决定后端，proxy 负责转发和改写。**

---

## 3. 推荐阅读顺序

### 第 1 站：程序怎么启动、怎么退出

按顺序读：

1. [`cmd/gopostor/main.go`](cmd/gopostor/main.go)：只有十几行，看它如何调用 `gateway.Run()` 并把错误变成退出码。
2. [`internal/gateway/app.go`](internal/gateway/app.go)：重点看 `Run()`。

读 `Run()` 时带着这几个问题：

- `flag.String` / `flag.Parse` 如何解析 `-config`？
- `http.Server` 的五个字段分别是什么？为什么要设 `ReadHeaderTimeout`、`ReadTimeout`、`IdleTimeout`？
- `signal.NotifyContext` 如何把 Ctrl+C / SIGTERM 变成 `ctx.Done()`？
- 健康检查为什么单独用一个 `go func()`？
- `Shutdown` 和 `Close` 的区别是什么？`done` channel 为什么存在？
- `ListenAndServe` 为什么把 `http.ErrServerClosed` 当成正常结束？

### 第 2 站：配置和版本号

1. [`internal/gateway/config.go`](internal/gateway/config.go)
2. [`internal/gateway/version.go`](internal/gateway/version.go)
3. [`internal/gateway/routing.go`](internal/gateway/routing.go)

这是全项目"数据基础"最集中的一段，建议慢慢读：

- `Backend` 的六个导出字段和三个未导出字段分别做什么？`json:"..."` 标签如何工作？
- `parseConfig` 为什么先填默认值？`DisallowUnknownFields` 解决了什么问题？
- 为什么 `publicIp` 必须是 IPv4？为什么 `httpEndpoint` 不允许带路径和查询参数？
- `encodedIP` 的小端序是怎么算的？`127.0.0.1` 为什么等于 `16777343`？
- 版本公式 `year*25000 + month*1800 + day*50 + revision` 的逆运算怎么做？
- `+25` 是什么？为什么路由前要 `normalizeVersion`，而 token 里保留原值？
- `route` 的四条规则分别在什么情况下触发？为什么超范围不直接报错？

### 第 3 站：登录和 token

读 [`internal/gateway/token.go`](internal/gateway/token.go)：

- `createToken` 生成的 JSON 长什么样？`ExpiresAt` 和 `Hash` 为什么是固定值？
- `parseTokenVersion` 为什么只读 `Content.ClientVersion`，不校验 Puid 和 Hash？
- `flexibleInt` 为什么要兼容"数字"和"数字字符串"？
- `serveToken` 里 `Puid`/`Username` 为什么用 `*string`？`Language` 为什么用 `json.RawMessage`？
- 哪些字段缺失仍然签发 token？哪些错误必须返回 400？

### 第 4 站：请求分发

读 [`internal/gateway/handler.go`](internal/gateway/handler.go)：

- `gateway` 结构体为什么只保留 `config`、`client`、`health` 三个字段？`config` 为什么指向一份副本？
- `client` 为什么要统一设置超时并拒绝重定向？
- `serve` 的 `w` 为什么是普通的 `http.ResponseWriter`，而不是日志包装器？
- `serve` 如何判断路径、方法和房间码？
- `/api/games/123` 和 `/api/games/filtered` 如何区分？
- 方法校验那行 `strings.Contains(", "+allowed+", ", ...)` 为什么要补逗号和空格？
- 旧版加入房间为什么是 `POST /api/games?gameId=...`？
- token 无效时为什么要区分 `isGameLookup`？
- `RejectUnknownVersions` 为什么只拒绝游戏请求，不拒绝 `/api/user`？

### 第 5 站：转发、改写和错误处理

先读 [`internal/gateway/proxy.go`](internal/gateway/proxy.go)，再读 [`internal/gateway/response.go`](internal/gateway/response.go)。

这一段是业务核心，建议对照测试读：

- `classifyError` 的判断顺序为什么重要？`errors.Is` 和 `errors.As` 各解决什么问题？
- `sendBackendRequest` 如何继承客户端 context？为什么客户端取消时不记后端健康失败？
- `serveCreateGame` 的探测流程：什么情况算"后端拒绝创建"？什么情况算"连接失败"？
- `checkReportedAddress` 为什么只警告、不改返回值？
- `proxy` 为什么用 `EscapedPath` 而不是 `Path`？
- `rewrite` 如何区分 `Ip`、`IP`、`Game`、`games` 这几种 JSON 形状？
- `backendError` 为什么只透传 `Reason`，丢弃 `Message`？
- 老 Impostor 返回空 404 时，`missingEndpointAnswer` 如何代答？
- `/api/filtertags` 为什么是 404 Reason 17？

### 第 6 站：健康检查和访问日志

1. [`internal/gateway/health.go`](internal/gateway/health.go)
2. [`internal/gateway/logging.go`](internal/gateway/logging.go)

- 健康检查为什么"任何 HTTP 响应都算可达"？Impostor 1.9.x 根路径返回 404 说明了什么？
- `record` 为什么只更新状态、不再打日志？`Failures`/`LastError` 如何供 `/healthz` 使用？
- `logRequests` 如何读取请求 body，并用 `io.NopCloser` 放回去？
- `loggingResponseWriter` 如何同时写客户端和内存缓冲区？
- `client_ip` 和 `peer` 有什么区别？`forwardedClient` 为什么只信任本机代理传来的 `X-Real-IP`？
- 记录完整请求/响应 body 有什么隐私和内存风险？

### 第 7 站：用测试反推行为

测试文件是阅读源码的"答案册"。建议每读完一个生产文件，就读对应测试：

| 测试文件 | 对应生产文件 | 你能学到什么 |
|---|---|---|
| [`config_test.go`](internal/gateway/config_test.go) | config.go / version.go / routing.go | 边界值、开放范围、严格字段、IPv4 字节序 |
| [`token_test.go`](internal/gateway/token_test.go) | token.go | token 的逐字节格式、fuzz、benchmark |
| [`handler_test.go`](internal/gateway/handler_test.go) | handler.go / proxy.go / health.go | 端到端流程、探测、错误清理、缺失接口 |
| [`proxy_test.go`](internal/gateway/proxy_test.go) | proxy.go | 错误分类表驱动测试 |
| [`logging_test.go`](internal/gateway/logging_test.go) | logging.go | 访问日志字段、完整请求/响应 body、client_ip |
| [`helpers_test.go`](internal/gateway/helpers_test.go) | logging.go | 代理头信任规则 |

读测试时注意几个 Go 测试惯用法：

- `t.Run(name, func(t *testing.T){...})`：子测试，失败时能定位到具体用例。
- `t.Helper()`：标记辅助函数，报错行号指向调用者。
- `t.Cleanup(fn)`：测试结束时执行清理（关闭服务器、恢复日志输出）。
- `httptest.NewServer` / `httptest.NewRequest` / `httptest.NewRecorder`：不用真实网络就能测 HTTP。
- 表驱动测试：把输入和期望放进切片，循环断言。

---

## 4. 文件职责速查

| 文件 | 一句话职责 | 先看什么 |
|---|---|---|
| `cmd/gopostor/main.go` | 可执行程序入口 | `main` |
| `internal/gateway/app.go` | 启动、后台任务、优雅退出 | `Run` |
| `config.go` | 读取/校验配置，预计算 IP 和版本 | `parseConfig` |
| `version.go` | 版本整数编码/解码、`+25` 标志 | `encodeVersion`、`normalizeVersion` |
| `routing.go` | 按版本选择后端 | `route` |
| `handler.go` | HTTP 路径/方法分发、房间码校验 | `serve` |
| `token.go` | `/api/user`、token 签发与解析 | `serveToken`、`parseTokenVersion` |
| `proxy.go` | 请求后端、创建探测、错误分类、缺失接口代答 | `sendBackendRequest`、`backendError` |
| `response.go` | JSON 错误体、IP/Port 改写 | `writeJSON`、`rewrite` |
| `health.go` | 后端健康状态机 | `record`、`check`、`snapshot` |
| `logging.go` | 单行 JSON 访问日志、完整 body 捕获 | `logRequests`、`loggingResponseWriter` |

---

## 5. 关键设计问答

### 5.1 为什么 token 不做签名校验？

因为 Impostor 本身也不校验。token 只是一个 Base64 的 JSON，`Hash` 固定为 `impostor_was_here`，`ExpiresAt` 是固定过去时间。网关只从中读取 `ClientVersion` 做路由；真正的游戏身份和房间权限由后端在 UDP 阶段处理。

这样做的目的是**兼容现有客户端**：玩家可能从官方服务器或其他私服带来 token，只要里面的版本能读出来，网关就能路由。

### 5.2 `+25` 到底是什么？

Among Us 客户端可以在版本号上加 25，表示"禁用服务器权威（disable server authority）"。它是协议的一部分，不是版本的一部分。

- 路由时：`normalizeVersion` 去掉 25，否则版本会落到范围外。
- token 里：保留客户端原始值，不做修改。

编码中 `dayFactor=50`、`revision` 只允许 0..24，所以 `version % 50` 正常就是 revision；加 25 后余数变成 25..49，判断 `>= 25` 再减掉即可。

### 5.3 为什么版本超出范围还继续转发？

为了让 Impostor 在 UDP 握手时告诉玩家准确原因：

- 客户端太旧 → "请更新游戏"；
- 服务器后端太旧 → "服务器版本过旧"；
- 中间有版本空隙 → 尽量交给最近的后端。

如果网关直接返回 HTTP 错误，玩家只能看到一个笼统的失败。`rejectUnknownVersions=true` 是给运维的另一种选择：直接 `404 Reason 5`，不把请求发给可能不支持的后端。

### 5.4 为什么 IP 是一个整数？

Among Us 的匹配接口用 32 位整数表示 IPv4，而且字节序是小端序。网关在解析配置时就把 `publicIp` 预先编码成 `uint32`：

```text
127.0.0.1 的四个字节 127, 0, 0, 1
按小端序解释为 0x0100007F = 16777343
```

所以 `config.go` 里用 `binary.LittleEndian.Uint32`，`proxy.go` 里用移位还原成点分十进制，两者必须一致。

### 5.5 为什么每个响应都要改写 IP/Port？

Impostor 后端通常监听 `127.0.0.1` 或内网地址，它自报的 `PublicIp` 可能也是内网地址。玩家必须连接网关配置里的公网地址，所以网关在：

- `PUT /api/games`：直接返回配置地址；
- 房间列表：改写每个 listing 的 `IP`/`Port`；
- 过滤列表：改写 `games` 数组；
- 单房间：改写 `Game` 对象；
- 旧版加入：改写 `Ip`/`Port`。

后端自报地址与配置不一致时，只记录一次警告，仍然返回配置值。

### 5.6 为什么只透传 `Reason`，不传 `Message`？

Impostor 会把异常堆栈写进 `Message`，例如 `Unknown exception caught in filterSystem.Exception: at Impostor.Server`。把这些内容发给玩家既没用，又会泄露内部实现。客户端只依赖 `Errors[].Reason` 判断错误类型，所以网关重新构造最小错误体。

### 5.7 为什么老后端缺接口时网关要代答？

Impostor 1.9.x 不认识 16.0 之后客户端使用的新接口，ASP.NET 会返回**空 body 的 404/405**。如果直接透传，新客户端会解析失败。网关根据接口形状代答：

- `GET /api/games` → `[]`
- `GET /api/games/filtered` → `{"games":[],"metadata":{...0}}`
- `GET /api/filters` → `{"filters":[]}`
- `GET /api/games/{id}` → 404 Reason 3 + `"Game":null`
- 旧版加入 → 404 Reason 3

而 `/api/filtertags` 两个版本都没实现，所以保持 404 Reason 17。

### 5.8 为什么配置解析这么严格？

配置错误通常很难在运行时发现：比如把 `backendTimeoutMs` 写成 `backendTimeoutMillis`，如果静默忽略，程序会用默认值继续跑，问题被掩盖。`DisallowUnknownFields` 让拼写错误在启动时立刻失败；未知字段、重叠范围、非法 IP/端口、带路径的 endpoint 都会拒绝启动。

### 5.9 为什么健康检查"任何 HTTP 响应都算可达"？

因为 Impostor 1.9.x 的根路径没有 handler，会返回 404。404 说明"服务器活着，只是没有这个路由"；只有连接拒绝、超时、DNS 失败等网络层错误才说明后端不可达。

### 5.10 为什么访问日志要记录完整 body？

当前实现按“简单访问日志”的目标，把请求和响应 body 原样写进一行 JSON，方便直接看到客户端发了什么、服务器回了什么。

代价是：

- `/api/user` 的响应 body 就是 token，请求 body 可能包含 PUID/用户名，日志文件属于敏感数据；
- 请求 body 在日志读取阶段限制为 64KB：超限返回 413 并只记录前 64KB；读取失败返回 400 并记录已收到的部分，均不会继续业务处理；大房间列表仍会占用内存和磁盘；
- 日志量会明显大于只记录元信息的访问日志。

如果部署在公网，应限制日志访问权限，并根据请求量设置日志轮转。

### 5.11 并发安全体现在哪里？

- `gateway` 只创建一次，被所有请求 goroutine 共享；`config`/`client`/`health` 创建后基本只读。
- `healthMonitor.states` 由 `sync.Mutex` 保护；`snapshot` 复制值，避免序列化时与后台更新竞争。
- 每个请求的 `loggingResponseWriter` 和请求 body 缓冲区都是独立的，不需要锁。
- `log.Print` 本身是并发安全的，多行日志不会互相穿插。

### 5.12 为什么用 `json.Number`？

后端返回的 `Large` 字段可能是 `9007199254740993`，超过 float64 能精确表示的 2^53。Go 的 `encoding/json` 默认把数字解成 float64，会丢精度。`decoder.UseNumber()` 保留原始数字文本，网关原样写回客户端。

---

## 6. 把项目跑起来

```powershell
# 1. 准备配置
Copy-Item config.example.json config.json

# 2. 运行（没有真实 Impostor 也能启动，/healthz 会显示后端 down）
go run ./cmd/gopostor -config config.json

# 3. 另开一个终端：存活检查
curl.exe http://127.0.0.1:24000/
curl.exe -i http://127.0.0.1:24000/healthz

# 4. 登录拿 token（PowerShell）
$body = '{"Puid":"demo","Username":"demo","Language":0,"ClientVersion":50595550}'
$token = Invoke-RestMethod -Method Post -Uri http://127.0.0.1:24000/api/user -Body $body
$token

# 5. 带 token 请求（后端没启动时会得到 502，这是预期）
Invoke-WebRequest -Method Put -Uri http://127.0.0.1:24000/api/games -Headers @{ Authorization = "Bearer $token" }
```

常用测试命令：

```sh
go test ./...
go vet ./...
go test -race ./...          # 需要 CGO，通常在 Linux/CI 上运行
go test ./internal/gateway -fuzz=FuzzParseTokenVersion -fuzztime=30s
go test ./internal/gateway -bench=. -benchmem
```

`config.example.json` 里的两个 `publicIp` 都是 `127.0.0.1`。如果没有启动真实 Impostor，`/healthz` 会显示后端 down，这是正常的。

---

## 7. 阅读技巧

1. **先看函数签名，再看函数体。** 参数和返回值往往已经说明了职责。
2. **跟着一个请求走。** 在 `logRequests`、`serve`、`route`、`proxy` 各打一个断点（或用日志里的 `time`、`client_ip`、`method`、`path` 对照），观察同一次请求经过哪些函数。
3. **遇到不懂的 Go 语法，先查本项目里的同类用法。** 例如看到 `defer`，可以对照 `app.go` 的关闭流程、`health.go` 的解锁、`logging.go` 的收尾。
4. **不要逐行读标准库。** 把 `http.Client.Do`、`json.Decoder`、`sync.Mutex` 当成黑盒，先理解项目在什么时机调用它们。
5. **读完生产代码，立刻读对应测试。** 测试会告诉你边界条件，比如 `+25`、负房间码、空 404、错误 Message 清理。
6. **修改一点，跑一次测试。** 例如改一个日志字段名，运行 `go test ./...`，观察哪些测试依赖它。
7. **用 `go doc` 查标准库。** 例如 `go doc net/http.Server.Shutdown`、`go doc sync.Map.LoadOrStore`。

---

## 8. 可以尝试的练习

由易到难：

1. 给访问日志增加 `request_id` 字段，并把它转发给后端，练习中间件和请求头。
2. 为 `validGameID` 增加一个边界测试（例如 `-2147483648`、`2147483647`）。
3. 给 `/` 增加一个返回版本号的接口（需要新增配置字段、日志字段和测试）。
4. 为响应 body 日志增加可配置的大小上限和 `truncated` 标记（练习 `io.LimitReader` 和日志体积控制）。
5. 实现"按后端名称过滤 `/healthz` 输出"（需要修改 `snapshot` 和 handler）。
6. 尝试把 `healthMonitor` 的定时检查改成可动态调整间隔（需要新的并发控制）。

每做一步，先写测试再改代码，最后跑 `gofmt`、`go vet`、`go test ./...`。

---

## 9. 术语表

| 术语 | 含义 |
|---|---|
| Impostor | Among Us 的第三方开源服务器实现；本项目为它做前置 HTTP 网关 |
| matchmaking | 匹配阶段：登录、创建房间、加入房间、房间列表 |
| token | `/api/user` 返回的 Base64 JSON，客户端后续放在 `Authorization: Bearer` 里 |
| backend | 一个具体版本的 Impostor 实例，配置在 `backends[]` 中 |
| route | 根据 `ClientVersion` 选择 backend 的过程 |
| publicIp / publicPort | 玩家实际连接后端 UDP 的公网地址 |
| encodedIP | `publicIp` 的小端序 uint32 编码，直接放进 JSON 的 `Ip`/`IP` 字段 |
| `+25` / authority flag | 客户端"禁用服务器权威"标志，路由前去掉 |
| Reason | Impostor 错误体里的数字原因码，如 3=房间不存在、5=版本不支持、11=未授权、17=通用错误 |
| healthz | 网关自身的健康检查接口，有后端 down 时返回 503 |
| access log | 每个请求一行 JSON，包含时间、来源、请求/响应和耗时 |
| client_ip / peer | 客户端 IP（可能是代理转发的）/ TCP 直连对端地址 |

---

读完之后，如果你能不看代码回答下面三个问题，说明主线已经掌握了：

1. 一个 `ClientVersion=50595575` 的客户端，`+25` 去掉后是多少？会路由到示例配置的哪个后端？
2. `PUT /api/games` 在后端不可用、后端返回 500、关闭探测三种情况下分别返回什么？
3. Impostor 1.9.x 对 `GET /api/games/filtered` 返回空 404 时，客户端最终收到什么？
