package engine

import (
	"errors"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	providerfixture "github.com/fwtllh-png/QCode/internal/adapter/provider/fixture"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestCommentaryPrecedesToolWithoutReleasingFinalOutput(t *testing.T) {
	for _, projectionFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "published", true: "projection fails"}[projectionFails], func(t *testing.T) {
			engine, model := streamingTurn(t, &streamingTool{chunks: []string{"found"}}, nil)
			model.streams[0] = &providerfixture.SliceStream{Events: []provider.StreamEvent{
				{Type: provider.EventMessageStart},
				{Type: provider.EventTextDelta, Text: "Checking the handler."},
				{Type: provider.EventToolCallDelta, ToolCall: &provider.ToolCallFragment{
					Index: 0, ID: "call_1", Name: "stream", Arguments: `{}`,
				}},
				{Type: provider.EventMessageStop},
			}}
			var messages []protocol.CommentaryCompletedData
			var sawTool, streamedDelta, retracted bool
			result, err := engine.Run(t.Context(), "Inspect the handler", func(event Event) error {
				if event.Commentary != nil {
					if sawTool {
						t.Error("commentary arrived after its tool")
					}
					messages = append(messages, *event.Commentary)
					if projectionFails {
						return errors.New("projection unavailable")
					}
				}
				if event.ToolCall != nil {
					sawTool = true
				}
				if event.State == Streaming && event.Block != nil &&
					event.Block.Type == provider.ContentText {
					streamedDelta = true
				}
				if event.State == Streaming && event.OutputDiscarded != nil {
					if !streamedDelta {
						t.Error("output was discarded before any delta streamed")
					}
					if sawTool {
						t.Error("narration retraction arrived after the tool phase")
					}
					if event.OutputDiscarded.SampleID == "" ||
						event.OutputDiscarded.Reason == "" {
						t.Errorf("retraction lacks identity: %+v", event.OutputDiscarded)
					}
					retracted = true
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(messages) != 1 || messages[0].Text != "Checking the handler." ||
				len(messages[0].CallIDs) != 1 || messages[0].CallIDs[0] != "call_1" ||
				messages[0].MessageID == "" || messages[0].SampleID == "" ||
				!sawTool || !streamedDelta || !retracted || result.Text != "done" {
				t.Fatalf("messages=%+v tool=%v streamed=%v retracted=%v result=%+v",
					messages, sawTool, streamedDelta, retracted, result)
			}
			if len(model.requests) != 2 {
				t.Fatalf("commentary added model calls: %d", len(model.requests))
			}
			count := 0
			for _, message := range engine.history {
				if message.Role != provider.RoleAssistant {
					continue
				}
				for _, block := range message.Blocks {
					count += strings.Count(block.Text, "Checking the handler.")
				}
			}
			if count != 1 {
				t.Fatalf("commentary appears %d times in durable assistant history", count)
			}
		})
	}
}

func TestCommentaryAbsentForPlainAnswer(t *testing.T) {
	engine, model := streamingTurn(t, &streamingTool{}, nil)
	model.streams = model.streams[1:]
	_, err := engine.Run(t.Context(), "Answer", func(event Event) error {
		if event.Commentary != nil {
			t.Fatal("plain answer was released as commentary")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
