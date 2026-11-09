# MiniCoze Go + Python 七天核心闭环计划

日期：2026-10-10。依据：重构草案 V1.0 与本仓库代码盘点。

## 2026-10-11 顺序调整（按用户要求）

优先 Go 身份鉴权，验证后接入 Go SSE，再继续 Python Runtime 及原计划。此前的事件协议代码保留。

| 天 | 调整后工作 | 当前状态 |
| --- | --- | --- |
| 1 | Go 登录/JWT/用户状态/资源授权，再接入受保护 SSE 桥接 | 已实现并通过本地测试；真实数据库和模型联调待验收 |
| 2 | Python Runtime、请求 DTO、快照、内部认证与模型流；以开关接入 NestJS | 已补齐单模型预览必要基础；NestJS 策略接入未实施 |
| 3 | Go 平台基础业务接口与 Python 网关；逐项替换 NestJS 桥接 | 已实现基础读取与直连 Python 的独立预览；正式聊天桥接保留至持久化迁移 |
| 4 | Run/消息/快照持久化、幂等与唯一终态 | 已实现并通过隔离 PostgreSQL 与 HTTP 测试；业务数据库迁移与真实模型验收待部署 |
| 5 | Go 工具与知识网关、Python 工具循环 | 已实现持久化草稿的只读工具、关键词检索、预算与调用记录；完整跨进程闭环通过，发布能力迁移后置至第七天 |
| 6 | 模型级取消、超时、运行回收、日志与用量 | 已实现停机收束、预算传递、批量回收、关联日志与分轮用量；真实 Python 取消及隔离数据库验证通过 |
| 7 | 发布/公开入口、端到端回归、基线与回退演练 | 已实现发布能力快照与 WEB/API 入口；隔离数据库及真实 Python 回归通过，完成本地模拟基线与回退演练；生产验收待部署 |

第七天实现、公开身份规则、开关及验证边界见 [第七天记录](day-7-publish-public.md)。下文为早期阶段记录；公开 API Key 运行入口现可选择切到 Go，发布管理仍由 NestJS 提供。

Go 代码位于 `apps/platform-go`；复用现有数据库及密码，不重建用户表。登录和 SSE 开发切流使用 `VITE_GO_API_PROXY_TARGET`，默认仍走 NestJS。注册/修改资料/改密/公开 APIKey 入口继续保留在旧服务。Go HTTP 请求取消已经验证，但旧模型执行取消尚未实现，不能宣称整条执行链路已停止。

验证结果：Go 全部测试、vet、服务编译通过；直接调用 NestJS JwtService 验证双方 Token，验证既有 bcrypt 密码；真实 HTTP 测试验证 SSE 首帧、内容、权限拦截及连接取消。Go 1.27.2 已安装至用户目录并配置 PATH。Vite 可选路由检查通过。真实数据库和模型端到端验收仍待完成。

下面保留最初计划与第一天事件改造记录，执行顺序以上表为准。

第三天范围与验证见 `docs/plans/day-3-platform-python.md`。因第二天尚未落地，本次补齐 Python 必要基础，采用独立预览先验证 Go→Python，未将无持久化运行替换已有聊天。此调整避免在第四天数据库改造前丢失会话记录。

第四天实现、开关、迁移步骤与验证边界见 `docs/plans/day-4-persistent-runs.md`。默认仍保留 NestJS 桥接；开启 Go 持久化链路前需应用新增 Prisma 迁移。

七天目标是可验证的单 Agent 核心迁移闭环，不等同于草案全部完成。保留 NestJS 回退入口；完整工作流、多 Agent、Checkpoint、可靠异步队列与复杂降级后置。每天先满足退出条件，再进入下一阶段。

