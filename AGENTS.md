# Mandatory for ALL readers

**This file intentionally contains no directory layout, file listing, or code structure details.**
Providing structural information here makes agents lazy, as they stop exploring source files, i.e. they guess. Don't guess. Read the actual code. Every time.

**Code must never reference session-scoped artefacts** — blueprint task numbers, review-round ids, incident nicknames: cite the observed behaviour or a commit instead, because a session reference is meaningless to anyone without the same context.

This is a **stealth reverse proxy** with bounded concurrency and a TUI dashboard.

Ensure these characteristics:

- It sits in front of one or more upstream HTTP APIs (e.g. LLM providers).
- Certain request routes (method + path, e.g. `POST /v1/messages`) are "limited" — concurrency-bound and queued.
- By default, requests outside the limited set pass through freely — no introspection needed beyond route matching.
- The proxy is by default completely transparent to both request and response content. Transcoding must provide the strongest guarantees of request/response integrity, and is strictly opt-in, with allowances for compatibility.
- Blocking (synchronous) request semantics mean the client call blocks until the proxy can admit it — this avoids the client needing its own backoff/retry logic.
- The TUI (charm/bubbletea v2) visualizes concurrency, queue state, and live metrics.
- The binary is `go install`-able from `github.com/joeycumines/ai-concurrency-shaper`.

The proxy supports optional HTTP request/response transcoding between the OpenAI Responses API (`/v1/responses`), the OpenAI Chat Completions API (`/v1/chat/completions`), and the Anthropic Messages API (`/v1/messages`).

Transcoding handlers are **route-scoped wrappers** around the existing proxy pipeline. `Proxy` owns a set of route mappings (`WithTranscodeMapping`); when a request arrives whose method+path matches a configured mapping (e.g. `POST /v1/responses` → upstream `/v1/chat/completions`), `Proxy.ServeHTTP` dispatches the request to the matching `TranscodeHandler` instead of the transparent path. Dispatch is method-scoped: non-POST methods on a mapped path pass through transparently. The handler:

1. reads and unmarshals the request body into the client schema,
2. decodes it into a canonical intermediate representation and renders the upstream schema directly (never through chained wire JSON),
3. rewrites the request URL path, recomputes `Content-Length`, strips and reapplies authentication per the target policy, and forwards through the proxy's existing engine,
4. converts the upstream response back to the client dialect — either as a single JSON document or as a translated `text/event-stream`.

**Non-transcoded routes remain transparent passthrough.** With no mapping configured there is no interception. Transcoding is a configurable feature, **off by default**, enabled only via CLI flags (`-transcode-route`, `-transcode-responses-chat`, `-transcode-messages-chat`, `-transcode-messages-responses`).

Transcoding invariants:

- Default behavior must be highly compatible out of the box — as strict as possible while remaining so: fidelity-only knobs (parameters, roles) are opt-in capabilities whose absence is an observable policy-gated loss, never a hard error and never a silently rendered incompatibility.
- Compatibility nuance: whether to absorb an upstream defect or refuse the exchange is decided per case, by what the client can still be told truthfully — clamped arithmetic, totals that do not add up, absent or over-large breakdowns and provider-extension fields can be absorbed and noted because the client dialect can carry the corrected value honestly, while a defect that would mislead the client (a missing required field, a type-corrupt modeled field, malformed syntax, a self-contradictory union arm) can justify refusal; every correction is recorded as a note, so the decision is visible in the per-request log.
- Client-facing protocols are OpenAI Responses and Anthropic Messages only. Chat Completions is an upstream-only fallback.
- Supported directions are Responses → Chat Completions, Messages → Responses, and Messages → Chat Completions; prefer Responses → Chat Completions for Responses clients, and Messages → Responses when its explicit loss policy is configured.
- Match mappings by HTTP method + path. Create-route mappings are POST-only.
- Decode source wire type → canonical IR → render target wire type. Never chain JSON conversions through another dialect.
- The supported surface is a strict subset. Every unsupported field or variant must produce a client-dialect error; never silently drop, default, merge, or reinterpret it.
- Preserve turn boundaries, content order, tool-call identity, tool-result identity, and stream lifecycle ordering.

- Thinking-block synthesis is adjudicated SAFE under the marker+scrubbing contract (operator ruling, 2026-09-07): the proxy may synthesize Anthropic `thinking` blocks ONLY under the `provider_reasoning_thinking` capability, ONLY with the marker signature (`SyntheticThinkingSignature`), and the request path MUST scrub marker-signature thinking blocks out of replayed history before any upstream rendering — a synthetic signature must never reach an upstream. `redacted_thinking` is never synthesized (no source data exists). Non-marker thinking blocks are source-authenticated artifacts: preserve them byte-for-byte or reject/explicitly lose them.
- Use event-specific SSE types. Emit every required field, keep `event:` equal to JSON `type`, and emit exactly one success terminal or one error terminal.
- A failed, malformed, truncated, or cancelled exchange must never be reported as a successful model completion.
- Strip inbound authentication before applying target authentication. Never forward credentials across providers blindly or log/journal them.
- `stream.Proxy` is mandatory for streaming copy/cancellation. The handler must still seal downstream writes, close the upstream body, and classify the final outcome before returning.
- Keep accepted-request, retry-replay, decoded-request, successful-response, and error-body limits separate.
- Official SDKs may be used only in temporary manual conformance checks, not as committed dependencies.

### Contract-role strictness and the directional loss model

The transcoder touches wire contracts of two different **roles**, and each role dictates a different strictness posture. Confusing the two — applying the client contract's strictness to the upstream contract — is the root of recurring availability failures (provider-extension fields like `completion_cost` and `cache_cost` must never break a request).

