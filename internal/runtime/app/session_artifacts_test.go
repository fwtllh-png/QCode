package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/persist/artifact"

	sessionhistory "github.com/fwtllh-png/QCode/internal/persist/history"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestRecoveryPreservesInlineAutoApprovedPlan(t *testing.T) {
	events := NewMemoryEventStore(8)
	meta := protocol.EventMeta{
		Sequence: 1, OperationID: "operation-inline-plan",
		ThreadID: "thread-profile", TurnID: "turn-inline-plan",
		ItemID: "item-inline-plan",
	}
	started, err := protocol.NewEvent(meta, &protocol.TurnStartedData{
		Provider: "fixture", Model: "fixture-model",
		ProfileRevision: 2, Intent: protocol.TurnIntentAnswer,
		Prompt: "Implement the project",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := events.Append(t.Context(), started); err != nil {
		t.Fatal(err)
	}
	meta.Sequence++
	planBody := `{"version":1,"revision":1,"steps":[` +
		`{"id":"implement","title":"Implement project","status":"in_progress"}]}`
	planEvent, err := protocol.NewEvent(meta, &protocol.PlanDeltaData{
		Body: planBody, Done: true, ArtifactID: "plan-inline",
		ProfileRevision: 2, Status: string(protocol.PlanArtifactReady),
		CanImplement: true, CanAutopilot: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := events.Append(t.Context(), planEvent); err != nil {
		t.Fatal(err)
	}
	meta.Sequence++
	receipt, err := protocol.NewEvent(meta, &protocol.ExecutionReceiptData{
		Goal: "Implement the project", Intent: protocol.TurnIntentAnswer,
		Plan: planBody, Verification: protocol.ReceiptVerification{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := events.Append(t.Context(), receipt); err != nil {
		t.Fatal(err)
	}
	meta.Sequence++
	failed, err := protocol.NewEvent(meta, &protocol.TurnFailedData{
		Code: protocol.CodeUnavailable, Message: "context admission failed",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := events.Append(t.Context(), failed); err != nil {
		t.Fatal(err)
	}
	profile := runtimeTestProfile()
	profile.Revision = 2
	runtime := NewRuntime(Options{
		Engine: &profileTestEngine{}, EventStore: events,
		SessionProfiles: &memoryProfileStore{profile: profile},
		DefaultProfile:  profile, ProfileCapabilities: runtimeTestCapabilities(profile),
		SessionLifecycle: artifactLifecycle(),
	})
	t.Cleanup(func() { closeRuntime(t, runtime) })

	prepared, err := runtime.PrepareTurnRecovery(t.Context(), protocol.TurnRecoveryRequest{
		Version: protocol.WorkflowIntentVersion, Action: protocol.TurnRecoveryContinue,
		SessionID: "session-profile", SourceTurnID: "turn-inline-plan",
		IdempotencyKey: "continue-inline-plan",
	})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Recovery.PlanID != "plan-inline" ||
		prepared.Recovery.PlanTransition != protocol.PlanTransitionAutopilot {
		t.Fatalf("recovery did not retain inline Plan: %+v", prepared.Recovery)
	}
	payload := &protocol.StartTurnPayload{
		ThreadID: "thread-profile",
		Recovery: &prepared.Recovery,
	}
	if err := runtime.PrepareStartPayload(
		t.Context(),
		"/workspace",
		payload,
	); err != nil {
		t.Fatalf("recovered inline Plan authorization: %v", err)
	}
	payload.Recovery.PlanID = "plan-forged"
	if err := runtime.PrepareStartPayload(
		t.Context(),
		"/workspace",
		payload,
	); protocol.CodeOf(err) != protocol.CodeConflict {
		t.Fatalf("forged inline Plan recovery error = %v, want conflict", err)
	}
}

type memoryArtifactStore struct {
	checkpoint protocol.SessionCheckpoint
	history    []protocol.CompactedMessage
	profile    protocol.SessionProfile
	plan       protocol.SessionPlanArtifact
	context    agentcontext.ContextSnapshot
}

func (s *memoryArtifactStore) SaveCheckpoint(
	context.Context,
	protocol.SessionCheckpoint,
	[]protocol.CompactedMessage,
	protocol.SessionProfile,
) (protocol.SessionCheckpoint, error) {
	return protocol.SessionCheckpoint{}, errors.New("unexpected Checkpoint save")
}

func (s *memoryArtifactStore) GetCheckpoint(
	context.Context,
	string,
) (
	protocol.SessionCheckpoint,
	[]protocol.CompactedMessage,
	protocol.SessionProfile,
	error,
) {
	return s.checkpoint,
		append([]protocol.CompactedMessage(nil), s.history...),
		s.profile,
		nil
}

func (s *memoryArtifactStore) SaveContextCheckpoint(
	context.Context,
	protocol.SessionCheckpoint,
	[]protocol.CompactedMessage,
	agentcontext.ContextSnapshot,
	protocol.SessionProfile,
) (protocol.SessionCheckpoint, error) {
	return protocol.SessionCheckpoint{}, errors.New("unexpected Context Checkpoint save")
}

func (s *memoryArtifactStore) GetContextCheckpoint(
	context.Context,
	string,
) (
	protocol.SessionCheckpoint,
	agentcontext.ContextSnapshot,
	protocol.SessionProfile,
	error,
) {
	return s.checkpoint, agentcontext.CloneContextSnapshot(s.context), s.profile, nil
}

func (s *memoryArtifactStore) ListCheckpoints(
	context.Context,
	string,
	int,
) ([]protocol.SessionCheckpoint, error) {
	return []protocol.SessionCheckpoint{s.checkpoint}, nil
}

func (s *memoryArtifactStore) CountCheckpoints(
	context.Context,
	string,
) (int, error) {
	return 1, nil
}

func (s *memoryArtifactStore) SavePlan(
	context.Context,
	protocol.SessionPlanArtifact,
) (protocol.SessionPlanArtifact, error) {
	return protocol.SessionPlanArtifact{}, errors.New("unexpected Plan save")
}

func (s *memoryArtifactStore) GetPlan(
	context.Context,
	string,
) (protocol.SessionPlanArtifact, error) {
	return s.plan, nil
}

func (s *memoryArtifactStore) LatestPlan(
	context.Context,
	string,
	protocol.ThreadID,
) (protocol.SessionPlanArtifact, bool, error) {
	return s.plan, s.plan.ID != "", nil
}

type artifactTestEngine struct {
	profileTestEngine
	history      []provider.Message
	restores     int
	restoreErrAt int
	restoreErr   error
	forks        map[protocol.ThreadID][]provider.Message
	contexts     map[protocol.ThreadID]agentcontext.ContextSnapshot
}

type artifactFailingEventStore struct {
	EventStore
	kind protocol.EventKind
	err  error
}

func (s artifactFailingEventStore) Append(
	ctx context.Context,
	event protocol.Event,
) error {
	if event.Kind == s.kind {
		return s.err
	}
	return s.EventStore.Append(ctx, event)
}

func (e *artifactTestEngine) History(
	protocol.ThreadID,
) ([]provider.Message, error) {
	return append([]provider.Message(nil), e.history...), nil
}

func (e *artifactTestEngine) RestoreCheckpoint(
	_ protocol.ThreadID,
	history []provider.Message,
) error {
	e.restores++
	if e.restoreErrAt == e.restores {
		return e.restoreErr
	}
	e.history = append([]provider.Message(nil), history...)
	return nil
}

func (e *artifactTestEngine) ForkCheckpoint(
	_ protocol.ThreadID,
	child protocol.ThreadID,
	history []provider.Message,
) error {
	if e.forks == nil {
		e.forks = make(map[protocol.ThreadID][]provider.Message)
	}
	e.forks[child] = append([]provider.Message(nil), history...)
	return nil
}

func (e *artifactTestEngine) Release(threadID protocol.ThreadID) {
	delete(e.forks, threadID)
	delete(e.contexts, threadID)
}

func (e *artifactTestEngine) ContextSnapshot(
	threadID protocol.ThreadID,
) (agentcontext.ContextSnapshot, error) {
	snapshot, ok := e.contexts[threadID]
	if !ok {
		return agentcontext.ContextSnapshot{}, errors.New("context snapshot is unavailable")
	}
	return agentcontext.CloneContextSnapshot(snapshot), nil
}

func (e *artifactTestEngine) RestoreContext(
	threadID protocol.ThreadID,
	snapshot agentcontext.ContextSnapshot,
) (agentcontext.ReconciliationReceipt, error) {
	if e.contexts == nil {
		e.contexts = make(map[protocol.ThreadID]agentcontext.ContextSnapshot)
	}
	e.contexts[threadID] = agentcontext.CloneContextSnapshot(snapshot)
	e.history = append([]provider.Message(nil), snapshot.History...)
	return agentcontext.ReconciliationReceipt{BindingMatch: true}, nil
}

func (e *artifactTestEngine) ForkContext(
	_ protocol.ThreadID,
	threadID protocol.ThreadID,
	snapshot agentcontext.ContextSnapshot,
) (agentcontext.ReconciliationReceipt, error) {
	if e.contexts == nil {
		e.contexts = make(map[protocol.ThreadID]agentcontext.ContextSnapshot)
	}
	e.contexts[threadID] = agentcontext.CloneContextSnapshot(snapshot)
	if e.forks == nil {
		e.forks = make(map[protocol.ThreadID][]provider.Message)
	}
	e.forks[threadID] = append([]provider.Message(nil), snapshot.History...)
	return agentcontext.ReconciliationReceipt{BindingMatch: true}, nil
}

type artifactCurrentContextStore struct {
	current map[protocol.ThreadID]agentcontext.CurrentContextCommit
}

func (s *artifactCurrentContextStore) CommitContextRebase(
	context.Context,
	agentcontext.ContextRebaseEnvelope,
) error {
	return nil
}

func (s *artifactCurrentContextStore) LatestContextSnapshot(
	_ context.Context,
	threadID protocol.ThreadID,
) (agentcontext.ContextSnapshot, bool, error) {
	commit, ok := s.current[threadID]
	return agentcontext.CloneContextSnapshot(commit.Snapshot), ok, nil
}

func (s *artifactCurrentContextStore) CommitCurrentContext(
	_ context.Context,
	commit agentcontext.CurrentContextCommit,
) error {
	if err := commit.Validate(); err != nil {
		return err
	}
	if s.current == nil {
		s.current = make(map[protocol.ThreadID]agentcontext.CurrentContextCommit)
	}
	s.current[commit.ThreadID] = commit
	return nil
}

func (s *artifactCurrentContextStore) DeleteCurrentContext(
	_ context.Context,
	threadID protocol.ThreadID,
	commitID string,
	_ bool,
) error {
	if current, ok := s.current[threadID]; ok && current.ID == commitID {
		delete(s.current, threadID)
	}
	return nil
}

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
	encoded, err := sessionhistory.EncodeCompactedHistory(checkpointContext.History)
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
	events, _, err := runtime.ReplayEvents(t.Context(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var restoreReferenced, forkReferenced bool
	for _, event := range events {
		switch data := event.Data.(type) {
		case *protocol.CheckpointRestoredData:
			saved := current.current["thread-profile"]
			restoreReferenced = data.ContextCommitID == saved.ID &&
				data.ContextDigest == saved.Snapshot.Digest &&
				data.ContextRevision == saved.Snapshot.Revision &&
				data.StateEpoch == saved.Snapshot.Epoch
		case *protocol.CheckpointForkedData:
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
	encoded, err := sessionhistory.EncodeCompactedHistory([]provider.Message{
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
	encoded, err := sessionhistory.EncodeCompactedHistory([]provider.Message{
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

func TestPlanExecutionDoesNotMutateProfile(t *testing.T) {
	profile := runtimeTestProfile()
	profile.ApprovalPosture = "suggest"
	profiles := &memoryProfileStore{profile: profile}
	artifacts := &memoryArtifactStore{plan: protocol.SessionPlanArtifact{
		Version: protocol.CheckpointProtocolVersion,
		ID:      "plan-scoped", SessionID: "session-profile",
		ThreadID: "thread-profile", TurnID: "turn-plan", Cursor: 7,
		Status: protocol.PlanArtifactReady,
		Body: `{"version":1,"revision":1,"steps":[` +
			`{"id":"implement","title":"Update parser","status":"pending"}]}`,
		ProfileRevision: profile.Revision,
		CanImplement:    true, CanAutopilot: true,
		CreatedAt: time.Now().UTC(),
	}}
	runtime := NewRuntime(Options{
		Engine: &profileTestEngine{}, SessionProfiles: profiles,
		DefaultProfile:      profile,
		ProfileCapabilities: runtimeTestCapabilities(profile),
		SessionLifecycle:    artifactLifecycle(), SessionArtifacts: artifacts,
	})
	t.Cleanup(func() { closeRuntime(t, runtime) })
	prepared, err := runtime.PreparePlanExecution(
		t.Context(),
		"session-profile",
		"plan-scoped",
		protocol.PlanTransitionAutopilot,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prepared.Prompt, artifacts.plan.Body) {
		t.Fatalf("execution prompt = %q", prepared.Prompt)
	}
	if profiles.profile.Mode != "act" ||
		profiles.profile.ApprovalPosture != "suggest" ||
		profiles.profile.Revision != profile.Revision {
		t.Fatalf("persistent profile mutated = %+v", profiles.profile)
	}
	artifacts.plan.Body = "1. Update parser"
	if _, err := runtime.PreparePlanExecution(
		t.Context(),
		"session-profile",
		"plan-scoped",
		protocol.PlanTransitionImplement,
	); protocol.CodeOf(err) != protocol.CodeInvalidArgument {
		t.Fatalf("Markdown Plan execution error = %v", err)
	}
}

func TestPlanExecutionSurvivesPlanningPolicyChange(t *testing.T) {
	profile := runtimeTestProfile()
	digest, err := artifact.PlanExecutionProfileDigest(profile)
	if err != nil {
		t.Fatal(err)
	}
	profiles := &memoryProfileStore{profile: profile}
	artifacts := &memoryArtifactStore{plan: protocol.SessionPlanArtifact{
		Version: protocol.CheckpointProtocolVersion,
		ID:      "plan-compatible", SessionID: "session-profile",
		ThreadID: "thread-profile", TurnID: "turn-plan", Cursor: 7,
		Status: protocol.PlanArtifactReady,
		Body: `{"version":1,"revision":1,"steps":[` +
			`{"id":"implement","title":"Update parser","status":"pending"}]}`,
		ProfileRevision:        profile.Revision,
		ExecutionProfileDigest: digest,
		CanImplement:           true,
		CanAutopilot:           true,
		CreatedAt:              time.Now().UTC(),
	}}
	runtime := NewRuntime(Options{
		Engine: &profileTestEngine{}, SessionProfiles: profiles,
		DefaultProfile:      profile,
		ProfileCapabilities: runtimeTestCapabilities(profile),
		ProfileModels: map[string]protocol.ModelCapabilities{
			profile.Provider + "\x00other-model": runtimeTestCapabilities(
				profile,
			).ModelCapabilities,
		},
		SessionLifecycle: artifactLifecycle(), SessionArtifacts: artifacts,
	})
	t.Cleanup(func() { closeRuntime(t, runtime) })

	required := "required"
	updated, err := protocol.ApplySessionProfilePatch(
		profile,
		protocol.SessionProfilePatch{PlanningPolicy: &required},
	)
	if err != nil {
		t.Fatal(err)
	}
	profiles.profile = updated.Profile
	snapshot, err := runtime.SessionPlan(t.Context(), "session-profile")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Artifact == nil || !snapshot.Artifact.CanImplement {
		t.Fatalf("Planning policy change disabled execution: %+v", snapshot.Artifact)
	}
	if _, err := runtime.PreparePlanExecution(
		t.Context(),
		"session-profile",
		"plan-compatible",
		protocol.PlanTransitionImplement,
	); err != nil {
		t.Fatalf("Planning policy change made execution stale: %v", err)
	}

	model := "other-model"
	changed, err := protocol.ApplySessionProfilePatch(
		updated.Profile,
		protocol.SessionProfilePatch{Model: &model},
	)
	if err != nil {
		t.Fatal(err)
	}
	profiles.profile = changed.Profile
	if _, err := runtime.PreparePlanExecution(
		t.Context(),
		"session-profile",
		"plan-compatible",
		protocol.PlanTransitionImplement,
	); protocol.CodeOf(err) != protocol.CodeConflict {
		t.Fatalf("execution profile change error = %v, want conflict", err)
	}
}

func TestPlanExecutionAllowsTrailingSourceOperationCommit(t *testing.T) {
	profile := runtimeTestProfile()
	profiles := &memoryProfileStore{profile: profile}
	artifacts := &memoryArtifactStore{plan: protocol.SessionPlanArtifact{
		Version: protocol.CheckpointProtocolVersion,
		ID:      "plan-race", SessionID: "session-profile",
		ThreadID: "thread-profile", TurnID: "turn-plan", Cursor: 7,
		Status: protocol.PlanArtifactReady,
		Body: `{"version":1,"revision":1,"steps":[` +
			`{"id":"implement","title":"Update parser","status":"pending"}]}`,
		ProfileRevision: profile.Revision,
		CanImplement:    true, CanAutopilot: true,
		CreatedAt: time.Now().UTC(),
	}}
	runtime := NewRuntime(Options{
		Engine: &profileTestEngine{}, SessionProfiles: profiles,
		DefaultProfile:      profile,
		ProfileCapabilities: runtimeTestCapabilities(profile),
		SessionLifecycle:    artifactLifecycle(), SessionArtifacts: artifacts,
	})
	t.Cleanup(func() { closeRuntime(t, runtime) })
	runtime.EventService.mu.Lock()
	runtime.terminals["turn-plan"] = protocol.EventTurnCompleted
	runtime.EventService.mu.Unlock()
	runtime.OperationService.mu.Lock()
	runtime.OperationService.accepted["operation-plan"] = PendingOperation{
		ID: "operation-plan", SessionID: "session-profile",
	}
	runtime.OperationService.mu.Unlock()

	status, err := runtime.SessionStatus(t.Context(), "session-profile")
	if err != nil {
		t.Fatal(err)
	}
	if status.Status != protocol.SessionStatusRunning {
		t.Fatalf("race status = %q, want running", status.Status)
	}
	plan, err := runtime.SessionPlan(t.Context(), "session-profile")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Artifact == nil || !plan.Artifact.CanImplement {
		t.Fatalf("Plan transition remained disabled: %+v", plan.Artifact)
	}
	if _, err := runtime.PreparePlanExecution(
		t.Context(),
		"session-profile",
		"plan-race",
		protocol.PlanTransitionImplement,
	); err != nil {
		t.Fatalf("prepare during trailing commit: %v", err)
	}
	lease, err := runtime.active.Reserve(
		"thread-profile",
		"turn-active",
		"operation-active",
		"item-active",
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = runtime.active.Release(lease) }()
	if _, err := runtime.PreparePlanExecution(
		t.Context(),
		"session-profile",
		"plan-race",
		protocol.PlanTransitionImplement,
	); protocol.CodeOf(err) != protocol.CodeConflict {
		t.Fatalf("active Plan execution error = %v", err)
	}
}

func TestPrepareTurnRecoveryAcceptsJournalAdmissionFailureWithoutStarted(t *testing.T) {
	events := NewMemoryEventStore(4)
	meta := protocol.EventMeta{
		Sequence: 1, OperationID: "operation-draft",
		ThreadID: "thread-profile", TurnID: "turn-draft-block",
		ItemID: "item-draft",
	}
	failed, err := protocol.NewEvent(meta, &protocol.TurnFailedData{
		Code:    protocol.CodeConflict,
		Message: "workspace journal has a retained draft; continue, retry, or revert it first",
		Fault: &protocol.FaultMetadata{
			Disposition: protocol.FaultRetryTurn,
			SideEffects: protocol.SideEffectDraft,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := events.Append(t.Context(), failed); err != nil {
		t.Fatal(err)
	}
	profile := runtimeTestProfile()
	lifecycle := artifactLifecycle()
	lifecycle.summary.LatestTurnID = "turn-draft-block"
	runtime := NewRuntime(Options{
		EventStore:          events,
		SessionLifecycle:    lifecycle,
		Engine:              &profileTestEngine{},
		SessionProfiles:     &memoryProfileStore{profile: profile},
		DefaultProfile:      profile,
		ProfileCapabilities: runtimeTestCapabilities(profile),
	})
	t.Cleanup(func() { closeRuntime(t, runtime) })
	prepared, err := runtime.PrepareTurnRecovery(
		t.Context(),
		protocol.TurnRecoveryRequest{
			Version: protocol.WorkflowIntentVersion, Action: protocol.TurnRecoveryRetry,
			SessionID: "session-profile", SourceTurnID: "turn-draft-block",
			IdempotencyKey: "retry-draft-block",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Recovery.Action != protocol.TurnRecoveryRetry ||
		prepared.Recovery.SourceTurnID != "turn-draft-block" ||
		!strings.Contains(prepared.Prompt, "retained workspace journal draft") {
		t.Fatalf("journal admission recovery = %+v", prepared)
	}
}

func TestTurnRecoveryCreatesANewPromptWithoutReplayingOperations(t *testing.T) {
	events := NewMemoryEventStore(16)
	meta := protocol.EventMeta{
		Sequence:    1,
		OperationID: "operation-source",
		ThreadID:    "thread-profile",
		TurnID:      "turn-source",
		ItemID:      "item-source",
	}
	started, err := protocol.NewEvent(meta, &protocol.TurnStartedData{
		Provider:        "fixture",
		Model:           "fixture-model",
		Prompt:          "Fix the parser",
		Intent:          protocol.TurnIntentWorkspaceChange,
		PlanID:          "plan-source",
		PlanTransition:  protocol.PlanTransitionImplement,
		ProfileRevision: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := events.Append(t.Context(), started); err != nil {
		t.Fatal(err)
	}
	meta.Sequence = 2
	output, err := protocol.NewEvent(meta, &protocol.OutputDeltaData{
		Text: "I inspected the parser and reached validation.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := events.Append(t.Context(), output); err != nil {
		t.Fatal(err)
	}
	meta.Sequence = 3
	toolStarted, err := protocol.NewEvent(meta, &protocol.ToolStartData{
		Tool: "file_read", CallID: "call-read",
		Arguments: json.RawMessage(`{"path":"parser.go"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := events.Append(t.Context(), toolStarted); err != nil {
		t.Fatal(err)
	}
	meta.Sequence = 4
	toolResult, err := protocol.NewEvent(meta, &protocol.ToolResultData{
		Tool: "file_read", CallID: "call-read", Output: "package parser",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := events.Append(t.Context(), toolResult); err != nil {
		t.Fatal(err)
	}
	meta.Sequence = 5
	receipt, err := protocol.NewEvent(meta, &protocol.ExecutionReceiptData{
		Intent:       protocol.TurnIntentWorkspaceChange,
		Outcome:      protocol.TurnOutcomeChanged,
		ReadPaths:    []string{"parser.go"},
		Verification: protocol.ReceiptVerification{},
		WorkspaceOutcome: &protocol.ReceiptWorkspaceOutcome{
			Status: "unchanged",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := events.Append(t.Context(), receipt); err != nil {
		t.Fatal(err)
	}
	meta.Sequence = 6
	failed, err := protocol.NewEvent(meta, &protocol.TurnFailedData{
		Code: protocol.CodeConflict, Message: "validation failed",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := events.Append(t.Context(), failed); err != nil {
		t.Fatal(err)
	}
	profile := runtimeTestProfile()
	profile.Revision = 2
	profile.ApprovalPosture = "bypass"
	profiles := &memoryProfileStore{profile: profile}
	engine := &profileTestEngine{}
	lifecycle := artifactLifecycle()
	lifecycle.summary.LatestTurnID = "turn-source"
	runtime := NewRuntime(Options{
		EventStore:          events,
		SessionLifecycle:    lifecycle,
		Engine:              engine,
		SessionProfiles:     profiles,
		DefaultProfile:      profile,
		ProfileCapabilities: runtimeTestCapabilities(profile),
	})
	t.Cleanup(func() { closeRuntime(t, runtime) })
	retry, err := runtime.PrepareTurnRecovery(
		t.Context(),
		protocol.TurnRecoveryRequest{
			Version:   protocol.WorkflowIntentVersion,
			Action:    protocol.TurnRecoveryRetry,
			SessionID: "session-profile", SourceTurnID: "turn-source",
			IdempotencyKey: "retry-source",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if retry.Prompt != "Fix the parser" ||
		retry.DisplayPrompt != "Fix the parser" ||
		retry.Intent != protocol.TurnIntentWorkspaceChange ||
		retry.IdempotencyKey != "retry-source" {
		t.Fatalf("Retry preparation = %+v", retry)
	}
	engine.mu.Lock()
	applied := engine.applied
	engine.mu.Unlock()
	if applied.Revision != profile.Revision ||
		applied.ApprovalPosture != "bypass" {
		t.Fatalf("Recovery applied profile = %+v, want %+v", applied, profile)
	}
	continued, err := runtime.PrepareTurnRecovery(
		t.Context(),
		protocol.TurnRecoveryRequest{
			Version:   protocol.WorkflowIntentVersion,
			Action:    protocol.TurnRecoveryContinue,
			SessionID: "session-profile", SourceTurnID: "turn-source",
			Prompt: "Run focused tests", IdempotencyKey: "continue-source",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(continued.Prompt, "Do not restate") ||
		strings.Contains(continued.Prompt, "Do not file_read recovery_evidence") ||
		strings.Contains(continued.Prompt, "Do not repeat this Continue envelope") ||
		strings.Contains(continued.Prompt, "Do not repeat completed Tool") ||
		strings.Contains(continued.Prompt, "Inspect current workspace state before every") ||
		continued.Intent != protocol.TurnIntentWorkspaceChange ||
		continued.Recovery.Action != protocol.TurnRecoveryContinue ||
		continued.Recovery.SourceTurnID != "turn-source" ||
		continued.Recovery.PlanID != "plan-source" ||
		continued.Recovery.PlanTransition != protocol.PlanTransitionImplement ||
		continued.Recovery.ProfileRevision != profile.Revision ||
		!strings.HasPrefix(continued.Prompt, "Run focused tests") ||
		!strings.Contains(continued.Prompt, `<source_request turn="turn-source"/>`) ||
		!strings.Contains(continued.Prompt, "<recovery_evidence>") ||
		!strings.Contains(continued.Prompt, `"version":3`) ||
		!strings.Contains(continued.Prompt, `"intent":"workspace_change"`) ||
		!strings.Contains(continued.Prompt, `"known_reads":["parser.go"]`) ||
		!strings.Contains(continued.Prompt, `"outcomes"`) ||
		!strings.Contains(continued.Prompt, `"tool":"file_read"`) ||
		strings.Contains(continued.Prompt, `"outcome":"changed"`) ||
		strings.Contains(continued.Prompt, "Additional guidance:") ||
		!strings.Contains(continued.Prompt, "Run focused tests") {
		t.Fatalf("Continue preparation = %+v", continued)
	}
	if continued.DisplayPrompt != "Run focused tests" {
		t.Fatalf("Continue display prompt = %q", continued.DisplayPrompt)
	}
	if artifact.RecoveryWorkItemGoal(continued.Prompt) != "Run focused tests" {
		t.Fatalf("Continue goal = %q", artifact.RecoveryWorkItemGoal(continued.Prompt))
	}
	reads, _ := artifact.ParseRecoveryWorkItem(continued.Prompt)
	if len(reads) == 0 || reads[0] != "parser.go" {
		t.Fatalf("Continue known reads = %v", reads)
	}
	recoveredMeta := protocol.EventMeta{
		Sequence:    7,
		OperationID: "operation-continued",
		ThreadID:    "thread-profile",
		TurnID:      "turn-continued",
		ItemID:      "item-continued",
	}
	recoveredStart, err := protocol.NewEvent(
		recoveredMeta,
		&protocol.TurnStartedData{
			Provider: "fixture", Model: "fixture-model",
			Prompt: continued.Prompt, DisplayPrompt: continued.DisplayPrompt,
			Intent: protocol.TurnIntentWorkspaceChange,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := events.Append(t.Context(), recoveredStart); err != nil {
		t.Fatal(err)
	}
	recoveredMeta.Sequence = 8
	recoveredTerminal, err := protocol.NewEvent(
		recoveredMeta,
		&protocol.TurnCanceledData{Reason: protocol.CancelReasonUserInterrupted},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := events.Append(t.Context(), recoveredTerminal); err != nil {
		t.Fatal(err)
	}
	lifecycle.summary.LatestTurnID = "turn-continued"
	_, err = runtime.PrepareTurnRecovery(
		t.Context(),
		protocol.TurnRecoveryRequest{
			Version:   protocol.WorkflowIntentVersion,
			Action:    protocol.TurnRecoveryRetry,
			SessionID: "session-profile", SourceTurnID: "turn-source",
			IdempotencyKey: "retry-stale-source",
		},
	)
	if protocol.CodeOf(err) != protocol.CodeConflict {
		t.Fatalf("stale recovery source error = %v, want conflict", err)
	}
	problem := protocol.ProblemOf(err)
	if problem.Details == nil ||
		problem.Details.Reason != protocol.ProblemReasonStaleRecoverySource {
		t.Fatalf("stale recovery source problem = %+v", problem)
	}
	retriedContinue, err := runtime.PrepareTurnRecovery(
		t.Context(),
		protocol.TurnRecoveryRequest{
			Version:   protocol.WorkflowIntentVersion,
			Action:    protocol.TurnRecoveryRetry,
			SessionID: "session-profile", SourceTurnID: "turn-continued",
			IdempotencyKey: "retry-continued",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if retriedContinue.Prompt != continued.Prompt ||
		retriedContinue.DisplayPrompt != continued.DisplayPrompt {
		t.Fatalf(
			"Retry unwrapped latest recovery Turn: prompt=%q display=%q",
			retriedContinue.Prompt,
			retriedContinue.DisplayPrompt,
		)
	}
	continuedAgain, err := runtime.PrepareTurnRecovery(
		t.Context(),
		protocol.TurnRecoveryRequest{
			Version:   protocol.WorkflowIntentVersion,
			Action:    protocol.TurnRecoveryContinue,
			SessionID: "session-profile", SourceTurnID: "turn-continued",
			IdempotencyKey: "continue-continued",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if continuedAgain.DisplayPrompt != "Continue: Run focused tests" ||
		!strings.HasPrefix(continuedAgain.Prompt, "Run focused tests") ||
		!strings.Contains(
			continuedAgain.Prompt,
			`<source_request turn="turn-continued"/>`,
		) {
		t.Fatalf("Continue unwrapped to stale request: %+v", continuedAgain)
	}
	pollutedMeta := protocol.EventMeta{
		Sequence:    9,
		OperationID: "operation-polluted",
		ThreadID:    "thread-profile",
		TurnID:      "turn-polluted",
		ItemID:      "item-polluted",
	}
	pollutedStart, err := protocol.NewEvent(
		pollutedMeta,
		&protocol.TurnStartedData{
			Provider: "fixture", Model: "fixture-model",
			Prompt: artifact.TurnRecoveryPromptPrefix +
				" Do not infer the task from an older conversation Turn.\n\n" +
				"Source Turn ID: turn-continued\n" +
				"Terminal state: canceled: user_interrupted\n\n" +
				"Original model-visible request:\n" +
				"<source_request>\nFix the parser\n</source_request>",
			DisplayPrompt: "Continue: Fix the parser",
			Intent:        protocol.TurnIntentWorkspaceChange,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := events.Append(t.Context(), pollutedStart); err != nil {
		t.Fatal(err)
	}
	pollutedMeta.Sequence = 10
	pollutedTerminal, err := protocol.NewEvent(
		pollutedMeta,
		&protocol.TurnCanceledData{Reason: protocol.CancelReasonUserInterrupted},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := events.Append(t.Context(), pollutedTerminal); err != nil {
		t.Fatal(err)
	}
	lifecycle.summary.LatestTurnID = "turn-polluted"
	recoveredPolluted, err := runtime.PrepareTurnRecovery(
		t.Context(),
		protocol.TurnRecoveryRequest{
			Version:   protocol.WorkflowIntentVersion,
			Action:    protocol.TurnRecoveryContinue,
			SessionID: "session-profile", SourceTurnID: "turn-polluted",
			IdempotencyKey: "continue-polluted",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if recoveredPolluted.DisplayPrompt != "Continue: Run focused tests" {
		t.Fatalf(
			"Continue retained polluted request: %+v",
			recoveredPolluted,
		)
	}
	lifecycle.summary.LatestTurnID = "turn-source"
	recoveryPayload := &protocol.StartTurnPayload{
		ThreadID: "thread-profile",
		Recovery: &continued.Recovery,
	}
	if err := runtime.PrepareStartPayload(
		t.Context(), "/workspace", recoveryPayload,
	); err != nil {
		t.Fatalf("validated Plan recovery = %v", err)
	}
	profiles.mu.Lock()
	profiles.profile.Revision++
	profiles.mu.Unlock()
	if err := runtime.PrepareStartPayload(
		t.Context(), "/workspace", recoveryPayload,
	); protocol.CodeOf(err) != protocol.CodeConflict {
		t.Fatalf("stale profile recovery error = %v, want conflict", err)
	}
	refreshed, err := runtime.PrepareTurnRecovery(
		t.Context(),
		protocol.TurnRecoveryRequest{
			Version:        protocol.WorkflowIntentVersion,
			Action:         protocol.TurnRecoveryContinue,
			SessionID:      "session-profile",
			SourceTurnID:   "turn-source",
			IdempotencyKey: "continue-after-profile-change",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.Recovery.PlanID != "" ||
		refreshed.Recovery.PlanTransition != "" ||
		refreshed.Recovery.ProfileRevision != 0 ||
		!strings.Contains(refreshed.Prompt, "Submit a fresh structured Plan") {
		t.Fatalf("stale Plan was not removed from recovery: %+v", refreshed)
	}
	refreshedPayload := &protocol.StartTurnPayload{
		ThreadID: "thread-profile",
		Recovery: &refreshed.Recovery,
	}
	if err := runtime.PrepareStartPayload(
		t.Context(), "/workspace", refreshedPayload,
	); err != nil {
		t.Fatalf("recovery without stale Plan binding = %v", err)
	}
	profiles.mu.Lock()
	profiles.profile.Revision--
	profiles.mu.Unlock()
	recoveryPayload.Recovery.PlanID = "plan-other"
	if err := runtime.PrepareStartPayload(
		t.Context(), "/workspace", recoveryPayload,
	); protocol.CodeOf(err) != protocol.CodeConflict {
		t.Fatalf("forged Plan recovery error = %v, want conflict", err)
	}
	for _, internal := range []string{
		"Source Turn ID",
		"<source_request>",
		"<recovery_evidence>",
		`"call_id"`,
		`"arguments_digest"`,
	} {
		if strings.Contains(continued.DisplayPrompt, internal) {
			t.Fatalf(
				"Continue display prompt leaked %q: %q",
				internal,
				continued.DisplayPrompt,
			)
		}
	}
	replayed, err := events.Replay(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(replayed) != 10 {
		t.Fatalf("Recovery preparation emitted historical operations: %+v", replayed)
	}
}

func TestTurnRecoveryUsesRecoverableStartOperationRejection(t *testing.T) {
	events := NewMemoryEventStore(4)
	meta := protocol.EventMeta{
		Sequence: 1, OperationID: "operation-source",
		ThreadID: "thread-source", TurnID: "turn-source", ItemID: "item-source",
	}
	started, err := protocol.NewEvent(meta, &protocol.TurnStartedData{
		Provider: "fixture", Model: "fixture-model",
		Prompt: "Continue implementation",
		Intent: protocol.TurnIntentWorkspaceChange,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := events.Append(t.Context(), started); err != nil {
		t.Fatal(err)
	}
	meta.Sequence++
	rejected, err := protocol.NewEvent(meta, &protocol.OperationRejectedData{
		Code:    protocol.CodeUnavailable,
		Message: "terminal envelope could not be committed",
		Fault: &protocol.FaultMetadata{
			Origin:         protocol.FaultOriginPersistence,
			Disposition:    protocol.FaultRetryStep,
			SideEffects:    protocol.SideEffectDraft,
			RecoveryAction: "retry the idempotent terminal commit",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := events.Append(t.Context(), rejected); err != nil {
		t.Fatal(err)
	}
	profile := runtimeTestProfile()
	lifecycle := artifactLifecycle()
	lifecycle.threadIDs = []protocol.ThreadID{"thread-source", "thread-profile"}
	runtime := NewRuntime(Options{
		Engine: &profileTestEngine{}, EventStore: events,
		SessionProfiles:     &memoryProfileStore{profile: profile},
		DefaultProfile:      profile,
		ProfileCapabilities: runtimeTestCapabilities(profile),
		SessionLifecycle:    lifecycle,
	})
	t.Cleanup(func() { closeRuntime(t, runtime) })

	prepared, err := runtime.PrepareTurnRecovery(
		t.Context(),
		protocol.TurnRecoveryRequest{
			Version:   protocol.WorkflowIntentVersion,
			Action:    protocol.TurnRecoveryContinue,
			SessionID: "session-profile", SourceTurnID: "turn-source",
			IdempotencyKey: "continue-rejected-source",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Recovery.SourceTurnID != "turn-source" ||
		!strings.Contains(
			prepared.Prompt,
			"interrupted before terminal commit (unavailable): "+
				"terminal envelope could not be committed",
		) {
		t.Fatalf("recovery preparation = %+v", prepared)
	}
}

func TestTurnRecoveryDefaultsLegacyEmptyIntentToAnswer(t *testing.T) {
	events := NewMemoryEventStore(4)
	meta := protocol.EventMeta{
		Sequence:    1,
		OperationID: "operation-source",
		ThreadID:    "thread-profile",
		TurnID:      "turn-source",
		ItemID:      "item-source",
	}
	started, err := protocol.NewEvent(meta, &protocol.TurnStartedData{
		Provider: "fixture",
		Model:    "fixture-model",
		Prompt:   "Explain the parser",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := events.Append(t.Context(), started); err != nil {
		t.Fatal(err)
	}
	meta.Sequence++
	completed, err := protocol.NewEvent(meta, &protocol.TurnCompletedData{
		Text: "The parser validates input.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := events.Append(t.Context(), completed); err != nil {
		t.Fatal(err)
	}
	runtime := NewRuntime(Options{
		EventStore:       events,
		SessionLifecycle: artifactLifecycle(),
	})
	t.Cleanup(func() { closeRuntime(t, runtime) })

	prepared, err := runtime.PrepareTurnRecovery(
		t.Context(),
		protocol.TurnRecoveryRequest{
			Version:        protocol.WorkflowIntentVersion,
			Action:         protocol.TurnRecoveryContinue,
			SessionID:      "session-profile",
			SourceTurnID:   "turn-source",
			IdempotencyKey: "continue-source",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Intent != protocol.TurnIntentAnswer {
		t.Fatalf("Recovery intent = %q, want answer", prepared.Intent)
	}
}

func TestRecoveryDisplayPromptUnwrapsLegacyInternalPrompt(t *testing.T) {
	legacy := artifact.TurnRecoveryPromptPrefix + ` Do not infer the task.

Original model-visible request:
<source_request>
Fix the parser
</source_request>

<recovery_evidence>
{"source_turn_id":"turn-source","closed_tools":[{"call_id":"call-read"}]}
</recovery_evidence>`
	if got := artifact.RecoveryDisplayPrompt(legacy, legacy); got != "Fix the parser" {
		t.Fatalf("legacy recovery display prompt = %q", got)
	}
	nested := artifact.TurnRecoveryPromptPrefix + ` Do not infer the task.

Original model-visible request:
<source_request>
` + legacy + `
</source_request>`
	if got := artifact.RecoverySourcePrompt(nested); got != "Fix the parser" {
		t.Fatalf("nested recovery source prompt = %q", got)
	}
	withQuotedClose := artifact.TurnRecoveryPromptPrefix + ` Do not infer the task.

Original model-visible request:
<source_request>
Fix the parser
</source_request>

Recovery guidance:
<guidance>
Explain the literal </source_request> tag.
</guidance>`
	if got := artifact.RecoverySourcePrompt(withQuotedClose); got != "Fix the parser" {
		t.Fatalf("quoted close recovery source prompt = %q", got)
	}
	if got := artifact.RecoveryDisplayPrompt(
		"internal recovery context",
		"Continue: Continue: Fix the parser",
	); got != "Fix the parser" {
		t.Fatalf("nested recovery display prompt = %q", got)
	}
	if got := artifact.RecoveryDisplayPrompt(
		legacy,
		"Continue: Fix the parser\n\nGuidance: obsolete direction",
	); got != "Fix the parser" {
		t.Fatalf("recovered display prompt retained old guidance = %q", got)
	}
}

func TestRecoveryEvidenceIsCanonicalAndBounded(t *testing.T) {
	first := artifact.RecoveryDigestJSON(
		[]byte(`{"b":9223372036854775807,"a":1}`),
	)
	second := artifact.RecoveryDigestJSON(
		[]byte(`{"a":1,"b":9223372036854775807}`),
	)
	if first == "" || first != second {
		t.Fatalf("canonical argument digests = %q and %q", first, second)
	}
	tools := make([]artifact.RecoveryToolEvidence, 200)
	for index := range tools {
		tools[index] = artifact.RecoveryToolEvidence{
			Tool:            "file_read",
			CallID:          fmt.Sprintf("call-%03d", index),
			ArgumentsDigest: first,
			OutputDigest:    artifact.RecoveryDigest([]byte(strings.Repeat("x", index+1))),
		}
	}
	rendered := artifact.RenderRecoveryEvidence(
		"turn-source",
		protocol.TurnIntentWorkspaceChange,
		"failed (conflict): no changes",
		tools,
		&protocol.ExecutionReceiptData{
			ReadPaths:    []string{"parser.go", "parser_test.go"},
			Verification: protocol.ReceiptVerification{},
			WorkspaceOutcome: &protocol.ReceiptWorkspaceOutcome{
				Status: "unchanged",
			},
		},
		"partial conclusion",
	)
	if rendered == "" || len(rendered) > artifact.TurnRecoveryEvidenceLimit {
		t.Fatalf("rendered recovery evidence bytes = %d", len(rendered))
	}
	var capsule artifact.RecoveryEvidenceCapsule
	if err := json.Unmarshal([]byte(rendered), &capsule); err != nil {
		t.Fatal(err)
	}
	if capsule.Version != 3 ||
		capsule.Intent != protocol.TurnIntentWorkspaceChange ||
		capsule.SourceTurnID != "turn-source" ||
		capsule.OmittedTools == 0 ||
		len(capsule.Tools)+capsule.OmittedTools != len(tools) ||
		capsule.WorkItem == nil ||
		len(capsule.WorkItem.KnownReads) != 2 ||
		len(capsule.Outcomes) == 0 ||
		capsule.Receipt == nil ||
		len(capsule.Receipt.ReadPaths) != 2 ||
		capsule.PartialOutput != "partial conclusion" {
		t.Fatalf("recovery evidence = %+v", capsule)
	}
}

func artifactLifecycle() *memorySessionLifecycleStore {
	now := time.Now().UTC()
	return &memorySessionLifecycleStore{summary: protocol.SessionSummary{
		Version:         protocol.SessionLifecycleVersion,
		Revision:        1,
		SessionID:       "session-profile",
		ThreadID:        "thread-profile",
		Title:           "Artifacts",
		Status:          protocol.SessionStatusIdle,
		Isolation:       "shared",
		WorkspaceRoot:   "/workspace",
		WorkspaceLabel:  "workspace",
		ExecutionTarget: "local",
		CreatedAt:       now,
		UpdatedAt:       now,
	}}
}
