# https-proxy 设计文档

## 目标

在“**只有 1 个域名、对外只开 1 个端口（443）**”的限制下，把内网 App（例如 code-server）通过 HTTPS 代理到公网，要求：

- 支持 WebSocket（Upgrade、双向消息、close 传播）
- 支持流式响应（chunk/flush 边到边）
- 支持大文件传输（请求/响应 body 不整包缓存）
- 支持 HTTP Trailer / 1xx（尽量保持语义完整）
- 不依赖 Cloudflare / SSH / FRP

## 名词

- Client：真实访问者（浏览器/脚本）
- Nginx：对外入口（TLS 终止，443）
- Gateway：https-proxy 服务（仅明文 HTTP/WS，被 Nginx 反代）
- Agent：内网机器上的隧道客户端（与 Gateway 建立长连接）
- App：Agent 转发到的本地服务（code-server 或任意 HTTP 服务）

## 约束与约定（固定）

- 对外仅 `443`（Nginx）
- Gateway 不持有证书、不做 TLS：只提供 `http/ws`，由 Nginx 反代
- 入口 URL（对外统一在一个 host 上）：
  - Agent 注册：`wss://<DOMAIN>/https-proxy/register?uuid=<UUID>`
  - Client 访问：`https://<DOMAIN>/https-proxy/<UUID>/xxx/yyy`
- Nginx 必须 **剥掉** `/https-proxy/` 前缀再转发给 Gateway

## 端到端链路（关键流程）

### 1) Agent 注册（长连接）

- Agent 连接：`/https-proxy/register?uuid=<UUID>`
- Header：`X-Token: <TOKEN>`
- Gateway 行为：
  - 校验 Token
  - 若 uuid 已被活跃连接占用 → 拒绝
  - 否则注册该 uuid，维持 WebSocket 长连接
  - 心跳：Agent 每 20s 发 ping；Gateway 超过 60s 未收到 ping 视为断线

### 2) Client 访问（HTTP/WS）

- Client 访问：`/https-proxy/<UUID>/...`
- Nginx：
  - `location ^~ /https-proxy/` 将请求反代到 Gateway
  - 剥离前缀：`/https-proxy/<UUID>/x` → 上游 `/<UUID>/x`
  - 必须支持 WebSocket 代理（`Upgrade/Connection` + `proxy_http_version 1.1`）
- Gateway：
  - 通过 uuid 查找在线 Agent
  - 将请求通过 Agent 长连接转发到目标 App，并把响应回传给 Client

### 3) Agent 转发行为（路径拼接）

- Agent 配置一个完整的 `target base URL`，例如：`http://127.0.0.1:8080/aaa`
- Gateway 侧收到 suffix path `/xxx/yyy` 后，Agent 转发到：`http://127.0.0.1:8080/aaa/xxx/yyy`

## 设计取舍

### 多路复用/并发策略

不强求 yamux；优先保证功能与正确性，并兼顾性能。

建议路线（MVP → 演进）：

1) MVP：Agent 维持 WebSocket **连接池**（同 uuid 多连接），Gateway 按请求分配连接
   - 优点：实现快、坑少、足够支撑 code-server 并发
   - 缺点：连接数增多（但对外仍然只有 443）
2) 需要进一步压榨连接数/开销时：再收敛为单连接 multiplex（自定义 framing 或引入成熟 mux）

### HTTP 全量透传原则

- body 以流方式转发：不做整包缓存
- headers 基本透传，但过滤 hop-by-hop headers（Connection/Upgrade/Transfer-Encoding 等）
- Trailer：支持 request/response trailers 的透传
- 1xx：尽量保留（Gateway 收到后直接写给 Client）
- WebSocket：
  - Gateway 识别 Upgrade
  - 代理层做“消息帧”转发（text/binary），并正确传播 close

## 安全与可观测性

- Auth：仅 Agent register 需要 token；Client 访问由 Nginx 暴露路径路由（是否对外开放由 Nginx 侧控制）
- 防滥用（后续逐步加）：单 uuid 并发上限、单请求 header/body 上限、超时与空闲回收
- 观测（后续逐步加）：按 uuid/请求 id 打点日志，关键错误能定位到链路环节（Client/Nginx/Gateway/Agent/App）

## 验收标准（最终）

- 只开放 443 对外（Gateway 无证书）
- Nginx 剥离 `/https-proxy/` 前缀并可代理 WebSocket
- HTTP 代理可用：状态码/headers/body 正常、支持大 body
- Trailer 代理可用：request/response trailers 正常透传
- 流式响应可用：curl `--no-buffer` 可实时输出
- WebSocket 可用：code-server 终端可用、消息不丢不乱序、close 传播正常
