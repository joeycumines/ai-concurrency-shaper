// Package transcode implements HTTP request/response transcoding between the
// OpenAI Responses API, the OpenAI Chat Completions API, and the Anthropic
// Messages API.
//
// The schema types in this package model the well-defined wire formats of
// those three external APIs (chat.go, responses.go, anthropic.go). Union-typed
// fields (content as string-or-array, tool_choice as string-or-object, tool
// output as string-or-blocks) mirror the JSON semantics of the Bifrost
// reference implementation (github.com/joeycumines/bifrost, core/schemas and
// core/providers/anthropic/types.go) so payloads in the internal test corpus
// round-trip faithfully.
package transcode
