package transcode

import (
	"fmt"
	"net/http"
	"strings"
)

// Gemini GenerateContent client routes are keyed by the JSON operation's
// template. The stream operation shares the same route key, so the matcher
// below is the only place that parses Gemini paths.
// Reference: https://ai.google.dev/api/generate-content

// GeminiOperation is the GenerateContent operation a Gemini path names.
type GeminiOperation uint8

const (
	// GeminiGenerate is the JSON operation, ":generateContent".
	GeminiGenerate GeminiOperation = iota + 1
	// GeminiStream is the server-sent-event operation, ":streamGenerateContent".
	GeminiStream
)

// GeminiRouteMatch is one matched Gemini request path.
type GeminiRouteMatch struct {
	// Key is the route key shared by both operations: POST <prefix>/models/{model}:generateContent.
	Key RouteKey
	// Model is the validated client model identifier, without a "models/" prefix.
	Model string
	// Operation is the operation the request path names.
	Operation GeminiOperation
	// TemplatePath is the request path with the model replaced by {model}, for
	// example <prefix>/models/{model}:streamGenerateContent.
	TemplatePath string
}

const (
	geminiModelsSegment   = "/models"
	geminiGenerateAction  = "generateContent"
	geminiStreamAction    = "streamGenerateContent"
	geminiGenerateSuffix  = "/models/{model}:" + geminiGenerateAction
	geminiTemplateSegment = "{model}"
)

// ParseGeminiClientRoutePath validates a Gemini client route template and
// returns its prefix: everything before /models/{model}:generateContent. The
// prefix may be empty.
func ParseGeminiClientRoutePath(path string) (string, error) {
	if canonicalizeRoutePath(path) != path || !strings.HasSuffix(path, geminiGenerateSuffix) {
		return "", fmt.Errorf(
			"gemini route path %q must have the form <prefix>/models/{model}:generateContent",
			path,
		)
	}
	prefix := strings.TrimSuffix(path, geminiGenerateSuffix)
	if strings.ContainsAny(prefix, "{}:") {
		return "", fmt.Errorf(
			"gemini route path %q must have the form <prefix>/models/{model}:generateContent",
			path,
		)
	}
	return prefix, nil
}

// MatchGeminiRequestPath matches a request against the Gemini GenerateContent
// operations. It returns false at the first failed rule: method, canonical
// path, /models directory, action name, or model identifier.
func MatchGeminiRequestPath(method, path string) (GeminiRouteMatch, bool) {
	if !strings.EqualFold(method, http.MethodPost) {
		return GeminiRouteMatch{}, false
	}
	if canonicalizeRoutePath(path) != path {
		return GeminiRouteMatch{}, false
	}
	slash := strings.LastIndexByte(path, '/')
	if slash < 0 {
		return GeminiRouteMatch{}, false
	}
	dir, segment := path[:slash], path[slash+1:]
	if dir != geminiModelsSegment && !strings.HasSuffix(dir, geminiModelsSegment) {
		return GeminiRouteMatch{}, false
	}
	colon := strings.LastIndexByte(segment, ':')
	if colon < 0 {
		return GeminiRouteMatch{}, false
	}
	model, action := segment[:colon], segment[colon+1:]
	var operation GeminiOperation
	switch action {
	case geminiGenerateAction:
		operation = GeminiGenerate
	case geminiStreamAction:
		operation = GeminiStream
	default:
		return GeminiRouteMatch{}, false
	}
	if !ValidCatalogIdent(model) {
		return GeminiRouteMatch{}, false
	}
	return GeminiRouteMatch{
		Key:          RouteKey{Method: http.MethodPost, Path: dir + "/" + geminiTemplateSegment + ":" + geminiGenerateAction},
		Model:        model,
		Operation:    operation,
		TemplatePath: dir + "/" + geminiTemplateSegment + ":" + action,
	}, true
}

// IsGeminiOperationPath reports whether the final path segment ends in a
// Gemini GenerateContent action, whatever precedes it. It is deliberately
// broader than MatchGeminiRequestPath and is used only to refuse queue
// comments on such paths.
func IsGeminiOperationPath(path string) bool {
	segment := path[strings.LastIndexByte(path, '/')+1:]
	return strings.HasSuffix(segment, ":"+geminiGenerateAction) ||
		strings.HasSuffix(segment, ":"+geminiStreamAction)
}
