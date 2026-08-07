<p align="center">
  <img src="./assets/hellogrok.png" alt="hellogrok 图标" width="128">
</p>

# hellogrok

跨平台 Grok Build 本地代理，让自定义模型渠道兼容常见 API 格式、Build 原生 Web 工具以及独立的渠道鉴权。

当前版本：**0.1.0**

[English](./README.md) · [简体中文](./README_CN.md)

## 项目作用

hellogrok 用于把 Grok Build 接入采用 OpenAI Responses、OpenAI Chat Completions 或 Anthropic Messages API 的自定义模型渠道，并在所选搜索模式可用时保留 Grok Build 原生的 `web_search` 和 `web_fetch` 工作流。

启用代理后，hellogrok 会检查并临时准备 Grok 必需配置，把每个自定义渠道路由到对应的本地端点，同时隔离渠道凭据与 Grok 官方登录鉴权。停止代理时会恢复原始配置。

未配置自定义 URL 的 Grok 官方模型继续使用 Grok Build 原生的登录与网络路径。

## 功能

- **自定义渠道兼容**：支持 `responses`、`chat_completions` 和 `messages` 后端。
- **Grok Build Web 工具**：支持原生 `web_search` 以及独立的 `web_fetch` 工作流。
- **灵活搜索分流**：可以使用当前渠道自带的 hosted search，也可以使用 Grok Build 客户端搜索模型。
- **鉴权隔离**：避免 Grok 官方登录令牌覆盖自定义渠道已经配置的凭据。
- **响应兼容**：把受支持的上游响应和流规范化为 Grok Build 可用格式。
- **自动配置检查**：代理启动时准备必需设置，并在使用前完成校验。
- **精确配置恢复**：停止代理时恢复用户原值；异常退出后可以使用 `restore` 命令恢复。
- **模型热切换**：使用前准备所有显式自定义渠道，通过 `/model` 切换时无需手动修改 URL。
- **子代理支持**：让受支持的 Grok Build 子代理使用相同的渠道和 Web 工具设置。
- **托盘状态记忆**：记住代理启用状态，下次打开托盘时恢复用户选择。
- **状态与日志**：提供托盘状态、实时日志窗口、路由检查和恢复命令。
- **登录自启动**：支持 Windows、Linux 和 macOS 用户登录后自动启动。

hellogrok 是 Grok Build 渠道代理，不是系统代理、PAC 服务、VPN 或通用 HTTPS 拦截工具。

## 搜索模式

搜索行为取决于当前自定义模型的 `supports_backend_search` 设置以及 Grok Build 配置的搜索模型：

| 配置 | 结果 |
|------|------|
| `supports_backend_search = true` | 上游支持时，由当前渠道使用自身的 hosted Web search。 |
| `false` 或缺省，同时配置了 `[models].web_search` | Grok Build 暴露客户端 `web_search`，并使用已配置模型执行搜索。 |
| `false` 或缺省，未配置 `[models].web_search`，但存在有效的 xAI 官方登录或 API 凭据 | Grok Build 可以使用官方默认搜索模型。 |
| 没有可用的 hosted 或客户端搜索路径 | 当前模型无法使用 `web_search`。 |
| `web_fetch` | 作为独立的 Grok Build 工具保留，同时受当前工具权限限制。 |

hellogrok 不会创建、选择或替换 `[models].web_search`，只使用用户在 Grok 配置中选择的模型。

使用已配置客户端搜索模型的自定义渠道示例：

```toml
[models]
web_search = "deepseek-v4-flash"

[model.deepseek-v4-flash]
model = "deepseek-v4-flash"
base_url = "https://api.example.com/v1"
env_key = "DEEPSEEK_API_KEY"
api_backend = "responses"
supports_backend_search = false
```

`api_backend` 支持 `responses`、`chat_completions` 和 `messages`；缺省时按 Grok Build 兼容配置使用 `chat_completions`。

## 平台支持

| 平台 | 交互方式 | 架构 |
|------|----------|------|
| Windows | 原生托盘程序和 CLI | amd64、arm64 |
| Linux | CLI；可选源码托盘版 | amd64、arm64 |
| macOS | CLI；可选源码托盘版 | amd64、arm64 |

标准 Linux 和 macOS 构建不依赖 CGO。可选托盘版使用 `tray` 构建标签，并依赖相应平台的桌面开发库。

## 快速开始

### 前置条件

- Grok Build 可以读取 `~/.grok/config.toml`，其中至少配置了一个自定义模型 URL。
- 每个自定义渠道均配置了 `api_key`、`env_key` 或受支持的鉴权提供器。
- 从源码编译需要 Go **1.26.5**。

可以通过 `GROK_HOME` 指定 `~/.grok` 以外的 Grok 配置目录。

### Windows

```powershell
git clone https://github.com/hellowind777/hellogrok.git
cd hellogrok
.\scripts\build.ps1
.\dist\hellogrok-cli.exe routes
.\dist\hellogrok.exe
```

通过托盘菜单启用代理、设置登录自启动或打开状态与日志。

### Linux 或 macOS

```bash
git clone https://github.com/hellowind777/hellogrok.git
cd hellogrok
mkdir -p dist
CGO_ENABLED=0 go build -trimpath -o dist/hellogrok ./cmd/hellogrok
./dist/hellogrok routes
./dist/hellogrok start
```

使用 Grok Build 期间保持前台进程运行。通过 Ctrl+C 或 SIGTERM 停止，使配置得到正常恢复。

可选 Linux/macOS 托盘构建：

```bash
CGO_ENABLED=1 go build -trimpath -tags tray -o dist/hellogrok-tray ./cmd/hellogrok
```

Linux 需要 GTK 3 和 AppIndicator 开发包，macOS 需要 Xcode Command Line Tools。

## CLI

| 命令 | 用途 |
|------|------|
| `hellogrok start` | 在前台运行代理。 |
| `hellogrok version` | 输出当前安装版本。 |
| `hellogrok routes` | 列出自定义渠道路由，不输出凭据。 |
| `hellogrok restore` | 异常退出后恢复代理管理的设置。 |
| `hellogrok autostart enable` | 为当前可执行文件启用登录自启动。 |
| `hellogrok autostart disable` | 禁用登录自启动。 |
| `hellogrok autostart status` | 查看当前自启动状态。 |
| `hellogrok log` | 输出并打开日志文件。 |
| `hellogrok logview` | 在当前终端持续查看日志。 |
| `hellogrok help` | 显示命令帮助。 |

Windows 运行数据位于 `%LOCALAPPDATA%\hellogrok`，Linux 和 macOS 运行数据位于 `~/.hellogrok`。

## 使用说明

- 启用代理后重新启动 Grok Build，使其重新加载已准备的模型配置。
- 整个 Grok Build 会话期间需要保持 hellogrok 运行。
- 移动或替换可执行文件前，应先正常停止代理。
- 强制终止后，先确认代理进程已经停止，再执行 `hellogrok restore`。
- `env_key`、`env_http_headers` 或 `GROK_HOME` 使用的环境变量也必须对登录自启动进程可见。
- 自定义服务商必须真实支持其声明的 API 与搜索能力；hellogrok 无法补出服务商本身不存在的搜索后端。

## 许可证

本项目采用 [MIT License](./LICENSE) 授权。
