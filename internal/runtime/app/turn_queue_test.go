package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type queuedTurnEngine struct {
	mu      sync.Mutex
	started chan struct{}
	release chan struct{}
	calls   int
}

func (e *queuedTurnEngine) StartTurn(
	ctx context.Context,
	payload *protocol.StartTurnPayload,
	sink EngineSink,
) error {
	e.mu.Lock()
	e.calls++
	call := e.calls
	e.mu.Unlock()
	if err := sink.Emit(&protocol.TurnStartedData{
		Provider: "test",
		Model:    "test",
	}); err != nil {
		return err
	}
	if call == 1 {
		close(e.started)
		select {
		case <-e.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return sink.Emit(&protocol.TurnCompletedData{Text: payload.Prompt})
}

func (*queuedTurnEngine) CancelTurn(
	context.Context,
	*protocol.CancelTurnPayload,
	EngineSink,
) error {
	return nil
}

func (*queuedTurnEngine) SteerTurn(
	_ context.Context,
	payload *protocol.SteerTurnPayload,
	sink EngineSink,
) error {
	return sink.Emit(&protocol.TurnSteeredData{Prompt: payload.Prompt})
}

func (*queuedTurnEngine) DecideApproval(
	context.Context,
	*protocol.ApprovalDecisionPayload,
	EngineSink,
) error {
	return nil
}

func (*queuedTurnEngine) ReplyInput(
	context.Context,
	*protocol.InputReplyPayload,
	EngineSink,
) error {
	return nil
}

func (*queuedTurnEngine) CompactThread(
	context.Context,
	*protocol.CompactThreadPayload,
	EngineSink,
) error {
	return nil
}

func (*queuedTurnEngine) ForkThread(
	context.Context,
	*protocol.ForkThreadPayload,
	EngineSink,
) error {
	return nil
}

func (*queuedTurnEngine) RevertTurn(
	context.Context,
	*protocol.RevertTurnPayload,
	EngineSink,
) error {
	return nil
}

func TestTurnQueueAdvancesAfterTerminalInFIFOOrder(t *testing.T) {
	engine := &queuedTurnEngine{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	runtime := NewRuntime(Options{
		Engine: engine, EventHistory: 32, SubscriberBuffer: 32,
	})
	t.Cleanup(func() { closeRuntime(t, runtime) })
	events, err := runtime.Events(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	start := startOperation(t, 1)
	if err := runtime.Submit(t.Context(), start); err != nil {
		t.Fatal(err)
	}
	started := receiveEvent(t, events)
	<-engine.started
	threadID, turnID, _ := protocol.OperationReferences(start)

	for index, prompt := range []string{"second", "third"} {
		operation, err := protocol.NewOperation(&protocol.EnqueueTurnPayload{
			ThreadID: threadID,
			TurnID:   turnID,
			ItemID:   protocol.ItemID("queue-operation-" + prompt),
			QueueID:  "queue-" + prompt,
			Prompt:   prompt,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := runtime.Submit(t.Context(), operation); err != nil {
			t.Fatal(err)
		}
		queued := receiveEvent(t, events)
		if queued.Kind != protocol.EventTurnQueued {
			t.Fatalf("queue event %d = %s", index, queued.Kind)
		}
	}

	close(engine.release)
	var queueIDs []string
	for len(queueIDs) < 2 {
		event := receiveEvent(t, events)
		if event.Kind != protocol.EventTurnStarted ||
			event.TurnID == started.TurnID {
			continue
		}
		data := event.Data.(*protocol.TurnStartedData)
		queueIDs = append(queueIDs, data.QueueID)
	}
	if queueIDs[0] != "queue-second" || queueIDs[1] != "queue-third" {
		t.Fatalf(
			"claimed queue items = %v, want [queue-second queue-third]",
			queueIDs,
		)
	}
	if items := runtime.TurnQueueService.snapshotMap(); len(items) != 0 {
		t.Fatalf("pending queue = %v, want empty", items)
	}
}

func TestIdleEnqueuePersistsThenStartsNewTurn(t *testing.T) {
	engine := &recordingStartEngine{}
	runtime := NewRuntime(Options{
		Engine: engine, EventHistory: 16, SubscriberBuffer: 16,
	})
	t.Cleanup(func() { closeRuntime(t, runtime) })
	events, err := runtime.Events(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := protocol.NewOperation(&protocol.EnqueueTurnPayload{
		ThreadID: "idle-enqueue-thread",
		TurnID:   "just-finished-turn",
		ItemID:   "idle-enqueue-item",
		QueueID:  "idle-enqueue-queue",
		Prompt:   "start after the previous turn finished",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Submit(t.Context(), operation); err != nil {
		t.Fatal(err)
	}
	sawQueued, sawStarted := false, false
	deadline := time.After(2 * time.Second)
	for {
		select {
		case event := <-events:
			switch event.Kind {
			case protocol.EventOperationRejected:
				t.Fatalf("idle enqueue rejected: %+v", event.Data)
			case protocol.EventTurnQueued:
				sawQueued = true
			case protocol.EventTurnStarted:
				data := event.Data.(*protocol.TurnStartedData)
				sawStarted = data.QueueID == "idle-enqueue-queue"
			}
			if sawQueued && sawStarted && engine.starts.Load() == 1 &&
				engine.prompt() == "start after the previous turn finished" {
				return
			}
		case <-deadline:
			t.Fatalf(
				"idle enqueue did not start: queued=%t started=%t calls=%d prompt=%q",
				sawQueued, sawStarted, engine.starts.Load(), engine.prompt(),
			)
		}
	}
}

func TestTurnQueueProjectionUpdatesRemovesAndClaims(t *testing.T) {
	now := testTime()
	items := make(map[string]protocol.QueuedTurn)
	apply := func(sequence protocol.Cursor, data protocol.EventData) {
		t.Helper()
		event, err := protocol.NewEvent(protocol.EventMeta{
			Sequence: sequence, OperationID: "operation",
			ThreadID: "thread", TurnID: "turn", ItemID: "item",
		}, data)
		if err != nil {
			t.Fatal(err)
		}
		event.CreatedAt = now
		if err := ApplyTurnQueueEvent(items, event); err != nil {
			t.Fatal(err)
		}
	}

	apply(1, &protocol.TurnQueuedData{QueueID: "queue", Prompt: "before"})
	apply(2, &protocol.QueuedTurnUpdatedData{QueueID: "queue", Prompt: "after"})
	if got := items["queue"].Prompt; got != "after" {
		t.Fatalf("prompt = %q, want after", got)
	}
	apply(3, &protocol.TurnStartedData{
		Provider: "test", Model: "test", QueueID: "queue",
	})
	if len(items) != 0 {
		t.Fatalf("pending queue = %v, want empty", items)
	}
}

func TestTurnQueueCanUpdateAndPromoteIntoCurrentTurn(t *testing.T) {
	engine := &queuedTurnEngine{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	runtime := NewRuntime(Options{
		Engine: engine, EventHistory: 16, SubscriberBuffer: 16,
	})
	t.Cleanup(func() {
		close(engine.release)
		closeRuntime(t, runtime)
	})
	events, err := runtime.Events(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	start := startOperation(t, 1)
	if err := runtime.Submit(t.Context(), start); err != nil {
		t.Fatal(err)
	}
	receiveEvent(t, events)
	<-engine.started
	threadID, turnID, _ := protocol.OperationReferences(start)
	submit := func(payload protocol.OperationPayload) {
		t.Helper()
		operation, err := protocol.NewOperation(payload)
		if err != nil {
			t.Fatal(err)
		}
		if err := runtime.Submit(t.Context(), operation); err != nil {
			t.Fatal(err)
		}
	}
	submit(&protocol.EnqueueTurnPayload{
		ThreadID: threadID, TurnID: turnID, ItemID: "enqueue-item",
		QueueID: "queue", Prompt: "before",
	})
	if event := receiveEvent(t, events); event.Kind != protocol.EventTurnQueued {
		t.Fatalf("event = %s, want %s", event.Kind, protocol.EventTurnQueued)
	}
	submit(&protocol.UpdateQueuedTurnPayload{
		ThreadID: threadID, TurnID: turnID, ItemID: "update-item",
		QueueID: "queue", Prompt: "after",
	})
	if event := receiveEvent(t, events); event.Kind != protocol.EventQueuedTurnUpdated {
		t.Fatalf("event = %s, want %s", event.Kind, protocol.EventQueuedTurnUpdated)
	}
	submit(&protocol.EnqueueTurnPayload{
		ThreadID: threadID, TurnID: turnID, ItemID: "enqueue-remove-item",
		QueueID: "queue-remove", Prompt: "remove me",
	})
	if event := receiveEvent(t, events); event.Kind != protocol.EventTurnQueued {
		t.Fatalf("event = %s, want %s", event.Kind, protocol.EventTurnQueued)
	}
	submit(&protocol.RemoveQueuedTurnPayload{
		ThreadID: threadID, TurnID: turnID, ItemID: "remove-item",
		QueueID: "queue-remove",
	})
	if event := receiveEvent(t, events); event.Kind != protocol.EventQueuedTurnRemoved {
		t.Fatalf("event = %s, want %s", event.Kind, protocol.EventQueuedTurnRemoved)
	}
	submit(&protocol.PromoteQueuedTurnPayload{
		ThreadID: threadID, TurnID: turnID, ItemID: "promote-item",
		QueueID: "queue",
	})
	promoted := receiveEvent(t, events)
	if promoted.Kind != protocol.EventTurnSteered {
		t.Fatalf("event = %s, want %s", promoted.Kind, protocol.EventTurnSteered)
	}
	data := promoted.Data.(*protocol.TurnSteeredData)
	if data.Prompt != "after" || data.QueueID != "queue" {
		t.Fatalf("promoted data = %+v", data)
	}
	if items := runtime.TurnQueueService.snapshotMap(); len(items) != 0 {
		t.Fatalf("pending queue = %v, want empty", items)
	}
}

func TestRecoveredPendingQueueStartsWhenRuntimeBecomesReady(t *testing.T) {
	store := NewMemoryEventStore(16)
	attachmentContent := "persisted queue attachment"
	attachmentDigest := sha256.Sum256([]byte(attachmentContent))
	queuedEvent, err := protocol.NewEvent(protocol.EventMeta{
		Sequence: 1, OperationID: "enqueue-operation",
		ThreadID: "thread", TurnID: "source-turn", ItemID: "queue-item",
	}, &protocol.TurnQueuedData{
		QueueID: "queue-recovered",
		Prompt:  "resume after restart",
		Context: []protocol.EditorContextReference{{
			Kind:   protocol.EditorContextAttachment,
			Source: protocol.EditorContextSourceNativePicker,
			Digest: hex.EncodeToString(attachmentDigest[:]),
			Label:  "notes.txt", MediaType: "text/plain",
			Content: attachmentContent, Explicit: true,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(t.Context(), queuedEvent); err != nil {
		t.Fatal(err)
	}
	pending := make(map[string]protocol.QueuedTurn)
	if err := ApplyTurnQueueEvent(pending, queuedEvent); err != nil {
		t.Fatal(err)
	}
	runtime, err := PrepareRuntimeWithRecovery(t.Context(), Options{
		Engine: &testEngine{}, EventStore: store,
		Recovery: &RecoveryState{
			LastSequence:       1,
			Terminals:          map[protocol.TurnID]protocol.EventKind{},
			PendingApprovals:   map[string]PendingApproval{},
			PendingInputs:      map[string]PendingInput{},
			PendingQueuedTurns: pending,
			PendingOperations:  map[protocol.OperationID]PendingOperation{},
			ToolItems:          map[EventItemOwner]protocol.ItemID{},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	recovered := runtime.TurnQueueService.snapshotMap()["queue-recovered"]
	if len(recovered.Context) != 1 ||
		recovered.Context[0].Content != attachmentContent {
		t.Fatalf("recovered queue context = %+v", recovered.Context)
	}
	events, err := runtime.Events(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeRuntime(t, runtime) })

	started := receiveEvent(t, events)
	if started.Kind != protocol.EventTurnStarted {
		t.Fatalf("event = %s, want %s", started.Kind, protocol.EventTurnStarted)
	}
	data := started.Data.(*protocol.TurnStartedData)
	if data.QueueID != "queue-recovered" {
		t.Fatalf("queue_id = %q, want queue-recovered", data.QueueID)
	}
}

type failingQueuedTurnEngine struct {
	*queuedTurnEngine
}

func (e failingQueuedTurnEngine) StartTurn(
	ctx context.Context,
	payload *protocol.StartTurnPayload,
	sink EngineSink,
) error {
	switch payload.Prompt {
	case "fail-terminal":
		return sink.Emit(&protocol.TurnFailedData{
			Code: protocol.CodeInternal, Message: "preparation failed",
		})
	case "fail-reject":
		return errors.New("preparation failed")
	default:
		return e.queuedTurnEngine.StartTurn(ctx, payload, sink)
	}
}

// A queued Turn can fail before turn.started either with a terminal event or
// with an operation rejection. Neither may strand its claim or queue item, or
// every later queue edit reports "already starting" and the queue stops.
func TestTurnQueueAdvancesPastQueuedTurnsThatFailBeforeStarting(t *testing.T) {
	engine := failingQueuedTurnEngine{&queuedTurnEngine{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}}
	runtime := NewRuntime(Options{
		Engine: engine, EventHistory: 64, SubscriberBuffer: 64,
	})
	t.Cleanup(func() { closeRuntime(t, runtime) })
	events, err := runtime.Events(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	start := startOperation(t, 1)
	if err := runtime.Submit(t.Context(), start); err != nil {
		t.Fatal(err)
	}
	receiveEvent(t, events)
	<-engine.started
	threadID, turnID, _ := protocol.OperationReferences(start)
	for _, prompt := range []string{"fail-terminal", "fail-reject", "ok"} {
		operation, err := protocol.NewOperation(&protocol.EnqueueTurnPayload{
			ThreadID: threadID, TurnID: turnID,
			ItemID:  protocol.ItemID("queue-operation-" + prompt),
			QueueID: "queue-" + prompt, Prompt: prompt,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := runtime.Submit(t.Context(), operation); err != nil {
			t.Fatal(err)
		}
		if queued := receiveEvent(t, events); queued.Kind != protocol.EventTurnQueued {
			t.Fatalf("queue event = %s", queued.Kind)
		}
	}
	close(engine.release)
	deadline := time.After(3 * time.Second)
	for started := false; !started; {
		select {
		case event := <-events:
			if data, ok := event.Data.(*protocol.TurnStartedData); ok {
				started = data.QueueID == "queue-ok"
			}
		case <-deadline:
			t.Fatalf(
				"queue stalled: items=%v",
				runtime.TurnQueueService.snapshotMap(),
			)
		}
	}
	waitForCondition(t, func() bool {
		runtime.TurnQueueService.mu.Lock()
		defer runtime.TurnQueueService.mu.Unlock()
		return len(runtime.TurnQueueService.items) == 0 &&
			len(runtime.TurnQueueService.claims) == 0
	})
}

func TestTurnQueueProjectionConsumesItemWhoseStartFailedBeforeStarting(t *testing.T) {
	items := map[string]protocol.QueuedTurn{}
	queued, err := protocol.NewEvent(protocol.EventMeta{
		Sequence: 1, OperationID: "enqueue", ThreadID: "thread",
		TurnID: "source", ItemID: "item",
	}, &protocol.TurnQueuedData{QueueID: "queue", Prompt: "later"})
	if err != nil {
		t.Fatal(err)
	}
	if err := ApplyTurnQueueEvent(items, queued); err != nil {
		t.Fatal(err)
	}
	_, operationID := queuedTurnStart("queue", "thread")
	failed, err := protocol.NewEvent(protocol.EventMeta{
		Sequence: 2, OperationID: operationID, ThreadID: "thread",
		TurnID: "queued-turn", ItemID: "queued-item",
	}, &protocol.TurnFailedData{
		Code: protocol.CodeInternal, Message: "preparation failed",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ApplyTurnQueueEvent(items, failed); err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("pending queue after failed start = %v", items)
	}
}

// Durable Runtimes publish terminals through the terminal outbox rather than
// the live publish path; that path must settle the queue the same way.
func TestTerminalOutboxProjectionReleasesQueuedStartThatNeverStarted(t *testing.T) {
	runtime := NewRuntime(Options{
		Engine: &testEngine{}, EventStore: NewMemoryEventStore(16),
		SubscriberBuffer: 8,
	})
	t.Cleanup(func() { closeRuntime(t, runtime) })
	if err := runtime.EventService.publish(
		"enqueue", "thread-outbox", "source-turn", "queue-item",
		&protocol.TurnQueuedData{QueueID: "queue-outbox", Prompt: "later"},
	); err != nil {
		t.Fatal(err)
	}
	_, operationID := queuedTurnStart("queue-outbox", "thread-outbox")
	if _, claimed := runtime.TurnQueueService.claimNext("thread-outbox"); !claimed {
		t.Fatal("queued turn was not claimable")
	}
	if err := runtime.PublishTerminalProjection(
		context.Background(),
		turnkernel.ProjectionOutboxEntry{
			ID: "terminal-outbox", EventID: "event-terminal-outbox",
			OperationID: operationID, ThreadID: "thread-outbox",
			TurnID: "queued-turn", ItemID: "queued-item",
			Kind: string(protocol.EventTurnFailed),
		},
		&protocol.TurnFailedData{
			Code: protocol.CodeInternal, Message: "preparation failed",
		},
	); err != nil {
		t.Fatal(err)
	}
	runtime.TurnQueueService.mu.Lock()
	defer runtime.TurnQueueService.mu.Unlock()
	if len(runtime.TurnQueueService.items) != 0 ||
		len(runtime.TurnQueueService.claims) != 0 {
		t.Fatalf(
			"queue after outbox terminal: items=%v claims=%v",
			runtime.TurnQueueService.items,
			runtime.TurnQueueService.claims,
		)
	}
}

func testTime() (value time.Time) {
	return time.Unix(1, 0).UTC()
}
