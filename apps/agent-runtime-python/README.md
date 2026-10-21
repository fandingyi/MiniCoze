# Python Agent Runtime

内部单模型流式执行服务。Python 只消费 Go 传来的已授权快照，不访问 PostgreSQL、不解析用户 JWT、不保存会话。模型凭据由 Python 环境变量托管；Go 只持有独立的服务间凭据。这是草案模型密钥方案中的“Python 安全持有并直连供应商”选项。

## 安装与启动

要求 Python 3.11+。本机已创建 `.venv`；复现环境可执行：

```powershell
python -m venv .venv
.venv/Scripts/python.exe -m pip install -r requirements.lock.txt
```

按 `.env.example` 设置环境变量，程序不会自动读取 .env。RUNTIME_SERVICE_TOKEN 至少 32 个字符并与 Go 一致；MODEL_BASE_URL 为受信任的 OpenAI Chat Completions 兼容地址（含 /v1，如供应商需要）；MODEL_ALLOWLIST 为允许的模型名，须与数据库 Agent.model 一致。不要把真实凭据提交到仓库。

```powershell
.venv/Scripts/python.exe -m uvicorn runtime.app:create_app --factory --host 127.0.0.1 --port 8001
.venv/Scripts/python.exe -m pytest -q
```

生产部署放在私网并使用适当的 TLS/服务身份配置，不把 Runtime 暴露给浏览器。

## 接口与行为

- `GET /healthz`：进程存活。
- `POST /internal/v1/runs:stream`：服务 Bearer 认证；请求类型见 `runtime/contracts.py`。
- 支持 preview、draft、published 文本运行与 Go 提供的历史消息；工具用于持久化 draft 和 published，需配置固定的 `GO_TOOL_GATEWAY_URL`。发布版工具由 Go 从发布快照加载，运行时仍检查撤权。
- 工具循环最多 5 轮模型请求、8 次调用，使用 Go 网关执行已授权只读工具与关键词检索。Python 不执行任意函数或工具 URL，不接收插件凭据。
- 事件采用 v1 内部信封，依次为 run.created、run.in_progress、message.delta、message.completed、run.completed、stream.done；错误使用 run.failed，不把截断流当成成功。
- 输入限制 256 KiB，最多 16 个并发，整轮执行预算最高 120 秒，每轮输出上限 8192 tokens；累计文本最多 1 MiB。
- 模型连接关闭、超时、非法 JSON、未授权工具调用均输出脱敏错误。取消会关闭执行连接；持久化 CANCELLED 状态由 Go 管理。
- 请求完成要求供应商发出 finish_reason（stop/length）和 `[DONE]`。采用 Chat Completions 的流式 usage 选项，供应商需支持这一协议。
- 业务错误正文不回显模型响应、APIKey 或原始提示词；重试/降级尚未引入，避免一次请求无意重复计费。

## 验证

31 项 Python 测试通过，覆盖认证、请求约束、模型白名单、历史消息、分轮用量、超时分类、日志脱敏、有界流解析、截断和工具循环。使用本地模拟供应商，未调用付费模型。当前测试依赖输出一条 Starlette/httpx TestClient 弃用提示，不影响测试结果。

Go 的 `RUN_PYTHON_INTEGRATION=1` 测试会自动启动此 Runtime，验证真实跨进程 HTTP 首段输出、中文内容、完整事件顺序，以及客户端取消传到模型连接。测试结束会停止临时进程。

同时设置 `RUN_POSTGRES_INTEGRATION=1` 可验证 Go → Python → Go 工具/知识网关 → PostgreSQL 的完整闭环。第五天范围与部署步骤见仓库根目录 `docs/plans/day-5-tools-knowledge.md`。

第六天新增内部 model.round.completed 回执，必须配合更新后的 Go 持久化入口及 AgentModelRound 迁移部署。模型已完成轮次的用量由 Go 保存，未知或中断轮次不按零消耗处理；参见 `docs/plans/day-6-lifecycle-observability.md`。
