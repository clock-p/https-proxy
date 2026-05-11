# clockbridge

目标：在 **1 个域名 + 只对外 443** 的约束下，把内网 App（比如 code-server）通过 HTTPS 代理到公网。

约束/设计要点：

- 证书由 Nginx 统一管理（本仓库的 Gateway 只跑明文 HTTP/WS，由 Nginx 反代）
- Agent 通过 WebSocket（ws/wss）注册到 Gateway，维持长连接
- Client 通过 `https://<DOMAIN>/https-proxy/<UUID>/...` 访问
- HTTP 全量透传：支持 WebSocket Upgrade、流式响应、大文件传输、Trailers
- 浏览器 WebSocket 无法自定义 `Authorization` 请求头；Gateway 支持把 client URL 上的 `access_token` / `authorization` / `token` 查询参数映射为上游 `Authorization: Bearer <token>`，并在转发前从 query 中移除。

## 本地最小联调（不依赖 Nginx）

1) 启动 mock app（本地 18081）：

```bash
go run ./cmd/mockapp --listen 127.0.0.1:18081 --base /aaa
```

2) 启动 gateway（本地 18080）：

```bash
HTTPS_PROXY_AGENT_TOKENS=<TOKEN> go run ./cmd/gateway --listen 127.0.0.1:18080
```

3) 启动 clockbridge-cli（注册到 gateway，并转发到 mock app）：

```bash
go run ./cmd/clockbridge-cli \
  -x-token <TOKEN> \
  -R http://127.0.0.1:18081/aaa \
  u1@http://127.0.0.1:18080
```

4) 用 mock client 验证（HTTP/stream/ws/Trailers/1xx）：

```bash
go run ./cmd/mockclient --base http://127.0.0.1:18080/u1
go run ./cmd/mockclient --base http://127.0.0.1:18080/u1 --mode stream
go run ./cmd/mockclient --base http://127.0.0.1:18080/u1 --mode ws
go run ./cmd/mockclient --base http://127.0.0.1:18080/u1 --mode ws-close-client
go run ./cmd/mockclient --base http://127.0.0.1:18080/u1 --mode ws-close-up
go run ./cmd/mockclient --base http://127.0.0.1:18080/u1 --mode resp-trailer
go run ./cmd/mockclient --base http://127.0.0.1:18080/u1 --mode req-trailer
go run ./cmd/mockclient --base http://127.0.0.1:18080/u1 --mode interim
go run ./cmd/mockclient --base http://127.0.0.1:18080/u1 --mode big-post --size $((50*1024*1024))
```

浏览器直连 WS 可使用 query token：

```text
wss://<uuid>.example.com/ws?access_token=<TOKEN>
wss://<uuid>.example.com/ws?authorization=Bearer%20<TOKEN>
```

## 接入 Nginx（线上域名）

按约定：`https://<DOMAIN>/https-proxy/` 由 Nginx 反代到本机 `127.0.0.1:<gateway_port>`。

关键核对（保持长连接）：

- `proxy_http_version 1.1` + `Upgrade/Connection` 转发
- `proxy_read_timeout` / `proxy_send_timeout` 需 >= `HTTPS_PROXY_STREAM_IDLE_TIMEOUT`（当前建议 12h）

建议片段（/https-proxy/ 这一段）：

```nginx
location ^~ /https-proxy/ {
  proxy_http_version 1.1;
  proxy_set_header Upgrade $http_upgrade;
  proxy_set_header Connection $connection_upgrade;
  proxy_read_timeout 43200s;
  proxy_send_timeout 43200s;
  proxy_pass http://127.0.0.1:18080/;
}
```

## 运行手册（Gateway/CLI）

Gateway 启动：

```bash
HTTPS_PROXY_AGENT_TOKENS=<TOKEN> \
HTTPS_PROXY_STREAM_IDLE_TIMEOUT=12h \
go run ./cmd/gateway --listen 127.0.0.1:18080
```

clockbridge-cli 采用 SSH 风格：

```bash
clockbridge-cli -R <target_url> <uuid>@<register_host>
clockbridge-cli -L [bind_addr:]<port> <upstream_url>
```

注意：`clockbridge-cli` 参数遵循 Go flag 规则，选项请放在位置参数之前。

### 1) `-R`：注册隧道（本地服务暴露到受管域名）

```bash
clockbridge-cli \
  -i /path/to/token.txt \
  -x-token <x_token_optional> \
  --proxy-mode legacy_framed \
  --register-ip 10.0.0.12 \
  -R http://127.0.0.1:<app_port>/<base_path> \
  <uuid>@register-https-proxy.example.com
```

`<target_url>` 支持：

- `http(s)://...`：现有 HTTP/WS 反代语义
- `tcp://host:port`：把客户端发起的 WebSocket 连接转发到本地 TCP 服务（如 `tcp://127.0.0.1:22`）

`proxy_mode` 与 target 约束：

