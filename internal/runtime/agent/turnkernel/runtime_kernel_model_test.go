package turnkernel

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	providerassembly "github.com/fwtllh-png/QCode/internal/adapter/provider/assembly"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestModelSampleProgressOwnership(t *testing.T) {
	for _, outcome := range []string{"accepted", "invalid_extension", "persistence_failure"} {
		t.Run(outcome, func(t *testing.T) {
			fail := false
			store := &failingDomainFactStore{
				TerminalEnvelopeStore: NewMemoryTerminalEnvelopeStore(nil, nil),
				fail:                  &fail,
			}
			runtime, err := NewStoreCoordinatorRuntime(store)
			if err != nil {
				t.Fatal(err)
			}
			kernel, err := NewRuntimeKernel(
				KernelIdentity{TurnID: "turn-progress", ProfileRevision: 1},
				protocol.TurnIntentAnswer, "act", nil, false,
				nil, nil, nil, nil, nil, DefaultPolicy(), runtime,
			)
			if err != nil {
				t.Fatal(err)
			}
			if err := kernel.BeginModelSample(t.Context(), "sample-1"); err != nil {
				t.Fatal(err)
			}
			assembly := providerassembly.NewResponseAssembly("sample-1")
			if err := assembly.BeginTransport(provider.TransportMetadata{
				LogicalRequestID: "sample-1", TransportRequestID: "transport-1", Attempt: 1,
			}); err != nil {
				t.Fatal(err)
			}
			for _, event := range []provider.StreamEvent{
				{Type: provider.EventTextDelta, EventID: "text-1", Text: "partial"},
				{Type: provider.EventToolCallDelta, ToolCall: &provider.ToolCallFragment{
					Index: 0, ID: "call-1", Name: "read", Arguments: `{"path":`,
				}},
				{Type: provider.EventReplayState, Replay: &provider.ReplayState{
					Version: provider.ReplayVersion, Data: json.RawMessage(`{"id":1}`),
				}},
				{Type: provider.EventResponseState, Response: &provider.ResponseState{
					ID: "response-1", Output: []json.RawMessage{json.RawMessage(`{"id":1}`)},
				}},
			} {
				if _, err := assembly.Apply(event); err != nil {
					t.Fatal(err)
				}
			}
			if err := kernel.RecordModelSampleProgress("sample-1", assembly); err != nil {
				t.Fatal(err)
			}
			before := kernel.Snapshot()
			factsBefore, err := store.LoadDomainFacts(t.Context(), "turn-progress")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := assembly.Apply(provider.StreamEvent{
				Type: provider.EventTextDelta, Text: " answer",
			}); err != nil {
				t.Fatal(err)
			}
			if outcome == "invalid_extension" {
				assembly.Segments[0].Blocks[0].Text = "rewritten"
			}
			fail = outcome == "persistence_failure"
			err = kernel.RecordModelSampleProgress("sample-1", assembly)
			if (err == nil) != (outcome == "accepted") {
				t.Fatalf("RecordModelSampleProgress() error = %v", err)
			}
			want := before
			if outcome == "accepted" {
				want.SampleLedger["sample-1"] = kernel.Snapshot().SampleLedger["sample-1"]
				if !reflect.DeepEqual(want.SampleLedger["sample-1"].Assembly, assembly) {
					t.Fatal("accepted progress differs from submitted assembly")
				}
			}
			wantFacts, err := store.LoadDomainFacts(t.Context(), "turn-progress")
			if err != nil {
				t.Fatal(err)
			}
			if outcome != "accepted" && !reflect.DeepEqual(wantFacts, factsBefore) {
				t.Fatal("rejected progress changed durable facts")
			}

			// Both the caller's assembly and the public accessor remain mutable
			// without changing either authoritative state or committed facts.
			for _, exposed := range []*providerassembly.ResponseAssembly{
				assembly, kernel.SampleAssembly("sample-1"),
			} {
				segment := &exposed.Segments[0]
				segment.Blocks[0].Text = "mutated"
				segment.ToolFragments[0].Arguments = "mutated"
				segment.Replay.Data[2] = 'x'
				segment.Response.Output[0][2] = 'x'
				clear(segment.Seen)
				segment.Transport.TransportRequestID = "mutated"
			}
			if !reflect.DeepEqual(kernel.Snapshot(), want) ||
				!reflect.DeepEqual(kernel.SampleAssembly("sample-1"), want.SampleLedger["sample-1"].Assembly) {
				t.Fatal("mutable progress escaped into kernel state")
			}
			facts, err := store.LoadDomainFacts(t.Context(), "turn-progress")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(facts, wantFacts) {
				t.Fatal("mutable progress escaped into durable facts")
			}
		})
	}
}
