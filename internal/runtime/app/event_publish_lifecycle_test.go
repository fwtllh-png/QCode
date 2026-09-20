package app

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

// recordingLifecycle records projected kinds so tests can assert which event
// classes reach durable lifecycle projection.
type recordingLifecycle struct {
	mu    sync.Mutex
	kinds []protocol.EventKind
}

func (r *recordingLifecycle) Recover(context.Context) (RecoveryState, error) {
	return RecoveryState{}, nil
}

func (r *recordingLifecycle) Accept(
	context.Context,
	protocol.Operation,
	string,
	json.RawMessage,
) (Acceptance, error) {
	return Acceptance{}, nil
}

func (r *recordingLifecycle) Project(
	_ context.Context,
	event protocol.Event,
) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.kinds = append(r.kinds, event.Kind)
	return nil
}

func (r *recordingLifecycle) Commit(context.Context, CommitReceipt) error {
	return nil
}

func (r *recordingLifecycle) projected(kind protocol.EventKind) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, seen := range r.kinds {
		if seen == kind {
			return true
		}
	}
	return false
}

// Streaming noise never reaches the durable log, so lifecycle projection must
// not run for it; persisted kinds keep projecting.
func TestStreamingNoiseSkipsLifecycleProjection(t *testing.T) {
	lifecycle := &recordingLifecycle{}
	runtime := NewRuntime(Options{
		Engine:           &testEngine{},
		EventStore:       NewMemoryEventStore(16),
		SubscriberBuffer: 8,
		Lifecycle:        lifecycle,
	})
	t.Cleanup(func() { closeRuntime(t, runtime) })

	publish := func(data protocol.EventData) error {
		return runtime.EventService.publishWithIdentity(
			protocol.OperationID("operation-lifecycle-guard"),
			protocol.ThreadID("thread-lifecycle-guard"),
			protocol.TurnID("turn-lifecycle-guard"),
			protocol.ItemID("item-lifecycle-guard"),
			"",
			data,
		)
	}
	for _, noise := range []protocol.EventData{
		&protocol.OutputDeltaData{Text: "chunk"},
		&protocol.ReasoningDeltaData{Text: "thinking"},
		&protocol.OutputDraftData{Text: "draft", SampleID: "sample-guard"},
	} {
		if err := publish(noise); err != nil {
			t.Fatal(err)
		}
	}
	if err := publish(&protocol.CommentaryCompletedData{
		MessageID: "message-lifecycle-guard",
		SampleID:  "sample-lifecycle-guard",
		Text:      "kept",
		CallIDs:   []string{"call-lifecycle-guard"},
	}); err != nil {
		t.Fatal(err)
	}
	if lifecycle.projected(protocol.EventOutputDelta) {
		t.Fatal("output.delta must not project lifecycle")
	}
	if lifecycle.projected(protocol.EventReasoningDelta) {
		t.Fatal("reasoning.delta must not project lifecycle")
	}
	if lifecycle.projected(protocol.EventOutputDraft) {
		t.Fatal("output.draft must not project lifecycle")
	}
	if !lifecycle.projected(protocol.EventCommentaryCompleted) {
		t.Fatal("commentary.completed must project lifecycle")
	}
}
