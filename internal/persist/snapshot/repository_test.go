package snapshot

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/persist/state/cas"
	sqlitestate "github.com/fwtllh-png/QCode/internal/persist/state/sqlite"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestSnapshotRoundTripVerifiesSchemaAndHash(t *testing.T) {
	repository, _, _ := testRepository(t)
	saved, err := repository.Save(t.Context(), Snapshot{
		ID: "snapshot-1", ThreadID: "thread-1", TurnID: "turn-1",
		Cursor: 7, Kind: "runtime", Content: []byte(`{"state":"ok"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := repository.Recover(t.Context(), "thread-1", "runtime")
	if err != nil {
		t.Fatal(err)
	}
	if recovered.SchemaVersion != SchemaVersion ||
		recovered.ContentHash != saved.ContentHash ||
		string(recovered.Content) != `{"state":"ok"}` {
		t.Fatalf("recovered snapshot = %+v", recovered)
	}
}

func TestContextCheckpointRoundTripBindsEpochAndWorkspace(t *testing.T) {
	repository, _, _ := testRepository(t)
	profile := artifactProfile()
	history := []protocol.CompactedMessage{{
		Role: "user", Content: json.RawMessage(`["implement parser"]`), Turn: 1,
	}}
	window, err := agentcontext.NewWindowLedger("window-1", 1)
	if err != nil {
		t.Fatal(err)
	}
	binding := agentcontext.WorkspaceBinding{
		WorkspaceIdentity: "workspace:test",
		JournalRevision:   1,
		BoundPaths: []agentcontext.BoundPath{{
			Path: "parser.go", ContentDigest: "sha256:content",
		}},
	}
	binding.Seal()
	contextSnapshot := agentcontext.ContextSnapshot{
		Version: agentcontext.ContextSnapshotVersion,
		Epoch:   3, Revision: 7, Turn: 1,
		History: []provider.Message{
			func() provider.Message {
				message := provider.TextMessage(provider.RoleUser, "implement parser")
				message.Turn = 1
				return message
			}(),
		},
		Workspace: binding, Window: window,
	}
	if err := contextSnapshot.Seal(); err != nil {
		t.Fatal(err)
	}
	checkpoint := protocol.SessionCheckpoint{
		Version: protocol.CheckpointProtocolVersion,
		ID:      "checkpoint-context", SessionID: "session-1",
		ThreadID: "thread-1", TurnID: "turn-1", Cursor: 7,
		Status: protocol.CheckpointCompleted, Summary: "Context checkpoint",
		ProfileRevision: profile.Revision,
		StateEpoch:      contextSnapshot.Epoch,
		ContextDigest:   contextSnapshot.Digest,
		WorkspaceDigest: contextSnapshot.Workspace.SparseDigest,
		CreatedAt:       time.Now().UTC(),
	}
	if _, err := repository.SaveContextCheckpoint(
		t.Context(),
		checkpoint,
		history,
		contextSnapshot,
		profile,
	); err != nil {
		t.Fatal(err)
	}
	recovered, gotContext, gotProfile, err :=
		repository.GetContextCheckpoint(t.Context(), checkpoint.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.ContextDigest != contextSnapshot.Digest ||
		gotContext.Digest != contextSnapshot.Digest ||
		gotProfile.Revision != profile.Revision {
		t.Fatalf(
			"checkpoint=%+v context=%+v profile=%+v",
			recovered,
			gotContext,
			gotProfile,
		)
	}
}

func TestSessionCheckpointAndPlanArtifactsAreImmutableAndVerified(t *testing.T) {
	repository, _, _ := testRepository(t)
	profile := artifactProfile()
	history := []protocol.CompactedMessage{{
		Role: "user", Content: json.RawMessage(`["implement parser"]`), Turn: 1,
	}}
	first, err := repository.SaveCheckpoint(t.Context(), protocol.SessionCheckpoint{
		Version: protocol.CheckpointProtocolVersion,
		ID:      "checkpoint-1", SessionID: "session-1",
		ThreadID: "thread-1", TurnID: "turn-1", Cursor: 7,
		Status: protocol.CheckpointCompleted, Summary: "Implemented parser",
		ProfileRevision: profile.Revision, CreatedAt: time.Now().UTC(),
	}, history, profile)
	if err != nil {
		t.Fatal(err)
	}
	second, err := repository.SaveCheckpoint(t.Context(), protocol.SessionCheckpoint{
		Version: protocol.CheckpointProtocolVersion,
		ID:      "checkpoint-2", SessionID: "session-1",
		ThreadID: "thread-1", TurnID: "turn-1", Cursor: 8,
		Status: protocol.CheckpointInterrupted, Summary: "Interrupted safely",
		ProfileRevision: profile.Revision, CreatedAt: time.Now().UTC(),
	}, history, profile)
	if err != nil {
		t.Fatal(err)
	}
	if second.ParentCheckpointID != first.ID {
		t.Fatalf("checkpoint parent = %q", second.ParentCheckpointID)
	}
	list, err := repository.ListCheckpoints(t.Context(), "session-1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != second.ID {
		t.Fatalf("checkpoint list = %+v", list)
	}
	count, err := repository.CountCheckpoints(t.Context(), "session-1")
	if err != nil || count != 2 {
		t.Fatalf("checkpoint count = %d, error = %v", count, err)
	}
	recovered, gotHistory, gotProfile, err := repository.GetCheckpoint(
		t.Context(), first.ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.ID != first.ID || len(gotHistory) != 1 ||
		gotProfile.Revision != profile.Revision {
		t.Fatalf("recovered checkpoint = %+v, history=%+v", recovered, gotHistory)
	}

	document := json.RawMessage(`{"version":1,"revision":1,"steps":[{"id":"implement","title":"Update parser","status":"pending"}]}`)
	executionDigest := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	plan, err := repository.SavePlan(t.Context(), protocol.SessionPlanArtifact{
		Version: protocol.CheckpointProtocolVersion,
		ID:      "plan-1", SessionID: "session-1",
		ThreadID: "thread-1", TurnID: "turn-1", Cursor: 6,
		Status: protocol.PlanArtifactReady, Body: string(document),
		ProfileRevision:        profile.Revision,
		ExecutionProfileDigest: executionDigest,
		CanImplement:           true, CanAutopilot: true,
		CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	latest, found, err := repository.LatestPlan(
		t.Context(), "session-1", "thread-1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !found || latest.ID != plan.ID || latest.Body != plan.Body ||
		latest.ExecutionProfileDigest != executionDigest ||
		!json.Valid([]byte(latest.Body)) {
		t.Fatalf("latest plan = %+v", latest)
	}
}

func TestCheckpointContentCompressesRepeatedHistoryAndReadsLegacyJSON(
	t *testing.T,
) {
	history := make([]protocol.CompactedMessage, 0, 960)
	for turn := uint64(1); turn <= 480; turn++ {
		history = append(history,
			protocol.CompactedMessage{
				Role: "user", Content: json.RawMessage(`["say hello"]`),
				Turn: turn,
			},
			protocol.CompactedMessage{
				Role: "assistant", Content: json.RawMessage(`["hello"]`),
				Turn: turn,
			},
		)
	}
	content := checkpointContent{
		History: history,
		Profile: artifactProfile(),
	}
	compressed, err := encodeCheckpointContent(content)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}
	if len(compressed)*5 >= len(legacy) {
		t.Fatalf(
			"compressed checkpoint = %d bytes, legacy = %d bytes",
			len(compressed),
			len(legacy),
		)
	}
	t.Logf(
		"checkpoint storage: compressed=%d legacy=%d",
		len(compressed),
		len(legacy),
	)
	for name, raw := range map[string][]byte{
		"compressed": compressed,
		"legacy":     legacy,
	} {
		t.Run(name, func(t *testing.T) {
			var restored checkpointContent
			if err := decodeCheckpointContent(raw, &restored); err != nil {
				t.Fatal(err)
			}
			if len(restored.History) != len(history) ||
				restored.History[0].Turn != 1 ||
				restored.History[len(restored.History)-1].Turn != 480 ||
				restored.Profile.Revision != content.Profile.Revision {
				t.Fatalf("restored checkpoint = %+v", restored)
			}
		})
	}
}

func artifactProfile() protocol.SessionProfile {
	return protocol.SessionProfile{
		Version:             protocol.SessionProfileVersion,
		Revision:            2,
		Mode:                "act",
		Provider:            "fixture",
		Model:               "fixture-model",
		ApprovalPosture:     "suggest",
		ExecutionTarget:     "local",
		MaxSteps:            8,
		PromptCacheRevision: 1,
	}
}

func TestSnapshotRejectsCorruptedContent(t *testing.T) {
	repository, _, content := testRepository(t)
	saved, err := repository.Save(t.Context(), Snapshot{
		ID: "snapshot-corrupt", ThreadID: "thread-1", Cursor: 1,
		Kind: "runtime", Content: []byte("trusted"),
	})
	if err != nil {
		t.Fatal(err)
	}
	objectPath := filepath.Join(
		content.Root(), "objects", saved.ContentHash[:2], saved.ContentHash[2:],
	)
	if err := os.WriteFile(objectPath, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = repository.Get(t.Context(), saved.ID)
	var integrity *IntegrityError
	if !errors.As(err, &integrity) || !errors.Is(err, ErrIntegrity) {
		t.Fatalf("corrupt snapshot error = %v, want IntegrityError", err)
	}
}

func TestSnapshotRejectsUnsupportedSchema(t *testing.T) {
	repository, database, _ := testRepository(t)
	saved, err := repository.Save(t.Context(), Snapshot{
		ID: "snapshot-schema", ThreadID: "thread-1", Cursor: 1,
		Kind: "runtime", Content: []byte("trusted"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.DB().ExecContext(t.Context(), `
		UPDATE snapshots SET schema_version = 99 WHERE id = ?`, saved.ID,
	); err != nil {
		t.Fatal(err)
	}
	_, err = repository.Get(t.Context(), saved.ID)
	var schemaErr *SchemaError
	if !errors.As(err, &schemaErr) || !errors.Is(err, ErrUnsupportedSchema) {
		t.Fatalf("schema error = %v, want SchemaError", err)
	}
}

func TestRepositoryFailsClosedOnMalformedStoredMetadata(t *testing.T) {
	repository, database, _ := testRepository(t)
	saved, err := repository.Save(t.Context(), Snapshot{
		ID: "malformed", ThreadID: "thread-1", TurnID: "turn-1",
		Cursor: 1, Kind: "runtime", Content: []byte("trusted"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.DB().ExecContext(
		t.Context(),
		"PRAGMA ignore_check_constraints = ON",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := database.DB().ExecContext(
		t.Context(),
		"UPDATE snapshots SET metadata_json = ? WHERE id = ?",
		`{"broken":`,
		saved.ID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := database.DB().ExecContext(
		t.Context(),
		"PRAGMA ignore_check_constraints = OFF",
	); err != nil {
		t.Fatal(err)
	}
	_, err = repository.Get(t.Context(), saved.ID)
	var integrityErr *IntegrityError
	if !errors.As(err, &integrityErr) {
		t.Fatalf("malformed metadata error = %v, want IntegrityError", err)
	}
}

func TestRepositoryContractDuplicateCancelAndMissingSchema(t *testing.T) {
	repository, database, content := testRepository(t)
	value := Snapshot{
		ID: "contract", ThreadID: "thread-1", TurnID: "turn-1",
		Cursor: 1, Kind: "runtime", Content: []byte("trusted"),
	}
	if _, err := repository.Save(t.Context(), value); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Save(t.Context(), value); err == nil {
		t.Fatal("duplicate snapshot identity succeeded")
	}
	var storePath string
	if err := database.DB().QueryRowContext(
		t.Context(),
		"SELECT file FROM pragma_database_list WHERE name = 'main'",
	).Scan(&storePath); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlitestate.Open(t.Context(), storePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	repository = NewRepository(reopened.DB(), content)
	if persisted, err := repository.Get(t.Context(), value.ID); err != nil ||
		persisted.ID != value.ID {
		t.Fatalf("snapshot after restart = %+v, error = %v", persisted, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := repository.Get(ctx, value.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Get error = %v", err)
	}
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "missing.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := NewRepository(db, content).Get(t.Context(), value.ID); err == nil {
		t.Fatal("repository without schema succeeded")
	}
}

func testRepository(t *testing.T) (*Repository, *sqlitestate.Store, *cas.Store) {
	t.Helper()
	root := t.TempDir()
	database, err := sqlitestate.Open(t.Context(), filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	content, err := cas.Open(filepath.Join(root, "cas"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = content.Close(t.Context())
		_ = database.Close()
	})
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, statement := range []string{
		`INSERT INTO workspaces(id, root_path, created_at, updated_at)
		 VALUES ('workspace-1', '/workspace', ?, ?)`,
		`INSERT INTO sessions(id, workspace_id, status, created_at, updated_at)
		 VALUES ('session-1', 'workspace-1', 'open', ?, ?)`,
		`INSERT INTO threads(id, session_id, status, created_at, updated_at)
		 VALUES ('thread-1', 'session-1', 'open', ?, ?)`,
		`INSERT INTO turns(id, thread_id, ordinal, status, created_at, updated_at)
		 VALUES ('turn-1', 'thread-1', 0, 'active', ?, ?)`,
	} {
		if _, err := database.DB().ExecContext(t.Context(), statement, now, now); err != nil {
			t.Fatal(err)
		}
	}
	return NewRepository(database.DB(), content), database, content
}
