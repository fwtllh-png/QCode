package engine

import (
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
)

// A text-only sample is the candidate final answer: its deltas stream live,
// every delta carries the sample identity a retraction would name, and no
// retraction is emitted because nothing invalidates the text.
func TestTextOnlySamplesStreamDeltasWithSampleIdentity(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		textStream("the complete answer"),
	}}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))

	var deltas []Event
	if _, err := engine.Run(t.Context(), "answer directly", func(event Event) error {
		if event.State == Streaming && event.Block != nil &&
			event.Block.Type == provider.ContentText {
			deltas = append(deltas, event)
		}
		if event.OutputDiscarded != nil {
			t.Errorf("text-only sample was retracted: %+v", event.OutputDiscarded)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(deltas) == 0 {
		t.Fatal("text-only sample streamed no deltas")
	}
	var streamed strings.Builder
	sampleIDs := make(map[string]struct{}, 1)
	for _, event := range deltas {
		streamed.WriteString(event.Block.Text)
		if event.SampleID == "" {
			t.Errorf("delta %q carries no sample id", event.Block.Text)
		}
		sampleIDs[event.SampleID] = struct{}{}
	}
	if streamed.String() != "the complete answer" {
		t.Fatalf("streamed %q", streamed.String())
	}
	if len(sampleIDs) != 1 {
		t.Fatalf("one sample produced %d sample ids: %v", len(sampleIDs), sampleIDs)
	}
}
