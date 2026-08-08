# Release Notes — v0.1.3

## Conversation continuity after model switches

When you switch models with `/model` in Grok Build, historical reasoning items from the previous model may contain encrypted state that the new model cannot decrypt. This previously caused repeated request rejections and retries. hellogrok now automatically clears encrypted reasoning items that originate from a different channel, protocol, or upstream endpoint, while preserving all visible messages, tool calls, tool results, search history, and unencrypted reasoning intact.

Reasoning provenance is tracked in a bounded private index using SHA-256 digests only — no conversation content is exposed. Encrypted state from the same source continues to pass through normally. Unknown state from older conversations is also passed through on the first attempt; only a structured signature or decryption rejection triggers a single cleanup replay, and a second rejection marks it as non-retryable.

## Faster startup and more reliable channel routing

Proxy startup no longer probes every upstream provider for search capabilities. When `supports_backend_search` is unset or `false`, Grok Build's client-search path is used directly. An explicit `true` selects hosted search without a startup probe. This eliminates the one-to-two-minute startup delays that occurred when Grok, Claude, or GPT channels were unavailable or incompatible. Incorrect capability declarations now fail on the first real search request instead of during startup.

Channel identifiers now fully preserve dots and dashes in IDs, display names, upstream model names, URL paths, and channel-owned credentials. Legacy model tables are normalized only while the proxy is active and restored byte-for-byte on stop. The official `messages` backend and historical `message` alias both route through Messages conversion. All backends default to Bearer authentication; providers requiring `X-Api-Key` can set `auth_scheme = "x_api_key"`.

## True streaming for all supported protocols

Responses SSE continues to pass through frame by frame. Anthropic Messages and Chat Completions requests now preserve `stream=true`, with SSE deltas converted incrementally into Grok Build Responses events — covering reasoning, answer text, function arguments, hosted-search activity, usage statistics, completion, and terminal errors. The first local delta is emitted before the upstream response finishes.

If a provider ignores `stream=true` and returns a single complete JSON response, hellogrok keeps the request functional through a buffered SSE fallback and explicitly records the downgrade. Once the full upstream response has arrived, genuine per-token timing cannot be reconstructed.

## Unified search source counts across all channels

Search evidence from Responses, Messages, and Chat Completions channels is normalized into both `web_search_call.action.sources` and `output_text.annotations`. This applies regardless of which protocol is selected as Grok Build's client-search model, independent of `supports_backend_search`. Grok Build can therefore display its native deduplicated site count for both hosted and client search.

Only real HTTP(S) URLs from structured results, citations, annotations, or answers with independently confirmed search activity are emitted. Ordinary links in answers never create a search call, and hellogrok does not fabricate sources or counts when a provider returns no URL evidence.

## Smarter response validation and retry control

Successful upstream responses must now contain a valid Responses, Messages, or Chat Completions structure. Malformed 2xx responses return a clear 502 instead of entering Grok Build's retry loop as if they were legitimate successes. Deterministic failures caused by configuration, schema, or conversion issues now include `X-Should-Retry: false` to prevent pointless retries; real transport failures and upstream retry hints retain their existing behavior. HTML success pages returned from an incorrect API root produce a direct `base_url`/`api_backend` diagnostic without exposing the page body.

Client-search tool selection is now driven solely by structured `tool_choice`, not by scanning prompt keywords. Provider-specific search replay is isolated by channel, prior conversation, and stable search identity — ambiguous matches are rejected outright rather than merging unrelated search content.

## Automatic session sync for open windows

After enabling or disabling the proxy, hellogrok connects to the current Grok Build shared leader over ACP, reloads the model catalog, and reselects the current model in idle custom-model sessions. This means you no longer need to open a new window after changing proxy settings — in-memory URLs, backends, credentials, and normalized model IDs refresh automatically. Sessions that are actively in use, waiting for input, externally replaced, or using `--no-leader` are handled conservatively, with a clear prompt when manual `/model` reselection is still needed.

Both current and legacy ACP model-switch methods are supported. On Windows, a live named-pipe leader incorrectly reported as stale by Grok Build 1.0.0 is accepted only while its lock is actively held. Version 5 rewrite transactions remain recoverable after the state schema upgrade.

## Windows: status panel, logging, and port conflicts

The status panel now organizes information into proxy, channel, Grok session, protocol/search, and configuration recovery sections. Logs append across proxy sessions, retaining the latest seven distinct usage days by default, with support for custom retention periods and wrapped next-match search. Status text wraps automatically while raw log lines retain horizontal scrolling.

Port conflicts on `127.0.0.1:18787`, including WinSock error 10048, are detected before any changes are made to the Grok configuration. Asynchronous tray operation failures are now reported directly instead of leaving the interface in an indeterminate state.
