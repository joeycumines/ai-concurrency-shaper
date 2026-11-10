package transcode

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire"
	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/geminigeneratecontent"
)

// GeminiRequestFacts carries the Gemini-only request facts the handler needs
// after decoding; they have no place in the canonical request.
type GeminiRequestFacts struct {
	// IncludeThoughts is generationConfig.thinkingConfig.includeThoughts.
	IncludeThoughts bool
	// BodyModel is the top-level body "model", when present.
	BodyModel *string
}

// DecodeGeminiRequest decodes a Gemini GenerateContent request body into the
// canonical request. Decoding is strict: unknown fields anywhere are errors,
// and every field the canonical form cannot carry is either an approved,
// observable loss under policy or a rejection. target is needed only for the
// topK decision.
func DecodeGeminiRequest(
	body []byte,
	policy LossPolicy,
	target UpstreamProtocol,
) (DecodeResult, GeminiRequestFacts, error) {
	var result DecodeResult
	var facts GeminiRequestFacts
	req, err := geminigeneratecontent.DecodeRequest(body)
	if err != nil {
		return result, facts, geminiDecodeError(err)
	}
	facts.BodyModel = req.Model
	d := newGeminiDecoder(policy, target, &result.Report)
	if err := d.request(req, &result.Request, &facts); err != nil {
		return result, facts, err
	}
	if err := ValidateCanonicalRequest(result.Request); err != nil {
		return result, facts, err
	}
	return result, facts, nil
}

// geminiDecodeError adds the accepted spelling to an unknown-field rejection
// of a snake_case name. Gemini field names on this gateway are lowerCamelCase
// JSON names, as the official SDKs send them.
func geminiDecodeError(err error) error {
	var decodeErr *wire.DecodeError
	if !errors.As(err, &decodeErr) || decodeErr.Kind != wire.DecodeUnknownField {
		return err
	}
	const marker = `unknown field "`
	_, rest, found := strings.Cut(decodeErr.Message, marker)
	if !found {
		return err
	}
	name, _, _ := strings.Cut(rest, `"`)
	if !strings.Contains(name, "_") {
		return err
	}
	return fmt.Errorf(
		"%w; Gemini field names on this gateway must use the camelCase JSON spelling, for example %q",
		err,
		geminiCamelCase(name),
	)
}

// geminiCamelCase converts a snake_case field name to lowerCamelCase.
func geminiCamelCase(name string) string {
	var b strings.Builder
	first := true
	for part := range strings.SplitSeq(name, "_") {
		if part == "" {
			continue
		}
		if first {
			b.WriteString(part)
			first = false
			continue
		}
		b.WriteString(strings.ToUpper(part[:1]) + part[1:])
	}
	return b.String()
}

// geminiDecoder carries the state one request decode needs: call identity,
// correlation, and the once-per-request notes.
type geminiDecoder struct {
	policy LossPolicy
	target UpstreamProtocol
	report *ConversionReport

	// callIDs holds every functionCall id seen so far in the request.
	callIDs map[string]struct{}
	// minted counts proxy-minted call ids.
	minted int
	// open lists functionCalls not yet answered, in request order.
	open []geminiOpenCall

	callIDNoted     bool
	correlationNote bool
}

// geminiOpenCall is a functionCall waiting for its functionResponse.
type geminiOpenCall struct {
	id      string
	name    string
	content int
}

func newGeminiDecoder(policy LossPolicy, target UpstreamProtocol, report *ConversionReport) *geminiDecoder {
	return &geminiDecoder{
		policy:  policy,
		target:  target,
		report:  report,
		callIDs: make(map[string]struct{}),
	}
}

// request converts the decoded wire request. The order matters for the
// call-identity walk: system instruction, contents in order, then tools and
// configuration.
func (d *geminiDecoder) request(
	req geminigeneratecontent.GenerateContentRequest,
	out *CanonicalRequest,
	facts *GeminiRequestFacts,
) error {
	if len(req.Contents) == 0 {
		return errors.New("contents is required")
	}
	if req.SystemInstruction != nil {
		turn, ok, err := d.systemTurn(*req.SystemInstruction)
		if err != nil {
			return err
		}
		if ok {
			out.Turns = append(out.Turns, turn)
		}
	}
	for i, content := range req.Contents {
		turns, err := d.contentTurns(i, content)
		if err != nil {
			return err
		}
		// A content whose every part was dropped under an approved loss has no
		// canonical form; the loss is already recorded, so no turn is emitted.
		out.Turns = append(out.Turns, turns...)
	}
	if err := d.tools(req.Tools, out); err != nil {
		return err
	}
	if err := d.toolConfig(req.ToolConfig, out); err != nil {
		return err
	}
	if err := d.generationConfig(req.GenerationConfig, out, facts); err != nil {
		return err
	}
	return d.requestControls(req, out)
}

