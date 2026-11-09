# 第三天：Go 平台读取与 Python 运行网关

## 本次交付

| 模块 | 实现 | 边界 |
| --- | --- | --- |
| Go 平台读取 | 工作空间列表/详情、Agent 列表/详情、会话详情及消息 | 复用现有表，仅查询；写接口继续由 NestJS 负责 |
| Python Runtime | 单模型流、严格请求校验、服务认证、预算、错误事件 | 补齐第二天必要基础；未迁移 NestJS 执行策略或工具循环 |
| Go Python 网关 | 查询已授权快照，调用内部 Runtime，校验并转换事件 | 独立无持久化预览；不替换已有持久聊天 |
| 断连处理 | 浏览器→Go→Python→模型 HTTP 取消 | 已用本地真实 HTTP 链路验证；未落库 CANCELLED |

## 新增 API

所有下列 Go 接口都要求用户 Bearer JWT：

```text
GET  /api/workspaces?page=1&pageSize=20
GET  /api/workspaces/{workspaceId}
GET  /api/agents?workspaceId=...&page=1&pageSize=20&status=DRAFT&keyword=...
GET  /api/agents/{agentId}
GET  /api/workspaces/{workspaceId}/conversations/{conversationId}
POST /api/agent-runs/preview/stream
```

列表保留 `code/message/data` 和 `list/total/page/pageSize`，分页上限 100。查询参数进入 SQL 占位参数；Agent 和工作空间在查询内按成员关系过滤；会话查询同时限制当前用户和目标工作空间，不能通过猜测 ID 跨用户读消息。不可见详情返回 404，Agent 列表无成员权限返回 403。

预览只接收：

```json
{"agentId":"已有的纯文本 Agent ID","message":"你好"}
```

Go 读取该 Agent 的数据库草稿配置，一次请求固定一份快照，不接受客户端指定 userId、workspaceId、模型凭据、工具或任意模型服务 URL。存在插件、知识库或工作流绑定时返回 409，要求继续使用旧运行入口，避免静默丢功能。暂不接受配置覆盖和历史会话。

预览返回 `X-Run-Persistence: none`；事件中的 conversationId 为临时预览标识，不能拿它查询历史。不会写 Message/Run，不承诺刷新后恢复。

## 部署及切换

1. 配置并启动 Python Runtime，见 `apps/agent-runtime-python/README.md`。
2. 为 Go 设置 `PYTHON_RUNTIME_URL=http://127.0.0.1:8001`、同一个 `RUNTIME_SERVICE_TOKEN`，启动 Go。
3. 前端已有 `VITE_GO_API_PROXY_TARGET` 开关扩展支持新预览 URL。原有 `/agent-runs/stream` 仍桥接 NestJS，不改变既有聊天持久化行为。
4. 新增的读取 API 可直接访问 Go；本次没有自动切换页面请求。若配置生产代理，须只切相应 GET，避免把 POST/PATCH/DELETE 一同切走。Vite 原有读取路径仍走 NestJS。
5. 未配置 PYTHON_RUNTIME_URL 时预览路由不注册。移除配置可关闭新入口，现有聊天不依赖 Python。

没有静默自动回退：Python 执行已开始后若失败，返回明确失败事件，不重新请求旧模型，避免重复执行与计费。

## 协议检查

Go 检查 schema_version、run_id、event_id、连续序号、时间戳、事件顺序、消息 ID 和最终内容一致性。禁止未知事件及 payload 字段。Python 异常截断时补充单一 run.failed 和 stream.done；已收到终态时不再追加第二终态。这里是内存中的预览流约束，不是第四天的数据库幂等状态机。

## 验证记录

- Python：18 项测试通过；认证、输入大小、能力限制、模型异常与截断均有覆盖。
- Go：全部回归通过，包含新增读取接口权限/分页、网关凭据隔离、快照、非法事件、截断等测试。
- 跨进程：开启 RUN_PYTHON_INTEGRATION，真实启动 Python 与本地测试模型；验证未结束时首段到达、中文无损、事件顺序和取消传播。
- 真实 PostgreSQL 查询与真实模型供应商尚未联调；测试存储是替身，不能将其描述为真实数据库验收。

## 下一阶段

第四天迁移 Run/消息/快照持久化、提交幂等和唯一终态；之后才把正式聊天从 NestJS 桥接切到 Go→Python。工具、知识和工作流继续按后续计划实施。
