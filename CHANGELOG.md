# Changelog
All notable changes to this project will be documented in this file.
The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)

## [Unreleased]

### Fixed
- A streamed generation accumulating to the per-part bound (1 MiB) deterministically failed its `[DONE]` terminal release: the generated-frame bound equaled the accumulated bound, so the terminal envelope (which repeats the full content) exceeded it, erroring a COMPLETED upstream conversation and penalizing the circuit breaker. The generated-frame, terminal-batch, and exchange generated-total bounds are now jointly derived from the exchange accumulated-total (worst-case 6x JSON escaping plus envelope repetition plus the request echo), so every accepted exchange can complete its release; the amplification protection stays enforced at the new bounds.
- Transcode upstream transport failures no longer echo the raw transport error into the client-facing 502 body: a custom `http.Client`-based transport produces `*url.Error` values carrying the full outbound request URL (including a credential-bearing upstream base query), which could reach the unauthenticated client. The client now receives a neutral dialect error; the detail is logged server-side with sensitive URL query values redacted, recursing through nested error chains like the native passthrough sanitizer.
- A multi-part all-text tool result rendered to a Chat tool message was silently joined into one `\n`-separated string with no observability. The join is now the named `tool_result_text_join` decision: rejected under strict policy, recorded observably when approved via `-transcode-allow-loss tool_result_text_join`.
- Root-package tests no longer compile against nonexistent symbols: the idle-connection test calls the exported `proxy.MaxIdleConnsPerHost`, and `TestValidateMBFlag` lives where its unexported subject does (`internal/config`).
- Live 502 under `-transcode-messages-chat`/`-transcode-responses-chat`: the strict wire decoder now accepts the Verboo gateway's `cache_cost` extension (both placements) as opaque raw JSON and never forwards it.
- Recurring upstream-response 502s on unknown provider-extension fields (`completion_cost`, and the broader provider set): the upstream response decode is now TOLERANT to unknown fields on the subject-to-change provider envelope (a `wire.DecodeTolerant` path), so a field like `completion_cost` never fails a request. The CLIENT request decode and the content-block unions stay STRICT, preserving the lossless-transcoding invariant. `completion_cost` is modeled as opaque raw JSON at both placements and never forwarded.
- Approved-loss logging no longer floods the TUI/log: all approved losses of one conversion are aggregated into a single per-request line, and a per-route loss-profile summary is logged once at startup.
- Native passthrough with an upstream base URL that carries a path query or a trailing slash: the Rewrite hook now uses `pr.SetURL`, merging the base query before the client query and joining paths at the slash boundary (no doubled slashes, no path canonicalization).
- Transcode route keys are canonicalized the same way the router normalizes inbound paths, so a mapping configured as `/v1/responses/` (or with dot segments) dispatches instead of silently falling through to passthrough; canonically-equivalent routes are rejected as duplicates.
- Proxy configuration is frozen at construction: `auth.FreezeAuthPolicy` resolves the credential once (mutable `SecretSource`s can no longer change live behavior), and the route-limiters map and matcher (including `Pattern.Segments`) are copied, so caller mutation after `New` cannot alter routing or auth.
- Transcode auth defaults are documented as implemented — unset inherits the provider auth policy, else strip-only `none`; `-transcode-auth-source provider` is the explicit inheritance opt-in (never `auto`/`inbound` by default).
- A `-h`/`--help` lexeme consumed as the value of a preceding value-taking flag (e.g. `-upstream -h`) is no longer treated as a help request — it fails semantic validation like any other bad value.
- A proxy bind failure no longer leaves a metrics listener running (proxy binds first), and a fatal server error now calls `stop()` and gracefully shuts down every server while preserving the original error.
- Metrics `RecordAbortedRequest` publishes the journal entry before the aborted counter, so a consumer that observes the counter can read the matching log entry.
- `make lint` passes: dead increments in the TUI detail overlay are removed, and the pre-existing deadcode findings are catalogued as confirmed false positives / test-only-exercised wire API in `.deadcodeignore`.

### Changed
- `-transcode-auth-source` accepts `provider` (explicit provider-auth inheritance; conflicts with `-transcode-auth`/`-transcode-auth-header`, requires a configured provider auth source).
- The metrics endpoint binds after the proxy listener; a bad `-metrics-bind` address fails at startup with the proxy listener already released.

[Unreleased]: https://github.com/joeycumines/ai-concurrency-shaper/compare/366a3c8...HEAD