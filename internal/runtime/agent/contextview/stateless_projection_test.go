package contextview

import (
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
)

func TestStatelessProjectionKeepsWorldPatchesAndToolCallReasoning(t *testing.T) {
	worldV1 := provider.TextMessage(provider.RoleSystem, "world v1")
	full, err := agentcontext.ProjectWorld([]agentcontext.WorldSection{{
		ID: "workspace", Digest: "v1", Message: &worldV1,
	}}, agentcontext.WorldBaseline{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	worldV2 := provider.TextMessage(provider.RoleSystem, "world v2")
	patch, err := agentcontext.ProjectWorld([]agentcontext.WorldSection{{
		ID: "workspace", Digest: "v2", Message: &worldV2,
	}}, full.Baseline, full.Messages)
	if err != nil {
		t.Fatal(err)
	}
	call := statelessToolCall("closed", 1)
	call.Blocks = append([]provider.ContentBlock{
		{Type: provider.ContentReasoning, Text: "consumed reasoning"},
		{Type: provider.ContentText, Text: "running the tool"},
	}, call.Blocks...)
	pending := statelessToolCall("pending", 1)
	pending.Blocks = append([]provider.ContentBlock{{
		Type: provider.ContentReasoning, Text: "pending reasoning",
	}}, pending.Blocks...)
	history := append(agentcontext.CloneMessages(full.Messages), call, statelessToolResult("closed", 1))
	history = append(history, patch.Messages...)
	history = append(history, pending)
	projected := ProjectStatelessHistory(history)
	if len(projected) != 5 || projected[0].Text() != "world v1" ||
		len(projected[1].Blocks) != 3 ||
		projected[1].Blocks[0].Type != provider.ContentReasoning ||
		projected[1].Blocks[1].Type != provider.ContentText ||
		projected[1].Blocks[1].Text != "running the tool" ||
		projected[1].Blocks[2].ToolCall == nil ||
		projected[3].Text() != "world v2" ||
		projected[4].Blocks[0].Type != provider.ContentReasoning {
		t.Fatalf("projected messages = %+v", projected)
	}
	if history[1].Blocks[0].Type != provider.ContentReasoning {
		t.Fatal("durable history was mutated")
	}
}

func TestStatelessProjectionKeepsClosedRoundAnalysisText(t *testing.T) {
	history := []provider.Message{
		{
			Role: provider.RoleUser, Turn: 1,
			Blocks: []provider.ContentBlock{{
				Type: provider.ContentText, Text: "why is settlement failing",
			}},
		},
		{
			Role: provider.RoleAssistant, Turn: 1,
			Blocks: []provider.ContentBlock{
				{
					Type: provider.ContentText,
					Text: "已排除缓存问题，下一步只修改取消结算路径",
				},
				{
					Type: provider.ContentToolCall,
					ToolCall: &provider.ToolCall{
						ID: "search-1", Name: "search_text",
						Arguments: `{"query":"cancel settlement"}`,
					},
				},
			},
		},
		statelessToolResult("search-1", 1),
	}
	projected := ProjectStatelessHistory(history)
	if projected[1].Text() != "已排除缓存问题，下一步只修改取消结算路径" ||
		projected[1].Blocks[1].ToolCall == nil {
		t.Fatalf("closed-round analysis was dropped: %+v", projected[1])
	}
	if history[1].Text() != "已排除缓存问题，下一步只修改取消结算路径" {
		t.Fatal("durable history was mutated")
	}
}

func TestStatelessProjectionKeepsCurrentTurnWorldPatchesAppendOnly(t *testing.T) {
	worldV1 := provider.TextMessage(provider.RoleSystem, "world v1")
	worldV1.Turn = 7
	full, err := agentcontext.ProjectWorld([]agentcontext.WorldSection{{
		ID: "workspace", Digest: "v1", Message: &worldV1,
	}}, agentcontext.WorldBaseline{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	worldV2 := provider.TextMessage(provider.RoleSystem, "world v2")
	worldV2.Turn = 7
	patch, err := agentcontext.ProjectWorld([]agentcontext.WorldSection{{
		ID: "workspace", Digest: "v2", Message: &worldV2,
	}}, full.Baseline, full.Messages)
	if err != nil {
		t.Fatal(err)
	}
	projected := ProjectStatelessHistory(
		append(agentcontext.CloneMessages(full.Messages), patch.Messages...),
	)
	if len(projected) != 2 || projected[0].Text() != "world v1" ||
		projected[1].Text() != "world v2" {
		t.Fatalf("current-turn patches were rewritten: %+v", projected)
	}
}

func TestStatelessProjectionKeepsHistoricalImagesByteIdentical(t *testing.T) {
	history := []provider.Message{{
		Role: provider.RoleUser, Turn: 1, Blocks: []provider.ContentBlock{{
			Type: provider.ContentImage, Attachment: &provider.Attachment{
				Name: "old.png", MediaType: "image/png", Data: []byte("image"),
				Handle: "image_0123456789abcdef",
			},
		}},
	}, {
		Role: provider.RoleAssistant, Turn: 2,
		Blocks: []provider.ContentBlock{{Type: provider.ContentText, Text: "facts"}},
	}, {
		Role: provider.RoleUser, Turn: 3,
		Blocks: []provider.ContentBlock{{Type: provider.ContentText, Text: "continue"}},
	}}
	projected := ProjectStatelessHistory(history)
	if projected[0].Blocks[0].Type != provider.ContentImage ||
		string(projected[0].Blocks[0].Attachment.Data) != "image" ||
		projected[0].Blocks[0].Attachment.Handle != "image_0123456789abcdef" {
		t.Fatalf("historical image was rewritten: %+v", projected)
	}
	if string(history[0].Blocks[0].Attachment.Data) != "image" {
		t.Fatal("durable image history was mutated")
	}
}

func TestStatelessProjectionRetainsImageWithoutHandle(t *testing.T) {
	history := []provider.Message{{
		Role: provider.RoleUser, Turn: 1, Blocks: []provider.ContentBlock{{
			Type: provider.ContentImage, Attachment: &provider.Attachment{
				Name: "old.png", MediaType: "image/png", Data: []byte("image"),
			},
		}},
	}, {Role: provider.RoleUser, Turn: 3, Blocks: []provider.ContentBlock{{
		Type: provider.ContentText, Text: "continue",
	}}}}
	projected := ProjectStatelessHistory(history)
	if projected[0].Blocks[0].Type != provider.ContentImage {
		t.Fatalf("projection=%+v", projected)
	}
}

func statelessToolCall(id string, turn uint64) provider.Message {
	return provider.Message{Role: provider.RoleAssistant, Turn: turn,
		Blocks: []provider.ContentBlock{{Type: provider.ContentToolCall,
			ToolCall: &provider.ToolCall{ID: id, Name: "fixture", Arguments: `{}`}}}}
}

func statelessToolResult(id string, turn uint64) provider.Message {
	return provider.Message{Role: provider.RoleTool, Turn: turn,
		Blocks: []provider.ContentBlock{{Type: provider.ContentToolResult,
			ToolResult: &provider.ToolResult{CallID: id, Content: "ok"}}}}
}
