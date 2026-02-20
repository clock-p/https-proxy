# clockbridge TODO（逐步验证）

说明：按“每一步可验证”的方式推进；每完成一项就勾选，并在 PR/记录里附上对应的验收命令与结果截图/日志片段（尽量短）。

## Phase 0：Nginx 路由打通（不涉及 Agent/App）

- [x] 在 Nginx 配置中新增 `location ^~ /https-proxy/`（支持 WS）
- [x] Nginx：把 `/https-proxy/xxx` 转发成上游 `/xxx`（前缀剥离确认）
- [x] 启动本地 Gateway（先只提供 `/status`）
- [x] 验收：`GET https://<DOMAIN>/https-proxy/status` 返回 `{"ok":true}`（或等价 JSON）
- [x] 验收：`GET https://<DOMAIN>/https-proxy/` 返回 404（确认没有意外 root handler）

## Phase 1：Agent 注册长连接 + 心跳（不涉及 Client 转发）

- [x] Gateway：实现 `/register?uuid=` WebSocket Upgrade
- [x] Gateway：校验 `X-Token`（配置化 token 列表，逗号分隔）
- [x] Gateway：uuid 互斥（同 uuid 已在线则拒绝新连接）
- [x] Agent：实现注册连接（ws/wss 由 URL scheme 决定）
- [x] Agent：每 20s 发送 ping（Gateway 60s 未收到认为断线）
- [x] Gateway：提供在线状态观察接口（例如 `/status` 返回在线 uuid 数/列表）
- [x] 验收：启动 Agent 后 `/status` 能看到 uuid 在线；停 Agent 60s 内自动下线

## Phase 2：最小 HTTP 转发（GET/POST，支持大 body）

- [x] 约定：Client path `/https-proxy/<uuid>/xxx/yyy` → Agent suffix path `/xxx/yyy`
- [x] Agent：支持配置 `target base`（例如 `http://127.0.0.1:8080/aaa`）
- [x] 目标拼接：suffix path 追加到 base 后面（`/aaa` + `/xxx/yyy`）
- [x] Header 处理：透传绝大多数 header；过滤 hop-by-hop headers
- [x] Body：Client→Agent→App 端到端流式转发（不整包缓存）
- [x] 返回：App→Agent→Client 端到端转发（状态码/headers/body）
- [x] 验收：mock App 的 `/hello`（JSON）能通过公网 URL 正常访问（本地验证）
- [x] 验收：POST 大 body（比如 50MB）不超时、不 OOM（至少本地压测一次）
- [x] 验收：request/response trailers 正常透传（本地验证）
- [x] 验收：1xx 中间响应可透传（本地验证）

## Phase 3：流式响应（chunk/flush）

- [x] mock App：实现 `/stream`，每 200ms flush 一段文本（总 10s 左右）
- [x] Gateway：支持将响应 data 边到边转发（`Flush()` 生效）
- [x] 验收：`curl --no-buffer https://<DOMAIN>/https-proxy/<uuid>/stream` 能实时看到输出

## Phase 4：WebSocket 透传（浏览器可用）

- [x] mock App：实现 `/ws` echo（text/binary 都支持）
- [x] Gateway：识别 Upgrade 并走 WS 代理流程
- [x] 双向转发：Client<->Gateway<->Agent<->App 的消息不丢、不乱序（本地验证）
- [x] 关闭传播：任意一侧 close 后，另一侧能及时关闭
- [x] 验收：mock client 连 `wss://<DOMAIN>/https-proxy/<uuid>/ws`，发一条回一条
- [ ] 验收：浏览器页面用原生 WebSocket API 连接测试（可选）

## Phase 5：上真实 App（code-server）

- [x] Agent target 指向 code-server（含 base path）
- [x] 验收：页面加载 OK（静态资源）
- [x] 验收：WS 通路 OK（/healthz 代理）
- [x] 验收：终端 OK（WebSocket）
- [x] 验收：大文件上传/下载 OK（长连接/大 body）
- [ ] 记录问题清单：任何不兼容点（headers、timeouts、buffering）

## Phase 6：性能与稳健性（逐项加）

- [x] 并发：支持同一 uuid 多并发请求（先连接池，必要时再 multiplex）
- [x] 限流：单 uuid 最大并发、单请求最大 header/body 上限（防滥用）
- [x] 超时：上游 dial 超时、读写超时、空闲超时（合理默认值）
- [x] 断线：Agent 断线时在 Gateway 侧快速失败（502/504），不挂死
- [x] 日志：按 uuid/请求 id 打点（便于排查）
- [x] 压测：最小压测脚本（HTTP 并发 + WS 并发）