**Role 1 — the client contract (authoritative, pinned).** OpenAI Responses and Anthropic Messages, on both the request (client→proxy) and response (proxy→client) surfaces. These are pinned to authoritative SDK snapshots (`contracts.lock.json`). The client contract is the seat of the **lossless-transcoding invariant**: what the client sends must be honored losslessly, or observably lost under a named loss key, or rejected — never silently dropped, defaulted, merged, or reinterpreted. Therefore **client-request decode is STRICT** (`DisallowUnknownFields`). A field the client sends that is outside the pinned surface is a client-dialect error; if we silently ignored it we would break the invariant. This strictness is a *consequence* of the lossless guarantee, not paranoia.

**Role 2 — the upstream provider contract (subject to change, extensible).** OpenAI Chat Completions and OpenAI Responses on the upstream surfaces. The upstream is a third-party provider that extends its wire freely and independently (LiteLLM adds `completion_cost`/`cache_cost`; OpenRouter adds `cost`/`native_finish_reason`; vLLM adds `prompt_logprobs`/`metrics`; DeepSeek adds `logprobs.reasoning_content`). This contract is **subject to change** and is not under the client's or the transcoder's control. Therefore **upstream-response decode is TOLERANT to unknown fields** (`wire.DecodeTolerant`) — an unknown field is skipped (discarded) and never forwarded; it must never fail the request. There is **no automatic capture or logging** of a newly observed spelling: it is discoverable only by proactively capturing real bytes (`make field-recapture`), then modeling it in the wire shadows, documenting it in `pins.md`, and adding it to the field-capture corpus. The known provider-extension spellings are documented in `pins.md`.

**Tolerance is scoped, not blanket.** The upstream *envelope* (and the nested choice/message/usage objects) is the subject-to-change surface. The **content-block unions** and the **client request** are authoritative and stay strict: a `text` content block carrying `image_url`, or an unknown content-block `type`, is still rejected. This is the intelligent detection of a subject-to-change contract — it is a per-surface policy, not a per-field guess. On the OpenAI Responses upstream the OUTPUT ITEMS (and the content parts within them) likewise stay strict — they are content-bearing unions like the content-block unions, so an unknown output-item or content-part type is still rejected. The following ALWAYS reject on every surface (tolerance never relaxes them): duplicate JSON keys, illegal nulls on modeled fields, trailing values, malformed syntax, missing-required semantic fields, contradictory-union arm violations, and a **type-corrupt MODELED field** (e.g. `total_tokens:"two"` — a type error is never silently skipped). In the chat **stream**, the non-streaming `message` arm is a STRUCTURAL rejection (the streaming surface carries only deltas; a chunk carrying a `message` arm is corrupt wire, not a provider extension) — it is modeled in the shadow and rejected, so its content can never be silently dropped.

**The directional loss model.** Loss is **directional** and **per-feature**; the canonical IR is the pivot. The granular loss registry (`LOSS_MATRIX.md`) is direction-scoped: request-side keys (`request_reasoning`, `multiple_system_turns`, `anthropic_controls`, `previous_response_id`) and response-side keys (`provider_reasoning_text`, `usage_unknown`, `response_service_tier`). The **same field can be inert in one direction and a rejection in the other** — e.g. `reasoning_content` is a response-side provider extension in Chat→Messages (capability-gated text or an approved loss) while a request-side reasoning control is the separate `request_reasoning` key. For each direction every feature is routed to exactly one of three outcomes:

1. **Lossless** — the feature maps 1:1 to the target dialect and is forwarded.
2. **Observable loss** — the feature is non-portable and approved for loss; it is dropped and the loss is **logged** (see "Loss observability" below).
3. **Rejection** — the feature is non-portable and not approved; the conversion fails with a client-dialect error.

This three-way split is what preserves lossless transcoding: nothing is ever silently dropped, defaulted, merged, or reinterpreted. The tolerance on the upstream response does not relax it, because the invariant governs the client contract, and the union-arm strictness still guards client-visible content.

**Approved losses vs Notes.** The `ConversionReport.Losses` slice holds BOTH approved losses (`Lose`, gated by the policy) and **Notes** (`Note`, losses.go) — a Note is a *sanctioned encoding* recorded WITHOUT a policy decision (a capability-gated or loss-sanctioned mapping that invents or reinterprets content, e.g. `provider_reasoning_text` mapped to ordinary text). A Note is **not** a loss. Logging and the startup summary must distinguish them.

**Loss observability.** At **startup**, each transcode route logs one summary of its **loss profile**: the policy's `Allowed` set (with `LossKeyDescription`), or `none (strict)` for a strict route. Per **request**, each conversion stage logs ONE aggregated line: `request`-side (after request conversion) and `response`-side (after response conversion) — at most two per exchange. The line lists `feature at path` for approved losses, and `note: feature at path: <detail>` for Notes (so a Note's rationale is inline, since the startup summary covers only the Allowed set). Duplicate `feature at path` entries are deduped. The two-stage split is deliberate: request-side losses are logged before the upstream round-trip, so they remain observable even when the upstream/response fails.

**The lossless-transcoding invariant governs the CLIENT contract.** What the client sends is honored losslessly, or observably lost under a named key, or rejected — never silently dropped, defaulted, merged, or reinterpreted. This is enforced by the strict client-request decode, the union-arm strictness, and the directional loss model; the upstream-response tolerance never touches it.


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

Strict constraints:

- TUI output is not captured anywhere and is visible only to the local operator during an interactive session. Do not redact secrets from TUI display — the journal and TUI may show raw credential headers and URLs.
- Logging must be explicit about what is logged - for example, do not log arbitrary HTTP headers, as they may contain secrets. While the TUI does show the log, log output is also available in non-interactive sessions, so logging must be safe for that context.
