# ai-concurrency-shaper

Reverse proxy with bounded concurrency for AI/LLM API endpoints.

Sits in front of an upstream HTTP API (e.g. Anthropic, OpenAI) and limits concurrent requests to configured routes. Requests that exceed the limit block until a slot opens. No client-side backoff needed. Non-matching requests pass through unmodified.

Run with `-tui` for a terminal dashboard with live metrics and request inspection.

<p align="center">
  <img src="docs/assets/tui.webp" alt="TUI dashboard showing live request metrics" width="800">
</p>

## Install

```sh
go install github.com/joeycumines/ai-concurrency-shaper@latest
```

## Usage

```sh
ai-concurrency-shaper -upstream https://api.anthropic.com
```

### Flags

#### Core

| Flag | Default | Description |
|------|---------|-------------|
| `-upstream` | _(required)_ | Upstream base URL |
| `-bind` | `:8080` | Listen address |
| `-limit` | _(repeatable)_ | Route pattern to limit, matched by trailing segments (defaults to common AI endpoints) |
| `-concurrency` | `4` | Max concurrent limited requests |
| `-global-concurrency` | `0` | Global concurrency limit (0 = disabled) |
| `-queue-timeout` | `30s` | Max wait for a concurrency slot |
| `-upstream-disable-keep-alives` | `false` | Disable HTTP keep-alives to upstream; each request uses a fresh TCP connection. Use when the upstream counts idle connections as concurrent. |
| `-retry` | `-1` | Max retry attempts (-1 = unlimited, 0 = disabled) |
| `-retry-max-body-mb` | `5` | Max request body size (MB) eligible for retry |
| `-tui` | `false` | Enable terminal dashboard |
| `-version` | | Print version and exit |

#### Concurrency Protection

The proxy's internal semaphore limits how many tokens are held concurrently. What the downstream actually observes depends on its own accounting: providers differ in how they measure concurrency (active connections, in-flight requests, token usage windows, etc.), and most have some lag between completing a response and decrementing their counter. These flags insert delays after slot release to reduce the risk that the downstream observes N+1 or higher concurrency, at the cost of throughput. They are configurable because the right tradeoff depends on the upstream's accounting behavior. Defaults are conservative.

| Flag | Default | Description |
|------|---------|-------------|
| `-release-cooldown` | `200ms` | Delay after releasing a slot before re-admission. Reduces the chance the next request arrives while the downstream is still cleaning up. |
| `-cancel-cooldown` | `200ms` | Hold the slot after a client disconnects once an upstream attempt has started. Mitigates N+1 from rapid connect/disconnect cycles. |
| `-failure-hold` | `2s` | Hold the slot after an upstream failure (5xx, 429, or rate-limit-signaled 403) when the circuit breaker is disabled or its penalty is zero. When the breaker is enabled with a non-zero penalty, the phantom penalty takes precedence instead. |
| `-retry-min-delay` | `1s` | Minimum delay before retrying. Reduces the chance the retry arrives before the downstream has finished accounting. |
| `-retry-skip-429` | `true` | Do not retry 429 responses. Avoids the feedback loop where retries amplify concurrency at the downstream. |
| `-adaptive-headroom` | `false` | Reduce effective concurrency by one slot after a 429, restoring after a quiet window. Use when the provider can see N+1 concurrent requests due to connection teardown or CDN accounting lag. |
| `-adaptive-headroom-window` | `30s` | How long the one-slot 429 headroom is held. Each new 429 resets this window. |

The upstream HTTP transport sizes `MaxIdleConnsPerHost` to the sum of configured route/global concurrency caps, with a per-host minimum floor of 20 applied after the global cap. This avoids closing a large burst of healthy keep-alive connections when multiple route limiters or groups share the same upstream host. Use `-upstream-disable-keep-alives` only when the upstream counts idle/open connections as concurrent.

#### Circuit Breaker

| Flag | Default | Description |
|------|---------|-------------|
| `-circuit-breaker` | `true` | Enable circuit breaker |
| `-cb-threshold` | `5` | Failures within window to trip the breaker |
| `-cb-window` | `30s` | Failure counting window |
| `-cb-open-timeout` | `10s` | Time before the breaker probes (half-open) |
| `-cb-max-open-timeout` | `120s` | Max open timeout after backoff |
| `-cb-penalty` | `2s` | Base phantom concurrency hold time |
| `-cb-max-penalty` | `60s` | Max phantom concurrency hold time |

