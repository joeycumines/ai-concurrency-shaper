// Copyright (C) 2026 Joseph Cumines
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package transcode

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/geminigeneratecontent"
)

// skipThoughtSignatureValidator is the documented placeholder thought
// signature Google accepts for history the model did not produce.
const skipThoughtSignatureValidator = "skip_thought_signature_validator"

// gcallPrefix marks proxy-minted function-call ids; they are never rendered
// as Gemini ids (the upstream must see only its own or client-issued ids).
const gcallPrefix = "gcall_"

// geminiRenderPart is one rendered Gemini content part. The payload members
// are mutually exclusive; thoughtSignature may accompany a functionCall.
type geminiRenderPart struct {
	Text             *string                       `json:"text,omitempty"`
	Thought          *bool                         `json:"thought,omitempty"`
	ThoughtSignature *string                       `json:"thoughtSignature,omitempty"`
	InlineData       *geminigeneratecontent.Blob   `json:"inlineData,omitempty"`
	FunctionCall     *geminiRenderFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *geminiRenderFunctionResponse `json:"functionResponse,omitempty"`
}

type geminiRenderFunctionCall struct {
	ID   string          `json:"id,omitempty"`
	Name string          `json:"name,omitempty"`
	Args json.RawMessage `json:"args,omitempty"`
}

type geminiRenderFunctionResponse struct {
	ID       string          `json:"id,omitempty"`
	Name     string          `json:"name,omitempty"`
	Response json.RawMessage `json:"response,omitempty"`
}

// geminiRenderContent is one rendered Gemini content (one canonical turn).
type geminiRenderContent struct {
	Role  string             `json:"role,omitempty"`
	Parts []geminiRenderPart `json:"parts,omitempty"`
}

// geminiRenderTool is one entry of the tools array.
type geminiRenderTool struct {
	FunctionDeclarations []geminiRenderFunctionDeclaration `json:"functionDeclarations,omitempty"`
}

type geminiRenderFunctionDeclaration struct {
	Name                 string          `json:"name,omitempty"`
	Description          string          `json:"description,omitempty"`
	ParametersJSONSchema json.RawMessage `json:"parametersJsonSchema,omitempty"`
}

// geminiRenderToolConfig is the rendered toolConfig.
type geminiRenderToolConfig struct {
	FunctionCallingConfig geminiRenderFunctionCallingConfig `json:"functionCallingConfig"`
}

type geminiRenderFunctionCallingConfig struct {
	Mode                 string   `json:"mode"`
	AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
}

// geminiRenderGenerationConfig is the rendered generationConfig. Only the
// fields the canonical request can carry are modeled; everything else is a
// loss decided by the caller.
type geminiRenderGenerationConfig struct {
	MaxOutputTokens    *int64          `json:"maxOutputTokens,omitempty"`
	Temperature        *float64        `json:"temperature,omitempty"`
	TopP               *float64        `json:"topP,omitempty"`
	TopK               *int64          `json:"topK,omitempty"`
	StopSequences      []string        `json:"stopSequences,omitempty"`
	ResponseMimeType   *string         `json:"responseMimeType,omitempty"`
	ResponseJSONSchema json.RawMessage `json:"responseJsonSchema,omitempty"`
}

// geminiRenderRequest is the rendered GenerateContent request body.
type geminiRenderRequest struct {
	SystemInstruction *geminiRenderContent          `json:"systemInstruction,omitempty"`
	Contents          []geminiRenderContent         `json:"contents"`
	Tools             []geminiRenderTool            `json:"tools,omitempty"`
	ToolConfig        *geminiRenderToolConfig       `json:"toolConfig,omitempty"`
	GenerationConfig  *geminiRenderGenerationConfig `json:"generationConfig,omitempty"`
}

