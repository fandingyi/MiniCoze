# Go 平台：身份鉴权与 SSE 迁移入口

第三天已增加基础读取接口与 Python 网关，详见 `docs/plans/day-3-platform-python.md`（仓库根目录）。设置 PYTHON_RUNTIME_URL 与 RUNTIME_SERVICE_TOKEN 后注册 `POST /api/agent-runs/preview/stream`；该入口仅运行数据库中的纯文本 Agent 草稿，不持久化。原 SSE 桥接保持不变。

新增 GET：`/api/workspaces`、`/api/workspaces/{id}`、`/api/agents`（必传 workspaceId）、`/api/agents/{id}`、`/api/workspaces/{workspaceId}/conversations/{id}`。带成员/归属过滤，写接口仍由旧服务承担。

运行跨进程测试：先安装 `apps/agent-runtime-python` 独立依赖，然后设置 `RUN_PYTHON_INTEGRATION=1` 并执行 `go test ./... -count=1`。测试自动启动并关闭 Python，只调用本地测试模型，不需要真实 APIKey。

已实现：现有 bcrypt 密码登录、HS256 JWT 签发/校验、数据库用户状态检查、当前用户查询、工作空间角色与 Agent/会话权限检查、受保护的 SSE 迁移入口。沿用 Prisma 表，不执行 DDL。

## 启动

需要 Go 1.23+、已有 PostgreSQL。运行前通过环境变量配置 `.env.example` 的字段（程序不会自动读取 .env）。DATABASE_URL 可沿用 Prisma 的 `?schema=public`，内部转换为 PostgreSQL search_path。JWT_SECRET 必须与 NestJS 一致，JWT_EXPIRES_IN 支持 `2h`、`30m`、`7d`，不接受无单位数字。

在本目录执行：

```text
go run ./cmd/server
go test ./... -count=1
go vet ./...
go build -o build/platform.exe ./cmd/server
```

默认监听 127.0.0.1:3001。若刚安装 Go，新终端会读取更新后的用户 PATH。

## 接口

| 接口 | 行为 |
| --- | --- |
| POST /api/auth/login | email/password；返回原有 code/message/data、accessToken、tokenType、user；HTTP 201 |
| GET /api/auth/profile | Bearer JWT；从数据库读取用户，拒绝已删除或非 active 用户 |
| POST /api/agent-runs/stream | Bearer JWT；校验成员、发布状态与已有会话归属，再流式转发至 NestJS |
| GET /healthz | 进程存活检查（不表示数据库持续可用） |

注册、资料更新、密码修改和发布管理仍由 NestJS 承担。公开 WEB/API Key 运行默认保留在 NestJS，第七天新增可选 Go 入口，见下文。通过开发代理/反向代理仅把已迁移 API 路由给 Go，其他 API 保持原路由；不要直接把整个前端 API_BASE_URL 切到 Go。

Vite 开发环境已提供可选开关 `VITE_GO_API_PROXY_TARGET=http://127.0.0.1:3001`：只将登录与私有运行 SSE 转到 Go，其余请求仍由 `VITE_API_PROXY_TARGET` 指向 NestJS。profile 路径同时承担 GET/PUT，开发代理暂时整体保留在 NestJS，Go 的 GET profile 可直接验证。保持前端 `VITE_API_BASE_URL=/api`，关闭鉴权 mock；删除 Go 开关并重启 Vite 即回退。生产代理需按 HTTP 方法单独配置，不受 Vite 设置影响。

登录后若没有任何工作空间成员记录，在事务中创建默认空间；Go 登录之间通过用户行锁避免重复创建。迁移期间应让登录只有一个入口，旧 NestJS 的初始化逻辑不使用相同行锁。

## 认证与授权边界

- 仅接受 Authorization Bearer，固定 HS256，要求 sub 与 exp，校验过期及 nbf；不从请求体或查询参数接受用户身份。
- JWT 不缓存用户状态；每次受保护请求读取当前用户。停用用户是相比现有 NestJS 的额外限制。
- 工作空间成员可运行，OWNER/ADMIN 才可管理；管理角色规则已提供，但尚未迁移管理接口。
- 非预览运行要求 ACTIVE 且存在当前版本；现有 Runtime 仍会解析并校验发布快照。
- 已有会话必须同时匹配用户、Agent、预览模式；不存在的会话 ID 仍沿用原 Runtime 的创建语义，最终写入校验由 NestJS 再次执行。
- Go 不接受 publicAccess、publishedSnapshot、userId 等客户端注入字段；工具与知识库最终绑定校验仍由现有业务执行层承担，此次未宣称完成全域权限迁移。
- 登录校验并发上限 8；这是计算资源保护，不是完整的账号/IP 防暴力破解策略。

