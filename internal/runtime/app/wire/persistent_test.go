package wire

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	usagestate "github.com/fwtllh-png/QCode/internal/observability/usage"
	"github.com/fwtllh-png/QCode/internal/persist/state"
	turnstate "github.com/fwtllh-png/QCode/internal/persist/state/turnstate"
	threadstate "github.com/fwtllh-png/QCode/internal/persist/thread"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	"github.com/fwtllh-png/QCode/internal/runtime/app"
	apppersistence "github.com/fwtllh-png/QCode/internal/runtime/app/persistence"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func newPersistentRuntime(
	ctx context.Context,
	options PersistentRuntimeOptions,
) (*app.Runtime, error) {
	runtime, err := PreparePersistentRuntime(ctx, options)
	if err != nil {
		return nil, err
	}
	if err := runtime.Start(ctx); err != nil {
		_ = runtime.Close(context.Background())
		return nil, err
	}
	return runtime, nil
}

type persistentTestEngine struct {
	starts      atomic.Int64
	sideEffects atomic.Int64
	block       bool
}

func (e *persistentTestEngine) StartTurn(
	ctx context.Context,
	payload *protocol.StartTurnPayload,
	sink app.EngineSink,
) error {
	e.starts.Add(1)
	if err := sink.Emit(&protocol.TurnStartedData{Provider: "test", Model: "test"}); err != nil {
		return err
	}
	e.sideEffects.Add(1)
	if e.block {
		<-ctx.Done()
		return commitPersistentTestTerminal(
			payload,
			sink,
			protocol.CancelReasonShutdown,
		)
	}
	return commitPersistentTestTerminal(payload, sink, "")
}

func commitPersistentTestTerminal(
	payload *protocol.StartTurnPayload,
	sink app.EngineSink,
	cancelReason string,
) error {
	commitSink, ok := sink.(app.TerminalCommitSink)
	if !ok {
		return errors.New("persistent test engine requires terminal commit sink")
	}
	state := turnkernel.NewStateWithPolicy(
		protocol.TurnIntentAnswer,
		"act",
		1,
		turnkernel.Policy{},
	)
	apply := func(command turnkernel.Command) error {
		transition, err := (turnkernel.Reducer{}).Apply(state, command)
		if err != nil {
			return err
		}
		state = transition.State
		return nil
	}
	if err := apply(turnkernel.StartTurn{}); err != nil {
		return err
	}
	if err := apply(turnkernel.PreparationFinished{}); err != nil {
		return err
	}
	var terminal protocol.EventData
	receipt := &protocol.ExecutionReceiptData{
		Goal:   payload.Prompt,
		Intent: protocol.TurnIntentAnswer,
	}
	if cancelReason == "" {
		if err := apply(turnkernel.ModelTextReceived{
			Text: payload.Prompt,
		}); err != nil {
			return err
		}
		if err := apply(turnkernel.ReleaseProvisionalOutput{}); err != nil {
			return err
		}
		if err := apply(turnkernel.TerminalRequested{}); err != nil {
			return err
		}
		receipt.Outcome = protocol.TurnOutcomeAnswered
		terminal = &protocol.TurnCompletedData{
			Text:    payload.Prompt,
			Outcome: protocol.TurnOutcomeAnswered,
		}
	} else {
		if err := apply(turnkernel.TerminalRequested{
			CancelReason: cancelReason,
		}); err != nil {
			return err
		}
		terminal = &protocol.TurnCanceledData{Reason: cancelReason}
	}
	if err := apply(turnkernel.FinishTerminal{}); err != nil {
		return err
	}
	digest, err := turnkernel.Digest(state)
	if err != nil {
		return err
	}
	measurement, err := turnkernel.NewTerminalMeasurementSnapshot(
		time.Now().UTC(),
		nil,
		state.Usage,
		true,
	)
	if err != nil {
		return err
	}
	receipt.MeasurementDigest = measurement.Digest
	receipt.UsageDigest = measurement.UsageDigest
	return commitSink.CommitTerminal(app.TerminalMaterial{
		FrozenState: state,
		DomainFacts: []turnkernel.DomainFact{{
			TurnID:      string(payload.TurnID),
			Sequence:    1,
			Command:     "finish_terminal",
			State:       state,
			StateDigest: digest,
		}},
		Measurement: measurement,
		Receipt:     receipt,
		Terminal:    terminal,
	})
}

