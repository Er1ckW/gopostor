# Gopostor

Gopostor 是使用 Go 标准库编写的 Among Us / Impostor HTTP 匹配网关。玩家只需设置一个服务器地址，网关在 HTTP 匹配阶段根据客户端上报的版本选择对应的 Impostor 后端，并返回其公网 IP 和 UDP 端口；后续游戏流量直接连接该后端。

每个需要支持的 Impostor 版本单独运行一个后端实例。仓库提供 Impostor 1.9.3 和 1.10.9 的配置示例，不包含 Impostor 后端源码。编译后的网关只需要二进制文件和实际配置 `config.json`，无需安装 Go 或第三方运行时。

## 构建与运行

在 gopostor 目录内执行（Go 1.22 或更高）：

```sh
cp config.example.json config.json
# 修改 config.json 中的后端地址、端口和版本范围
go build -o gopostor ./cmd/gopostor
./gopostor -config config.json
```

Windows PowerShell 可用 `Copy-Item config.example.json config.json` 准备配置，使用 `go build -o gopostor.exe ./cmd/gopostor` 构建，再执行 `.\gopostor.exe -config config.json`。

开发时也可在项目根目录执行 `go run ./cmd/gopostor -config config.json`。

默认配置路径是工作目录下的 `config.json`。配置在启动时加载，修改后重启。`config.example.json` 是纳入版本管理的示例；实际配置 `config.json` 已加入 `.gitignore`，部署时单独准备。

Windows PowerShell 上交叉编译 Linux amd64：

```powershell
$env:GOOS = 'linux'
$env:GOARCH = 'amd64'
$env:CGO_ENABLED = '0'
go build -trimpath -o dist/gopostor ./cmd/gopostor
```

ARM64 服务器将 GOARCH 改为 arm64。上传对应架构的二进制文件。

## 项目结构

