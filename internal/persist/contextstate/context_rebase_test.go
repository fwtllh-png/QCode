package contextstate

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	sessionstate "github.com/fwtllh-png/QCode/internal/persist/session"
	"github.com/fwtllh-png/QCode/internal/persist/state"
	turnstate "github.com/fwtllh-png/QCode/internal/persist/state/turnstate"
	threadstate "github.com/fwtllh-png/QCode/internal/persist/thread"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestNarrativeCurrentContextCommitAfterTerminal(t *testing.T) {
	for _, encoding := range []string{"manifest", "session_delta"} {
		t.Run(encoding, func(t *testing.T) {
			store, err := state.Open(t.Context(), state.Options{DataDir: filepath.Join(t.TempDir(), "state")})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.CloseAll(context.Background()) })
			workspace := t.TempDir()
			if err := ensureContextThread(t.Context(), store, "thread", "session", workspace); err != nil {
				t.Fatal(err)
			}
			window, err := agentcontext.NewWindowLedger("window", 1)
			if err != nil {
				t.Fatal(err)
			}
			binding, err := agentcontext.CaptureWorkspaceBinding(workspace, "workspace:test", 1, nil)
			if err != nil {
				t.Fatal(err)
			}
			snapshot := agentcontext.ContextSnapshot{Version: agentcontext.ContextSnapshotVersion, Epoch: 1, Revision: 1, Turn: 1, History: []provider.Message{provider.TextMessage(provider.RoleUser, "source")}, Workspace: binding, Window: window}
			if err := snapshot.Seal(); err != nil {
				t.Fatal(err)
			}
			repository := NewRepository(store)
			maintenance := func(id string, base uint64) agentcontext.CurrentContextCommit {
				candidate := snapshot
				candidate.Revision = base + 1
				candidate.Compaction.NarrativeAttempts = []string{id}
				if err := candidate.Seal(); err != nil {
					t.Fatal(err)
				}
				return agentcontext.CurrentContextCommit{ID: id, ThreadID: "thread", TurnID: "turn", BaseRevision: &base, Snapshot: candidate}
			}
			reject := func(commit agentcontext.CurrentContextCommit) {
				t.Helper()
				if err := repository.CommitCurrentContext(t.Context(), commit); err == nil || !strings.Contains(err.Error(), "revision conflict") {
					t.Fatalf("commit %s: expected base conflict, got %v", commit.ID, err)
				}
			}
			// A missing root does not authorize skipping the first revision.
			reject(maintenance("missing-base", 1))
			terminal := func(threadID protocol.ThreadID, turnID string, revision uint64) {
				t.Helper()
				terminalSnapshot := snapshot
				terminalSnapshot.Revision = revision
				if err := terminalSnapshot.Seal(); err != nil {
					t.Fatal(err)
				}
				raw := narrativeTerminalContext(t, store, threadID, turnID, terminalSnapshot, encoding)
				commitNarrativeTerminal(t, store, threadID, turnID, raw)
			}
			terminal("thread", "turn-1", 1)
			if err := repository.CommitCurrentContext(t.Context(), maintenance("first", 1)); err != nil {
				t.Fatalf("first maintenance after terminal: %v", err)
			}
			stale := maintenance("stale", 2)
			terminal("thread", "turn-2", 3)
			terminal("other-thread", "other-turn", 9)
			reject(stale)
			reject(maintenance("skipped", 4))
			next := maintenance("after-terminal", 3)
			for range 2 {
				if err := repository.CommitCurrentContext(t.Context(), next); err != nil {
					t.Fatalf("maintenance after interleaved terminal: %v", err)
				}
			}
			reject(maintenance("competing", 3))
			restored, found, err := repository.LatestContextSnapshot(t.Context(), "thread")
			if err != nil || !found || restored.Revision != 4 || len(restored.Compaction.NarrativeAttempts) != 1 || restored.Compaction.NarrativeAttempts[0] != next.ID {
				t.Fatalf("restore after interleaved commits: revision=%d found=%v err=%v", restored.Revision, found, err)
			}
			// Valid JSON with an invalid context must fail closed, not become
			// revision zero and permit overwriting the damaged terminal.
			commitNarrativeTerminal(t, store, "thread", "corrupt", json.RawMessage(`{"version":1,"digest":"invalid"}`))
			if err := repository.CommitCurrentContext(t.Context(), maintenance("after-corrupt", 4)); err == nil || !strings.Contains(err.Error(), "digest") {
				t.Fatalf("corrupt terminal context: %v", err)
			}
			validManifest := narrativeTerminalContext(t, store, "thread", "corrupt-manifest", snapshot, "manifest")
			var corruptManifest agentcontext.ContextEnvelope
			if err := json.Unmarshal(validManifest, &corruptManifest); err != nil {
				t.Fatal(err)
			}
			corruptManifest.Digest = "invalid"
			invalidManifest, err := json.Marshal(corruptManifest)
			if err != nil {
				t.Fatal(err)
			}
			commitNarrativeTerminal(t, store, "thread", "corrupt-manifest", invalidManifest)
			if err := repository.CommitCurrentContext(t.Context(), maintenance("after-corrupt-manifest", 4)); err == nil || !strings.Contains(err.Error(), "digest") {
				t.Fatalf("corrupt terminal manifest: %v", err)
			}
			wrongThread := narrativeTerminalContext(t, store, "other-thread", "wrong-thread", snapshot, "manifest")
			commitNarrativeTerminal(t, store, "thread", "wrong-thread", wrongThread)
			if err := repository.CommitCurrentContext(t.Context(), maintenance("after-wrong-thread", 4)); err == nil || !strings.Contains(err.Error(), "thread is inconsistent") {
				t.Fatalf("cross-thread manifest: %v", err)
			}
		})
	}
}

