package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type testEngine struct {
	block        bool
	cancelMu     sync.Mutex
	cancelReason string
	approvalMu   sync.Mutex
	approval     *protocol.ApprovalDecisionPayload
	mutationMu   sync.Mutex
	compactCalls int
	forkCalls    int
	revertCalls  int
}

func (e *testEngine) StartTurn(
	ctx context.Context, payload *protocol.StartTurnPayload, sink EngineSink,
) error {
	if err := sink.Emit(&protocol.TurnStartedData{Provider: "test", Model: "test"}); err != nil {
		return err
	}
	if e.block {
		<-ctx.Done()
		e.cancelMu.Lock()
		reason := e.cancelReason
		e.cancelMu.Unlock()
		if reason == "" {
			return ctx.Err()
		}
		return sink.Emit(&protocol.TurnCanceledData{Reason: reason})
	}
	if err := sink.Emit(&protocol.OutputDeltaData{Text: payload.Prompt}); err != nil {
		return err
	}
	return sink.Emit(&protocol.TurnCompletedData{Text: payload.Prompt})
}

func (e *testEngine) CancelTurn(
	_ context.Context,
	payload *protocol.CancelTurnPayload,
	_ EngineSink,
) error {
	e.cancelMu.Lock()
	e.cancelReason = protocol.NormalizeCancelReason(payload.Reason)
	e.cancelMu.Unlock()
	return nil
}
func (*testEngine) SteerTurn(_ context.Context, payload *protocol.SteerTurnPayload, sink EngineSink) error {
	return sink.Emit(&protocol.TurnSteeredData{Prompt: payload.Prompt})
}
func (e *testEngine) DecideApproval(_ context.Context, payload *protocol.ApprovalDecisionPayload, sink EngineSink) error {
	copy := *payload
	e.approvalMu.Lock()
	e.approval = &copy
	e.approvalMu.Unlock()
	return sink.Emit(&protocol.ApprovalResolvedData{RequestID: payload.RequestID, Decision: payload.Decision})
}
func (*testEngine) ReplyInput(_ context.Context, payload *protocol.InputReplyPayload, sink EngineSink) error {
	return sink.Emit(&protocol.InputResolvedData{RequestID: payload.RequestID, Answer: payload.Answer})
}
func (e *testEngine) CompactThread(context.Context, *protocol.CompactThreadPayload, EngineSink) error {
	e.mutationMu.Lock()
	e.compactCalls++
	e.mutationMu.Unlock()
	return errors.New("compact unsupported")
}
func (e *testEngine) ForkThread(_ context.Context, payload *protocol.ForkThreadPayload, sink EngineSink) error {
	e.mutationMu.Lock()
	e.forkCalls++
	e.mutationMu.Unlock()
	return sink.Emit(&protocol.ThreadForkedData{NewThreadID: payload.NewThreadID})
}
func (e *testEngine) RevertTurn(_ context.Context, payload *protocol.RevertTurnPayload, sink EngineSink) error {
	e.mutationMu.Lock()
	e.revertCalls++
	e.mutationMu.Unlock()
	return sink.Emit(&protocol.TurnRevertedData{TargetTurnID: payload.TargetTurnID})
}

