<p align="center">
  <img src="./assets/hellogrok.png" alt="hellogrok icon" width="128">
</p>

# hellogrok

A cross-platform local proxy that makes Grok Build custom model channels work with common API formats, native Web tools, isolated authentication, and automatic configuration recovery.

[![Version](https://img.shields.io/badge/version-0.1.1-2f6feb.svg)](./internal/appinfo/appinfo.go)
[![Go](https://img.shields.io/badge/Go-1.26.5-00ADD8.svg)](./go.mod)
[![License: MIT](https://img.shields.io/badge/license-MIT-green.svg)](./LICENSE)
[![Platforms](https://img.shields.io/badge/platform-Windows%20%7C%20Linux%20%7C%20macOS-lightgrey.svg)](#platform-support)

[English](./README.md) · [简体中文](./README_CN.md) · [Changelog](./CHANGELOG.md)

## Contents

- [Why hellogrok](#why-hellogrok)
- [Features](#features)
- [Search and configuration](#search-and-configuration)
- [Quick start](#quick-start)
- [Platform support](#platform-support)
- [Tray and CLI](#tray-and-cli)
- [Autostart](#autostart)
- [How it works](#how-it-works)
- [Troubleshooting](#troubleshooting)
- [Development](#development)
- [Limitations](#limitations)
- [Contributing](#contributing)
- [License](#license)

## Why hellogrok

Grok Build can use custom model endpoints, but real-world providers do not all expose the same protocol, response shape, authentication method, or Web-search behavior. A channel that works with `curl` can still fail in a normal Grok Build conversation, lose native Web tools, or receive the wrong login credential.

hellogrok provides one local compatibility layer for those custom channels. It prepares the required Grok configuration while running, keeps each channel tied to its own endpoint and credentials, supports Grok Build's native Web workflows, and restores the original configuration when stopped.

It is intended for users who maintain multiple third-party model channels and want to switch between them from Grok Build without manually rewriting URLs or changing tool configuration for every session.

## Features

[Full Changelog](./CHANGELOG.md)

### Channel compatibility

- Supports upstream `responses`, `chat_completions`, and Anthropic-compatible `messages` APIs.
- Normalizes supported responses and streams into a form Grok Build can consume.
- Preserves each configured upstream URL path and model identifier.
- Prepares every explicit custom channel before use, avoiding first-request failures after `/model` switching.

### Native Web tools

- Supports Grok Build's native `web_search` workflow for hosted and client-search modes.
- Keeps `web_fetch` available as an independent tool when allowed by the active agent configuration.
- Applies the same search behavior to supported subagents.
- Keeps official Grok models on Grok Build's native search and login path.

### Authentication and configuration safety

- Uses channel-owned API keys, environment keys, authentication providers, and headers.
- Prevents an official Grok login token from being sent to an unrelated custom channel.
- Checks and temporarily completes required Grok settings when the proxy starts.
- Restores original values on normal stop, tray exit, Ctrl+C, SIGTERM, or failed startup.
- Recovers proxy-managed settings after an unclean exit with `hellogrok restore`.

### Desktop and operations

- Provides a native Windows tray application and a console CLI.
- Remembers the user's proxy-enabled choice between tray launches.
- Defaults to proxy-enabled on first launch so new users see a working proxy immediately.
- Includes login autostart controls for Windows, Linux, and macOS.
- Provides route inspection, current status, a live log window, and terminal log following.
- Builds for Windows, Linux, and macOS on amd64 and arm64.

hellogrok is a Grok Build channel proxy. It is not a system proxy, PAC service, VPN, or general HTTPS interceptor.

## Search and configuration

### Search modes

Search behavior follows the explicit search-model selection first, then the selected custom channel's `supports_backend_search` setting:

| Setting | Search behavior |
|---------|-----------------|
| `[models].web_search` or `GROK_WEB_SEARCH_MODEL` is set | All custom conversation channels use Grok Build client `web_search` through the selected search model. The environment variable takes precedence over the config file value. |
| No search model, `supports_backend_search = true` | The selected channel uses its own hosted Web search when its upstream endpoint supports it. |
| No search model, `supports_backend_search = false` | Grok Build uses its official client-search fallback when valid xAI credentials are available. |
| No search model, setting omitted on a Grok relay | hellogrok auto-detects the relay's hosted search capability at startup; confirmed hosted search uses the relay, otherwise Grok Build keeps its official client-search fallback. |
| No search model, setting omitted on another custom channel | Grok Build uses its official client-search fallback when available. |
| No usable hosted or client search path | `web_search` is unavailable for that model. |
| `web_fetch` | Remains independent of the search-model selection and follows the active tool permissions. |

hellogrok never creates, selects, or replaces `[models].web_search`. Explicit `true` and `false` channel settings remain authoritative when no client search model is selected.

### Example configuration

This example uses an existing custom channel as Grok Build's client search model:

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

Set `supports_backend_search = true` instead when that channel should execute its own hosted search and the configured upstream endpoint actually supports it.

### Supported channel settings

| Setting | Required | Default | Purpose |
|---------|----------|---------|---------|
| `model` | No | Model table ID | Model identifier sent to the upstream channel. |
| `base_url` or `api_base_url` | Yes | None | Custom upstream endpoint. Models without a custom URL are not proxied. |
| `api_backend` | No | `chat_completions` | Upstream API format: `responses`, `chat_completions`, or `messages`. |
| `api_key` | One auth method | None | Static channel credential. Prefer `env_key` for shared configurations. |
| `env_key` | One auth method | None | Environment variable name or ordered list of names containing the channel credential. |
| `auth_provider` | One auth method | None | Grok command-based authentication provider. |
| `auth_scheme` | No | Backend-dependent | Upstream authentication scheme, including Bearer and `X-Api-Key` styles. |
| `extra_headers` | No | Empty | Additional channel-owned HTTP headers. |
| `env_http_headers` | No | Empty | HTTP headers populated from environment variables. |
| `supports_backend_search` | No | Automatic | Selects hosted search (`true`) or Grok Build client search (`false`); an omitted value lets hellogrok check Grok relays at startup. |

Model settings may be declared directly under `[model.<id>]` or inherited from a referenced `[model_providers.<id>]`. Model-level values take precedence.

Do not manually set a custom channel URL to hellogrok's local address. The application manages temporary local URLs only while the proxy is active.

## Quick start

### Prerequisites

- Grok Build with a readable `~/.grok/config.toml` containing at least one custom model URL.
- A valid credential source for every custom channel.
- Go **1.26.5** when building from source.

Grok Build **0.2.118** is the current verified baseline. Newer versions should be checked with the included smoke tests because Grok Build's custom-model behavior may evolve.

Set `GROK_HOME` to use a Grok configuration directory other than `~/.grok`.

### Windows

```powershell
git clone https://github.com/hellowind777/hellogrok.git
cd hellogrok
.\scripts\build.ps1
.\dist\hellogrok-cli.exe routes
.\dist\hellogrok.exe
```

Use the tray menu to select **Start proxy**. Then start a new Grok Build process so it reloads the prepared model configuration.

### Linux or macOS

```bash
git clone https://github.com/hellowind777/hellogrok.git
cd hellogrok
mkdir -p dist
CGO_ENABLED=0 go build -trimpath -o dist/hellogrok ./cmd/hellogrok
./dist/hellogrok routes
./dist/hellogrok start
```

Expected startup output includes a local channel endpoint and a successful configuration rewrite. Keep the process running while using Grok Build. Ctrl+C or SIGTERM stops the proxy and restores the original configuration.

### First-use checklist

1. Run `hellogrok routes` and confirm every intended custom model is listed with the correct backend and an available authentication source.
2. Start hellogrok before opening a new Grok Build process.
3. Switch models with `/model` and test a normal conversation.
4. Test `web_search` and `web_fetch` separately according to the selected search mode.
5. Stop hellogrok normally and confirm Grok Build's configuration no longer points to the local proxy.

## Platform support

| Platform | Standard interface | Tagged release artifacts | Architectures |
|----------|--------------------|--------------------------|---------------|
| Windows | Native tray and CLI | GUI and console `.exe` files | amd64, arm64 |
| Linux | Foreground CLI or systemd user service | CLI binary | amd64, arm64 |
| macOS | Foreground CLI or LaunchAgent | CLI binary | amd64, arm64 |

Standard release binaries use `CGO_ENABLED=0`. Tagged releases are configured to include SHA-256 checksum files.

Linux and macOS users can build the optional tray interface from source:

```bash
CGO_ENABLED=1 go build -trimpath -tags tray -o dist/hellogrok-tray ./cmd/hellogrok
```

Linux tray builds require GTK 3 and AppIndicator development packages. macOS tray builds require Xcode Command Line Tools. The standard Unix CLI does not require these desktop dependencies.

Current Windows and macOS artifacts are not code-signed or notarized.

## Tray and CLI

### Tray controls

The Windows tray application and optional Unix tray build provide:

- **Start proxy** — enabled by default on first launch; later starts and stops remember the selected state.
- **Autostart** — enables or disables login startup.
- **Status and logs** — opens the current status and live log window.
- **Exit** — restores the configuration, stops the proxy, and exits. Defers when a config-ownership conflict exists.

Only one tray instance runs in a login session; launching it again exits immediately instead of creating a second tray. The remembered tray state is independent from the foreground `hellogrok start` command.

**Quit protection**: When a provider manager still owns Grok Build's configuration, the tray defers exit to avoid leaving an orphaned proxy route — resolve the configuration conflict first, then quit.

### Compatibility with CC Switch

CC Switch and hellogrok can run at the same time only when CC Switch is not managing Grok Build. CC Switch's Grok Build proxy takeover and provider switch both write `~/.grok/config.toml`; using either operation while hellogrok owns that file creates a configuration-ownership conflict even though the proxies listen on different ports.

- hellogrok refuses to start when it detects CC Switch's Grok Build takeover marker (`PROXY_MANAGED` on its `/grokbuild/v1` route).
- If CC Switch takeover is enabled after hellogrok starts, hellogrok refuses to stop or exit until CC Switch releases Grok Build. This keeps CC Switch from later restoring a stopped `127.0.0.1:18787` route.
- If a provider manager completely replaces the live Grok config and no hellogrok route remains, hellogrok preserves the external config and relinquishes its obsolete recovery state.
- CC Switch may continue managing Claude, Codex, Gemini, and other applications while hellogrok is active.

If both Grok proxies were enabled accidentally, disable CC Switch's Grok Build takeover first, then stop hellogrok. Avoid switching the CC Switch Grok Build provider while hellogrok is active.

### CLI reference

| Command | Purpose |
|---------|---------|
| `hellogrok start` | Run the proxy in the foreground. |
| `hellogrok version` | Print the installed version. |
| `hellogrok routes` | List custom routes without printing credentials. |
| `hellogrok restore` | Restore proxy-managed settings after an unclean exit. |
| `hellogrok autostart enable` | Enable login autostart for the current executable. |
| `hellogrok autostart disable` | Disable login autostart. |
| `hellogrok autostart status` | Show the current autostart state. |
| `hellogrok log` | Print and open the log file. |
| `hellogrok logview` | Follow the log in the current terminal. |
| `hellogrok help` | Show command help. |

### Runtime data

| Platform | Location |
|----------|----------|
| Windows | `%LOCALAPPDATA%\hellogrok` |
| Linux and macOS | `~/.hellogrok` |

Runtime data contains application preferences, logs, and the recovery state used to restore managed configuration.

## Autostart

### Windows

Enable **Autostart** from the tray or run `hellogrok autostart enable`. Login startup opens the tray and applies the remembered proxy-enabled state.

### Linux

The standard CLI registers a systemd user service. Enable it and start it immediately with:

```bash
./dist/hellogrok autostart enable
systemctl --user start hellogrok.service
systemctl --user status hellogrok.service
```

### macOS

The standard CLI registers a per-user LaunchAgent. Enable it and load it immediately with:

```bash
./dist/hellogrok autostart enable
launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/com.hellogrok.proxy.plist"
```

Autostart records the current executable's absolute path. Disable and re-enable it after moving the binary. Credentials referenced by `env_key`, `env_http_headers`, or `GROK_HOME` must be available in the login-started process environment, not only in the current shell.

## How it works

```text
Grok Build
    |
    v
hellogrok local channel proxy
    |
    v
Configured custom API channel
```

At startup, hellogrok validates custom channels and temporarily points them to the local proxy. During a session it routes each request to the channel's configured API, credentials, model, and search mode. When stopped, it restores the original Grok configuration.

Native `web_search`, `web_fetch`, official Grok login behavior, and supported subagent workflows remain controlled through Grok Build rather than being replaced by a separate search service.

## Troubleshooting

### No custom routes are found

Confirm that the intended `[model.<id>]` or referenced provider has a valid `base_url` or `api_base_url`. Official models without a custom URL are intentionally excluded.

### `web_search` is unavailable

Check the startup log for the selected search route. A hosted channel needs real upstream search evidence. A client-search route needs either a valid `[models].web_search` model or usable official xAI credentials. `web_fetch` is independent but can still be removed by the active tool permissions.

### A request returns 401, 403, or 502

Run `hellogrok routes` and inspect **Status and logs**. Confirm the channel URL, backend, credential source, model identifier, and provider availability. An upstream outage, rate limit, unsupported payload, or stripped search tool must be fixed by the provider or relay.

### The configuration still points to localhost after a forced exit

Ensure no hellogrok process is running, then execute `hellogrok restore`. Do not run `restore` against an active proxy.

### Port `18787` is already in use

Stop the existing hellogrok instance before starting another one. Only one instance should manage a Grok configuration directory at a time.

### Autostart works, but a channel has no credentials

Move shell-only environment variables into the persistent user or service environment, then restart the login service. The autostart process cannot inherit variables that existed only in an earlier terminal session.

### Cannot quit while a provider manager owns Grok Build

Open the provider manager (e.g., CC Switch) and disable its Grok Build takeover first, then quit hellogrok. This prevents CC Switch from later restoring a route to a stopped proxy.

## Development

Run the local quality checks:

```bash
go test ./... -count=1
go vet ./...
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
```

Windows users with configured live channels can run the integration smoke tests:

```powershell
.\scripts\run_grok_all_channels_test.ps1
.\scripts\run_grok_all_channels_test.ps1 -RequireWebSearch -MaxTurns 1 -TimeoutSeconds 150
.\scripts\run_grok_all_channels_test.ps1 -RequireSubagentSearch -MaxTurns 4 -TimeoutSeconds 240
.\scripts\run_grok_all_channels_test.ps1 -RequireWebFetch -MaxTurns 2 -TimeoutSeconds 150
```

CI runs tests and default builds on Windows, Linux, Intel macOS, and Apple Silicon macOS. It also builds the optional tray target natively on Linux and macOS. Tagged releases produce amd64 and arm64 artifacts for all three operating systems.

## Limitations

- hellogrok cannot create provider-side search capability. A hosted-search channel must actually support search and return its results.
- A relay that removes tool declarations, tool calls, citations, or result events cannot be fully repaired downstream.
- Provider-specific API extensions outside the supported Responses, Chat Completions, and Messages formats may require additional adaptation.
- Upstream availability, model access, account pools, rate limits, and gateway errors remain the provider's responsibility.
- Optional Unix tray behavior depends on the installed desktop environment; the standard Unix CLI is the portable path.
- Current release artifacts are unsigned. Build from source when local trust requirements demand it.

## Contributing

1. Create a focused branch for the change.
2. Follow the existing package boundaries and avoid unrelated refactors.
3. Add or update tests for behavior changes.
4. Run the quality checks above.
5. Update both README files when user-facing behavior changes.
6. Open a pull request describing the problem, approach, and verification results.

## License

Licensed under the [MIT License](./LICENSE).
