// Package transcode implements HTTP request/response transcoding between the
// OpenAI Responses API, the OpenAI Chat Completions API, and the Anthropic
// Messages API.
//
// The transcoder is native-first and strict:
//
//   - Client-facing protocols are OpenAI Responses and Anthropic Messages
//     only. Chat Completions exists only as an upstream compatibility target
//     for providers that expose no preferable API.
//   - Every source payload decodes exactly once into a small canonical IR and
//     every target renders directly from that IR. Wire serializers are never
//     chained through another dialect.
//   - The supported surface is a strict subset. Every unsupported field or
//     variant produces a typed error in the client dialect; nothing is
//     silently dropped, defaulted, merged, or reinterpreted. The modeled
//     opaque provider extensions of the Chat dialect — off-schema fields
//     real gateways emit, inventoried with their decode fates in pins.md's
//     "Modeled opaque provider extensions" table — are a deliberate,
//     documented exception to strictness, pinned by committed unit tests
//     and, for the spellings that caused live field regressions, replayed
//     by the field-capture corpus (testcorpus/testdata/field/).
//   - Anthropic thinking blocks are preserved byte-for-byte only for the same
//     protocol; they are never synthesized, and OpenAI reasoning is never
//     relabeled as authenticated thinking.
//   - The Responses stream uses event-specific SSE types that emit every
//     required field with the SSE event name equal to the JSON type.
//   - Cross-provider credentials are stripped before the target
//     authentication policy is applied.
//   - Streaming copy and cancellation use stream.Proxy from
//     github.com/joeycumines/sesame/stream; the handler owns downstream
//     sealing, upstream body closure, and outcome classification around it.
//
// # Compiler pipeline
//
// A transcoded exchange is a one-directional compile: decode source wire →
// source-aware semantic IR → render target wire. The IR is never a wire
// intermediate — JSON is never re-encoded into a second dialect through a
// generic envelope. Request conversion (convert_request.go) and response
// conversion (convert_response.go) each preserve turn boundaries, content
// order, tool-call identity, and tool-result identity; the source's raw
// model-generated tool arguments survive byte-exact. Every unsupported field
// or variant produces a client-dialect error under the configured loss
// policy — nothing is silently dropped, defaulted, merged, or reinterpreted.
//
// # Semantic matrix
//
// Every canonical field and source artifact is classified per target dialect
// (review-j finding 10). New fields must be placed in exactly one row below
// before being used in a renderer:
//
//   - exact: the value crosses protocols unchanged;
//   - transformed: the value is mapped to an equivalent form without losing
//     meaning (e.g. stop reasons, tool-call identity);
//   - loss-gated: the value is dropped, and the drop is approved by the
//     exchange LossPolicy or rejected (the Feature names the decision);
//   - unsupported: the value always produces a client-dialect error.
//
// Canonical request fields:
//
//	ClientModel, Turns, Tools, ToolChoice, MaxOutputTokens, Temperature,
//	TopP, Metadata        exact (rendered per target)
//	ParallelTools         transformed (per-target capability gate)
//	StopSequences         loss-gated (FeatureStopSequences; Responses has none)
//	StructuredOutput      transformed (per-target capability gate)
//	Stream                exact
//	FunctionResult.IsError loss-gated (FeatureToolResultErrorStatus; permissive
//	                       encoding: visible error_status_prefix text)
//	Reasoning.Effort      transformed (Chat reasoning_effort, capability gate)
//	Reasoning.Summary     loss-gated (FeatureReasoningSummary; Chat has
//	                       no summary style)
//	System prompt parts   transformed for text-only prompts into the
//	                       string-only create-request instructions;
//	                       multiple system turns and non-text system content
//	                       are loss-gated (FeatureMultipleSystemTurns,
//	                       FeatureSystemNonTextContent); for Chat targets
//	                       system-channel turns consolidate into one
//	                       leading system message — a turn after dialog
//	                       turns is loss-gated (FeatureMidConversation
//	                       System) and a leading-only merge is a sanctioned
//	                       note under the same key; the system_anywhere
//	                       capability restores positional rendering
//	Thinking blocks       exact only for Messages targets; loss-gated
//	                       (FeatureAuthenticatedThinking) otherwise
//	Reasoning items       exact only for Responses targets; loss-gated
//	                       (FeatureReasoningSummary) otherwise
//	Input item identity   dropped unconditionally; the drop is noted
//	                       observably under FeaturePreviousResponseID (input
//	                       item ids are Responses conversation-state
//	                       references; no target dialect carries them)
//	Images, Documents     transformed (per-target capability gates)
//	Developer role        transformed (capability gate; system fallback)
//
// Canonical response fields:
//
//	ID, Model, CreatedAt, Status, Stop, Items      exact/transformed
//	Reasoning items       loss-gated (FeatureReasoningSummary; Messages targets)
//	Conversation state    loss-gated (FeatureOutputItemBoundaries; Messages
//	                       responses cannot carry tool results)
//	Output message phase  loss-gated (FeatureOutputPhase; Messages has no phase;
//	                       the stream gates each phase-bearing item exactly
//	                       once; input-message phases are loss-gated at
//	                       decode)
//	Usage                 transformed with checked arithmetic; each unknown
//	                       component is loss-gated (FeatureUsageUnknown,
//	                       FeatureUsageCacheReadUnknown,
//	                       FeatureUsageCacheWriteUnknown,
//	                       FeatureUsageReasoningUnknown)
//	Chat logprobs         loss-gated (FeatureLogprobs)
//	Chat service tier     loss-gated (FeatureResponseServiceTier)
//	Envelope controls     loss-gated (FeatureResponsesControls; the
//	                       background/max_tool_calls/prompt/prompt_cache_key/
//	                       safety_identifier controls echoed on a Responses
//	                       upstream cannot be reproduced in Messages; failed
//	                       envelopes surface as client-dialect errors and
//	                       never reach this decision)
//	Provider reasoning    capability-gated (ProviderReasoningText — the chat
//	                       `reasoning` / DeepSeek-Qwen `reasoning_content`
//	                       response extension — maps to ordinary text with
//	                       the named provider_reasoning_text encoding,
//	                       reported via ConversionReport in the streaming
//	                       path)
//
// # Failure taxonomy
//
// Every exchange records exactly one typed outcome (ExchangeProvenance, in
// outcome.go), and the breaker classification uses only that outcome:
//
//   - upstream_http, upstream_body_error, upstream_transport_error: upstream
//     failure (breaker penalty, slot hold)
//   - local_request_conversion_error, local_response_conversion_error,
//     local_stream_validation_error: local failure (never breaker-relevant)
//   - client_abort: neither success nor upstream failure unless a definitive
//     upstream failure already exists
//   - downstream_write_error: local, downstream delivery failed
//
// Invalid model-generated tool arguments are local unrepresentable output
// when the target requires an object. Malformed source wire (strict decode
// violations, lifecycle contradictions, contract-violating usage totals) is
// corrupt upstream wire: an upstream failure. Unsupported-but-valid source
// features are local conversion errors.
//
// # Outcome accounting
//
// The per-request OutcomeSink records exactly once (a handler defer records
// an internal local-failure outcome if no path recorded one; the proxy panics
// if the outcome is missing — an invariant violation). Retry-After is
// anchored at the original upstream header receipt (Set=true whenever the
// upstream signaled a hold, even when already expired); the translated
// downstream header is never re-parsed. Attempt facts are explicit: a local
// request-conversion or signing error never reads as upstream-attempted, so
// no cancel-cooldown hold fires for an upstream that was never contacted.
//
// # Retry/signing order
//
// retry.Transport (buffers and replays bodies) wraps the attempt-marker
// transport, which wraps SigningTransport, which wraps the base transport.
// Each actual attempt is therefore: rebuild body → finalize Content-Length →
// sign → send. Signatures are never reused across attempts and the original
// request is never mutated. Signer failures are typed non-retryable local
// defects: never retried, never recorded as breaker failures, never masked
// as context cancellations, and sanitized for the client (detail goes to the
// log).
//
// # Stream FSM
//
// Every supported stream event (chat chunks, responses events, anthropic
// events) passes through one lifecycle state machine per direction
// (responses_stream_fsm.go, the direction converters in stream_converters.go):
// duplicate and post-done events are rejected, a terminal requires every child
// lifecycle closed, output indexes are unique, terminal snapshots reconcile
// with the incremental observation, and total event/item/part/state/byte
// budgets bound the whole exchange. Converted output commits in atomic staged
// terminal batches; the stream is translated incrementally through
// stream.Proxy (mandatory copy/cancellation boundary) and a stream that ends
// before a terminal condition emits a client-dialect error event — never a
// silent clean EOF. A failed, malformed, truncated, or cancelled exchange is
// never reported as a successful model completion.
//
// # Wire pins and the loss matrix
//
// contracts.lock.json is the authoritative contract registry; pins.md is
// generated from it (go generate ./internal/transcode) and drift-tested.
// LOSS_MATRIX.md is generated from the same loss-key registry the program
// uses (gen/lossmatrix), so code and documentation cannot drift.
//
// Authoritative contracts:
// https://platform.openai.com/docs/api-reference/responses
// https://platform.openai.com/docs/api-reference/chat
// https://platform.claude.com/docs/en/api/messages
// https://platform.claude.com/docs/en/build-with-claude/streaming
package transcode
