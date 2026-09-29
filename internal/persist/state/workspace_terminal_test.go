package state_test

import (
	"context"
	"testing"
	"time"

	threadstate "github.com/fwtllh-png/QCode/internal/persist/thread"
	sessionstate "github.com/fwtllh-png/QCode/internal/persist/session"
	"github.com/fwtllh-png/QCode/internal/persist/state"
	turnstate "github.com/fwtllh-png/QCode/internal/persist/state/turnstate"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestWorkspaceTerminalStoreRecoversOnlyBoundProjections(t *testing.T) {
	store, err := state.Open(t.Context(), state.Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.CloseAll(context.Background()) })
	sessions := sessionstate.NewSQLiteRepository(store.SQLite())
	threads := threadstate.NewSQLiteRepository(store.SQLite())
	rootA := t.TempDir()
	rootB := t.TempDir()
	for _, fixture := range []struct {
		suffix string
		root   string
	}{
		{"a", rootA},
		{"b", rootB},
	} {
		sessionID := "session-" + fixture.suffix
		threadID := protocol.ThreadID("thread-" + fixture.suffix)
		turnID := protocol.TurnID("turn-" + fixture.suffix)
		if err := sessions.EnsureSeed(
			t.Context(),
			sessionID,
			fixture.root,
		); err != nil {
			t.Fatal(err)
		}
		if _, err := threads.Create(
			t.Context(),
			threadstate.Thread{
				ID: threadID, SessionID: sessionID, Status: threadstate.ThreadOpen,
			},
		); err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if _, err := store.SQLite().DB().ExecContext(
			t.Context(),
			`INSERT INTO turns(
				id, thread_id, ordinal, status, created_at, updated_at
			 ) VALUES (?, ?, 0, 'active', ?, ?)`,
			turnID,
			threadID,
			now,
			now,
		); err != nil {
			t.Fatal(err)
		}
		envelope := workspaceTerminalEnvelope(t, fixture.suffix)
		if _, err := turnstate.NewSQLiteRepository(store.SQLite()).CommitTerminal(
			t.Context(),
			envelope,
		); err != nil {
			t.Fatal(err)
		}
	}
	projections, err := state.NewWorkspaceTerminalStore(
		store.SQLite(),
		rootA,
	).PendingTerminalProjections(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(projections) != 1 ||
		projections[0].Envelope.TurnID != "turn-a" {
		t.Fatalf("Workspace terminal projections = %+v", projections)
	}
}

func workspaceTerminalEnvelope(
	t *testing.T,
	suffix string,
) turnkernel.TerminalEnvelope {
	t.Helper()
	reducer := turnkernel.Reducer{}
	current := turnkernel.NewState(protocol.TurnIntentAnswer, "act", 1)
	apply := func(command turnkernel.Command) {
		transition, err := reducer.Apply(current, command)
		if err != nil {
			t.Fatal(err)
		}
		current = transition.State
	}
	apply(turnkernel.StartTurn{})
	apply(turnkernel.PreparationFinished{})
	apply(turnkernel.ModelTextReceived{Text: "done"})
	apply(turnkernel.ReleaseProvisionalOutput{})
	apply(turnkernel.TerminalRequested{})
	apply(turnkernel.FinishTerminal{})
	digest, err := turnkernel.Digest(current)
	if err != nil {
		t.Fatal(err)
	}
	measurement, err := turnkernel.NewTerminalMeasurementSnapshot(
		time.Unix(1, 0),
		nil,
		current.Usage,
		true,
	)
	if err != nil {
		t.Fatal(err)
	}
	turnID := "turn-" + suffix
	threadID := protocol.ThreadID("thread-" + suffix)
	operationID := protocol.OperationID("operation-" + suffix)
	itemID := protocol.ItemID("item-" + suffix)
	entry := func(
		id string,
		kind protocol.EventKind,
	) turnkernel.ProjectionOutboxEntry {
		return turnkernel.ProjectionOutboxEntry{
			ID: id + "-" + suffix,
			EventID: protocol.EventID(
				"event-" + id + "-" + suffix,
			),
			OperationID: operationID,
			ThreadID:    threadID,
			TurnID:      protocol.TurnID(turnID),
			ItemID:      itemID,
			Kind:        string(kind),
			Payload:     []byte(`{}`),
		}
	}
	decision := *current.Terminal
	return turnkernel.TerminalEnvelope{
		TurnID:      turnID,
		EffectID:    "terminal:" + turnID,
		FrozenState: current,
		DomainFacts: []turnkernel.DomainFact{{
			TurnID: turnID, Sequence: 1, Command: "finish_terminal",
			State: current, StateDigest: digest,
		}},
		Measurement: measurement,
		Receipt: &protocol.ExecutionReceiptData{
			Goal: "answer", Intent: protocol.TurnIntentAnswer,
			Outcome:           protocol.TurnOutcomeAnswered,
			MeasurementDigest: measurement.Digest,
			UsageDigest:       measurement.UsageDigest,
		},
		FinalOutput: current.FinalOutput,
		TerminalEvent: turnkernel.Event{
			Kind: turnkernel.EventTerminalCommitted, Terminal: &decision,
		},
		OperationCommit: turnkernel.OperationCommitFact{
			OperationID: operationID, Status: "committed",
		},
		Outbox: []turnkernel.ProjectionOutboxEntry{
			entry("receipt", protocol.EventExecutionReceipt),
			entry("terminal", protocol.EventTurnCompleted),
		},
	}
}
