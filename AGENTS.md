# AGENTS.md — ai-concurrency-shaper

## Structural Notice

**This file intentionally contains no directory layout, file listing, or code structure details.**

Providing structural information here makes agents lazy — they stop reading source files and start guessing. Don't guess. Read the actual code. Every time.

If you need to know how something works, **open the file**. If you need to know what files exist, **list the directory**. If you need to know what a function does, **read its signature and its tests**.

## Role

You are an implementer. Your job is to write correct, well-tested Go code for this project. Read first. Implement second. Verify third.

## Principles

- Read the relevant source before writing anything.
- Write tests that prove correctness, not tests that mirror implementation.
- Keep the public API small and intentional.
- Handle errors explicitly. No `_` for errors unless there is a comment justifying it.
- No unnecessary abstractions.
- When in doubt, prefer the simpler path — but make the simpler path robust.

## Scope Reminder

This is a **stealth reverse proxy** with bounded concurrency and a TUI dashboard.

- It sits in front of an upstream HTTP API (e.g. an LLM provider).
- Certain request routes (method + path, e.g. `POST /v1/messages`) are "limited" — concurrency-bound and queued.
- Requests outside the limited set pass through freely — no introspection needed beyond route matching.
- **No response body reading or munging.** The proxy is completely transparent to response content.
- Clients use the default HTTP client. Blocking (synchronous) request semantics mean the client call blocks until the proxy can admit it — this avoids the client needing its own backoff/retry logic.
- The TUI (charm/bubbletea v2) visualizes concurrency, queue state, and live metrics.
- The binary is `go install`-able from `github.com/joeycumines/ai-concurrency-shaper`.

If you drift beyond this scope, stop and re-read this file.

## Transcoding

The proxy supports optional HTTP request/response transcoding between the OpenAI Responses API (`/v1/responses`), the OpenAI Chat Completions API (`/v1/chat/completions`), and the Anthropic Messages API (`/v1/messages`).

### Pipeline integration

Transcoding handlers are **route-scoped wrappers** around the existing proxy pipeline. `Proxy` owns a set of route mappings (`WithTranscodeMapping`); when a request arrives whose method+path matches a configured mapping (e.g. `POST /v1/responses` → upstream `/v1/chat/completions`), `Proxy.ServeHTTP` dispatches the request to the matching `TranscodeHandler` instead of the transparent path. The handler:

1. reads and unmarshals the request body into the client schema (`internal/transcode/schemas.go`),
2. converts the payload to the upstream schema (`internal/transcode/mux.go`),
3. rewrites the request URL path (e.g. `/v1/responses` → `/v1/chat/completions`), recomputes `Content-Length`, and forwards through the proxy's existing engine,
4. converts the upstream response back to the client schema — either as a single JSON document or as a translated `text/event-stream`.

**Non-transcoded routes remain 100% transparent passthrough.** No mapping configured, no interception, no body reading, no latency added: the zero-overhead path is untouched. This is deliberate: transcoding is a configurable feature, **off by default**, enabled only via CLI flags (`-transcode-route`, `-transcode-responses-chat`, `-transcode-messages-chat`, `-transcode-messages-responses`).

### Streaming and half-close lifecycle

All streaming request/response proxying inside `TranscodeHandler` uses `stream.Proxy(ctx, local, remote)` from `github.com/joeycumines/sesame/stream`. This is mandatory — do not substitute `io.Copy` or raw net pipes. `stream.Proxy` handles **asymmetrical half-closes** correctly: when the local side emits `io.EOF` (client finished sending the request body), remote's write pipe is soft-closed while remote's read pipe stays open, so the upstream response stream continues to flow. Downstream cancellation (`r.Context().Done()`) aborts the proxy immediately and releases the upstream connection.

The handler translates SSE incrementally: it parses upstream `data:` frames, converts each event structure through a stateful accumulator (`ChatResponsesStreamState` / `AnthropicResponsesStreamState`), writes the translated frame as `data: {...}\n\n`, and flushes immediately via `http.Flusher`. Terminal events (`[DONE]`, `message_stop`) are flushed on upstream EOF before the stream closes. Malformed SSE lines are skipped safely without dropping the connection.

`stream.Proxy` aborts and returns as soon as the passed context is cancelled; it does not itself close the remote side in that case. The handler therefore cancels the upstream request context on `r.Context().Done()`, which releases the upstream connection.
