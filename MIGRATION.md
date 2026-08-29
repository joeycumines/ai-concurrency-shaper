# Migration guide

How the transcoding feature set changes from the pre-release shape to the
pinned, granular release shape. The transcoding feature is unreleased: there
are **no deprecated aliases** and **no startup expansion logs**. A name that
no longer exists is a startup error, full stop.

## Loss names: broad names removed, granular names only

The `-transcode-allow-loss` flag accepts **only the granular registry
names**. The broad, pre-release permission sets (wildcards such as `all`,
`permissive`, and the early coarse names that authorized unrelated drops)
are **removed**, not aliased. Passing one of them fails startup:

```sh
$ ai-concurrency-shaper ... -transcode-allow-loss all
invalid -transcode-allow-loss: unknown loss feature "all"
```

The granular names (the complete, ordered registry is
`internal/transcode/LOSS_MATRIX.md`, generated from the same registry the
program uses):

| Loss key | What losing it authorizes |
| --- | --- |
| `previous_response_id` | Responses previous-response conversation state (input item ids are dropped unconditionally and noted observably under this key) |
| `request_top_logprobs` | The Responses `top_logprobs` request field |
| `request_service_tier` | The Responses `service_tier` request field |
| `request_truncation` | The Responses `truncation` request field |
| `multiple_system_turns` | Multiple system turns collapsed into one shape |
| `system_non_text_content` | Non-text system prompt content |
| `mid_conversation_system` | Mid-conversation system turns cannot keep their position in a chat request; under this permission system-channel turns consolidate into one leading system message (position/timing lost, content and authority preserved), and leading-only consolidation of multiple system turns is a sanctioned note under the same key. The `system_anywhere` chat capability restores positional rendering for upstreams that accept system messages anywhere |
| `tool_schema_strictness` | Emitting explicit `strict:false` for source tools without a strictness semantic |
| `tool_result_error_status` | Encoding a tool-result error status into visible content |
| `tool_result_multimodal_content` | Encoding multimodal tool results as a JSON text envelope |
| `output_item_boundaries` | Merging or dropping output item boundaries |
| `output_phase` | Dropping the commentary/final-answer phase distinction |
| `usage_unknown` | Rendering required usage when the source provided none |
| `usage_cache_read_unknown` | Rendering the required cache-read breakdown when unknown |
| `usage_cache_write_unknown` | Rendering the required cache-creation breakdown when unknown |
| `usage_reasoning_unknown` | Rendering the required reasoning breakdown when unknown |
| `provider_reasoning_text` | Mapping provider reasoning text to ordinary text |
| `request_reasoning` | Dropping a request-side reasoning control (an Anthropic `thinking` budget or a Responses `reasoning.effort`) the target cannot reproduce |
| `reasoning_summary` | Dropping reasoning summaries |
| `tool_result_json_envelope` | The sanctioned `transcode_version 1` multimodal encoding |
| `developer_role` | Collapsing the developer role |
| `structured_output` | Dropping an unrepresentable structured-output format |
| `parallel_tool_calls` | Dropping the parallel-tool-calls setting |
| `stop_sequences` | Dropping stop sequences |
| `image_input` | Dropping image input |
| `document_input` | Dropping document input |
| `authenticated_thinking` | Dropping Anthropic thinking/signature blocks across protocols |
| `top_k` | Dropping the `top_k` setting |
| `logprobs` | Dropping response token log-probabilities |
| `responses_controls` | Tolerating Responses envelope controls observably: noting `include`/`client_metadata`, dropping `prompt_cache_key` on requests, and dropping the controls echoed on Responses upstream responses (the conversation-state request controls `background`/`max_tool_calls`/`prompt`/`safety_identifier`/`status` are errors under every policy) |
| `anthropic_controls` | Dropping the Anthropic Messages client-side envelope controls (`context_management`, `output_config`) |
| `builtin_tools` | Responses built-in tools (web_search, file_search, code_interpreter, computer_use, and other non-function tool types) cannot be reproduced in a chat request; an approved loss drops them, and a tool_choice the drop leaves dangling is reconciled (auto drops with a note, required and named references reject) |
| `response_service_tier` | Dropping the upstream tier actually served |

## Behavior changes

1. **Strict is the default, and it is strict.** The zero-value loss policy
   (no `-transcode-allow-loss` flags) rejects every non-portable feature
   with a client-dialect error. Messages-client streaming, for example,
   requires `usage_unknown` (the source provides no usage until the
   terminal, and the Messages wire requires usage on `message_start`).
2. **Messages-to-Responses mappings require the strictness loss.** Messages
   tools carry no strictness semantic, and the Responses function-tool
   contract requires an explicit `strict` field. Under the strict policy a
   Messages-to-Responses mapping can never serve a tool-carrying request,
   so the configuration is now **rejected at startup**. Enable it with
   `-transcode-allow-loss tool_schema_strictness`.
3. **Usage totals must be exact.** A source whose `total_tokens` is not
   exactly `input + output` (chat and responses contracts) is corrupt
   upstream wire: the exchange fails as an upstream failure, never as a
   silent re-total. Absent usage is still never fabricated as zero.
4. **Outcomes are exact and per-request.** Every transcoded exchange
   records exactly one outcome; the breaker classification comes from that
   outcome alone. Retry-After is anchored at the original upstream header
   receipt; a translated downstream header is never re-parsed.
5. **Per-attempt signing.** An external signer (programmatic API) signs
   every actual retry attempt after body reconstruction and
   Content-Length finalization. A signer failure is a local error: never
   retried, never a breaker failure, and never reported to the client with
   detail.
6. **Exchange IDs carry 128 random bits.** The per-exchange ID prefix is
   now 32 lowercase hex characters from `crypto/rand`; collisions across
   unrelated exchanges are cryptographically negligible while ordering
   within one exchange stays deterministic.
7. **Impossible configurations fail at startup**, never on the first
   request: a Messages-to-Responses mapping under strict policy, a model
   map with no resolution policy, invalid or pipeline-reserved custom auth
   headers, output limits smaller than the minimum legal terminal or error
   frame, duplicate client routes, and a signer configuration that cannot
   replay request bodies.

## Rollout procedure

1. Upgrade the binary.
2. Replace any pre-release broad loss name with the granular keys it
   implied (see the table above). Unknown names fail startup with the exact
   offending value.
3. If you run a Messages-to-Responses mapping, add
   `-transcode-allow-loss tool_schema_strictness`.
4. Run `-version` and a config-only smoke start against a non-production
   upstream before switching traffic.
5. The failure taxonomy is unchanged in the client-visible direction:
   unsupported-but-valid source features are local conversion errors;
   malformed source wire and source body failures are upstream failures;
   invalid model-generated tool arguments are local unrepresentable output
   when the target requires an object; client aborts are neither success
   nor upstream failure unless a definitive upstream failure already
   exists.
