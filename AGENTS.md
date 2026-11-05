# AGENTS.md

适用范围：本文件所在目录（仓库根目录）及其子目录。

## 1. 项目边界

- 本仓为开源数据面组件，优先保持通用语义与稳定接口行为。
- 不在本仓承载私有业务鉴权策略；私有策略应放在上游控制面（如 `web`）。

## 2. 代码索引（功能去哪看）

- gateway 入口与 `/register`、`/status`：`internal/gateway/server.go`
- agent register 与上游转发：`internal/agent/agent.go`
- proxy mode / target kind 共享语义：`internal/shared/proxymode.go`
- CLI 参数与 register URL 组装：`cmd/clockbridge-cli/main.go`
- CLI 首参数 `--frp` 直接进入固定 v0.71.1 的官方 frpc 命令入口，其余参数完全由 frpc 解析；默认仍是 bridge。两套 token/`-i` 参数语义不能混用，不在此处添加私有 web 鉴权策略。
- 行为测试：`cmd/clockbridge-cli/main_test.go`、`cmd/clockbridge-cli/dispatch_test.go`（子进程验证入口、配置错误及默认 bridge 转发）
- 环境变量与运行说明：`README.md`
- Go 版本与 frp 固定依赖：`go.mod`、`go.sum`（frp v0.71.1 要求 Go 1.26+）；frp 许可：`third_party/frp/LICENSE`

## 3. 变更要求

- 协议语义修改需先补测试，再改实现。
- 文档以 `README.md` 为准；仓库内保持单一架构说明入口。