- `legacy_framed`：支持 `http(s)` / `tcp` / `ws(s)` target
- `transparent_tunnel`：当前支持 `tcp://host:port` 与 `ws(s)://host[:port]/...` target
- `transparent_tunnel`：当前仅作用于 client WebSocket 请求路径；普通 HTTP 请求仍走 legacy 语义
- `transparent_tunnel + ws(s)`：网关/agent 在升级完成后进入 raw relay（不做 WS message 级解析），用于提升 `wstunnel` 等隧道兼容性

效果：

- clockbridge-cli 会构造 `wss://register-https-proxy.example.com/register?uuid=<uuid>` 进行注册。
- client 侧仍通过受管地址访问：`https://<uuid>.example.com/...` 或 path 模式。
- 若设置 `--register-ip`，TCP 实际连接会改为该地址（支持 `ip`/`ip:port`/`hostname[:port]`），但请求 Host 与 TLS SNI 仍使用 `<register_host>`。
- `--proxy-mode` 会随 register 请求携带 `proxy_mode` 能力（当前支持：`legacy_framed`、`transparent_tunnel`）。
- `clockbridge-cli` 会基于 `-R` target 自动附带 `target_kind`（`http`/`tcp`/`ws`），供网关选择 transparent 子路径。

`/status` 返回说明（兼容旧字段）：

- 保留：`ok`、`count`、`uuids`
- `proxy_modes`：`uuid -> effective_proxy_mode`（当前生效模式）
- `proxy_modes_requested`：`uuid -> requested_proxy_mode`（agent 注册声明）
- `proxy_mode_reasons`：`uuid -> resolve_reason`（模式决策原因）
- `target_kinds`：`uuid -> target_kind`（register 声明）
- `proxy_mode_policy`：当前网关策略快照（`transparent_enabled`、`force_legacy_mode`、`allow_uuids`、`allow_prefixes`）

`<register_host>` 协议规则：

- 未带 scheme：默认按 `https` 处理（即注册用 `wss`）
- `http://...`：注册用 `ws`
- `https://...`：注册用 `wss`

`--register-ip` 规则：

- 仅 `-R` 模式可用；`-L` 模式传入会报错
- 支持 `ip`、`ip:port` 或 `hostname[:port]`（IPv6 端口写法如 `[fd00::1]:8443`）
- 传域名时会在本地做 DNS 解析（查询顺序：A 记录 -> AAAA）
- 若只给 `ip`，端口按注册协议默认值补齐：`wss/https -> 443`，`ws/http -> 80`
- 若给 `ip:port` 或 `hostname:port`，按显式端口连接

### 2) `-L`：本地反向代理（受管域名回流到本地端口）

```bash
clockbridge-cli \
  -i /path/to/token.txt \
  -L 127.0.0.1:28789 \
  https://<uuid>.example.com/
```

效果：

- 本地 worker 只连 `http://127.0.0.1:28789`。
- clockbridge-cli 会把 HTTP/WS 转发到受管 HTTPS 域名。
- 会自动注入 `Authorization: Bearer <token>`（来自 `-i` / `--token` / 环境变量）。

### 3) 认证参数

- `-i <file>`：Bearer token 文件（ssh 风格 identity file）
- `--token <token>`：Bearer token 直传（调试）
- `-x-token <token>`：仅 `-R` 模式使用（X-Token 头）
- `--proxy-mode <legacy_framed|transparent_tunnel>`：仅 `-R` 模式使用；声明实例数据面模式（默认 `legacy_framed`，`transparent_tunnel` 当前支持 `tcp://` 与 `ws(s)://` target）
- `--register-ip <ip|ip:port|hostname[:port]>`：仅 `-R` 模式使用，覆盖 register 的 TCP 连接目标；适用于“连内网 IP/域名但保持域名 Host/SNI”
- 环境变量兜底：
  - `CLOCKBRIDGE_HTTPS_PROXY_TOKEN`
  - `CLOCKBRIDGE_HTTPS_PROXY_TOKEN_PATH`

版本信息：

```bash
./clockbridge-gateway --version
./clockbridge-cli --version
```

环境变量（Gateway）：

