# hellogrok

Cross-platform local reverse proxy that makes Grok Build custom model channels work with strict Responses parsing, isolated channel authentication, and Build-native `web_search`.

Current version: **0.1.0**

[English](./README.md) · [简体中文](./README_CN.md)

## Contents

- [Why hellogrok](#why-hellogrok)
- [Features](#features)
- [Platform support](#platform-support)
- [Quick start](#quick-start)
- [CLI reference](#cli-reference)
- [Autostart](#autostart)
- [How it works](#how-it-works)
- [Configuration behavior](#configuration-behavior)
- [Testing](#testing)
- [Troubleshooting](#troubleshooting)
- [Limits](#limits)

## Why hellogrok

Grok Build accepts OpenAI-compatible custom models, but many gateways implement only part of the Responses schema. A gateway may return valid text while omitting fields such as `annotations`, message IDs, statuses, usage details, or SSE sequence numbers. Grok Build then rejects the response during deserialization.

Search creates a second compatibility problem. Grok Build does not probe whether the selected third-party endpoint supports search. At source revision `a5589e9`, hosted `web_search` exists only when all of these static gates pass: `[features].backend_tools`, the selected model's `supports_backend_search`, the agent's tool allowlist, and a credentialed effective search model. That search model comes from `[models].web_search` when explicitly set, otherwise from Build's compiled default. If it cannot be authenticated, Build can still add xAI-only `x_search` while omitting Web search. Once backend search is active, Build also filters the client-function `web_search` from the normal tool list. Its Responses serializer emits hosted tools, but its Chat Completions and Anthropic Messages serializers currently discard them. Adding Responses SSE fields such as `sequence_number` cannot restore this request-side capability; `sequence_number` only orders response events.

hellogrok repairs both directions at the protocol boundary. While it is running, every explicit custom channel is presented to Build as a per-channel Responses endpoint. The proxy then translates that canonical request back to the channel's original Responses, Chat Completions, or Messages protocol. It temporarily materializes each model's effective `supports_backend_search`: `true` selects that channel's hosted search, while `false` (including an omitted flag) leaves Build's ordinary client `web_search` function intact. An explicit `[models].web_search` model wins; without one, Build can use its official default search model when an xAI login session or API credential is available. On a new user turn that explicitly asks to search, browse, or obtain current information, the proxy selects that function unless the caller already chose another tool. It never creates, selects, or modifies the search model itself.

## Features

- **All-channel routing** — rewrites every explicit custom `base_url` before the first model request.
- **Transactional config and exact restoration** — commits a recovery record before replacing the config, detects the state-commit/config-replace crash window, and read-back validates required settings; tray stop, exit, Ctrl+C, SIGTERM, or a failed startup restores every managed setting, while the next start or `restore` handles a forced termination. A configured `[subagents]` tree with an omitted `enabled` value is temporarily repaired to `true` without overriding an explicit `false`.
- **Remembered tray state** — persists the user's last proxy-enabled choice and restores it automatically on the next tray launch.
- **Three-protocol facade** — exposes one strict Responses contract to Build and translates to the channel's original Responses, Messages, or Chat Completions endpoint.
- **Responses compatibility** — additively fills required response and SSE fields while preserving existing upstream values.
- **Stream-shape compatibility** — turns a complete JSON Responses body into valid Responses SSE when a gateway ignores `stream = true`; malformed, oversized, or prematurely ended upstream streams produce a canonical `event: error` instead of silent truncation.
- **Authentication isolation** — configured channel keys replace an unrelated Grok OAuth token before a third-party request.
- **Capability-aware search routing** — `supports_backend_search = true` uses the selected channel's hosted search; `false` or omitted preserves Build's client `web_search` path through an explicit `[models].web_search` or Build's authenticated official default, and prefers it over `web_fetch` for explicit search intent.
- **Grok Build search dialect** — normalizes hosted requests to one `web_search` plus one `x_search` on Grok relays, while client-search result turns return to automatic selection to avoid repeated calls.
- **Tool-choice preservation** — enforces Responses `allowed_tools` while converting to Messages or Chat and rejects malformed Messages tool arguments instead of silently changing them.
- **Search evidence diagnostics** — distinguishes a declared tool from an executed call and returned sources without logging prompts, queries, URLs, or credentials.
- **Cross-platform core** — Windows, Linux, and macOS release builds use `CGO_ENABLED=0`.
- **Optional Unix tray** — Linux/macOS users can compile a CGO tray build with the `tray` tag.

hellogrok is a path-routing proxy, not a system proxy, PAC, or general HTTPS interception tool.

## Platform support

| Platform | Official binary | Default interface | Architectures |
|----------|-----------------|-------------------|---------------|
| Windows | GUI and console binaries | Tray UI; CLI subcommands are also available | amd64, arm64 |
| Linux | Static-style Go CLI without GUI/CGO | Foreground CLI or systemd user service | amd64, arm64 |
| macOS | Go CLI without GUI/CGO | Foreground CLI or LaunchAgent | amd64, arm64 |

The optional Linux/macOS tray build is source-only. It is not required for proxy operation.

Tagged releases include SHA-256 checksum files. Release binaries are not code-signed or notarized; users who require a trusted local build should compile from source. Downloaded Linux/macOS binaries may need `chmod +x <downloaded-file>` before execution.

## Quick start

### Prerequisites

- Grok Build 0.2.118, with a readable `~/.grok/config.toml`, is the verified baseline. Newer builds must pass the included channel/search smoke tests because their private client-search request shape may change; older versions are unsupported.
- A channel `api_key` or `env_key` for each third-party endpoint.
- For `supports_backend_search = true`, an upstream that implements hosted Web search on its configured API protocol. For `false` or omitted, client search requires either a valid credentialed `[models].web_search` model or credentials for Build's official default search model.
- Go 1.26.5 when compiling from source.

### Build on Windows

```powershell
git clone https://github.com/hellowind777/hellogrok.git
cd hellogrok
.\scripts\build.ps1
```

Run `dist\hellogrok.exe` for the tray, or use `dist\hellogrok-cli.exe` from a terminal.

### Build on Linux or macOS

```bash
git clone https://github.com/hellowind777/hellogrok.git
cd hellogrok
mkdir -p dist
CGO_ENABLED=0 go build -trimpath -o dist/hellogrok ./cmd/hellogrok
./dist/hellogrok routes
./dist/hellogrok start
```

Expected startup signals include:

```text
channel facade on http://127.0.0.1:18787/c/<channel>/responses
config rewrite all: ...
```

Keep the process running while using Grok Build. Ctrl+C or SIGTERM stops the proxy and restores the Grok configuration.

### Optional Linux/macOS tray

Linux requires GTK and AppIndicator development packages:

```bash
# Debian / Ubuntu
sudo apt-get install gcc libgtk-3-dev libayatana-appindicator3-dev
CGO_ENABLED=1 go build -trimpath -tags tray -o dist/hellogrok-tray ./cmd/hellogrok
```

On macOS, install Xcode Command Line Tools, then build:

```bash
xcode-select --install
CGO_ENABLED=1 go build -trimpath -tags tray -o dist/hellogrok-tray ./cmd/hellogrok
```

A distributed macOS tray application should additionally be wrapped, signed, and notarized as an `.app`. The official macOS artifact stays CLI-only so it has no CGO or desktop-framework dependency.

## CLI reference

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

| Command | Behavior |
|---------|----------|
| `start` | Runs the proxy in the foreground. Ctrl+C/SIGTERM performs a clean restore. |
| `version` | Prints the installed hellogrok version. |
| `restore` | Restores proxy-managed configuration after an unclean exit. Do not run it against an active proxy. |
| `routes` | Lists route hosts and model IDs without printing credentials. |
| `autostart ...` | Manages login autostart for the current executable. |
| `log` | Prints the log path and opens it with the platform default application. |
| `logview` | Follows the log in the current terminal. |

Data locations:

| Platform | Runtime data |
|----------|--------------|
| Windows | `%LOCALAPPDATA%\hellogrok` |
| Linux/macOS | `~/.hellogrok` |

Set `GROK_HOME` to override the default Grok directory (`~/.grok`).

The tray's **Start proxy** choice is stored in `settings.json` under that data directory. A successful manual start remembers enabled; a manual stop remembers disabled. Exiting hellogrok still restores Grok's config but does not clear that intent, so the next tray launch starts the proxy again automatically. The foreground `hellogrok start` command does not alter this tray preference.

## Autostart

### Windows

Use the tray checkbox or run `hellogrok autostart enable`. This registers the current executable in Microsoft's documented per-user [`Run` logon key](https://learn.microsoft.com/en-us/windows/win32/setupapi/run-and-runonce-registry-keys). At login it opens the tray and applies the remembered **Start proxy** state. Windows may delay Run entries, so startup is not guaranteed to be immediate after login. Status also verifies that the registered target still exists; re-enable autostart after moving the binary.

### Linux

For the headless distribution, `autostart enable` writes and enables `~/.config/systemd/user/hellogrok.service` with `hellogrok start`, so the proxy runs directly after login. A source-built `tray` binary instead registers a no-argument launch, opens the tray, and applies the remembered proxy state. Status requires an enabled unit whose target exists and is executable. The unit records the executable's absolute path, so disable and re-enable autostart after moving it. The systemd documentation states that [`enable` creates startup dependencies but does not start the unit immediately](https://github.com/systemd/systemd/blob/main/man/systemctl.xml), so run `systemctl --user start` below once or log in again.

```bash
./dist/hellogrok autostart enable
systemctl --user start hellogrok.service
systemctl --user status hellogrok.service
```

### macOS

For the headless distribution, `autostart enable` writes `~/Library/LaunchAgents/com.hellogrok.proxy.plist` with `hellogrok start`. Apple documents that the per-user launchd reads `~/Library/LaunchAgents` at login, sends agents `SIGTERM` at logout, and takes the executable plus arguments from [`ProgramArguments`](https://developer.apple.com/library/archive/documentation/MacOSX/Conceptual/BPSystemStartup/Chapters/CreatingLaunchdJobs.html). A source-built `tray` binary registers an interactive no-argument launch and applies the remembered proxy state; a LaunchAgent can present UI, but Apple recommends a Login Item for a production `.app`, so this source-only tray path still requires testing on the target macOS release. Enabling clears launchd's persisted disabled state; status validates the plist, target executable, and launchd disabled state. It loads on the next login. To load it immediately:

```bash
./dist/hellogrok autostart enable
launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/com.hellogrok.proxy.plist"
```

Disabling autostart prevents future launches and removes the plist. It deliberately does not terminate a currently running LaunchAgent; stop that process separately when immediate shutdown is required.

The Windows registry path is covered by a live enable/status/disable round trip on Windows. Linux uses a standard systemd user service and macOS uses a standard LaunchAgent; generated configuration, escaping, arguments, and amd64/arm64 builds are tested, but each release should still receive one login-session integration test on the corresponding operating system.

A login-launched process does not inherit variables set only in the current shell. If the configuration depends on `GROK_HOME`, model `env_key`, or `env_http_headers`, define them in the operating system's persistent user environment or in the corresponding systemd/LaunchAgent environment configuration. Otherwise, a channel that works when started manually may lack its configuration or credentials after login.

## How it works

```text
Grok Build
  -> Responses: http://127.0.0.1:18787/c/<channel>/responses
  -> hosted search OR client web_search function + independent web_fetch
  -> channel model/auth isolation
  -> original Responses | Messages | Chat Completions endpoint
  <- canonical Responses response/SSE + additive schema fill
  <- Grok Build
```

The original route is held in memory before the config is rewritten. The local URL contains only an escaped channel ID, never an upstream hostname or credential:

| Origin `base_url` | Temporary proxy URL |
|-------------------|---------------------|
| `https://congee.pro/v1` for model `gpt-main` | `http://127.0.0.1:18787/c/gpt-main` |
| `https://api.deepseek.com/anthropic` for model `deepseek-pro` | `http://127.0.0.1:18787/c/deepseek-pro` |
| `http://localhost:8000/v1` for model `local-chat` | `http://127.0.0.1:18787/c/local-chat` |

Build appends `/responses` to that temporary base. hellogrok appends `/responses`, `/messages`, or `/chat/completions` to the preserved original base according to the channel's original `api_backend`; it does not invent or remove a `/v1` or `/anthropic` prefix.

This facade follows Grok Build's actual serializer boundary, not a guessed configuration convention. At source revision `a5589e9`, the [agent builder ties hosted Web Search to `web_search_config.is_enabled()` while adding X Search independently](https://github.com/xai-org/grok-build/blob/a5589e958437d79e13db026eedcb1720bffd4063/crates/codegen/xai-grok-agent/src/builder.rs#L1191-L1198), and the [per-turn gate both checks `supports_backend_search` and removes the function Web Search](https://github.com/xai-org/grok-build/blob/a5589e958437d79e13db026eedcb1720bffd4063/crates/codegen/xai-grok-shell/src/session/acp_session_impl/sampler_turn.rs#L139-L182). The [Responses converter serializes hosted Web Search and separately supports xAI X Search](https://github.com/xai-org/grok-build/blob/a5589e958437d79e13db026eedcb1720bffd4063/crates/codegen/xai-grok-sampling-types/src/conversation/responses.rs#L315-L380), while the [Chat converter only consumes function tools and leaves `search_parameters` unset](https://github.com/xai-org/grok-build/blob/a5589e958437d79e13db026eedcb1720bffd4063/crates/codegen/xai-grok-sampling-types/src/conversation/chat_completions.rs#L252-L307), and the [Messages converter likewise only consumes function tools](https://github.com/xai-org/grok-build/blob/a5589e958437d79e13db026eedcb1720bffd4063/crates/codegen/xai-grok-sampling-types/src/conversation/messages.rs#L290-L350). Finally, [`web_fetch` has its own feature/env/remote resolver](https://github.com/xai-org/grok-build/blob/a5589e958437d79e13db026eedcb1720bffd4063/crates/codegen/xai-grok-shell/src/agent/config.rs#L2522-L2531); `[models].web_search` cannot enable it.

The independent grok2api implementation confirms the non-obvious Grok CLI route rule: for a stable Build cache session it [adds `x_search` when the request already contains `web_search`](https://github.com/chenyme/grok2api/blob/56cfd210ce33169997b709ec7529b90c708f919b/backend/internal/infra/provider/cli/responses_cache_route.go#L45-L73), explicitly describing that entry as required by official Build behavior. This does not make X Search an alias for Web Search; it means a Grok Build upstream expects the pair for that route. hellogrok therefore preserves one of each only on Grok Responses routes.

Some Grok relays intercept an ordinary function named `web_search` as their own hosted-search declaration, even on a route configured for client search, and then never return the function call Build needs. On `supports_backend_search = false`/omitted routes, hellogrok therefore renames only that ordinary function on the upstream wire to the collision-safe `hellogrok_client_web_search` (with a numeric suffix if the user already owns that name). Tool choices and prior function-call history are mapped consistently, and the upstream response is mapped back to `web_search`. Build, its UI, conversation history, and the configured search model see only the native name; hosted `web_search` and `x_search` declarations are never aliased.

hellogrok does not replace a relay URL with grok2api's private upstream. grok2api uses `https://cli-chat-proxy.grok.com/v1` with [Build OAuth and CLI-specific authentication/session headers](https://github.com/chenyme/grok2api/blob/56cfd210ce33169997b709ec7529b90c708f919b/backend/internal/infra/provider/cli/adapter.go#L803-L854); its Console route requires SSO, DPoP proof signing, browser state, and `x-cluster`. A relay's public API key is not either credential, so changing only the URL would turn a valid relay request into an unauthenticated upstream request.

Typical additive response fills include:

- response `created_at` and `model`
- Responses SSE `sequence_number`
- `output_text.annotations` and `logprobs`
- message/output item `id` and `status`
- `web_search_call.action`, `query`, and `sources`
- input/output token detail objects
- empty chat `finish_reason` normalized to `null`

## Configuration behavior

At startup, hellogrok:

1. Claims `127.0.0.1:18787` before touching config. A second instance fails before it can mistake the active instance's state for an orphan.
2. Recovers managed settings left by an earlier unclean exit.
3. Loads every `[model.*]` entry whose effective model or provider config has an explicit `base_url` or `api_base_url`.
4. Temporarily ensures `[features].backend_tools = true` and `[features].web_fetch = true`. When a `[subagents]` tree exists but omits `enabled`, it also temporarily adds `enabled = true`; Grok Build 0.2.118 otherwise deserializes that partial table with subagents disabled. Explicit `true` and `false` remain user-owned.
5. Rewrites each explicit model's `base_url` (and an effective `api_base_url`) to its own facade URL, ensures `api_backend = "responses"`, and materializes the effective `supports_backend_search` Boolean. A missing model/provider value becomes `false`, preventing a remote catalog capability from hiding client search. Official xAI/Grok entries without a custom URL are untouched. A proxy URL left without a matching recovery record aborts startup instead of silently losing its upstream origin.
6. Parses and validates the prepared TOML before committing it, then reads the file back and validates every required value again. Any failure immediately rolls back and aborts startup.
7. Adds hosted `web_search` only for routes whose effective `supports_backend_search` is `true`. For `false`, ordinary functions remain available; on a new turn with explicit search/current-information intent and automatic tool choice, it clarifies the roles of `web_search` and `web_fetch` and selects `web_search`. It does not force the tool-result turn, preventing a loop. A request that already contains a hosted search declaration is still normalized because it may be the second request made by Build's client Web Search model; for that non-streaming execution request, the proxy selects hosted `web_search` and requires final text with source URLs.
8. Replaces an unrelated OAuth bearer token with the selected channel's configured key, then patches only missing response fields on the way back.

Before rewriting, hellogrok records whether each managed field existed plus its exact original line, comment, and line ending. The record is committed first: if the process dies before the config replacement, the next recovery recognizes that all managed values are still original and discards the unapplied record. A normal stop or failed startup restores `base_url`, `api_base_url`, `api_backend`, `supports_backend_search`, `backend_tools`, `web_fetch`, and a proxy-added `[subagents].enabled`, deleting fields or parent tables that the proxy created. `[models].web_search`, explicit subagent enablement, and unrelated TOML are never changed. After an OS-level forced termination, a persisted recovery record is applied automatically on the next start or explicitly with `hellogrok restore`. Whether the final filesystem writes survive sudden power loss still depends on the durability guarantees of the OS, filesystem, and storage device.

The effective flag may be declared on `[model.<id>]` or inherited from `[model_providers.<id>]`. An explicit model value wins, so model-level `false` overrides provider-level `true`; only when both locations omit the field does hellogrok materialize `false` for the running proxy. A non-Boolean value aborts startup before any config write. The resulting behavior is:

| Effective `supports_backend_search` | Search-model state | Search visible to the current model | Executor |
|-------------------------------------|--------------------|-------------------------------------|----------|
| `true` | any | hosted `web_search` / `x_search` supported by the route | selected channel upstream |
| `false` or omitted | explicit `[models].web_search` resolves with credentials | ordinary function `web_search` | Grok Build client, using the configured model |
| `false` or omitted | no explicit override, official login or xAI API credential available | ordinary function `web_search` | Grok Build client, using its compiled official default |
| `false` or omitted | neither search-model credential path is usable | no `web_search` | none |

An explicit search-model override takes precedence over the compiled default even when the user is logged in. `web_fetch` is independent of all four rows and remains a Build-local function. Built-in official models without a custom URL retain Grok Build's native capability and OAuth handling.

The wire adapter is protocol-specific:

| `api_backend` | Request sent upstream | Response handling |
|---------------|-----------------------|-------------------|
| `responses` | preserves ordinary `web_search`/`web_fetch` functions and can select client `web_search` for explicit search intent; the client function uses its collision-safe name only on the upstream wire; for hosted requests, sends one standard `web_search`, adds one `x_search` only on Grok routes, and removes only colliding search functions | streams the upstream response, restores the client function name first, records actual search evidence, and additively fills only missing strict Responses fields |
| `messages` | converts ordinary functions, the wire alias, and a selected function choice; converts an incoming hosted declaration to `web_search_20250305`, and uses `x-api-key` by default | converts thinking, server search blocks, citations, tool calls, usage, and text into canonical Responses, restores the client function name, and retains exact search blocks in a bounded per-channel replay cache for later turns |
| `chat_completions` | converts ordinary functions, the wire alias, and a selected function choice; maps an incoming hosted declaration to legacy xAI `search_parameters` on Grok routes or OpenAI-compatible `web_search_options` otherwise | converts the non-stream Chat response into canonical Responses and restores the client function name; emits a `web_search_call` only when citations, search results, or a positive search-usage counter prove that search ran |

For DeepSeek, keep the channel's original backend aligned with its current documented split: configure `deepseek-v4-flash` on the native [Responses endpoint with hosted `web_search`](https://api-docs.deepseek.com/zh-cn/guides/responses_api/) and set `supports_backend_search = true` only when that endpoint provides server search. Keep `deepseek-v4-pro` on the [Anthropic-compatible endpoint](https://api-docs.deepseek.com/zh-cn/guides/anthropic_api/) until DeepSeek documents Responses support for Pro. Set the flag to `false` when search should instead use Build's configured client search model. DeepSeek echoes a `{"type":"web_search"}` `tool_choice` in Responses output, but Build's pinned async-openai response type does not accept that enum. hellogrok normalizes only this descriptive field to `"auto"` on the return leg; the request sent to DeepSeek and all search output remain intact.

Start a new Grok Build process after enabling the proxy so it reloads the temporary model settings. A `true` route should report `hosted_web_search=1` and `function_web_search=0`; a `false`/omitted route with either usable search-model credential path should expose an ordinary `web_search` function. Its upstream diagnostic should report `function_web_search=1` and `client_web_search_aliased=true`, and an explicit search turn should also report `client_web_search_forced=true`. The search model's dedicated request should then report `client_web_search_prepared=true`, while the tool-result turn returns `client_web_search_forced=false`. A client route without either credential path has neither search form. Hosted Grok requests additionally report `x_search=1`. A completed hosted search logs nonzero `calls`, `completed`, and returned `sources` or `annotations`.

There are three separate Web paths:

- Hosted `web_search` is declared on a `supports_backend_search = true` model request and executed by that provider. hellogrok adds and adapts this declaration.
- Client `web_search` is an ordinary function available to a `false`/omitted model when either an explicit `[models].web_search` or Build's authenticated official default resolves. For explicit search intent, hellogrok selects it on the new user turn. Build then makes a separate request through that search model; the proxy adapts its existing hosted declaration, selects hosted search, and requires final text plus sources. Plain conversation, a caller-selected different tool, and the tool-result turn are not forced.
- `web_fetch` is a Grok Build-local function tool that fetches one known URL. hellogrok preserves its function definition and call across Responses, Messages, and Chat, but the fetch itself runs locally in Build.

Subagents follow the same routing. Grok Build passes the parent session's client-search sampling config, `web_fetch` config, and global search-disable flag into each child, then re-resolves `supports_backend_search` when `[subagents.models]` selects another model. In Build 0.2.118, adding only `[subagents.models]` unexpectedly makes `enabled` default to `false`; hellogrok repairs that omitted value transactionally. `general-purpose`, `explore`, and `plan` children retain Web tools in their normal capability modes, including `read-only`. An explicit request to delegate a search is left for `spawn_subagent`; hellogrok selects `web_search` on the child's request instead of hijacking the parent. A child that explicitly requests only `web_fetch` is not forced to search. An explicit Responses `allowed_tools` choice is enforced in parent and child requests. Grok CLI's `--tools` and `--disable-web-search` filters do not carry their original intent on the wire: the latter disables Build's client `web_search` and local `web_fetch`, but Build may still serialize xAI-only `x_search`, while a filtered hosted tool may simply be absent. hellogrok therefore cannot always distinguish a CLI exclusion from a route that needs compatibility injection.

The Build UI prints `Web Search <query>` for hosted search and `Fetch <URL>` for local fetch. A URL after `Web Search` therefore means the model used that URL as its search query; it does not prove that `web_fetch` ran.

Grok Console Free is not accessible by changing a relay URL. Current grok2api code [obtains a key-bound DPoP token and signs every request](https://github.com/chenyme/grok2api/blob/56cfd210ce33169997b709ec7529b90c708f919b/backend/internal/infra/provider/console/dpop.go), together with [SSO/Cloudflare identity and Console-specific headers such as `x-cluster`](https://github.com/chenyme/grok2api/blob/56cfd210ce33169997b709ec7529b90c708f919b/backend/internal/infra/provider/console/headers.go). A relay's public API key is not that internal credential. hellogrok preserves the documented standard `web_search` request, but it will not guess private endpoints, forge DPoP identity, or silently change `grok-4.5` to `Console/grok-4.5`. Use a provider-qualified model ID only when the relay advertises or documents it.

Credential resolution mirrors Build's channel ownership rules: a non-empty model `api_key`, the first resolved model `env_key`, then inherited provider credentials. Explicit model auth shadows provider auth. A valid command-based `[auth_provider.*]` or `[model_providers.*.auth]` may own the incoming Bearer or `X-Api-Key` selected by `auth_scheme`; undefined, empty, or malformed helpers fail closed. The proxy reads the Build-facing scheme and emits the upstream scheme separately, so an Anthropic-style Messages route can safely use `X-Api-Key`. Static keys and auth headers always win. Built-in models without a custom URL keep Grok Build's own login/OAuth path, while a custom Grok channel never receives that login token merely because its wire model also begins with `grok-`.

hellogrok does not add `stream_tool_calls = false`. Existing values are restored unchanged; the Build-facing leg always uses Responses, so that Chat-only workaround is neither required nor applied globally.

## Testing

```bash
go test ./... -count=1
```

CI runs the test suite and default `CGO_ENABLED=0` build on Windows, Linux, and macOS. It also compiles the optional tray target natively on Linux and macOS. Tagged releases cross-build:

- Windows amd64/arm64 GUI and console binaries
- Linux amd64/arm64 CLI binaries
- macOS amd64/arm64 CLI binaries

Windows users with configured live channels can additionally run:

```powershell
.\scripts\run_grok_all_channels_test.ps1
.\scripts\run_grok_all_channels_test.ps1 -RequireWebSearch -MaxTurns 1 -TimeoutSeconds 150
.\scripts\run_grok_all_channels_test.ps1 -RequireSubagentSearch -MaxTurns 4 -TimeoutSeconds 240
.\scripts\run_grok_all_channels_test.ps1 -Models gpt-congee -RequireSubagentSearch -ExpectedSubagentModel grok-llmx -MaxTurns 4 -TimeoutSeconds 240
.\scripts\run_grok_all_channels_test.ps1 -RequireWebFetch -MaxTurns 2 -TimeoutSeconds 150
```

The smoke runner isolates `HOME`, disables AnySearch, and applies a minimal per-probe tool allowlist before using headless permission bypass: plain replies have no effective tools, direct probes expose only `web_search` or `web_fetch`, and subagent probes expose only `Agent` plus `web_search`. The default plain probe also verifies the exact `OK` response instead of treating a zero exit code as success. The Web-search probe accepts either the hosted or client-function path according to the selected model's effective capability. It reads that channel's first proxy request, so a later request made by `[models].web_search` cannot hide the main-turn mode. The subagent probe additionally requires an observed `spawn_subagent` call and result, verifies that the parent request did not steal the delegated search, and requires a later child client-search selection or completed hosted-search call with source evidence. For a cross-model probe, configure `general-purpose` under the real `GROK_HOME` first; project-layer `[subagents.models]` is intentionally excluded from Build's trust-independent model resolution. `-ExpectedSubagentModel` only verifies the channel actually observed in proxy logs.

## Troubleshooting

### A plain prompt returns upstream 502

Inspect the upstream status in the proxy log first. If the upstream succeeded but Build reported 502, inspect the schema probe for missing Responses fields. The additive response patch runs only on the return path and is never sent back to the upstream gateway.

### Web search is not visible in Build's local tool list

For `supports_backend_search = true`, absence from that list is expected because hosted tools execute upstream; inspect the proxy request and later `search evidence` lines instead. For `false`/omitted, `web_search` appears when Build can authenticate either the explicit `[models].web_search` model or its compiled official default. Without either path, absence is intended. Visibility alone does not make the conversation model select it; an explicit search prompt should produce `client_web_search_forced=true` in the proxy log. `web_fetch` is independent and should remain visible when the proxy has enabled its feature gate.

### A Grok relay narrates a search but returns no tool call

First confirm the request log contains `hosted_web_search=1`, `x_search=1`, and `function_web_search=0`. If repeated forced-search probes sometimes return real `web_search_call` items from a `*-build` model and sometimes return only prose from the plain model, the relay is mixing Build-capable and generic account/provider routes under one public model ID. The relay operator must route Free Build OAuth accounts through the CLI upstream, preserve both hosted declarations and their result events, pin a stable session to one capable account, and health-check for a real completed call instead of treating HTTP 200 as search support. A downstream client cannot repair a node chosen inside the relay or authenticate to that private upstream with the relay's public API key.

### Grok Build reports a missing Responses field

Confirm the model's `base_url` points to `127.0.0.1:18787` while the proxy runs. A direct URL means the model was added after startup or the proxy is not active; stop and start it again.

### The previous process was killed and config still points to localhost

```bash
./dist/hellogrok restore
```

### Port 18787 is already in use

Stop the existing hellogrok process before starting another instance. Do not run two instances against the same Grok configuration.

### Linux autostart cannot connect to the user service manager

The current session does not provide a systemd user manager. Run `hellogrok start` directly or configure the process manager used by that distribution.

## Limits

- A `supports_backend_search = true` provider must implement hosted search on the configured endpoint. Field adaptation cannot create a provider-side search service, and a relay that strips tools or search result events cannot be repaired downstream.
- A `false`/omitted route depends on Grok Build authenticating either the explicit `[models].web_search` model or its own compiled official default. hellogrok does not invent or replace either model and never silently switches to hosted search. Selection recognizes explicit Chinese or English search, browsing, and freshness intent; other prompts remain under the selected model's normal tool choice.
- A relay account pool must not advertise one model ID for a mixture of Build-search and non-Build nodes. hellogrok can send the correct dialect but cannot select or replace an account hidden behind the relay.
- Chat Completions has no single cross-provider search field. hellogrok covers the legacy xAI `search_parameters` and OpenAI-compatible `web_search_options` conventions; a provider using another extension needs an explicit adapter. [Current xAI documentation](https://docs.x.ai/developers/model-capabilities/text/comparison) puts native agentic Web/X search on Responses and describes Chat Completions as function-calling only, so an xAI/Grok Chat route is searchable only when its gateway still implements the legacy extension. Configure the channel as Responses when the provider offers it.
- Messages and Chat upstream calls are deliberately non-streaming so the proxy can produce one valid canonical Responses event sequence. They retain content and tool semantics but do not reduce time-to-first-token.
- `--disable-web-search` or CLI `--tools` alone cannot reliably suppress hosted search on a `supports_backend_search = true` route: Build can still emit `x_search`, or omit a hosted declaration without encoding why, and the local facade cannot recover that absent CLI intent. To guarantee no hosted search, set the route capability to `false` and exclude the client functions with the session tool allowlist. An explicit wire-level Responses `allowed_tools` choice is always honored.
- Grok Build may issue an auxiliary request through the selected custom route. hellogrok pins every request on that route to the channel's configured wire model to prevent cross-model leakage. Some providers reject large or unsupported auxiliary payloads even when the primary turn succeeds.
- Upstream model availability, rate limits, malformed HTML, and gateway 5xx responses remain upstream responsibilities.
- Optional Unix tray behavior depends on the desktop environment; the official Unix CLI does not.
- Windows and macOS release artifacts are currently unsigned.

## License

Licensed under the [MIT License](./LICENSE).