The circuit breaker treats 5xx, 429, transport errors, and rate-limit-signaled 403s as upstream failures. A bare 403 without `Retry-After` or `x-ratelimit-*` headers is treated as an authentication/authorization client error and is passed through, avoiding the trap where a bad API key is masked by a proxy-generated 503 after the breaker opens.

#### Observability Semantics

The TUI summary separates clean completions from incomplete exchanges. `Clean proxied` and `Clean passthrough` count requests whose HTTP exchange completed through the proxy. `Aborted` counts exchanges that did not complete cleanly, including client disconnects, downstream write/flush failures, and upstream response-body copy failures; the request and network views mark those rows as aborted and leave response-complete timing unset. Status buckets still count any committed HTTP status, so an aborted stream that already received `200` appears in both the `2xx` bucket and `Aborted`.

For `101 Switching Protocols`, local inability to complete the requested upgrade (for example a non-Hijacker downstream writer or a `101` response body that is not bidirectional) and failed downstream 101 handshake write/flush are aborted HTTP exchanges, but they do not count as upstream breaker failures. Once the handshake succeeds, later WebSocket or other upgraded-protocol stream closes are treated as upgraded connection lifetime events, not HTTP response-body aborts.

#### Retry Tuning

| Flag | Default | Description |
|------|---------|-------------|
| `-retry-wait-min` | `500ms` | Minimum retry wait |
| `-retry-wait-max` | `30s` | Maximum retry wait |

### Examples

Proxy with default limits and TUI:

```sh
ai-concurrency-shaper -upstream https://api.anthropic.com -tui
```

`-limit` patterns are **end-anchored suffix matches**: only the trailing path segments matter, and the path may have any prefix. This means `-limit "POST /chat/completions"` matches `/v1/chat/completions` and `/api/v2/chat/completions`, but it does **not** match `/v1/chat/completions/123`. A sub-resource needs its own `-limit` pattern or it passes through unlimited.

```sh
# matches /v1/chat/completions and /openai/deployments/x/chat/completions
ai-concurrency-shaper \
  -upstream https://api.openai.com \
  -limit "POST /chat/completions:2" \
  -limit "POST /embeddings:4" \
  -global-concurrency 10
```

Grouped routes sharing a limiter:

```sh
ai-concurrency-shaper \
  -upstream https://api.anthropic.com \
  -limit "POST /messages:3@messages" \
  -limit "POST /messages/batches:3@messages"
```

Maximum throughput (disable all protections; only safe if the upstream has no accounting lag):

```sh
ai-concurrency-shaper \
  -upstream https://api.anthropic.com \
  -circuit-breaker=false \
  -release-cooldown 0 \
  -cancel-cooldown 0 \
  -failure-hold 0 \
  -retry-min-delay 0 \
  -retry-skip-429=false
```

### Concrete Example

Proxying to an API with strict concurrency limits, slow accounting, streaming responses, and occasional 5xx errors:

```sh
ai-concurrency-shaper \
  -upstream https://api.example.com \
  -concurrency 4 \
  -queue-timeout 30m \
  -bind 127.0.0.1:8080 \
  -tui \
  -retry 2 \
  -retry-min-delay 5s \
  -retry-skip-429=true \
  -release-cooldown 500ms \
  -cancel-cooldown 500ms \
  -circuit-breaker=true \
  -cb-threshold 2 \
  -cb-window 10s \
  -cb-penalty 5s \
  -cb-max-penalty 60s \
  -upstream-disable-keep-alives \
  -adaptive-headroom \
  -adaptive-headroom-window 30s
```

