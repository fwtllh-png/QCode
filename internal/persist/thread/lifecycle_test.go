package thread

import (
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/persist/state"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

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
