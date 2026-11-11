package transcode

import (
	"fmt"

	"github.com/joeycumines/ai-concurrency-shaper/internal/transcode/wire/openairesponses"
)

// plainToolNames returns the names of the request's ordinary function tools:
// a namespace child never displaces a plain function's name.
func plainToolNames(tools []openairesponses.Tool) map[string]struct{} {
	reserved := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		if tool.Type == "function" && tool.Name != "" {
			reserved[tool.Name] = struct{}{}
		}
	}
	return reserved
}

// chooseToolFlatName returns the chat-facing flat name for a namespace child
// and records it in the exchange map. The bare child name is used while it is
// unique; a collision with a plain function name or an earlier child is
// qualified as namespace + "__" + child, and a qualified name that is itself
// taken is disambiguated with a numeric suffix. Every mapping stays
// invertible, so no name is ever silently misattributed.
func chooseToolFlatName(
	names *ToolNames,
	reserved map[string]struct{},
	namespace string,
	child string,
) string {
	ref := ToolNameRef{Namespace: namespace, Name: child}
	if existing, ok := names.RefToFlat[ref]; ok {
		return existing
	}
	if _, taken := reserved[child]; !taken {
		if _, used := names.FlatToRef[child]; !used {
			names.FlatToRef[child] = ref
			names.RefToFlat[ref] = child
			return child
		}
	}
	base := namespace + "__" + child
	qualified := base
	for n := 2; ; n++ {
		_, used := names.FlatToRef[qualified]
		_, taken := reserved[qualified]
		if !used && !taken {
			break
		}
		qualified = fmt.Sprintf("%s_%d", base, n)
	}
	names.FlatToRef[qualified] = ref
	names.RefToFlat[ref] = qualified
	return qualified
}

// namespaceChild is one flattened namespace child together with the
// namespace it was declared under: a nested namespace keeps its own name so
// the response can restore the qualifier the client registered.
type namespaceChild struct {
	Namespace string
	Tool      openairesponses.Tool
}

// namespaceReplayOwnedName reports whether any tool this exchange renders to the
// upstream already answers to bareName. The universe is exactly the rendered
// wire names — a plain function keeps its own name, and a namespace child is
// known by its flat name (bare while unique, qualified otherwise) — so a
// replayed call can never be forwarded under a name another tool owns.
func namespaceReplayOwnedName(tools []CanonicalTool, bareName string) bool {
	for _, tool := range tools {
		if tool.Name == bareName {
			return true
		}
	}
	return false
}

// namespaceReplayResolve is the per-exchange adjudication of a replayed
// function call's namespace qualifier.
//
// The canonical part keeps the qualifier the client sent: a Responses target
// carries it on the wire, and history the continuity store reconstructed never
// passes through decode, so a decision written into the part at decode time
// would be missing or stale for a later exchange. Each flattening renderer
// therefore decides for its own exchange:
//
//   - no qualifier: the bare name is the call's name;
//   - the exchange declares this (namespace, name) pair: the flat wire name,
//     so the upstream sees exactly the tool it was taught;
//   - the exchange does not declare the pair and a surviving tool owns the bare
//     name: an error, because forwarding it would attribute a client-sent call
//     to a different tool (no policy can approve that);
//   - otherwise: one namespace_replay_undeclared loss, and the bare name,
//     which is the only representation a flattening target can receive.
type namespaceReplayResolve struct {
	names  *ToolNames
	tools  []CanonicalTool
	policy LossPolicy
	report *ConversionReport
}

// newNamespaceReplayResolve builds the per-exchange resolver. report is the
// exchange's request-side report, so each decision is recorded once, in the
// stage that performs it.
func newNamespaceReplayResolve(
	names *ToolNames,
	tools []CanonicalTool,
	policy LossPolicy,
	report *ConversionReport,
) namespaceReplayResolve {
	return namespaceReplayResolve{names: names, tools: tools, policy: policy, report: report}
}

// wireName returns the name the upstream must be given for call, recording the
// adjudication at path. An error means the exchange cannot be served without
// reinterpreting client-sent history, or that the loss policy refused the drop.
func (r namespaceReplayResolve) wireName(call CanonicalFunctionCall, path string) (string, error) {
	if call.Namespace == "" {
		return call.Name, nil
	}
	ref := ToolNameRef{Namespace: call.Namespace, Name: call.Name}
	if flat, ok := r.names.flatName(ref); ok {
		return flat, nil
	}
	if namespaceReplayOwnedName(r.tools, call.Name) {
		return "", fmt.Errorf(
			"%s: function call %q declares namespace %q, which this request does not "+
				"declare for that name, and the name is already used by another declared tool; "+
				"forwarding it would attribute the call to that tool instead",
			path, call.Name, call.Namespace,
		)
	}
	if err := r.report.Lose(
		r.policy,
		FeatureNamespaceReplayUndeclared,
		path,
		fmt.Sprintf(
			"replayed call %q references namespace %q, which this request does not declare for that name; "+
				"the qualifier cannot be mapped, so the call is forwarded under its bare child name",
			call.Name,
			call.Namespace,
		),
	); err != nil {
		return "", err
	}
	return call.Name, nil
}

// flattenNamespaceTool returns the nested function tools of a namespace
// tool, recursing into nested namespaces. The grouping is client-side
// structure a chat request cannot express; the nested function tools are
// fully portable, so the flatten is reported as a named Note. A nested
// non-function tool type is the SAME builtin_tools loss decision as a
// top-level built-in tool: approved, it drops
// observably; rejected, the request fails — never a different, harder rule
// than the top-level path.
func flattenNamespaceTool(
	tool openairesponses.Tool,
	report *ConversionReport,
	policy LossPolicy,
) ([]namespaceChild, error) {
	var out []namespaceChild
	for i, nested := range tool.Tools {
		switch nested.Type {
		case "namespace":
			inner, err := flattenNamespaceTool(nested, report, policy)
			if err != nil {
				return nil, fmt.Errorf("namespace %d: %w", i, err)
			}
			out = append(out, inner...)
		case "function":
			out = append(out, namespaceChild{Namespace: tool.Name, Tool: nested})
		default:
			if err := report.Lose(
				policy,
				FeatureBuiltinTools,
				fmt.Sprintf("tools[].tools[%d]", i),
				fmt.Sprintf(
					"the %q built-in tool nested in a namespace cannot be reproduced in a chat request",
					nested.Type,
				),
			); err != nil {
				return nil, err
			}
		}
	}
	if len(out) == 0 {
		// Every nested tool was a dropped built-in. The top-level rule for
		// an all-built-in tools list is accept-and-drop under the approval,
		// and the tool_choice reconciliation owns the no-tools-left case —
		// a namespace is never a different, harder rule than the top level.
		if err := report.Note(
			FeatureBuiltinTools,
			fmt.Sprintf("tools[namespace=%s]", tool.Name),
			fmt.Sprintf(
				"namespace tool %q carried no portable function tools (all nested tools dropped under the builtin_tools approval)",
				tool.Name,
			),
		); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if err := report.Note(
		FeatureBuiltinTools,
		fmt.Sprintf("tools[namespace=%s]", tool.Name),
		fmt.Sprintf(
			"namespace tool %q flattened into %d function tool(s) for the chat request",
			tool.Name,
			len(out),
		),
	); err != nil {
		return nil, err
	}
	return out, nil
}
