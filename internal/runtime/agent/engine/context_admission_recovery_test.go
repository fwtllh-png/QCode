package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	providerfixture "github.com/fwtllh-png/QCode/internal/adapter/provider/fixture"
	"github.com/fwtllh-png/QCode/internal/observability/verify"
)

func TestContextReservationRejectionAllowsSmallerBatch(t *testing.T) {
	f := newVerifyGateFixture(t, VerifyOptions{}, &scriptedVerifier{receipts: []verify.Receipt{passedReceipt()}}, 0, 8)
	arguments := `{"path":"value.txt","old":"before","new":"after"}`
	f.provider.streams = []provider.Stream{f.provider.streams[0], &providerfixture.SliceStream{Events: []provider.StreamEvent{
		{Type: provider.EventToolCallDelta, ToolCall: &provider.ToolCallFragment{Index: 0, ID: "large-a", Name: "file_edit", Arguments: arguments}},
		{Type: provider.EventToolCallDelta, ToolCall: &provider.ToolCallFragment{Index: 1, ID: "large-b", Name: "file_edit", Arguments: arguments}},
		{Type: provider.EventMessageStop},
	}}, toolCallStream("small", "file_edit", arguments), textStream("done")}
	var rejected int
	result, err := f.engine.RunForTurn(t.Context(), "reservation-recovery", "edit value.txt", func(event Event) error {
		if event.ToolCall == nil || event.Result == nil {
			return nil
		}
		if event.ToolCall.ID == "read" {
			current := f.engine.ContextAdmission(nil, nil)
			f.engine.options.Context.TruthRetention.MandatoryMaxEntities = current.ProjectedEntities + 1
		}
		if event.ToolCall.ID == "large-a" || event.ToolCall.ID == "large-b" {
			rejected++
			if !event.Result.IsError || event.Result.Metadata["error_category"] != "context_reservation_exceeded" || f.contents(t) != "before\n" {
				t.Fatalf("batch was executed or not rejected: %+v", event.Result)
			}
		}
		return nil
	})
	if err != nil || result.State != Completed || rejected != 2 || f.contents(t) != "after\n" {
		t.Fatalf("state=%s rejected=%d contents=%q err=%v", result.State, rejected, f.contents(t), err)
	}
	encoded, _ := json.Marshal(f.provider.requests[2].Messages)
	if !strings.Contains(string(encoded), "split_batch_or_resolve_obligations") {
		t.Fatal("model did not receive actionable batch rejection")
	}
}
