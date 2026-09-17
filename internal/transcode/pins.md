# Wire contract pins

Authority for every schema decision in this package. The authoritative
registry is `contracts.lock.json` — the pinned-revisions table below is
generated from it (`go generate ./internal/transcode`, see contracts.go) and
must never be maintained by hand. Any claim about an official wire shape must
trace to these pinned revisions. Unpinned "current contract" claims are not
normative; when live documentation disagrees with the pin, the pin wins until
the pin is deliberately bumped (see below).

## Pinned revisions

| Protocol | Source | Version | Snapshot SHA256 |
| --- | --- | --- | --- |
| OpenAI Responses | openai-go | v1.12.0 | `26da47966721b5a2614b656439970f6e67cce6bcef04175b0d6c6291a97f5fce` |
| OpenAI Chat Completions | openai-go | v1.12.0 | `bde5294743898ff93efee09fe80eb59c07599c252cb591d532205fbde1aeb53c` |
| Anthropic Messages | anthropic-api | 2023-06-01 | `7904599c41df8745b57614322d73e970c8bf54bb48563829d8ec3a2381920ce5` |

Generated from `contracts.lock.json` (authoritative) by `go generate` — see
contracts.go. Do not edit this table by hand. The schema inventories below
are the checked-in schema detail; their integrity is covered by the snapshot
hashes (drift test in contracts_test.go).

## Update procedure (governance, review-j finding 17)

1. Bump the revision in `contracts.lock.json` (the authoritative registry)
   and in the disposable wirecheck module (`go get ...@vX.Y.Z`).
2. Re-run `extract.go` for the affected files and regenerate this document's
   inventories.
3. Update the affected inventory section (its content is covered by the
   lock's `snapshot_sha256` — a change without a deliberate pin bump fails
   the drift test in contracts_test.go).
4. Run `go generate ./internal/transcode` to regenerate the pinned-revisions
   table from the lock.
5. Review the schema diff: every added/removed/changed field must be traced to
   `internal/transcode/*.go` and `internal/transcode/wire/` (the client
   contract and the content-block unions are strict, so a bump that adds
   official client-side fields is a breaking step that must land with its
   schema and fixture updates in the same commit; the upstream envelope is
   tolerant).
6. Record the bump in `contracts.lock.json`, this file, and `blueprint.json`
   goalLog.

## OpenAI Chat Completions inventory (chatcompletion.go, v1.12.0)

Required fields are marked `required`; `nullable` fields are always present on
the wire and may be null.

### ChatCompletion (non-stream response envelope)

```
id,required             string
choices,required        []ChatCompletionChoice
created,required        int64
model,required          string
object,required         constant.ChatCompletion
service_tier,nullable   ChatCompletionServiceTier
system_fingerprint      string
usage                   CompletionUsage
```

### ChatCompletionChoice

```
finish_reason,required  string
index,required          int64
logprobs,required       ChatCompletionChoiceLogprobs
message,required        ChatCompletionMessage
```

`ChatCompletionChoiceLogprobs`: `content,required []ChatCompletionChoiceLogprob`,
`refusal,required []ChatCompletionChoiceLogprob`.

### ChatCompletionMessage

```
content,required        string
refusal,required        string
role,required           constant.Assistant
annotations             []
audio,nullable          ChatCompletionAudio
function_call           ChatCompletionMessageFunctionCall
tool_calls              []ChatCompletionMessageToolCall
```

### ChatCompletionMessageToolCall (non-stream tool call — NO index field)

```
id,required             string
function,required       ChatCompletionMessageToolCallFunction
type,required           constant.Function
```

`ChatCompletionMessageToolCallFunction`: `arguments,required string`,
`name,required string`.

### ChatCompletionMessageParam (request message union)

Arms: `ChatCompletionDeveloperMessageParam`, `ChatCompletionSystemMessageParam`,
`ChatCompletionUserMessageParam`, `ChatCompletionAssistantMessageParam`,
`ChatCompletionToolMessageParam`, `ChatCompletionFunctionMessageParam`.

- Assistant param: `content ChatCompletionAssistantMessageParamContentUnion`
  (null | string | array), `tool_calls []ChatCompletionMessageToolCallParam`
  — `ChatCompletionMessageToolCallParam` has `id,required`,
  `function,required`, `type,required` (NO index).
- Tool param: `tool_call_id,required string`.
- Developer param: `content,required` union; `name`.

### CompletionUsage (shared non-stream + stream chunk usage)

