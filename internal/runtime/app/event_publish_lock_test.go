package app

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

// stalledLifecycle blocks durable projection of one event kind until released,
// standing in for a slow SQLite fsync.
type stalledLifecycle struct {
	recordingLifecycle
	kind    protocol.EventKind
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newStalledLifecycle(kind protocol.EventKind) *stalledLifecycle {
	return &stalledLifecycle{
		kind:    kind,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (l *stalledLifecycle) Project(ctx context.Context, event protocol.Event) error {
	if event.Kind == l.kind {
		l.once.Do(func() { close(l.entered) })
		<-l.release
	}
	return l.recordingLifecycle.Project(ctx, event)
}

// assertStateReadableDuringProjection runs every Runtime state reader that
// shares EventService.mu while a durable projection is stalled. None of them
// may wait for the projection's I/O.
func assertStateReadableDuringProjection(
	t *testing.T,
	runtime *Runtime,
	lifecycle *stalledLifecycle,
	publish func() error,
) {
	t.Helper()
	published := make(chan error, 1)
	go func() { published <- publish() }()
	select {
	case <-lifecycle.entered:
	case err := <-published:
		t.Fatalf("publish finished before projection stalled: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("projection never started")
	}
	readers := map[string]func(){
		"Snapshot":           func() { runtime.Snapshot(context.Background()) },
		"PendingApproval":    func() { runtime.PendingApproval("approval-none") },
		"PendingInput":       func() { runtime.PendingInput("input-none") },
		"queue claim lookup": func() { runtime.TurnQueueService.claimed("queue-none") },
	}
	for name, read := range readers {
		done := make(chan struct{})
		go func() {
			read()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(time.Second):
			close(lifecycle.release)
			<-published
			t.Fatalf("%s waited for durable projection I/O", name)
		}
	}
	close(lifecycle.release)
	if err := <-published; err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeStateStaysReadableWhileLiveEventProjects(t *testing.T) {
	lifecycle := newStalledLifecycle(protocol.EventCommentaryCompleted)
	runtime := NewRuntime(Options{
		Engine: &testEngine{}, EventStore: NewMemoryEventStore(16),
		SubscriberBuffer: 8, Lifecycle: lifecycle,
	})
	t.Cleanup(func() { closeRuntime(t, runtime) })
	assertStateReadableDuringProjection(t, runtime, lifecycle, func() error {
		return runtime.EventService.publish(
			"operation-stall", "thread-stall", "turn-stall", "item-stall",
			&protocol.CommentaryCompletedData{
				MessageID: "message-stall", SampleID: "sample-stall",
				Text: "slow projection", CallIDs: []string{"call-stall"},
			},
		)
	})
	if !lifecycle.projected(protocol.EventCommentaryCompleted) {
		t.Fatal("stalled event was not projected after release")
	}
}

func TestRuntimeStateStaysReadableWhileTerminalOutboxProjects(t *testing.T) {
	lifecycle := newStalledLifecycle(protocol.EventTurnCompleted)
	runtime := NewRuntime(Options{
		Engine: &testEngine{}, EventStore: NewMemoryEventStore(16),
		SubscriberBuffer: 8, Lifecycle: lifecycle,
	})
	t.Cleanup(func() { closeRuntime(t, runtime) })
	entry := turnkernel.ProjectionOutboxEntry{
		ID: "terminal-stall", EventID: "event-terminal-stall",
		OperationID: "operation-terminal-stall", ThreadID: "thread-terminal-stall",
		TurnID: "turn-terminal-stall", ItemID: "item-terminal-stall",
		Kind: string(protocol.EventTurnCompleted),
	}
	assertStateReadableDuringProjection(t, runtime, lifecycle, func() error {
		return runtime.PublishTerminalProjection(
			context.Background(), entry,
			&protocol.TurnCompletedData{Text: "done"},
		)
	})
	runtime.EventService.mu.Lock()
	kind := runtime.terminals["turn-terminal-stall"]
	runtime.EventService.mu.Unlock()
	if kind != protocol.EventTurnCompleted {
		t.Fatalf("terminal index = %q after outbox projection", kind)
	}
}
