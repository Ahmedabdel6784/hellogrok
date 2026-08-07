# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.1.1] — 2026-08-07

### Added

- Explicit search model selection via `[models].web_search` config key and `GROK_WEB_SEARCH_MODEL` environment variable, with env taking precedence over config.
- Hosted search capability auto-detection for Grok relay channels at startup (`web_search` + `x_search` probing).
- Chat Completions search dialect auto-detection (`search_parameters` vs `web_search_options`).
- CC Switch takeover detection before config rewrite — start is refused when CC Switch already owns Grok Build.
- Single-instance enforcement for both tray and foreground modes via OS-level lock.
- Windows log window application icon display in title bar and taskbar.
- Search route resolution logging at startup, visible in `hellogrok routes` and the log window.

### Changed

- Tray now defaults to proxy-enabled on first launch.
- Tray quit defers exit when a config-ownership conflict exists, preventing orphaned proxy URLs.
- SIGINT/SIGTERM handlers retry stop on deferred errors instead of leaving the process inconsistent.
- `BackendSearchSet` field distinguishes explicit `supports_backend_search` declarations from Build's false default, enabling runtime probing for unset values.
- `Route` now carries `HostedSearchKnown`, `HostedWebSearch`, `HostedXSearch`, and `HostedChatSearchDialect` for capability-aware request normalization.
- Tool choice normalization is now capability-aware — only confirmed hosted search declarations are injected.

### Fixed

- Managed search flag now placed after channel settings in config rewrites, preventing key-ordering issues.
- Client search wire alias properly hidden from upstream requests.

## [0.1.0] — 2026-08-07

### Added

- Initial release: cross-platform local proxy for Grok Build custom model channels.
- Response normalization for `responses`, `chat_completions`, and Anthropic-compatible `messages` APIs.
- Native Web tool support: `web_search` (hosted and client modes) and `web_fetch`.
- Channel-owned authentication isolation (API keys, env keys, auth providers, headers).
- Windows native tray application with proxy toggle, autostart, status/log window, and quit.
- CLI for foreground proxy, route inspection, config restore, autostart management, and log viewing.
- Automatic configuration preparation and recovery on normal and abnormal exit.
- Login autostart for Windows (registry), Linux (systemd user service), and macOS (LaunchAgent).
- CC Switch compatibility detection and conflict warnings.
- Builds for Windows, Linux, and macOS on amd64 and arm64.

[0.1.1]: https://github.com/hellowind777/hellogrok/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/hellowind777/hellogrok/releases/tag/v0.1.0