func TestRuntimeConcurrentSubmitHasStrictSequenceAndUniqueTerminal(t *testing.T) {
	const count = 32
	runtime := NewRuntime(Options{
		Engine: &testEngine{}, OperationBuffer: count,
		EventHistory: count * 3, SubscriberBuffer: count * 3,
	})
	t.Cleanup(func() { closeRuntime(t, runtime) })
	events, err := runtime.Events(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for index := range count {
		group.Add(1)
		go func() {
			defer group.Done()
			submitEventually(t, runtime, startOperation(t, index))
		}()
	}
	group.Wait()

	terminals := make(map[protocol.TurnID]int)
	for sequence := protocol.Cursor(1); len(terminals) < count; sequence++ {
		event := receiveEvent(t, events)
		if event.Sequence != sequence {
			t.Fatalf("sequence = %d, want %d", event.Sequence, sequence)
		}
		if protocol.IsTerminalEvent(event.Kind) {
			terminals[event.TurnID]++
		}
	}
	for turnID, terminalCount := range terminals {
		if terminalCount != 1 {
			t.Fatalf("turn %s terminal count = %d", turnID, terminalCount)
		}
	}
}

func TestRuntimeCancelActuallyCancelsActiveTurn(t *testing.T) {
	runtime := NewRuntime(Options{Engine: &testEngine{block: true}})
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
	if started.Kind != protocol.EventTurnStarted {
		t.Fatalf("first event = %s", started.Kind)
	}
	threadID, turnID, _ := protocol.OperationReferences(start)
	cancel, err := protocol.NewOperation(&protocol.CancelTurnPayload{
		ThreadID: threadID, TurnID: turnID, ItemID: "cancel_item", Reason: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Submit(t.Context(), cancel); err != nil {
		t.Fatal(err)
	}
	terminal := receiveEvent(t, events)
	if terminal.Kind != protocol.EventTurnCanceled {
		t.Fatalf("terminal = %s, want %s", terminal.Kind, protocol.EventTurnCanceled)
	}
	data, ok := terminal.Data.(*protocol.TurnCanceledData)
	if !ok {
		t.Fatalf("data type = %T", terminal.Data)
	}
	if data.Reason != "test" {
		t.Fatalf("reason = %q, want test", data.Reason)
	}
	// The canceled terminal re-projects live events, so it stays under the
	// emission (start) operation identity rather than the cancel operation.
	if terminal.OperationID != start.ID || terminal.ItemID != "item_1" {
		t.Fatalf(
			"terminal operation=%s item=%s, want start operation %s",
			terminal.OperationID, terminal.ItemID, start.ID,
		)
	}
}

type missingTerminalEngine struct{ testEngine }

func (*missingTerminalEngine) StartTurn(
	_ context.Context, _ *protocol.StartTurnPayload, sink EngineSink,
) error {
	return sink.Emit(&protocol.TurnStartedData{Provider: "test", Model: "test"})
}

func TestRuntimeFailsClosedWhenEngineReturnsWithoutTerminal(t *testing.T) {
	runtime := NewRuntime(Options{Engine: &missingTerminalEngine{}})
	t.Cleanup(func() { closeRuntime(t, runtime) })
	events, err := runtime.Events(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Submit(t.Context(), startOperation(t, 1)); err != nil {
		t.Fatal(err)
	}
	if started := receiveEvent(t, events); started.Kind != protocol.EventTurnStarted {
		t.Fatalf("first event = %s", started.Kind)
	}
	rejected := receiveEvent(t, events)
	if rejected.Kind != protocol.EventOperationRejected {
		t.Fatalf("result = %s, want %s", rejected.Kind, protocol.EventOperationRejected)
	}
	data, ok := rejected.Data.(*protocol.OperationRejectedData)
	if !ok || !strings.Contains(data.Message, "without terminal material") {
		t.Fatalf("rejection data = %+v", rejected.Data)
	}
}

type panickingEngine struct{ testEngine }

func (*panickingEngine) StartTurn(
	context.Context, *protocol.StartTurnPayload, EngineSink,
) error {
	panic("intentional panic")
}

func TestRuntimeRejectsEnginePanicWithoutSyntheticTerminal(t *testing.T) {
	runtime := NewRuntime(Options{Engine: &panickingEngine{}})
	t.Cleanup(func() { closeRuntime(t, runtime) })
	events, err := runtime.Events(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Submit(t.Context(), startOperation(t, 1)); err != nil {
		t.Fatal(err)
	}
	rejected := receiveEvent(t, events)
	if rejected.Kind != protocol.EventOperationRejected {
		t.Fatalf("result = %s, want %s", rejected.Kind, protocol.EventOperationRejected)
	}
	data, ok := rejected.Data.(*protocol.OperationRejectedData)
	if !ok || data.Code != protocol.CodeInternal ||
		data.Fault == nil ||
		data.Fault.Disposition != protocol.FaultFailTurn ||
		!strings.Contains(data.Message, "engine panicked") {
		t.Fatalf("rejection data = %+v", rejected.Data)
	}
}

func TestRuntimeUnsupportedOperationIsExplicitlyRejected(t *testing.T) {
	runtime := NewRuntime(Options{Engine: &testEngine{}})
	t.Cleanup(func() { closeRuntime(t, runtime) })
	events, err := runtime.Events(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := protocol.NewOperation(&protocol.CompactThreadPayload{
		ThreadID: "thread", TurnID: "turn", ItemID: "item",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Submit(t.Context(), operation); err != nil {
		t.Fatal(err)
	}
	event := receiveEvent(t, events)
	if event.Kind != protocol.EventOperationRejected {
		t.Fatalf("event = %s, want operation.rejected", event.Kind)
	}
}

func TestRuntimeDispatchesControlOperations(t *testing.T) {
	runtime := NewRuntime(Options{Engine: &testEngine{}, SubscriberBuffer: 8})
	t.Cleanup(func() { closeRuntime(t, runtime) })
	events, err := runtime.Events(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	// Active turn + pending approval so steer injects and approval resumes.
	lease, err := runtime.active.Reserve("thread", "turn", "op", "item")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.active.Release(lease) })
	runtime.EventService.mu.Lock()
	runtime.approvals["approval_1"] = PendingApproval{
		RequestID: "approval_1", ThreadID: "thread", TurnID: "turn",
	}
	runtime.EventService.mu.Unlock()
	operations := []struct {
		payload protocol.OperationPayload
		event   protocol.EventKind
	}{
		{
			payload: &protocol.SteerTurnPayload{
				ThreadID: "thread", TurnID: "turn", ItemID: "steer", Prompt: "change",
			},
			event: protocol.EventTurnSteered,
		},
		{
			payload: &protocol.ApprovalDecisionPayload{
				ThreadID: "thread", TurnID: "turn", ItemID: "approval",
				RequestID: "approval_1", Decision: protocol.ApprovalApprove,
			},
			event: protocol.EventApprovalResolved,
		},
	}
	for _, test := range operations {
		operation, createErr := protocol.NewOperation(test.payload)
		if createErr != nil {
			t.Fatal(createErr)
		}
		if submitErr := runtime.Submit(t.Context(), operation); submitErr != nil {
			t.Fatal(submitErr)
		}
		event := receiveEvent(t, events)
		if event.Kind != test.event {
			t.Fatalf("event = %s, want %s", event.Kind, test.event)
		}
	}
}

func TestRuntimeRejectsThreadMutationWhileTurnIsActive(t *testing.T) {
	engine := &testEngine{}
	runtime := NewRuntime(Options{Engine: engine, SubscriberBuffer: 8})
	t.Cleanup(func() { closeRuntime(t, runtime) })
	events, err := runtime.Events(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := runtime.active.Reserve("thread", "turn", "op", "item")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.active.Release(lease) })
	payloads := []protocol.OperationPayload{
		&protocol.CompactThreadPayload{
			ThreadID: "thread", TurnID: "turn", ItemID: "compact",
		},
		&protocol.ForkThreadPayload{
			ThreadID: "thread", TurnID: "turn", ItemID: "fork",
			NewThreadID: "forked",
		},
		&protocol.RevertTurnPayload{
			ThreadID: "thread", TurnID: "turn", ItemID: "revert",
			TargetTurnID: "previous",
		},
	}
	for _, payload := range payloads {
		operation, err := protocol.NewOperation(payload)
		if err != nil {
			t.Fatal(err)
		}
		if err := runtime.Submit(t.Context(), operation); err != nil {
			t.Fatal(err)
		}
		event := receiveEvent(t, events)
		if event.Kind != protocol.EventOperationRejected {
			t.Fatalf("event = %s, want %s", event.Kind, protocol.EventOperationRejected)
		}
		rejected, ok := event.Data.(*protocol.OperationRejectedData)
		if !ok || rejected.Code != protocol.CodeConflict {
			t.Fatalf("rejection = %#v", event.Data)
		}
	}
	engine.mutationMu.Lock()
	defer engine.mutationMu.Unlock()
	if engine.compactCalls != 0 || engine.forkCalls != 0 ||
		engine.revertCalls != 0 {
		t.Fatalf(
			"active mutation reached Engine: compact=%d fork=%d revert=%d",
			engine.compactCalls,
			engine.forkCalls,
			engine.revertCalls,
		)
	}
}

func TestRuntimeDispatchesIdleThreadMutations(t *testing.T) {
	engine := &testEngine{}
	runtime := NewRuntime(Options{Engine: engine, SubscriberBuffer: 8})
	t.Cleanup(func() { closeRuntime(t, runtime) })
	events, err := runtime.Events(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		payload protocol.OperationPayload
		event   protocol.EventKind
	}{
		{
			payload: &protocol.ForkThreadPayload{
				ThreadID: "thread", TurnID: "turn", ItemID: "fork",
				NewThreadID: "forked",
			},
			event: protocol.EventThreadForked,
		},
		{
			payload: &protocol.RevertTurnPayload{
				ThreadID: "thread", TurnID: "turn", ItemID: "revert",
				TargetTurnID: "previous",
			},
			event: protocol.EventTurnReverted,
		},
	}
	for _, test := range tests {
		operation, err := protocol.NewOperation(test.payload)
		if err != nil {
			t.Fatal(err)
		}
		if err := runtime.Submit(t.Context(), operation); err != nil {
			t.Fatal(err)
		}
		if event := receiveEvent(t, events); event.Kind != test.event {
			t.Fatalf("event = %s, want %s", event.Kind, test.event)
		}
	}
}

func TestApprovalHandlerProxiesParentOperationToChildIdentity(t *testing.T) {
	engine := &testEngine{}
	runtime := NewRuntime(Options{Engine: engine, SubscriberBuffer: 8})
	t.Cleanup(func() { closeRuntime(t, runtime) })
	events, err := runtime.Events(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := runtime.active.Reserve(
		"child-thread", "child-turn", "child-start", "child-item",
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.active.Release(lease) })
	runtime.EventService.mu.Lock()
	runtime.approvals["child-approval"] = PendingApproval{
		RequestID: "child-approval",
		ThreadID:  "child-thread",
		TurnID:    "child-turn",
		ItemID:    "child-item",
	}
	runtime.EventService.mu.Unlock()
	operation, err := protocol.NewOperation(&protocol.ApprovalDecisionPayload{
		ThreadID: "parent-thread", TurnID: "parent-turn", ItemID: "parent-item",
		RequestID: "child-approval", Decision: protocol.ApprovalApprove,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Submit(t.Context(), operation); err != nil {
		t.Fatal(err)
	}
	event := receiveEvent(t, events)
	if event.Kind != protocol.EventApprovalResolved ||
		event.ThreadID != "child-thread" ||
		event.TurnID != "child-turn" ||
		event.ItemID != "parent-item" {
		t.Fatalf("proxied approval event = %+v", event)
	}
	engine.approvalMu.Lock()
	proxied := engine.approval
	engine.approvalMu.Unlock()
	if proxied == nil || proxied.ThreadID != "child-thread" ||
		proxied.TurnID != "child-turn" || proxied.ItemID != "parent-item" {
		t.Fatalf("proxied approval payload = %+v", proxied)
	}
}

func TestRuntimeDropsSlowSubscriberDeterministically(t *testing.T) {
	runtime := NewRuntime(Options{
		Engine: &testEngine{}, OperationBuffer: 4, EventHistory: 16, SubscriberBuffer: 1,
	})
	t.Cleanup(func() { closeRuntime(t, runtime) })
	events, err := runtime.Events(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Submit(t.Context(), startOperation(t, 1)); err != nil {
		t.Fatal(err)
	}
	waitForProcessed(t, runtime, 1)
	deadline := time.After(time.Second)
	for {
		select {
		case _, open := <-events:
			if !open {
				if runtime.Snapshot(t.Context()).Metrics.SubscribersDropped != 1 {
					t.Fatal("slow subscriber was not counted")
				}
				return
			}
		case <-deadline:
			t.Fatal("slow subscriber was not closed")
		}
	}
}

func TestRuntimeSubmitCloseRaceAccountsForAcceptedOperations(t *testing.T) {
	runtime := NewRuntime(Options{
		Engine: &testEngine{}, OperationBuffer: 64, EventHistory: 256, SubscriberBuffer: 256,
	})
	var accepted atomic.Uint64
	var group sync.WaitGroup
	for index := range 64 {
		group.Add(1)
		go func() {
			defer group.Done()
			err := runtime.Submit(context.Background(), startOperation(t, index))
			if err == nil {
				accepted.Add(1)
			} else if !errors.Is(err, ErrClosed) && !errors.Is(err, ErrQueueFull) {
				t.Errorf("Submit() error = %v", err)
			}
		}()
	}
	closeRuntime(t, runtime)
	group.Wait()
	if got := runtime.Snapshot(t.Context()).OperationsProcessed; got != accepted.Load() {
		t.Fatalf("processed = %d, accepted = %d", got, accepted.Load())
	}
}

func TestRuntimeReplayEventsPagesWithoutSubscribing(t *testing.T) {
	runtime := NewRuntime(Options{Engine: &testEngine{}, EventHistory: 64})
	t.Cleanup(func() { closeRuntime(t, runtime) })
	submitEventually(t, runtime, startOperation(t, 1))
	waitForProcessed(t, runtime, 1)
	head := runtime.Snapshot(t.Context()).LastSequence
	if head < 3 {
		t.Fatalf("last sequence = %d, want the turn's three events", head)
	}

	var collected []protocol.Event
	cursor := protocol.Cursor(0)
	for {
		page, more, err := runtime.ReplayEvents(t.Context(), cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) > 2 {
			t.Fatalf("page size = %d, want at most 2", len(page))
		}
		collected = append(collected, page...)
		if !more {
			break
		}
		if len(page) == 0 {
			t.Fatal("more events reported but page is empty")
		}
		cursor = page[len(page)-1].Sequence
	}
	if protocol.Cursor(len(collected)) != head {
		t.Fatalf("replayed %d events, want %d", len(collected), head)
	}
	for index, event := range collected {
		if event.Sequence != protocol.Cursor(index+1) {
			t.Fatalf("event %d sequence = %d", index, event.Sequence)
		}
	}
	// Paging must not leave a subscriber behind, or a host holding a live
	// subscription would receive every event twice.
	if subscribers := runtime.Snapshot(t.Context()).Subscribers; subscribers != 0 {
		t.Fatalf("subscribers = %d, want 0", subscribers)
	}

	if _, _, err := runtime.ReplayEvents(t.Context(), 0, 0); !protocol.IsCode(
		err, protocol.CodeInvalidArgument,
	) {
		t.Fatalf("zero limit error = %v", err)
	}
	if _, _, err := runtime.ReplayEvents(t.Context(), head+1, 2); !errors.Is(
		err, ErrCursorAhead,
	) {
		t.Fatalf("cursor ahead error = %v", err)
	}
}

func TestRuntimeReplayEventsSurfacesCursorGap(t *testing.T) {
	// A two-event window forces the oldest events out, which is what retention
	// looks like to a reconnecting client.
	runtime := NewRuntime(Options{Engine: &testEngine{}, EventHistory: 2})
	t.Cleanup(func() { closeRuntime(t, runtime) })
	submitEventually(t, runtime, startOperation(t, 1))
	waitForProcessed(t, runtime, 1)

	_, _, err := runtime.ReplayEvents(t.Context(), 0, 8)
	var gap *CursorGapError
	if !errors.As(err, &gap) {
		t.Fatalf("replay from evicted cursor error = %v", err)
	}
	if gap.OldestAvailable == 0 || gap.Latest < gap.OldestAvailable {
		t.Fatalf("gap = %+v", gap)
	}
}

func startOperation(t *testing.T, index int) protocol.Operation {
	t.Helper()
	operation, err := protocol.NewOperation(&protocol.StartTurnPayload{
		ThreadID: protocol.ThreadID(fmt.Sprintf("thread_%d", index)),
		TurnID:   protocol.TurnID(fmt.Sprintf("turn_%d", index)),
		ItemID:   protocol.ItemID(fmt.Sprintf("item_%d", index)),
		Prompt:   "hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	return operation
}

func TestStartTurnRecoverySourceMustBeTerminalInTheSameThread(t *testing.T) {
	events := NewMemoryEventStore(4)
	terminal, err := protocol.NewEvent(protocol.EventMeta{
		Sequence: 1, OperationID: "operation-source",
		ThreadID: "thread-source", TurnID: "turn-source", ItemID: "item-source",
	}, &protocol.TurnFailedData{
		Code: protocol.CodeConflict, Message: "verification blocked",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := events.Append(t.Context(), terminal); err != nil {
		t.Fatal(err)
	}
	handler := StartTurnHandler{Runtime: &Runtime{
		ctx: t.Context(), events: events,
	}}
	recovery := &protocol.TurnRecoveryContext{
		Action: protocol.TurnRecoveryContinue, SourceTurnID: "turn-source",
	}
	if err := handler.validateStart(&protocol.StartTurnPayload{
		ThreadID: "thread-source", Recovery: recovery,
	}); err != nil {
		t.Fatal(err)
	}
	if err := handler.validateStart(&protocol.StartTurnPayload{
		ThreadID: "thread-other", Recovery: recovery,
	}); protocol.CodeOf(err) != protocol.CodeConflict {
		t.Fatalf("cross-Thread recovery error = %v", err)
	}
	lifecycle := artifactLifecycle()
	lifecycle.summary.ThreadID = "thread-other"
	lifecycle.threadIDs = []protocol.ThreadID{"thread-source", "thread-other"}
	handler.sessionLifecycle = lifecycle
	if err := handler.validateStart(&protocol.StartTurnPayload{
		ThreadID: "thread-other", Recovery: recovery,
	}); err != nil {
		t.Fatalf("same-Session cross-Thread recovery error = %v", err)
	}
	recovery.SourceTurnID = "turn-missing"
	if err := handler.validateStart(&protocol.StartTurnPayload{
		ThreadID: "thread-source", Recovery: recovery,
	}); protocol.CodeOf(err) != protocol.CodeConflict {
		t.Fatalf("missing recovery error = %v", err)
	}
}

func TestStartTurnRecoveryAcceptsRecoverableStartOperationRejection(t *testing.T) {
	events := NewMemoryEventStore(4)
	meta := protocol.EventMeta{
		Sequence: 1, OperationID: "operation-source",
		ThreadID: "thread-source", TurnID: "turn-source", ItemID: "item-source",
	}
	started, err := protocol.NewEvent(meta, &protocol.TurnStartedData{
		Provider: "test", Model: "test", Prompt: "continue work",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := events.Append(t.Context(), started); err != nil {
		t.Fatal(err)
	}
	meta.Sequence++
	rejected, err := protocol.NewEvent(meta, &protocol.OperationRejectedData{
		Code:    protocol.CodeUnavailable,
		Message: "terminal envelope could not be committed",
		Fault: &protocol.FaultMetadata{
			Origin:      protocol.FaultOriginPersistence,
			Disposition: protocol.FaultRetryStep,
			SideEffects: protocol.SideEffectDraft,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := events.Append(t.Context(), rejected); err != nil {
		t.Fatal(err)
	}
	handler := StartTurnHandler{Runtime: &Runtime{
		ctx: t.Context(), events: events,
	}}
	recovery := &protocol.TurnRecoveryContext{
		Action: protocol.TurnRecoveryContinue, SourceTurnID: "turn-source",
	}
	if err := handler.validateStart(&protocol.StartTurnPayload{
		ThreadID: "thread-source", Recovery: recovery,
	}); err != nil {
		t.Fatal(err)
	}

	meta = protocol.EventMeta{
		Sequence: 3, OperationID: "operation-other",
		ThreadID: "thread-source", TurnID: "turn-other", ItemID: "item-other",
	}
	otherStarted, err := protocol.NewEvent(meta, &protocol.TurnStartedData{
		Provider: "test", Model: "test", Prompt: "other work",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := events.Append(t.Context(), otherStarted); err != nil {
		t.Fatal(err)
	}
	meta.Sequence++
	meta.OperationID = "child-operation"
	childRejected, err := protocol.NewEvent(meta, &protocol.OperationRejectedData{
		Code:    protocol.CodeUnavailable,
		Message: "child operation failed",
		Fault: &protocol.FaultMetadata{
			Disposition: protocol.FaultRetryStep,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := events.Append(t.Context(), childRejected); err != nil {
		t.Fatal(err)
	}
	recovery.SourceTurnID = "turn-other"
	if err := handler.validateStart(&protocol.StartTurnPayload{
		ThreadID: "thread-source", Recovery: recovery,
	}); protocol.CodeOf(err) != protocol.CodeConflict {
		t.Fatalf("child rejection recovery error = %v, want conflict", err)
	}
}

func receiveEvent(t *testing.T, events <-chan protocol.Event) protocol.Event {
	t.Helper()
	select {
	case event, open := <-events:
		if !open {
			t.Fatal("event stream closed")
		}
		return event
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for event")
		return protocol.Event{}
	}
}

func submitEventually(t *testing.T, runtime *Runtime, operation protocol.Operation) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		err := runtime.Submit(context.Background(), operation)
		if err == nil {
			return
		}
		if !errors.Is(err, ErrQueueFull) || time.Now().After(deadline) {
			t.Fatalf("Submit() error = %v", err)
		}
		time.Sleep(time.Millisecond)
	}
}

func waitForProcessed(t *testing.T, runtime *Runtime, count uint64) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for runtime.Snapshot(t.Context()).OperationsProcessed != count {
		if time.Now().After(deadline) {
			t.Fatalf("runtime did not process %d operations", count)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRuntimeIdleTurnStarts(t *testing.T) {
	runtime := NewRuntime(Options{
		Engine: &testEngine{}, SubscriberBuffer: 8,
	})
	t.Cleanup(func() { closeRuntime(t, runtime) })
	events, err := runtime.Events(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := protocol.NewOperation(&protocol.StartTurnPayload{
		ThreadID: "thread-idle", TurnID: "turn-idle", ItemID: "item-idle",
		Prompt: "auto work", Idle: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Submit(t.Context(), operation); err != nil {
		t.Fatal(err)
	}
	event := receiveEvent(t, events)
	if event.Kind != protocol.EventTurnStarted {
		t.Fatalf("event = %s, want turn.started", event.Kind)
	}
}

func TestRuntimeToolAndApprovalGetOwnedItemIDs(t *testing.T) {
	runtime := NewRuntime(Options{
		Engine: &itemOwningEngine{}, SubscriberBuffer: 16,
	})
	t.Cleanup(func() { closeRuntime(t, runtime) })
	events, err := runtime.Events(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	start := startOperation(t, 42)
	if err := runtime.Submit(t.Context(), start); err != nil {
		t.Fatal(err)
	}
	_, startTurnItem := mustReceiveKinds(t, events,
		protocol.EventTurnStarted,
		protocol.EventApprovalRequired,
		protocol.EventToolResult,
		protocol.EventTurnCompleted,
	)
	approval := mustFindKind(t, startTurnItem, protocol.EventApprovalRequired)
	tool := mustFindKind(t, startTurnItem, protocol.EventToolResult)
	completed := mustFindKind(t, startTurnItem, protocol.EventTurnCompleted)
	_, _, startItem := protocol.OperationReferences(start)
	if approval.ItemID == "" || approval.ItemID == startItem {
		t.Fatalf("approval ItemID = %q start = %q", approval.ItemID, startItem)
	}
	if tool.ItemID == "" || tool.ItemID == startItem || tool.ItemID == approval.ItemID {
		t.Fatalf("tool ItemID = %q approval = %q start = %q", tool.ItemID, approval.ItemID, startItem)
	}
	if completed.ItemID != startItem {
		t.Fatalf("completed ItemID = %q want start %q", completed.ItemID, startItem)
	}
}

func TestRuntimeToolItemIDsAreScopedToTurn(t *testing.T) {
	runtime := NewRuntime(Options{
		Engine: &testEngine{}, SubscriberBuffer: 16,
	})
	t.Cleanup(func() { closeRuntime(t, runtime) })
	events, err := runtime.Events(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, turnID := range []protocol.TurnID{"turn-a", "turn-b", "turn-a"} {
		if err := runtime.publish(
			"operation-"+protocol.OperationID(turnID),
			"thread",
			turnID,
			"fallback-"+protocol.ItemID(turnID),
			&protocol.ToolResultData{
				Tool: "turn_complete", CallID: "shared-call", Output: "ok",
			},
		); err != nil {
			t.Fatal(err)
		}
	}
	first := receiveEvent(t, events)
	second := receiveEvent(t, events)
	repeated := receiveEvent(t, events)
	if first.ItemID == second.ItemID {
		t.Fatalf(
			"tool ItemID reused across turns: %q for %s and %s",
			first.ItemID,
			first.TurnID,
			second.TurnID,
		)
	}
	if repeated.ItemID != first.ItemID {
		t.Fatalf(
			"tool ItemID changed within turn: first %q repeated %q",
			first.ItemID,
			repeated.ItemID,
		)
	}
}

type itemOwningEngine struct{}

func (*itemOwningEngine) StartTurn(
	_ context.Context, _ *protocol.StartTurnPayload, sink EngineSink,
) error {
	if err := sink.Emit(&protocol.TurnStartedData{Provider: "test", Model: "test"}); err != nil {
		return err
	}
	if err := sink.Emit(&protocol.ApprovalRequiredData{
		RequestID: "req-1", CallID: "call-1", Tool: "exec_command",
		Arguments: []byte(`{}`), ArgumentsDigest: "x",
		ExpiresAt:     time.Now().Add(time.Minute),
		AllowedScopes: []protocol.ApprovalScope{protocol.ApprovalScopeOnce},
		Effect:        "process.mutating",
		Risk:          "high",
		ReasonCode:    "approval_required",
	}); err != nil {
		return err
	}
	if err := sink.Emit(&protocol.ToolResultData{
		Tool: "exec_command", CallID: "call-1", Output: "ok",
	}); err != nil {
		return err
	}
	return sink.Emit(&protocol.TurnCompletedData{})
}
func (*itemOwningEngine) CancelTurn(context.Context, *protocol.CancelTurnPayload, EngineSink) error {
	return nil
}
func (*itemOwningEngine) SteerTurn(context.Context, *protocol.SteerTurnPayload, EngineSink) error {
	return nil
}
func (*itemOwningEngine) DecideApproval(context.Context, *protocol.ApprovalDecisionPayload, EngineSink) error {
	return nil
}
func (*itemOwningEngine) ReplyInput(context.Context, *protocol.InputReplyPayload, EngineSink) error {
	return nil
}
func (*itemOwningEngine) CompactThread(context.Context, *protocol.CompactThreadPayload, EngineSink) error {
	return nil
}
func (*itemOwningEngine) ForkThread(context.Context, *protocol.ForkThreadPayload, EngineSink) error {
	return nil
}
func (*itemOwningEngine) RevertTurn(context.Context, *protocol.RevertTurnPayload, EngineSink) error {
	return nil
}

func mustReceiveKinds(t *testing.T, events <-chan protocol.Event, kinds ...protocol.EventKind) (protocol.ItemID, []protocol.Event) {
	t.Helper()
	out := make([]protocol.Event, 0, len(kinds))
	var startItem protocol.ItemID
	for range kinds {
		event := receiveEvent(t, events)
		out = append(out, event)
		if event.Kind == protocol.EventTurnStarted {
			startItem = event.ItemID
		}
	}
	return startItem, out
}

func mustFindKind(t *testing.T, events []protocol.Event, kind protocol.EventKind) protocol.Event {
	t.Helper()
	for _, event := range events {
		if event.Kind == kind {
			return event
		}
	}
	for _, event := range events {
		t.Logf("observed event kind=%s data=%+v", event.Kind, event.Data)
	}
	t.Fatalf("missing event kind %s", kind)
	return protocol.Event{}
}

func TestRuntimeRestoreBackfillsOwnedItemMaps(t *testing.T) {
	runtime := NewRuntime(Options{Engine: &testEngine{}})
	t.Cleanup(func() { closeRuntime(t, runtime) })
	runtime.restore(RecoveryState{
		PendingApprovals: map[string]PendingApproval{
			"req-a": {
				RequestID: "req-a", TurnID: "turn-a", ItemID: "item-approval",
			},
		},
		PendingInputs: map[string]PendingInput{
			"req-i": {
				RequestID: "req-i", TurnID: "turn-i", ItemID: "item-input",
			},
		},
		ToolItems: map[EventItemOwner]protocol.ItemID{
			{TurnID: "turn-t", LocalID: "call-1"}: "item-tool",
		},
	})
	if got := runtime.approvalItems[eventItemOwner("turn-a", "req-a")]; got != "item-approval" {
		t.Fatalf("approvalItems = %q", got)
	}
	if got := runtime.inputItems[eventItemOwner("turn-i", "req-i")]; got != "item-input" {
		t.Fatalf("inputItems = %q", got)
	}
	if got := runtime.toolItems[eventItemOwner("turn-t", "call-1")]; got != "item-tool" {
		t.Fatalf("toolItems = %q", got)
	}
}

func closeRuntime(t *testing.T, runtime *Runtime) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := runtime.Close(ctx); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}