```
completion_tokens,required    int64
prompt_tokens,required        int64
total_tokens,required         int64
completion_tokens_details     CompletionUsageCompletionTokensDetails
prompt_tokens_details         CompletionUsagePromptTokensDetails
```

### ChatCompletionChunk (streaming chunk envelope)

```
id,required             string
choices,required        []ChatCompletionChunkChoice
created,required        int64
model,required          string
object,required         constant.ChatCompletionChunk
service_tier,nullable   ChatCompletionChunkServiceTier
system_fingerprint      string
usage,nullable          CompletionUsage
```

### ChatCompletionChunkChoice

```
delta,required          ChatCompletionChunkChoiceDelta
finish_reason,required  string
index,required          int64
logprobs,nullable       ChatCompletionChunkChoiceLogprobs
```

### ChatCompletionChunkChoiceDelta

```
content,nullable        string
function_call           ChatCompletionChunkChoiceDeltaFunctionCall
refusal,nullable        string
role                    string
tool_calls              []ChatCompletionChunkChoiceDeltaToolCall
```

### ChatCompletionChunkChoiceDeltaToolCall (streaming tool-call fragment — index REQUIRED)

```
index,required          int64
id                      string
function                ChatCompletionChunkChoiceDeltaToolCallFunction
type                    string
```

`ChatCompletionChunkChoiceDeltaToolCallFunction`: `arguments string`,
`name string` (both optional — partial deltas).

## OpenAI Responses inventory (responses/response.go, v1.12.0)

### Response (response envelope)

```
id,required               string
created_at,required       float64
error,required            ResponseError
incomplete_details,required ResponseIncompleteDetails
instructions,required     ResponseInstructionsUnion   (string | []ResponseInputItemUnion)
metadata,required         shared.Metadata
model,required            shared.ResponsesModel
object,required           constant.Response
output,required           []ResponseOutputItemUnion
parallel_tool_calls,required bool
temperature,required      float64
tool_choice,required      ResponseToolChoiceUnion
tools,required            []ToolUnion
top_p,required            float64
background,nullable       bool
max_output_tokens,nullable int64
max_tool_calls,nullable   int64
previous_response_id,nullable string
prompt,nullable           ResponsePrompt
prompt_cache_key          string
reasoning,nullable        shared.Reasoning
safety_identifier         string
service_tier,nullable     ResponseServiceTier
status                    ResponseStatus
text                      ResponseTextConfig
top_logprobs,nullable     int64
truncation,nullable       ResponseTruncation
usage                     ResponseUsage
user                      string
```

NOT in v1.12.0 (present in later revisions only; the tolerant upstream
envelope decode accepts them — they are discarded, never forwarded — and a
pin bump is needed only to MODEL/MAP them, not to avoid a failure):
`completed_at`, `conversation`, `moderation`, `store`.

### ResponseUsage

```
input_tokens,required          int64
input_tokens_details,required  ResponseUsageInputTokensDetails
output_tokens,required         int64
output_tokens_details,required ResponseUsageOutputTokensDetails
total_tokens,required          int64
```

`ResponseUsageInputTokensDetails`: `cached_tokens,required int64`.
`ResponseUsageOutputTokensDetails`: `reasoning_tokens,required int64`.
NOT in v1.12.0: `cache_write_tokens`.

### ResponseError

```
code,required    ResponseErrorCode
message,required string
```

### ResponseIncompleteDetails

```
reason    string
```

### ResponseNewParams (create-request)

`instructions param.Opt[string]` — the create-request instructions is a
**string**. The response echo may be the string-or-item-list union
(`ResponseInstructionsUnion`), but the outbound create request must never
emit an array.

Other create-request fields: `background`, `max_output_tokens`,
`max_tool_calls`, `parallel_tool_calls`, `previous_response_id`, `store`,
`temperature`, `top_logprobs`, `top_p`, `prompt_cache_key`,
`safety_identifier`, `user`, `include`, `metadata`, `prompt`,
`service_tier`, `truncation`, `input` (union), `model`, `reasoning`,
`text`, `tool_choice` (union), `tools`.

### Stream events (ResponseStreamEventUnion members)

All 20 events carry `type` and `sequence_number` (required):

