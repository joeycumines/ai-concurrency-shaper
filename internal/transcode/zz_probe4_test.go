package transcode

import (
	"fmt"
	"testing"
)

func TestProbeChatDevEmptyVsNonEmpty(t *testing.T) {
	for _, devText := range []string{"", "dev"} {
		for _, dev := range []bool{true} {
			request := CanonicalRequest{
				ClientModel: "m",
				Turns: []CanonicalTurn{
					{Role: CanonicalDeveloper, Parts: []CanonicalPart{CanonicalText{Text: devText}}},
					textTurn(CanonicalSystem, "s"),
					textTurn(CanonicalUser, "u"),
				},
			}
			context := testExchangeContext()
			context.LossPolicy = LossPolicy{Allowed: map[Feature]struct{}{
				FeatureMidConversationSystem: {},
				FeatureDeveloperRole:         {},
			}}
			rendered, report, err := RenderChatRequest(request, context, ChatCapabilities{DeveloperRole: dev})
			fmt.Printf("devText=%q dev=%v BODY=%s ERR=%v\n", devText, dev, rendered, err)
			for _, loss := range report.Losses {
				fmt.Printf("   ENTRY kind=%v feature=%s path=%s\n", loss.Kind, loss.Feature, loss.Path)
			}
		}
	}
}
