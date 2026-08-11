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
| OpenAI Responses | openai-go | v1.12.0 | `bbd67c90691efe37097affc8cd3200b2360f14426ae0d60b2d59e444a7f87186` |
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
   `internal/transcode/*.go` and `internal/transcode/wire/` (strict decoders
   reject unknown fields, so a bump that adds official fields is a breaking
   step that must land with its schema and fixture updates in the same commit).
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

NOT in v1.12.0 (present in later revisions only; a strict decode of a newer
upstream response carrying them is an unknown-field failure until the pin is
bumped): `completed_at`, `conversation`, `moderation`, `store`.

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
