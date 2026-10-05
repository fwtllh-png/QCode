package app

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/persist/snapshot"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func artifactContextSnapshot(t *testing.T, text string) agentcontext.ContextSnapshot {
	t.Helper()
	window, err := agentcontext.NewWindowLedger("checkpoint-window", 1)
	if err != nil {
		t.Fatal(err)
	}
	binding := agentcontext.WorkspaceBinding{WorkspaceIdentity: "workspace:test"}
	binding.Seal()
	message := provider.TextMessage(provider.RoleUser, text)
	message.Turn = 1
	value := agentcontext.ContextSnapshot{
		Version: agentcontext.ContextSnapshotVersion, Epoch: 1, Revision: 1, Turn: 1,
		History: []provider.Message{message}, Workspace: binding, Window: window,
	}
	if err := value.Seal(); err != nil {
		t.Fatal(err)
	}
	return value
}

type checkpointReplayStore struct {
	*MemoryEventStore
	turnReads int
	broken    bool
}

func (s *checkpointReplayStore) ReplayTurn(ctx context.Context, turn protocol.TurnID) ([]protocol.Event, error) {
	s.turnReads++
	if s.broken {
		return nil, errors.New("injected Turn replay error")
	}
	return s.MemoryEventStore.ReplayTurn(ctx, turn)
}

type recordingCheckpointStore struct {
	memoryArtifactStore
	saved []snapshot.CheckpointState
}

func (s *recordingCheckpointStore) SaveCheckpoint(_ context.Context, checkpoint protocol.SessionCheckpoint,
	history []protocol.CompactedMessage, profile protocol.SessionProfile,
) (protocol.SessionCheckpoint, error) {
	s.saved = append(s.saved, snapshot.CheckpointState{Checkpoint: checkpoint, History: history, Profile: profile})
	return checkpoint, nil
}

func (s *recordingCheckpointStore) SaveContextCheckpoint(_ context.Context, checkpoint protocol.SessionCheckpoint,
	value agentcontext.ContextSnapshot, profile protocol.SessionProfile,
) (protocol.SessionCheckpoint, error) {
	if err := value.Validate(); err != nil {
		return protocol.SessionCheckpoint{}, err
	}
	s.saved = append(s.saved, snapshot.CheckpointState{Checkpoint: checkpoint, Context: &value, Profile: profile})
	return checkpoint, nil
}

func TestTerminalCheckpointReusesTurnReplayAndExactSnapshot(t *testing.T) {
	for _, exact := range []bool{true, false} {
		for _, tc := range []struct {
			name     string
			terminal protocol.EventData
			status   protocol.CheckpointStatus
			retry    bool
		}{
			{"completed", &protocol.TurnCompletedData{Text: "done"}, protocol.CheckpointCompleted, false},
			{"interrupted", &protocol.TurnCanceledData{Reason: "user_interrupted"}, protocol.CheckpointInterrupted, false},
			{"replay retry", &protocol.TurnCompletedData{Text: "done"}, protocol.CheckpointCompleted, true},
			{"failed", &protocol.TurnFailedData{Code: protocol.CodeInternal, Message: "failed"}, "", false},
		} {
			t.Run(fmt.Sprintf("exact=%t/%s", exact, tc.name), func(t *testing.T) {
				profile := runtimeTestProfile()
				value := artifactContextSnapshot(t, "checkpoint context")
				history, err := agentcontext.EncodeCompactedHistory(value.History)
				if err != nil {
					t.Fatal(err)
				}
				events := &checkpointReplayStore{MemoryEventStore: NewMemoryEventStore(16)}
				appendEvent := func(sequence protocol.Cursor, thread protocol.ThreadID, turn protocol.TurnID, data protocol.EventData) protocol.Event {
					t.Helper()
					event, err := protocol.NewEvent(protocol.EventMeta{
						Sequence: sequence, OperationID: "operation-checkpoint", ThreadID: thread, TurnID: turn,
						ItemID: protocol.ItemID(fmt.Sprintf("item-%d", sequence)),
					}, data)
					if err != nil {
						t.Fatal(err)
					}
					if err := events.Append(t.Context(), event); err != nil {
						t.Fatal(err)
					}
					return event
				}
				appendEvent(1, "parent-thread", "parent-turn", &protocol.CheckpointForkedData{
					CheckpointID: "checkpoint-parent", NewThreadID: "thread-profile", Title: "Fork",
					SourceCursor: 1, ReplacementHistory: history,
				})
				appendEvent(2, "thread-profile", "turn-checkpoint", &protocol.ExecutionReceiptData{
					Changes: []protocol.ReceiptChange{{Path: "a.go", Tool: "file_edit"}},
				})
				receipt := appendEvent(3, "thread-profile", "turn-checkpoint", &protocol.ExecutionReceiptData{
					Changes:        []protocol.ReceiptChange{{Path: "a.go", Tool: "file_edit"}, {Path: "b.go", Tool: "file_edit"}},
					ToolsSucceeded: []string{"file_edit"},
				})
				terminal := appendEvent(4, "thread-profile", "turn-checkpoint", tc.terminal)
				engine := &artifactTestEngine{history: value.History,
					contexts: map[protocol.ThreadID]agentcontext.ContextSnapshot{"thread-profile": value}}
				var runtimeEngine Engine = engine
				if !exact {
					runtimeEngine = struct {
						Engine
						CheckpointEngine
					}{engine, engine}
				}
				artifacts := &recordingCheckpointStore{}
				runtime := NewRuntime(Options{
					Engine: runtimeEngine, EventStore: events, SessionArtifacts: artifacts,
					SessionLifecycle: artifactLifecycle(), SessionProfiles: &memoryProfileStore{profile: profile},
					DefaultProfile: profile, ProfileCapabilities: runtimeTestCapabilities(profile),
					Recovery: &RecoveryState{LastSequence: terminal.Sequence},
				})
				t.Cleanup(func() { closeRuntime(t, runtime) })
				if tc.retry {
					events.broken = true
					runtime.PersistTerminalArtifactForTurn(t.Context(), "thread-profile", "turn-checkpoint")
					if len(artifacts.saved) != 0 || events.turnReads != 1 {
						t.Fatal("failed replay persisted a checkpoint or read twice")
					}
					events.broken = false
					events.turnReads = 0
				}
				runtime.PersistTerminalArtifactForTurn(t.Context(), "thread-profile", "turn-checkpoint")
				if events.turnReads != 1 {
					t.Fatalf("Turn replay count = %d, want 1", events.turnReads)
				}
				if tc.status == "" {
					if len(artifacts.saved) != 0 {
						t.Fatal("failed Turn created a checkpoint")
					}
					return
				}
				if len(artifacts.saved) != 1 {
					t.Fatalf("saved checkpoints = %d", len(artifacts.saved))
				}
				saved := artifacts.saved[0]
				checkpoint := saved.Checkpoint
				if checkpoint.Status != tc.status || checkpoint.ChangedFiles != 2 || !checkpoint.ExternalSideEffects ||
					checkpoint.ParentCheckpointID != "checkpoint-parent" || checkpoint.ChangeReceipt == nil ||
					checkpoint.ChangeReceipt.EventID != receipt.ID || checkpoint.ChangeReceipt.Cursor != receipt.Sequence ||
					checkpoint.Cursor != terminal.Sequence || checkpoint.ProfileRevision != profile.Revision {
					t.Fatalf("saved checkpoint facts = %+v", checkpoint)
				}
				if exact {
					if engine.historyReads != 0 || engine.contextReads != 1 || saved.Context == nil ||
						saved.Context.Digest != value.Digest || len(saved.History) != 0 {
						t.Fatalf("exact save: history reads=%d context reads=%d state=%+v", engine.historyReads, engine.contextReads, saved)
					}
				} else if engine.historyReads != 1 || engine.contextReads != 0 || saved.Context != nil || len(saved.History) != len(history) {
					t.Fatalf("history save: history reads=%d context reads=%d state=%+v", engine.historyReads, engine.contextReads, saved)
				}
			})
		}
	}
}

