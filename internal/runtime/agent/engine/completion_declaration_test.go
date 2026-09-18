package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	providerfixture "github.com/fwtllh-png/QCode/internal/adapter/provider/fixture"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	completiontool "github.com/fwtllh-png/QCode/internal/adapter/tool/completion"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/interact"
	"github.com/fwtllh-png/QCode/internal/observability/verify"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	"github.com/fwtllh-png/QCode/internal/security/policy"
)

type declarationWriteTool struct{}

func (declarationWriteTool) Descriptor() tool.Descriptor {
	return tool.Descriptor{
		Name: "write_fixture", Description: "record one fixture mutation",
		Visibility: tool.VisibleModel, Capability: tool.CapabilityWrite,
		AccessMode: tool.AccessWrite, ParallelPolicy: tool.ParallelSerial,
		SandboxRequirement: tool.SandboxNone,
		Availability:       tool.AvailabilityAvailable,
		InputSchema: map[string]any{
			"type": "object", "properties": map[string]any{},
			"additionalProperties": false,
		},
	}
}

func (declarationWriteTool) Execute(
	context.Context, json.RawMessage,
) (tool.Result, error) {
	return tool.Result{
		Content: `{"status":"written"}`,
		Outcome: &tool.Outcome{Facts: &tool.OutcomeFacts{
			WorkspaceChanges: []tool.WorkspaceChange{{
				Path: "a.go", Kind: tool.WorkspaceModified, Added: 1,
			}},
		}},
	}, nil
}

type declarationCommandTool struct{}

func (declarationCommandTool) Descriptor() tool.Descriptor {
	return tool.Descriptor{
		Name: "exec_command", Description: "record fixture verification",
		Visibility: tool.VisibleModel, Capability: tool.CapabilityProcess,
		AccessMode: tool.AccessTree, ParallelPolicy: tool.ParallelSerial,
		SandboxRequirement: tool.SandboxNone,
		Availability:       tool.AvailabilityAvailable,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"covered_paths": map[string]any{
					"type":  "array",
					"items": map[string]any{"type": "string"},
				},
			},
			"required": []string{"covered_paths"}, "additionalProperties": false,
		},
	}
}

func (declarationCommandTool) TrustedBinding() tool.TrustedBinding {
	binding := tool.TrustedBindingFromDescriptor(
		declarationCommandTool{}.Descriptor(),
	)
	binding.ProducesVerificationEvidence = true
	return binding
}

func (declarationCommandTool) Execute(
	_ context.Context, raw json.RawMessage,
) (tool.Result, error) {
	var input struct {
		CoveredPaths []string `json:"covered_paths"`
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return tool.Result{}, err
	}
	return commandEvidenceResult(verify.StatusPassed, input.CoveredPaths), nil
}

