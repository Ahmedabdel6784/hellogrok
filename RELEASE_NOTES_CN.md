# 发布说明 — v0.1.2

## 可靠启动与渠道分流

代理启动时不再逐个访问上游探测搜索能力。`supports_backend_search` 缺省或为 `false` 时使用 Grok Build 客户端搜索路径；显式为 `true` 时直接选择 hosted 搜索，不再发送启动探测请求。这样消除了 Grok、Claude、GPT 等渠道不可用或不兼容时反复出现的一至两分钟等待；错误的能力声明会在首次真实搜索请求时直接返回。

渠道解析现在会完整保留带点号或连字符的 ID、显示名称、上游模型名、URL 路径和渠道独立凭据。旧式未引用点号模型表只在代理启用期间规范化，停止时逐字节恢复。官方 `messages` 后端和历史 `message` 别名都会进入 Messages 转换。所有后端默认使用 Bearer 鉴权；明确要求 `X-Api-Key` 的服务商可设置 `auth_scheme = "x_api_key"`。

## 所有受支持协议的真流式输出

Responses SSE 继续逐帧透传。Anthropic Messages 与 Chat Completions 请求现在保留 `stream=true`，并将其 SSE 增量转换为 Grok Build Responses 事件，覆盖推理、回答正文、函数参数、hosted 搜索活动、用量、完成状态和终止错误。本地首个增量会在上游响应结束前发出。

若服务商忽略 `stream=true` 并返回单个完整 JSON 响应，hellogrok 会通过缓冲 SSE 回退保持请求可用，并在日志中明确记录这次降级。完整上游响应到达后无法还原真实 token 时序。

## 所有渠道协议的原生搜索来源数量

Responses、Messages 和 Chat Completions 渠道的搜索证据都会同时规范到 `web_search_call.action.sources` 和 `output_text.annotations`。无论哪种受支持协议被选为 Grok Build 客户端搜索模型，此行为都独立于 `supports_backend_search`。因此，Grok Build 可在 hosted 搜索和客户端搜索中显示原生的去重站点数量。

hellogrok 只会输出来自结构化结果、引用、注解，或已独立证明确实执行搜索的最终回答中的真实 HTTP(S) URL。普通回答链接不会创建搜索调用；服务商没有返回 URL 证据时，也不会虚构来源或数量。

## 响应校验与重试控制

上游成功响应现在必须包含 Responses、Messages 或 Chat Completions 的最小有效结构。格式错误的 2xx 响应会返回聚焦的 502，不再作为表面成功结果进入 Grok Build 重试循环。确定性的配置、结构和转换错误会携带 `X-Should-Retry: false`；真实传输故障和上游重试提示仍保留原有语义。错误 API 根地址返回 HTML 成功页时，会直接提示 `base_url`/`api_backend` 配置问题且不暴露页面正文。

客户端搜索只依据结构化 `tool_choice` 选择工具，不再扫描提示词关键词。供应商专有搜索重放按渠道、前序对话和稳定搜索标识隔离；候选存在歧义时直接拒绝，不会拼接不相关的搜索块。

## 已打开 Grok Build 的同步与恢复

代理启用或停用后，hellogrok 会通过 ACP 连接现有 Grok Build 共享 leader，重载模型目录，并让空闲的自定义模型会话重新选择当前模型。这样无需新开窗口即可刷新会话内存中的 URL、后端、凭据和规范化模型 ID。正在活动、等待输入、已被外部配置替换及 `--no-leader` 的会话会被保守处理，并明确提示何时仍需在 `/model` 中手动重选。

当前与旧版 ACP 模型切换方法均受支持。Windows 上，Grok Build 1.0.0 误报为 stale 的活动命名管道 leader 只有在对应锁确实被占用时才会被接受。状态结构升级后，版本 5 的改写事务仍可恢复。

## Windows 状态、日志与端口处理

状态面板现在按代理、渠道、Grok 会话、协议与搜索、配置恢复分类显示。日志跨代理会话追加，默认保留最近七个实际使用日，并支持可选保留周期和循环查找。状态文本自动换行，原始日志行保留横向滚动。

`127.0.0.1:18787` 被占用时，包括 WinSock 错误 10048，hellogrok 会在修改 Grok 配置前发现冲突。异步托盘操作失败会直接显示，不再让界面停留在不确定状态。
