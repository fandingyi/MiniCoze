# 第七天：发布快照、公开运行与迁移验收

日期：2026-10-19。

## 本次落地

1. **发布能力冻结**：Go 从 `Agent.currentVersionId → AgentVersion.snapshot` 读取模型、提示词、工具和知识库范围。修改草稿模型或增加工具不会扩大发布版权限。Python 的持久化工具循环支持 `draft` 和 `published`。
2. **知识库发布记录**：NestJS 发布时将知识库 ID、名称和启用状态写入快照。旧快照没有 `knowledge` 时按空权限处理，需要重新发布才能使用知识库，不从草稿补齐。
3. **公开 WEB 运行**：服务端签发带 HMAC 签名和到期时间的 HttpOnly Cookie。会话、Run 查询、取消和幂等键按渠道与访客身份隔离；客户端 `visitorId` 不作为 WEB 身份证明。
4. **公开 API 运行**：复用现有 API Key 的 SHA-256 存储，检查渠道状态、有效期、Agent 状态、创建者状态及工作区成员身份。密钥轮换后旧凭据失效。
5. **复用运行生命周期**：公开请求经过内部上下文委托进入同一持久化运行流程，保留幂等、超时、取消、工具审计和最终消息事务；不会给访客签发创建者 JWT。
6. **可选切流**：Go 与 Vite 分别增加默认关闭的公开入口开关，发布管理仍由 NestJS 承担。

发布快照冻结的是能力范围，知识库内容仍读取当前启用的 READY 文档片段，不是历史文档版本。工具执行时仍检查当前禁用、解绑及成员撤销状态：发布快照不覆盖管理员的撤权操作。目前支持只读内置工具与关键词检索；启用的工作流或不支持的工具会拒绝执行。

## 接口与关键步骤

| 接口 | 身份与用途 |
| --- | --- |
| `GET /api/public/agents/{slug}` | 查询发布信息并建立访客 Cookie，不返回提示词或模型凭据 |
| `POST /api/public/agents/{slug}/chat/stream` | Cookie 访客，发布版 SSE |
| `GET /api/public/agents/{slug}/runs/{runId}` | 同一访客查询运行 |
| `POST /api/public/agents/{slug}/runs/{runId}/cancel` | 同一访客取消运行 |
| `POST /api/public/agent-runs/stream` | `Authorization: Bearer <API Key>`，服务端集成 |
| `GET /api/public/agent-runs/{runId}` | 同一 API Key 及 visitorId 查询运行 |
| `POST /api/public/agent-runs/{runId}/cancel` | 同一 API Key 及 visitorId 取消运行 |

所有流式提交需要 `Idempotency-Key`，长度 8～128。同一次提交重试复用同一个键；下一轮消息使用新键。请求体支持 `message`、`inputs`、`conversationId`、`visitorId`；不接受客户端指定 Agent、用户、工具、预览模式或模型。

WEB 调用顺序：先 GET 获取 Cookie → POST 消息并消费 SSE → 保存服务端返回的 conversationId → 携带同一 Cookie 继续会话。浏览器同源调用自动携带 Cookie；跨源调用需要 credentials 和正确的 CORS 配置，跨站部署不属于本次验收范围。公开聊天页面已接入 `/share/agents/:slug` 和 `/embed/agents/:slug`，位于登录守卫之外。支持流式回答、停止、新会话和 Go 幂等重试；聊天记录仅保存在当前页面内存，刷新后开启新会话。

API Key 是服务级凭据；可选 visitorId 仅细分该凭据内部的会话，不代表终端用户认证。API Key 入口支持流式运行、幂等重放、查询与取消；如果创建运行时填写了 visitorId，查询和取消也必须通过 `?visitorId=同一值` 指定该命名空间，否则返回 404。流式响应的 Location 包含查询地址。Key 轮换会切换会话命名空间，需要开始新会话。

旧 NestJS 访客会话命名空间不迁入 Go。切流后应创建新会话，不能把旧客户端 visitorId 直接升级为可信身份。创建者作为业务管理者仍可通过自己的受保护接口查看其运行记录，访客之间互相隔离。

## 启用方式