- **`-concurrency 4`** matches the upstream's per-key limit. The proxy holds at most 4 tokens concurrently.
- **`-queue-timeout 30m`** LLM streaming requests can take minutes. A 30s timeout would kill legitimate long-running requests.
- **`-retry 2`** bounded retries. More than 2 risks amplifying load during an outage; unlimited (`-1`) is only safe when the upstream recovers quickly.
- **`-retry-min-delay 5s`** the upstream has slow accounting. Without this floor, the default backoff (starting at 500ms) could send a retry before the upstream finishes cleaning up from the failed attempt.
- **`-retry-skip-429=true`** a 429 means the upstream is overloaded. Retrying adds concurrent requests and makes the problem worse.
- **`-release-cooldown 500ms`** the default 200ms is too aggressive for this upstream; accounting lag is closer to 500ms.
- **`-cancel-cooldown 500ms`** mitigates N+1 from rapid disconnect/reconnect cycles.
- **`-cb-threshold 2` / `-cb-window 10s`** trip after 2 failures in 10s. Aggressive, but appropriate when the upstream is generally reliable and any 5xx indicates a real problem. Bare 403 auth errors do not count unless the response carries rate-limit signals.
- **`-cb-penalty 5s` / `-cb-max-penalty 60s`** phantom concurrency penalty (exponential backoff). The proxy holds the slot as if a request is still in flight, reducing the rate of new requests reaching the struggling upstream. Stacks with `release-cooldown` (5s penalty, then release, then 500ms cooldown = 5.5s total).
- **`-upstream-disable-keep-alives`** the provider counts *open* connections, not just in-flight requests. Without this, idle keep-alive connections can push the observed concurrency above the `-concurrency 4` limit and trigger 429s.
- **`-adaptive-headroom` / `-adaptive-headroom-window 30s`** some providers (or their CDNs) can still observe a momentary N+1 concurrent requests because connection teardown is asynchronous. When a 429 arrives, these flags temporarily reduce the effective limit by one slot, creating headroom. The slot is restored after 30s with no new 429s.

## Transcoding

The proxy can transcode requests between the OpenAI Responses API
(`/v1/responses`), the OpenAI Chat Completions API (`/v1/chat/completions`,
upstream only), and the Anthropic Messages API (`/v1/messages`). A
transcoded route is **method + path** scoped and intercepts only the
configured client route; every other route stays a transparent passthrough.
Transcoding is **off by default** — no mapping, no interception.

### Route examples

```sh
# Responses client -> Chat upstream (transcode /v1/responses to /v1/chat/completions)
ai-concurrency-shaper -upstream https://api.openai.com -transcode-responses-chat

# Messages client -> Responses upstream
ai-concurrency-shaper -upstream https://api.openai.com -transcode-messages-responses \
  -transcode-allow-loss tool_schema_strictness

# Messages client -> Chat upstream
ai-concurrency-shaper -upstream https://api.anthropic.com -transcode-messages-chat \
  -transcode-allow-loss usage_unknown

# Explicit route with a custom path
ai-concurrency-shaper -upstream https://api.example.com \
  -transcode-route "responses@/v1/responses=chat-completions@/v1/chat/completions"
```

Both Messages presets map `/v1/messages`, so enabling both is a startup
error. A Messages-to-Responses mapping without an approved
`tool_schema_strictness` loss is also a startup error (Messages tools cannot
preserve the strictness the Responses contract requires).

### Sensible defaults

Every CLI mapping (`-transcode-route` and the presets) starts from a
sensible out-of-the-box configuration so a minimal invocation works against
a modern OpenAI-compatible chat upstream. All three layers are additive —
the flags below extend the defaults, never replace them:

| Layer | Default | Meaning |
| --- | --- | --- |
| Chat capabilities | `developer_role`, `parallel_tool_calls`, `reasoning_effort`, `provider_reasoning_text` | the standard modern OpenAI-compatible chat surface, enabled via `-transcode-chat-capability` (granular names: `developer_role`, `image_input`, `structured_outputs`, `parallel_tool_calls`, `stop_sequences`, `reasoning_effort`, `provider_reasoning_text`) |
| Allowed client query | `beta` | Anthropic clients (Claude Code) gate every request with `?beta=true`; harmless on chat endpoints. Add more via `-transcode-allow-client-query` |
| Loss policy | `reasoning_summary`, `authenticated_thinking`, `responses_controls`, `anthropic_controls`, `builtin_tools`, `usage_unknown`, `usage_cache_read_unknown`, `usage_cache_write_unknown`, `usage_reasoning_unknown` | the non-portable features real Responses/Messages client traffic triggers (reasoning summaries, Anthropic thinking blocks, Responses and Anthropic envelope controls, built-in tools, and usage breakdowns the chat upstreams do not always report); approved via `-transcode-allow-loss` on top of the defaults. Note: approving `responses_controls` tolerates `include`/`client_metadata`/`prompt_cache_key` and upstream-echoed controls — the conversation-state request controls (`background`, `max_tool_calls`, `prompt`, `safety_identifier`, `status`) are errors under every policy |