func TestSubmittedPlanContinuesCurrentTurn(t *testing.T) {
	registry := tool.NewRegistry(nil, nil)
	root := t.TempDir()
	if err := interact.Register(registry, interact.Options{
		Host: interact.NewHost(0), Workspace: root,
	}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(&completiontool.Tool{}); err != nil {
		t.Fatal(err)
	}
	runtime := &scriptedProvider{streams: []provider.Stream{
		toolCallStream("plan-1", "submit_plan", `{
			"version":1,
			"title":"Execute this plan",
			"steps":[{"id":"implement","title":"Implement the change"}]
		}`),
		toolCallStream("plan-2", "update_plan", `{
			"version":1,
			"title":"Execute this plan",
			"steps":[{"id":"implement","title":"Implement the change","status":"done"}]
		}`),
		toolCallStream("complete-1", completiontool.Name, `{
			"status":"complete",
			"summary":"Implemented.",
			"pending_actions":[]
		}`),
	}}
	engine := newEngine(t, runtime, registry)
	engine.options.Security.ConfigurePlanning(policy.PlanningAdaptive)

	result, err := engine.RunForTurn(t.Context(), "turn-plan-auto", "implement", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != Completed || result.Text != "Implemented." {
		t.Fatalf("result = %+v", result)
	}
	if len(runtime.requests) != 3 {
		t.Fatalf("model requests = %d, want 3", len(runtime.requests))
	}
}

func TestSubmittedPlanRetriesPreviouslyRejectedReplayTool(t *testing.T) {
	registry := tool.NewRegistry(nil, nil)
	if err := interact.Register(registry, interact.Options{
		Host: interact.NewHost(0), Workspace: t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}
	write := &countingCatalogExecutor{descriptor: tool.Descriptor{
		Name: "edit_fixture", Description: "edit a fixture",
		Visibility: tool.VisibleModel, Capability: tool.CapabilityWrite,
		AccessMode: tool.AccessWrite, ParallelPolicy: tool.ParallelSerial,
		RepeatPolicy:       tool.RepeatReplaySameTurn,
		SandboxRequirement: tool.SandboxNone,
		Availability:       tool.AvailabilityAvailable,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"value": map[string]any{"type": "string"},
			},
			"required":             []string{"value"},
			"additionalProperties": false,
		},
	}}
	if err := registry.Register(write); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(&completiontool.Tool{}); err != nil {
		t.Fatal(err)
	}
	runtime := &scriptedProvider{streams: []provider.Stream{
		toolCallStream("edit-before-plan", "edit_fixture", `{"value":"same"}`),
		toolCallStream("submit-plan", "submit_plan", `{
			"version":1,
			"title":"Execute this plan",
			"steps":[{"id":"implement","title":"Implement the change"}]
		}`),
		toolCallStream("edit-after-plan", "edit_fixture", `{"value":"same"}`),
		toolCallStream("complete", completiontool.Name, `{
			"status":"complete",
			"summary":"Implemented.",
			"pending_actions":[]
		}`),
	}}
	engine := newEngine(t, runtime, registry)
	engine.options.Security.ConfigurePlanning(policy.PlanningRequired)

	result, err := engine.RunForTurn(t.Context(), "turn-plan-retry", "implement", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != Completed || result.Text != "Implemented." {
		t.Fatalf("result = %+v", result)
	}
	if write.calls.Load() != 1 {
		t.Fatalf("edit executions = %d, want 1 after Plan submission", write.calls.Load())
	}
}

func TestReadOnlyAnswerCompletesFromEndTurnWithoutDeclarationRepair(t *testing.T) {
	registry := declarationRegistry(t, false)
	runtime := &scriptedProvider{streams: []provider.Stream{textStream("Direct answer.")}}
	engine := declarationEngine(t, runtime, registry, passedReceipt())

	result, err := engine.RunForTurnWithIntentAndAttachments(
		t.Context(), "turn-answer-direct", "answer this",
		protocol.TurnIntentAnswer, nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != Completed || result.Text != "Direct answer." {
		t.Fatalf("result = %+v", result)
	}
	if len(runtime.requests) != 1 {
		t.Fatalf("model requests = %d, want 1", len(runtime.requests))
	}
}

func TestWorkspaceChangeCompletesFromCapturedText(t *testing.T) {
	registry := declarationRegistry(t, false)
	runtime := &scriptedProvider{streams: []provider.Stream{
		toolCallStream("write-1", "write_fixture", `{}`),
		textStream("Implemented and verified."),
	}}
	engine := declarationEngine(t, runtime, registry, passedReceipt())
	var events []Event

	result, err := engine.RunForTurnWithIntentAndAttachments(
		t.Context(), "turn-1", "change a.go",
		protocol.TurnIntentWorkspaceChange, nil,
		func(event Event) error {
			events = append(events, event)
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != Completed || result.Text != "Implemented and verified." {
		t.Fatalf("result = %+v", result)
	}
	if len(runtime.requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(runtime.requests))
	}
	verifyIndex, finalIndex := -1, -1
	for index, event := range events {
		if event.State == Verifying {
			verifyIndex = index
		}
		if event.Text == "Implemented and verified." {
			finalIndex = index
		}
	}
	if verifyIndex < 0 || finalIndex < 0 || verifyIndex >= finalIndex {
		t.Fatalf("verification must precede final answer: %+v", events)
	}
}

func TestAnswerMutationCompletesFromCapturedText(t *testing.T) {
	registry := declarationRegistry(t, false)
	runtime := &scriptedProvider{streams: []provider.Stream{
		toolCallStream("write-1", "write_fixture", `{}`),
		textStream("I changed the file and the review is complete."),
	}}
	engine := declarationEngine(t, runtime, registry, passedReceipt())

	result, err := engine.RunForTurnWithIntentAndAttachments(
		t.Context(), "turn-answer", "fix a.go",
		protocol.TurnIntentAnswer, nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != Completed ||
		result.Text != "I changed the file and the review is complete." {
		t.Fatalf("result = %+v", result)
	}
	if len(runtime.requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(runtime.requests))
	}
}

func TestReadOnlyToolTurnCompletesFromCapturedText(t *testing.T) {
	registry := declarationRegistry(t, false)
	if err := registry.Register(&echoTool{}); err != nil {
		t.Fatal(err)
	}
	runtime := &scriptedProvider{streams: []provider.Stream{
		toolCallStream("read-1", "echo", `{"text":"evidence"}`),
		textStream("The review is complete and the findings are ready."),
	}}
	engine := declarationEngine(t, runtime, registry, passedReceipt())

	result, err := engine.RunForTurnWithIntentAndAttachments(
		t.Context(), "turn-read-only", "review the evidence",
		protocol.TurnIntentAnswer, nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != Completed ||
		result.Text != "The review is complete and the findings are ready." {
		t.Fatalf("result = %+v", result)
	}
	if len(runtime.requests) != 2 {
		t.Fatalf("read-only tool requests = %d, want 2", len(runtime.requests))
	}
}

func TestNoToolPlanCompletesFromCapturedText(t *testing.T) {
	registry := declarationRegistry(t, false)
	runtime := &scriptedProvider{streams: []provider.Stream{
		textStream("I will now provide the implementation plan."),
	}}
	engine := declarationEngine(t, runtime, registry, passedReceipt())

	result, err := engine.RunForTurnWithIntentAndAttachments(
		t.Context(), "turn-read-only-plan", "plan the change",
		protocol.TurnIntentPlan, nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != Completed ||
		result.Text != "I will now provide the implementation plan." ||
		len(runtime.requests) != 1 {
		t.Fatalf("result=%+v requests=%d", result, len(runtime.requests))
	}
	history := engine.History()
	if len(history) == 0 {
		t.Fatal("completed text did not commit history")
	}
	final := history[len(history)-1]
	if final.Role != provider.RoleAssistant ||
		blocksText(final.Blocks) != "I will now provide the implementation plan." {
		t.Fatalf("final history message = %+v", final)
	}
}

func TestReadOnlyPlanCompletesOnFirstTextSample(t *testing.T) {
	registry := declarationRegistry(t, false)
	runtime := &scriptedProvider{streams: []provider.Stream{
		textStream("The R3, R4, and R5 evidence review is complete."),
	}}
	engine := declarationEngine(t, runtime, registry, passedReceipt())

	result, err := engine.RunForTurnWithIntentAndAttachments(
		t.Context(), "turn-progressing-declarations",
		"review R3, R4, and R5 evidence",
		protocol.TurnIntentPlan, nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != Completed ||
		result.Text != "The R3, R4, and R5 evidence review is complete." ||
		len(runtime.requests) != 1 {
		t.Fatalf("result=%+v requests=%d", result, len(runtime.requests))
	}
}

func TestIncompleteDeclarationStopsWithResumableBlockedOutcome(t *testing.T) {
	registry := declarationRegistry(t, false)
	if err := registry.Register(&echoTool{}); err != nil {
		t.Fatal(err)
	}
	runtime := &scriptedProvider{streams: []provider.Stream{
		toolCallStream("read-1", "echo", `{"text":"first evidence"}`),
		toolCallStream("incomplete-1", completiontool.Name, `{
			"status":"incomplete",
			"summary":"the second evidence check remains",
			"pending_actions":["inspect the second piece of evidence"]
		}`),
	}}
	engine := declarationEngine(t, runtime, registry, passedReceipt())
	var events []Event

	result, err := engine.RunForTurnWithIntentAndAttachments(
		t.Context(), "turn-incomplete", "review both pieces of evidence",
		protocol.TurnIntentOperation, nil,
		func(event Event) error {
			events = append(events, event)
			return nil
		},
	)
	if err == nil ||
		!strings.Contains(err.Error(), "declared incomplete") {
		t.Fatalf("error = %v", err)
	}
	if result.State != Failed {
		t.Fatalf("result = %+v", result)
	}
	if len(runtime.requests) != 2 {
		t.Fatalf("incomplete declaration did not stop the turn: %+v",
			runtime.requests)
	}
	sawBlockedDeclaration := false
	for _, event := range events {
		if event.ToolCall != nil &&
			event.ToolCall.ID == "incomplete-1" &&
			event.Result != nil {
			accepted, _ := event.Result.Metadata["completion_declaration_accepted"].(bool)
			rejection, _ := event.Result.Metadata["completion_declaration_rejection"].(string)
			sawBlockedDeclaration = !accepted && rejection == "convergence_blocked"
		}
	}
	if !sawBlockedDeclaration {
		t.Fatalf("incomplete declaration was not blocked: %+v", events)
	}
}

func TestEngineRejectsRequiredCompletionToolMissing(t *testing.T) {
	_, err := newTestEngine(Options{ProviderConfig: ProviderConfig{Provider: &scriptedProvider{}, Route: testRoute(t)}, ToolConfig: ToolConfig{Tools: tool.NewRegistry(nil, nil),
		RequireCompletionDeclaration: true},
	})
	if err == nil || !strings.Contains(err.Error(), "turn_complete") {
		t.Fatalf("New() error = %v", err)
	}
}

func TestCompletionDeclarationBindsExactMutationRevision(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	scope := attachTestScope(t, engine)
	scope.state.diff.Record(turnkernel.TurnDiffEntry{Path: "a.go", Kind: "modified"})
	scope.state.verification = append(scope.state.verification, verify.Evidence{
		SchemaVersion: 1, Kind: "verify", Status: verify.StatusPassed,
		CoveredPaths: []string{"a.go"}, CallID: "verify-1",
		MutationRevision: 1,
	})
	declaration := tool.CompletionDeclaration{
		Status: "complete", Summary: "done",
	}
	sameBatch := tool.Result{Outcome: &tool.Outcome{Facts: &tool.OutcomeFacts{
		Completion: &declaration,
	}}}
	sameBatchCandidate := engine.completionCandidate(
		provider.ToolCall{
			ID: "complete-same-batch", Name: completiontool.Name,
		},
		sameBatch,
		true,
		1,
		1,
	)
	if !sameBatchCandidate.BatchMutated {
		t.Fatalf("same-batch candidate = %+v", sameBatchCandidate)
	}
	bindCompletionDecision(&sameBatch, turnkernel.CompletionDecision{
		Reason:         "same_batch_mutation",
		RequiredAction: "correct_and_retry_turn_complete",
	})
	if accepted, _ := sameBatch.Metadata["completion_declaration_accepted"].(bool); accepted {
		t.Fatalf("same-batch declaration accepted: %#v", sameBatch.Metadata)
	}

	accepted := tool.Result{Outcome: &tool.Outcome{Facts: &tool.OutcomeFacts{
		Completion: &declaration,
	}}}
	bindCompletionDecision(&accepted, turnkernel.CompletionDecision{
		Accepted:          true,
		Summary:           "done",
		RequiredAction:    "await_runtime_verification",
		Mutation:          1,
		ChangedPaths:      []string{"a.go"},
		VerificationCalls: []string{"verify-1"},
		CompletionCall:    "complete-1",
	})
	bound := accepted.Outcome.Facts.Completion
	if bound == nil {
		t.Fatalf("exact declaration rejected: %#v", accepted.Outcome)
	}
	if got := bound.ChangedPaths; len(got) != 1 || got[0] != "a.go" {
		t.Fatalf("runtime-bound changed paths = %v", got)
	}
	if got := bound.VerificationCallIDs; len(got) != 1 ||
		got[0] != "verify-1" {
		t.Fatalf("runtime-bound verification call IDs = %v", got)
	}
	if bound.MutationRevision != 1 {
		t.Fatalf("bound mutation revision = %d, want 1", bound.MutationRevision)
	}
}

func TestRejectedCompletionResultExposesTheRuntimeDecision(t *testing.T) {
	result := tool.Result{
		Content: `{"status":"pending_runtime_validation"}`,
		Outcome: &tool.Outcome{Facts: &tool.OutcomeFacts{
			Completion: &tool.CompletionDeclaration{
				Status: "complete", Summary: "read-only review complete",
			},
		}},
	}

	bindCompletionDecision(&result, turnkernel.CompletionDecision{
		Reason:         "no_observed_changes",
		RequiredAction: "perform_workspace_mutation",
		CompletionCall: "complete-read-only",
	})

	if !strings.Contains(result.Content, `"status":"rejected"`) ||
		!strings.Contains(result.Content, `"reason":"no_observed_changes"`) ||
		!strings.Contains(result.Content, `"required_action":"perform_workspace_mutation"`) {
		t.Fatalf("rejected completion result = %s", result.Content)
	}
	if accepted, _ := result.Metadata["completion_declaration_accepted"].(bool); accepted {
		t.Fatalf("rejected completion metadata = %#v", result.Metadata)
	}
}

func TestMalformedCompletionResultPreservesSchemaError(t *testing.T) {
	tests := map[string]string{
		"missing arguments": "",
		"nested arguments": `{
			"arguments":"{\"status\":\"incomplete\",\"summary\":\"work remains\",\"pending_actions\":[\"continue\"]}"
		}`,
	}
	for name, arguments := range tests {
		t.Run(name, func(t *testing.T) {
			engine := newEngine(
				t,
				&scriptedProvider{},
				declarationRegistry(t, false),
			)
			var emitted *tool.Result

			results, err := engine.runTools(
				t.Context(),
				"turn-malformed-completion",
				[]provider.ToolCall{{
					ID: "complete-malformed", Name: completiontool.Name,
					Arguments: arguments,
				}},
				make(map[string]tool.Result),
				func(_ State, event Event) error {
					if event.Result != nil {
						copy := *event.Result
						emitted = &copy
					}
					return nil
				},
			)
			if err != nil {
				t.Fatalf("runTools() error = %v", err)
			}
			if len(results) != 1 || !results[0].IsError {
				t.Fatalf("results = %+v", results)
			}
			var output struct {
				Status         string `json:"status"`
				Reason         string `json:"reason"`
				RequiredAction string `json:"required_action"`
				ErrorDetail    string `json:"error_detail"`
			}
			if err := json.Unmarshal([]byte(results[0].Content), &output); err != nil {
				t.Fatalf("decode result %q: %v", results[0].Content, err)
			}
			if output.Status != "rejected" ||
				output.Reason != "invalid_declaration" ||
				output.RequiredAction != "correct_and_retry_turn_complete" ||
				!strings.Contains(output.ErrorDetail, "status") ||
				!strings.Contains(output.ErrorDetail, "summary") ||
				!strings.Contains(output.ErrorDetail, "pending_actions") {
				t.Fatalf("output = %+v", output)
			}
			detail, _ := results[0].Metadata["completion_declaration_error"].(string)
			if detail != output.ErrorDetail {
				t.Fatalf("metadata detail = %q, output detail = %q", detail, output.ErrorDetail)
			}
			if emitted == nil || emitted.Content != results[0].Content {
				t.Fatalf("emitted = %+v, results = %+v", emitted, results)
			}
		})
	}
}

func TestVerificationRepairInvalidatesCompletionDeclaration(t *testing.T) {
	registry := declarationRegistry(t, true)
	runtime := &scriptedProvider{streams: []provider.Stream{
		toolCallStream("write-1", "write_fixture", `{}`),
		toolCallStream("complete-1", completiontool.Name, `{
			"status":"complete",
			"summary":"mutation complete",
			"pending_actions":[]
		}`),
		toolCallStream("verify-1", "exec_command", `{"covered_paths":["a.go"]}`),
		toolCallStream("complete-2", completiontool.Name, `{
			"status":"complete",
			"summary":"Implemented and verified.",
			"pending_actions":[]
		}`),
	}}
	engine := declarationEngine(t, runtime, registry, verify.Receipt{
		Scope: verify.ScopeDiagnostics, Status: verify.StatusUnavailable,
		Message: "no diagnostics covered a.go",
	})
	engine.options.Verify.Mode = VerifyModeHard
	var completion *tool.CompletionDeclaration

	result, err := engine.RunForTurnWithIntentAndAttachments(
		t.Context(), "turn-1", "change a.go",
		protocol.TurnIntentWorkspaceChange, nil, func(event Event) error {
			if event.Completion != nil {
				copy := *event.Completion
				completion = &copy
			}
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != Completed || result.Verification == nil ||
		result.Verification.Status != verify.StatusPassed {
		t.Fatalf("result = %+v", result)
	}
	if len(runtime.requests) != 4 ||
		!requestContains(runtime.requests[2], "[verify]") ||
		completion == nil ||
		completion.CallID != "complete-2" {
		t.Fatalf("repair sequence = %+v", runtime.requests)
	}
}

func TestHardVerificationRepairCompletesFromLaterText(t *testing.T) {
	registry := declarationRegistry(t, true)
	runtime := &scriptedProvider{streams: []provider.Stream{
		toolCallStream("write-1", "write_fixture", `{}`),
		toolCallStream("complete-1", completiontool.Name, `{
			"status":"complete",
			"summary":"mutation complete",
			"pending_actions":[]
		}`),
		toolCallStream("verify-1", "exec_command", `{"covered_paths":["a.go"]}`),
		textStream("Implemented and verified."),
	}}
	engine := declarationEngine(t, runtime, registry, verify.Receipt{
		Scope: verify.ScopeDiagnostics, Status: verify.StatusUnavailable,
		Message: "no diagnostics covered a.go",
	})
	engine.options.Verify.Mode = VerifyModeHard

	result, err := engine.RunForTurnWithIntentAndAttachments(
		t.Context(), "turn-progress", "change a.go",
		protocol.TurnIntentWorkspaceChange, nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != Completed ||
		result.Text != "Implemented and verified." ||
		len(runtime.requests) != 4 {
		t.Fatalf("result=%+v requests=%d", result, len(runtime.requests))
	}
}

func batchCallStream(calls ...provider.ToolCall) provider.Stream {
	events := make([]provider.StreamEvent, 0, len(calls)+1)
	for index, call := range calls {
		events = append(events, provider.StreamEvent{
			Type: provider.EventToolCallDelta,
			ToolCall: &provider.ToolCallFragment{
				Index: index, ID: call.ID, Name: call.Name,
				Arguments: call.Arguments,
			},
		})
	}
	events = append(events, provider.StreamEvent{
		Type: provider.EventMessageStop, StopReason: provider.StopReasonToolUse,
	})
	return &providerfixture.SliceStream{Events: events}
}

// A completion declaration batched with other calls must recover through
// model-correctable feedback from any position in the batch, instead of
// failing the turn on an illegal kernel phase transition.
func TestBatchedCompletionDeclarationRecoversFromAnyPosition(t *testing.T) {
	declarationFor := func(id string) provider.ToolCall {
		return provider.ToolCall{
			ID: id, Name: completiontool.Name,
			Arguments: `{
				"status":"complete",
				"summary":"Recovered.",
				"pending_actions":[]
			}`,
		}
	}
	commandFor := func(id string) provider.ToolCall {
		return provider.ToolCall{
			ID: id, Name: "exec_command",
			Arguments: `{"covered_paths":["a.go"]}`,
		}
	}
	writeFor := func(id string) provider.ToolCall {
		return provider.ToolCall{ID: id, Name: "write_fixture", Arguments: `{}`}
	}
	cases := []struct {
		name   string
		calls  []provider.ToolCall
		reason string
		intent protocol.TurnIntent
		input  string
	}{
		{
			name:   "declaration first",
			calls:  []provider.ToolCall{declarationFor("complete-1"), commandFor("verify-1")},
			reason: "declaration_must_be_only_call",
			intent: protocol.TurnIntentAnswer,
			input:  "settle the fixture",
		},
		{
			name: "declaration middle",
			calls: []provider.ToolCall{
				commandFor("verify-1"), declarationFor("complete-1"),
				commandFor("verify-2"),
			},
			reason: "declaration_must_be_only_call",
			intent: protocol.TurnIntentAnswer,
			input:  "settle the fixture",
		},
		{
			name:   "declaration last",
			calls:  []provider.ToolCall{commandFor("verify-1"), declarationFor("complete-1")},
			reason: "declaration_must_be_only_call",
			intent: protocol.TurnIntentAnswer,
			input:  "settle the fixture",
		},
		{
			name:   "declaration before same-batch mutation",
			calls:  []provider.ToolCall{declarationFor("complete-1"), writeFor("write-1")},
			reason: "same_batch_mutation",
			intent: protocol.TurnIntentWorkspaceChange,
			input:  "change a.go",
		},
		{
			name:   "declaration after same-batch mutation",
			calls:  []provider.ToolCall{writeFor("write-1"), declarationFor("complete-1")},
			reason: "same_batch_mutation",
			intent: protocol.TurnIntentWorkspaceChange,
			input:  "change a.go",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			retry := toolCallStream("complete-2", completiontool.Name, `{
				"status":"complete",
				"summary":"Recovered.",
				"pending_actions":[]
			}`)
			runtime := &scriptedProvider{streams: []provider.Stream{
				batchCallStream(testCase.calls...), retry,
			}}
			engine := declarationEngine(t, runtime, declarationRegistry(t, true),
				verify.Receipt{
					Scope:   verify.ScopeDiagnostics,
					Status:  verify.StatusUnavailable,
					Message: "no diagnostics covered a.go",
				})
			result, err := engine.RunForTurnWithIntentAndAttachments(
				t.Context(), "turn-batch", testCase.input,
				testCase.intent, nil, nil,
			)
			if err != nil {
				t.Fatalf(
					"Run() error = %v requests=%+v", err, runtime.requests,
				)
			}
			if result.State != Completed || result.Text != "Recovered." {
				t.Fatalf("result=%+v", result)
			}
			if len(runtime.requests) < 2 ||
				!requestToolResultContains(
					runtime.requests[1], testCase.reason,
				) {
				t.Fatalf(
					"rejection feedback missing: requests=%+v", runtime.requests,
				)
			}
		})
	}
}

func requestToolResultContains(request provider.ModelRequest, value string) bool {
	for _, message := range request.Messages {
		for _, block := range message.Blocks {
			if block.ToolResult != nil &&
				strings.Contains(block.ToolResult.Content, value) {
				return true
			}
		}
	}
	return false
}

func declarationRegistry(t *testing.T, withVerification bool) *tool.Registry {
	t.Helper()
	registry := tool.NewRegistry(nil, nil)
	for _, executor := range []tool.Executor{
		declarationWriteTool{}, &completiontool.Tool{},
	} {
		if err := registry.Register(executor); err != nil {
			t.Fatal(err)
		}
	}
	if withVerification {
		if err := registry.Register(declarationCommandTool{}); err != nil {
			t.Fatal(err)
		}
	}
	return registry
}

func declarationEngine(
	t *testing.T,
	runtime *scriptedProvider,
	registry *tool.Registry,
	receipt verify.Receipt,
) *Engine {
	t.Helper()
	root := t.TempDir()
	engine, err := newTestEngine(Options{ProviderConfig: ProviderConfig{Provider: runtime, Route: testRoute(t),
		MaxOutputTokens: 256, MaxSteps: 12}, ToolConfig: ToolConfig{Tools: registry,

		Authorize:                    func(provider.ToolCall) bool { return true },
		RequireCompletionDeclaration: true,

		Verify: VerifyOptions{
			Mode: VerifyModeSoft, Scope: verify.ScopeDiagnostics,
			MaxRepairSteps: 1,
			Runner:         &scriptedVerifier{receipts: []verify.Receipt{receipt}},
		}}, SecurityConfig: SecurityConfig{Workspace: root, Journal: newTestWorkspaceJournal(t, root)}, LifecycleConfig: LifecycleConfig{InputHost: interact.NewHost(0)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func toolCallStream(id, name, arguments string) provider.Stream {
	return &providerfixture.SliceStream{Events: []provider.StreamEvent{
		{
			Type: provider.EventToolCallDelta,
			ToolCall: &provider.ToolCallFragment{
				Index: 0, ID: id, Name: name, Arguments: arguments,
			},
		},
		{Type: provider.EventMessageStop, StopReason: provider.StopReasonToolUse},
	}}
}

func requestContains(request provider.ModelRequest, value string) bool {
	for _, message := range request.Messages {
		if strings.Contains(message.Text(), value) {
			return true
		}
	}
	return false
}

func TestPreserveProvisionalAppendsClosingSummaryToCapturedText(t *testing.T) {
	registry := declarationRegistry(t, false)
	runtime := &scriptedProvider{streams: []provider.Stream{
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventTextDelta, Text: "The evidence review found three issues: R3, R4, and R5 are stale."},
			{Type: provider.EventToolCallDelta, ToolCall: &provider.ToolCallFragment{
				Index: 0, ID: "complete-1", Name: completiontool.Name,
				Arguments: `{
					"status":"complete",
					"summary":"Review complete.",
					"output_mode":"preserve_provisional",
					"pending_actions":[]
				}`,
			}},
			{Type: provider.EventMessageStop, StopReason: provider.StopReasonToolUse},
		}},
	}}
	engine := declarationEngine(t, runtime, registry, passedReceipt())

	result, err := engine.RunForTurnWithIntentAndAttachments(
		t.Context(), "turn-preserved-narration",
		"review R3, R4, and R5 evidence",
		protocol.TurnIntentPlan, nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	want := "The evidence review found three issues: R3, R4, and R5 are stale." +
		"\n\nReview complete."
	if result.State != Completed || result.Text != want {
		t.Fatalf("result=%+v want=%q", result, want)
	}
	if len(runtime.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(runtime.requests))
	}
}
