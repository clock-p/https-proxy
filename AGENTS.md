# AGENTS.md

适用范围：本文件所在目录（仓库根目录）及其子目录。

## 1. 项目边界

- 本仓为开源数据面组件，优先保持通用语义与稳定接口行为。
- 不在本仓承载私有业务鉴权策略；私有策略应放在上游控制面（如 `web`）。

## 2. 代码索引（功能去哪看）

- gateway 入口与 `/register`、`/status`：`internal/gateway/server.go`
- CLI 参数与 register URL 组装：`cmd/clockbridge-cli/main.go`
- 行为测试：`cmd/clockbridge-cli/main_test.go`
- 环境变量与运行说明：`README.md`

## 3. 变更要求

- 协议语义修改需先补测试，再改实现。
- 文档以 `README.md` 为准；仓库内保持单一架构说明入口。