Capabilities are exercised only when the client actually uses the feature:
`reasoning_effort` forwards the Responses `reasoning.effort` (and an
Anthropic `thinking` budget, see below) as the chat `reasoning_effort`
parameter; `provider_reasoning_text` maps the chat `reasoning` response
extension to client text; `developer_role` preserves the Responses
developer-role messages; `parallel_tool_calls` forwards the
parallel-tool-calls setting.

Anthropic `thinking` on a Messages client maps to chat `reasoning_effort`
as follows: `type: adaptive` emits nothing (the client delegated the
decision to the model — the chat provider applies its own default);
`type: disabled` emits nothing (no thinking requested, matching the chat
absence of `reasoning_effort`) and the elision is reported;
`type: enabled` maps `budget_tokens` through
a deterministic, documented threshold table (`<1024` minimal, `<4096` low,
`<16384` medium, else high) and the mapping is reported on every exchange.
The thinking members are validated per type (`enabled` carries
`budget_tokens`, `disabled` carries none, `adaptive` carries `display`) —
a cross-type member is a malformed request, not a silently ignored field.
Request-side reasoning controls — an enabled `thinking` budget or a
Responses `reasoning.effort` — are gated by their own `request_reasoning`
loss key, deliberately distinct from the response-side
`provider_reasoning_text` (chat reasoning content): an operator never has
to approve losing response reasoning text just to strip a request knob,
or vice versa. Without the `reasoning_effort` capability an enabled budget
is a loss/reject decision, never a silent drop — and on a non-Chat target
(e.g. Messages→Responses) an enabled budget is likewise a loss/reject
decision, never silently dropped.

One scope caveat on "out of the box": the sensible defaults make a minimal
invocation work for the Chat directions (`-transcode-responses-chat`,
`-transcode-messages-chat`). The Messages→Responses direction is not
zero-flag: it requires `-transcode-allow-loss tool_schema_strictness`
(shown in the route examples above), and a mapping without that approval
fails at startup rather than serving tool traffic loosely.

### Removing defaults

The sensible defaults above exist so a minimal invocation works out of the
box against a modern OpenAI-compatible chat upstream. An operator targeting
a strict legacy chat upstream — one that rejects the `reasoning_effort`
parameter or chokes on the `?beta=true` query — can withdraw any default
from the command line alone, without the programmatic API. Three layers are
negatable the same way: a value prefixed with `!` withdraws a default.

```sh
# Withdraw the default reasoning_effort capability and the default beta
# query forwarding, but keep every other default. Explicit positives still
# apply on top, so this also adds image_input:
ai-concurrency-shaper -upstream https://api.example.com -transcode-responses-chat \
  -transcode-chat-capability '!reasoning_effort' \
  -transcode-chat-capability image_input \
  -transcode-allow-client-query '!beta'

# Withdraw a default loss approval (builtin_tools) so built-in tool requests
# are rejected instead of dropped:
ai-concurrency-shaper -upstream https://api.example.com -transcode-responses-chat \
  -transcode-allow-loss '!builtin_tools'
```

Negations validate against the same granular vocabulary as positives: an
unknown `!name` fails at startup exactly like an unknown positive, never on
the first request. A name given both positively and negated is a conflict
and fails at startup — a negation that could be silently overridden by a
positive elsewhere on the command line would be a trap. An explicit
positive always survives a negation of a *different* name; a negation only
ever withdraws a default.

To start from a completely blank slate instead of withdrawing defaults one
by one, `-transcode-strict-defaults` removes every default at once: no
default chat capabilities, no beta query forwarding, and no default loss
approvals. Explicit `-transcode-chat-capability`,
`-transcode-allow-client-query`, and `-transcode-allow-loss` values still
apply on top.

```sh
# Blank slate: approve only usage_unknown and forward only the api-version
# query, nothing else:
ai-concurrency-shaper -upstream https://api.example.com -transcode-responses-chat \
  -transcode-strict-defaults \
  -transcode-allow-loss usage_unknown \
  -transcode-allow-client-query api-version
```

### Loss policy

Every non-portable feature is gated by exactly one granular, direction-
specific loss key (the complete registry is `internal/transcode/LOSS_MATRIX.md`,
generated from the same registry the program uses). CLI mappings start from
the sensible default approvals listed above; the programmatic API (zero
`LossPolicy`) is **strict** and rejects every non-portable feature with a
client-dialect error — nothing is silently dropped, defaulted, merged, or
reinterpreted. The `-transcode-allow-loss` flag (repeatable, comma/space
separated) approves additional keys on top of the defaults; only the
granular names exist, there are no aliases:

