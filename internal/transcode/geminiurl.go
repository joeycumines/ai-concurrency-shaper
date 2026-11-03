package transcode

import (
	"errors"
	"fmt"
	"net/url"
)

// BuildGeminiURL builds the upstream URL for a Gemini GenerateContent call:
// <base path><upstreamPrefix>/models/<model>:generateContent, or
// :streamGenerateContent with alt=sse for a stream. The base URL's own query
// is kept. Client query parameters are never forwarded: the Gemini API
// rejects unknown parameters.
func BuildGeminiURL(base *url.URL, upstreamPrefix, model string, stream bool) (*url.URL, error) {
	if base == nil {
		return nil, errors.New("nil upstream URL")
	}
	if !ValidCatalogIdent(model) {
		return nil, fmt.Errorf("gemini upstream model %q is not a bare model id", model)
	}
	action := geminiGenerateAction
	if stream {
		action = geminiStreamAction
	}
	// JoinPath works on the escaped path, so an encoded separator or
	// question mark in the configured base survives as written. An empty
	// base path is rooted first: JoinPath would otherwise produce a relative
	// path, and the upstream path must stay absolute.
	root := *base
	if root.EscapedPath() == "" {
		root.Path = "/"
		root.RawPath = ""
	}
	out := root.JoinPath(upstreamPrefix, "models", model+":"+action)
	query := base.Query()
	if stream {
		query.Set("alt", "sse")
	}
	out.RawQuery = query.Encode()
	return out, nil
}
