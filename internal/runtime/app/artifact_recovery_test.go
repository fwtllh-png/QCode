package app

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

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
	if RecoveryWorkItemGoal(continued.Prompt) != "Run focused tests" {
		t.Fatalf("Continue goal = %q", RecoveryWorkItemGoal(continued.Prompt))
	}
	reads, _ := ParseRecoveryWorkItem(continued.Prompt)
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
			Prompt: TurnRecoveryPromptPrefix +
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