// systemTurn converts systemInstruction. It returns ok=false when every part
// was dropped under an approved loss, so no system turn is emitted.
func (d *geminiDecoder) systemTurn(content geminigeneratecontent.Content) (CanonicalTurn, bool, error) {
	switch content.Role {
	case "", "user", "system":
	case "model":
		return CanonicalTurn{}, false, errors.New("systemInstruction.role must not be model")
	default:
		return CanonicalTurn{}, false, fmt.Errorf("systemInstruction.role %q is not supported", content.Role)
	}
	if len(content.Parts) == 0 {
		return CanonicalTurn{}, false, errors.New("systemInstruction.parts is required")
	}
	var parts []CanonicalPart
	for j, part := range content.Parts {
		path := fmt.Sprintf("systemInstruction.parts[%d]", j)
		if !geminiTextOnly(part) {
			return CanonicalTurn{}, false, fmt.Errorf("%s must be a text part: system instructions carry only text", path)
		}
		converted, err := d.part(path, CanonicalSystem, -1, part, nil)
		if err != nil {
			return CanonicalTurn{}, false, err
		}
		parts = append(parts, converted...)
	}
	if len(parts) == 0 {
		return CanonicalTurn{}, false, nil
	}
	return CanonicalTurn{Role: CanonicalSystem, Parts: parts}, true, nil
}

// contentTurns converts contents[i] into one or more canonical turns,
// preserving the turn boundary. A role-less content is read as a user turn
// with a note.
//
// A functionResponse part is a tool result and belongs to a user turn in the
// canonical IR whatever role its content declares. The Gemini API accepts a
// functionResponse in a model content and agy sends every tool result that
// way, so the canonical role of each converted part follows its kind:
// function results form user turns, every other part keeps the content's
// role. A content that mixes them therefore yields more than one canonical
// turn, in part order.
func (d *geminiDecoder) contentTurns(i int, content geminigeneratecontent.Content) ([]CanonicalTurn, error) {
	path := fmt.Sprintf("contents[%d]", i)
	var role CanonicalRole
	switch content.Role {
	case "user":
		role = CanonicalUser
	case "model":
		role = CanonicalAssistant
	case "":
		role = CanonicalUser
		if err := d.report.Note(
			FeatureGeminiRoleInferred,
			path+".role",
			"a content with no role is read as a user turn",
		); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("%s.role %q is not supported (want user or model)", path, content.Role)
	}
	if len(content.Parts) == 0 {
		return nil, fmt.Errorf("%s.parts is required", path)
	}

	// functionResponse parts may omit their ids. Count them by name first,
	// so the correlation rule sees the whole content.
	idless := make(map[string]int)
	for _, part := range content.Parts {
		if part.FunctionResponse != nil && part.FunctionResponse.ID == "" {
			idless[part.FunctionResponse.Name]++
		}
	}

	var turns []CanonicalTurn
	for j, part := range content.Parts {
		partPath := fmt.Sprintf("%s.parts[%d]", path, j)
		converted, err := d.part(partPath, role, i, part, idless)
		if err != nil {
			return nil, err
		}
		for _, canonical := range converted {
			partRole := role
			if _, isResult := canonical.(CanonicalFunctionResult); isResult {
				partRole = CanonicalUser
			}
			if len(turns) == 0 || turns[len(turns)-1].Role != partRole {
				turns = append(turns, CanonicalTurn{Role: partRole})
			}
			turns[len(turns)-1].Parts = append(turns[len(turns)-1].Parts, canonical)
		}
	}
	// Every part was dropped under an approved loss (the loss was recorded as
	// each was converted); the caller emits no turn.
	return turns, nil
}

// geminiTextOnly reports whether a part carries text and nothing else but
// its thought and signature members.
func geminiTextOnly(part geminigeneratecontent.Part) bool {
	return part.Text != nil &&
		part.InlineData == nil && part.FileData == nil &&
		part.FunctionCall == nil && part.FunctionResponse == nil &&
		!geminiRawPayloadPresent(part)
}

// geminiRawPayloadPresent reports whether a part carries any raw payload
// member (code, tool, or audio/video material).
func geminiRawPayloadPresent(part geminigeneratecontent.Part) bool {
	return slices.ContainsFunc([]json.RawMessage{
		part.ExecutableCode, part.CodeExecutionResult, part.ToolCall,
		part.ToolResponse, part.AudioTranscription, part.VideoMetadata,
		part.SpeechMetadata,
	}, geminigeneratecontent.RawMemberPresent)
}
