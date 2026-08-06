# hellogrok

当前版本：**0.1.0**

跨平台本地反向代理，用于让 Grok Build 的自定义模型渠道兼容严格的 Responses 解析、渠道鉴权隔离和 Build 原生 `web_search`。

[English](./README.md) · [简体中文](./README_CN.md)

## 目录

- [为什么需要 hellogrok](#为什么需要-hellogrok)
- [功能](#功能)
- [平台支持](#平台支持)
- [快速开始](#快速开始)
- [CLI 命令](#cli-命令)
- [开机启动](#开机启动)
- [工作原理](#工作原理)
- [配置行为](#配置行为)
- [测试](#测试)
- [故障排查](#故障排查)
- [限制](#限制)

## 为什么需要 hellogrok

Grok Build 可以接入 OpenAI 兼容的自定义模型，但很多中转只实现了部分 Responses 数据结构。即使文本响应本身有效，中转也可能缺少 `annotations`、消息 ID、状态、usage details 或 SSE 序号，最终被 Grok Build 在反序列化阶段拒绝。

搜索还有另一层兼容问题。Grok Build 不会探测当前第三方端点是否支持搜索。在源码版本 `a5589e9` 中，只有 `[features].backend_tools`、当前模型的 `supports_backend_search`、agent 工具白名单以及有效搜索模型已取得凭据这四个静态条件同时成立，hosted `web_search` 才会生成。搜索模型优先取显式 `[models].web_search`，否则使用 Build 编译时默认值；无法鉴权时，Build 仍可能加入 xAI 专用的 `x_search`，却不加入 Web 搜索。backend search 一旦生效，Build 还会从普通工具列表删除客户端函数版 `web_search`。Responses 序列化器会输出 hosted 工具，但 Chat Completions 和 Anthropic Messages 序列化器目前会丢弃它们。补 `sequence_number` 等 Responses SSE 字段无法恢复请求侧能力；`sequence_number` 只负责响应事件排序。

hellogrok 在协议边界同时修复请求和响应。运行期间，每个显式自定义渠道在 Build 看来都是独立的 Responses 端点；代理再把这份规范请求转换回该渠道原有的 Responses、Chat Completions 或 Messages 协议。代理临时物化每个模型的有效 `supports_backend_search`：`true` 使用当前渠道自身的 hosted search；`false`（包括原字段缺省）保留 Build 的普通函数 `web_search`。显式 `[models].web_search` 优先；未设置时，只要存在 Grok 官方登录会话或 xAI API 凭据，Build 就能使用内建官方默认搜索模型。当新一轮用户消息明确要求搜索、联网、查询最新信息，且调用方没有指定其他工具时，代理会把这一轮定向到该函数；代理不会创建、选择或修改搜索模型。

## 功能

- **全渠道路由**：在第一次模型请求前改写所有显式自定义 `base_url`。
- **配置事务与精确恢复**：先提交恢复记录，再替换配置，并识别“状态已提交、配置尚未替换”的崩溃窗口；启动时回读校验必需字段，托盘停止、退出、Ctrl+C、SIGTERM 或启动失败时恢复所有代理管理字段，异常强制终止后由下次启动或 `restore` 恢复。已配置 `[subagents]` 树但缺省 `enabled` 时会临时修复为 `true`，显式 `false` 不会被覆盖。
- **托盘状态记忆**：记住用户上次启用或停用代理的选择，重新打开托盘后自动恢复运行状态。
- **三协议外观层**：对 Build 暴露统一严格的 Responses 契约，再转换到渠道原有的 Responses、Messages 或 Chat Completions 端点。
- **Responses 兼容**：增量补齐响应和 SSE 必填字段，同时保留上游已有值。
- **流形态兼容**：中转忽略 `stream = true`、返回完整 Responses JSON 时，转换为合法 Responses SSE；上游流格式损坏、过大或缺少终止事件时返回规范 `event: error`，不静默截断。
- **鉴权隔离**：向第三方请求前，用已配置的渠道 key 替换无关的 Grok OAuth token。
- **按能力分流搜索**：`supports_backend_search = true` 使用当前渠道 hosted search；`false` 或缺省保留 Build 通过显式 `[models].web_search` 或已鉴权官方默认模型执行的客户端 `web_search`，并在明确搜索意图下优先选择它而不是 `web_fetch`。
- **Grok Build 搜索方言**：只对已有 hosted 请求做规范化；Grok 中转保留一个 `web_search` 加一个 `x_search`，客户端搜索的工具结果回填轮保持自动选择，避免重复调用。
- **工具选择保真**：转换到 Messages 或 Chat 时严格执行 Responses `allowed_tools`，Messages 工具参数损坏时明确失败，不再静默替换语义。
- **搜索证据诊断**：区分工具已声明、实际已调用和已返回来源，且不记录提示词、查询文本、URL 或凭据。
- **跨平台核心**：Windows、Linux、macOS 发布构建均使用 `CGO_ENABLED=0`。
- **可选 Unix 托盘**：Linux/macOS 用户可以使用 `tray` 构建标签自行编译 CGO 托盘版本。

hellogrok 是路径路由代理，不是系统代理、PAC 或通用 HTTPS 拦截工具。

## 平台支持

| 平台 | 官方产物 | 默认交互方式 | 架构 |
|------|----------|--------------|------|
| Windows | GUI 和控制台二进制 | 托盘界面，同时支持 CLI 子命令 | amd64、arm64 |
| Linux | 无 GUI/CGO 的 Go CLI | 前台 CLI 或 systemd 用户服务 | amd64、arm64 |
| macOS | 无 GUI/CGO 的 Go CLI | 前台 CLI 或 LaunchAgent | amd64、arm64 |

Linux/macOS 托盘版本仅提供源码构建方式，不影响代理核心使用。

标签发布会同时提供 SHA-256 校验文件。发布二进制目前没有代码签名或公证；需要本地可信构建的用户应从源码编译。下载的 Linux/macOS 二进制可能需要先执行 `chmod +x <downloaded-file>`。

## 快速开始

### 前置条件

- 已安装作为实测基线的 Grok Build 0.2.118，并存在可读取的 `~/.grok/config.toml`。更新版本可能改变私有客户端搜索请求形态，必须通过仓库内的渠道/搜索冒烟测试后再使用；不兼容更旧版本。
- 每个第三方端点配置了渠道 `api_key` 或 `env_key`。
- `supports_backend_search = true` 时，上游必须在已配置协议上实现 hosted Web 搜索；设为 `false` 或缺省且需要客户端搜索时，必须有能解析且有凭据的 `[models].web_search`，或能鉴权 Build 官方默认搜索模型。
- 从源码编译需要 Go 1.26.5。

### Windows 编译

```powershell
git clone https://github.com/hellowind777/hellogrok.git
cd hellogrok
.\scripts\build.ps1
```

运行 `dist\hellogrok.exe` 使用托盘，或在终端中使用 `dist\hellogrok-cli.exe`。

### Linux 或 macOS 编译

```bash
git clone https://github.com/hellowind777/hellogrok.git
cd hellogrok
mkdir -p dist
CGO_ENABLED=0 go build -trimpath -o dist/hellogrok ./cmd/hellogrok
./dist/hellogrok routes
./dist/hellogrok start
```

成功启动时应看到：

```text
channel facade on http://127.0.0.1:18787/c/<channel>/responses
config rewrite all: ...
```

使用 Grok Build 期间保持进程运行。Ctrl+C 或 SIGTERM 会停止代理并恢复 Grok 配置。

### 可选 Linux/macOS 托盘

Linux 需要 GTK 和 AppIndicator 开发包：

```bash
# Debian / Ubuntu
sudo apt-get install gcc libgtk-3-dev libayatana-appindicator3-dev
CGO_ENABLED=1 go build -trimpath -tags tray -o dist/hellogrok-tray ./cmd/hellogrok
```

macOS 先安装 Xcode Command Line Tools：

```bash
xcode-select --install
CGO_ENABLED=1 go build -trimpath -tags tray -o dist/hellogrok-tray ./cmd/hellogrok
```

对外分发 macOS 托盘程序时还应封装、签名并公证 `.app`。官方 macOS 产物保持 CLI 形态，因此不依赖 CGO 或桌面框架。

## CLI 命令

```text
hellogrok start
hellogrok version
hellogrok restore
hellogrok routes
hellogrok autostart enable
hellogrok autostart disable
hellogrok autostart status
hellogrok log
hellogrok logview
hellogrok help
```

| 命令 | 行为 |
|------|------|
| `start` | 在前台运行代理；Ctrl+C/SIGTERM 会执行完整恢复。 |
| `version` | 输出当前安装的 hellogrok 版本。 |
| `restore` | 异常退出后恢复代理管理的配置；不要对仍在运行的代理执行。 |
| `routes` | 列出路由主机和模型 ID，不打印凭据。 |
| `autostart ...` | 管理当前可执行文件的登录自启动。 |
| `log` | 打印日志路径，并使用系统默认程序打开。 |
| `logview` | 在当前终端持续查看日志。 |

数据位置：

| 平台 | 运行数据 |
|------|----------|
| Windows | `%LOCALAPPDATA%\hellogrok` |
| Linux/macOS | `~/.hellogrok` |

可通过 `GROK_HOME` 覆盖默认 Grok 目录 `~/.grok`。

托盘的“启动代理”选择保存在上述目录的 `settings.json`。手动勾选并启动成功后记为启用，手动取消后记为停用。退出 hellogrok 时仍会先恢复 Grok 配置，但不会清除启用意图；下次打开托盘会自动重新启动代理。命令行 `hellogrok start` 不修改这项托盘偏好。

## 开机启动

### Windows

使用托盘复选框或执行 `hellogrok autostart enable`。它使用 Microsoft 文档规定的当前用户 [`Run` 登录启动键](https://learn.microsoft.com/en-us/windows/win32/setupapi/run-and-runonce-registry-keys)注册当前可执行文件；登录时打开托盘，并按上次“启动代理”的记忆状态决定是否自动启用代理。Windows 可能延后执行登录启动项，并不保证登录后立即启动。状态检查同时确认注册目标文件仍然存在，移动二进制后需重新启用。

### Linux

无界面发行版的 `autostart enable` 会写入并启用 `~/.config/systemd/user/hellogrok.service`，以 `hellogrok start` 在登录后直接运行代理。使用 `tray` 标签自行编译的托盘版则注册无参数启动，登录后打开托盘并采用记忆的代理状态。状态检查要求 systemd unit 已启用、注册目标存在且具有执行权限。unit 记录当前可执行文件的绝对路径，移动二进制后需要先禁用再重新启用。systemd 官方说明 [`enable` 只建立启动依赖，不会立即启动服务](https://github.com/systemd/systemd/blob/main/man/systemctl.xml)，所以首次启用后需执行下面的 `systemctl --user start` 或重新登录。

```bash
./dist/hellogrok autostart enable
systemctl --user start hellogrok.service
systemctl --user status hellogrok.service
```

### macOS

无界面发行版的 `autostart enable` 会写入 `~/Library/LaunchAgents/com.hellogrok.proxy.plist`，以 `hellogrok start` 在登录后直接运行代理。Apple 文档确认每用户 launchd 会在登录时读取 `~/Library/LaunchAgents` 并向退出中的 agent 发送 `SIGTERM`，[`ProgramArguments` 定义程序与参数](https://developer.apple.com/library/archive/documentation/MacOSX/Conceptual/BPSystemStartup/Chapters/CreatingLaunchdJobs.html)。使用 `tray` 标签编译的托盘版会注册交互式无参数启动并采用记忆的代理状态；LaunchAgent 能显示界面但 Apple 更推荐正式 `.app` 使用 Login Item，因此源码托盘模式仍需在目标 macOS 版本实测。启用时会清除 launchd 的持久化禁用状态；状态检查会验证 plist、目标程序和 launchd 禁用状态。下次登录时自动加载。立即载入可执行：

```bash
./dist/hellogrok autostart enable
launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/com.hellogrok.proxy.plist"
```

禁用开机启动时会阻止后续登录启动并删除 plist，但不会终止当前正在运行的 LaunchAgent；需要立即停止时应另行结束当前进程。

Windows 注册表行为已在 Windows 上进行真实启用、状态检查和禁用回环测试。Linux 使用标准 systemd 用户服务，macOS 使用标准 LaunchAgent；两端生成配置、路径转义、启动参数和 amd64/arm64 编译均由测试覆盖，但发布前仍应在对应操作系统的真实登录会话各做一次集成验证。

登录启动进程不会继承只在当前终端临时设置的环境变量。若配置依赖 `GROK_HOME`、模型 `env_key` 或 `env_http_headers`，请把它们写入操作系统的持久用户环境，或写入对应的 systemd/LaunchAgent 环境配置；否则手动启动可用的渠道可能在登录启动后缺少配置或凭据。

## 工作原理

```text
Grok Build
  -> Responses: http://127.0.0.1:18787/c/<channel>/responses
  -> hosted search 或客户端 web_search 函数 + 独立 web_fetch
  -> 渠道模型/鉴权隔离
  -> 原始 Responses | Messages | Chat Completions 端点
  <- 规范 Responses 响应/SSE + 增量字段补全
  <- Grok Build
```

代理在改写配置前把原始路由保存在内存中。本地 URL 只包含转义后的渠道 ID，不包含上游主机名或凭据：

| 原始 `base_url` | 临时代理 URL |
|-----------------|-------------|
| 模型 `gpt-main` 的 `https://congee.pro/v1` | `http://127.0.0.1:18787/c/gpt-main` |
| 模型 `deepseek-pro` 的 `https://api.deepseek.com/anthropic` | `http://127.0.0.1:18787/c/deepseek-pro` |
| 模型 `local-chat` 的 `http://localhost:8000/v1` | `http://127.0.0.1:18787/c/local-chat` |

Build 会在临时 base 后追加 `/responses`。hellogrok 根据渠道原始 `api_backend`，在保留的原始 base 后追加 `/responses`、`/messages` 或 `/chat/completions`；不会自行添加或删除 `/v1`、`/anthropic` 等前缀。

这套外观层依据 Grok Build 的真实序列化边界，而不是猜测配置习惯。在源码版本 `a5589e9` 中，[agent builder 把 hosted Web Search 绑定到 `web_search_config.is_enabled()`，却独立加入 X Search](https://github.com/xai-org/grok-build/blob/a5589e958437d79e13db026eedcb1720bffd4063/crates/codegen/xai-grok-agent/src/builder.rs#L1191-L1198)；[每轮门控既检查 `supports_backend_search`，又会删除函数版 Web Search](https://github.com/xai-org/grok-build/blob/a5589e958437d79e13db026eedcb1720bffd4063/crates/codegen/xai-grok-shell/src/session/acp_session_impl/sampler_turn.rs#L139-L182)。[Responses 转换器会序列化 hosted Web Search，并另行支持 xAI 的 X Search](https://github.com/xai-org/grok-build/blob/a5589e958437d79e13db026eedcb1720bffd4063/crates/codegen/xai-grok-sampling-types/src/conversation/responses.rs#L315-L380)；[Chat 转换器只读取函数工具，并把 `search_parameters` 留空](https://github.com/xai-org/grok-build/blob/a5589e958437d79e13db026eedcb1720bffd4063/crates/codegen/xai-grok-sampling-types/src/conversation/chat_completions.rs#L252-L307)；[Messages 转换器同样只读取函数工具](https://github.com/xai-org/grok-build/blob/a5589e958437d79e13db026eedcb1720bffd4063/crates/codegen/xai-grok-sampling-types/src/conversation/messages.rs#L290-L350)。最后，[`web_fetch` 有自己独立的 feature/env/remote 配置解析器](https://github.com/xai-org/grok-build/blob/a5589e958437d79e13db026eedcb1720bffd4063/crates/codegen/xai-grok-shell/src/agent/config.rs#L2522-L2531)，`[models].web_search` 无法启用它。

独立实现的 grok2api 也证实了这个不直观的 Grok CLI 路由规则：对具备稳定缓存会话的 Build 请求，它会在已有 `web_search` 时[补入 `x_search`](https://github.com/chenyme/grok2api/blob/56cfd210ce33169997b709ec7529b90c708f919b/backend/internal/infra/provider/cli/responses_cache_route.go#L45-L73)，并明确说明这是官方 Build 行为要求的路由项。这不表示 X Search 是 Web Search 的别名，而是 Grok Build 上游在这条路由上需要同时看到二者。因此 hellogrok 只对 Grok Responses 路由保留一对工具。

部分 Grok 中转会把普通函数名 `web_search` 截获成自己的 hosted search，即使渠道按客户端搜索配置，也不会返回 Build 所需的函数调用。对于 `supports_backend_search = false`/缺省路由，hellogrok 因此只在上游线缆上把这个普通函数临时改名为无冲突的 `hellogrok_client_web_search`（若用户已有同名工具则自动加数字后缀），并同步改写工具选择和历史函数调用；上游响应返回后再映射回 `web_search`。Build、界面、会话历史和搜索模型始终只看到原生名称，hosted `web_search`/`x_search` 声明也不会被改名。

hellogrok 不会把中转 URL 替换成 grok2api 使用的私有上游。grok2api 访问 `https://cli-chat-proxy.grok.com/v1` 时使用 [Build OAuth 及 CLI 专用鉴权与会话头](https://github.com/chenyme/grok2api/blob/56cfd210ce33169997b709ec7529b90c708f919b/backend/internal/infra/provider/cli/adapter.go#L803-L854)；Console 路由还要求 SSO、DPoP 逐请求签名、浏览器状态和 `x-cluster`。中转公开 API key 不属于其中任何一种凭据，只改 URL 会把有效的中转请求变成未鉴权的上游请求。

常见增量补全包括：

- response `created_at` 和 `model`
- Responses SSE `sequence_number`
- `output_text.annotations` 和 `logprobs`
- message/output item `id` 和 `status`
- `web_search_call.action`、`query` 和 `sources`
- 输入/输出 token details
- 空 chat `finish_reason` 规范化为 `null`

## 配置行为

启动时，hellogrok 会：

1. 先独占 `127.0.0.1:18787`；第二个实例会在接触配置前失败，不会误恢复正在运行实例的配置。
2. 恢复上次异常退出留下的代理管理字段。
3. 加载所有有效模型或 provider 配置中显式存在 `base_url` 或 `api_base_url` 的 `[model.*]`。
4. 临时确保 `[features].backend_tools = true` 和 `[features].web_fetch = true`。如果已经存在 `[subagents]` 配置树却缺少 `enabled`，还会临时补入 `enabled = true`；Grok Build 0.2.118 否则会把这类局部表反序列化为禁用子代理。显式 `true` 和 `false` 始终归用户所有。
5. 把每个显式模型节的 `base_url`（以及实际使用的 `api_base_url`）改写到独立本地外观 URL，临时确保 `api_backend = "responses"`，并物化有效 `supports_backend_search` 布尔值。模型和 provider 都未设置时写入 `false`，防止远端模型目录能力误隐藏客户端搜索。没有自定义 URL 的 xAI/Grok 官方条目保持不变。如果配置残留了本地代理 URL 却没有对应恢复记录，启动会直接失败，不会静默丢失其上游来源。
6. 在落盘前解析并校验生成的 TOML，落盘后重新读取并再次校验每个必需字段；任一步失败都会立即回滚并拒绝启动。
7. 仅当路由的有效 `supports_backend_search` 为 `true` 时补 hosted `web_search`。为 `false` 时保留普通函数；在一轮新的、明确要求搜索或最新信息且工具选择仍为自动时，强化 `web_search`/`web_fetch` 的用途说明并定向选择 `web_search`。工具结果回填轮不再强制，避免循环。请求若本来已带 hosted 声明仍会规范化，因为它可能是 Build 客户端搜索模型发出的第二笔请求；代理会在这笔非流式搜索执行请求中选择 hosted `web_search`，并要求搜索模型返回含来源 URL 的最终文本。
8. 用当前渠道配置的 key 替换无关 OAuth bearer token，并在响应返回时只补齐缺失字段。

原字段是否存在、原始整行、注释和换行方式会在改写前写入恢复状态，而且恢复记录先于配置提交。如果进程恰好死在配置替换之前，下次恢复会识别所有管理值仍为原值并丢弃这份未应用记录。正常停止或启动失败时，hellogrok 会恢复 `base_url`、`api_base_url`、`api_backend`、`supports_backend_search`、`backend_tools`、`web_fetch` 和代理新增的 `[subagents].enabled`，并删除由代理新增的字段或父表；`[models].web_search`、用户显式设置的子代理开关和其他无关 TOML 从不改写。操作系统强制结束进程时，已经持久化的恢复状态会由下次启动自动处理，也可运行 `hellogrok restore`；突发断电能否保留最后一次文件系统写入仍取决于操作系统、文件系统和存储设备的持久化保证。

有效开关既可写在 `[model.<id>]`，也可从 `[model_providers.<id>]` 继承。模型项显式值优先，因此模型项的 `false` 可以覆盖 provider 的 `true`；只有两处都没有配置时，hellogrok 才在代理运行期间物化为 `false`。非布尔值会在任何配置写入前终止启动。最终分流如下：

| 有效 `supports_backend_search` | 搜索模型状态 | 当前模型可见的搜索 | 执行方 |
|--------------------------------|--------------|--------------------|--------|
| `true` | 任意 | 当前路由支持的 hosted `web_search` / `x_search` | 当前渠道上游 |
| `false` 或缺省 | 显式 `[models].web_search` 可解析且有凭据 | 普通函数 `web_search` | Grok Build 客户端，通过配置模型 |
| `false` 或缺省 | 未显式配置，已有官方登录或 xAI API 凭据 | 普通函数 `web_search` | Grok Build 客户端，通过编译时官方默认模型 |
| `false` 或缺省 | 两种搜索模型鉴权路径都不可用 | 无 `web_search` | 无 |

即使已经登录官方账号，显式搜索模型仍然优先于编译时默认值。`web_fetch` 与四行状态都无关，始终是 Build 本地函数。没有自定义 URL 的官方模型继续使用 Grok Build 原生能力和 OAuth 鉴权。

不同协议采用不同适配：

| `api_backend` | 发给上游的请求 | 响应处理 |
|---------------|----------------|----------|
| `responses` | 保留普通 `web_search`/`web_fetch` 函数；明确搜索意图可定向客户端 `web_search`；客户端函数只在线缆上使用防冲突别名；hosted 请求发送一个标准 `web_search`，仅 Grok 路由另加一个 `x_search`，并只删除冲突的同名搜索函数 | 直接流式转发，先还原客户端函数名，记录真实搜索证据，并只补齐严格 Responses 模型缺失的字段 |
| `messages` | 转换普通函数、线缆别名及定向工具选择；只有输入已有 hosted 声明时才转换为 `web_search_20250305`，默认使用 `x-api-key` | 把 thinking、服务端搜索块、引用、工具调用、usage 和正文转换为规范 Responses，并还原客户端函数名；按渠道在有界内存缓存中保留原始搜索块供后续轮次回放 |
| `chat_completions` | 转换普通函数、线缆别名及定向工具选择；只有输入已有 hosted 声明时，Grok 路由才加入旧版 xAI `search_parameters`，其他兼容路由加入 `web_search_options` | 把非流式 Chat 响应转换为规范 Responses，并还原客户端函数名；只有引用、搜索结果或正数搜索用量能证明实际执行时，才生成 `web_search_call` |

DeepSeek 渠道的原始后端应按当前官方能力配置：`deepseek-v4-flash` 使用原生 [Responses 端点与 hosted `web_search`](https://api-docs.deepseek.com/zh-cn/guides/responses_api/)，并仅在该端点提供服务端搜索时设置 `supports_backend_search = true`；`deepseek-v4-pro` 在 DeepSeek 正式宣布支持 Responses 前继续使用 [Anthropic 兼容端点](https://api-docs.deepseek.com/zh-cn/guides/anthropic_api/)。需要改用 Build 配置的客户端搜索模型时应设为 `false`。DeepSeek 会在 Responses 响应中回显 `{"type":"web_search"}` 的 `tool_choice`，而 Build 当前固定的 async-openai 响应类型不接受该枚举；hellogrok 只在回发 Build 时把这个描述字段规范化为 `"auto"`，不会改动发给 DeepSeek 的请求或搜索结果。

启用代理后需要新开 Grok Build 进程，使它重新读取临时模型设置。`true` 路由应显示 `hosted_web_search=1`、`function_web_search=0`；`false`/缺省且任一搜索模型鉴权路径有效时，主对话应看到普通 `web_search` 函数，代理上游诊断应显示 `function_web_search=1`、`client_web_search_aliased=true`，明确搜索轮还应显示 `client_web_search_forced=true`；随后搜索模型的专用请求应显示 `client_web_search_prepared=true`。工具结果回填轮的 `client_web_search_forced` 应恢复为 `false`。两种鉴权路径都不可用时两种搜索都没有。Grok 的 hosted 请求还应为 `x_search=1`。真正的 hosted 搜索完成后，诊断中的 `calls`、`completed` 以及 `sources` 或 `annotations` 应为非零。

这里存在三条完全不同的 Web 路径：

- Hosted `web_search` 随 `supports_backend_search = true` 的当前模型请求发给上游，由该提供商执行；hellogrok 会补充并适配这条声明。
- Client `web_search` 在当前模型为 `false`/缺省，且显式 `[models].web_search` 或已鉴权官方默认模型任一有效时作为普通函数出现。明确搜索意图下，hellogrok 在新用户轮选择该函数；Build 随后通过该搜索模型发起独立请求，代理适配这笔请求已有的 hosted 声明、选择 hosted 搜索并要求返回最终文本和来源。普通对话、用户已指定其他工具以及工具结果回填轮不会被强制。
- `web_fetch` 是 Grok Build 本地执行的普通函数工具，只抓取一个已知 URL。hellogrok 会在 Responses、Messages、Chat 三种协议中保留其定义与调用，但实际 HTTP 抓取发生在 Build 本地。

子代理遵循相同分流。Grok Build 会把父会话的客户端搜索采样配置、`web_fetch` 配置和全局搜索禁用标志传给每个子代理；当 `[subagents.models]` 选择另一模型时，再按该模型重新解析 `supports_backend_search`。Build 0.2.118 在只添加 `[subagents.models]` 时会意外把缺省 `enabled` 解析为 `false`，hellogrok 会以事务方式临时修复这个缺省值。`general-purpose`、`explore`、`plan` 的正常能力模式都保留 Web 工具，`read-only` 也不例外。用户明确要求把搜索委派给子代理时，hellogrok 会保留父代理的 `spawn_subagent` 选择，并在子代理请求中选择 `web_search`，不会让父代理抢先搜索；子代理明确只要求 `web_fetch` 时也不会被强制搜索。父子请求中显式的 Responses `allowed_tools` 始终有效。Grok CLI 的 `--tools` 和 `--disable-web-search` 不会把原始意图带到线上：后者会关闭客户端 `web_search` 和本地 `web_fetch`，但 Build 仍可能序列化 xAI 专用 `x_search`；被 CLI 过滤的 hosted 工具也可能只是缺失。hellogrok 因此不能始终区分“CLI 明确排除”和“渠道需要兼容注入”。

Build 界面对 hosted 搜索显示 `Web Search <查询词>`，对本地抓取显示 `Fetch <URL>`。因此 `Web Search` 后面出现 URL，只表示模型把 URL 当成搜索查询，不代表它调用了 `web_fetch`。

Grok Console Free 也不是替换中转 URL 就能直连。当前 grok2api 会[获取与密钥绑定的 DPoP token，并为每次请求重新签名](https://github.com/chenyme/grok2api/blob/56cfd210ce33169997b709ec7529b90c708f919b/backend/internal/infra/provider/console/dpop.go)，同时携带 [SSO/Cloudflare 身份和 `x-cluster` 等 Console 专用请求头](https://github.com/chenyme/grok2api/blob/56cfd210ce33169997b709ec7529b90c708f919b/backend/internal/infra/provider/console/headers.go)。中转对外发放的普通 API key 不是这份内部凭据。hellogrok 会保留文档规定的标准 `web_search` 请求，但不会猜测私有端点、伪造 DPoP 身份，也不会把 `grok-4.5` 静默改成 `Console/grok-4.5`；只有中转公开列出或文档明确说明时才应使用带提供商前缀的模型 ID。

鉴权解析遵循 Build 的渠道所有权：非空模型 `api_key`、首个已解析的模型 `env_key`，然后才是继承的 provider 凭据。模型显式鉴权会遮蔽 provider 鉴权。只有有效且含 `command` 的 `[auth_provider.*]` 或 `[model_providers.*.auth]` 才能拥有由 `auth_scheme` 决定的 Bearer 或 `X-Api-Key`；未定义、空或格式错误的 helper 一律失败关闭。代理分别识别 Build 入站 scheme 与上游 scheme，因此 Anthropic 风格 Messages 渠道可安全转换为 `X-Api-Key`。静态 key 和鉴权请求头始终优先。没有自定义 URL 的内置模型继续走 Build 登录/OAuth；自定义 Grok 渠道不会仅因 wire model 也以 `grok-` 开头就收到登录 token。

hellogrok 不会添加 `stream_tool_calls = false`。用户原有值会原样恢复；Build 到代理这一跳始终使用 Responses，因此无需把这个 Chat 专用规避项全局写入配置。

## 测试

```bash
go test ./... -count=1
```

CI 会在 Windows、Linux、macOS 上执行测试和默认 `CGO_ENABLED=0` 构建，并在 Linux/macOS 原生编译可选托盘目标。标签发布会交叉构建：

- Windows amd64/arm64 GUI 和控制台二进制
- Linux amd64/arm64 CLI 二进制
- macOS amd64/arm64 CLI 二进制

Windows 用户配置真实渠道后，还可以执行：

```powershell
.\scripts\run_grok_all_channels_test.ps1
.\scripts\run_grok_all_channels_test.ps1 -RequireWebSearch -MaxTurns 1 -TimeoutSeconds 150
.\scripts\run_grok_all_channels_test.ps1 -RequireSubagentSearch -MaxTurns 4 -TimeoutSeconds 240
.\scripts\run_grok_all_channels_test.ps1 -Models gpt-congee -RequireSubagentSearch -ExpectedSubagentModel grok-llmx -MaxTurns 4 -TimeoutSeconds 240
.\scripts\run_grok_all_channels_test.ps1 -RequireWebFetch -MaxTurns 2 -TimeoutSeconds 150
```

冒烟脚本会隔离 `HOME`、禁用 AnySearch，并在启用无交互权限模式前按探针应用最小工具白名单：普通回复没有有效工具，直接探针只暴露 `web_search` 或 `web_fetch`，子代理探针只暴露 `Agent` 与 `web_search`。默认普通探针还会核对回复必须严格等于 `OK`，不再把退出码为零误判为成功。Web 搜索探针会按所选模型的有效能力接受 hosted 或客户端函数路径，并读取该渠道的第一笔代理请求，因此 `[models].web_search` 后续发出的请求不会覆盖主对话模式判断。子代理探针还要求观察到真实的 `spawn_subagent` 调用和结果，确认父请求没有抢先执行被委派的搜索，并要求后续子请求选择客户端搜索，或完成一次带来源证据的 hosted 搜索。跨模型探针必须先在真实 `GROK_HOME` 中配置 `general-purpose`；Build 会有意排除项目层 `[subagents.models]`，不让它参与免信任的模型解析。`-ExpectedSubagentModel` 只校验代理日志里实际观察到的子渠道。

## 故障排查

### 普通提示词返回上游 502

先在代理日志中检查上游状态。如果上游成功而 Build 报 502，再查看 schema probe 是否报告 Responses 缺失字段。增量响应补丁只在回发 Build 时执行，绝不会发回上游中转。

### Build 本地工具列表中看不到 Web 搜索

`supports_backend_search = true` 时不出现在该列表属于正常情况，因为 hosted tools 由上游执行；应改看代理请求行和后续 `search evidence`。设为 `false`/缺省时，只要 Build 能鉴权显式 `[models].web_search` 或编译时官方默认模型，普通函数 `web_search` 就会出现；两种路径都无凭据时不显示才是预期状态。工具可见不等于当前模型一定会选它；明确搜索提示应在日志中产生 `client_web_search_forced=true`。`web_fetch` 独立于二者，代理启用其 feature gate 后应始终可见。

### Grok 中转只在正文里声称搜索，没有工具调用

先确认请求日志同时出现 `hosted_web_search=1`、`x_search=1`、`function_web_search=0`。如果连续强制搜索时，有些请求由 `*-build` 模型返回真实 `web_search_call`，另一些却由普通模型只返回叙述文本，说明中转把具备 Build 搜索能力和不具备该能力的账号或 provider 路由混在同一个公开模型 ID 下。中转维护者需要让 Free Build OAuth 账号走 CLI 上游，完整保留两个 hosted 声明及其结果事件，把稳定会话固定到同一个可搜索账号，并以真实完成调用而非 HTTP 200 做健康检查。下游客户端无法修复中转内部已经选中的节点，也不能拿中转公开 API key 去认证私有上游。

### Grok Build 报告 Responses 字段缺失

确认代理运行期间该模型的 `base_url` 指向 `127.0.0.1:18787`。仍为直连 URL 通常说明模型在启动后新增或代理没有运行；停止并重新启动代理。

### 上次进程被强制结束，配置仍指向 localhost

```bash
./dist/hellogrok restore
```

### 18787 端口已占用

启动新实例前先停止现有 hellogrok。不要让两个实例同时操作同一份 Grok 配置。

### Linux 开机启动无法连接用户服务管理器

当前会话没有 systemd 用户管理器。直接运行 `hellogrok start`，或使用该发行版采用的进程管理器。

## 限制

- `supports_backend_search = true` 的提供商和模型必须在已配置端点实现 hosted search；字段适配无法创造提供商侧不存在的搜索服务，中转若主动丢弃工具或搜索结果事件也无法在下游补回。
- `false`/缺省路由依赖 Grok Build 能鉴权显式 `[models].web_search` 或其自身编译时官方默认模型；hellogrok 不会虚构或替换任何搜索模型，也不会静默切换到 hosted 搜索。搜索定向只识别明确的中英文搜索、联网和时效性意图，其余提示仍由当前模型自行选择工具。
- 中转账号池不能在同一个模型 ID 下混用 Build 搜索节点和非 Build 节点。hellogrok 可以发送正确方言，但无法选择或替换中转内部隐藏的账号。
- Chat Completions 没有统一的跨提供商搜索字段。hellogrok 覆盖旧版 xAI `search_parameters` 和 OpenAI 兼容 `web_search_options`；采用其他扩展的提供商仍需新增明确适配器。[xAI 当前文档](https://docs.x.ai/developers/model-capabilities/text/comparison)把原生 Web/X agentic search 放在 Responses，并把 Chat Completions 描述为仅支持 function calling；因此 Grok Chat 渠道只有在中转仍实现旧扩展时才能搜索，上游提供 Responses 时应直接配置为 Responses。
- Messages 和 Chat 上游请求有意使用非流式模式，以便代理生成一条完整有效的规范 Responses 事件序列；内容和工具语义会保留，但不能缩短首 token 等待时间。
- 对 `supports_backend_search = true` 路由，仅用 `--disable-web-search` 或 CLI `--tools` 不能可靠关闭 hosted search：Build 仍可能发出 `x_search`，或在不说明原因的情况下省略 hosted 声明，代理无法从线上请求反推出缺失的 CLI 意图。需要保证完全禁用时，应把能力设为 `false`，并用会话工具白名单排除客户端函数；显式线协议 Responses `allowed_tools` 始终会被遵守。
- Grok Build 可能通过当前自定义路由发送辅助请求。hellogrok 会把该路由上的每个请求固定为渠道配置的 wire model，避免跨模型泄漏；即便主对话成功，部分提供商仍可能拒绝过大或不支持的辅助载荷。
- 上游模型可用性、限流、错误 HTML 和网关 5xx 仍由上游负责。
- 可选 Unix 托盘依赖桌面环境；官方 Unix CLI 不受此限制。
- Windows 和 macOS 发布产物目前未签名。

## 许可证

采用 [MIT License](./LICENSE) 授权。
