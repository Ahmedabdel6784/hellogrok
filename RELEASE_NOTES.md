# Release Notes — v0.1.2

## Reliable startup and channel routing

Proxy startup no longer probes every upstream for search capabilities. Omitted or false `supports_backend_search` values use Grok Build's client-search path, while an explicit true value selects hosted search without a startup request. This removes the repeated one-to-two-minute waits caused by unavailable or incompatible Grok, Claude, and GPT channels; a bad capability declaration now fails on the first real search request.

Channel parsing now preserves dotted and dashed IDs, display names, upstream model names, URL paths, and channel-owned credentials. Legacy unquoted dotted model tables are normalized only while the proxy is active and restored byte-for-byte on stop. The official `messages` backend and historical `message` alias both route through Messages conversion. Authentication defaults to Bearer for all backends, with `auth_scheme = "x_api_key"` available for providers that require it.

## True streaming across every supported protocol

Responses SSE continues to pass through frame by frame. Anthropic Messages and Chat Completions requests now retain `stream=true`, and their SSE is converted incrementally into Grok Build Responses events for reasoning, answer text, function arguments, hosted-search activity, usage, completion, and terminal errors. The first local delta is emitted before the upstream response finishes.

If a provider ignores `stream=true` and returns one complete JSON response, hellogrok keeps the request usable through a buffered SSE fallback and records that downgrade explicitly. A complete upstream response cannot be reconstructed into genuine token timing.

## Native search source counts for all channel protocols

Search evidence from Responses, Messages, and Chat Completions channels is normalized into both `web_search_call.action.sources` and `output_text.annotations`. This also applies when any supported protocol is selected as Grok Build's client-search model, independently of `supports_backend_search`. Grok Build can therefore render its native deduplicated site count for both hosted and client search.

Only real HTTP(S) URLs from structured results, citations, annotations, or a final answer with independently confirmed search activity are emitted. Ordinary answer links never create a search call, and hellogrok does not invent sources or counts when a provider returns no URL evidence.

## Response validation and retry control

Successful upstream responses must now contain the minimum valid Responses, Messages, or Chat Completions envelope. Malformed 2xx responses return a focused 502 instead of entering Grok Build's retry loop as apparent successes. Deterministic configuration, schema, and conversion failures include `X-Should-Retry: false`; real transport failures and upstream retry hints retain their retry behavior. Successful HTML pages from an incorrect API root produce a direct `base_url`/`api_backend` diagnostic without exposing the page body.

Client-search tool use is driven only by structured `tool_choice`, not prompt keywords. Provider-only search replay is isolated by channel, prior conversation, and stable search identity, and ambiguous matches are rejected instead of combining unrelated search blocks.

## Live Grok Build synchronization and recovery

After proxy enable or disable, hellogrok connects to an existing Grok Build shared leader over ACP, reloads the model catalog, and reselects the current model in idle custom-model sessions. This refreshes in-memory URLs, backends, credentials, and normalized model IDs without requiring a new window. Active, input-blocked, externally replaced, and `--no-leader` sessions are handled conservatively and report when manual `/model` reselection is still required.

Both current and legacy ACP model-switch methods are supported. On Windows, a live named-pipe leader misreported as stale by Grok Build 1.0.0 is accepted only while its lock is actively held. Version 5 rewrite transactions remain recoverable after the state schema upgrade.

## Windows status, logs, and port handling

The status panel now groups proxy, channel, Grok-session, protocol/search, and recovery information. Logs append across sessions, retain the latest seven distinct usage days by default, and support selectable retention plus wrapped next-match search. Status text wraps while raw log lines keep horizontal scrolling.

An occupied `127.0.0.1:18787`, including WinSock error 10048, is detected before Grok configuration is changed. Asynchronous tray failures are shown directly instead of leaving the interface in an uncertain state.
