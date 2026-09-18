package snapshot

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestCheckpointSummariesMatchIndividualReadsAndValidateIdentity(t *testing.T) {
	repository, database, _ := testRepository(t)
	now := time.Now().UTC()
	for _, id := range []string{"empty", "outside"} {
		if _, err := database.DB().ExecContext(t.Context(),
			`INSERT INTO sessions(id, workspace_id, status, created_at, updated_at)
			 VALUES (?, 'workspace-1', 'open', ?, ?)`, id, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := database.DB().ExecContext(t.Context(),
		`INSERT INTO threads(id, session_id, parent_thread_id, status, created_at, updated_at)
		 VALUES ('fork', 'session-1', 'thread-1', 'open', ?, ?)`, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := database.DB().ExecContext(t.Context(),
		`INSERT INTO turns(id, thread_id, ordinal, status, created_at, updated_at)
		 VALUES ('fork-turn', 'fork', 1, 'completed', ?, ?)`, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	for i, thread := range []protocol.ThreadID{"thread-1", "fork", "thread-1"} {
		turn := protocol.TurnID("turn-1")
		if thread == "fork" {
			turn = "fork-turn"
		}
		if _, err := repository.SaveCheckpoint(t.Context(), protocol.SessionCheckpoint{
			Version: protocol.CheckpointProtocolVersion, ID: fmt.Sprint("checkpoint-", i),
			SessionID: "session-1", ThreadID: thread, TurnID: turn, Cursor: protocol.Cursor(i + 1),
			Status: protocol.CheckpointCompleted, Summary: "checkpoint",
			ProfileRevision: artifactProfile().Revision, ChangedFiles: i + 1,
			CreatedAt: now.Add(time.Duration(3-i) * time.Second),
		}, []protocol.CompactedMessage{{Role: "user", Content: json.RawMessage(`["test"]`), Turn: 1}}, artifactProfile()); err != nil {
			t.Fatal(err)
		}
	}
	batch, err := repository.CheckpointSummaries(t.Context(), []string{"session-1", "empty", "missing"})
	if err != nil {
		t.Fatal(err)
	}
	latest, err := repository.ListCheckpoints(t.Context(), "session-1", 1)
	if err != nil {
		t.Fatal(err)
	}
	count, err := repository.CountCheckpoints(t.Context(), "session-1")
	if err != nil || batch["session-1"].Count != count || count != 3 ||
		batch["session-1"].ChangedFiles != latest[0].ChangedFiles || latest[0].ChangedFiles != 3 ||
		batch["empty"].Count != 0 || batch["missing"].Count != 0 {
		t.Fatalf("batch=%+v latest=%+v count=%d err=%v", batch, latest, count, err)
	}
	// Corrupt metadata must not become an unchecked sidebar statistic.
	if _, err := database.DB().ExecContext(t.Context(), `
		UPDATE snapshots SET metadata_json = json_set(metadata_json, '$.session_id', 'outside')
		WHERE id = 'checkpoint-2'`); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.CheckpointSummaries(t.Context(), []string{"session-1"}); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("cross-session checkpoint accepted: %v", err)
	}
	if _, err := repository.CheckpointSummaries(t.Context(), []string{"empty"}); err != nil {
		t.Fatalf("unrequested session was decoded: %v", err)
	}
}
