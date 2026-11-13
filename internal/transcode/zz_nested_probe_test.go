package transcode

import (
	"fmt"
	"testing"
)

// Probe: can a Messages decoder produce a CanonicalFunctionResult whose Parts
// contain a part the new central restriction rejects?
func TestProbeNestedToolResult(t *testing.T) {
	cases := map[string]string{
		"nested tool_result": `{"model":"m","max_tokens":16,"messages":[
			{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"f","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[
				{"type":"tool_result","tool_use_id":"t2","content":"x"}]}]}]}`,
		"nested tool_use": `{"model":"m","max_tokens":16,"messages":[
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[
				{"type":"tool_use","id":"t9","name":"g","input":{}}]}]}]}`,
		"nested server tool": `{"model":"m","max_tokens":16,"messages":[
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[
				{"type":"server_tool_use","id":"s1","name":"web_search","input":{}}]}]}]}`,
	}
	for name, body := range cases {
		result, err := DecodeMessagesRequest([]byte(body), LossPolicy{Allowed: map[Feature]struct{}{
			FeatureAnthropicServerTools: {},
		}})
		if err != nil {
			fmt.Printf("%s: DECODE ERR: %v\n", name, err)
			continue
		}
		fmt.Printf("%s: decoded turns=%#v\n", name, result.Request.Turns)
		verr := ValidateCanonicalRequest(result.Request)
		fmt.Printf("%s: VALIDATE ERR: %v\n", name, verr)
		_, _, rerr := RenderChatRequest(result.Request, testExchangeContext(), ChatCapabilities{})
		fmt.Printf("%s: CHAT RENDER ERR: %v\n", name, rerr)
	}
}
