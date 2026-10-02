package contextstate

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	sessionstate "github.com/fwtllh-png/QCode/internal/persist/session"
	"github.com/fwtllh-png/QCode/internal/persist/state"
	turnstate "github.com/fwtllh-png/QCode/internal/persist/state/turnstate"
	threadstate "github.com/fwtllh-png/QCode/internal/persist/thread"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestContextRebaseCommitIsAtomicIdempotentAndRecoverable(t *testing.T) {
	store, err := state.Open(t.Context(), state.Options{
		DataDir: filepath.Join(t.TempDir(), "state"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.CloseAll(context.Background()) })
	if err := ensureContextThread(
		t.Context(),
		store,
		"thread-1",
		"session-1",
		t.TempDir(),
	); err != nil {
		t.Fatal(err)
	}
	window, err := agentcontext.NewWindowLedger("window-2", 2)
	if err != nil {
		t.Fatal(err)
	}
	binding := agentcontext.WorkspaceBinding{
		WorkspaceIdentity: "workspace:test",
		JournalRevision:   1,
	}
	binding.Seal()
	message := provider.TextMessage(provider.RoleUser, "retained")
	message.Turn = 1
	truth := agentcontext.TruthCapsule{
		SchemaVersion: agentcontext.TruthSchemaVersion,
		Generation:    1, CompatibilityHash: "sha256:compat",
		ModelID: "model", ContextTokens: 8192,
		DownshiftPolicy: agentcontext.DownshiftRuntimeTruthOnly,
	}
	truth.Seal()
	snapshot := agentcontext.ContextSnapshot{
		Version: agentcontext.ContextSnapshotVersion,
		Epoch:   1, Revision: 2, Turn: 1,
		History:   []provider.Message{message},
		Workspace: binding, Window: window,
		Compaction: agentcontext.Compaction{
			State: &agentcontext.CompactionState{
				ID: "compact-1", ThreadID: "thread-1", TurnID: "turn-1",
				Phase: "completed", PlanDigest: "sha256:plan",
				Truth: truth, SourceWindowID: "window-1",
				TargetWindowID:      "window-2",
				SourceContextDigest: "sha256:source",
				FallbackReason:      "structural_only",
			},
		},
	}
	if err := snapshot.Seal(); err != nil {
		t.Fatal(err)
	}
	envelope := agentcontext.ContextRebaseEnvelope{
		CompactionID: "compact-1", ThreadID: "thread-1", TurnID: "turn-1",
		SourceWindowID: "window-1", TargetWindowID: "window-2",
		SourceContextDigest: "sha256:source",
		AuthorityDigest: func() string {
			digest, digestErr := truth.AuthorityDigest()
			if digestErr != nil {
				t.Fatal(digestErr)
			}
			return digest
		}(),
		Snapshot: snapshot,
	}
	if err := envelope.Seal(); err != nil {
		t.Fatal(err)
	}
	repository := NewRepository(store)
	if err := repository.CommitContextRebase(t.Context(), envelope); err != nil {
		t.Fatal(err)
	}
	if err := repository.CommitContextRebase(t.Context(), envelope); err != nil {
		t.Fatalf("idempotent replay failed: %v", err)
	}
	recovered, found, err := repository.LatestContextSnapshot(
		t.Context(),
		"thread-1",
	)
	if err != nil || !found || recovered.Digest != snapshot.Digest {
		t.Fatalf(
			"recovered=%+v found=%t err=%v",
			recovered,
			found,
			err,
		)
	}
	conflict := envelope
	conflict.Snapshot.Revision++
	conflict.BaseRevision = conflict.Snapshot.Revision - 1
	if err := conflict.Snapshot.Seal(); err != nil {
		t.Fatal(err)
	}
	if err := conflict.Seal(); err != nil {
		t.Fatal(err)
	}
	if err := repository.CommitContextRebase(
		t.Context(),
		conflict,
	); err == nil {
		t.Fatal("conflicting rebase replay succeeded")
	}

	next := snapshot
	next.Revision = 3
	next.Turn = 2
	next.Compaction.State = &agentcontext.CompactionState{
		ID: "compact-2", ThreadID: "thread-1", TurnID: "turn-2",
		Phase: "completed", PlanDigest: "sha256:plan-2",
		Truth: truth, SourceWindowID: "window-2",
		TargetWindowID:      "window-2",
		SourceContextDigest: snapshot.Digest,
		FallbackReason:      "structural_only",
	}
	if err := next.Seal(); err != nil {
		t.Fatal(err)
	}
	nextEnvelope := agentcontext.ContextRebaseEnvelope{
		CompactionID: "compact-2", ThreadID: "thread-1", TurnID: "turn-2",
		SourceWindowID: "window-2", TargetWindowID: "window-2",
		SourceContextDigest: snapshot.Digest,
		AuthorityDigest:     envelope.AuthorityDigest,
		Snapshot:            next,
	}
	if err := nextEnvelope.Seal(); err != nil {
		t.Fatal(err)
	}
	kernelState := turnkernel.NewState(
		protocol.TurnIntentAnswer,
		"act",
		1,
	)
	stateDigest, err := turnkernel.Digest(kernelState)
	if err != nil {
		t.Fatal(err)
	}
	fact := turnkernel.DomainFact{
		TurnID: "turn-2", Sequence: 1,
		Command: "effect_result_received",
		State:   kernelState, StateDigest: stateDigest,
	}
	badBatch := turnkernel.DomainFactBatch{
		TurnID: "turn-2", ExpectedNext: 2,
		Facts: []turnkernel.DomainFact{fact},
	}
	if err := repository.CommitContextRebaseWithFacts(
		t.Context(),
		nextEnvelope,
		badBatch,
	); err == nil {
		t.Fatal("invalid fact sequence committed context rebase")
	}
	unchanged, found, err := repository.LatestContextSnapshot(
		t.Context(),
		"thread-1",
	)
	if err != nil || !found || unchanged.Revision != snapshot.Revision {
		t.Fatalf(
			"failed transaction changed context: snapshot=%+v found=%t err=%v",
			unchanged,
			found,
			err,
		)
	}
	batch := turnkernel.DomainFactBatch{
		TurnID: "turn-2", ExpectedNext: 1,
		Facts: []turnkernel.DomainFact{fact},
	}
	if err := repository.CommitContextRebaseWithFacts(
		t.Context(),
		nextEnvelope,
		batch,
	); err != nil {
		t.Fatal(err)
	}
	facts, err := turnstate.NewSQLiteRepository(
		store.SQLite(),
	).LoadDomainFacts(t.Context(), "turn-2")
	if err != nil || len(facts) != 1 ||
		facts[0].StateDigest != stateDigest {
		t.Fatalf("facts=%+v err=%v", facts, err)
	}
	if err := repository.CommitContextRebaseWithFacts(
		t.Context(),
		nextEnvelope,
		batch,
	); err != nil {
		t.Fatalf("atomic replay failed: %v", err)
	}
	stale := nextEnvelope
	stale.CompactionID = "compact-stale"
	stale.Snapshot.Compaction.State.ID = stale.CompactionID
	if err := stale.Snapshot.Seal(); err != nil {
		t.Fatal(err)
	}
	if err := stale.Seal(); err != nil {
		t.Fatal(err)
	}
	if err := repository.CommitContextRebase(
		t.Context(),
		stale,
	); err == nil {
		t.Fatal("stale base revision committed a context rebase")
	}
	if err := repository.CommitContextRebase(
		t.Context(),
		envelope,
	); err == nil {
		t.Fatal("superseded context rebase replay succeeded")
	}
}

func TestCurrentContextCommitPersistsForkBaselineAndCanRollBack(t *testing.T) {
	store, err := state.Open(t.Context(), state.Options{
		DataDir: filepath.Join(t.TempDir(), "state"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.CloseAll(context.Background()) })
	workspace := t.TempDir()
	if err := ensureContextThread(
		t.Context(),
		store,
		"thread-parent",
		"session-1",
		workspace,
	); err != nil {
		t.Fatal(err)
	}
	window, err := agentcontext.NewWindowLedger("fork-window", 1)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := agentcontext.CaptureWorkspaceBinding(
		workspace,
		"workspace:test",
		1,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	message := provider.TextMessage(provider.RoleUser, "fork baseline")
	message.Turn = 1
	snapshot := agentcontext.ContextSnapshot{
		Version: agentcontext.ContextSnapshotVersion,
		Epoch:   1, Revision: 1, Turn: 1,
		History:   []provider.Message{message},
		Workspace: binding,
		Window:    window,
	}
	if err := snapshot.Seal(); err != nil {
		t.Fatal(err)
	}
	repository := NewRepository(store)
	commit := agentcontext.CurrentContextCommit{
		ID:             "checkpoint-fork-1",
		ThreadID:       "thread-child",
		TurnID:         "turn-1",
		SessionID:      "session-1",
		ParentThreadID: "thread-parent",
		Title:          "Child",
		SourceCursor:   7,
		Snapshot:       snapshot,
	}
	if err := repository.CommitCurrentContext(t.Context(), commit); err != nil {
		t.Fatal(err)
	}
	recovered, found, err := repository.LatestContextSnapshot(
		t.Context(),
		"thread-child",
	)
	if err != nil || !found || recovered.Digest != snapshot.Digest {
		t.Fatalf("recovered=%+v found=%t err=%v", recovered, found, err)
	}
	var (
		sessionID string
		parentID  string
		title     string
		cursor    uint64
	)
	if err := store.SQLite().DB().QueryRowContext(
		t.Context(),
		`SELECT session_id, parent_thread_id, title, source_cursor
		 FROM threads WHERE id = ?`,
		"thread-child",
	).Scan(&sessionID, &parentID, &title, &cursor); err != nil {
		t.Fatal(err)
	}
	if sessionID != "session-1" || parentID != "thread-parent" ||
		title != "Child" || cursor != 7 {
		t.Fatalf(
			"child=(session=%q parent=%q title=%q cursor=%d)",
			sessionID,
			parentID,
			title,
			cursor,
		)
	}
	if err := repository.DeleteCurrentContext(
		t.Context(),
		"thread-child",
		commit.ID,
		true,
	); err != nil {
		t.Fatal(err)
	}
	if _, found, err := repository.LatestContextSnapshot(
		t.Context(),
		"thread-child",
	); err != nil || found {
		t.Fatalf("deleted context found=%t err=%v", found, err)
	}
	var count int
	if err := store.SQLite().DB().QueryRowContext(
		t.Context(),
		`SELECT COUNT(*) FROM threads WHERE id = ?`,
		"thread-child",
	).Scan(&count); err != nil || count != 0 {
		t.Fatalf("child count=%d err=%v", count, err)
	}
}

func ensureContextThread(ctx context.Context, store *state.Store, threadID protocol.ThreadID, sessionID, workspaceRoot string) error {
	if err := sessionstate.NewSQLiteRepository(store.SQLite()).EnsureSeed(ctx, sessionID, workspaceRoot); err != nil {
		return err
	}
	_, err := threadstate.NewSQLiteRepository(store.SQLite()).Create(ctx, threadstate.Thread{ID: threadID, SessionID: sessionID, Title: "context"})
	return err
}
