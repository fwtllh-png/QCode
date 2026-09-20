package engine

import (
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	providerfixture "github.com/fwtllh-png/QCode/internal/adapter/provider/fixture"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
)

// TestEngineAttachesCostToStreamingUsage pins that a usage event carries the
// cost of the tokens it reports. Cost used to be computed only at turn
// completion, which never reached the protocol usage event, so every persisted
// usage row recorded a zero cost even with known pricing.
func TestEngineAttachesCostToStreamingUsage(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventTextDelta, Text: "done"},
			{Type: provider.EventUsage, Usage: &provider.Usage{
				InputTokens: 1_000_000, OutputTokens: 2_000_000,
			}},
			{Type: provider.EventMessageStop},
		}},
	}}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))

	var streamed []Event
	result, err := engine.Run(t.Context(), "work", func(event Event) error {
		if event.State == Streaming && event.Usage != nil {
			streamed = append(streamed, event)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(streamed) != 1 {
		t.Fatalf("streaming usage events = %d, want 1", len(streamed))
	}
	// testRoute prices input and output at 1 USD per million tokens.
	const wantCost = 3.0
	if streamed[0].CostUSD != wantCost {
		t.Fatalf("streaming usage cost = %v, want %v", streamed[0].CostUSD, wantCost)
	}
	if result.CostUSD != wantCost {
		t.Fatalf("turn cost = %v, want %v", result.CostUSD, wantCost)
	}
	if !streamed[0].CostKnown {
		t.Fatal("usage event reports unknown cost for a priced model")
	}
	if streamed[0].Sample != 1 {
		t.Fatalf("usage sample = %d, want the first provider call", streamed[0].Sample)
	}
	if streamed[0].Provider == "" || streamed[0].Model == "" {
		t.Fatalf("usage event does not name the model that answered: %+v", streamed[0])
	}
	if streamed[0].ModelMetadata == nil ||
		streamed[0].ModelMetadata.Limits != "fixture" ||
		streamed[0].ModelMetadata.Capabilities != "fixture" {
		t.Fatalf("usage event metadata provenance = %+v", streamed[0].ModelMetadata)
	}
}

// TestEngineNumbersUsageBySampleAcrossCalls pins the boundary an aggregator needs.
// Usage is cumulative within one provider call but not across calls, so without a
// per-call number a consumer cannot tell "the same call reporting more" from "the
// next call reporting its own tokens" — the ambiguity that made persisted usage
// double-count.
func TestEngineNumbersUsageBySampleAcrossCalls(t *testing.T) {
	registry := tool.NewRegistry(nil, nil)
	if err := registry.Register(&echoTool{}); err != nil {
		t.Fatal(err)
	}
	runtime := &scriptedProvider{streams: []provider.Stream{
		// First call asks for a tool, reporting input before output the way
		// message_start-style usage events do.
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventUsage, Usage: &provider.Usage{InputTokens: 100}},
			{Type: provider.EventToolCallDelta, Index: 0, ToolCall: &provider.ToolCallFragment{
				ID: "call-1", Name: "echo", Arguments: `{"text":"hi"}`,
			}},
			{Type: provider.EventUsage, Usage: &provider.Usage{InputTokens: 100, OutputTokens: 20}},
			{Type: provider.EventMessageStop},
		}},
		// Second call answers with text.
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventTextDelta, Text: "done"},
			{Type: provider.EventUsage, Usage: &provider.Usage{
				InputTokens: 150, OutputTokens: 30,
			}},
			{Type: provider.EventMessageStop},
		}},
	}}
	engine := newEngine(t, runtime, registry)

	var streamed []Event
	if _, err := engine.Run(t.Context(), "work", func(event Event) error {
		if event.State == Streaming && event.Usage != nil {
			streamed = append(streamed, event)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(streamed) != 3 {
		t.Fatalf("streaming usage events = %d, want 3", len(streamed))
	}
	// The first call's two reports share a sample and are cumulative over it;
	// the second call starts a new sample and counts only its own tokens.
	if streamed[0].Sample != 1 || streamed[1].Sample != 1 || streamed[2].Sample != 2 {
		t.Fatalf("samples = %d/%d/%d, want 1/1/2",
			streamed[0].Sample, streamed[1].Sample, streamed[2].Sample)
	}
	if streamed[1].Usage.InputTokens != 100 || streamed[1].Usage.OutputTokens != 20 {
		t.Fatalf("first call total = %+v, want 100 in / 20 out", streamed[1].Usage)
	}
	if streamed[2].Usage.InputTokens != 150 || streamed[2].Usage.OutputTokens != 30 {
		t.Fatalf("second call total = %+v, want only its own tokens", streamed[2].Usage)
	}
}

func TestEngineDoesNotRepublishIdenticalOrDoubledUsage(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventTextDelta, Text: "done"},
			{Type: provider.EventUsage, Usage: &provider.Usage{InputTokens: 100, OutputTokens: 20}},
			{Type: provider.EventUsage, Usage: &provider.Usage{InputTokens: 100, OutputTokens: 20}},
			{Type: provider.EventUsage, Usage: &provider.Usage{InputTokens: 200, OutputTokens: 40}},
			{Type: provider.EventMessageStop},
		}},
	}}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	var streamed []Event
	if _, err := engine.Run(t.Context(), "work", func(event Event) error {
		if event.State == Streaming && event.Usage != nil {
			streamed = append(streamed, event)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(streamed) != 1 {
		t.Fatalf("streaming usage events = %d, want the first snapshot only", len(streamed))
	}
	if streamed[0].Usage.InputTokens != 100 || streamed[0].Usage.OutputTokens != 20 {
		t.Fatalf("published usage = %+v, want the original snapshot", streamed[0].Usage)
	}
}
