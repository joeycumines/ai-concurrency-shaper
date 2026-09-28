# Opencode Zen / Zen Go upstream contract

Observed 2026-09-28 from public endpoints (no credentials involved) plus the
first-party client source. This is the reference the per-model native routing
and the `-opencode` preset are built against. It is tracked so a fresh clone
retains the contract; `blueprint.json` and `WIP.md` are excluded from git by
repo-local `.git/info/exclude`.

## Model discovery

| Request | Result |
| --- | --- |
| `GET https://opencode.ai/zen/v1/models` | `200`, OpenAI list shape, 82 models |
| `GET https://opencode.ai/zen/go/v1/models` | `200`, OpenAI list shape, 43 models |

Discovery needs no credential. Sample Zen ids: `claude-opus-4-8`,
`gpt-5.6-luna`, `gemini-3.8-flash`, `deepseek-v4-flash`, `qwen3.8-max`,
`jev-1.13`, `big-pickle`, plus `*-free` tiers. Sample Go ids: `minimax-m3`,
`kimi-k3`, `qwen3.7-max`, `mimo-v2.5`, `gpt-6-luna`,
`muse-spark-1.3-contributor`.

## Keyless inference

Both surfaces refuse an unauthenticated inference request:

```
POST https://opencode.ai/zen/v1/messages
  {"model":"claude-haiku-4-5","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}
-> 401 {"type":"error","error":{"type":"AuthError","message":"Missing API key."}}

POST https://opencode.ai/zen/go/v1/chat/completions
  {"model":"mimo-v2.5","messages":[{"role":"user","content":"hi"}]}
-> 401 {"type":"error","error":{"type":"AuthError","message":"Missing API key."}}
```

There is no keyless tier to support; authenticated `-auth-source` bearer auth is
the only working shape.

## Native dialect per family

Each model is served on the path its family uses. A client speaking that same
dialect should be forwarded with only the model identifier rewritten
(`-native-route` plus the `via` model-table fact); a client speaking another
dialect is converted by the existing transcode mappings.

| Family | Native path | Example ids |
| --- | --- | --- |
| Responses | `/v1/responses` | `gpt-*`, `grok-*`, `muse-spark-*` |
| Messages | `/v1/messages` | `claude-*`, `qwen3.*-plus`, `qwen3.*-max`, `minimax-m*` |
| Chat completions | `/v1/chat/completions` | `deepseek-*`, `glm-*`, `kimi-*`, `mimo-*`, `big-pickle`, `ling-*`, `nemotron-*` |
| Per-model path | `/v1/models/<id>` | `gemini-*` (deferred: no model in the served table uses it) |
| Custom | `/v1/systemone` | `jev-*` (deferred) |

## Session affinity (x-opencode-session)

OpenCode Go requires a stable per-conversation session value; a request without
it fails (`400 MissingSessionID` / "Model is unavailable" /
"cannot be routed efficiently"). The first-party client sends the session,
affinity, and client-attribution headers together on every request; the
gateway sticks consecutive requests from one session to one backend provider
and strips the `x-opencode-*` headers before forwarding to that backend.

First-party header set (from the pinned client source, `packages/core/src/session/model-request.ts`
on the in-development 2.x line, and the released-stable request-preparation code
on the 1.x line):

```
x-opencode-session:  <session id>
x-opencode-client:   <client name>            (artifact / flag; "cli" for the CLI)
x-opencode-project:  <project id>             (only when the client has one)
x-opencode-request:  <user/request id>        (older clients only; the gateway strips it)
x-parent-session-id: <parent session id>      (only for sub-agent sessions)
User-Agent:          opencode/<version>       (released shape; 2.x adds channel/name)
```

The gateway's own fallback, when the header is absent, is the workspace id or
the client IP. The proxy mirrors that: inbound identity first, then a stable
derived key.

## Claude Code as a gateway client

Claude Code treats `ANTHROPIC_BASE_URL` as an Anthropic Messages gateway:
`POST /v1/messages?beta=true` plus `anthropic-beta` / `anthropic-version`
headers, `x-api-key` or bearer auth, and an optional
`POST /v1/messages/count_tokens` probe. When that probe is absent or fails,
Claude Code estimates context locally rather than erroring — so a
dialect-shaped answer is sufficient.
