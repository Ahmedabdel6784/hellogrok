# 发布说明 — v0.1.4

## 私有心跳帧不再导致 Grok Build 失败

部分兼容 Responses 的中转会在模型工作期间注入非标准 `keepalive`、`heartbeat` 或 `ping` JSON 事件。Grok Build 1.0.0 会用严格的 Responses 事件枚举解析每个数据帧，因此这些私有值会触发 `serialization error: unknown variant keepalive`，即使上游任务及其子代理仍在执行。

hellogrok 现在会识别 SSE `event:` 字段、JSON `type`/`event` 字段和裸数据载荷中的常见拼写变体，并将其转换为标准的 `: keepalive` SSE 注释。这样既能保持连接活跃，又不会进入 Grok Build 的类型化事件流，也不会占用 Responses 事件序号。结束日志只记录心跳数量，不记录私有载荷。

## 已完成的流会立即关闭

三种受支持后端现在都以各自的协议终止事件作为本地流结束标志：Responses 在 `response.completed`、`response.incomplete` 或 `response.failed` 后停止；Messages 在 `message_stop` 后停止；Chat Completions 在 `[DONE]` 后停止。随后 hellogrok 会关闭上游响应体，从而取消那些已经完成却仍保持 HTTP 连接的中转请求，避免 Grok Build 长时间停在 `Waiting for response...`。