1. 在测试环境应用第 4～6 天的三个 Prisma 迁移，配置 Go/Python 内部服务密钥、模型白名单与模型供应商。本次没有连接或迁移业务数据库。
2. Go 设置 `GO_PERSISTED_RUNS=true`、`GO_PUBLIC_RUNS=true`；`PUBLIC_SESSION_SECRET` 使用独立随机值，至少 32 字符，多实例共享。HTTPS 保持 `PUBLIC_COOKIE_SECURE=true`；仅本机 HTTP 联调设为 false。
3. 从 NestJS 发布 Agent 并启用 WEB/API 渠道；知识库能力需要本次格式的新发布快照。
4. 本地 Vite 设置 `VITE_GO_API_PROXY_TARGET=http://127.0.0.1:3001` 和 `VITE_GO_PUBLIC_PROXY_ENABLED=true`，重启开发服务。生产环境需在反向代理配置对应路由，Vite 配置不是生产网关配置。
5. 先验证公开信息与 Cookie，再验证流式完成、同键重放、跨访客拒绝和渠道关闭。本次未修改实际 `.env` 或开启业务流量。

## 验证记录

- **Go 全链路**：隔离 PostgreSQL，实际启动 Python/uvicorn；发布版完成 echo → 知识检索 → 模型后续轮次 → 最终消息入库，重复提交不重复调用模型，取消传到模型连接。供应商由本地 HTTP 模拟，无付费模型调用。
- **权限回归**：草稿修改不影响发布模型与工具范围；不同访客同幂等键不冲突；跨访客会话续聊、Run 查询/取消被拒绝；篡改 Cookie、客户端覆盖模式、过期或格式错误的有效期、轮换后的旧 Key、禁用渠道均被拒绝。
- **Python**：32 项测试通过，包含草稿及发布版工具循环。依赖测试客户端有一条弃用提示，不影响本次结果。
- **NestJS**：3 个测试套件、19 项测试通过，覆盖知识库发布快照及事件契约；全量 TypeScript 类型检查通过。
- **构建与配置**：Go vet/build、Prisma schema 校验、Vite 公开代理开关及旧路由回退检查通过；补丁无空白错误。
- **本地基线**：2026-10-19，16 次串行公开提交，成功 16/16；一次测量 p50 **4.30 ms**、p95 **6.85 ms**。计时范围为进程内 Go HTTP handler、真实临时 PostgreSQL、localhost 模拟 Runtime 的完整请求。未计浏览器网络、真实 Python 推理与供应商等待，不代表模型延迟、生产吞吐量或 TTFT。

复现 Go 集成测试（在 `apps/platform-go`，先将 Go 加入 PATH）：

```powershell
$env:RUN_POSTGRES_INTEGRATION='1'
$env:RUN_PYTHON_INTEGRATION='1'
go test ./... -count=1 -v
go vet ./...
go build ./...
```

Python：在 `apps/agent-runtime-python` 运行 `.venv/Scripts/python.exe -m pytest -q`。测试只使用临时数据库和模拟供应商，不读取业务 DATABASE_URL。

## 回退演练与操作边界

自动化演练覆盖两个层次：

1. **路由回退**：在测试中切回已有 Go→NestJS 桥接处理器，模拟 NestJS SSE 服务收到请求，Python 路径未被调用；切回 Go 后，原公开请求仍可幂等重放。
2. **版本回退**：在临时数据库切换 Agent 的 currentVersionId，新运行读取目标版本，历史已完成 Run 的快照与状态保持不变。

这验证了路由与持久化兼容边界，不等同于实际部署中的滚动回退或真实 NestJS 数据兼容验收。

部署时先停止向待退实例派发新请求，等待正在执行的流结束或取消，再关闭公开代理开关恢复 NestJS 公开入口。私有聊天回退可关闭 `GO_PERSISTED_RUNS` 并保留 `NESTJS_INTERNAL_URL`；随后重启 Go。保留新增表和历史数据，不执行向下删表迁移。回退后开始新公开会话；不要对结果未确认的在途请求换入口自动重试，以免重复生成。

真实供应商、业务数据库、生产负载压测和生产回退仍待环境验收。公开页面已完成本地模拟服务浏览器验收，尚未与部署环境联调。当前并发限制是单实例共享上限，未实现按访客配额、付费额度或分布式限流。