## 第四至第六天持久化链路

设置 `GO_PERSISTED_RUNS=true` 可启用持久化 Go → Python 链路，须先部署第四、第五天 Prisma 迁移。草稿调试支持只读 system_tools 和绑定知识库的关键词检索，Python 需配置 `GO_TOOL_GATEWAY_URL`。运行查询、取消、工具调用记录查询均由 Go 提供。

具体配置、验证与限制见 `docs/plans/day-4-persistent-runs.md`、`docs/plans/day-5-tools-knowledge.md`（仓库根目录）。默认关闭开关；下面的 SSE 桥接描述仅适用于 NestJS 旧链路。

第六天新增停机收束、GO_RUN_TIMEOUT、GO_RUN_MAX_CONCURRENT、分轮用量及 `/api/agent-runs/{runId}/metrics`。需应用 AgentModelRound 迁移并同步更新 Python。详细说明见 `docs/plans/day-6-lifecycle-observability.md`。

## 第七天发布与公开运行

持久化链路支持发布快照中的只读工具与知识库权限；NestJS 新发布会写入知识库范围，旧快照缺失该字段时不补读草稿。额外开启 `GO_PUBLIC_RUNS=true`，配置独立的 `PUBLIC_SESSION_SECRET`（至少 32 字符）后可启用公开入口。WEB 使用带有效期的签名 HttpOnly Cookie 隔离访客，API 使用既有渠道 Key；请求必须携带 Idempotency-Key。

Vite 公开切流另需 `VITE_GO_PUBLIC_PROXY_ENABLED=true`，默认关闭。HTTPS 保持 `PUBLIC_COOKIE_SECURE=true`。不迁移旧匿名会话，需要开始新会话。接口、启用顺序、基线和回退演练详见 [第七天记录](../../docs/plans/day-7-publish-public.md)。

## SSE 迁移边界（旧 NestJS 桥接）

NESTJS_INTERNAL_URL 必须是受信任的固定内网 origin，不能来自用户。Go 使用同一 Bearer Token 调用旧运行入口，保留 NestJS 的第二次权限校验。

Go 按原始字节流转发，不重复编号或改变事件内容，保留第一天的 SSE metadata。响应及时 flush，最多 32 个并发运行，慢客户端写超时 10 秒，上游首响应头超时 15 秒，单次请求预算 120 秒。超额返回 429；上游建立连接失败返回脱敏 502；上游 HTTP 业务错误原样返回。

客户端断开会取消 Go 到 NestJS 的 HTTP 请求；**NestJS 模型执行层尚未支持完整取消传播，因此不能保证模型调用立即停止，也未实现 CANCELLED 终态落库**。这是后续 Runtime 改造任务。当前同样不提供 Last-Event-ID 恢复或持久重放。

## 验证范围

2026-10-21 新增 `/readyz`，检查数据库、启用的持久化表及已配置 Python Runtime；失败或停机返回 503。`/healthz` 继续只表示存活。布尔开关只接受 true/false，公开入口依赖持久化开关。详见 [部署前就绪检查](../../docs/plans/deployment-readiness.md)。

测试使用可控存储替身，不需要生产数据库。JWT/bcrypt 互操作测试需要 Node 与已安装的 `apps/backend` 依赖，直接调用 NestJS JwtService 完成双向验证。SSE 测试使用真实本地 HTTP 服务验证首事件及时到达、Unicode/事件 ID 不变、客户端取消向上游传播，以及无权限请求不会触达 Runtime。

尚需部署环境验证：真实 PostgreSQL 登录/默认空间事务、实际 NestJS 模型流及反向代理切流。不要将本地单元/HTTP 测试描述为生产端到端验收。

2026-10-11 本地结果：Go 1.27.2 安装完成，用户 PATH 已配置；`go test ./... -count=1`、`go vet ./...`、服务编译均通过。Vite 配置加载检查通过，确认未开启时仅走旧代理，开启后仅登录与私有 SSE 命中新代理。未修改本地密钥文件，也未自动启动或切换正在运行的服务。
