// Package geminigeneratecontent implements the pinned Google Gemini
// Developer API v1beta GenerateContent wire contract (Discovery revision
// 20261028).
//
// Requests decode strictly: an unknown field is a client-dialect error.
// Response envelopes decode tolerantly, because the upstream provider
// extends its wire freely, while content parts stay strict. The package
// imports only internal/transcode/wire, never the transcode package, so the
// pinned contract can never depend on transcode internals.
package geminigeneratecontent
