package app

import (
	"strings"
	"testing"
	"time"

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
	digest, err := PlanExecutionProfileDigest(profile)
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
