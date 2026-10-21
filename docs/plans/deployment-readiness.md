# 部署前就绪检查

日期：2026-10-21。本次补齐探针与启动配置校验，不修改实际环境变量，不迁移业务数据库，不开启业务流量。

## 两类健康状态

| 接口 | 用途 | 检查范围 |
| --- | --- | --- |
| Go `GET /healthz` | 存活探针 | HTTP 进程能够响应 |
| Go `GET /readyz` | 就绪探针 | 数据库连通、启用持久化时的 Run/工具调用/模型轮次表、已配置的 Python Runtime |
| Python `GET /healthz` | 存活探针 | HTTP 进程能够响应 |
| Python `GET /internal/v1/ready` | 内部就绪接口 | 服务令牌匹配、生命周期 HTTP 客户端已初始化且未关闭，返回是否配置工具网关 |

Go 就绪检查共用 2 秒截止时间，任一依赖失败返回 503，正常返回 200。响应仅为 ready/not ready，禁止缓存，不输出数据库地址、内部异常或服务令牌。停机时生命周期取消会让就绪探针失败，并中止进行中的依赖检查。

Python 探针需要 `Authorization: Bearer <RUNTIME_SERVICE_TOKEN>`。Go 自动携带该令牌，拒绝重定向、不使用环境代理，最多读取 4 KiB 响应。旧 Runtime 缺少该接口、令牌不一致或未就绪均会让 Go 返回 503。持久化模式额外要求 Python 已配置 GO_TOOL_GATEWAY_URL，防止文本请求正常但所有工具请求失败的配置遗漏。

探针**不发送模型请求**，也不从 Python 回调 Go，避免探针产生费用或形成循环依赖。tools_ready 表示工具网关配置存在，不证明网关实际可达。模型凭据有效性、白名单中模型的供应商可用性、完整迁移约束、旧 NestJS 桥接上游和真实工具执行仍需专项验收，不能仅凭 readyz=200 判断全部业务可用。

## 启动配置校验

GO_PERSISTED_RUNS、GO_PUBLIC_RUNS、PUBLIC_COOKIE_SECURE 只接受空值、`true`、`false`。空值分别使用 false、false、true。大小写错误、拼写错误及多余空格均直接报错，不静默改变运行路径或 Cookie 安全设置。

`GO_PUBLIC_RUNS=true` 必须同时配置 `GO_PERSISTED_RUNS=true`，否则启动失败。以上校验在连接数据库前执行；报错只包含配置项名称，不回显配置内容。

## 联调顺序

1. 同步部署包含内部就绪接口的 Python 版本，配置模型白名单及内部令牌；持久化模式还需配置工具网关地址。
2. 在目标测试环境应用既有 Prisma 迁移，再启动 Go。保持公开流量切换开关关闭，先从内部网络检查 `/healthz` 和 `/readyz`。
3. 通过独立、受控的聊天请求验证真实模型与工具调用，再进行公开渠道和数据归属验收。
4. 部署系统将 `/healthz` 用于存活检查，将 `/readyz` 用于准入判断；内部就绪接口与服务令牌不暴露给浏览器。
5. 回退或更新时停止新流量，等待或取消在途请求后再重启。关闭 Go 持久化模式并不自动验证旧 NestJS 的可用性，必须单独检查桥接入口。

## 本地验证

- Go：严格布尔配置、依赖故障脱敏、停机返回 503、内部认证、旧协议/错误响应拒绝、重定向拒绝、取消传播。
- Python：未认证探针返回 401；工具配置状态正确；HTTP 客户端关闭后就绪返回 503，而存活仍返回 200。全套 34 项测试通过。
- 集成：临时 PostgreSQL、实际 Python 进程及模拟供应商；预览与发布工具链均加入实际 HTTP 就绪探针。

在 apps/platform-go 设置 RUN_POSTGRES_INTEGRATION=1、RUN_PYTHON_INTEGRATION=1 后运行 `go test ./... -count=1`；普通 `go test ./...` 会跳过依赖外部进程的集成测试。负载测试依然单独由 RUN_PUBLIC_LOAD 控制。
