# Release Notes — v0.1.1

## Explicit search model selection

The `[models].web_search` config key and `GROK_WEB_SEARCH_MODEL` environment variable now correctly route Grok Build client search through the selected model. The environment variable takes precedence over the config file value. An empty environment value is treated as absent, falling back to the config key.

## Hosted search capability auto-detection

Grok relay channels with an omitted `supports_backend_search` are now auto-probed at startup. The proxy detects whether the relay provides `web_search`, `x_search`, or both, and normalizes hosted search requests accordingly. Chat Completions channels also get dialect auto-detection (`search_parameters` vs `web_search_options`) so search fields match the provider's expected format.

## CC Switch takeover detection and conflict management

hellogrok now detects CC Switch's Grok Build takeover marker (`PROXY_MANAGED` on the `/grokbuild/v1` route) before rewriting the Grok configuration. Start is refused when CC Switch already owns the file. The tray menu also defers quit when a config-ownership conflict exists, preventing CC Switch from later restoring a route to a stopped proxy.

## Single-instance enforcement

A second tray launch in the same login session exits immediately with a clear message. The foreground command also enforces single-instance via an OS-level lock, preventing duplicate proxy processes from competing for the same local port or configuration file.

## Default proxy-enabled on first launch

The proxy is now enabled by default on first launch. Users who prefer to start stopped can disable it once; subsequent launches remember the choice.

## Tray quit protection

When a provider manager (e.g., CC Switch) still owns the Grok configuration, the tray defers exit rather than leaving an orphaned proxy URL. Resolve the configuration conflict in the other tool first, then quit hellogrok.

## Signal handling robustness

SIGINT and SIGTERM handlers now retry the stop sequence when a deferred error occurs (e.g., a config-ownership conflict detected mid-shutdown), keeping the process alive until the configuration can be safely restored.

## Windows log window icon

The Windows status and log window now displays the application icon in both the title bar and the taskbar.

## Search route resolution at startup

Each channel's effective search mode — hosted capability, client search selection, or auto-detection result — is now resolved and logged at startup, making `hellogrok routes` and the log window report the actual search behavior for every channel.
