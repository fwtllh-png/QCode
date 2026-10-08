package contextview

import (
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
)

func TestConversationProjectionDeduplicatesAndCoversParent(t *testing.T) {
	source := agentcontext.IndexConversationAnswer("thread", "report", 1, "1. 父项前提\n   1. 子项定义\n2. 别的定义")
	state := &agentcontext.ConversationState{}
	if err := state.Add(source); err != nil {
		t.Fatal(err)
	}
	history := []provider.Message{{Role: provider.RoleAssistant, Turn: 1, Blocks: []provider.ContentBlock{{Type: provider.ContentText, Text: source.Text}}}}
	raw := SelectConversation(state, agentcontext.Plan{}, history)
	if len(raw) != 1 || raw[0].Coverage.Representation != "raw_history" {
		t.Fatal("raw answer duplicated")
	}
	if err := state.Select(agentcontext.NewConversationSelection(nil, []string{source.Items[1].ID}, 2, "select", "继续子项"), nil); err != nil {
		t.Fatal(err)
	}
	extract := SelectConversation(state, agentcontext.Plan{}, nil)
	if len(extract) != 1 || len(extract[0].Coverage.Ranges) != 1 || extract[0].Coverage.Ranges[0].Start != source.Items[0].Start || extract[0].Coverage.Ranges[0].End != source.Items[0].End {
		t.Fatal("nested item lost its parent definition")
	}
}