func (*persistentTestEngine) CancelTurn(
	context.Context, *protocol.CancelTurnPayload, app.EngineSink,
) error {
	return nil
}

func (*persistentTestEngine) SteerTurn(
	context.Context, *protocol.SteerTurnPayload, app.EngineSink,
) error {
	return app.ErrOperationUnsupported
}

func (*persistentTestEngine) DecideApproval(
	context.Context, *protocol.ApprovalDecisionPayload, app.EngineSink,
) error {
	return app.ErrOperationUnsupported
}

func (*persistentTestEngine) ReplyInput(
	context.Context, *protocol.InputReplyPayload, app.EngineSink,
) error {
	return app.ErrOperationUnsupported
}

func (*persistentTestEngine) CompactThread(
	context.Context, *protocol.CompactThreadPayload, app.EngineSink,
) error {
	return app.ErrOperationUnsupported
}

func (*persistentTestEngine) ForkThread(
	context.Context, *protocol.ForkThreadPayload, app.EngineSink,
) error {
	return app.ErrOperationUnsupported
}

func (*persistentTestEngine) RevertTurn(
	context.Context, *protocol.RevertTurnPayload, app.EngineSink,
) error {
	return app.ErrOperationUnsupported
}

func TestPersistentWorkspaceRuntimesShareStoreWithoutCrossingEvents(
	t *testing.T,
) {
	store, err := state.Open(t.Context(), state.Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.CloseAll(context.Background()) })
	repositories, err := apppersistence.NewPersistentRepositories(store)
	if err != nil {
		t.Fatal(err)
	}
	rootA := t.TempDir()
	rootB := t.TempDir()
	for _, fixture := range []struct {
		root      string
		sessionID string
		threadID  protocol.ThreadID
	}{
		{rootA, "session-a", "thread-a"},
		{rootB, "session-b", "thread-b"},
	} {
		if err := repositories.Sessions.EnsureSeed(
			t.Context(),
			fixture.sessionID,
			fixture.root,
		); err != nil {
			t.Fatal(err)
		}
		if _, err := repositories.Threads.Create(
			t.Context(),
			threadstate.Thread{
				ID: fixture.threadID, SessionID: fixture.sessionID,
				Status: threadstate.ThreadOpen,
			},
		); err != nil {
			t.Fatal(err)
		}
	}
	runtimeA, err := newPersistentRuntime(
		t.Context(),
		PersistentRuntimeOptions{
			Store: store, WorkspaceRoot: rootA, Engine: app.NoopEngine{},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closePersistentRuntime(t, runtimeA) })
	runtimeB, err := newPersistentRuntime(
		t.Context(),
		PersistentRuntimeOptions{
			Store: store, WorkspaceRoot: rootB, Engine: app.NoopEngine{},
		},
	)
	if err != nil {
		closePersistentRuntime(t, runtimeA)
		t.Fatal(err)
	}
	t.Cleanup(func() { closePersistentRuntime(t, runtimeB) })
	eventsA, err := runtimeA.Events(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	eventsB, err := runtimeB.Events(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	operation := func(
		threadID protocol.ThreadID,
		turnID protocol.TurnID,
	) protocol.Operation {
		value, err := protocol.NewOperation(&protocol.StartTurnPayload{
			ThreadID: threadID, TurnID: turnID,
			ItemID: protocol.ItemID("item-" + turnID), Prompt: "run",
		})
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	operationA := operation("thread-a", "turn-a")
	operationB := operation("thread-b", "turn-b")
	var submit sync.WaitGroup
	submit.Go(func() {
		if err := runtimeA.Submit(t.Context(), operationA); err != nil {
			t.Errorf("submit Workspace A: %v", err)
		}
	})
	submit.Go(func() {
		if err := runtimeB.Submit(t.Context(), operationB); err != nil {
			t.Errorf("submit Workspace B: %v", err)
		}
	})
	submit.Wait()
	assertWorkspaceTerminal := func(
		events <-chan protocol.Event,
		threadID protocol.ThreadID,
	) {
		t.Helper()
		for {
			event := waitForEvent(t, events)
			if event.ThreadID != threadID {
				t.Fatalf("Workspace %s received event %+v", threadID, event)
			}
			if protocol.IsTerminalEvent(event.Kind) {
				return
			}
		}
	}
	assertWorkspaceTerminal(eventsA, "thread-a")
	assertWorkspaceTerminal(eventsB, "thread-b")

	closePersistentRuntime(t, runtimeA)
	cursor := runtimeB.Snapshot(t.Context()).LastSequence
	continued, err := runtimeB.Events(t.Context(), cursor)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtimeB.Submit(
		t.Context(),
		operation("thread-b", "turn-b-next"),
	); err != nil {
		t.Fatalf("Workspace B stopped with Workspace A: %v", err)
	}
	assertWorkspaceTerminal(continued, "thread-b")
}

func TestPersistentRuntimeRestartIsIdempotentAndKeepsOneTerminal(t *testing.T) {
	root := t.TempDir()
	store := seedPersistentState(t, root)
	engine := &persistentTestEngine{}
	runtime, err := newPersistentRuntime(t.Context(), PersistentRuntimeOptions{
		Store: store, Engine: engine,
	})
	if err != nil {
		t.Fatal(err)
	}
	events, err := runtime.Events(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	operation := persistentStartOperation(t, "turn-1", "item-1")
	if err := runtime.SubmitWithKey(t.Context(), operation, "request-1"); err != nil {
		t.Fatal(err)
	}
	waitForTerminal(t, events, operation.ID)
	for _, id := range []protocol.OperationID{operation.ID, operation.ID + "-retry"} {
		duplicate := operation
		duplicate.ID = id
		if err := runtime.SubmitWithKey(t.Context(), duplicate, "request-1"); err != nil {
			t.Fatal(err)
		}
	}
	closePersistentRuntime(t, runtime)

	reopened, err := state.Open(t.Context(), state.Options{DataDir: root})
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := newPersistentRuntime(t.Context(), PersistentRuntimeOptions{
		Store: reopened, Engine: engine,
	})
	if err != nil {
		t.Fatal(err)
	}
	before := recovered.Snapshot(t.Context())
	for _, id := range []protocol.OperationID{operation.ID, operation.ID + "-retry"} {
		duplicate := operation
		duplicate.ID = id
		if err := recovered.SubmitWithKey(t.Context(), duplicate, "request-1"); err != nil {
			t.Fatal(err)
		}
	}
	after := recovered.Snapshot(t.Context())
	if after.LastSequence != before.LastSequence || after.OperationsProcessed != 0 {
		t.Fatalf("duplicate changed runtime state: before=%+v after=%+v", before, after)
	}
	if engine.starts.Load() != 1 || engine.sideEffects.Load() != 1 {
		t.Fatalf(
			"engine starts=%d side effects=%d, want one each",
			engine.starts.Load(), engine.sideEffects.Load(),
		)
	}

	conflict := operation
	conflict.Payload = &protocol.StartTurnPayload{
		ThreadID: "thread-1", TurnID: "turn-1", ItemID: "item-1", Prompt: "different",
	}
	for _, id := range []protocol.OperationID{operation.ID, operation.ID + "-conflict"} {
		conflict.ID = id
		if err := recovered.SubmitWithKey(t.Context(), conflict, "request-1"); !errors.Is(err, app.ErrOperationConflict) {
			t.Fatalf("conflicting operation error = %v, want ErrOperationConflict", err)
		}
	}
	replayed, err := reopened.Replay(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	terminals := 0
	for _, event := range replayed {
		if event.TurnID == "turn-1" && protocol.IsTerminalEvent(event.Kind) {
			terminals++
		}
	}
	if terminals != 1 {
		t.Fatalf("terminal events = %d, want exactly one", terminals)
	}
	var operationStatus string
	var receipt sql.NullString
	if err := reopened.SQLite().DB().QueryRowContext(t.Context(), `
		SELECT status, response_json FROM operations WHERE id = ?`, operation.ID,
	).Scan(&operationStatus, &receipt); err != nil {
		t.Fatal(err)
	}
	if operationStatus != string(threadstate.OperationCommitted) || !receipt.Valid {
		t.Fatalf("operation status=%q receipt=%+v", operationStatus, receipt)
	}
	closePersistentRuntime(t, recovered)
}

func TestC5SQLiteConcurrentOutboxRecoveryProjectsStableEventsOnce(
	t *testing.T,
) {
	store := seedPersistentState(t, t.TempDir())
	t.Cleanup(func() { _ = store.CloseAll(context.Background()) })
	repositories, err := apppersistence.NewPersistentRepositories(store)
	if err != nil {
		t.Fatal(err)
	}
	operation := persistentStartOperation(
		t,
		"turn-round13-outbox",
		"item-round13-outbox",
	)
	canonical, err := app.CanonicalOperationPayload(operation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repositories.Lifecycle.Accept(
		t.Context(),
		operation,
		"round13-outbox",
		canonical,
	); err != nil {
		t.Fatal(err)
	}
	envelope := round13TerminalEnvelope(t, operation)
	terminalStore := turnstate.NewSQLiteRepository(store.SQLite())
	if _, err := terminalStore.CommitTerminalOperation(
		t.Context(),
		envelope,
	); err != nil {
		t.Fatal(err)
	}
	receiptEntry := envelope.Outbox[0]
	var receiptData protocol.ExecutionReceiptData
	if err := json.Unmarshal(receiptEntry.Payload, &receiptData); err != nil {
		t.Fatal(err)
	}
	interruptedEvent, err := protocol.NewEventWithIdentity(
		protocol.EventMeta{
			Sequence:    1,
			OperationID: receiptEntry.OperationID,
			ThreadID:    receiptEntry.ThreadID,
			TurnID:      receiptEntry.TurnID,
			ItemID:      receiptEntry.ItemID,
		},
		receiptEntry.EventID,
		time.Now(),
		&receiptData,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(t.Context(), interruptedEvent); err != nil {
		t.Fatal(err)
	}

	var wait sync.WaitGroup
	results := make(chan *app.Runtime, 2)
	errs := make(chan error, 2)
	for range 2 {
		wait.Go(func() {
			runtime, openErr := newPersistentRuntime(
				t.Context(),
				PersistentRuntimeOptions{
					Store:  store,
					Engine: app.NoopEngine{},
				},
			)
			if openErr == nil {
				results <- runtime
			}
			errs <- openErr
		})
	}
	wait.Wait()
	close(results)
	close(errs)
	var runtimes []*app.Runtime
	for runtime := range results {
		runtimes = append(runtimes, runtime)
	}
	for openErr := range errs {
		if openErr != nil {
			t.Fatal(openErr)
		}
	}
	t.Cleanup(func() {
		for _, runtime := range runtimes {
			_ = runtime.Close(context.Background())
		}
	})

	events, err := store.Replay(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	var receipts, terminals int
	eventIDs := make(map[protocol.EventID]struct{}, len(events))
	for _, event := range events {
		eventIDs[event.ID] = struct{}{}
		switch event.Kind {
		case protocol.EventExecutionReceipt:
			receipts++
		case protocol.EventTurnCompleted:
			terminals++
		}
	}
	if len(events) != 2 || len(eventIDs) != 2 ||
		receipts != 1 || terminals != 1 {
		t.Fatalf(
			"concurrent SQLite projection events=%+v ids=%v receipts=%d terminals=%d",
			events,
			eventIDs,
			receipts,
			terminals,
		)
	}
	var pending int
	if err := store.SQLite().DB().QueryRowContext(
		t.Context(),
		`SELECT COUNT(*) FROM turn_terminal_outbox WHERE published = 0`,
	).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatalf("pending terminal outbox rows = %d", pending)
	}
	var status string
	var response sql.NullString
	if err := store.SQLite().DB().QueryRowContext(
		t.Context(),
		`SELECT status, response_json FROM operations WHERE id = ?`,
		operation.ID,
	).Scan(&status, &response); err != nil {
		t.Fatal(err)
	}
	if status != string(threadstate.OperationCommitted) ||
		!response.Valid ||
		!json.Valid([]byte(response.String)) {
		t.Fatalf("operation status=%q response=%+v", status, response)
	}
}

func round13TerminalEnvelope(
	t *testing.T,
	operation protocol.Operation,
) turnkernel.TerminalEnvelope {
	t.Helper()
	_, turnID, itemID := protocol.OperationReferences(operation)
	threadID, _, _ := protocol.OperationReferences(operation)
	state := turnkernel.NewStateWithPolicy(
		protocol.TurnIntentAnswer,
		"act",
		1,
		turnkernel.Policy{},
	)
	apply := func(command turnkernel.Command) {
		t.Helper()
		transition, err := (turnkernel.Reducer{}).Apply(state, command)
		if err != nil {
			t.Fatal(err)
		}
		state = transition.State
	}
	apply(turnkernel.StartTurn{})
	apply(turnkernel.PreparationFinished{})
	apply(turnkernel.ModelTextReceived{Text: "done"})
	apply(turnkernel.ReleaseProvisionalOutput{})
	apply(turnkernel.TerminalRequested{})
	apply(turnkernel.FinishTerminal{})
	digest, err := turnkernel.Digest(state)
	if err != nil {
		t.Fatal(err)
	}
	measurement, err := turnkernel.NewTerminalMeasurementSnapshot(
		time.Unix(1, 0),
		nil,
		state.Usage,
		true,
	)
	if err != nil {
		t.Fatal(err)
	}
	receipt := &protocol.ExecutionReceiptData{
		Goal:              "round13",
		Intent:            protocol.TurnIntentAnswer,
		Outcome:           protocol.TurnOutcomeAnswered,
		MeasurementDigest: measurement.Digest,
		UsageDigest:       measurement.UsageDigest,
	}
	receiptPayload, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	terminal := &protocol.TurnCompletedData{
		Text:    "done",
		Outcome: protocol.TurnOutcomeAnswered,
	}
	terminalPayload, err := json.Marshal(terminal)
	if err != nil {
		t.Fatal(err)
	}
	operationReceipt, err := json.Marshal(app.CommitReceipt{
		OperationID: operation.ID,
		Status:      "committed",
		CompletedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	decision := *state.Terminal
	entry := func(
		id string,
		eventID protocol.EventID,
		kind protocol.EventKind,
		payload json.RawMessage,
	) turnkernel.ProjectionOutboxEntry {
		return turnkernel.ProjectionOutboxEntry{
			ID:          id,
			EventID:     eventID,
			OperationID: operation.ID,
			ThreadID:    threadID,
			TurnID:      turnID,
			ItemID:      itemID,
			Kind:        string(kind),
			Payload:     payload,
		}
	}
	return turnkernel.TerminalEnvelope{
		TurnID:      string(turnID),
		EffectID:    "terminal:" + string(turnID),
		FrozenState: state,
		DomainFacts: []turnkernel.DomainFact{{
			TurnID:      string(turnID),
			Sequence:    1,
			Command:     "finish_terminal",
			State:       state,
			StateDigest: digest,
		}},
		Measurement: measurement,
		Receipt:     receipt,
		FinalOutput: append([]string(nil), state.FinalOutput...),
		TerminalEvent: turnkernel.Event{
			Kind:     turnkernel.EventTerminalCommitted,
			Terminal: &decision,
		},
		OperationCommit: turnkernel.OperationCommitFact{
			OperationID: operation.ID,
			Status:      "committed",
			Receipt:     operationReceipt,
		},
		Outbox: []turnkernel.ProjectionOutboxEntry{
			entry(
				"receipt",
				"evt_11111111111111111111111111111111",
				protocol.EventExecutionReceipt,
				receiptPayload,
			),
			entry(
				"terminal",
				"evt_22222222222222222222222222222222",
				protocol.EventTurnCompleted,
				terminalPayload,
			),
		},
	}
}

func TestPersistentRuntimeEnforcesOneActiveTurnPerThread(t *testing.T) {
	store := seedPersistentState(t, t.TempDir())
	engine := &persistentTestEngine{block: true}
	runtime, err := newPersistentRuntime(t.Context(), PersistentRuntimeOptions{
		Store: store, Engine: engine,
	})
	if err != nil {
		t.Fatal(err)
	}
	events, err := runtime.Events(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	first := persistentStartOperation(t, "turn-active", "item-active")
	if err := runtime.Submit(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	waitForKind(t, events, protocol.EventTurnStarted)

	second := persistentStartOperation(t, "turn-other", "item-other")
	if err := runtime.Submit(t.Context(), second); !errors.Is(err, app.ErrActiveTurn) {
		t.Fatalf("second active turn error = %v, want ErrActiveTurn", err)
	}
	closePersistentRuntime(t, runtime)

	reopened, err := state.Open(t.Context(), state.Options{DataDir: store.Root()})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.CloseAll(context.Background())
	replayed, err := reopened.Replay(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	terminals := 0
	for _, event := range replayed {
		if event.TurnID == "turn-active" && protocol.IsTerminalEvent(event.Kind) {
			terminals++
		}
	}
	if terminals != 1 {
		t.Fatalf("active turn terminal events = %d, want one", terminals)
	}
}

func TestPersistentRuntimeRestoresPendingWithoutReplayingEngine(t *testing.T) {
	root := t.TempDir()
	store := seedPersistentState(t, root)
	repositories, err := apppersistence.NewPersistentRepositories(store)
	if err != nil {
		t.Fatal(err)
	}
	operation := persistentStartOperation(t, "turn-pending", "item-pending")
	canonical, err := app.CanonicalOperationPayload(operation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repositories.Lifecycle.Accept(
		t.Context(), operation, "pending-key", canonical,
	); err != nil {
		t.Fatal(err)
	}
	approval := &protocol.ApprovalRequiredData{
		RequestID:       "approval-pending",
		CallID:          "call-pending",
		Tool:            "shell",
		Arguments:       json.RawMessage(`{"command":"true"}`),
		ArgumentsDigest: "digest",
		AllowedScopes:   []protocol.ApprovalScope{protocol.ApprovalScopeOnce},
		ExpiresAt:       time.Now().Add(time.Hour).UTC(),
		Effect:          "process.mutating",
		Risk:            "high",
		ReasonCode:      "approval_required",
	}
	event, err := protocol.NewEvent(protocol.EventMeta{
		Sequence: 1, OperationID: operation.ID,
		ThreadID: "thread-1", TurnID: "turn-pending", ItemID: "item-pending",
	}, approval)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	if err := store.CloseAll(t.Context()); err != nil {
		t.Fatal(err)
	}

	reopened, err := state.Open(t.Context(), state.Options{DataDir: root})
	if err != nil {
		t.Fatal(err)
	}
	engine := &persistentTestEngine{}
	runtime, err := newPersistentRuntime(t.Context(), PersistentRuntimeOptions{
		Store: reopened, Engine: engine,
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := runtime.Snapshot(t.Context())
	if snapshot.PendingApprovals != 1 || snapshot.PendingOperations != 1 ||
		snapshot.LastSequence != 1 {
		t.Fatalf("recovered snapshot = %+v", snapshot)
	}
	if err := runtime.SubmitWithKey(t.Context(), operation, "pending-key"); err != nil {
		t.Fatal(err)
	}
	if engine.starts.Load() != 0 || engine.sideEffects.Load() != 0 {
		t.Fatal("pending accepted operation was replayed into Engine")
	}
	if runtime.Snapshot(t.Context()).LastSequence != 1 {
		t.Fatal("pending duplicate emitted a new event")
	}
	closePersistentRuntime(t, runtime)
}

func TestPersistentRepositoriesProjectThreadLifecycleEvents(t *testing.T) {
	store := seedPersistentState(t, t.TempDir())
	defer store.CloseAll(context.Background())
	repositories, err := apppersistence.NewPersistentRepositories(store)
	if err != nil {
		t.Fatal(err)
	}
	accept := func(operation protocol.Operation) {
		t.Helper()
		canonical, err := app.CanonicalOperationPayload(operation)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := repositories.Lifecycle.Accept(
			t.Context(), operation, "", canonical,
		); err != nil {
			t.Fatal(err)
		}
	}
	appendAndProject := func(sequence protocol.Cursor, operation protocol.Operation, data protocol.EventData) {
		t.Helper()
		threadID, turnID, itemID := protocol.OperationReferences(operation)
		event, err := protocol.NewEvent(protocol.EventMeta{
			Sequence: sequence, OperationID: operation.ID,
			ThreadID: threadID, TurnID: turnID, ItemID: itemID,
		}, data)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Append(t.Context(), event); err != nil {
			t.Fatal(err)
		}
		if err := repositories.Lifecycle.Project(t.Context(), event); err != nil {
			t.Fatal(err)
		}
	}

	target := persistentStartOperation(t, "turn-target", "item-target")
	accept(target)
	appendAndProject(1, target, &protocol.TurnCompletedData{Text: "target"})

	current := persistentStartOperation(t, "turn-current", "item-current")
	accept(current)
	appendAndProject(2, current, &protocol.TurnStartedData{Provider: "test", Model: "test"})
	appendAndProject(3, current, &protocol.UsageData{
		InputTokens: 9, OutputTokens: 4, ReasoningTokens: 2,
	})

	fork, err := protocol.NewOperation(&protocol.ForkThreadPayload{
		ThreadID: "thread-1", TurnID: "turn-current",
		ItemID: "item-fork", NewThreadID: "thread-fork",
	})
	if err != nil {
		t.Fatal(err)
	}
	accept(fork)
	appendAndProject(4, fork, &protocol.ThreadForkedData{
		NewThreadID: "thread-fork", SourceCursor: 3,
	})

	compact, err := protocol.NewOperation(&protocol.CompactThreadPayload{
		ThreadID: "thread-1", TurnID: "turn-current", ItemID: "item-compact",
	})
	if err != nil {
		t.Fatal(err)
	}
	accept(compact)
	appendAndProject(5, compact, &protocol.ThreadCompactedData{Summary: "summary"})

	revert, err := protocol.NewOperation(&protocol.RevertTurnPayload{
		ThreadID: "thread-1", TurnID: "turn-current",
		ItemID: "item-revert", TargetTurnID: "turn-target",
	})
	if err != nil {
		t.Fatal(err)
	}
	accept(revert)
	appendAndProject(6, revert, &protocol.TurnRevertedData{
		TargetTurnID: "turn-target",
	})

	parent, err := repositories.Threads.Get(t.Context(), "thread-1")
	if err != nil {
		t.Fatal(err)
	}
	if parent.LatestCursor != 6 {
		t.Fatalf("latest cursor = %d, want 6", parent.LatestCursor)
	}
	forked, err := repositories.Threads.Get(t.Context(), "thread-fork")
	if err != nil {
		t.Fatal(err)
	}
	if forked.ParentThreadID != "thread-1" || forked.SessionID != "session-1" {
		t.Fatalf("forked thread = %+v", forked)
	}
	if forked.SourceCursor != 3 {
		t.Fatalf("forked source cursor = %d, want 3", forked.SourceCursor)
	}
	targetTurn, err := repositories.Threads.GetTurn(t.Context(), "turn-target")
	if err != nil {
		t.Fatal(err)
	}
	if targetTurn.Status != threadstate.TurnCompleted {
		t.Fatalf("target turn status = %q", targetTurn.Status)
	}
	compactedItem, err := repositories.Threads.GetItem(t.Context(), "item-compact")
	if err != nil {
		t.Fatal(err)
	}
	if compactedItem.Kind != string(protocol.EventThreadCompacted) {
		t.Fatalf("compaction item kind = %q", compactedItem.Kind)
	}
	aggregates, err := repositories.Usage.QueryAggregates(t.Context(), usagestate.Query{
		SessionID: "session-1", TurnID: "turn-current",
		Provider: "test", Model: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(aggregates) != 1 || aggregates[0].InputTokens != 9 ||
		aggregates[0].OutputTokens != 4 || aggregates[0].ReasoningTokens != 2 {
		t.Fatalf("projected usage = %+v", aggregates)
	}
}

func TestPersistentRecoveryIgnoresEventsFromDeletedSessions(t *testing.T) {
	root := t.TempDir()
	store := seedPersistentState(t, root)
	defer store.CloseAll(context.Background())
	event, err := protocol.NewEvent(protocol.EventMeta{
		Sequence: 1, OperationID: "operation-deleted",
		ThreadID: "thread-1", TurnID: "turn-deleted", ItemID: "item-deleted",
	}, &protocol.TurnStartedData{Provider: "test", Model: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQLite().DB().ExecContext(
		t.Context(),
		"DELETE FROM sessions WHERE id = 'session-1'",
	); err != nil {
		t.Fatal(err)
	}
	repositories, err := apppersistence.NewPersistentRepositories(store)
	if err != nil {
		t.Fatal(err)
	}
	if err := repositories.Sessions.EnsureSeed(
		t.Context(),
		"session-replacement",
		filepath.Join(root, "workspace"),
	); err != nil {
		t.Fatal(err)
	}
	_, err = repositories.Threads.Create(t.Context(), threadstate.Thread{
		ID: "thread-replacement", SessionID: "session-replacement",
	})
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := repositories.Lifecycle.Recover(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if recovery.LastSequence != 1 {
		t.Fatalf("last sequence = %d, want 1", recovery.LastSequence)
	}
	var usageContexts int
	if err := store.SQLite().DB().QueryRowContext(
		t.Context(),
		"SELECT COUNT(*) FROM usage_turn_context",
	).Scan(&usageContexts); err != nil {
		t.Fatal(err)
	}
	if usageContexts != 0 {
		t.Fatalf("deleted Session usage contexts = %d, want 0", usageContexts)
	}
}

func seedPersistentState(t *testing.T, root string) *state.Store {
	t.Helper()
	store, err := state.Open(t.Context(), state.Options{DataDir: root})
	if err != nil {
		t.Fatal(err)
	}
	repositories, err := apppersistence.NewPersistentRepositories(store)
	if err != nil {
		t.Fatal(err)
	}
	if err := repositories.Sessions.EnsureSeed(
		t.Context(),
		"session-1",
		filepath.Join(root, "workspace"),
	); err != nil {
		t.Fatal(err)
	}
	_, err = repositories.Threads.Create(t.Context(), threadstate.Thread{
		ID: "thread-1", SessionID: "session-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func persistentStartOperation(
	t *testing.T,
	turnID protocol.TurnID,
	itemID protocol.ItemID,
) protocol.Operation {
	t.Helper()
	operation, err := protocol.NewOperation(&protocol.StartTurnPayload{
		ThreadID: "thread-1", TurnID: turnID, ItemID: itemID, Prompt: "persist me",
	})
	if err != nil {
		t.Fatal(err)
	}
	return operation
}

func waitForTerminal(
	t *testing.T,
	events <-chan protocol.Event,
	operationID protocol.OperationID,
) {
	t.Helper()
	for {
		event := waitForEvent(t, events)
		if event.OperationID == operationID && protocol.IsTerminalEvent(event.Kind) {
			return
		}
	}
}

func waitForKind(
	t *testing.T,
	events <-chan protocol.Event,
	kind protocol.EventKind,
) {
	t.Helper()
	for {
		if event := waitForEvent(t, events); event.Kind == kind {
			return
		}
	}
}

func waitForEvent(t *testing.T, events <-chan protocol.Event) protocol.Event {
	t.Helper()
	select {
	case event, ok := <-events:
		if !ok {
			t.Fatal("event stream closed")
		}
		return event
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for persistent runtime event")
		return protocol.Event{}
	}
}

func closePersistentRuntime(t *testing.T, runtime *app.Runtime) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := runtime.Close(ctx); err != nil {
		t.Fatal(err)
	}
}