// RenderGeminiRequest renders a canonical request as a Gemini GenerateContent
// request body (R2G, M2G). Every canonical turn becomes exactly one content,
// in order, never merged. Function-call thought signatures are restored from
// the exchange's signature store; the first function call of each current-turn
// model content without a stored signature gets the documented placeholder
// under the gemini_thought_signature_placeholder loss.
func RenderGeminiRequest(
	request CanonicalRequest,
	context *ExchangeContext,
) ([]byte, ConversionReport, error) {
	var report ConversionReport
	if err := ValidateCanonicalRequest(request); err != nil {
		return nil, report, err
	}
	policy := context.lossPolicy()
	if err := RequirePortableArtifacts(request, policy, &report); err != nil {
		return nil, report, err
	}
	// Responses request fields Gemini cannot reproduce take the shared
	// envelope decision, so a Responses-to-Gemini route never drops a client
	// field silently. Gemini has no reasoning-effort control, no user field,
	// and no store.
	if err := responsesEchoLosses(context, &report, echoCarriage{}); err != nil {
		return nil, report, err
	}

	out := geminiRenderRequest{}

	// System/developer turns concatenate into one systemInstruction with one
	// text part per canonical text part, in order. The position and role
	// decisions are taken per turn through the helpers every other renderer
	// uses, so this target cannot drift: a system turn after dialog takes the
	// mid_conversation_system position decision, a developer turn takes the
	// developer_role decision, and several contributing turns take the shared
	// join decision.
	var systemParts []geminiRenderPart
	var contributing []int
	sawClientDialog := false
	// sawRetainedDialog tracks dialog turns the continuity store
	// reconstructed. A request-side gate polices CLIENT input, so a position
	// the client's own request never created must not be charged to it: a
	// client system turn physically following retained dialog is still the
	// client's own leading instruction.
	sawRetainedDialog := false
	for i, turn := range request.Turns {
		if turn.Role != CanonicalSystem && turn.Role != CanonicalDeveloper {
			if i < request.RetainedTurns {
				sawRetainedDialog = true
			} else {
				sawClientDialog = true
			}
			continue
		}
		path := fmt.Sprintf("turns[%d]", i)
		retained := i < request.RetainedTurns
		midDialog := sawClientDialog
		if retained {
			midDialog = sawRetainedDialog || sawClientDialog
		}
		// Render the turn's parts FIRST: a turn whose parts all took a drop
		// decision contributes nothing, so it occupies no position and takes
		// no role decision. Charging before knowing the contribution would
		// bill the client for a move that never happened.
		var turnParts []geminiRenderPart
		for j, part := range turn.Parts {
			partPath := fmt.Sprintf("%s.parts[%d]", path, j)
			switch p := part.(type) {
			case CanonicalText:
				// An empty text part carries no bytes; emitting it would put an
				// empty prompt part on the wire for content the client never
				// wrote.
				if p.Text == "" {
					continue
				}
				turnParts = append(turnParts, geminiRenderPart{Text: new(p.Text)})
			case CanonicalThinkingPart:
				// Marker-signature thinking is proxy-synthesized and scrubbed
				// before upstream rendering; a real signature is refused
				// centrally by ValidateCanonicalRequest. Either way nothing
				// renders, in every turn role.
				continue
			default:
				if err := report.Lose(
					policy,
					FeatureSystemNonTextContent,
					partPath,
					"non-text system prompt content cannot be expressed in Gemini's text-only systemInstruction",
				); err != nil {
					return nil, report, err
				}
			}
		}
		if len(turnParts) == 0 {
			continue
		}
		if midDialog {
			if err := chargePosition(
				request,
				i,
				FeatureMidConversationSystem,
				path,
				"a system turn after a dialog turn still moves into systemInstruction; its position is lost",
				&report,
				policy,
			); err != nil {
				return nil, report, err
			}
		}
		if turn.Role == CanonicalDeveloper {
			if err := chargePosition(
				request,
				i,
				FeatureDeveloperRole,
				path,
				"Gemini's systemInstruction is role-neutral; the developer-role distinction is lost",
				&report,
				policy,
			); err != nil {
				return nil, report, err
			}
		}
		systemParts = append(systemParts, turnParts...)
		contributing = append(contributing, i)
	}
	if err := chargeSystemTurnJoin(
		request,
		contributing,
		"systemInstruction",
		"multiple system turns consolidate into one Gemini systemInstruction",
		&report,
		policy,
	); err != nil {
		return nil, report, err
	}
	if len(systemParts) > 0 {
		out.SystemInstruction = &geminiRenderContent{Parts: systemParts}
	}

	// One content per canonical turn, in order. The client decoders already
	// group parallel calls and their results into single turns.
	callNames := make(map[string]string) // call id -> function name, for result name resolution
	replay := newNamespaceReplayResolve(context.toolNames(), request.Tools, policy, &report)
	currentTurn := len(request.Turns) // index of the first current-turn model content
	for i, turn := range request.Turns {
		if turn.Role == CanonicalUser {
			hasNonResult := false
			for _, part := range turn.Parts {
				if _, isResult := part.(CanonicalFunctionResult); !isResult {
					hasNonResult = true
					break
				}
			}
			if hasNonResult {
				currentTurn = i + 1
			}
		}
	}

	for i, turn := range request.Turns {
		if turn.Role == CanonicalSystem || turn.Role == CanonicalDeveloper {
			continue
		}
		var role string
		switch turn.Role {
		case CanonicalUser:
			role = "user"
		case CanonicalAssistant:
			role = "model"
		default:
			return nil, report, fmt.Errorf("turn %d has unsupported role %q", i, turn.Role)
		}
		content := geminiRenderContent{Role: role}
		isCurrentModel := role == "model" && i >= currentTurn
		placeholderUsed := false
		for j, part := range turn.Parts {
			path := fmt.Sprintf("turns[%d].parts[%d]", i, j)
			switch p := part.(type) {
			case CanonicalText:
				content.Parts = append(content.Parts, geminiRenderPart{Text: new(p.Text)})
			case CanonicalRefusal:
				if err := report.Note(
					FeatureGeminiRefusalText,
					path,
					"refusal content is rendered as ordinary text; Gemini has no refusal part in a model turn",
				); err != nil {
					return nil, report, err
				}
				content.Parts = append(content.Parts, geminiRenderPart{Text: new(p.Text)})
			case CanonicalThinkingPart:
				// Marker-signature thinking was proxy-synthesized: drop it
				// silently (the scrub contract). A real signature was already
				// handled by RequirePortableArtifacts.
				continue
			case CanonicalImage:
				if p.URL != "" {
					if err := report.Lose(
						policy,
						FeatureImageInput,
						path,
						"Gemini fileData accepts only Google file URIs; an image URL cannot be forwarded",
					); err != nil {
						return nil, report, err
					}
					continue
				}
				if p.Detail != "" && p.Detail != "auto" {
					if err := report.Lose(
						policy,
						FeatureGeminiImageDetail,
						path,
						fmt.Sprintf("Gemini has no per-image detail control equivalent to %q", p.Detail),
					); err != nil {
						return nil, report, err
					}
				}
				content.Parts = append(content.Parts, geminiRenderPart{
					InlineData: &geminigeneratecontent.Blob{MimeType: p.MediaType, Data: p.Base64},
				})
			case CanonicalDocument:
				if p.Base64 == "" || p.MediaType != "application/pdf" {
					if err := report.Lose(
						policy,
						FeatureDocumentInput,
						path,
						"only a base64 PDF document can be carried as Gemini inlineData",
					); err != nil {
						return nil, report, err
					}
					continue
				}
				content.Parts = append(content.Parts, geminiRenderPart{
					InlineData: &geminigeneratecontent.Blob{MimeType: p.MediaType, Data: p.Base64},
				})
			case CanonicalFunctionCall:
				// The call's namespace qualifier is resolved for THIS
				// exchange: the Gemini wire carries only the flat name, and
				// the functionResponse name rendered below must match what the
				// model was taught (callNames).
				name, err := replay.wireName(p, path)
				if err != nil {
					return nil, report, err
				}
				call := &geminiRenderFunctionCall{
					Name: name,
					Args: p.Arguments,
				}
				if !strings.HasPrefix(p.CallID, gcallPrefix) {
					call.ID = p.CallID
				}
				callNames[p.CallID] = name
				part := geminiRenderPart{FunctionCall: call}
				// Thought-signature rules (§13.3): a stored signature wins;
				// otherwise the first functionCall of each current-turn model
				// content gets the documented placeholder.
				if context != nil && context.GeminiSignatures != nil {
					if sig, ok := context.GeminiSignatures.Get(context.GeminiSignatureScope, p.CallID); ok {
						part.ThoughtSignature = new(sig)
					}
				}
				if part.ThoughtSignature == nil && isCurrentModel && !placeholderUsed {
					placeholderUsed = true
					if err := report.Lose(
						policy,
						FeatureGeminiThoughtSignaturePlaceholder,
						path,
						"no stored thought signature for this call; the documented skip_thought_signature_validator placeholder is sent (Google documents it for history the model did not produce)",
					); err != nil {
						return nil, report, err
					}
					part.ThoughtSignature = new(skipThoughtSignatureValidator)
				}
				content.Parts = append(content.Parts, part)
			case CanonicalFunctionResult:
				name, ok := callNames[p.CallID]
				if !ok {
					return nil, report, fmt.Errorf(
						"turn %d part %d: function result references call %q with no earlier call in the request",
						i, j, p.CallID,
					)
				}
				resp := &geminiRenderFunctionResponse{Name: name}
				if !strings.HasPrefix(p.CallID, gcallPrefix) {
					resp.ID = p.CallID
				}
				var texts []string
				for k, nested := range p.Parts {
					switch n := nested.(type) {
					case CanonicalText:
						texts = append(texts, n.Text)
					case CanonicalRefusal:
						texts = append(texts, n.Text)
					default:
						if err := report.Lose(
							policy,
							FeatureToolResultMultimodalContent,
							fmt.Sprintf("%s.parts[%d]", path, k),
							"non-text tool-result content cannot be carried by a Gemini functionResponse",
						); err != nil {
							return nil, report, err
						}
					}
				}
				text := strings.Join(texts, "\n")
				var payload map[string]string
				if p.IsError {
					payload = map[string]string{"error": text}
				} else {
					payload = map[string]string{"output": text}
				}
				raw, err := json.Marshal(payload)
				if err != nil {
					return nil, report, err
				}
				resp.Response = raw
				if err := report.Note(
					FeatureGeminiFunctionResponseObject,
					path,
					"the functionResponse payload is carried as the {\"output\"|\"error\": text} object shape",
				); err != nil {
					return nil, report, err
				}
				content.Parts = append(content.Parts, geminiRenderPart{FunctionResponse: resp})
			default:
				return nil, report, fmt.Errorf("turn %d part %d has unsupported type %T", i, j, part)
			}
		}
		if len(content.Parts) == 0 {
			return nil, report, fmt.Errorf("turn %d has no content that this upstream can carry", i)
		}
		out.Contents = append(out.Contents, content)
	}
	if len(out.Contents) == 0 {
		return nil, report, errors.New("request has no contents")
	}

	// Tools.
	if len(request.Tools) > 0 {
		tools := make([]geminiRenderTool, 0, len(request.Tools))
		for i, tool := range request.Tools {
			decl := geminiRenderFunctionDeclaration{
				Name:                 tool.Name,
				Description:          tool.Description,
				ParametersJSONSchema: tool.JSONSchema,
			}
			if tool.Strict != nil && *tool.Strict {
				if err := report.Lose(
					policy,
					FeatureGeminiToolStrictness,
					fmt.Sprintf("tools[%d].strict", i),
					"Gemini enforces argument schemas only in VALIDATED function-calling mode, which also changes tool-choice semantics; per-tool strictness cannot be reproduced",
				); err != nil {
					return nil, report, err
				}
			}
			tools = append(tools, geminiRenderTool{FunctionDeclarations: []geminiRenderFunctionDeclaration{decl}})
		}
		out.Tools = tools
	}

	// Tool choice.
	if request.ToolChoice != nil {
		cfg := geminiRenderToolConfig{}
		switch request.ToolChoice.Mode {
		case "auto":
			cfg.FunctionCallingConfig.Mode = "AUTO"
		case "none":
			cfg.FunctionCallingConfig.Mode = "NONE"
		case "required":
			cfg.FunctionCallingConfig.Mode = "ANY"
		case "named":
			cfg.FunctionCallingConfig.Mode = "ANY"
			cfg.FunctionCallingConfig.AllowedFunctionNames = []string{request.ToolChoice.Name}
		default:
			return nil, report, fmt.Errorf("unsupported tool choice mode %q", request.ToolChoice.Mode)
		}
		out.ToolConfig = &cfg
	}

	// Parallel tool calls. Gemini's function-calling mode always allows
	// parallel calls, so a client's explicit disable cannot be honoured: with
	// tools declared the upstream may emit calls the client forbade, which is a
	// real and refusable loss. The two inert shapes (no tools, or a choice that
	// forbids every call) and an explicit "allowed" are Notes/no-ops, because
	// nothing is lost.
	if _, err := chargeParallelTools(
		request,
		false,
		"parallel_tool_calls",
		&report,
		policy,
	); err != nil {
		return nil, report, err
	}

	// Generation config.
	gen := &geminiRenderGenerationConfig{}
	anyGen := false
	if request.MaxOutputTokens != nil {
		gen.MaxOutputTokens = new(int64(*request.MaxOutputTokens))
		anyGen = true
	}
	if request.Temperature != nil {
		gen.Temperature = new(*request.Temperature)
		anyGen = true
	}
	if request.TopP != nil {
		gen.TopP = new(*request.TopP)
		anyGen = true
	}
	if request.TopK != nil {
		gen.TopK = new(int64(*request.TopK))
		anyGen = true
	}
	if len(request.StopSequences) > 0 {
		gen.StopSequences = request.StopSequences
		anyGen = true
	}
	if request.StructuredOutput != nil {
		mime := "application/json"
		gen.ResponseMimeType = &mime
		gen.ResponseJSONSchema = request.StructuredOutput.Schema
		anyGen = true
		if !request.StructuredOutput.Strict {
			if err := report.Note(
				FeatureGeminiStructuredOutputEnforced,
				"structured_output.strict",
				"the client asked for best-effort structured output, but Gemini constrains output to the schema in every case; enforcement is recorded",
			); err != nil {
				return nil, report, err
			}
		}
	}
	if anyGen {
		out.GenerationConfig = gen
	}

	// Metadata and request-side reasoning have no Gemini request field that
	// means the same thing.
	if len(request.Metadata) > 0 {
		if err := report.Lose(
			policy,
			FeatureMetadata,
			"metadata",
			"request metadata is not forwarded because the target provider does not accept it",
		); err != nil {
			return nil, report, err
		}
	}
	if request.Thinking != nil {
		if err := report.Lose(
			policy,
			FeatureRequestReasoning,
			"thinking",
			"request-side reasoning controls cannot be reproduced in a Gemini request (thinkingConfig is not mapped)",
		); err != nil {
			return nil, report, err
		}
	}

	body, err := json.Marshal(out)
	if err != nil {
		return nil, report, err
	}
	return body, report, nil
}
