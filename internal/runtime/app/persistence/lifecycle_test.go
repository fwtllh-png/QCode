package persistence

import (
	"path/filepath"
	"testing"
	"time"

	sessionstate "github.com/fwtllh-png/QCode/internal/persist/session"
	"github.com/fwtllh-png/QCode/internal/persist/state"
	threadstate "github.com/fwtllh-png/QCode/internal/persist/thread"
	"github.com/fwtllh-png/QCode/internal/runtime/app"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestRejectedStartTurnReleasesThreadForRetry(t *testing.T) {
	store, err := state.Open(t.Context(), state.Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close(t.Context()) })
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, statement := range []string{
		`INSERT INTO workspaces(id, root_path, created_at, updated_at)
		 VALUES ('workspace', '/workspace', ?, ?)`,
		`INSERT INTO sessions(id, workspace_id, status, created_at, updated_at)
		 VALUES ('session', 'workspace', 'open', ?, ?)`,
		`INSERT INTO threads(id, session_id, title, status, created_at, updated_at)
		 VALUES ('thread', 'session', 'chat', 'open', ?, ?)`,
	} {
		if _, err := store.SQLite().DB().ExecContext(
			t.Context(), statement, now, now,
		); err != nil {
			t.Fatal(err)
		}
	}
	lifecycle := NewLifecycle(store)
	start := func(operationID, turnID, itemID string) protocol.Operation {
		operation, err := protocol.NewOperation(&protocol.StartTurnPayload{
			ThreadID: "thread", TurnID: protocol.TurnID(turnID),
			ItemID: protocol.ItemID(itemID), Prompt: "implement",
		})
		if err != nil {
			t.Fatal(err)
		}
		operation.ID = protocol.OperationID(operationID)
		canonical, err := app.CanonicalOperationPayload(operation)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := lifecycle.Accept(
			t.Context(), operation, operationID, canonical,
		); err != nil {
			t.Fatal(err)
		}
		return operation
	}
	rejected := start("operation-1", "turn-1", "item-1")
	event, err := protocol.NewEvent(protocol.EventMeta{
		Sequence: 1, OperationID: rejected.ID,
		ThreadID: "thread", TurnID: "turn-1", ItemID: "item-1",
	}, &protocol.OperationRejectedData{
		Code: protocol.CodeConflict, Message: "rejected",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.Project(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	var status threadstate.TurnStatus
	if err := store.SQLite().DB().QueryRowContext(
		t.Context(),
		`SELECT status FROM turns WHERE id = 'turn-1'`,
	).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != threadstate.TurnFailed {
		t.Fatalf("rejected turn status = %s, want %s", status, threadstate.TurnFailed)
	}
	start("operation-2", "turn-2", "item-2")
}

func TestRecoverRequeuesCommittedStartForActiveTurnWithoutTerminal(t *testing.T) {
	store, err := state.Open(t.Context(), state.Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close(t.Context()) })
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, statement := range []string{
		`INSERT INTO workspaces(id, root_path, created_at, updated_at)
		 VALUES ('workspace', '/workspace', ?, ?)`,
		`INSERT INTO sessions(id, workspace_id, status, created_at, updated_at)
		 VALUES ('session', 'workspace', 'open', ?, ?)`,
		`INSERT INTO threads(id, session_id, title, status, created_at, updated_at)
		 VALUES ('thread', 'session', 'chat', 'open', ?, ?)`,
		`INSERT INTO operations(
			id, session_id, kind, status, request_json, created_at, updated_at
		 ) VALUES ('operation', 'session', 'turn.start', 'committed', '{}', ?, ?)`,
		`INSERT INTO turns(
			id, thread_id, operation_id, ordinal, status, created_at, updated_at
		 ) VALUES ('turn', 'thread', 'operation', 1, 'active', ?, ?)`,
	} {
		if _, err := store.SQLite().DB().ExecContext(
			t.Context(), statement, now, now,
		); err != nil {
			t.Fatal(err)
		}
	}

	recovery, err := NewLifecycle(store).Recover(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	pending, ok := recovery.PendingOperations["operation"]
	if !ok || pending.SessionID != "session" ||
		string(pending.Canonical) != "{}" {
		t.Fatalf("pending operations = %+v, want interrupted start", recovery.PendingOperations)
	}
}

func TestRecoverRebuildsPendingTurnQueueFromRetainedEvents(t *testing.T) {
	store, err := state.Open(t.Context(), state.Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close(t.Context()) })
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, statement := range []string{
		`INSERT INTO workspaces(id, root_path, created_at, updated_at)
		 VALUES ('workspace', '/workspace', ?, ?)`,
		`INSERT INTO sessions(id, workspace_id, status, created_at, updated_at)
		 VALUES ('session', 'workspace', 'open', ?, ?)`,
		`INSERT INTO threads(id, session_id, title, status, created_at, updated_at)
		 VALUES ('thread', 'session', 'chat', 'open', ?, ?)`,
	} {
		if _, err := store.SQLite().DB().ExecContext(
			t.Context(), statement, now, now,
		); err != nil {
			t.Fatal(err)
		}
	}
	events := []protocol.EventData{
		&protocol.TurnQueuedData{QueueID: "queue-1", Prompt: "before"},
		&protocol.QueuedTurnUpdatedData{QueueID: "queue-1", Prompt: "after"},
	}
	for index, data := range events {
		event, err := protocol.NewEvent(protocol.EventMeta{
			Sequence:    protocol.Cursor(index + 1),
			OperationID: "operation",
			ThreadID:    "thread", TurnID: "turn", ItemID: "item",
		}, data)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Append(t.Context(), event); err != nil {
			t.Fatal(err)
		}
	}

	recovery, err := NewLifecycle(store).Recover(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	item, ok := recovery.PendingQueuedTurns["queue-1"]
	if !ok || item.Prompt != "after" || item.AddedSequence != 1 {
		t.Fatalf("pending queue = %+v", recovery.PendingQueuedTurns)
	}
}

func TestWorkspaceLifecycleRecoversOnlyBoundOperations(t *testing.T) {
	store, err := state.Open(t.Context(), state.Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.CloseAll(t.Context()) })
	rootA := filepath.Join(t.TempDir(), "workspace-a")
	rootB := filepath.Join(t.TempDir(), "workspace-b")
	repository := sessionstate.NewSQLiteRepository(store.SQLite())
	for _, value := range []struct {
		root      string
		sessionID string
	}{
		{rootA, "session-a"},
		{rootB, "session-b"},
	} {
		if err := repository.EnsureSeed(
			t.Context(),
			value.sessionID,
			value.root,
		); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, value := range []struct {
		operationID string
		sessionID   string
	}{
		{"operation-a", "session-a"},
		{"operation-b", "session-b"},
	} {
		if _, err := store.SQLite().DB().ExecContext(
			t.Context(),
			`INSERT INTO operations(
				id, session_id, kind, status, request_json, created_at, updated_at
			) VALUES (?, ?, ?, ?, '{}', ?, ?)`,
			value.operationID,
			value.sessionID,
			protocol.OperationStartTurn,
			threadstate.OperationAccepted,
			now,
			now,
		); err != nil {
			t.Fatal(err)
		}
	}

	recovery, err := NewWorkspaceLifecycle(store, rootA).Recover(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(recovery.PendingOperations) != 1 ||
		recovery.PendingOperations["operation-a"].SessionID != "session-a" {
		t.Fatalf("Workspace A pending operations = %+v", recovery.PendingOperations)
	}
	if _, exists := recovery.PendingOperations["operation-b"]; exists {
		t.Fatal("Workspace A recovered Workspace B operation")
	}
}