```
response.created                 Response
response.in_progress             Response
response.completed               Response
response.incomplete              Response
response.failed                  Response
error                            code, message, param (all required)
response.output_item.added       item (union), output_index
response.output_item.done        item (union), output_index
response.content_part.added      item_id, output_index, content_index, part (union)
response.content_part.done       item_id, output_index, content_index, part (union)
response.output_text.delta       item_id, output_index, content_index, delta, logprobs
response.output_text.done        item_id, output_index, content_index, text, logprobs
response.function_call_arguments.delta  item_id, output_index, delta
response.function_call_arguments.done   item_id, output_index, arguments (no name)
response.refusal.delta           item_id, output_index, content_index, delta
response.refusal.done            item_id, output_index, content_index, refusal
response.reasoning_summary_part.added   item_id, output_index, summary_index, part
response.reasoning_summary_part.done    item_id, output_index, summary_index, part
response.reasoning_summary_text.delta   item_id, output_index, summary_index, delta
response.reasoning_summary_text.done    item_id, output_index, summary_index, text
```

### ResponseOutputMessage

```
id,required        string
content,required   []ResponseOutputContentPartUnion
role,required      constant.Assistant
status,required    ResponseOutputMessageStatus
type,required      constant.Message
```

NOT in v1.12.0: `phase`. (Current internal model accepts `phase` as a shadow
field; it is not part of the pinned contract.)

### ResponseOutputText

```
annotations,required  []ResponseOutputTextAnnotation
text,required         string
type,required         constant.OutputText
logprobs              []
```

### ResponseOutputRefusal

```
refusal,required  string
type,required     constant.Refusal
```

### ResponseFunctionToolCall

```
arguments,required  string
call_id,required    string
name,required       string
type,required       constant.FunctionCall
id                  string
status              ResponseFunctionToolCallStatus
```

### ResponseInstructionsUnion

```
OfString        string
OfInputItemList []ResponseInputItemUnion
```

## Responses namespace tools (modeled extension beyond the pin)

The v1.12.0 Responses pin predates the namespace-tool surface, so the
following shapes are deliberate extensions, modeled from the current official
contract and a real Codex CLI 0.154.0 capture (the transformer's client
contract is strict, so an unmodeled field would reject the request):

- A tool of `type:"namespace"` groups nested function tools
  (`{"type":"namespace","name":...,"description":...,"tools":[...]}`); each
  child carries `name`, `description`, `strict`, and an object `parameters`
  schema. Namespace children are flattened into flat chat function tools; the
  grouping itself is client-side structure the chat dialect cannot express and
  is recorded as a note.
- `function_call` output items, replayed `function_call` input items, and
  `function_call_output` input items carry an optional `namespace` field
  alongside the bare `name`. The qualifier is always this separate field; it
  is never concatenated into the name.
- The per-exchange flattening map (flat name to namespace + child) is the only
  reverse lookup, so no separator is ever parsed out of a name. A child whose
  bare name collides with a plain function tool or another namespace child is
  qualified deterministically with `namespace + "__" + child`; when that
  qualified name is itself already taken (a plain tool may legally be spelled
  that way), a numeric suffix (`namespace__child_2`, `_3`, ...) keeps every
  mapping invertible.
- `tool_choice` has no namespaced selector in any inspected contract; the
  flattened bare (or qualified) name is what a named choice addresses.

Strictness is unchanged: these fields are modeled, and any other unknown field
on the client contract still rejects.

## Responses reasoning-item routing marker (modeled extension beyond the pin)

A gateway serving the native Responses API may attach a `format` routing
marker to reasoning output items (observed live 2026-09-17 on the camel
mount: `"format":"azure-openai-responses-v1"` alongside `id`, `type`,
`status`, `summary`, `encrypted_content`). The marker names the gateway's
own response dialect, not model output: it is decoded into
`ReasoningOutputItem.Format` as opaque raw JSON, stripped before the item
enters the canonical bytes, and never forwarded into any client dialect
(the strict output-item union would otherwise fail the exchange on it).
Evidence: the exhibiting flow record
`scratch/flowlogs/000010-17635-POST-v1_messages.json` (upstream model
`openai/gpt-5.6-luna`; flow records are git-ignored scratch, so the durable
evidence is the committed fixture
`testcorpus/testdata/field/camel_reasoning_format_field.json`, accessor
`FieldCamelReasoningFormatJSON` in `testcorpus.go`).

## Anthropic Messages inventory (message.go, v1.61.0)

### Message (non-stream response + message_start payload)

