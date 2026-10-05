package agentcontext

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
)

func TestNormalizePreservesOnlyUniqueOrderedToolPairs(t *testing.T) {
	snapshot := NewMessageLedger(LedgerInput{History: []provider.Message{
		toolCallContextMessage("paired", 1),
		toolResultContextMessage("paired", 1),
		toolCallContextMessage("missing", 2),
		toolResultContextMessage("result-only", 2),
		toolCallContextMessage("duplicate", 3),
		toolCallContextMessage("duplicate", 3),
		toolResultContextMessage("duplicate", 3),
		toolResultContextMessage("reversed", 4),
		toolCallContextMessage("reversed", 4),
	}}).Snapshot()
	normalized, receipt, err := snapshot.Normalize(model.Capabilities{
		ToolCalls: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.ToolCalls != 5 || receipt.ToolResults != 4 ||
		receipt.PairedCalls != 1 || receipt.DroppedOrphans != 7 ||
		receipt.ModelVisibleOrphans != 0 {
		t.Fatalf("receipt=%+v", receipt)
	}
	messages := normalized.Messages()
	if len(messages) != 2 ||
		messages[0].Blocks[0].ToolCall.ID != "paired" ||
		messages[1].Blocks[0].ToolResult.CallID != "paired" {
		t.Fatalf("normalized=%+v", messages)
	}
}

func TestNormalizeProjectsUnsupportedModalities(t *testing.T) {
	snapshot := NewMessageLedger(LedgerInput{History: []provider.Message{{
		Role: provider.RoleUser,
		Blocks: []provider.ContentBlock{{
			Type: provider.ContentImage,
			Attachment: &provider.Attachment{
				Name: "screen.png", MediaType: "image/png", Data: []byte("png"),
			},
		}},
	}, {
		Role: provider.RoleAssistant,
		Blocks: []provider.ContentBlock{{
			Type: provider.ContentReasoning, Text: "private reasoning",
		}, {
			Type: provider.ContentText, Text: "public answer",
		}},
	}}}).Snapshot()
	normalized, receipt, err := snapshot.Normalize(model.Capabilities{})
	if err != nil {
		t.Fatal(err)
	}
	messages := normalized.Messages()
	if receipt.ProjectedImages != 1 || receipt.DroppedReasoning != 1 ||
		len(messages) != 2 ||
		messages[0].Blocks[0].Type != provider.ContentText ||
		!strings.Contains(messages[0].Blocks[0].Text, "screen.png") ||
		messages[1].Text() != "public answer" {
		t.Fatalf("receipt=%+v messages=%+v", receipt, messages)
	}
	supported, supportedReceipt, err := snapshot.Normalize(model.Capabilities{
		ImageInput: true, Reasoning: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if supportedReceipt.ProjectedImages != 0 ||
		supportedReceipt.DroppedReasoning != 0 ||
		supported.Messages()[0].Blocks[0].Type != provider.ContentImage {
		t.Fatalf(
			"supported receipt=%+v messages=%+v",
			supportedReceipt, supported.Messages(),
		)
	}
}

func TestNormalizePreservesReasoningBoundToToolCall(t *testing.T) {
	snapshot := NewMessageLedger(LedgerInput{History: []provider.Message{
		{
			Role: provider.RoleAssistant,
			Blocks: []provider.ContentBlock{
				{Type: provider.ContentReasoning, Text: "required replay"},
				{Type: provider.ContentToolCall, ToolCall: &provider.ToolCall{
					ID: "call_1", Name: "fixture", Arguments: `{}`,
				}},
			},
		},
		toolResultContextMessage("call_1", 1),
	}}).Snapshot()

	normalized, receipt, err := snapshot.Normalize(model.Capabilities{
		ToolCalls: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	messages := normalized.Messages()
	if receipt.DroppedReasoning != 0 ||
		len(messages) != 2 ||
		len(messages[0].Blocks) != 2 ||
		messages[0].Blocks[0].Type != provider.ContentReasoning {
		t.Fatalf("receipt=%+v messages=%+v", receipt, messages)
	}
}

func TestNormalizePreservesReasoningBoundToVersionedReplay(t *testing.T) {
	message := provider.Message{
		Role: provider.RoleAssistant,
		Blocks: []provider.ContentBlock{
			{Type: provider.ContentReasoning, Text: "required replay"},
			{Type: provider.ContentText, Text: "answer"},
		},
	}
	message.Provenance = &provider.AssistantProvenance{
		Adapter:  model.AdapterOpenAICompatible,
		Provider: "provider",
		Model:    "model",
		Replay: &provider.ReplayState{
			Version:       provider.ReplayVersion,
			ContentDigest: provider.MessageContentDigest(message),
			Data:          json.RawMessage(`{"state":"replay"}`),
		},
	}
	normalized, receipt, err := NewMessageLedger(
		LedgerInput{History: []provider.Message{message}},
	).Snapshot().Normalize(model.Capabilities{})
	if err != nil {
		t.Fatal(err)
	}
	messages := normalized.Messages()
	if receipt.DroppedReasoning != 0 ||
		len(messages) != 1 ||
		len(messages[0].Blocks) != 2 ||
		messages[0].Provenance == nil ||
		messages[0].Provenance.Replay == nil {
		t.Fatalf("receipt=%+v messages=%+v", receipt, messages)
	}
}

func TestNormalizeDropsReasoningAfterOrphanToolCallIsRemoved(t *testing.T) {
	message := provider.Message{
		Role: provider.RoleAssistant,
		Blocks: []provider.ContentBlock{
			{
				Type: provider.ContentReasoning,
				ID:   "rs_orphan",
				Text: "orphan reasoning",
			},
			{Type: provider.ContentToolCall, ToolCall: &provider.ToolCall{
				ID: "orphan", Name: "fixture", Arguments: `{}`,
			}},
		},
	}
	message.Provenance = &provider.AssistantProvenance{
		Adapter:  model.AdapterOpenAICompatible,
		Provider: "provider",
		Model:    "model",
		Replay: &provider.ReplayState{
			Version:       provider.ReplayVersion,
			ContentDigest: provider.MessageContentDigest(message),
			Data:          json.RawMessage(`{"state":"orphan"}`),
		},
	}
	snapshot := NewMessageLedger(
		LedgerInput{History: []provider.Message{message}},
	).Snapshot()

	normalized, receipt, err := snapshot.Normalize(model.Capabilities{
		ToolCalls: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(normalized.Messages()) != 0 ||
		receipt.DroppedOrphans != 1 ||
		receipt.DroppedReasoning != 1 {
		t.Fatalf(
			"receipt=%+v messages=%+v",
			receipt,
			normalized.Messages(),
		)
	}
}

func TestNormalizeIsolatesSourceAndAccessorContent(t *testing.T) {
	for _, removeOrphan := range []bool{false, true} {
		name := "unchanged"
		if removeOrphan {
			name = "filtered"
		}
		t.Run(name, func(t *testing.T) {
			message := provider.Message{
				Role: provider.RoleAssistant,
				Blocks: []provider.ContentBlock{
					{Type: provider.ContentText, Text: "before image"},
					{Type: provider.ContentImage, Attachment: &provider.Attachment{
						Name: "screen.png", MediaType: "image/png", Data: []byte("image"),
					}},
					{Type: provider.ContentText, Text: "after image"},
				},
			}
			if removeOrphan {
				message.Blocks = append([]provider.ContentBlock{{
					Type:     provider.ContentToolCall,
					ToolCall: &provider.ToolCall{ID: "orphan", Name: "fixture", Arguments: `{}`},
				}}, message.Blocks...)
			}
			message.Provenance = &provider.AssistantProvenance{
				Adapter: model.AdapterOpenAICompatible, Provider: "provider", Model: "model",
				Replay: &provider.ReplayState{
					Version: provider.ReplayVersion, ContentDigest: provider.MessageContentDigest(message),
					Data: json.RawMessage(`{"state":"replay"}`),
				},
			}
			source := NewMessageLedger(LedgerInput{History: []provider.Message{message}}).Snapshot()
			before := source.Messages()
			normalized, receipt, err := source.Normalize(model.Capabilities{ImageInput: true, ToolCalls: true})
			if err != nil {
				t.Fatal(err)
			}
			want := normalized.Messages()
			if len(want) != 1 || len(want[0].Blocks) != 3 ||
				want[0].Blocks[0].Text != "before image" || want[0].Blocks[2].Text != "after image" {
				t.Fatalf("normalized block order changed: %+v", want)
			}
			if (want[0].Provenance.Replay == nil) != removeOrphan ||
				(receipt.DroppedOrphans == 1) != removeOrphan {
				t.Fatalf("replay/receipt mismatch: message=%+v receipt=%+v", want[0], receipt)
			}
			for _, accessor := range []struct {
				name string
				read func() []provider.Message
			}{
				{"messages", normalized.Messages},
				{"partition", func() []provider.Message { return normalized.Partition(KindHistory) }},
				{"items", func() []provider.Message { return []provider.Message{normalized.Items()[0].Message} }},
			} {
				copy := accessor.read()
				copy[0].Blocks[0].Text = "changed"
				copy[0].Blocks[1].Attachment.Data[0] = 'X'
				copy[0].Provenance.Model = "changed"
				if copy[0].Provenance.Replay != nil {
					copy[0].Provenance.Replay.Data[0] = '['
				}
				if !reflect.DeepEqual(normalized.Messages(), want) || !reflect.DeepEqual(source.Messages(), before) {
					t.Fatalf("%s mutation or normalization changed snapshot content", accessor.name)
				}
			}
		})
	}
}

func toolCallContextMessage(id string, turn uint64) provider.Message {
	return provider.Message{Role: provider.RoleAssistant, Turn: turn,
		Blocks: []provider.ContentBlock{{Type: provider.ContentToolCall,
			ToolCall: &provider.ToolCall{ID: id, Name: "fixture", Arguments: `{}`}}}}
}

func toolResultContextMessage(id string, turn uint64) provider.Message {
	return provider.Message{Role: provider.RoleTool, Turn: turn,
		Blocks: []provider.ContentBlock{{Type: provider.ContentToolResult,
			ToolResult: &provider.ToolResult{CallID: id, Content: "ok"}}}}
}