func TestExactCheckpointRestoreRollsBackContextOnPublicationFailure(t *testing.T) {
	profile := runtimeTestProfile()
	source := artifactContextSnapshot(t, "checkpoint context")
	previous := artifactContextSnapshot(t, "current context")
	history, err := agentcontext.EncodeCompactedHistory(source.History)
	if err != nil {
		t.Fatal(err)
	}
	artifacts := &memoryArtifactStore{profile: profile, context: source, history: history,
		checkpoint: protocol.SessionCheckpoint{
			Version: protocol.CheckpointProtocolVersion, ID: "checkpoint-context", SessionID: "session-profile",
			ThreadID: "thread-profile", TurnID: "turn-checkpoint", Cursor: 8, Status: protocol.CheckpointCompleted,
			ProfileRevision: profile.Revision, ContextDigest: source.Digest,
		}}
	engine := &artifactTestEngine{contexts: map[protocol.ThreadID]agentcontext.ContextSnapshot{"thread-profile": previous}}
	current := &artifactCurrentContextStore{}
	publishErr := errors.New("injected checkpoint publication failure")
	runtime := NewRuntime(Options{
		Engine: engine, SessionArtifacts: artifacts, SessionLifecycle: artifactLifecycle(), ContextRebaseStore: current,
		SessionProfiles: &memoryProfileStore{profile: profile}, DefaultProfile: profile,
		ProfileCapabilities: runtimeTestCapabilities(profile),
		EventStore:          artifactFailingEventStore{EventStore: NewMemoryEventStore(8), kind: protocol.EventCheckpointRestored, err: publishErr},
	})
	runtime.durable = true
	t.Cleanup(func() { closeRuntime(t, runtime) })
	if _, err := runtime.RestoreCheckpoint(t.Context(), "session-profile", "checkpoint-context"); !errors.Is(err, publishErr) {
		t.Fatalf("restore error = %v", err)
	}
	if engine.contexts["thread-profile"].Digest != previous.Digest ||
		current.current["thread-profile"].Snapshot.Digest != previous.Digest || artifacts.context.Digest != source.Digest {
		t.Fatal("publication failure did not restore the prior context independently of the checkpoint")
	}
	if artifacts.checkpointReads != 1 || engine.historyReads != 0 {
		t.Fatalf("rollback reads: checkpoint=%d history=%d", artifacts.checkpointReads, engine.historyReads)
	}
}
