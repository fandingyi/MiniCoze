# Runtime 事件协议 v1

内部信封用于后续 Go/Python 通信；第一天在 NestJS 输出边界建立参考实现。其 TypeScript 定义位于 `apps/backend/src/modules/agent-runtime/runtime-event-contract.ts`。

```json
{
  "schema_version": 1,
  "event_id": "run-example:1",
  "run_id": "run-example",
  "sequence": 1,
  "timestamp": "2026-10-10T00:00:00.000Z",
  "type": "message.delta",
  "payload": { "messageId": "message-example", "content": "你好" }
}
```

- 一个流只属于一个 run；sequence 从 1 递增；event_id 为 run_id:sequence。
- timestamp 为 UTC ISO 8601。运行 ID 非空且不能含 CR/LF。
- v1 payload 延续已有 camelCase，去掉 type/runId；不对嵌套对象做自动大小写转换。
- 已支持事件：run.created、run.in_progress、message.delta、message.completed、knowledge.status、tool.call.created、tool.call.completed、tool.call.failed、run.completed、run.failed、stream.done。
- 各事件 payload 字段与现有 RuntimeEvent 对应分支一致。工具结果及参数仍为结构化 JSON。
- 对外通过适配器还原扁平事件，新增 metadata；SSE 使用 id、event、data 和空行分帧。JSON 编码保留换行与 Unicode。
- stream.done 只表示流结束，不代表业务成功。第一天没有新增取消事件，也未修改原 error 传输错误行为。
- 当前序号仅存活于本次连接，不支持 Last-Event-ID 恢复，不可重新生成旧流后当作持久事件重放。
- 第二天从这些参考类型补充跨语言请求校验与网络输入校验；类型断言不能代替不可信 Python 响应的校验。

## 前端收到的对应帧

```text
id: run-example:1
event: message.delta
data: {"messageId":"message-example","content":"你好","type":"message.delta","runId":"run-example","schema_version":1,"event_id":"run-example:1","sequence":1,"timestamp":"2026-10-10T00:00:00.000Z"}

```