func narrativeTerminalContext(t *testing.T, store *state.Store, threadID protocol.ThreadID, turnID string, snapshot agentcontext.ContextSnapshot, encoding string) json.RawMessage {
	t.Helper()
	accounting := agentcontext.AccountingDelta{TurnID: turnID}
	accounting.Seal()
	if encoding == "manifest" {
		manifest, err := agentcontext.BuildContextManifest(t.Context(), store.Content(), threadID, protocol.TurnID(turnID), snapshot, nil, agentcontext.ManifestLimits{})
		if err != nil {
			t.Fatal(err)
		}
		raw, err := agentcontext.EncodeContextEnvelope(manifest, accounting)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	delta, err := agentcontext.NewSessionDelta(snapshot, accounting)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(delta)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func commitNarrativeTerminal(t *testing.T, store *state.Store, threadID protocol.ThreadID, turnID string, raw json.RawMessage) {
	t.Helper()
	reducer := turnkernel.Reducer{}
	kernelState := turnkernel.NewState(protocol.TurnIntentAnswer, "act", 1)
	for _, command := range []turnkernel.Command{turnkernel.StartTurn{}, turnkernel.PreparationFinished{}, turnkernel.ModelTextReceived{Text: "done"}, turnkernel.ReleaseProvisionalOutput{}, turnkernel.TerminalRequested{}, turnkernel.FinishTerminal{}} {
		transition, err := reducer.Apply(kernelState, command)
		if err != nil {
			t.Fatal(err)
		}
		kernelState = transition.State
	}
	digest, err := turnkernel.Digest(kernelState)
	if err != nil {
		t.Fatal(err)
	}
	measurement, err := turnkernel.NewTerminalMeasurementSnapshot(time.Unix(1, 0), nil, kernelState.Usage, true)
	if err != nil {
		t.Fatal(err)
	}
	envelope := turnkernel.TerminalEnvelope{
		TurnID: turnID, EffectID: "terminal:" + turnID, FrozenState: kernelState,
		DomainFacts:  []turnkernel.DomainFact{{TurnID: turnID, Sequence: 1, Command: "finish_terminal", State: kernelState, StateDigest: digest}},
		SessionDelta: raw, Measurement: measurement,
		Receipt:     &protocol.ExecutionReceiptData{Goal: "answer", Intent: protocol.TurnIntentAnswer, Outcome: protocol.TurnOutcomeAnswered, MeasurementDigest: measurement.Digest, UsageDigest: measurement.UsageDigest},
		FinalOutput: kernelState.FinalOutput, TerminalEvent: turnkernel.Event{Kind: turnkernel.EventTerminalCommitted, Terminal: kernelState.Terminal},
		OperationCommit: turnkernel.OperationCommitFact{OperationID: protocol.OperationID("operation:" + turnID), Status: "committed"},
		Outbox: []turnkernel.ProjectionOutboxEntry{{
			ID: "terminal", EventID: protocol.EventID("event:" + turnID),
			OperationID: protocol.OperationID("operation:" + turnID),
			ThreadID:    threadID, TurnID: protocol.TurnID(turnID),
			ItemID: protocol.ItemID("item:" + turnID), Kind: "turn.completed", Payload: []byte(`{}`),
		}},
	}
	if _, err := turnstate.NewSQLiteRepository(store.SQLite()).CommitTerminal(t.Context(), envelope); err != nil {
		t.Fatal(err)
	}
}

func TestNarrativeCurrentContextCommitChecksBaseAndRetainsOwner(t *testing.T) {
	store, err := state.Open(t.Context(), state.Options{DataDir: filepath.Join(t.TempDir(), "state")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.CloseAll(context.Background()) })
	workspace := t.TempDir()
	if err := ensureContextThread(t.Context(), store, "thread", "session", workspace); err != nil {
		t.Fatal(err)
	}
	window, err := agentcontext.NewWindowLedger("window", 1)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := agentcontext.CaptureWorkspaceBinding(workspace, "workspace:test", 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := agentcontext.ContextSnapshot{Version: agentcontext.ContextSnapshotVersion, Epoch: 1, Revision: 1, Turn: 1, History: []provider.Message{provider.TextMessage(provider.RoleUser, "source")}, Workspace: binding, Window: window}
	if err := snapshot.Seal(); err != nil {
		t.Fatal(err)
	}
	repository := NewRepository(store)
	if err := repository.CommitCurrentContext(t.Context(), agentcontext.CurrentContextCommit{ID: "base", ThreadID: "thread", TurnID: "turn", Snapshot: snapshot}); err != nil {
		t.Fatal(err)
	}
	wrongBase := uint64(2)
	snapshot.Revision = 3
	_ = snapshot.Seal()
	if err := repository.CommitCurrentContext(t.Context(), agentcontext.CurrentContextCommit{ID: "stale", ThreadID: "thread", TurnID: "turn", BaseRevision: &wrongBase, Snapshot: snapshot}); err == nil {
		t.Fatal("maintenance skipped current revision")
	}
	base := uint64(1)
	snapshot.Revision = 2
	snapshot.Compaction.NarrativeAttempts = []string{"source-digest"}
	_ = snapshot.Seal()
	commit := agentcontext.CurrentContextCommit{ID: "narrative", ThreadID: "thread", TurnID: "turn", BaseRevision: &base, Snapshot: snapshot}
	for range 2 {
		if err := repository.CommitCurrentContext(t.Context(), commit); err != nil {
			t.Fatal(err)
		}
	}
	restored, found, err := repository.LatestContextSnapshot(t.Context(), "thread")
	if err != nil || !found {
		t.Fatalf("restore: %v, %v", found, err)
	}
	if restored.Revision != 2 || len(restored.Compaction.NarrativeAttempts) != 1 || restored.Compaction.NarrativeAttempts[0] != "source-digest" {
		t.Fatal("maintenance owner was not restored")
	}
}

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

	// A new business terminal may advance beyond the maintenance root before
	// the next explicit compaction; its base must use the terminal revision.
	terminalSnapshot, found, err := repository.LatestContextSnapshot(t.Context(), "thread-1")
	if err != nil || !found {
		t.Fatalf("load current context: found=%v err=%v", found, err)
	}
	terminalSnapshot.Revision = 4
	if err := terminalSnapshot.Seal(); err != nil {
		t.Fatal(err)
	}
	raw := narrativeTerminalContext(t, store, "thread-1", "turn-terminal", terminalSnapshot, "manifest")
	commitNarrativeTerminal(t, store, "thread-1", "turn-terminal", raw)
	for _, base := range []uint64{3, 4} {
		candidate := terminalSnapshot
		candidate.Revision = base + 1
		compactionState := *candidate.Compaction.State
		compactionState.ID = "compact-after-terminal"
		compactionState.TurnID = "turn-after-terminal"
		compactionState.SourceContextDigest = terminalSnapshot.Digest
		candidate.Compaction.State = &compactionState
		if err := candidate.Seal(); err != nil {
			t.Fatal(err)
		}
		afterTerminal := agentcontext.ContextRebaseEnvelope{
			CompactionID: compactionState.ID, ThreadID: "thread-1", TurnID: compactionState.TurnID,
			BaseRevision: base, SourceWindowID: compactionState.SourceWindowID,
			TargetWindowID: compactionState.TargetWindowID, SourceContextDigest: terminalSnapshot.Digest,
			AuthorityDigest: envelope.AuthorityDigest, Snapshot: candidate,
		}
		if err := afterTerminal.Seal(); err != nil {
			t.Fatal(err)
		}
		err := repository.CommitContextRebase(t.Context(), afterTerminal)
		if base == 3 {
			if err == nil || !strings.Contains(err.Error(), "revision conflict") {
				t.Fatalf("stale rebase after terminal: %v", err)
			}
		} else if err != nil {
			t.Fatalf("rebase after interleaved terminal: %v", err)
		}
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