## 第七天补齐记录

- **公开页面**：分享与嵌入链接无需登录；访客 Cookie 随请求发送，不读取创建者 JWT。旧 NestJS 使用页面内随机 visitorId 维持会话；只有 Go 返回 persistedRuns=true 才提供同键重试，避免对旧链路重复提交。
- **流协议**：独立 SSE 解析器支持跨字节 Unicode、CRLF；限制未完成事件缓冲为 2 MiB；缺少运行终态或 stream.done 时报告中断，不把 HTTP 连接关闭当成成功。
- **API 生命周期**：查询及取消复用现有权限检查；缺失或不同 visitorId 无法操作其他命名空间。隔离数据库回归还验证了活跃运行取消后 SSE 终止且状态落为 CANCELLED。
- **构建修复**：修复插件配置联合类型收窄、知识库枚举赋值，删除 PreviewChat 中不可达的重复事件分支，保留原有工具卡片、知识检索与终态处理。前端全部 6 处类型错误已清除。
- **验证**：Go 全套回归（临时 PostgreSQL + 实际 Python）及 vet 通过；前端 4 项 SSE 测试通过；tsc -b 与 Vite 生产构建通过。Vite 仍提示已有 vendor chunk 超过 500 kB，本轮未调整全站拆包。
- **浏览器验收**：使用本地 UI fixture 验证分享页收发、断流提示、同请求重试、停止后恢复输入、新会话清空消息、嵌入页匿名加载；这是模拟接口的 UI 验收，不是生产端到端测试。

前端测试：在 apps/web 运行 `node --test scripts/public-stream.test.mjs`；重现 UI 验收可运行 `node scripts/public-ui-fixture.mjs`，访问 `http://127.0.0.1:5187/share/agents/fixture`。输入“断线”模拟断流后查询原运行恢复回答，输入“等待”模拟可停止的慢响应。此 fixture 仅绑定本机，不连接数据库或模型。最新访客会话请求头、恢复策略及渠道并发配置见 [聊天可靠性补充](chat-reliability.md)。

本机 npm 启动器路径缺失，本次直接运行已安装的工具完成等价构建：`node node_modules/typescript/bin/tsc -b --pretty false`，随后 `node node_modules/vite/bin/vite.js build --mode production`。

## 持续并发验收

新增显式启用的 `public_load_test.go`。通过真实 localhost HTTP 访问公开 API，使用临时 PostgreSQL 和延迟 100 ms 的模拟 Runtime；8 个客户端、单实例准入上限 4，持续请求 30 秒。遇到 429 后客户端等待 10 ms，成功请求后立即发起下一次。本测试不调用真实供应商，不读取业务数据库配置。

2026-10-19 本次结果（包含末尾请求收束，共 30.35 秒）：

| 指标 | 结果 |
| --- | --- |
| 完成运行 | 983 |
| 预期 429 限流 | 6,956 |
| 异常请求 | 0 |
| 完成吞吐量 | 32.39 次/秒 |
| 成功请求完整耗时 p50 / p95 | 107.17 / 116.85 ms |
| 助手消息 | 983，与完成数一致 |
| 结束后遗留活跃 Run | 0 |

测试同时断言模型调用数等于完成数，429 请求不创建 Run，成功流各有一次 run.completed 和 stream.done。首次运行发现统计 SQL 将数据库角色 ASSISTANT 写成小写，修正统计后重新运行通过。指标描述的是这个受控负载；不代表真实模型的延迟、TTFT、最大容量或生产 SLA。

机器可读报告：[public-load.json](../../artifacts/day-7/public-load.json)。复现时，在 `apps/platform-go` 执行：

```powershell
$env:RUN_POSTGRES_INTEGRATION='1'
$env:RUN_PYTHON_INTEGRATION='1'
$env:RUN_PUBLIC_LOAD='1'
go test ./internal/platform -run TestPostgresRunLifecycle -count=1 -v
go vet ./...
```

不设置 RUN_PUBLIC_LOAD 时跳过负载阶段，避免日常单元测试额外等待 30 秒。每次显式运行会更新报告。临时数据库随测试销毁，业务运行开关保持原值。