```sh
# Approve two further losses on top of the CLI defaults: image input and
# multiple system turns
-transcode-allow-loss image_input -transcode-allow-loss multiple_system_turns
```

### Stream negotiation

A streaming request must be answered with a stream and a non-streaming
request with a JSON document; a mismatch (an upstream SSE stream for a
non-streaming request, or JSON for a streaming request) is an upstream
failure, never a silent fallback. The client's stream intent (the
`stream`/`stream: true` field in the client dialect) drives the negotiation.

### Authentication

- `-transcode-auth` selects the target policy: `auto`, `none`, `bearer`,
  `x-api-key`, `api-key`, or a custom `header`.
- `-transcode-auth-source inbound` forwards the single credential from the
  client request; `env:NAME` and `file:PATH` supply the secret from the
  environment or a bounded file read (64 KiB cap; atomic file replacement
  rotates the credential without a restart).
- Secrets are never accepted as command-line arguments. Inbound
  credentials are stripped before the target policy is applied: nothing is
  forwarded across provider boundaries unless the configured policy says
  so, and cookies, forwarding headers, and provider-specific controls
  never cross unless explicitly allowlisted.

### Operational limits

Every client-visible error message is bounded (`ErrorMessageBytes`), the
complete rendered JSON response is bounded before any header is committed
(`GeneratedResponseBytes`), generated SSE frames and terminal batches are
bounded (`GeneratedSSEFrameBytes`/`GeneratedSSEBatchBytes`), and the
inbound request/response bodies are bounded independently. Output limits
smaller than the minimum legal terminal or error frame are rejected at
startup — a stream that could never emit its terminal is a configuration
error, not a runtime surprise.

### Failure taxonomy

| Exchange | Classification |
| --- | --- |
| Unsupported-but-valid source feature | Local conversion error (never an upstream failure) |
| Malformed source wire, body failures, contract-violating usage totals | Upstream failure |
| Invalid model-generated tool arguments (target requires an object) | Local unrepresentable output |
| Client abort, no definitive upstream failure yet | Neither success nor upstream failure |
| Client abort after a definitive upstream failure | Upstream failure retained |
| Local signer failure | Local, never retried, never breaker-relevant |

## How Concurrency Protection Works

The proxy uses a token-bucket channel to enforce the concurrency limit. Each limited request acquires a token; the token is returned when the request completes. This bounds the proxy's internal concurrency, but the downstream may still observe more due to accounting lag (see above).

The concurrency protection flags insert dead zones between slot release and re-admission:

- **`-release-cooldown`** (success path): token is held for this duration before re-entering the pool. Default 200ms covers most provider accounting windows.
- **`-failure-hold`** (failure path): slot is held after 5xx, 429, or rate-limit-signaled 403 when the circuit breaker is disabled. Default 2s. When the breaker is enabled, the phantom penalty (`-cb-penalty`) handles failure-path holds instead — the two are mutually exclusive (else-if branches).
- **`-cancel-cooldown`** (client disconnect): slot is held briefly when a client disconnects after an upstream attempt has started. Default 200ms.
- **`-retry-min-delay`** (retry path): floor on retry delay to reduce the chance of arriving before the downstream finishes cleanup. Default 1s.
- **`-retry-skip-429`** (429 amplification): retrying a 429 adds concurrent requests at the downstream. Enabled by default.

### Stacking

Protections stack additively:

- Failure + release cooldown (breaker disabled): failure-hold (2s), then release, then release-cooldown (200ms). Total: 2.2s. Failure classification includes 5xx, 429, and rate-limit-signaled 403s; bare auth 403s are passed through.
- Failure + release cooldown (breaker enabled): phantom penalty (2 to 60s), then release, then release-cooldown (200ms). Total: penalty + 200ms. The failure-hold is not used in this path — the breaker penalty subsumes it.
- Client cancel + cancel cooldown: cancel-cooldown (200ms), then release, then release-cooldown (200ms). Total: 400ms.

## Building

```sh
make build          # compile
make test           # run tests
make lint           # vet + staticcheck + deadcode
make all            # build, then lint + test
```

## License

[GPL-3.0](LICENSE)