| 天 | 工作范围 | 验收条件 |
| --- | --- | --- |
| 1 | 盘点真实运行链路；建立版本化事件信封、前端兼容适配和测试 | 现有 SSE 字段与事件名保留；新增事件 ID、序号与版本；兼容测试通过 |
| 2 | 建立 Python Runtime；定义运行请求 DTO、配置快照与内部认证；实现单模型流；NestJS 通过可配置执行策略接入 | 真实模型完成一次聊天；错误有明确事件；关闭开关恢复旧策略 |
| 3 | Go 模块化单体基础；接入既有身份与资源归属规则；建立 Go→Python 运行入口和 SSE 适配 | Go 入口能鉴权、拒绝越权并输出兼容事件；模型凭据方案确定 |
| 4 | Go 持久化 Run、消息、快照；单一迁移工具；提交幂等与终态条件更新 | 重复提交不重复建消息；最终消息与运行状态事务一致；旧数据兼容 |
| 5 | Go 工具与知识检索网关；Python 工具循环；能力绑定校验与调用预算 | 一个只读工具与一个知识库跑通；禁用或未绑定资源被拒绝；引用可追溯 |
| 6 | 分层超时、取消、限并发、孤儿运行收束、链路日志和用量记录 | 超时、断连、重复事件与慢客户端有验证记录；没有无限缓冲；终态唯一 |
| 7 | 接入已有发布快照及公开入口；闭环回归、基线测量、回退演练和文档 | 草稿变更不影响发布版本；访客隔离通过；可演示并回退；记录实际指标 |

## 第一天核实的现状

- NestJS 实际运行入口：`agent-runs/stream`；公开入口复用 `writeRuntimeEventStream`。
- `AgentRuntime` 管生命周期，执行策略管模型与工具循环，已有可替换边界。
- 前端读取 SSE `data` 中的扁平 camelCase 对象，不能直接替换成内部 `payload` 信封。
- 实际结束标记为 `stream.done`，不是 `[DONE]`；后者属于供应商模型流。
- 服务端已有 `tool.call.failed`，前端类型没有列出；此次保留事件，不擅自删除或改名。
- `RuntimePrismaRepository` 的 Run 存在进程内 Map；Prisma 没有独立 Run 表。消息已持久化，但工具角色消息被跳过；usage 根据最新助手消息更新。第四天必须解决并发归属和事务一致性，不能宣称已有可靠 Run 状态机。
- `AgentRuntimeService.cancel()` 当前为空实现，第六天要落实跨进程取消。
- 当前终态命名含 `canceled`，草案使用 `cancelled`；第二天明确内部状态与旧字段映射。
- 本机能找到 Node 与 Python，PATH 未找到 Go；第三天前需要配置 Go 工具链。

## 第一天实际代码

`runtime-event-contract.ts` 定义 v1 内部信封与浏览器适配器，统一出口为既有事件增加 metadata 和 SSE id。每条流独立编号，拒绝混入其他 Run，不改变原事件对象。

内部事件：`schema_version / event_id / run_id / sequence / timestamp / type / payload`。
对外事件：保留原 `type / runId / messageId / content / ...`，增加 `schema_version / event_id / sequence / timestamp`，不嵌套 payload。

本次没有切换 Python/Go，没有数据库迁移，也没有实现持久重放、提交幂等或唯一终态。新增 event_id 只是后续去重的协议基础；不能把它当成已实现 exactly-once。

## 后续需明确的决策

1. 模型密钥：Python 安全持有并直连供应商，或 Go 模型代理，二选一。插件凭据始终由工具网关持有。
2. deadline 通过协议传剩余预算，取消通过连接或受认证接口传播，不跨语言传递 Go context。
3. 工具有副作用时，不把所有超时当成可重试；结果不确定要查询或人工核对。
4. 若提供可靠异步 API，必须有持久任务记录及恢复机制，进程内队列不够。
5. 发布迁移只接已有能力；若身份与数据迁移超出一天，优先保证主链路，不为赶日程删除校验。

## 第一天验证命令

实际结果：2 个测试套件、18 个测试全部通过；后端 TypeScript 全量类型检查通过；`git diff --check` 通过。覆盖全部 11 种既有事件的字段兼容、Unicode/换行、流序号隔离、跨 Run 拒绝、HTTP 204、建流前后错误行为。未进行真实模型、数据库或浏览器端到端验证。

在 `apps/backend` 执行：

```text
node node_modules/jest/bin/jest.js --runInBand --runTestsByPath src/modules/agent-runtime/runtime-event-contract.spec.ts src/modules/agent-runtime/runtime-event-stream.writer.spec.ts
node node_modules/typescript/bin/tsc --noEmit --incremental false
```

回退第一天行为：将 SSE writer 恢复成直接序列化 RuntimeEvent；没有数据格式迁移需要回滚。后续切流须演练，而不能仅凭保留旧代码保证可回退。