```
id,required             string
container,required      Container
content,required        []ContentBlock
model,required          Model
role,required           constant.Assistant (default "assistant")
stop_details,required   RefusalStopDetails
stop_reason,required    StopReason        (nullable on the wire: null before completion)
stop_sequence,required  string            (nullable on the wire)
type,required           constant.Message (default "message")
usage,required          Usage
```

`stop_reason` and `stop_sequence` are REQUIRED fields on the wire: they are
always present and are `null` when not applicable. `message_start` carries
`stop_reason: null`; the real value appears only in `message_delta`.

### Usage

```
cache_creation,required            CacheCreation
cache_creation_input_tokens,required int64
cache_read_input_tokens,required   int64
inference_geo,required             string
input_tokens,required              int64
output_tokens,required             int64
output_tokens_details,required     OutputTokensDetails
server_tool_use,required           ServerToolUsage
service_tier,required              UsageServiceTier
```

Anthropic total input semantics: `input_tokens + cache_creation_input_tokens +
cache_read_input_tokens` (cached tokens are a breakdown of the total, never
additive on top of it).

### MessageStartEvent

```
message,required  Message
type,required     constant.MessageStart (default "message_start")
```

### MessageDeltaEvent

```
delta,required  MessageDeltaEventDelta   {container, stop_details, stop_reason, stop_sequence — all required}
type,required   constant.MessageDelta (default "message_delta")
usage,required  MessageDeltaUsage     {cache_creation_input_tokens, cache_read_input_tokens,
                                       input_tokens, output_tokens, output_tokens_details, server_tool_use}
```

### MessageStopEvent

```
type,required  constant.MessageStop (default "message_stop")
```

### ContentBlock events

```
content_block_start  index,required; content_block (union),required
content_block_delta  index,required; delta (union),required
content_block_stop   index,required
```

## Anthropic server-side tools and content (modeled extension beyond the pin)

The v1.61.0 pin predates the server-tool surface named here, so the
following shapes are deliberate extensions, modeled from the official
contract and a real Claude Code 2.1.273 capture. Every shape is admitted
on the wire and decided under the `anthropic_server_tools` loss key (a
chat upstream executes no server tools): approved, it drops observably;
rejected, the request fails with the keyed error — never an unattributed
unknown-type rejection.

- Type-discriminated `tools[]` definitions (observed live 2026-09-17:
  `{"type":"web_search_20250305","name":"web_search","max_uses":8}`):
  admitted with raw params preserved (`Tool.Type` + `Tool.ServerParams`),
  dropped under the key. Fixture:
  `testcorpus/testdata/field/claude_server_tool_definition_field.json`
  (accessor `FieldClaudeServerToolDefinitionJSON`).
- Content blocks `server_tool_use`, `web_search_tool_result`,
  `code_execution`, `code_execution_tool_result`, `container_upload`:
  admitted with raw bytes preserved (`ContentBlock.ServerContent`),
  dropped under the key. A fabricated function call would dangle with no
  upstream executor, so mapping is refused by design.
- Content blocks `mcp_tool_use` / `mcp_tool_result`: client-side tools
  under a server spelling, carrying the `tool_use` / `tool_result`
  fields exactly — mapped 1:1 onto the canonical function call/result
  with no loss key.

The key is strict by default: a session carrying server tools fails with
the keyed error until the operator passes
`-transcode-allow-loss anthropic_server_tools`.

## Model-vs-pin deltas (as of cycle J, task J11)

Implemented J4/J5/J6/J7/J11:

- Chat: logprobs/service_tier/usage details modeled; tool-call wire types
  split (non-stream no index, streaming index required); stream lifecycle
  phases with the usage tail; include_usage requested.
- Responses envelope: background, max_tool_calls, prompt (typed
  ResponsesEnvelopePrompt), prompt_cache_key, safety_identifier modeled as
  typed shadows entering the FeatureResponsesControls loss/reject decision.
  NOT modeled because they are absent from v1.12.0: completed_at,
  conversation, moderation (pin governs; a pin bump is the reviewable step
  that introduces them).
- Create-request instructions is a plain string; the response echo renders
  the string arm of the ResponseInstructionsUnion; multi-part system prompts
  are loss-gated, never an items array.
- Anthropic message_start serializes null stop fields; usage uses checked
  nonnegative arithmetic with FeatureUsageTiming for unknown early usage.

## Pending deltas (as of cycle J, task J11)

All deltas identified at J1 are implemented (J4-J11); see the implemented
section above. Future pin bumps must re-run the inventory extraction and
review the schema diff per the update procedure.

