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

Transcoding handlers are **route-scoped wrappers** around the existing proxy pipeline. `Proxy` owns a set of route mappings (`WithTranscodeMapping`); when a request arrives whose method+path matches a configured mapping (e.g. `POST /v1/responses` → upstream `/v1/chat/completions`), `Proxy.ServeHTTP` dispatches the request to the matching `TranscodeHandler` instead of the transparent path. Dispatch is method-scoped: non-POST methods on a mapped path pass through transparently. The handler:

1. reads and unmarshals the request body into the client schema,
2. decodes it into a canonical intermediate representation and renders the upstream schema directly (never through chained wire JSON),
3. rewrites the request URL path, recomputes `Content-Length`, strips and reapplies authentication per the target policy, and forwards through the proxy's existing engine,
4. converts the upstream response back to the client dialect — either as a single JSON document or as a translated `text/event-stream`.

**Non-transcoded routes remain transparent passthrough.** With no mapping configured there is no interception. Transcoding is a configurable feature, **off by default**, enabled only via CLI flags (`-transcode-route`, `-transcode-responses-chat`, `-transcode-messages-chat`, `-transcode-messages-responses`).

### Transcoding invariants

- Default behavior must be highly compatible out of the box — as strict as possible while remaining so: fidelity-only knobs (parameters, roles) are opt-in capabilities whose absence is an observable policy-gated loss, never a hard error and never a silently rendered incompatibility.
- Client-facing protocols are OpenAI Responses and Anthropic Messages only. Chat Completions is an upstream-only fallback.
- Prefer native upstream protocols: Messages → Messages, Responses → Responses, then Messages → Responses, and Chat last.
- Match mappings by HTTP method + path. Create-route mappings are POST-only.
- Decode source wire type → canonical IR → render target wire type. Never chain JSON conversions through another dialect.
- The supported surface is a strict subset. Every unsupported field or variant must produce a client-dialect error; never silently drop, default, merge, or reinterpret it.
- Preserve turn boundaries, content order, tool-call identity, tool-result identity, and stream lifecycle ordering.
- Never synthesize Anthropic `thinking`, `redacted_thinking`, or signatures. Preserve an original Anthropic block byte-for-byte or reject/explicitly lose it.
- Use event-specific SSE types. Emit every required field, keep `event:` equal to JSON `type`, and emit exactly one success terminal or one error terminal.
- A failed, malformed, truncated, or cancelled exchange must never be reported as a successful model completion.
- Strip inbound authentication before applying target authentication. Never forward credentials across providers blindly or log/journal them.
- `stream.Proxy` is mandatory for streaming copy/cancellation. The handler must still seal downstream writes, close the upstream body, and classify the final outcome before returning.
- Keep accepted-request, retry-replay, decoded-request, successful-response, and error-body limits separate.
- Official SDKs may be used only in temporary manual conformance checks, not as committed dependencies.

Authoritative contracts:
https://platform.openai.com/docs/api-reference/responses
https://platform.openai.com/docs/api-reference/chat
https://platform.claude.com/docs/en/api/messages
https://platform.claude.com/docs/en/build-with-claude/streaming

### Streaming lifecycle

All streaming request/response proxying inside `TranscodeHandler` uses `stream.Proxy(ctx, local, remote)` from `github.com/joeycumines/sesame/stream`. This is mandatory — do not substitute `io.Copy` or raw net pipes.

`stream.Proxy` is the mandated bidirectional copy and cancellation boundary. The converted HTTP request body has already been submitted by `RoundTrip`; `stream.Proxy`'s local EOF triggers the configured adapter soft-close; the handler owns downstream sealing and body closure. Downstream cancellation (`r.Context().Done()`) aborts the proxy and releases the upstream connection.

The handler translates SSE incrementally: it parses upstream `data:` frames, converts each event through the direction-specific state machine, writes the translated frame, and flushes it. A held terminal event is released by the `[DONE]` sentinel or upstream EOF, after which the reader stops so the limiter slot and upstream connection are released even when the upstream keeps the connection open. Malformed frames are skipped only where the remaining stream can still produce a valid lifecycle; a stream that ends before a terminal condition emits a client-dialect error event rather than a silent clean EOF.

`stream.Proxy` aborts and returns as soon as the passed context is cancelled; it does not itself close the remote side in that case. The handler therefore cancels the upstream request context on `r.Context().Done()`, which releases the upstream connection.
