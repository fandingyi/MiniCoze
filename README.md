一个可视化的 AI Agent 的搭建平台，能够使用 Agent 各式各样的能力。例如，角色定位、知识能力、工具能力、工作流能力、模型记忆、行为约束、调试与评估等。

# MiniCoze

可视化 Agent 项目的 Monorepo 总仓库骨架。

Go + Python 聊天链路的最新协议、并发配置和验证记录见 [聊天可靠性补充](docs/plans/chat-reliability.md)。公开 WEB 客户端需先获取应用信息，再携带 Cookie 和 `X-Visitor-Session` 执行、查询或取消运行，前后端需同步发布。

## 目录职责

- `apps/web`: 前端应用目录。
- `apps/backend`: 后端&&AI服务目录。
- `packages/shared`: 跨应用共享代码、类型、工具函数等公共包目录。
- `docs`: 项目文档、架构说明、接口约定和协作规范。