## Modeled opaque provider extensions

The pins above cover the official schemas. Real gateways additionally
emit fields outside them; the wire shadows MODEL every observed spelling so
its presence is an observed inert extension. The table below lists every
chat-dialect spelling the wire shadows model,
each with its fate after decode, and each is pinned by a committed unit test
(spread across
`chat_schema_test.go`, `chat_response_strict_test.go`,
`chat_stream_strict_test.go`, `chat_reasoning_content_test.go`, and
`modern_client_test.go`). The field-capture corpus
(`testcorpus/testdata/field/`, exercised by
`field_capture_replay_test.go` through the production decode functions)
replays the subset that caused live field regressions, plus several sibling
spellings that ride the same captures; the remaining spellings are pinned by
those unit tests.

| Placement | Extension | Fate |
| --- | --- | --- |
| chat envelope (stream + non-stream) | `prompt_token_ids`, `prompt_text`, `cache_cost`, `completion_cost` | inert — decoded, never forwarded |
| chat choice | `token_ids`, `routed_experts`, `stop_reason`, `matched_stop` | inert — decoded, never forwarded |
| chat message | `token_ids`, `routed_experts`, `stop_reason`, `matched_stop` (defensive mirror), `reasoning`, `reasoning_content` | `reasoning`/`reasoning_content` map to capability-gated ordinary text; the rest are inert |
| chat stream delta | `reasoning`, `reasoning_content` | capability-gated ordinary text |
| chat usage (top level) | `reasoning_tokens`, `cached_tokens`, `prompt_cache_hit_tokens`, `prompt_cache_miss_tokens` | mapped to canonical usage (`CacheRead`, `ReasoningTokens`); `prompt_cache_miss_tokens` has no canonical home |
| chat usage (top level) | `cache_cost`, `completion_cost` | inert — decoded, never forwarded |
| chat `prompt_tokens_details` | `created_cache_tokens`, `multimodal_tokens` | `created_cache_tokens` maps to canonical `CacheWrite`; `multimodal_tokens` is inert |

None of these spellings is forwarded or re-rendered verbatim. Several
feed the canonical model instead: `reasoning`/`reasoning_content` become
capability-gated ordinary text, and the usage spellings become canonical
usage breakdowns (`CacheRead`, `CacheWrite`, `ReasoningTokens`).

With the `provider_reasoning_thinking` capability the same
`reasoning`/`reasoning_content` spellings render as NATIVE Anthropic
thinking blocks instead of ordinary text: each block is synthesized with
the proxy's marker signature (`shaper-synth-thinking-1`, carried by
`SyntheticThinkingSignature`) emitted as the block's `signature_delta`
before `content_block_stop`, and the Messages request path scrubs
marker-signature thinking blocks out of replayed history before any
upstream rendering, so the synthetic signature never reaches an upstream
(THINK-2, operator-adjudicated 2026-09-07). `redacted_thinking` is never
synthesized — no source data exists for it. Without the capability the
ordinary-text mapping applies (or the documented loss under a strict
policy).

### Known-but-unmodeled, tolerated provider extensions

The table above models the spellings the wire shadows deliberately capture.
Real gateways also emit other opaque fields that the tolerant upstream
envelope DISCARDS (never a failure, never forwarded). These are observed in
the wild and are NOT modeled; they are listed here so an operator can
recognize a known-safe extension versus something unexpected. If one becomes
semantically meaningful, model it in the wire shadows + document it in the
table above + add it to the field-capture corpus:

| Provider | Tolerated (discarded) extensions |
| --- | --- |
| OpenRouter / Dialagram | `cost`, `native_finish_reason`, `is_byok`, `cost_details`, `cache_write_tokens`, `video_tokens`, `image_tokens`, `error`; `delta.reasoning_details` (observed on `meta-muse-spark-1.3`, a sibling array of the modeled `reasoning` delta text — discarded, never forwarded, and never treated as output by the stream converter) |
| vLLM | `prompt_logprobs`, `kv_transfer_params`, `ec_transfer_params`, `metrics` |
| DeepSeek / open-weights | `logprobs.reasoning_content` (NOTE: `reasoning_content` at message/delta level IS modeled and maps to capability-gated text — see the table above; only the `logprobs`-nested spelling is discarded) |
| LiteLLM / Verboo | `completion_cost`, `cache_cost` (modeled as opaque raw JSON in the table above, never forwarded) |
| Legacy OpenAI (deprecated 2023) | `message.function_call`, `delta.function_call` — the legacy non-`tool_calls` tool-call spelling; a KNOWN official field that maps to one canonical tool call with a synthesized id derived from the response id (recorded as the ungated `legacy_function_call` note), never a silent drop (which would leave a `tool_use` stop reason with no tool call). Modern gateways emit `tool_calls`, which IS modeled |

