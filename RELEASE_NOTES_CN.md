# 发布说明 — v0.1.1

## 显式搜索模型选择

`[models].web_search` 配置项和 `GROK_WEB_SEARCH_MODEL` 环境变量现在正确地将 Grok Build 客户端搜索路由到选定模型。环境变量优先于配置文件的值。环境变量为空时视为未设置，回退到配置项。

## Hosted 搜索能力自动检测

缺省 `supports_backend_search` 的 Grok 中转渠道现在会在启动时自动探测。代理会检测中转是否提供 `web_search`、`x_search` 或两者兼有，并据此规范化 hosted 搜索请求。Chat Completions 渠道还会自动检测搜索方言（`search_parameters` 或 `web_search_options`），使搜索字段匹配提供商的预期格式。

## CC Switch 接管检测与冲突管理

hellogrok 现在会在重写 Grok 配置前检测 CC Switch 的 Grok Build 接管标记（`/grokbuild/v1` 路由上的 `PROXY_MANAGED`）。CC Switch 已持有配置文件时拒绝启动。托盘菜单在配置所有权冲突时也会推迟退出，避免 CC Switch 日后恢复指向已停服代理的路由。

## 单实例强制

同一登录会话中第二次启动托盘会直接退出并显示明确提示。前台命令也通过操作系统级锁强制单实例，避免重复代理进程争抢同一本地端口或配置文件。

## 首次启动默认启用代理

代理现在首次启动时默认启用。偏好手动启动的用户可以禁用一次，后续启动会记住该选择。

## 托盘退出保护

当供应商管理工具（如 CC Switch）仍持有 Grok 配置时，托盘会推迟退出以避免留下孤立代理地址。请先在另一工具中解决配置冲突，再退出 hellogrok。

## 信号处理健壮性

SIGINT 和 SIGTERM 处理程序现在会在遇到延迟错误（例如关闭过程中检测到配置所有权冲突）时重试停止序列，保持进程运行直到配置可以安全恢复。

## Windows 日志窗口图标

Windows 状态和日志窗口现在在标题栏和任务栏中显示应用程序图标。

## 启动时搜索路由解析

每个渠道的最终搜索模式——hosted 能力、客户端搜索选择或自动检测结果——现在在启动时解析并记录，使 `hellogrok routes` 和日志窗口能够报告每个渠道的实际搜索行为。
