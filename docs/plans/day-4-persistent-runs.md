# 第四天：Run、消息与快照持久化

日期：2026-10-14。

## 本次实现

- Go 创建运行时，在一个 PostgreSQL 事务中保存会话、Run、用户消息、配置快照和历史上下文；Python 只执行服务端提供的快照，不直接写业务数据库。
- 成功时，助手消息、usage 和 COMPLETED 状态在同一事务提交。提交确认后才输出 `message.completed`、`run.completed`。流中的增量文本属于临时展示，失败或取消不保存为最终助手消息。
- `(userId, requestKey)` 唯一索引和事务 advisory lock 保护并发重复提交。相同键不同请求返回 409；相同请求正在运行返回 409 并附原 Run；已结束则从数据库重建结果流，不再调用模型。
- 同一会话最多一个活跃 Run。完成和取消竞争通过行锁决定终态；后到的操作返回既有结果，不覆盖终态。数据库触发器阻止快照篡改、状态倒退和终态改写。
- 草稿支持配置覆盖；正式聊天使用已有 AgentVersion 的发布快照，忽略旧前端附带的模型、提示词、温度和输出预算。工具、知识库和工作流仍在能力门禁内，不能静默绕过。
- 历史上下文最多 50 条、合计 64KB，仅包含旧消息及已完成 Run 的消息。用户/助手消息时间顺序明确，失败 Run 的输入不进入后续模型历史。
- 新增按归属查询与取消接口。取消写库后关闭执行连接；另一 Go 实例的取消由最多约一秒的轮询感知。进程中断遗留的 Run 在 150 秒截止时间后，由启动检查或每 15 秒的清理任务标记失败，不自动重跑模型。
- 前端每次发送携带 `Idempotency-Key`。调用方可提供 `idempotencyKey`，只在重试同一个逻辑请求时复用；没有增加自动重试。

## 接口

| 接口 | 行为 |
| --- | --- |
| `POST /api/agent-runs/stream` | 开启持久化开关后使用 Go → Python；必须带 8–128 字符的 Idempotency-Key |
| `GET /api/agent-runs/{runId}` | 查询当前用户且仍有工作区成员资格的 Run，包括状态和最终输出 |
| `POST /api/agent-runs/{runId}/cancel` | 取消未结束的 Run；已结束则返回原终态 |
| `POST /api/agent-runs/preview/stream` | 保留第三天独立无持久化预览入口 |

运行响应包含 `X-Run-Id` 和 `Location`。重试结果附带 `Idempotency-Replayed: true`。结果重建使用独立事件 ID 命名空间，**不支持 Last-Event-ID 增量续传，不是原始事件日志重放，也不保证模型调用 exactly-once**。

## 数据迁移与启用

唯一迁移入口仍是 Prisma：`apps/backend/prisma/migrations/20261014090000_add_persistent_agent_runs/migration.sql`。

新表 AgentRun、新枚举 AgentRunStatus；Message 增加可空 runId，旧消息仍可读取。部分唯一索引、检查约束和触发器由 SQL 迁移维护，不能用 `prisma db push` 代替迁移。

1. 核对目标数据库与原有迁移状态，在后端目录按现有部署流程执行 `prisma migrate deploy`，再生成 Prisma Client。已有历史迁移异常应先解决，不能直接将所有历史迁移标记完成。
2. 配置 Go 与 Python 一致的 `RUNTIME_SERVICE_TOKEN`、Go 的 `PYTHON_RUNTIME_URL`、Python 的模型配置。
3. 在 Go 设置 `GO_PERSISTED_RUNS=true`。启动时校验 Run 表可用；未迁移时明确失败，不自动建表。
4. 开发前端设置 `VITE_GO_API_PROXY_TARGET=http://127.0.0.1:3001` 并重启 Vite，流式入口、Run 查询和取消走 Go。生产部署需要对应反向代理路由。
5. 先选无工具、知识库和工作流绑定的 Agent 进行草稿与发布聊天验收。

默认 `GO_PERSISTED_RUNS=false`，继续使用原 NestJS SSE 桥接。本次没有修改实际环境开关，也没有对用户业务数据库执行迁移。

回退时先停止接收新运行并等待或取消在途运行，再关闭开关恢复 NestJS 路由。保留新增表列，不执行破坏性降级。两条执行链路不能同时写同一活跃会话；NestJS 不参与 Go 的 Run 锁协议。生产回退及真实供应商验收仍需部署环境验证。

## 验证记录

- 临时 PostgreSQL：执行实际新增迁移，在最小旧表结构上验证并发幂等、请求冲突、归属校验、会话排他、不可变快照、助手写入失败回滚、历史顺序、完成/取消竞争、过期回收与发布版本读取。数据库自动创建和清理，不读取业务 DATABASE_URL。
- 持久化 HTTP：验证缺少幂等键拒绝、正常完成落库、重试不重复调用模型、Run 查询、终态取消无效和执行中取消。
- Go：全量测试含真实 Go → Python HTTP 联调，vet 与编译通过。模型提供端使用本地测试服务。
- Python：20 项测试通过，覆盖 draft/published 历史消息拼装及会话标识；有一条已有 Starlette 测试客户端弃用提示。
- Prisma schema 校验通过。
- 前端全量类型检查未通过：`useAgentSave.ts` 两处联合类型属性问题、`knowledge-base-helpers.ts` 三处枚举值问题、`PreviewChat.tsx` 一处消息结构问题。这些文件本次未修改；不能宣称前端全量构建通过。

Go 集成测试在 `apps/platform-go` 运行：设置 `RUN_POSTGRES_INTEGRATION=1`、`RUN_PYTHON_INTEGRATION=1`，执行 `go test ./... -count=1`。首次数据库测试会下载临时 PostgreSQL 二进制；Python 联调要求第三天的虚拟环境已安装。

## 下一步

第五天接入工具与知识网关，落实绑定校验、调用预算、工具结果和引用记录。当前只完成文本模型链路，尚未实现工具循环、可靠异步队列或中断后恢复执行。