`completion_cost` and `cache_cost` appear in BOTH the modeled table and this
list because they are modeled as opaque `json.RawMessage` (never forwarded)
while the wider LiteLLM/Verboo surface is tolerated-and-discarded.

### Contract-role strictness on the upstream envelope

The table above documents the KNOWN provider-extension spellings so their
presence is an observed inert extension, not a mystery. The upstream
response ENVELOPE (and the nested choice/message/usage objects) is a
**subject-to-change** contract: the decode there is TOLERANT to unknown
fields (see AGENTS.md 'Contract-role strictness and the directional loss
model'). An extension absent from this table is therefore NOT a failure —
it is skipped (discarded) and never forwarded. The CLIENT REQUEST decode and
the content-block unions stay STRICT: a client-sent unknown field, a text
block carrying `image_url`, or an unknown content-block type is still
rejected. A new provider spelling belongs here, in the wire shadows next to
its siblings, and in the corpus as a fixture — capture real bytes first
(`make field-recapture` in the top-level `project.mk`; see the README section
on provider extensions).

### Post-terminal accounting redelivery (observed stream shape)

Some gateways redeliver the terminal chunk with the usage accounting
piggybacked on it instead of sending the bare `choices: []` usage-only tail
that `stream_options.include_usage` defines (observed live on Dialagram
`meta-muse-spark-1.3`: the finish chunk carries `finish_reason: "tool_calls"`
with a role-only delta, `content: ""` and `reasoning: null`, and is then
repeated on the same single choice with a role-only delta and `content: ""`
plus the `usage` object attached; the official bare usage-only tail is the
common shape, and the accumulate-on-repeat shape is absorbed with or without
a usage object). The redelivery is pure accounting: the
stream converter folds its usage into the terminal envelope, applies the
same loss decisions (service tier, logprobs, unknown usage components), and
emits no events. It is NOT new output — a post-finish chunk carrying
content, reasoning, refusal, tool-call fragments, a non-empty legacy
`function_call` payload, a different `finish_reason`, or more than one
choice remains corrupt upstream wire and is rejected, exactly as before
(the benign empty-string `function_call` fragment is absorbed the same way
it is mid-stream).


### Data-only stream frames (observed stream shape)

Some gateways omit the SSE `event:` name on EVERY frame of a Responses
stream (observed live on the camel mount's native `/v1/responses`: frames
begin with a `: ` comment then carry only `data:` lines; the JSON payloads
carry the full `type` discriminator, e.g.
`{"p":"...","type":"response.created",...}`). The SSE specification makes
the `event` field optional, and the Responses JSON `type` is the
authoritative discriminator, so the converter routes such a frame by its
decoded JSON type and records the provider quirk as the ungated
`missing_event_name` note (once per stream, path `responses[].stream`).
Tolerance is scoped to an ABSENT name only: a PRESENT name that disagrees
with the JSON type remains a typed upstream wire error, exactly as before.
The sanitized bytes of one such capture (the camel native-Responses mount)
are committed at `testcorpus/testdata/field/data_only_responses_stream_field.sse`
and replayed through the production converter by
`TestFieldCaptureDataOnlyResponsesStreamReplays`.

### Tool-message content blocks (observed upstream behaviour)

The pinned Chat contract models the message content union as either a plain
string or an array of content blocks (text / image_url), for ANY role. Real
open-weights gateways differ in whether they accept image parts inside a
`role: "tool"` message: some reject them, others carry them and pass the
image to a vision model (observed live on the dialagram mount, whose
`qwen-3.8-max` answered quadrant colours from an image delivered as multipart
tool-message content). Because the acceptance is a property of the upstream,
not of the pinned wire, the multipart tool-message rendering is gated behind
the opt-in `tool_result_images` capability: without it, multimodal tool-result
content keeps the observable `tool_result_json_envelope` text encoding
(`tool_result_multimodal_content` + `tool_result_json_envelope` losses), which
is the compatible default. A media type outside the Chat image vocabulary is
an encoding error on BOTH paths (the envelope cannot data-URL it either), so
it is never silently dropped.
