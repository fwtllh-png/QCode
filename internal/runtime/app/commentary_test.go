package app

import (
	"context"
	"testing"

	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	"github.com/fwtllh-png/QCode/internal/runtime/app/eventhub"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestCommentaryTerminalOutboxRecoversAndDeduplicatesLiveMessage(t *testing.T) {
	for _, published := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing live event", true: "already published"}[published], func(t *testing.T) {
			envelope := c5TerminalEnvelope(t)
			message := turnkernel.Commentary{
				SampleID: "sample-commentary", Text: "Checking callers.", CallIDs: []string{"read"},
			}
			envelope.FrozenState.Commentary = []turnkernel.Commentary{message}
			envelope.FrozenState.SampleLedger[message.SampleID] = turnkernel.ModelSampleState{
				ID: message.SampleID, Attempt: 1, Status: turnkernel.SampleCompleted,
			}
			envelope.DomainFacts[0].State = envelope.FrozenState
			digest, err := turnkernel.Digest(envelope.FrozenState)
			if err != nil {
				t.Fatal(err)
			}
			envelope.DomainFacts[0].StateDigest = digest
			meta := envelope.Outbox[0]
			operation := protocol.Operation{
				ID: envelope.OperationCommit.OperationID,
				Payload: &protocol.StartTurnPayload{
					ThreadID: meta.ThreadID, TurnID: meta.TurnID, ItemID: meta.ItemID, Prompt: "answer",
				},
			}
			events := NewMemoryEventStore(32)
			store := turnkernel.NewMemoryTerminalEnvelopeStore(nil, nil)
			runtime := NewRuntime(Options{EventStore: events, TerminalStore: store})
			t.Cleanup(func() { _ = runtime.Close(context.Background()) })
			data := message.ProtocolData(envelope.TurnID)
			sink := &runtimeSink{runtime: runtime, operation: operation}
			if published {
				if err := sink.Emit(&data); err != nil {
					t.Fatal(err)
				}
				if err := sink.Emit(&data); err != nil {
					t.Fatal(err)
				}
			}
			terminal, err := eventhub.DecodeTerminalOutboxEntry(envelope.Outbox[1])
			if err != nil {
				t.Fatal(err)
			}
			committed, err := runtime.terminal.Commit(t.Context(), eventhub.TerminalRequest{
				Operation: operation,
				Material: eventhub.TerminalMaterial{
					FrozenState: envelope.FrozenState, DomainFacts: envelope.DomainFacts,
					Measurement: envelope.Measurement, Receipt: envelope.Receipt, Terminal: terminal,
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := runtime.terminal.Publish(t.Context(), committed); err != nil {
				t.Fatal(err)
			}
			if err := runtime.terminal.Recover(t.Context(), func(
				_ protocol.ThreadID,
				turnID protocol.TurnID,
				err error,
			) {
				t.Fatalf("recover terminal %s: %v", turnID, err)
			}); err != nil {
				t.Fatal(err)
			}
			projected, err := events.Replay(t.Context(), 0)
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, event := range projected {
				if event.Kind == protocol.EventCommentaryCompleted {
					count++
					if event.ID != eventhub.CommentaryEventID(data.MessageID) {
						t.Fatal("terminal publication changed message identity")
					}
				}
			}
			if count != 1 || projected[0].Kind != protocol.EventCommentaryCompleted ||
				projected[len(projected)-1].Kind != protocol.EventTurnCompleted {
				t.Fatalf("projected events = %+v", projected)
			}
			pending, err := store.PendingOutbox(t.Context(), envelope.TurnID)
			if err != nil || len(pending) != 0 {
				t.Fatalf("pending = %+v, err = %v", pending, err)
			}
		})
	}
}
