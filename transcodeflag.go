package main

import (
	"fmt"
	"strings"

	"github.com/joeycumines/ai-concurrency-shaper/internal/proxy"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode"
)

// transcodeRouteFlags implements flag.Value for repeatable -transcode-route
// flags of the form clientPath=upstreamPath:clientFormat:upstreamFormat.
type transcodeRouteFlags []proxy.TranscodeMapping

func (f transcodeRouteFlags) String() string {
	var parts []string
	for _, m := range f {
		parts = append(parts, fmt.Sprintf("%s=%s:%s:%s", m.ClientPath, m.UpstreamPath, m.ClientFormat, m.UpstreamFormat))
	}
	return strings.Join(parts, ", ")
}
func (f *transcodeRouteFlags) Set(v string) error {
	m, err := parseTranscodeRoute(v)
	if err != nil {
		return err
	}
	*f = append(*f, m)
	return nil
}

// parseTranscodeRoute parses one -transcode-route value.
func parseTranscodeRoute(value string) (proxy.TranscodeMapping, error) {
	// The format names never contain colons, so the last two colon-separated
	// segments are the formats and the remainder is clientPath=upstreamPath —
	// the upstream path may itself contain colons (e.g. /v1/models:predict).
	last := strings.LastIndex(value, ":")
	if last < 0 {
		return proxy.TranscodeMapping{}, fmt.Errorf("invalid transcode route %q: want clientPath=upstreamPath:clientFormat:upstreamFormat", value)
	}
	secondLast := strings.LastIndex(value[:last], ":")
	if secondLast < 0 {
		return proxy.TranscodeMapping{}, fmt.Errorf("invalid transcode route %q: want clientPath=upstreamPath:clientFormat:upstreamFormat", value)
	}
	clientFormat := transcode.Format(value[secondLast+1 : last])
	upstreamFormat := transcode.Format(value[last+1:])
	if !transcode.SupportedFormatPair(clientFormat, upstreamFormat) {
		return proxy.TranscodeMapping{}, fmt.Errorf("invalid transcode route %q: unsupported format pair %q -> %q", value, clientFormat, upstreamFormat)
	}
	paths := strings.SplitN(value[:secondLast], "=", 2)
	if len(paths) != 2 || paths[0] == "" || paths[1] == "" {
		return proxy.TranscodeMapping{}, fmt.Errorf("invalid transcode route %q: want clientPath=upstreamPath", value)
	}
	return proxy.TranscodeMapping{
		ClientPath:     paths[0],
		UpstreamPath:   paths[1],
		ClientFormat:   clientFormat,
		UpstreamFormat: upstreamFormat,
	}, nil
}

// buildTranscodeMappings combines the repeatable -transcode-route values with
// the three preset flags, in that order.
func buildTranscodeMappings(routes []proxy.TranscodeMapping, responsesChat, messagesChat, messagesResponses bool) []proxy.TranscodeMapping {
	var mappings []proxy.TranscodeMapping
	mappings = append(mappings, routes...)
	if responsesChat {
		mappings = append(mappings, proxy.TranscodeMapping{
			ClientPath:     "/v1/responses",
			UpstreamPath:   "/v1/chat/completions",
			ClientFormat:   transcode.FormatResponses,
			UpstreamFormat: transcode.FormatChatCompletions,
		})
	}
	if messagesChat {
		mappings = append(mappings, proxy.TranscodeMapping{
			ClientPath:     "/v1/messages",
			UpstreamPath:   "/v1/chat/completions",
			ClientFormat:   transcode.FormatMessages,
			UpstreamFormat: transcode.FormatChatCompletions,
		})
	}
	if messagesResponses {
		mappings = append(mappings, proxy.TranscodeMapping{
			ClientPath:     "/v1/messages",
			UpstreamPath:   "/v1/responses",
			ClientFormat:   transcode.FormatMessages,
			UpstreamFormat: transcode.FormatResponses,
		})
	}
	return mappings
}
