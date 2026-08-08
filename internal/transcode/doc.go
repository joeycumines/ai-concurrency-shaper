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
//     silently dropped, defaulted, merged, or reinterpreted.
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
//	FunctionResult.IsError loss-gated (FeatureToolResultError; permissive
//	                       encoding: visible error_status_prefix text)
//	Reasoning.Effort      transformed (Chat reasoning_effort, capability gate)
//	Reasoning.Summary     loss-gated (FeatureReasoningSummaryRequest; Chat has
//	                       no summary style)
//	Thinking blocks       exact only for Messages targets; loss-gated
//	                       (FeatureAuthenticatedThinking) otherwise
//	Reasoning items       exact only for Responses targets; loss-gated
//	                       (FeatureReasoningSummary) otherwise
//	Images, Documents     transformed (per-target capability gates)
//	Developer role        transformed (capability gate; system fallback)
//
// Canonical response fields:
//
//	ID, Model, CreatedAt, Status, Turns, StopReason   exact/transformed
//	ReasoningItems        loss-gated (FeatureReasoningSummary; Messages targets)
//	Conversation state    loss-gated (FeatureConversationState; Messages
//	                       responses cannot carry tool results)
//	Output message phase  loss-gated (FeaturePhase; Messages has no phase;
//	                       the stream gates each phase-bearing item exactly
//	                       once; input-message phases are loss-gated at
//	                       decode)
//	Usage                 transformed with checked arithmetic; unknown usage
//	                       is loss-gated (FeatureUsageTiming)
//	Chat logprobs         loss-gated (FeatureLogprobs)
//	Chat service tier     loss-gated (FeatureServiceTier)
//	Provider reasoning    capability-gated (ProviderReasoningText maps to
//	                       ordinary text with the named provider_reasoning_text
//	                       encoding, reported via ConversionReport in the
//	                       streaming path)
//
// Authoritative contracts:
// https://platform.openai.com/docs/api-reference/responses
// https://platform.openai.com/docs/api-reference/chat
// https://platform.claude.com/docs/en/api/messages
// https://platform.claude.com/docs/en/build-with-claude/streaming
package transcode