- `HTTPS_PROXY_AGENT_TOKENS`：Agent 注册 token 列表（逗号分隔）
- `HTTPS_PROXY_MAX_STREAMS_PER_AGENT`：单 uuid 最大并发（默认 0，表示不限制）
- `HTTPS_PROXY_MAX_BODY_BYTES`：单请求 body 上限（默认 512MB）
- `HTTPS_PROXY_MAX_HEADER_BYTES`：请求 header 上限（默认 1MB）
- `HTTPS_PROXY_STREAM_IDLE_TIMEOUT`：响应流空闲超时（默认 5m，当前建议 12h）
- `HTTPS_PROXY_HEARTBEAT_TIMEOUT`：Agent 心跳超时（默认 60s，设为 `0` 关闭心跳监测）
- `HTTPS_PROXY_FIRST_RESPONSE_TIMEOUT`：等待首包超时（默认 30s，设为 `0` 关闭）
- `HTTPS_PROXY_WS_OPEN_TIMEOUT`：等待 WS 握手超时（默认 10s，设为 `0` 关闭）
- `HTTPS_PROXY_CLIENT_WS_PING_INTERVAL`：gateway 主动向 client WebSocket 发送 ping 的间隔（默认 `0` 关闭；反代链路存在 idle timeout 时建议设置为 `25s`）
- `HTTPS_PROXY_CLIENT_WS_PING_DEBUG`：记录 client WebSocket ping/pong 调试日志（默认 `false`）
- `HTTPS_PROXY_HTTP_READ_HEADER_TIMEOUT`：网关 HTTP 读请求头超时（默认 5s，设为 `0` 关闭）
- `HTTPS_PROXY_HTTP_IDLE_TIMEOUT`：网关 HTTP KeepAlive 空闲超时（默认 120s，设为 `0` 关闭）
- `HTTPS_PROXY_TRANSPARENT_ENABLED`：是否允许 transparent 能力进入生效决策（默认 `false`）
- `HTTPS_PROXY_TRANSPARENT_ALLOW_UUIDS`：transparent 白名单（精确 uuid，逗号分隔）
- `HTTPS_PROXY_TRANSPARENT_ALLOW_PREFIXES`：transparent 白名单（uuid 前缀，逗号分隔）
- `HTTPS_PROXY_FORCE_LEGACY_MODE`：强制全部回退 legacy（默认 `false`）
- WebSocket 单条消息读取上限：10MB（gateway/client、gateway/cli、cli/upstream 统一）

## Phase5：code-server 轻量验证（本地）

注意：机器内存不多，避免压测或并发开太大。

1) 启动 code-server（本地 19090，无鉴权，关闭遥测/更新）：

```bash
code-server \
  --bind-addr 127.0.0.1:19090 \
  --auth none \
  --disable-telemetry \
  --disable-update-check \
  --user-data-dir /tmp/https-proxy-cs-data \
  --extensions-dir /tmp/https-proxy-cs-ext
```

2) 启动 gateway + clockbridge-cli（uuid=cs1）：

```bash
HTTPS_PROXY_AGENT_TOKENS=<TOKEN> go run ./cmd/gateway --listen 127.0.0.1:19080
```

```bash
go run ./cmd/clockbridge-cli \
  -x-token <TOKEN> \
  -R http://127.0.0.1:19090/ \
  cs1@http://127.0.0.1:19080
```

3) 轻量验证（页面与静态资源）：

```bash
curl --http1.1 -s http://127.0.0.1:19080/cs1/ | head -n 5
curl -I --http1.1 http://127.0.0.1:19080/cs1/_static/src/browser/media/pwa-icon-192.png
```

4) 轻量验证（WS 通路，healthz）：

```bash
node -e "const ws=new WebSocket('ws://127.0.0.1:19080/cs1/healthz',{headers:{Origin:'http://127.0.0.1:19080'}});const t=setTimeout(()=>{console.error('timeout');process.exit(2)},2000);ws.onopen=()=>ws.send('ping');ws.onmessage=(e)=>{console.log(String(e.data));clearTimeout(t);ws.close();};ws.onclose=()=>process.exit(0);ws.onerror=(e)=>{console.error('err',e.message||e);clearTimeout(t);process.exit(1);};"
```

5) 轻量验证（大文件上传/下载，走 code-server /proxy）：

```bash
truncate -s 50M /tmp/https-proxy-big.bin
cat > /tmp/https-proxy-bigserver.py <<'PY'
from http.server import HTTPServer, BaseHTTPRequestHandler
import os
BIG_PATH = "/tmp/https-proxy-big.bin"
class Handler(BaseHTTPRequestHandler):
    def log_message(self, fmt, *args): return
    def do_GET(self):
        if self.path != "/big.bin": self.send_response(404); self.end_headers(); return
        st = os.stat(BIG_PATH)
        self.send_response(200)
        self.send_header("Content-Type", "application/octet-stream")
        self.send_header("Content-Length", str(st.st_size))
        self.end_headers()
        with open(BIG_PATH, "rb") as f:
            while True:
                data = f.read(64 * 1024)
                if not data: break
                self.wfile.write(data)
    def do_POST(self):
        if self.path != "/upload": self.send_response(404); self.end_headers(); return
        length = int(self.headers.get("Content-Length", "0"))
        remaining, total = length, 0
        while remaining > 0:
            chunk = self.rfile.read(min(64 * 1024, remaining))
            if not chunk: break
            total += len(chunk); remaining -= len(chunk)
        self.send_response(200); self.send_header("Content-Type","text/plain"); self.end_headers()
        self.wfile.write(str(total).encode())
HTTPServer(("127.0.0.1", 19091), Handler).serve_forever()
PY
python3 /tmp/https-proxy-bigserver.py
```

另开终端：

```bash
curl --http1.1 -o /tmp/https-proxy-dl.bin http://127.0.0.1:19080/cs1/proxy/19091/big.bin
stat -c '%s' /tmp/https-proxy-big.bin /tmp/https-proxy-dl.bin
curl --http1.1 -X POST --data-binary @/tmp/https-proxy-big.bin http://127.0.0.1:19080/cs1/proxy/19091/upload
```

## 文档

- 项目目录与关键约束：`AGENTS.md`
- 对外行为说明：本文档（`README.md`）
