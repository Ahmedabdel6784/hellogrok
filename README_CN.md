<p align="center">
  <img src="./assets/hellogrok.png" alt="hellogrok 图标" width="128">
</p>

# hellogrok

跨平台 Grok Build 本地代理，让自定义模型渠道兼容常见 API 格式、Build 原生 Web 工具、独立鉴权和自动配置恢复。

[![Version](https://img.shields.io/badge/version-0.1.0-2f6feb.svg)](./internal/appinfo/appinfo.go)
[![Go](https://img.shields.io/badge/Go-1.26.5-00ADD8.svg)](./go.mod)
[![License: MIT](https://img.shields.io/badge/license-MIT-green.svg)](./LICENSE)
[![Platforms](https://img.shields.io/badge/platform-Windows%20%7C%20Linux%20%7C%20macOS-lightgrey.svg)](#平台支持)

[English](./README.md) · [简体中文](./README_CN.md)

## 目录

- [为什么需要 hellogrok](#为什么需要-hellogrok)
- [功能](#功能)
- [搜索与配置](#搜索与配置)
- [快速开始](#快速开始)
- [平台支持](#平台支持)
- [托盘与 CLI](#托盘与-cli)
- [开机启动](#开机启动)
- [工作原理](#工作原理)
- [故障排查](#故障排查)
- [开发与测试](#开发与测试)
- [使用限制](#使用限制)
- [参与贡献](#参与贡献)
- [许可证](#许可证)

## 为什么需要 hellogrok

Grok Build 可以接入自定义模型端点，但不同服务商实际提供的协议、响应格式、鉴权方式和 Web 搜索能力并不一致。一个能够通过 `curl` 返回文本的渠道，在正常 Grok Build 对话中仍可能失败、无法使用原生 Web 工具，或者收到错误的登录凭据。

hellogrok 为这些自定义渠道提供统一的本地兼容层。运行时准备 Grok 必需配置，让每个渠道固定使用自己的端点和凭据，支持 Grok Build 原生 Web 工作流，并在停止时恢复原始配置。

它适合需要维护多个第三方模型渠道，并希望直接在 Grok Build 中切换模型，而不想每次手动修改 URL 或工具设置的用户。

## 功能

### 渠道兼容

- 支持上游 `responses`、`chat_completions` 和 Anthropic 兼容 `messages` API。
- 将受支持的上游响应和流规范化为 Grok Build 可用格式。
- 保留每个渠道配置的上游 URL 路径和模型标识。
- 使用前准备所有显式自定义渠道，避免通过 `/model` 切换后首次请求失败。

### 原生 Web 工具

- 支持 hosted 和客户端搜索两种 Grok Build 原生 `web_search` 工作流。
- 当前代理工具权限允许时，将 `web_fetch` 作为独立工具保留。
- 让受支持的子代理使用相同的搜索行为。
- Grok 官方模型继续使用 Grok Build 原生搜索和登录路径。

### 鉴权与配置安全

- 支持渠道自己的 API key、环境变量密钥、鉴权提供器和请求头。
- 避免把 Grok 官方登录令牌发送给无关的自定义渠道。
- 代理启动时检查并临时补全 Grok 必需设置。
- 正常停止、退出托盘、Ctrl+C、SIGTERM 或启动失败时恢复原始值。
- 异常退出后可以使用 `hellogrok restore` 恢复代理管理的设置。

### 桌面与运维

- 提供 Windows 原生托盘程序和控制台 CLI。
- 记住用户选择的代理启用状态，并在下次打开托盘时恢复。
- 支持 Windows、Linux 和 macOS 登录自启动。
- 提供路由检查、当前状态、实时日志窗口和终端日志跟踪。
- 支持 Windows、Linux、macOS 的 amd64 和 arm64 构建。

hellogrok 是 Grok Build 渠道代理，不是系统代理、PAC 服务、VPN 或通用 HTTPS 拦截工具。

## 搜索与配置

### 搜索模式

搜索行为取决于当前自定义模型的 `supports_backend_search` 设置：

| 设置 | 搜索行为 |
|------|----------|
| `supports_backend_search = true` | 上游端点支持时，由当前渠道使用自身的 hosted Web search。 |
| `false` 或缺省，同时配置了 `[models].web_search` | Grok Build 暴露客户端 `web_search`，并使用已配置的搜索模型。 |
| `false` 或缺省，未配置 `[models].web_search`，但存在有效的 xAI 官方登录或 API 凭据 | Grok Build 可以使用官方默认搜索模型。 |
| 没有可用的 hosted 或客户端搜索路径 | 当前模型无法使用 `web_search`。 |
| `web_fetch` | 独立于搜索模型选择，并受当前工具权限限制。 |

自定义渠道缺省 `supports_backend_search` 时，代理运行期间按 `false` 处理。hellogrok 不会创建、选择或替换 `[models].web_search`；用户显式配置的模型始终是客户端搜索模型。

### 配置示例

下面的示例使用已有自定义渠道作为 Grok Build 客户端搜索模型：

```toml
[models]
web_search = "deepseek-v4-flash"

[model.deepseek-v4-flash]
model = "deepseek-v4-flash"
base_url = "https://api.example.com/v1"
env_key = ["DEEPSEEK_API_KEY"]
api_backend = "responses"
supports_backend_search = false
```

如果希望由当前渠道执行自身的 hosted search，并且配置的上游端点确实支持搜索，则改为 `supports_backend_search = true`。

### 支持的渠道设置

| 设置 | 是否必需 | 默认值 | 用途 |
|------|----------|--------|------|
| `model` | 否 | 模型表 ID | 发送给上游渠道的模型标识。 |
| `base_url` 或 `api_base_url` | 是 | 无 | 自定义上游端点；没有自定义 URL 的模型不会进入代理。 |
| `api_backend` | 否 | `chat_completions` | 上游 API 格式：`responses`、`chat_completions` 或 `messages`。 |
| `api_key` | 三选一 | 无 | 静态渠道凭据；共享配置建议优先使用 `env_key`。 |
| `env_key` | 三选一 | 无 | 保存渠道凭据的环境变量名或按顺序尝试的名称列表。 |
| `auth_provider` | 三选一 | 无 | Grok 命令式鉴权提供器。 |
| `auth_scheme` | 否 | 随后端确定 | 上游鉴权方式，包括 Bearer 和 `X-Api-Key` 风格。 |
| `extra_headers` | 否 | 空 | 额外的渠道自有 HTTP 请求头。 |
| `env_http_headers` | 否 | 空 | 从环境变量读取的 HTTP 请求头。 |
| `supports_backend_search` | 否 | `false` | 选择 hosted search（`true`）或 Grok Build 客户端搜索（`false`）。 |

模型设置可以直接写在 `[model.<id>]` 下，也可以从引用的 `[model_providers.<id>]` 继承；模型级设置优先。

不要手动把自定义渠道 URL 设置成 hellogrok 的本地地址。本地临时 URL 只应由应用在代理运行期间管理。

## 快速开始

### 前置条件

- Grok Build 可以读取 `~/.grok/config.toml`，其中至少配置了一个自定义模型 URL。
- 每个自定义渠道均有有效的凭据来源。
- 从源码编译需要 Go **1.26.5**。

当前实测基线为 Grok Build **0.2.118**。Grok Build 的自定义模型行为可能继续变化，使用更新版本时应运行仓库内的冒烟测试。

可以通过 `GROK_HOME` 指定 `~/.grok` 以外的 Grok 配置目录。

### Windows

```powershell
git clone https://github.com/hellowind777/hellogrok.git
cd hellogrok
.\scripts\build.ps1
.\dist\hellogrok-cli.exe routes
.\dist\hellogrok.exe
```

通过托盘菜单选择“启动代理”，然后新开一个 Grok Build 进程，使其重新读取已准备的模型配置。

### Linux 或 macOS

```bash
git clone https://github.com/hellowind777/hellogrok.git
cd hellogrok
mkdir -p dist
CGO_ENABLED=0 go build -trimpath -o dist/hellogrok ./cmd/hellogrok
./dist/hellogrok routes
./dist/hellogrok start
```

成功启动时会显示本地渠道端点和配置改写成功信息。使用 Grok Build 期间保持进程运行；Ctrl+C 或 SIGTERM 会停止代理并恢复原始配置。

### 首次使用检查

1. 执行 `hellogrok routes`，确认需要使用的自定义模型均已列出，后端和鉴权来源正确。
2. 启动 hellogrok 后再新开 Grok Build 进程。
3. 使用 `/model` 切换模型并测试普通对话。
4. 根据当前搜索模式分别测试 `web_search` 和 `web_fetch`。
5. 正常停止 hellogrok，确认 Grok Build 配置不再指向本地代理。

## 平台支持

| 平台 | 标准交互方式 | 标签发布产物 | 架构 |
|------|--------------|--------------|------|
| Windows | 原生托盘和 CLI | GUI 与控制台 `.exe` | amd64、arm64 |
| Linux | 前台 CLI 或 systemd 用户服务 | CLI 二进制 | amd64、arm64 |
| macOS | 前台 CLI 或 LaunchAgent | CLI 二进制 | amd64、arm64 |

标准发布二进制使用 `CGO_ENABLED=0`，标签发布流程会同时生成 SHA-256 校验文件。

Linux 和 macOS 用户可以从源码编译可选托盘界面：

```bash
CGO_ENABLED=1 go build -trimpath -tags tray -o dist/hellogrok-tray ./cmd/hellogrok
```

Linux 托盘构建需要 GTK 3 和 AppIndicator 开发包，macOS 托盘构建需要 Xcode Command Line Tools。标准 Unix CLI 不依赖这些桌面组件。

当前 Windows 和 macOS 产物没有代码签名或公证。

## 托盘与 CLI

### 托盘功能

Windows 托盘程序和可选 Unix 托盘版提供：

- **启动代理**：启动或停止代理，并记住当前选择。
- **开机启动**：启用或禁用登录自启动。
- **状态与日志**：打开当前状态和实时日志窗口。
- **退出**：恢复配置、停止代理并退出程序。

托盘记忆状态与前台运行的 `hellogrok start` 命令相互独立。

### CLI 命令

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

### 运行数据

| 平台 | 位置 |
|------|------|
| Windows | `%LOCALAPPDATA%\hellogrok` |
| Linux 和 macOS | `~/.hellogrok` |

运行数据包括应用偏好、日志以及用于恢复代理管理配置的恢复状态。

## 开机启动

### Windows

通过托盘启用“开机启动”，或执行 `hellogrok autostart enable`。登录启动会打开托盘，并按照记忆的代理启用状态运行。

### Linux

标准 CLI 会注册 systemd 用户服务。启用并立即启动：

```bash
./dist/hellogrok autostart enable
systemctl --user start hellogrok.service
systemctl --user status hellogrok.service
```

### macOS

标准 CLI 会注册当前用户的 LaunchAgent。启用并立即加载：

```bash
./dist/hellogrok autostart enable
launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/com.hellogrok.proxy.plist"
```

自启动配置会记录当前可执行文件的绝对路径；移动二进制后需要禁用再重新启用。`env_key`、`env_http_headers` 或 `GROK_HOME` 使用的变量必须对登录启动进程可见，不能只存在于当前终端。

## 工作原理

```text
Grok Build
    |
    v
hellogrok local channel proxy
    |
    v
Configured custom API channel
```

启动时，hellogrok 校验自定义渠道并临时将其指向本地代理；会话期间，每个请求都按该渠道配置的 API、凭据、模型和搜索模式转发；停止时恢复原始 Grok 配置。

原生 `web_search`、`web_fetch`、Grok 官方登录行为和受支持的子代理工作流仍由 Grok Build 管理，不会被替换成独立搜索服务。

## 故障排查

### 没有发现自定义路由

确认目标 `[model.<id>]` 或其引用的 provider 配置了有效的 `base_url` 或 `api_base_url`。没有自定义 URL 的官方模型会被有意排除。

### 无法使用 `web_search`

检查当前模型的搜索模式。`true` 渠道需要上游真实提供 hosted search；`false` 或缺省渠道需要有效的 `[models].web_search` 模型或可用的 xAI 官方凭据。`web_fetch` 与搜索模型独立，但仍可能被当前工具权限排除。

### 请求返回 401、403 或 502

执行 `hellogrok routes` 并查看“状态与日志”，确认渠道 URL、后端、凭据来源、模型标识和服务商状态。上游故障、限流、不支持的载荷或被中转丢弃的搜索工具需要由服务商或中转解决。

### 强制退出后配置仍指向 localhost

先确认没有 hellogrok 进程正在运行，再执行 `hellogrok restore`。不要对正在运行的代理执行 `restore`。

### 端口 `18787` 已占用

启动新实例前先停止现有 hellogrok。同一份 Grok 配置目录只能由一个实例管理。

### 开机启动成功，但渠道没有凭据

把只在终端中存在的环境变量写入持久用户环境或服务环境，然后重新启动登录服务。自启动进程无法继承先前终端会话里的临时变量。

## 开发与测试

执行本地质量检查：

```bash
go test ./... -count=1
go vet ./...
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
```

Windows 用户配置真实渠道后，可以运行集成冒烟测试：

```powershell
.\scripts\run_grok_all_channels_test.ps1
.\scripts\run_grok_all_channels_test.ps1 -RequireWebSearch -MaxTurns 1 -TimeoutSeconds 150
.\scripts\run_grok_all_channels_test.ps1 -RequireSubagentSearch -MaxTurns 4 -TimeoutSeconds 240
.\scripts\run_grok_all_channels_test.ps1 -RequireWebFetch -MaxTurns 2 -TimeoutSeconds 150
```

CI 会在 Windows、Linux、Intel macOS 和 Apple Silicon macOS 上运行测试与默认构建，并在 Linux 和 macOS 原生构建可选托盘目标。标签发布会生成三个操作系统的 amd64 与 arm64 产物。

## 使用限制

- hellogrok 无法创造服务商侧的搜索能力；hosted search 渠道必须真实支持搜索并返回结果。
- 中转如果主动删除工具声明、工具调用、引用或结果事件，下游无法完整恢复。
- 超出受支持 Responses、Chat Completions 和 Messages 格式的服务商私有扩展可能需要单独适配。
- 上游可用性、模型权限、账号池、限流和网关错误仍由服务商负责。
- 可选 Unix 托盘依赖已安装的桌面环境；标准 Unix CLI 是更通用的使用方式。
- 当前发布产物未签名；对本地信任有要求时应从源码构建。

## 参与贡献

1. 为改动创建目标明确的分支。
2. 遵循现有包边界，不夹带无关重构。
3. 行为变化需要新增或更新测试。
4. 执行上方质量检查。
5. 用户可见行为变化时同步更新两份 README。
6. 提交 Pull Request，并说明问题、实现方式和验证结果。

## 许可证

本项目采用 [MIT License](./LICENSE) 授权。
