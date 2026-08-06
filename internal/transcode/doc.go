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
// Authoritative contracts:
// https://platform.openai.com/docs/api-reference/responses
// https://platform.openai.com/docs/api-reference/chat
// https://platform.claude.com/docs/en/api/messages
// https://platform.claude.com/docs/en/build-with-claude/streaming
package transcode