按照 [Go 官方对服务项目的目录建议](https://go.dev/doc/modules/layout#server-project)，`cmd/` 放可执行程序入口，`internal/` 放内部实现。网关实现保留为一个包，包内按文件划分职责。

```text
.
├── go.mod
├── config.example.json          # 配置示例，复制为 config.json 后修改
├── cmd/
│   └── gopostor/
│       └── main.go              # 命令行入口
├── internal/gateway/
│   ├── app.go                   # 启动、后台任务和优雅退出
│   ├── config.go                # 配置加载、校验和提示
│   ├── version.go               # 协议版本解析、编码和标志处理
│   ├── routing.go               # 后端版本选择和路由结果说明
│   ├── handler.go               # HTTP 请求分发和房间码校验
│   ├── token.go                 # token 签发、解析、字段读取和 /api/user
│   ├── proxy.go                 # 后端请求、创建探测、错误分类和兼容处理
│   ├── response.go              # JSON 响应、错误响应和公网地址改写
│   ├── health.go                # 后端健康检查和状态
│   ├── logging.go               # 单行 JSON 访问日志
│   └── *_test.go                # 同包测试
└── deploy/
    ├── nginx/                   # Nginx 配置示例
    └── systemd/gopostor.service  # systemd 服务示例
```

构建产物（如 `dist/`）、日志、覆盖率和性能分析文件、临时文件与本地编辑器配置也已加入 `.gitignore`。

## 配置

| 字段 | 默认值 | 说明 |
|---|---|---|
| `listenIp` / `listenPort` | `127.0.0.1` / `24000` | 网关监听地址，前面由 Nginx 提供 HTTPS |
| `backendTimeoutMs` | `5000` | 单次后端请求超时 |
| `rejectUnknownVersions` | `false` | 版本不在任何范围内时：`false` 转给最近的后端，由 Impostor 告诉玩家该更新游戏还是服务器过旧；`true` 时游戏请求直接返回 `404 Reason 5` |
| `probeBackendOnCreate` | `true` | 创建房间前先向后端发送 `PUT /api/games`，后端不可用时立即返回错误，而不是让玩家等到 UDP 超时 |
| `healthCheckIntervalSec` | `30` | 后台检查每个后端 `GET /` 的间隔，`0` 只在启动时检查 |
| `backends[]` | | 每个后端的 `name`、`minVersion`、`maxVersion`、`publicIp`、`publicPort`、`httpEndpoint` |

`statsIntervalSec` 已废弃：旧配置仍然可以解析，但当前版本不再输出统计汇总。

以下为 [示例配置](config.example.json) 中的两个后端。版本范围应根据实际部署的 Impostor 所支持的客户端协议版本调整。

配置按 JSON 严格解析：出现未知字段（通常是拼写错误）会在启动时直接报错，不会静默回退到默认值。示例中的 `source` 仅用于记录版本范围的来源，可以省略。

| 示例实例 | 内部协议范围 | HTTP 地址 | 公网 UDP 端口 |
|---|---|---|---|
| 1.9.3 | 2023.10.1—2024.2.3 | http://127.0.0.1:23001 | 22031 |
| 1.10.9 | 2024.3.1—2026.7.21 | http://127.0.0.1:23002 | 22032 |

版本范围包含两端，不能重叠。版本可以写成 `2023.10.1` / `2022.2.24.1`，也可以写成客户端上报的整数（如 `50595550`）；`minVersion` 留空表示不设下限，`maxVersion` 留空表示不设上限。版本编码为 `年*25000 + 月*1800 + 日*50 + revision`，路由时去掉 +25 标志，token 保留原始版本。

部署前修改 `publicIp`、`publicPort`、`httpEndpoint`。公网 IP 必须为 IPv4；示例回环地址仅供本机测试，正式部署必须改成玩家可连通的公网 IPv4。客户端 HTTPS 由 Nginx 转发到网关，游戏 UDP 直接连到对应的 Impostor。

### 版本路由

1. 版本在某个范围内：转给该后端。
2. 低于所有范围：转给最老的后端（`below_range`）。
3. 高于所有范围或落在两个范围之间：转给版本下限不高于它的最近后端（`above_range` / `between_ranges`）。Among Us 更新后，新客户端会先到达最新后端，由后端在 UDP 握手时判断是否支持。

2 和 3 都会在日志中提示；`rejectUnknownVersions=true` 时这些客户端仍能拿到 token，但创建/加入/列表请求返回 `404 {"Errors":[{"Reason":5}]}`。

## 协议

支持 POST /api/user、GET/PUT/POST /api/games、GET /api/games/{gameId}、GET /api/games/filtered、GET /api/filters、GET /api/filtertags；GET/HEAD / 是网关运行检查，GET/HEAD /healthz 返回各后端健康状态（有后端不可达时返回 503）。

- **/api/user**：只要求有效的 `ClientVersion`（数字或数字字符串）。`Puid`、`Username`、`Language` 缺失或为空时照常签发 token，与 Impostor 一样使用 Base64 JSON、固定 Hash 和默认过期时间。token 不校验 EOS 签名。
- **PUT /api/games**：先探测后端（见 `probeBackendOnCreate`），再返回配置中的公网地址。后端自报的 PublicIp/PublicPort 与配置不一致时记录警告。
- **查询类请求**：携带原 Authorization 转发给后端，并把返回的 IP/端口改写成配置值。
- **后端错误**：只把 `Errors[].Reason` 转给客户端，丢弃 Impostor 写在 `Message` 里的异常文本。后端没有该接口（Impostor 1.9.x 对 16.0 以后的接口返回空 404）时，由网关返回空列表或"房间不存在"（Reason 3）。`GET /api/games/{id}` 的错误响应总是带 `"Game":null`。`/api/filtertags` 是例外：1.9.3 和 1.10.9 都没有实现它，网关返回 `404 Reason 17`，与旧行为一致。
- **网关错误码**：token 缺失/无效 `401 Reason 11`；后端连接失败、超时、重定向或无效响应 `502 Reason 17`；房间码格式错误 `400 Reason 3`。

**后端仍需正确处理房间列表的版本兼容性过滤，并保持 `Compatibility.AllowVersionMixing=false`。** Go 网关不替代后端的房间版本过滤。所有后端都必须启用内部 HTTP 服务。

## systemd

将 Linux 二进制 `gopostor`、实际配置 `config.json` 和 [gopostor.service](deploy/systemd/gopostor.service) 上传至服务器同一临时目录，在该目录执行：

```sh
id gopostor >/dev/null 2>&1 || sudo useradd --system --user-group --no-create-home --shell /usr/sbin/nologin gopostor
sudo install -d -m 0755 /opt/gopostor
sudo install -m 0755 gopostor /opt/gopostor/gopostor
sudo install -m 0644 config.json /opt/gopostor/config.json
sudo install -m 0644 gopostor.service /etc/systemd/system/gopostor.service
```

修改服务器配置后启动。切换已有服务时，先停止占用相同监听端口的旧服务。

```sh
sudo systemd-analyze verify /etc/systemd/system/gopostor.service
sudo systemctl daemon-reload
sudo systemctl enable --now gopostor.service
sudo systemctl status gopostor.service --no-pager
curl --fail http://127.0.0.1:24000/healthz
sudo journalctl -u gopostor.service -f
```

更新二进制前停止服务，安装后再启动；保留服务器已修改的配置文件。只改配置时执行 `sudo systemctl restart gopostor.service`。服务异常退出后 5 秒重启，60 秒内最多启动 5 次；修复重复启动失败的原因后执行 `sudo systemctl reset-failed gopostor.service` 再启动。

## Nginx

完整配置在 [deploy/nginx/](deploy/nginx/)：`gopostor.conf` 定义 80/443 两个 server，`gopostor.proxy.inc` 是两者共用的转发设置。以下命令在项目根目录执行：

```sh
sudo certbot certonly --nginx -d matchmaker.example.com   # 先申请证书，否则 443 的 server 无法通过 nginx -t
sudo mkdir -p /var/www/certbot
sudo install -m 0644 deploy/nginx/gopostor.conf deploy/nginx/gopostor.proxy.inc /etc/nginx/conf.d/
sudo sed -i 's/matchmaker.example.com/你的域名/g' /etc/nginx/conf.d/gopostor.conf
sudo nginx -t && sudo systemctl reload nginx
```

- 80 端口不跳转 HTTPS：重定向后的 POST/PUT 会变成 GET，匹配会失败。手机必须走 HTTPS。
- 转发 `Authorization`；`X-Real-IP` 会作为 `client_ip` 显示在 gopostor 日志中。
- `/healthz` 只允许本机访问；普通匹配请求按客户端 IP 限 20 次/秒、突发 40，超出返回 429，网吧等多人共用 IP 时可调大。
- 不对上游重试，避免同一个 PUT 被发送两次；`proxy_read_timeout 15s` 大于 `backendTimeoutMs`。
- 访问日志 `/var/log/nginx/gopostor.access.log` 记录 Nginx 侧的请求行、状态和上游耗时，可与 gopostor 的同名请求对照。
- 中国大陆服务器的 80/443 端口通常要求域名已备案，否则会被云厂商拦截，表现为客户端连不上、gopostor 也没有任何请求日志。

## 访问日志

所有日志写入 stderr，由 systemd 收集。每个请求只输出一行 JSON，`log` 不再添加时间戳前缀，采集器可以直接按行解析。字段固定为：

| 字段 | 含义 |
|---|---|
| `time` | 请求结束时间（RFC3339Nano） |
| `client_ip` | 客户端 IP；直连对端是本机代理时优先取 `X-Real-IP` / `X-Forwarded-For` |
| `peer` | TCP 直连对端的 `host:port` |
| `method` / `path` / `query` | 请求方法、路径、原始查询参数 |
| `user_agent` | 请求的 User-Agent |
| `request_content_type` | 请求的 Content-Type |
| `request_body` | 完整请求体 |
| `authorization_token` | Authorization 中的 token 经 Base64 解码后的 JSON，包含 `Content.ClientVersion`、`Puid` 等；没有 token 或无法解码时省略。解码不代表签名或身份已验证 |
| `status` | 响应状态码 |
| `response_content_type` | 响应的 Content-Type |
| `response_body` | 完整响应体 |
| `duration_ms` | 从读取请求体到写完响应的耗时（毫秒） |

示例：

```json
{"time":"2026-10-18T16:02:21.123+08:00","client_ip":"203.0.113.9","peer":"127.0.0.1:54321","method":"POST","path":"/api/user","query":"","user_agent":"AmongUs/...","request_content_type":"application/json","request_body":"{\"Puid\":\"...\",\"ClientVersion\":50595550}","status":200,"response_content_type":"text/plain; charset=utf-8","response_body":"eyJDb250ZW50...","duration_ms":12}
```

查看：

```sh
sudo journalctl -u gopostor -f -o cat
```

请求 body 上限为 64KB（65536 字节），在日志读取阶段生效；超限返回 HTTP 413，读取失败返回 HTTP 400，均不会进入业务处理。正常请求完整记录 body；超限请求只记录前 64KB，读取失败只记录已收到的部分，错误响应也记录在同一行日志中。

> **安全警告**：请求和响应 body 会原样写入日志。`/api/user` 的响应 body 就是 token，请求 body 可能包含 `Puid`、`Username`；日志文件因此属于敏感数据，必须限制读取权限。大房间列表和高请求量仍会增加内存和磁盘压力。

配置成功、后端列表、健康状态变化、统计汇总、warn-once 提示都不再单独输出日志；后端状态仍可通过 `/healthz` 查看。请求在进入 Go handler 之前失败（例如 HTTP 解析错误）不会被记录，后端 UDP 握手也不会记录。

若客户端报错而没有对应请求日志，请检查 Nginx 日志和请求目标地址：请求可能未进入 Go handler，或在 HTTP 协议解析阶段被拒绝。v2023.11.28 的内部版本为 2023.11.15（50595550，带标志为 50595575）。

## 测试命令

```sh
go test ./...
go vet ./...
go test -race ./...   # 需要 CGO，通常在 Linux/CI 上执行
```

默认测试可在单独克隆的本仓库中运行，读取提交的 `config.example.json`，不依赖本地实际配置或外部 Impostor 源码。测试包含本地模拟后端、token 原始 JSON 格式与往返、宽松字段、各版本范围与回退路由、+25 标志、负数房间码、地址改写、创建探测、后端错误清理、1.9.x 缺失接口、健康检查和访问日志字段。仓库的 GitHub Actions 会执行 `gofmt`、`go vet` 和 `go test -race`。

`parseTokenVersion` 还有一个 fuzz 目标：

```sh
go test ./internal/gateway -fuzz=FuzzParseTokenVersion -fuzztime=30s
```

如需额外核对 Impostor 的支持名单，设置环境变量 `IMPOSTOR_SOURCE_ROOT`，指向同时包含 `Impostor-1.9.3/` 和 `Impostor-1.10.9/` 的目录。测试会读取其中的 `src/Impostor.Server/Net/Manager/CompatibilityManager.cs`。未设置该变量时只跳过 `TestReleaseCompatibility`；设置后源码缺失或核对不一致都会使测试失败。

```sh
IMPOSTOR_SOURCE_ROOT=/path/to/impostor-sources go test ./... -run '^TestReleaseCompatibility$'
```

PowerShell 中先设置 `$env:IMPOSTOR_SOURCE_ROOT = 'D:\path\to\impostor-sources'`，再执行 `go test ./... -run '^TestReleaseCompatibility$'`。

真实游戏客户端和 Linux systemd 启停需在部署环境联调。
