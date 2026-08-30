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

That's the whole surface for a single upstream: every tuning flag below applies to it, top-level. To front several upstreams through one port instead, see [Multiple Providers](#multiple-providers) — the same flags apply per `--provider` section.

### Flags

Every flag lives in one of two scopes. **Server-scope** flags configure the listener itself; **provider-scope** flags configure one upstream's behavior (limiting, retry, breaker, cooldowns). In the single-upstream invocation above the two scopes are flattened onto one command line — that form keeps working exactly as before. With `--provider` sections, each flag must appear in the section it configures.

Run `ai-concurrency-shaper -h` (also inside a provider section, e.g. `--provider=acme -h`) for the complete flag reference. Usage errors — unknown flags, malformed sections — exit 2 with a hint pointing at `-h`; semantic failures (bad values, missing upstream) exit 1.

| Flag | Scope | Default | Description |
|------|-------|---------|-------------|
| `-upstream` | provider | _(required)_ | Upstream base URL |
| `-bind` | server | `:8080` | Listen address |
| `-metrics-bind` | server | _(unset)_ | Dedicated listen address for the Prometheus `/metrics` endpoint (see [Metrics Export](#metrics-export)); empty disables it |
| `-limit` | provider | _(repeatable)_ | Route pattern to limit, matched by trailing segments (defaults to common AI endpoints) |
| `-limit-all` | provider | `false` | Limit all requests, not just matching routes. Use for "dumb" blanket rate limiting when you don't know the upstream's expensive routes. |
| `-concurrency` | provider | `4` | Max concurrent limited requests |
| `-global-concurrency` | provider | `0` | Global concurrency limit (0 = disabled) |
| `-queue-timeout` | provider | `30s` | Max wait for a concurrency slot |
| `-upstream-disable-keep-alives` | provider | `false` | Disable HTTP keep-alives to upstream; each request uses a fresh TCP connection. Use when the upstream counts idle connections as concurrent. |
| `-retry` | provider | `-1` | Max retry attempts (-1 = unlimited, 0 = disabled) |
| `-retry-max-body-mb` | provider | `5` | Max request body size (MB) eligible for retry |
| `-auth-source` | provider | _(unset)_ | Upstream credential source: `env:VAR`, `file:PATH`, or `none` (strip-only). Unset disables upstream auth entirely — requests are forwarded verbatim |
| `-auth-mode` | provider | `auto` | How the credential is attached: `auto` (derived from the upstream host), `bearer`, `x-api-key`, `api-key`, or `header:NAME`. `none` strips client credentials without injecting anything |
| `-auth-header` | provider | _(required by `header:` mode)_ | Custom upstream auth header name (e.g. `X-Goog-Api-Key`) |
| `-anthropic-version` | provider | `2023-06-01` | `Anthropic-Version` header value applied when the resolved mode is `x-api-key` |
| `-tui` | server | `false` | Enable terminal dashboard |
| `-version` | server | | Print version and exit |

The tables in [Concurrency Protection](#concurrency-protection), [Circuit Breaker](#circuit-breaker), and [Retry Tuning](#retry-tuning) are all provider-scope.

#### Concurrency Protection

The proxy's internal semaphore limits how many tokens are held concurrently. What the downstream actually observes depends on its own accounting: providers differ in how they measure concurrency (active connections, in-flight requests, token usage windows, etc.), and most have some lag between completing a response and decrementing their counter. These flags insert delays after slot release to reduce the risk that the downstream observes N+1 or higher concurrency, at the cost of throughput. They are configurable because the right tradeoff depends on the upstream's accounting behavior. Defaults are conservative.

| Flag | Scope | Default | Description |
|------|-------|---------|-------------|
| `-release-cooldown` | provider | `200ms` | Delay after releasing a slot before re-admission. Reduces the chance the next request arrives while the downstream is still cleaning up. |
| `-cancel-cooldown` | provider | `200ms` | Hold the slot after a client disconnects once an upstream attempt has started. Mitigates N+1 from rapid connect/disconnect cycles. |
| `-failure-hold` | provider | `2s` | Hold the slot after an upstream failure (5xx, 429, or rate-limit-signaled 403) when the circuit breaker is disabled or its penalty is zero. When the breaker is enabled with a non-zero penalty, the phantom penalty takes precedence instead. |
| `-retry-min-delay` | provider | `1s` | Minimum delay before retrying. Reduces the chance the retry arrives before the downstream has finished accounting. |
| `-retry-skip-429` | provider | `true` | Do not retry 429 responses. Avoids the feedback loop where retries amplify concurrency at the downstream. |
| `-adaptive-headroom` | provider | `false` | Reduce effective concurrency by one slot after a 429, restoring after a quiet window. Use when the provider can see N+1 concurrent requests due to connection teardown or CDN accounting lag. |
| `-adaptive-headroom-window` | provider | `30s` | How long the one-slot 429 headroom is held. Each new 429 resets this window. |

The upstream HTTP transport sizes `MaxIdleConnsPerHost` to the sum of configured route/global concurrency caps, with a per-host minimum floor of 20 applied after the global cap. This avoids closing a large burst of healthy keep-alive connections when multiple route limiters or groups share the same upstream host. Use `-upstream-disable-keep-alives` only when the upstream counts idle/open connections as concurrent.

#### Circuit Breaker

| Flag | Scope | Default | Description |
|------|-------|---------|-------------|
| `-circuit-breaker` | provider | `true` | Enable circuit breaker |
| `-cb-threshold` | provider | `5` | Failures within window to trip the breaker |
| `-cb-window` | provider | `30s` | Failure counting window |
| `-cb-open-timeout` | provider | `10s` | Time before the breaker probes (half-open) |
| `-cb-max-open-timeout` | provider | `120s` | Max open timeout after backoff |
| `-cb-penalty` | provider | `2s` | Base phantom concurrency hold time |
| `-cb-max-penalty` | provider | `60s` | Max phantom concurrency hold time |

The circuit breaker treats 5xx, 429, transport errors, and rate-limit-signaled 403s as upstream failures. A bare 403 without `Retry-After` or `x-ratelimit-*` headers is treated as an authentication/authorization client error and is passed through, avoiding the trap where a bad API key is masked by a proxy-generated 503 after the breaker opens.

#### Metrics Export

Pass `-metrics-bind 127.0.0.1:2112` to expose a Prometheus text-format `/metrics` endpoint on a dedicated listener — it never shares the proxy port, so a bare-root provider keeps every path and scraping is never mistaken for proxied traffic. Every series carries a `provider` label (an unnamed single provider exports as `provider="default"`): `shaper_active`, `shaper_queued`, `shaper_retries_in_flight`, `shaper_clean_proxied_total`, `shaper_clean_passthrough_total`, `shaper_aborted_total`, `shaper_circuit_rejected_total`, `shaper_requests_total{status="1xx"…"5xx"}`, and `shaper_breaker_state` (0 closed / 1 half-open / 2 open; omitted when the provider's breaker is disabled). Series are grouped by metric name — each family forms one contiguous block across all providers — as the exposition format's grouping rule requires. The endpoint is off by default and binds before the proxy listener, so a bad address fails at startup. Bind it to loopback unless you know what you are exposing.

#### Observability Semantics

The TUI summary separates clean completions from incomplete exchanges. `Clean proxied` and `Clean passthrough` count requests whose HTTP exchange completed through the proxy. `Aborted` counts exchanges that did not complete cleanly, including client disconnects, downstream write/flush failures, and upstream response-body copy failures; the request and network views mark those rows as aborted and leave response-complete timing unset. Status buckets still count any committed HTTP status, so an aborted stream that already received `200` appears in both the `2xx` bucket and `Aborted`.

For `101 Switching Protocols`, local inability to complete the requested upgrade (for example a non-Hijacker downstream writer or a `101` response body that is not bidirectional) and failed downstream 101 handshake write/flush are aborted HTTP exchanges, but they do not count as upstream breaker failures. Once the handshake succeeds, later WebSocket or other upgraded-protocol stream closes are treated as upgraded connection lifetime events, not HTTP response-body aborts.

#### Retry Tuning

| Flag | Scope | Default | Description |
|------|-------|---------|-------------|
| `-retry-wait-min` | provider | `500ms` | Minimum retry wait |
| `-retry-wait-max` | provider | `30s` | Maximum retry wait |

### Multiple Providers

Front several upstreams through one port with `--provider` sections. A `--provider[=name]` marker starts a new section; every provider-scope flag after it — until the next marker — configures that upstream only. Server-scope flags (`-bind`, `-tui`, `-version`) belong before the first marker.

```sh
ai-concurrency-shaper \
  -bind 127.0.0.1:8080 \
  -tui \
  --provider=anthropic \
    -upstream https://api.anthropic.com \
    -prefix /anthropic \
    -concurrency 4 \
  --provider=openai \
    -upstream https://api.openai.com \
    -prefix /openai \
    -concurrency 8 \
    -retry 2
```

A client targeting `http://127.0.0.1:8080/anthropic/v1/messages` reaches Anthropic's `/v1/messages`; `-concurrency 4` bounds only the Anthropic section, while OpenAI gets its own limiter (8) and retry budget (2). Without `-name` on the marker, the display name derives from the upstream host (`api.anthropic.com` → `anthropic`); an explicit `-name` inside the section overrides the marker.

Two provider-only flags join the surface in sectioned mode:

| Flag | Default | Description |
|------|---------|-------------|
| `-prefix` | _(required in multi mode)_ | Mount path for the provider, stripped before forwarding |
| `-name` | _(derived from upstream host)_ | Display name in logs and the TUI header |

Mount semantics:

- The prefix is a true mount: `/anthropic/v1/messages` is forwarded upstream as `/v1/messages`. A request equal to the mount (`/anthropic`) forwards as `/`.
- With more than one provider, requests matching no prefix get `404 Not Found` — nothing leaks to a default upstream. A single bare provider (no `--provider` markers, no prefix) still serves every path from the root, exactly as before.
- Prefixes must not overlap: `/anthropic` and `/anthropic/v1` cannot coexist. Overlapping configurations are rejected at startup, before the listener binds.
- Providers are matched on whole path segments, so `/anthropic2` never matches the `/anthropic` prefix.

In the TUI, each provider keeps its own dashboard. The header shows one chip per provider (the active one highlighted), filled by the provider name instead of the `⚡ shaper` brand; `Tab`/`Shift+Tab` cycle providers, chips are clickable, and the number keys `1-6` still switch content tabs. The in-TUI help (`?`) lists the switch binding whenever the switcher is shown.

#### Upstream Authentication

Each provider can carry its own upstream credential, so clients no longer need to know provider secrets at all. Credentials live in the **environment**, resolved once at startup; they never appear in command lines, logs, or the TUI.

```sh
export SHAPER_PROVIDER_ANTHROPIC_API_KEY=sk-ant-...   # resolved by the proxy at startup
export SHAPER_PROVIDER_OPENAI_API_KEY=sk-...

ai-concurrency-shaper \
  -bind 127.0.0.1:8080 \
  --provider=anthropic \
    -upstream https://api.anthropic.com \
    -prefix /anthropic \
    -auth-source env:SHAPER_PROVIDER_ANTHROPIC_API_KEY \
  --provider=openai \
    -upstream https://api.openai.com \
    -prefix /openai \
    -auth-source env:SHAPER_PROVIDER_OPENAI_API_KEY
```

With auth enabled, every proxied request has all client HTTP credential headers (`Authorization`, `Proxy-Authorization`, `X-Api-Key`, `Api-Key`, `X-Goog-Api-Key`) plus protocol headers (`Anthropic-Version`, `Anthropic-Beta`) and cloud signature families (`x-amz-*`, `x-goog-*`) stripped first — then exactly one upstream credential is attached. A client that sends OpenAI's token to the Anthropic mount cannot leak it to Anthropic, and vice versa: leakage of these credential headers is structurally impossible, not merely discouraged. The injection happens inside Go's `Rewrite` hook, where hop-by-hop headers have already been removed, so a malicious `Connection` header list cannot strip the injected credential.

One deliberate exception: **`Cookie` headers are forwarded verbatim** to whichever mount receives the request, even with auth enabled — stripping them unconditionally would break cookie-authenticated upstreams. Per the TUI Redaction Constraint (see AGENTS.md), the journal and TUI display raw headers and URLs — including `Cookie` — because TUI output is not captured anywhere and is visible only to the local operator during an interactive session. If your clients send cookies you do not want forwarded, strip them before they reach the gateway.

The gateway never sends `X-Forwarded-For`, `X-Forwarded-Host`, `X-Forwarded-Proto`, or `Forwarded` upstream — client-supplied forwarding headers are removed and no client IP is injected (the gateway is a stealth hop). This is also true when auth is disabled.

Modes:

| `-auth-mode` | Upstream sees | Typical provider |
|--------------|---------------|------------------|
| `auto` _(default)_ | `x-api-key` for `api.anthropic.com`/`*.anthropic.com`, otherwise `Authorization: Bearer` | any |
| `bearer` | `Authorization: Bearer <secret>` | OpenAI-compatible |
| `x-api-key` | `X-Api-Key: <secret>` + configured `-anthropic-version` | Anthropic Messages |
| `api-key` | `Api-Key: <secret>` | Azure key-based |
| `header:NAME` | `NAME: <secret>` | Gemini-style custom headers (set `NAME` via `-auth-header NAME` instead if you prefer) |

Sources:

| `-auth-source` | Behavior |
|----------------|----------|
| `env:VAR` | Read `VAR` from the environment once at startup. Unset or blank → startup fails naming the variable |
| `file:PATH` | Read the secret from `PATH` once at startup. Unreadable or blank → startup fails naming the path |
| `none` | Strip-only hygiene: client credentials are removed, nothing is injected |
| _(unset)_ | Auth disabled entirely — requests are forwarded verbatim, exactly as before this feature existed |

Where secrets can and cannot appear: a referenced variable's *value* is never logged, printed, or written anywhere by the proxy — startup logs name only the *reference* (`env:SHAPER_PROVIDER_ACME_API_KEY`). Per AGENTS.md TUI Redaction Constraint, the journal and TUI may show raw credential headers and URLs (including upstream `Set-Cookie` and `?key=` query strings) — TUI output is not captured and is visible only to the local operator during an interactive session. Requests are still forwarded byte-for-byte unchanged, and transport-error log lines are scrubbed via `sanitizeTransportError` (any `*url.Error` query is redacted) as defense-in-depth for captured logs. Passing secrets as literal argv values would expose them in `ps`/`/proc`; use env references instead.

Other credential channels outside the header allowlist are your responsibility: credentials embedded in path segments are forwarded by design and appear in route labels and request listings; request bodies captured for the TUI preview may contain whatever the client sent; custom secret headers not in the strip list above are forwarded verbatim and shown unredacted. If clients send secrets through these channels that you do not want stored or displayed locally, strip them before they reach the gateway.

A multi-provider configuration with no auth on some providers prints one startup note (`N of M providers configured without upstream auth`) so an open relay is never silent.

#### Scope & Limitations

Routing is **path-prefix only**: there is no model-ID translation or request-body inspection today. Clients choose a provider by targeting its mount (`/anthropic/...`, `/openai/...`); a single base URL with body-aware model routing is future work.

Other explicit scope boundaries and deferred capabilities:
- **No Downstream Client Authentication (M4)**: There is no downstream client authentication (anything that can reach the port can use every mounted provider). Transcoding's inbound credential mode is per-route forwarding/transcoding, not virtual-key admission.
- **Stateless Inference without Session Stickiness (G6)**: Stateless LLM inference does not require session affinity or stickiness. Per-tool session affinity, if ever needed in the future, would be based on tool HTTP headers, cookies, or hashes.
- **Provider Resource Isolation vs Client Identity Rotation (G7)**: The proxy intentionally does not implement LocalAddr pooling, User-Agent rotation, TLS fingerprint spoofing, or keypool rotation on 429. Per the Maxim AI caveat, having $N$ API keys in the same organization does not yield $N\times$ quota under modern upstream mitigation policies (providers enforce limits per organization, project, or billing account). The correct and supported architectural pattern is defining distinct organizations, accounts, or regions as distinct `--provider` resources with isolated queue limiters and circuit breakers, rather than per-request identity rotation.
- **Readiness and Configuration**: Readiness is TCP-connect only, and configuration changes require a restart.

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

Two upstreams behind one port (see [Multiple Providers](#multiple-providers) for the mount and switcher details):

```sh
ai-concurrency-shaper \
  -bind 127.0.0.1:8080 \
  -tui \
  --provider=anthropic \
    -upstream https://api.anthropic.com \
    -prefix /anthropic \
  --provider=openai \
    -upstream https://api.openai.com \
    -prefix /openai \
    -limit "POST /chat/completions:8"
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

### Transcoding Flags

All transcoding flags are **provider-scope**: in sectioned mode (`--provider`), each flag configures that specific provider's transcode mappings.

| Flag | Scope | Default | Description |
|------|-------|---------|-------------|
| `-transcode-route` | provider | _(repeatable)_ | Explicit transcode route mapping (`client-proto@client-path=upstream-proto@upstream-path`) |
| `-transcode-responses-chat` | provider | `false` | Preset: Responses client (`POST /v1/responses`) → Chat upstream (`POST /v1/chat/completions`) |
| `-transcode-messages-chat` | provider | `false` | Preset: Messages client (`POST /v1/messages`) → Chat upstream (`POST /v1/chat/completions`) |
| `-transcode-messages-responses` | provider | `false` | Preset: Messages client (`POST /v1/messages`) → Responses upstream (`POST /v1/responses`) |
| `-transcode-chat-capability` | provider | _(repeatable)_ | Enable or withdraw (`!name`) an optional chat upstream capability |
| `-transcode-allow-client-query` | provider | _(repeatable)_ | Forward client query parameter (`name` or withdraw `!name`) |
| `-transcode-allow-loss` | provider | _(repeatable)_ | Approve non-portable semantic loss by granular key (or withdraw `!key`) |
| `-transcode-strict-defaults` | provider | `false` | Strip all out-of-the-box chat capabilities, query parameters, and loss approvals |
| `-transcode-model` | provider | _(repeatable)_ | Map client model name to upstream model name (`client=upstream`), identity fallback when omitted |
| `-transcode-auth` | provider | `auto` | Per-route target auth mode override (`auto`, `bearer`, `x-api-key`, `api-key`, `header:NAME`, `none`) |
| `-transcode-auth-source` | provider | `inbound` | Per-route credential source override (`inbound`, `env:VAR`, `file:PATH`, `none`) |
| `-transcode-auth-header` | provider | _(required for custom header mode)_ | Header name when `-transcode-auth` is custom `header:` |
| `-transcode-max-request-mb` | provider | `10` | Max unmarshaled request body size (MB) for transcoding |
| `-transcode-max-response-mb` | provider | `10` | Max unmarshaled non-streaming response body size (MB) for transcoding |

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
| Chat capabilities | `parallel_tool_calls`, `provider_reasoning_text` | a maximally compatible out-of-the-box core, enabled via `-transcode-chat-capability` (granular names: `developer_role`, `image_input`, `structured_outputs`, `parallel_tool_calls`, `stop_sequences`, `reasoning_effort`, `provider_reasoning_text`, `system_anywhere`). The fidelity-only knobs — `reasoning_effort` (a parameter several open-source servers reject) and `developer_role` (a role Qwen/Llama/DeepSeek chat templates do not know) — are deliberately opt-in: add them for upstreams that accept the modern surface |
| Allowed client query | `beta` | Anthropic clients (Claude Code) gate every request with `?beta=true`; harmless on chat endpoints. Add more via `-transcode-allow-client-query` |
| Loss policy | `reasoning_summary`, `authenticated_thinking`, `mid_conversation_system`, `responses_controls`, `anthropic_controls`, `builtin_tools`, `usage_unknown`, `usage_cache_read_unknown`, `usage_cache_write_unknown`, `usage_reasoning_unknown`, `request_reasoning`, `developer_role` | the non-portable features real Responses/Messages client traffic triggers (reasoning summaries, Anthropic thinking blocks, system turns that cannot keep their position in a chat request, Responses and Anthropic envelope controls, built-in tools, usage breakdowns the chat upstreams do not always report, and the effort/role knobs behind the opt-in capabilities); approved via `-transcode-allow-loss` on top of the defaults. Note: approving `responses_controls` tolerates `include`/`client_metadata`/`prompt_cache_key` and upstream-echoed controls — the conversation-state request controls (`background`, `max_tool_calls`, `prompt`, `safety_identifier`, `status`) are errors under every policy |

Capabilities are exercised only when the client actually uses the feature:
`provider_reasoning_text` maps the chat provider reasoning response
extension — spelled `reasoning` (OpenRouter style) or `reasoning_content`
(the DeepSeek/Qwen convention open-weights gateways stream) — to client
text; `parallel_tool_calls` forwards the parallel-tool-calls setting. Two
capabilities are opt-in because generic upstreams reject what they render:
`reasoning_effort` forwards the Responses `reasoning.effort` (and an
Anthropic `thinking` budget, see below) as the chat `reasoning_effort`
parameter — without it the knob drops observably under the default
`request_reasoning` loss; `developer_role` preserves Responses
developer-role messages — without it developer turns render as ordinary
system messages and the distinction drop is observable under the default
`developer_role` loss; `system_anywhere` renders system/developer turns
positionally for upstreams that accept system messages anywhere (e.g.
genuine OpenAI).

### System message placement

By default (no `system_anywhere` capability) a chat request carries
exactly one system-role message, at index 0: open-weights chat templates
(Qwen/Llama/DeepSeek Jinja) reject any system-role message after index 0,
including a second leading one. System-channel turns — the Anthropic
envelope `system` plus inline mid-conversation `role: "system"` messages,
and developer-role messages when the `developer_role` capability is off —
consolidate into that single leading message with their content joined in
order. When a system turn followed dialog turns, the position loss is
approved under the default `mid_conversation_system` loss key; a leading-
only merge of multiple system turns is recorded as a sanctioned note under
the same key. The strict programmatic policy rejects the position loss;
pass `-transcode-chat-capability system_anywhere` to restore positional
rendering for upstreams that accept it.

Anthropic `thinking` on a Messages client maps to chat `reasoning_effort`
as follows — when the opt-in `reasoning_effort` capability is enabled
(without it, an enabled budget drops observably under the default
`request_reasoning` loss and nothing is rendered): `type: adaptive` emits
nothing (the client delegated the
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

### Large tool surfaces vs small upstream contexts

The proxy renders client tool definitions faithfully — it never trims,
summarizes, or drops tools to fit an upstream. Coding agents attached to
several MCP servers routinely exceed open-weights context windows on their
own: a realistic ~270-tool surface measures in the hundreds of kilobytes
(roughly 80k–100k tokens of schema text alone), which a 32k-context model
rejects with an upstream 400 (`context_length_exceeded`) after paying full
queue and inference latency. That rejection passes through verbatim —
upstream status, upstream message, rendered in your client's dialect — so
when an agent session dies this way, count its tools before blaming the
route: point that client at a larger-context upstream or reduce the MCP
surface exposed to it. Parameter-level rejections (an upstream that refuses
`parallel_tool_calls` or `stream_options`) are a
different failure mode; withdraw those defaults per mapping as shown under
"Removing defaults". (`reasoning_effort` and developer roles are already
opt-in for exactly this reason: the compatible core never renders them.)

### Removing defaults

The sensible defaults above exist so a minimal invocation works out of the
box against essentially any OpenAI-compatible chat upstream — the
compatible core never renders parameters or roles that generic and
open-weights servers reject. An operator can still adjust every layer from
the command line alone, without the programmatic API: restore the modern
knobs for an upstream that accepts them, or withdraw any default with a
`!` prefix. Three layers are negatable the same way: a value prefixed with
`!` withdraws a default.

```sh
# Restore the modern-surface knobs for an upstream that accepts them
# (reasoning_effort parameter and developer-role messages), keep every
# default, and also add image_input:
ai-concurrency-shaper -upstream https://api.example.com -transcode-responses-chat \
  -transcode-chat-capability reasoning_effort \
  -transcode-chat-capability developer_role \
  -transcode-chat-capability image_input

# Withdraw a default loss approval (builtin_tools) so built-in tool requests
# are rejected instead of dropped, or drop the default beta query forwarding:
ai-concurrency-shaper -upstream https://api.example.com -transcode-responses-chat \
  -transcode-allow-loss '!builtin_tools' \
  -transcode-allow-client-query '!beta'
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

### Strict transcoding and the circuit breaker

A deterministic upstream body-shape failure on a **non-streaming JSON
exchange** — a 2xx response whose body violates the pinned wire contract
(malformed source wire, never a merely-unsupported feature) — is classified
as an **upstream failure** and counts against the
circuit breaker: a consistently-poisonous upstream will open the breaker,
which is the correct signal (the upstream, not the proxy, is emitting
bodies no client can consume). Streams classify identically: a corrupt
frame inside a stream is corrupt upstream wire — an upstream failure,
breaker-counted whether or not the exchange streams — while only locally
generated conversion errors on a live stream stay local. The proxy never re-sends a poisonous body
itself: its retry transport decides before the body is read, so a
decode-failure 502 costs exactly one upstream hit; the repeat 502s you may
see in the field are client-driven retries. Fix dialect coverage (teach the
transcoder the provider's shape) rather than widening breaker tolerance,
and note that with `-concurrency 1` each poison round trip holds the slot
for the full inference time, so queue latency amplifies while a poison
loop persists. Every non-streaming upstream-response wire-decode failure
is logged server-side (`transcode: METHOD /path: …`) alongside the bounded
client-facing error.

### Provider extensions and the field-capture corpus

The pinned wire contract covers the official schemas, but real gateways also
emit their own opaque extensions — `prompt_token_ids`, `prompt_text`,
`reasoning_content`, `matched_stop`, `stop_reason`, `routed_experts`,
`token_ids`, the top-level usage extensions (`reasoning_tokens`,
`cached_tokens`, `prompt_cache_hit_tokens`, `prompt_cache_miss_tokens`),
and `prompt_tokens_details.created_cache_tokens`; the exhaustive table lives in
`internal/transcode/pins.md`. Four field regressions — three of them
spellings from this list, plus an empty `status` string on Responses
history items — caused live field failures before they were modeled; every
one passed the synthetic test suite, because synthetic fixtures encode the
same assumptions as the decoders.

The defense is a committed corpus of sanitized fixtures reconstructed from
the four field regressions: `internal/transcode/testcorpus/testdata/field/`
holds stream, non-stream, and request fixtures carrying the exact spellings
and null-vs-value placement real providers use. The field-capture tests
replay them through the production decode functions — not a test-only
copy — so a newly captured extension the shadows do not yet model fails
`go test` with the field name, not a user session:

```sh
go test ./internal/transcode/ -run TestFieldCapture
```

To teach the transcoder a new provider shape, capture real gateway bytes
first. Manual recapture targets exist in `project.mk` for this (never run
against a shared instance; they cost real tokens and need a gateway you
are authorized to use):

```sh
make field-recapture          # boot a throwaway shaper (FIELD_UPSTREAM=…)
make field-recapture-probe    # save raw stream + non-stream bytes
make field-recapture-stop     # stop it by exact PID
```

The bearer credential is read from the environment at runtime and fed to
curl on stdin, so it is never persisted: the environment value is never expanded into make arguments, never shown in process listings (`make -n`, `/proc`), and the proxy never writes the secret to disk or to any captured log; only the reference name (`env:VAR`) appears in startup logs and in `project.mk`.
Refresh the fixtures from the captured bytes, add the extension to
the strict wire shadows alongside its siblings, and extend the corpus
test — the regression harness then holds the shape permanently.

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
