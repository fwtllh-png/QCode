package app

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestExactContextRestoreAndForkPersistCurrentBaselines(t *testing.T) {
	profile := runtimeTestProfile()
	window, err := agentcontext.NewWindowLedger("checkpoint-window", 1)
	if err != nil {
		t.Fatal(err)
	}
	binding := agentcontext.WorkspaceBinding{
		WorkspaceIdentity: "workspace:test",
	}
	binding.Seal()
	message := provider.TextMessage(provider.RoleUser, "checkpoint context")
	message.Turn = 1
	checkpointContext := agentcontext.ContextSnapshot{
		Version: agentcontext.ContextSnapshotVersion,
		Epoch:   1, Revision: 1, Turn: 1,
		History:   []provider.Message{message},
		Workspace: binding,
		Window:    window,
	}
	if err := checkpointContext.Seal(); err != nil {
		t.Fatal(err)
	}
	encoded, err := agentcontext.EncodeCompactedHistory(checkpointContext.History)
	if err != nil {
		t.Fatal(err)
	}
	artifacts := &memoryArtifactStore{
		checkpoint: protocol.SessionCheckpoint{
			Version: protocol.CheckpointProtocolVersion,
			ID:      "checkpoint-context", SessionID: "session-profile",
			ThreadID: "thread-profile", TurnID: "turn-checkpoint",
			Cursor: 8, Status: protocol.CheckpointCompleted,
			ProfileRevision: profile.Revision, CanRestore: true, CanFork: true,
			ContextDigest: checkpointContext.Digest,
		},
		profile: profile,
		context: checkpointContext,
		history: encoded,
	}
	engine := &artifactTestEngine{
		contexts: map[protocol.ThreadID]agentcontext.ContextSnapshot{
			"thread-profile": checkpointContext,
		},
	}
	current := &artifactCurrentContextStore{}
	lifecycle := artifactLifecycle()
	runtime := NewRuntime(Options{
		Engine:              engine,
		SessionProfiles:     &memoryProfileStore{profile: profile},
		DefaultProfile:      profile,
		ProfileCapabilities: runtimeTestCapabilities(profile),
		SessionLifecycle:    lifecycle,
		SessionArtifacts:    artifacts,
		ContextRebaseStore:  current,
	})
	runtime.durable = true
	t.Cleanup(func() { closeRuntime(t, runtime) })

	restored, err := runtime.RestoreCheckpoint(
		t.Context(),
		"session-profile",
		"checkpoint-context",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !restored.ExactContext ||
		current.current["thread-profile"].Snapshot.Digest != checkpointContext.Digest {
		t.Fatalf("restore=%+v current=%+v", restored, current.current)
	}
	if artifacts.checkpointReads != 1 || engine.historyReads != 0 {
		t.Errorf("exact restore reads: checkpoint=%d history=%d", artifacts.checkpointReads, engine.historyReads)
	}
	forked, err := runtime.ForkCheckpoint(
		t.Context(),
		"session-profile",
		"checkpoint-context",
		"Child",
	)
	if err != nil {
		t.Fatal(err)
	}
	commit, ok := current.current[forked.ThreadID]
	if !forked.ExactContext || !ok ||
		commit.ParentThreadID != "thread-profile" ||
		commit.SessionID != "session-profile" ||
		commit.Snapshot.Digest != checkpointContext.Digest {
		t.Fatalf("fork=%+v commit=%+v", forked, commit)
	}
	if artifacts.checkpointReads != 2 || engine.historyReads != 0 {
		t.Errorf("exact restore and fork reads: checkpoint=%d history=%d", artifacts.checkpointReads, engine.historyReads)
	}
	events, _, err := runtime.ReplayEvents(t.Context(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var restoreReferenced, forkReferenced bool
	for _, event := range events {
		switch data := event.Data.(type) {
		case *protocol.CheckpointRestoredData:
			if !reflect.DeepEqual(data.ReplacementHistory, encoded) {
				t.Fatalf("restore replacement history = %+v", data.ReplacementHistory)
			}
			saved := current.current["thread-profile"]
			restoreReferenced = data.ContextCommitID == saved.ID &&
				data.ContextDigest == saved.Snapshot.Digest &&
				data.ContextRevision == saved.Snapshot.Revision &&
				data.StateEpoch == saved.Snapshot.Epoch
		case *protocol.CheckpointForkedData:
			if !reflect.DeepEqual(data.ReplacementHistory, encoded) {
				t.Fatalf("fork replacement history = %+v", data.ReplacementHistory)
			}
			forkReferenced = data.ContextCommitID == commit.ID &&
				data.ContextDigest == commit.Snapshot.Digest &&
				data.ContextRevision == commit.Snapshot.Revision &&
				data.StateEpoch == commit.Snapshot.Epoch
		}
	}
	if !restoreReferenced || !forkReferenced {
		t.Fatalf(
			"context references restore=%t fork=%t events=%+v",
			restoreReferenced,
			forkReferenced,
			events,
		)
	}
}

func TestCheckpointRestoreIsStateOnlyAndForkPreservesLineage(t *testing.T) {
	profile := runtimeTestProfile()
	encoded, err := agentcontext.EncodeCompactedHistory([]provider.Message{
		provider.TextMessage(provider.RoleUser, "checkpoint prompt"),
		provider.TextMessage(provider.RoleAssistant, "checkpoint result"),
	})
	if err != nil {
		t.Fatal(err)
	}
	artifacts := &memoryArtifactStore{
		checkpoint: protocol.SessionCheckpoint{
			Version:         protocol.CheckpointProtocolVersion,
			ID:              "checkpoint-1",
			SessionID:       "session-profile",
			ThreadID:        "thread-profile",
			TurnID:          "turn-checkpoint",
			Cursor:          8,
			Status:          protocol.CheckpointCompleted,
			Summary:         "Checkpoint result",
			ProfileRevision: profile.Revision,
			CanRestore:      true,
			CanFork:         true,
			CreatedAt:       time.Now().UTC(),
		},
		history: encoded,
		profile: profile,
	}
	engine := &artifactTestEngine{
		profileTestEngine: profileTestEngine{},
		history: []provider.Message{
			provider.TextMessage(provider.RoleUser, "later work"),
		},
	}
	lifecycle := artifactLifecycle()
	runtime := NewRuntime(Options{
		Engine:              engine,
		SessionProfiles:     &memoryProfileStore{profile: profile},
		DefaultProfile:      profile,
		ProfileCapabilities: runtimeTestCapabilities(profile),
		SessionLifecycle:    lifecycle,
		SessionArtifacts:    artifacts,
	})
	t.Cleanup(func() { closeRuntime(t, runtime) })
	result, err := runtime.RestoreCheckpoint(
		t.Context(),
		"session-profile",
		"checkpoint-1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.SideEffectsReplayed || engine.restores != 1 ||
		len(engine.history) != 2 {
		t.Fatalf("Restore result=%+v history=%+v", result, engine.history)
	}
	events, _, err := runtime.ReplayEvents(t.Context(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind == protocol.EventToolStart ||
			event.Kind == protocol.EventToolResult {
			t.Fatalf("Restore replayed Tool event %+v", event)
		}
	}
	forked, err := runtime.ForkCheckpoint(
		t.Context(),
		"session-profile",
		"checkpoint-1",
		"Child",
	)
	if err != nil {
		t.Fatal(err)
	}
	if forked.ParentID != "thread-profile" ||
		len(engine.forks[forked.ThreadID]) != 2 ||
		lifecycle.summary.ThreadID != forked.ThreadID ||
		lifecycle.summary.ParentThreadID != "thread-profile" {
		t.Fatalf("Fork result=%+v lifecycle=%+v", forked, lifecycle.summary)
	}
}

func TestCheckpointRestoreJoinsPublicationAndRollbackFailures(t *testing.T) {
	profile := runtimeTestProfile()
	encoded, err := agentcontext.EncodeCompactedHistory([]provider.Message{
		provider.TextMessage(provider.RoleUser, "checkpoint prompt"),
		provider.TextMessage(provider.RoleAssistant, "checkpoint result"),
	})
	if err != nil {
		t.Fatal(err)
	}
	rollbackErr := errors.New("injected checkpoint rollback failure")
	publishErr := errors.New("injected checkpoint publication failure")
	engine := &artifactTestEngine{
		profileTestEngine: profileTestEngine{},
		history: []provider.Message{
			provider.TextMessage(provider.RoleUser, "current history"),
		},
		restoreErrAt: 2,
		restoreErr:   rollbackErr,
	}
	events := artifactFailingEventStore{
		EventStore: NewMemoryEventStore(16),
		kind:       protocol.EventCheckpointRestored,
		err:        publishErr,
	}
	runtime := NewRuntime(Options{
		Engine:              engine,
		EventStore:          events,
		SessionProfiles:     &memoryProfileStore{profile: profile},
		DefaultProfile:      profile,
		ProfileCapabilities: runtimeTestCapabilities(profile),
		SessionLifecycle:    artifactLifecycle(),
		SessionArtifacts: &memoryArtifactStore{
			checkpoint: protocol.SessionCheckpoint{
				Version: protocol.CheckpointProtocolVersion,
				ID:      "checkpoint-rollback", SessionID: "session-profile",
				ThreadID: "thread-profile", TurnID: "turn-checkpoint",
				Cursor: 8, Status: protocol.CheckpointCompleted,
				ProfileRevision: profile.Revision, CanRestore: true,
			},
			history: encoded,
			profile: profile,
		},
	})
	t.Cleanup(func() { closeRuntime(t, runtime) })
	_, err = runtime.RestoreCheckpoint(
		t.Context(),
		"session-profile",
		"checkpoint-rollback",
	)
	if err == nil ||
		!errors.Is(err, publishErr) ||
		!errors.Is(err, rollbackErr) {
		t.Fatalf("RestoreCheckpoint() error = %v", err)
	}
	if engine.restores != 2 {
		t.Fatalf("RestoreCheckpoint() calls = %d, want 2", engine.restores)
	}
}
