package thread

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	sessionstate "github.com/fwtllh-png/QCode/internal/persist/session"
	"github.com/fwtllh-png/QCode/internal/persist/state"
	sqlitestate "github.com/fwtllh-png/QCode/internal/persist/state/sqlite"
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
	var status TurnStatus
	if err := store.SQLite().DB().QueryRowContext(
		t.Context(),
		`SELECT status FROM turns WHERE id = 'turn-1'`,
	).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != TurnFailed {
		t.Fatalf("rejected turn status = %s, want %s", status, TurnFailed)
	}
	start("operation-2", "turn-2", "item-2")
}

func TestDeclaredIncompleteTerminalProjectsBlockedStatus(t *testing.T) {
	event, err := protocol.NewEvent(protocol.EventMeta{
		Sequence:    1,
		OperationID: "operation",
		ThreadID:    "thread",
		TurnID:      "turn",
		ItemID:      "item",
	}, &protocol.TurnFailedData{
		Code:    protocol.CodeConflict,
		Message: "turn declared incomplete with resumable pending actions",
		Convergence: &protocol.TurnConvergence{
			Cause:          "declared_incomplete",
			Summary:        "A prerequisite is unavailable.",
			PendingActions: []string{"Install it and continue."},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := terminalStatus(event); got != TurnBlocked {
		t.Fatalf("terminal status = %s, want %s", got, TurnBlocked)
	}
}

func TestRecoverableFaultProjectsBlockedStatus(t *testing.T) {
	event, err := protocol.NewEvent(protocol.EventMeta{
		Sequence:    1,
		OperationID: "operation",
		ThreadID:    "thread",
		TurnID:      "turn",
		ItemID:      "item",
	}, &protocol.TurnFailedData{
		Code:    protocol.CodeUnavailable,
		Message: "workspace journal has a retained draft",
		Fault: &protocol.FaultMetadata{
			Disposition: protocol.FaultResumeTurn,
			SideEffects: protocol.SideEffectDraft,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := terminalStatus(event); got != TurnBlocked {
		t.Fatalf("terminal status = %s, want %s", got, TurnBlocked)
	}
}

func TestBlockedTerminalReplayUpgradesLegacyFailedProjection(t *testing.T) {
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
		`INSERT INTO turns(
			id, thread_id, ordinal, status, created_at, updated_at, completed_at
		 ) VALUES ('turn', 'thread', 1, 'failed', ?, ?, ?)`,
	} {
		arguments := []any{now, now}
		if strings.Contains(statement, "INSERT INTO turns") {
			arguments = append(arguments, now)
		}
		if _, err := store.SQLite().DB().ExecContext(
			t.Context(),
			statement,
			arguments...,
		); err != nil {
			t.Fatal(err)
		}
	}
	event, err := protocol.NewEvent(protocol.EventMeta{
		Sequence:    1,
		OperationID: "operation",
		ThreadID:    "thread",
		TurnID:      "turn",
		ItemID:      "item",
	}, &protocol.TurnFailedData{
		Code:    protocol.CodeConflict,
		Message: "blocked",
		Convergence: &protocol.TurnConvergence{
			Cause:          "declared_incomplete",
			Summary:        "blocked",
			PendingActions: []string{"continue"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := NewLifecycle(store).Project(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	var status TurnStatus
	if err := store.SQLite().DB().QueryRowContext(
		t.Context(),
		`SELECT status FROM turns WHERE id = 'turn'`,
	).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != TurnBlocked {
		t.Fatalf("turn status = %s, want blocked", status)
	}
}

func TestCreateSeedReusesWorkspaceRoot(t *testing.T) {
	store, err := sqlitestate.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	repository := NewRepository(store.DB())
	for _, value := range []struct {
		workspace string
		session   string
		thread    string
	}{
		{"workspace-one", "session-one", "thread-one"},
		{"workspace-two", "session-two", "thread-two"},
	} {
		if _, err := repository.CreateSeed(
			t.Context(),
			sessionstate.Workspace{ID: value.workspace, RootPath: "/workspace"},
			sessionstate.Session{ID: value.session, WorkspaceID: value.workspace},
			Thread{ID: protocol.ThreadID(value.thread), SessionID: value.session},
		); err != nil {
			t.Fatal(err)
		}
	}
	var workspaceCount int
	if err := store.DB().QueryRowContext(
		t.Context(), `SELECT COUNT(*) FROM workspaces`,
	).Scan(&workspaceCount); err != nil {
		t.Fatal(err)
	}
	if workspaceCount != 1 {
		t.Fatalf("workspace count = %d, want 1", workspaceCount)
	}
	var distinctWorkspaceCount int
	if err := store.DB().QueryRowContext(t.Context(), `
		SELECT COUNT(DISTINCT workspace_id) FROM sessions`,
	).Scan(&distinctWorkspaceCount); err != nil {
		t.Fatal(err)
	}
	if distinctWorkspaceCount != 1 {
		t.Fatalf(
			"session workspace count = %d, want 1",
			distinctWorkspaceCount,
		)
	}
	if err := repository.Rename(t.Context(), "thread-two", "修复登录问题"); err != nil {
		t.Fatal(err)
	}
	renamed, err := repository.Get(t.Context(), "thread-two")
	if err != nil {
		t.Fatal(err)
	}
	if renamed.Title != "修复登录问题" {
		t.Fatalf("renamed title = %q", renamed.Title)
	}
}

func TestListIsBoundedAndWorkspaceScoped(t *testing.T) {
	store, err := sqlitestate.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, statement := range []string{
		`INSERT INTO workspaces(id, root_path, created_at, updated_at)
		 VALUES ('workspace-a', '/workspace/a', ?, ?)`,
		`INSERT INTO workspaces(id, root_path, created_at, updated_at)
		 VALUES ('workspace-b', '/workspace/b', ?, ?)`,
		`INSERT INTO sessions(id, workspace_id, status, created_at, updated_at)
		 VALUES ('session-a', 'workspace-a', 'open', ?, ?)`,
		`INSERT INTO sessions(id, workspace_id, status, created_at, updated_at)
		 VALUES ('session-b', 'workspace-b', 'open', ?, ?)`,
		`INSERT INTO threads(id, session_id, title, status, created_at, updated_at)
		 VALUES ('thread-a', 'session-a', 'a', 'open', ?, ?)`,
		`INSERT INTO threads(id, session_id, title, status, created_at, updated_at)
		 VALUES ('thread-b', 'session-b', 'b', 'open', ?, ?)`,
	} {
		if _, err := store.DB().ExecContext(t.Context(), statement, now, now); err != nil {
			t.Fatal(err)
		}
	}
	repository := NewRepository(store.DB())
	values, err := repository.List(t.Context(), Filter{WorkspaceRoot: "/workspace/a"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 || values[0].ID != "thread-a" {
		t.Fatalf("workspace-scoped threads = %+v", values)
	}
	if _, err := repository.GetInWorkspace(
		t.Context(), "thread-b", "/workspace/a",
	); err != ErrNotFound {
		t.Fatalf("foreign workspace get error = %v", err)
	}
	if _, err := repository.List(t.Context(), Filter{}, 1001); err == nil {
		t.Fatal("oversized thread list succeeded")
	}
}

func TestHistoryCursorBoundsNewestTurns(t *testing.T) {
	store, err := sqlitestate.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC().Format(time.RFC3339Nano)
	statements := []string{
		`INSERT INTO workspaces(id, root_path, created_at, updated_at)
		 VALUES ('workspace', '/workspace', ?, ?)`,
		`INSERT INTO sessions(id, workspace_id, status, created_at, updated_at)
		 VALUES ('session', 'workspace', 'open', ?, ?)`,
		`INSERT INTO threads(id, session_id, title, status, created_at, updated_at)
		 VALUES ('thread', 'session', 'chat', 'open', ?, ?)`,
		`INSERT INTO turns(id, thread_id, ordinal, status, created_at, updated_at)
		 VALUES ('turn-1', 'thread', 1, 'completed', ?, ?)`,
		`INSERT INTO turns(id, thread_id, ordinal, status, created_at, updated_at)
		 VALUES ('turn-2', 'thread', 2, 'completed', ?, ?)`,
	}
	for _, statement := range statements {
		if _, err := store.DB().ExecContext(t.Context(), statement, now, now); err != nil {
			t.Fatal(err)
		}
	}
	for _, value := range []struct {
		sequence int
		eventID  string
		turnID   string
	}{
		{5, "event-5", "turn-1"},
		{10, "event-10", "turn-2"},
	} {
		if _, err := store.DB().ExecContext(t.Context(), `
			INSERT INTO event_index(
				sequence, event_id, thread_id, turn_id, kind,
				log_offset, log_length, sha256, created_at
			) VALUES (?, ?, 'thread', ?, 'turn.started', 0, 1, ?, ?)`,
			value.sequence, value.eventID, value.turnID,
			"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			now,
		); err != nil {
			t.Fatal(err)
		}
	}
	repository := NewRepository(store.DB())
	cursor, err := repository.HistoryCursor(t.Context(), "thread", 1)
	if err != nil {
		t.Fatal(err)
	}
	if cursor != 9 {
		t.Fatalf("HistoryCursor() = %d, want 9", cursor)
	}
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
			OperationAccepted,
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
