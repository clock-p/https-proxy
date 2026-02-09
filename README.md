# https-proxy

目标：在 **1 个域名 + 只对外 443** 的约束下，把内网 App（比如 code-server）通过 HTTPS 代理到公网。

约束/设计要点：

- 证书由 Nginx 统一管理（本仓库的 Gateway 只跑明文 HTTP/WS，由 Nginx 反代）
- Agent 通过 WebSocket（ws/wss）注册到 Gateway，维持长连接
- Client 通过 `https://<DOMAIN>/https-proxy/<UUID>/...` 访问
- HTTP 全量透传：支持 WebSocket Upgrade、流式响应、大文件传输、Trailers

## 本地最小联调（不依赖 Nginx）

1) 启动 mock app（本地 18081）：

```bash
go run ./cmd/mockapp --listen 127.0.0.1:18081 --base /aaa
```

2) 启动 gateway（本地 18080）：

```bash
HTTPS_PROXY_AGENT_TOKENS=<TOKEN> go run ./cmd/gateway --listen 127.0.0.1:18080
```

3) 启动 agent（注册到 gateway，并转发到 mock app）：

```bash
go run ./cmd/agent \
  --register-url ws://127.0.0.1:18080/register?uuid=u1 \
  --token <TOKEN> \
  --target http://127.0.0.1:18081/aaa
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

## 运行手册（Gateway/Agent）

Gateway 启动：

```bash
HTTPS_PROXY_AGENT_TOKENS=<TOKEN> \
HTTPS_PROXY_STREAM_IDLE_TIMEOUT=12h \
go run ./cmd/gateway --listen 127.0.0.1:18080
```

Agent 启动（每个 uuid 一条连接）：

```bash
go run ./cmd/agent \
  --register-url ws://127.0.0.1:18080/register?uuid=<UUID> \
  --token <TOKEN> \
  --target http://127.0.0.1:<app_port>/<base_path>
```

版本信息：

```bash
./gateway --version
./agent --version
```

环境变量（Gateway）：

- `HTTPS_PROXY_AGENT_TOKENS`：Agent 注册 token 列表（逗号分隔）
- `HTTPS_PROXY_MAX_STREAMS_PER_AGENT`：单 uuid 最大并发（默认 0，表示不限制）
- `HTTPS_PROXY_MAX_BODY_BYTES`：单请求 body 上限（默认 512MB）
- `HTTPS_PROXY_MAX_HEADER_BYTES`：请求 header 上限（默认 1MB）
- `HTTPS_PROXY_STREAM_IDLE_TIMEOUT`：响应流空闲超时（默认 5m，当前建议 12h）
- WebSocket 单条消息读取上限：10MB（gateway/client、gateway/agent、agent/upstream 统一）

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

2) 启动 gateway + agent（uuid=cs1）：

```bash
HTTPS_PROXY_AGENT_TOKENS=<TOKEN> go run ./cmd/gateway --listen 127.0.0.1:19080
```

```bash
go run ./cmd/agent \
  --register-url ws://127.0.0.1:19080/register?uuid=cs1 \
  --token <TOKEN> \
  --target http://127.0.0.1:19090/
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

- 设计文档：`design.md`
- 执行清单：`todo.md`
