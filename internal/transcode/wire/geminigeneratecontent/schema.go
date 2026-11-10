package geminigeneratecontent

import "encoding/json"

// SchemaTypes is the pinned Gemini Schema.type enum. The transcode layer
// converts the raw Schema object into JSON Schema.
var SchemaTypes = []string{
	"TYPE_UNSPECIFIED",
	"STRING",
	"NUMBER",
	"INTEGER",
	"BOOLEAN",
	"ARRAY",
	"OBJECT",
	"NULL",
}

// Schema is the raw OpenAPI schema object carried by a function declaration
// or a response schema. Conversion lives in the transcode layer.
type Schema map[string]json.RawMessage
