package transcode

// MultiAgentPrimingReminderText is the bounded, documented multi-agent protocol
// reminder injected into the leading system turn when the multi_agent_priming
// capability is enabled. It derives directly from the captured multi_agent_v1
// schema (close_agent, resume_agent, send_input, spawn_agent, wait_agent).
const MultiAgentPrimingReminderText = `### Sub-Agent Orchestration Protocol Reminder
When delegating work to sub-agents:
1. Call spawn_agent to launch a sub-agent with a concrete, bounded task in message or items (model and reasoning_effort may be specified if required).
2. Use wait_agent with the returned agent id in targets to wait for completion and receive final status/output.
3. Call send_input with target and message or items to communicate with an active agent (set interrupt: true to redirect immediately).
4. Call close_agent with target once an agent is completed or no longer needed.
5. If an agent was closed and needs further work, call resume_agent with id to reopen it.`

// applyMultiAgentPriming appends the protocol reminder to the leading system or
// developer message, creating a leading system message if none exists. It preserves
// the caller's messages and content structs without mutation, keeps raw string
// content as strings, preserves developer roles, and records FeatureMultiAgentPriming
// as an accurate Note for the branch taken.
// multiAgentToolNames are the bare child names the priming reminder describes.
var multiAgentToolNames = map[string]struct{}{
	"spawn_agent": {}, "wait_agent": {}, "send_input": {},
	"close_agent": {}, "resume_agent": {},
}

// multiAgentNamespace is the Codex namespace that groups those children.
const multiAgentNamespace = "multi_agent_v1"

// declaresMultiAgentTools reports whether this request will render at least
// one of the sub-agent tools the reminder names. It checks the FLAT names the
// upstream will actually see - a namespace child is looked up by its resolved
// flat name, so a child that had to be collision-qualified still counts,
// while a request that declares none of them is never primed.
func declaresMultiAgentTools(tools []CanonicalTool, context *ExchangeContext) bool {
	// Every entry here is already a flat function tool: namespace children
	// were flattened to their resolved names at decode.
	for _, tool := range tools {
		if _, ok := multiAgentToolNames[tool.Name]; ok {
			return true
		}
	}
	// A namespace child reaches the upstream under its resolved FLAT name, so
	// look the five up there too. The namespace NAME alone is not a contract
	// about its children - a multi_agent_v1 group holding only `list_agents`
	// would otherwise be primed with a reminder naming five tools the upstream
	// was never sent.
	if context != nil && context.ToolNames != nil {
		for ref := range context.ToolNames.RefToFlat {
			if ref.Namespace != multiAgentNamespace {
				continue
			}
			if _, ok := multiAgentToolNames[ref.Name]; ok {
				return true
			}
		}
	}
	return false
}

func applyMultiAgentPriming(messages []ChatMessage, report *ConversionReport) ([]ChatMessage, error) {
	if len(messages) > 0 && (messages[0].Role == ChatMessageRoleSystem || messages[0].Role == ChatMessageRoleDeveloper) {
		leadRole := messages[0].Role
		noteDesc := "multi-agent protocol reminder appended to leading system turn"
		if leadRole == ChatMessageRoleDeveloper {
			noteDesc = "multi-agent protocol reminder appended to leading developer turn"
		}
		if err := report.Note(
			FeatureMultiAgentPriming,
			"messages[0].content",
			noteDesc,
		); err != nil {
			return nil, err
		}

		cloned := make([]ChatMessage, len(messages))
		copy(cloned, messages)

		content := messages[0].Content
		newContent := &ChatMessageContent{}
		if content != nil && content.ContentStr != nil {
			newStr := *content.ContentStr + "\n\n" + MultiAgentPrimingReminderText
			newContent.ContentStr = &newStr
		} else {
			var newBlocks []ChatContentBlock
			if content != nil && len(content.ContentBlocks) > 0 {
				newBlocks = make([]ChatContentBlock, len(content.ContentBlocks), len(content.ContentBlocks)+1)
				copy(newBlocks, content.ContentBlocks)
			}
			text := MultiAgentPrimingReminderText
			newBlocks = append(newBlocks, ChatContentBlock{
				Type: ChatContentBlockTypeText,
				Text: &text,
			})
			newContent.ContentBlocks = newBlocks
		}

		cloned[0].Content = newContent
		return cloned, nil
	}

	// No leading system or developer message existed: prepend one
	if err := report.Note(
		FeatureMultiAgentPriming,
		"messages[0]",
		"multi-agent protocol reminder prepended as leading system turn",
	); err != nil {
		return nil, err
	}

	text := MultiAgentPrimingReminderText
	sysMsg := ChatMessage{
		Role: ChatMessageRoleSystem,
		Content: &ChatMessageContent{
			ContentBlocks: []ChatContentBlock{
				{
					Type: ChatContentBlockTypeText,
					Text: &text,
				},
			},
		},
	}
	res := make([]ChatMessage, 0, len(messages)+1)
	res = append(res, sysMsg)
	res = append(res, messages...)
	return res, nil
}
