<p align="center">
  <img src="./assets/hellogrok.png" alt="hellogrok icon" width="128">
</p>

# hellogrok

A cross-platform local proxy that makes Grok Build custom model channels work with common API formats, native Web tools, and isolated channel credentials.

Current version: **0.1.0**

[English](./README.md) · [简体中文](./README_CN.md)

## What hellogrok does

hellogrok connects Grok Build to custom model channels that use OpenAI Responses, OpenAI Chat Completions, or Anthropic Messages APIs. It keeps Grok Build's native `web_search` and `web_fetch` workflows available where the selected search mode supports them.

While the proxy is enabled, hellogrok checks and temporarily prepares the required Grok configuration, routes each custom channel through its matching local endpoint, and keeps channel credentials separate from an official Grok login. Stopping the proxy restores the original configuration.

Official Grok models without a custom URL continue to use Grok Build's native login and network path.

## Features

- **Custom channel support** — works with `responses`, `chat_completions`, and `messages` backends.
- **Grok Build Web tools** — supports native `web_search` and independent `web_fetch` workflows.
- **Flexible search selection** — uses either the current channel's hosted search or a Grok Build client search model.
- **Authentication isolation** — prevents an official Grok login token from replacing a custom channel's configured credentials.
- **Response compatibility** — normalizes supported upstream responses and streams for Grok Build.
- **Automatic configuration checks** — prepares required settings when the proxy starts and validates them before use.
- **Exact configuration restoration** — restores user-owned values when the proxy stops; the `restore` command recovers from an unclean exit.
- **Model hot switching** — prepares all explicit custom channels before use, so `/model` switching does not require manual URL edits.
- **Subagent support** — applies the same channel and Web-tool behavior to supported Grok Build subagents.
- **Remembered tray state** — remembers whether the proxy was enabled and restores that choice on the next tray launch.
- **Status and logs** — provides tray status, a live log window, route inspection, and recovery commands.
- **Login autostart** — supports Windows, Linux, and macOS user-session startup.

hellogrok is a Grok Build channel proxy. It is not a system proxy, PAC service, VPN, or general HTTPS interceptor.

## Search modes

Search behavior follows the selected custom model's `supports_backend_search` value and Grok Build's configured search model:

| Configuration | Result |
|---------------|--------|
| `supports_backend_search = true` | The selected channel uses its own hosted Web search when the upstream supports it. |
| `false` or omitted, with `[models].web_search` configured | Grok Build exposes client `web_search` and uses that configured model to execute searches. |
| `false` or omitted, without `[models].web_search`, with a valid official xAI login or API credential | Grok Build can use its official default search model. |
| No usable hosted or client search path | `web_search` is unavailable for that model. |
| `web_fetch` | Remains an independent Grok Build tool, subject to the active tool permissions. |

hellogrok never creates, selects, or replaces `[models].web_search`. It uses the model chosen in the user's Grok configuration.

Example custom channel using a configured client search model:

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

Supported `api_backend` values are `responses`, `chat_completions`, and `messages`. If omitted, Grok Build-compatible configuration defaults to `chat_completions`.

## Platform support

| Platform | Interface | Architectures |
|----------|-----------|---------------|
| Windows | Native tray application and CLI | amd64, arm64 |
| Linux | CLI; optional source-built tray | amd64, arm64 |
| macOS | CLI; optional source-built tray | amd64, arm64 |

The standard Linux and macOS builds do not require CGO. The optional tray build uses the `tray` build tag and requires the platform's desktop development libraries.

## Quick start

### Prerequisites

- Grok Build with a readable `~/.grok/config.toml` containing at least one custom model URL.
- A configured `api_key`, `env_key`, or supported authentication provider for every custom channel.
- Go **1.26.5** when building from source.

Set `GROK_HOME` to use a Grok configuration directory other than `~/.grok`.

### Windows

```powershell
git clone https://github.com/hellowind777/hellogrok.git
cd hellogrok
.\scripts\build.ps1
.\dist\hellogrok-cli.exe routes
.\dist\hellogrok.exe
```

Use the tray menu to enable the proxy, configure login autostart, or open status and logs.

### Linux or macOS

```bash
git clone https://github.com/hellowind777/hellogrok.git
cd hellogrok
mkdir -p dist
CGO_ENABLED=0 go build -trimpath -o dist/hellogrok ./cmd/hellogrok
./dist/hellogrok routes
./dist/hellogrok start
```

Keep the foreground process running while using Grok Build. Stop it with Ctrl+C or SIGTERM so the configuration is restored cleanly.

Optional Linux/macOS tray build:

```bash
CGO_ENABLED=1 go build -trimpath -tags tray -o dist/hellogrok-tray ./cmd/hellogrok
```

Linux requires GTK 3 and AppIndicator development packages. macOS requires Xcode Command Line Tools.

## CLI

| Command | Purpose |
|---------|---------|
| `hellogrok start` | Run the proxy in the foreground. |
| `hellogrok version` | Print the installed version. |
| `hellogrok routes` | List configured custom routes without printing credentials. |
| `hellogrok restore` | Restore proxy-managed settings after an unclean exit. |
| `hellogrok autostart enable` | Enable login autostart for the current executable. |
| `hellogrok autostart disable` | Disable login autostart. |
| `hellogrok autostart status` | Show the current autostart state. |
| `hellogrok log` | Print and open the log file. |
| `hellogrok logview` | Follow the log in the current terminal. |
| `hellogrok help` | Show command help. |

Runtime data is stored in `%LOCALAPPDATA%\hellogrok` on Windows and `~/.hellogrok` on Linux and macOS.

## Usage notes

- Start a new Grok Build process after enabling the proxy so it reloads the prepared model configuration.
- Keep hellogrok running for the entire Grok Build session.
- Stop the proxy normally before moving or replacing the executable.
- After a forced termination, ensure no proxy process is running, then execute `hellogrok restore`.
- Environment variables used by `env_key`, `env_http_headers`, or `GROK_HOME` must also be available to login-started processes.
- A custom provider must actually support the API and search capability it advertises; hellogrok cannot add a provider-side search service that does not exist.

## License

Licensed under the [MIT License](./LICENSE).
