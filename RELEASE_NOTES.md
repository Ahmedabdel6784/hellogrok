# Release Notes — v0.1.4

## Private heartbeat frames no longer break Grok Build

Some Responses-compatible relays inject non-standard `keepalive`, `heartbeat`, or `ping` JSON events while a model is working. Grok Build 1.0.0 parses every data frame through a strict Responses event enum; these private values therefore caused `serialization error: unknown variant keepalive`, even though the upstream task and its subagents could still be running.

hellogrok now recognizes the common spelling variants in SSE `event:` fields, JSON `type` and `event` fields, and raw data payloads. It converts them to the standard `: keepalive` SSE comment, which keeps the connection active without entering Grok Build's typed event stream or consuming a Responses sequence number. Completion logs expose only the heartbeat count, never the private payload.

## Completed streams close immediately

All three supported backends now use their protocol terminal event as the end of the local stream. Responses stops at `response.completed`, `response.incomplete`, or `response.failed`; Messages stops at `message_stop`; Chat Completions stops at `[DONE]`. hellogrok then closes the upstream body, canceling relays that otherwise keep an already completed HTTP connection open and leave Grok Build stuck on `Waiting for response...`.
