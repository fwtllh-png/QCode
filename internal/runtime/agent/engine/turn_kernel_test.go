package engine

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	providerfixture "github.com/fwtllh-png/QCode/internal/adapter/provider/fixture"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/interact"
	"github.com/fwtllh-png/QCode/internal/observability/trace"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestEngineRecoveryProjectsAlreadyTerminalKernelWithoutProvider(t *testing.T) {
	store := turnkernel.NewMemoryTerminalEnvelopeStore(nil, nil)
	coordinators, err := turnkernel.NewStoreCoordinatorRuntime(store)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := coordinators.Open(
		t.Context(),
		"terminal-recovery",
		turnkernel.NewState(protocol.TurnIntentAnswer, "act", 1),
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []turnkernel.Command{
		turnkernel.StartTurn{},
		turnkernel.PreparationFinished{},
		turnkernel.TerminalRequested{CancelReason: "client input EOF"},
		turnkernel.FinishTerminal{},
	} {
		if err := handle.Coordinator.Submit(t.Context(), command); err != nil {
			t.Fatal(err)
		}
	}
	if err := coordinators.Release(t.Context(), "terminal-recovery"); err != nil {
		t.Fatal(err)
	}

	second := newEngine(
		t,
		&scriptedProvider{},
		tool.NewRegistry(nil, nil),
	)
	journal := newTestWorkspaceJournal(t, t.TempDir())
	second.journal = journal
	second.options.Journal = journal
	second.options.TurnCoordinatorRuntime = coordinators
	var terminal Event
	result, err := second.RunForTurnWithIntentAndAttachments(
		t.Context(),
		"terminal-recovery",
		"must not sample",
		protocol.TurnIntentAnswer,
		nil,
		func(event Event) error {
			if event.State == Canceled {
				terminal = event
			}
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != Canceled || terminal.State != Canceled {
		t.Fatalf("recovered result = %+v, terminal = %+v", result, terminal)
	}
	if err := journal.Begin("next-turn"); err != nil {
		t.Fatalf("restored terminal leaked active journal: %v", err)
	}
	if _, err := journal.Rollback(t.Context(), "next-turn"); err != nil {
		t.Fatal(err)
	}
}

func TestEngineRecoveryFinalizesAcceptedCancellationWithoutProvider(t *testing.T) {
	store := turnkernel.NewMemoryTerminalEnvelopeStore(nil, nil)
	coordinators, err := turnkernel.NewStoreCoordinatorRuntime(store)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := coordinators.Open(
		t.Context(),
		"canceled-recovery",
		turnkernel.NewState(protocol.TurnIntentAnswer, "act", 1),
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []turnkernel.Command{
		turnkernel.StartTurn{},
		turnkernel.PreparationFinished{},
		turnkernel.ModelSampleRequested{SampleID: "sample"},
		turnkernel.CancelRequested{Reason: protocol.CancelReasonShutdown},
	} {
		if err := handle.Coordinator.Submit(t.Context(), command); err != nil {
			t.Fatal(err)
		}
	}
	if err := coordinators.Release(t.Context(), "canceled-recovery"); err != nil {
		t.Fatal(err)
	}

	providerRuntime := &scriptedProvider{}
	engine := newEngine(t, providerRuntime, tool.NewRegistry(nil, nil))
	engine.options.TurnCoordinatorRuntime = coordinators
	result, err := engine.RunForTurnWithIntentAndAttachments(
		t.Context(),
		"canceled-recovery",
		"must not sample",
		protocol.TurnIntentAnswer,
		nil,
		func(Event) error { return nil },
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("recovered cancellation error = %v", err)
	}
	if result.State != Canceled || len(providerRuntime.requests) != 0 {
		t.Fatalf(
			"recovered result = %+v, provider requests = %d",
			result,
			len(providerRuntime.requests),
		)
	}
	facts, loadErr := store.LoadDomainFacts(
		t.Context(),
		"canceled-recovery",
	)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	state := facts[len(facts)-1].State
	if state.Phase != turnkernel.PhaseCanceled ||
		len(state.PendingEffects) != 0 {
		t.Fatalf(
			"recovered terminal state = %+v; result=%+v err=%v",
			state,
			result,
			err,
		)
	}
}

func TestEngineRecoveryResumesRunningInputToolWithEarlyReply(t *testing.T) {
	store := turnkernel.NewMemoryTerminalEnvelopeStore(nil, nil)
	coordinators, err := turnkernel.NewStoreCoordinatorRuntime(store)
	if err != nil {
		t.Fatal(err)
	}
	host := interact.NewHost(time.Minute)
	registry := tool.NewRegistry(nil, nil)
	if err := interact.Register(registry, interact.Options{
		Host: host, Workspace: t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := registry.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	binding, ok := snapshot.Binding("request_user_input")
	if !ok {
		t.Fatal("request_user_input binding is unavailable")
	}
	handle, err := coordinators.Open(
		t.Context(),
		"input-recovery",
		turnkernel.NewState(protocol.TurnIntentAnswer, "act", 1),
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []turnkernel.Command{
		turnkernel.StartTurn{},
		turnkernel.PreparationFinished{},
	} {
		if err := handle.Coordinator.Submit(t.Context(), command); err != nil {
			t.Fatal(err)
		}
	}
	if err := coordinators.Release(t.Context(), "input-recovery"); err != nil {
		t.Fatal(err)
	}
	kernel, err := turnkernel.NewRuntimeKernel(
		turnkernel.KernelIdentity{
			TurnID: "input-recovery", ProfileRevision: 1,
		},
		protocol.TurnIntentAnswer,
		"act",
		nil,
		false,
		nil,
		kernelTransitionObserver(trace.NewRecorder(time.Now), 0),
		nil,
		nil,
		noopMetrics{},
		turnkernel.DefaultPolicy(),
		coordinators,
	)
	if err != nil {
		t.Fatal(err)
	}
	call := provider.ToolCall{
		ID: "call-input", Name: "request_user_input",
		Arguments:         `{"prompt":"continue?","options":["yes","no"]}`,
		CatalogID:         binding.CatalogID,
		CatalogGeneration: binding.Generation,
		CatalogRevision:   binding.Revision,
		CatalogAuthority:  binding.Authority,
	}
	if err := kernel.StartTools([]provider.ToolCall{call}); err != nil {
		t.Fatal(err)
	}
	if err := kernel.StartTool(call.ID); err != nil {
		t.Fatal(err)
	}
	if err := kernel.RequireInput("input-recovery-request", "call-input-recovery"); err != nil {
		t.Fatal(err)
	}
	if err := coordinators.Release(t.Context(), "input-recovery"); err != nil {
		t.Fatal(err)
	}

	runtime := &scriptedProvider{streams: []provider.Stream{
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventMessageStart},
			{Type: provider.EventTextDelta, Text: "done"},
			{Type: provider.EventMessageStop},
		}},
	}}
	engine := newEngine(t, runtime, registry)
	engine.options.InputHost = host
	engine.options.TurnCoordinatorRuntime = coordinators
	if err := engine.RestoreInputRequest(interact.Request{
		RequestID: "input-recovery-request",
		CallID:    "tool",
		Tool:      call.Name,
		Prompt:    "continue?",
		Options:   []string{"yes", "no"},
		ExpiresAt: time.Now().Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		result Result
		err    error
	}
	if !engine.queueRecoveredInput(interact.Reply{
		RequestID: "input-recovery-request",
		Answer:    "yes",
	}) {
		t.Fatal("recovered input reply was not queued")
	}
	done := make(chan outcome, 1)
	go func() {
		result, runErr := engine.RunForTurnWithIntentAndAttachments(
			t.Context(),
			"input-recovery",
			"must resume",
			protocol.TurnIntentAnswer,
			nil,
			func(Event) error { return nil },
		)
		done <- outcome{result: result, err: runErr}
	}()
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatal(got.err)
		}
		if got.result.State != Completed || got.result.Text != "done" {
			t.Fatalf("recovered result = %+v", got.result)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("recovered input turn did not complete")
	}
	facts, err := store.LoadDomainFacts(t.Context(), "input-recovery")
	if err != nil {
		t.Fatal(err)
	}
	state := facts[len(facts)-1].State
	if state.Phase != turnkernel.PhaseCompleted ||
		state.PendingInput != nil ||
		len(state.PendingEffects) != 0 {
		t.Fatalf("recovered input state = %+v", state)
	}
}

func TestEngineRunsTurnKernelObserverWithoutChangingResult(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		textStream("done"),
	}}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	var records []turnkernel.TransitionRecord
	engine.options.TurnKernelObserver = func(record turnkernel.TransitionRecord) {
		records = append(records, record)
	}

	result, err := engine.RunForTurnWithIntentAndAttachments(
		t.Context(),
		"kernel-turn",
		"analyze",
		protocol.TurnIntentAnswer,
		nil,
		func(Event) error { return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != Completed || result.Text != "done" {
		t.Fatalf("result = %+v", result)
	}
	if len(records) == 0 ||
		records[len(records)-1].To != turnkernel.PhaseCompleted {
		t.Fatalf("kernel records = %+v", records)
	}
	seen := make(map[string]bool)
	for _, record := range records {
		if record.Drift != "" || record.Rejection != "" {
			t.Fatalf("kernel diagnostic = %+v", record)
		}
		seen[record.Command] = true
	}
	for _, command := range []string{
		"model_sample_result_received",
		"evaluate_turn_step",
		"release_provisional_output",
	} {
		if !seen[command] {
			t.Fatalf("missing authoritative command %q in %+v", command, records)
		}
	}
}

func TestEngineMutationTurnHasNoKernelDecisionDrift(t *testing.T) {
	registry := declarationRegistry(t, false)
	runtime := &scriptedProvider{streams: []provider.Stream{
		toolCallStream("write-1", "write_fixture", `{}`),
		toolCallStream("complete-1", "turn_complete", `{
			"status":"complete",
			"summary":"implemented and verified",
			"pending_actions":[]
		}`),
	}}
	engine := declarationEngine(t, runtime, registry, passedReceipt())
	var records []turnkernel.TransitionRecord
	engine.options.TurnKernelObserver = func(record turnkernel.TransitionRecord) {
		records = append(records, record)
	}

	result, err := engine.RunForTurnWithIntentAndAttachments(
		t.Context(),
		"kernel-mutation",
		"change a.go",
		protocol.TurnIntentWorkspaceChange,
		nil,
		func(Event) error { return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != Completed || result.Text != "implemented and verified" {
		t.Fatalf("result = %+v", result)
	}
	if len(records) == 0 ||
		records[len(records)-1].To != turnkernel.PhaseCompleted {
		t.Fatalf("kernel records = %+v", records)
	}
	seen := make(map[string]bool)
	for _, record := range records {
		if record.Drift != "" || record.Rejection != "" {
			t.Fatalf("kernel diagnostic = %+v", record)
		}
		seen[record.Command] = true
	}
	for _, command := range []string{
		"completion_evaluated",
		"verification_started",
		"verification_finished",
		"model_sample_result_received",
		"evaluate_turn_step",
		"release_provisional_output",
	} {
		if !seen[command] {
			t.Fatalf("missing authoritative command %q in %+v", command, records)
		}
	}
}

func TestTurnKernelC2ToolResultsBypassObserver(t *testing.T) {
	var records []turnkernel.TransitionRecord
	kernel := newEngineTurnKernel(
		protocol.TurnIntentAnswer,
		"act",
		nil,
		0,
		func(record turnkernel.TransitionRecord) {
			records = append(records, record)
		},
		nil,
	)
	write := provider.ToolCall{ID: "write-1", Name: "file_write"}
	if err := kernel.StartTools([]provider.ToolCall{write}); err != nil {
		t.Fatal(err)
	}
	if err := kernel.StartTool(write.ID); err != nil {
		t.Fatal(err)
	}
	writeResult := tool.Result{Content: "written"}
	if err := kernel.CloseTool(
		write,
		writeResult,
		[]tool.WorkspaceChange{{
			Path: "a.go", Kind: tool.WorkspaceModified,
		}},
	); err != nil {
		t.Fatal(err)
	}
	complete := provider.ToolCall{ID: "complete-1", Name: "turn_complete"}
	if err := kernel.StartTools([]provider.ToolCall{complete}); err != nil {
		t.Fatal(err)
	}
	if err := kernel.StartTool(complete.ID); err != nil {
		t.Fatal(err)
	}
	completeResult := tool.Result{
		Content: `{"status":"accepted"}`,
		Outcome: &tool.Outcome{Facts: &tool.OutcomeFacts{
			Completion: &tool.CompletionDeclaration{
				Status: "complete", Summary: "implemented",
				ChangedPaths: []string{"a.go"}, MutationRevision: 1,
				CallID: "complete-1",
			},
		}},
		Metadata: map[string]any{
			"completion_declaration_accepted": true,
		},
	}
	if err := kernel.CloseTool(complete, completeResult, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := kernel.EvaluateCompletion(turnkernel.CompletionCandidate{
		DeclarationValid: true,
		Status:           "complete",
		Summary:          "implemented",
		CompletionCall:   complete.ID,
		BatchSize:        1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := kernel.BeginVerification(); err != nil {
		t.Fatal(err)
	}
	if _, err := kernel.FinishVerification(turnkernel.VerificationFinished{
		Status: turnkernel.VerificationPassed,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := kernel.ReleaseOutput(); err != nil {
		t.Fatal(err)
	}
	finalizeKernelForTest(t, kernel, turnkernel.TerminalDecision{
		Kind: turnkernel.TerminalCompleted,
	})

	assertKernelHealthy(t, kernel, records, turnkernel.PhaseCompleted)
	if kernel.Snapshot().Journal != turnkernel.JournalCommitted ||
		kernel.Snapshot().MutationRevision != 1 {
		t.Fatalf("kernel state = %+v", kernel.Snapshot())
	}
}

func TestTurnKernelC2TerminalFailureClosesParallelTools(t *testing.T) {
	var records []turnkernel.TransitionRecord
	kernel := newEngineTurnKernel(
		protocol.TurnIntentAnswer,
		"act",
		nil,
		0,
		func(record turnkernel.TransitionRecord) {
			records = append(records, record)
		},
		nil,
	)
	first := provider.ToolCall{ID: "call-1", Name: "first"}
	second := provider.ToolCall{ID: "call-2", Name: "second"}
	if err := kernel.StartTools([]provider.ToolCall{first, second}); err != nil {
		t.Fatal(err)
	}
	if err := kernel.StartTool(first.ID); err != nil {
		t.Fatal(err)
	}
	if err := kernel.StartTool(second.ID); err != nil {
		t.Fatal(err)
	}
	if err := kernel.RequireApproval("approval-1", first.ID); err != nil {
		t.Fatal(err)
	}
	finalizeKernelForTest(t, kernel, turnkernel.TerminalDecision{
		Kind: turnkernel.TerminalFailed, Code: string(protocol.CodeConflict),
		Message: "batch failed",
	})

	assertKernelHealthy(t, kernel, records, turnkernel.PhaseFailed)
	if len(kernel.Snapshot().ClosedCalls) != 2 {
		t.Fatalf("closed calls = %+v", kernel.Snapshot().ClosedCalls)
	}
}

func TestTurnKernelOwnsToolStartAndResultRegistration(t *testing.T) {
	var records []turnkernel.TransitionRecord
	kernel := newEngineTurnKernel(
		protocol.TurnIntentAnswer,
		"act",
		nil,
		0,
		func(record turnkernel.TransitionRecord) {
			records = append(records, record)
		},
		nil,
	)
	call := provider.ToolCall{ID: "call-1", Name: "read"}
	if err := kernel.StartTools([]provider.ToolCall{call}); err != nil {
		t.Fatal(err)
	}
	if err := kernel.StartTool(call.ID); err != nil {
		t.Fatal(err)
	}
	result := tool.Result{Content: "done"}
	if err := kernel.CloseTool(call, result, nil); err != nil {
		t.Fatal(err)
	}
	if len(kernel.Snapshot().OpenCalls) != 0 ||
		len(kernel.Snapshot().ClosedCalls) != 1 {
		t.Fatalf("tool ledger = %+v", kernel.Snapshot())
	}
}

func TestTurnKernelToolBatchRejectsDuplicateIdentityBeforeExecution(t *testing.T) {
	var records []turnkernel.TransitionRecord
	kernel := newEngineTurnKernel(
		protocol.TurnIntentAnswer,
		"act",
		nil,
		0,
		func(record turnkernel.TransitionRecord) {
			records = append(records, record)
		},
		nil,
	)
	err := kernel.StartTools([]provider.ToolCall{
		{ID: "duplicate", Name: "first"},
		{ID: "duplicate", Name: "second"},
	})
	if err == nil || protocol.CodeOf(err) != protocol.CodeConflict {
		t.Fatalf("startTools() error = %v", err)
	}
	if len(kernel.Snapshot().OpenCalls) != 0 ||
		kernel.Snapshot().Phase != turnkernel.PhaseSampling {
		t.Fatalf("rejected batch changed state: %+v", kernel.Snapshot())
	}
	last := records[len(records)-1]
	if last.Drift != "" || last.Rejection == "" {
		t.Fatalf("authoritative rejection = %+v", last)
	}
}

func TestTurnKernelAbortClosesEveryOpenTool(t *testing.T) {
	kernel := newEngineTurnKernel(
		protocol.TurnIntentAnswer,
		"act",
		nil,
		0,
		nil,
		nil,
	)
	if err := kernel.StartTools([]provider.ToolCall{
		{ID: "first", Name: "read"},
		{ID: "second", Name: "search"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := kernel.AbortTools("turn canceled"); err != nil {
		t.Fatal(err)
	}
	if len(kernel.Snapshot().OpenCalls) != 0 ||
		len(kernel.Snapshot().ClosedCalls) != 2 ||
		kernel.Snapshot().Phase != turnkernel.PhaseSampling {
		t.Fatalf("aborted tool ledger = %+v", kernel.Snapshot())
	}
	for _, result := range kernel.Snapshot().ClosedCalls {
		if !result.IsError {
			t.Fatalf("aborted result = %+v", result)
		}
	}
}

func TestTurnKernelAbortClosesToolAwaitingApproval(t *testing.T) {
	kernel := newEngineTurnKernel(
		protocol.TurnIntentWorkspaceChange,
		"act",
		nil,
		0,
		nil,
		nil,
	)
	call := provider.ToolCall{ID: "edit", Name: "file_edit"}
	if err := kernel.StartTools([]provider.ToolCall{call}); err != nil {
		t.Fatal(err)
	}
	if err := kernel.StartTool(call.ID); err != nil {
		t.Fatal(err)
	}
	if err := kernel.RequireApproval("approval-edit", call.ID); err != nil {
		t.Fatal(err)
	}
	if err := kernel.CloseTool(
		call,
		tool.Result{Content: "push interrupted", IsError: true},
		nil,
	); err == nil || !strings.Contains(err.Error(), "pending approval") {
		t.Fatalf("premature tool result error = %v", err)
	}

	if err := kernel.AbortTools(protocol.CancelReasonHostInterrupted); err != nil {
		t.Fatal(err)
	}
	state := kernel.Snapshot()
	if len(state.OpenCalls) != 0 ||
		len(state.PendingApprovals) != 0 ||
		len(state.PendingEffects) != 0 ||
		len(state.ClosedCalls) != 1 {
		t.Fatalf("aborted approval ledger = %+v", state)
	}
}

func TestTurnKernelCancellationClosesToolAwaitingApproval(t *testing.T) {
	var records []turnkernel.TransitionRecord
	kernel := newEngineTurnKernel(
		protocol.TurnIntentWorkspaceChange,
		"act",
		nil,
		0,
		func(record turnkernel.TransitionRecord) {
			records = append(records, record)
		},
		nil,
	)
	call := provider.ToolCall{ID: "edit", Name: "file_edit"}
	if err := kernel.StartTools([]provider.ToolCall{call}); err != nil {
		t.Fatal(err)
	}
	if err := kernel.StartTool(call.ID); err != nil {
		t.Fatal(err)
	}
	if err := kernel.RequireApproval("approval-edit", call.ID); err != nil {
		t.Fatal(err)
	}
	if err := kernel.RequestCancel(protocol.CancelReasonHostInterrupted); err != nil {
		t.Fatal(err)
	}
	if err := kernel.CloseTool(
		call,
		tool.Result{Content: "tool aborted: context canceled", IsError: true},
		nil,
	); err != nil {
		t.Fatal(err)
	}
	finalizeKernelForTest(t, kernel, turnkernel.TerminalDecision{
		Kind: turnkernel.TerminalCanceled, Message: protocol.CancelReasonHostInterrupted,
	})
	assertKernelHealthy(t, kernel, records, turnkernel.PhaseCanceled)
	if len(kernel.Snapshot().OpenCalls) != 0 ||
		len(kernel.Snapshot().PendingApprovals) != 0 ||
		len(kernel.Snapshot().PendingEffects) != 0 ||
		len(kernel.Snapshot().ClosedCalls) != 1 {
		t.Fatalf("canceled approval ledger = %+v", kernel.Snapshot())
	}
}

func TestTurnKernelCancellationClosesToolAwaitingInput(t *testing.T) {
	var records []turnkernel.TransitionRecord
	kernel := newEngineTurnKernel(
		protocol.TurnIntentAnswer,
		"act",
		nil,
		0,
		func(record turnkernel.TransitionRecord) {
			records = append(records, record)
		},
		nil,
	)
	call := provider.ToolCall{ID: "interactive", Name: "request_input"}
	if err := kernel.StartTools([]provider.ToolCall{call}); err != nil {
		t.Fatal(err)
	}
	if err := kernel.StartTool(call.ID); err != nil {
		t.Fatal(err)
	}
	if err := kernel.RequireInput("input-1", "call-input-1"); err != nil {
		t.Fatal(err)
	}
	if effect, started, err := kernel.RoutedEffect(
		turnkernel.EffectAwaitInput,
		"",
	); err != nil || !started || effect.Status != turnkernel.EffectRunning {
		t.Fatalf("input wait effect was not started before waiting: effect=%+v started=%v err=%v", effect, started, err)
	}
	finalizeKernelForTest(t, kernel, turnkernel.TerminalDecision{
		Kind: turnkernel.TerminalCanceled, Message: "turn canceled",
	})
	assertKernelHealthy(t, kernel, records, turnkernel.PhaseCanceled)
	if len(kernel.Snapshot().OpenCalls) != 0 ||
		len(kernel.Snapshot().ClosedCalls) != 1 {
		t.Fatalf("canceled input ledger = %+v", kernel.Snapshot())
	}
	if pending := kernel.PendingRouted(
		turnkernel.EffectAwaitInput,
	); len(pending) != 0 {
		t.Fatalf("canceled input dispatcher entries = %+v", pending)
	}
}

func TestTurnKernelAcceptsApprovalResultBeforeResolution(t *testing.T) {
	kernel := newEngineTurnKernel(
		protocol.TurnIntentAnswer,
		"act",
		nil,
		0,
		nil,
		nil,
	)
	call := provider.ToolCall{ID: "approval-call", Name: "write"}
	if err := kernel.StartTools([]provider.ToolCall{call}); err != nil {
		t.Fatal(err)
	}
	if err := kernel.StartTool(call.ID); err != nil {
		t.Fatal(err)
	}
	if err := kernel.RequireApproval("approval-1", call.ID); err != nil {
		t.Fatal(err)
	}
	if effect, started, err := kernel.RoutedEffect(
		turnkernel.EffectAwaitApproval,
		call.ID,
	); err != nil || !started || effect.Status != turnkernel.EffectRunning {
		t.Fatalf("approval wait effect was not started before waiting: effect=%+v started=%v err=%v", effect, started, err)
	}
	if err := kernel.ResolveApproval("approval-1", false); err != nil {
		t.Fatal(err)
	}
	if len(kernel.Snapshot().PendingApprovals) != 0 {
		t.Fatalf("pending approvals = %+v", kernel.Snapshot().PendingApprovals)
	}
	for _, effect := range kernel.Snapshot().CompletedEffects {
		if effect.Kind == turnkernel.EffectAwaitApproval &&
			effect.Status != turnkernel.EffectSucceeded {
			t.Fatalf("approval effect = %+v", effect)
		}
	}
}

func TestTurnKernelSerializesDuplicateApprovalResults(t *testing.T) {
	kernel := newEngineTurnKernel(
		protocol.TurnIntentAnswer,
		"act",
		nil,
		0,
		nil,
		nil,
	)
	call := provider.ToolCall{ID: "approval-call", Name: "write"}
	if err := kernel.StartTools([]provider.ToolCall{call}); err != nil {
		t.Fatal(err)
	}
	if err := kernel.StartTool(call.ID); err != nil {
		t.Fatal(err)
	}
	if err := kernel.RequireApproval("approval-1", call.ID); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	var group sync.WaitGroup
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			results <- kernel.ResolveApproval("approval-1", false)
		}()
	}
	group.Wait()
	close(results)
	var accepted, rejected int
	for err := range results {
		if err == nil {
			accepted++
		} else {
			rejected++
		}
	}
	if accepted != 1 || rejected != 1 {
		t.Fatalf("approval results: accepted=%d rejected=%d", accepted, rejected)
	}
}

func TestTurnKernelAcceptsInputResultBeforeResolution(t *testing.T) {
	kernel := newEngineTurnKernel(
		protocol.TurnIntentAnswer,
		"act",
		nil,
		0,
		nil,
		nil,
	)
	call := provider.ToolCall{ID: "input-call", Name: "request_user_input"}
	if err := kernel.StartTools([]provider.ToolCall{call}); err != nil {
		t.Fatal(err)
	}
	if err := kernel.StartTool(call.ID); err != nil {
		t.Fatal(err)
	}
	if err := kernel.RequireInput("input-1", "call-input-1"); err != nil {
		t.Fatal(err)
	}
	if effect, started, err := kernel.RoutedEffect(
		turnkernel.EffectAwaitInput,
		"",
	); err != nil || !started || effect.Status != turnkernel.EffectRunning {
		t.Fatalf("input wait effect was not started before waiting: effect=%+v started=%v err=%v", effect, started, err)
	}
	if err := kernel.ResolveInput("input-1"); err != nil {
		t.Fatal(err)
	}
	if kernel.Snapshot().PendingInput != nil {
		t.Fatalf("pending input = %+v", kernel.Snapshot().PendingInput)
	}
	for _, effect := range kernel.Snapshot().CompletedEffects {
		if effect.Kind == turnkernel.EffectAwaitInput &&
			effect.Status != turnkernel.EffectSucceeded {
			t.Fatalf("input effect = %+v", effect)
		}
	}
}

func TestTurnKernelRejectsNewWorkAfterAcceptedCancel(t *testing.T) {
	kernel := newEngineTurnKernel(
		protocol.TurnIntentAnswer,
		"act",
		nil,
		0,
		nil,
		nil,
	)
	if err := kernel.RequestCancel(protocol.CancelReasonUserInterrupted); err != nil {
		t.Fatal(err)
	}
	if !kernel.Snapshot().Cancellation.Accepted {
		t.Fatalf("cancellation = %+v", kernel.Snapshot().Cancellation)
	}
	if err := kernel.StartTools([]provider.ToolCall{{
		ID:   "late-tool",
		Name: "read",
	}}); err == nil {
		t.Fatal("tool start succeeded after accepted cancel")
	}
	if len(kernel.Snapshot().OpenCalls) != 0 ||
		len(kernel.Snapshot().PendingEffects) != 0 {
		t.Fatalf("post-cancel work leaked: %+v", kernel.Snapshot())
	}
}

func TestTurnKernelCancellationClosesRunningProviderBeforeTerminal(t *testing.T) {
	kernel := newEngineTurnKernel(
		protocol.TurnIntentAnswer,
		"act",
		nil,
		0,
		nil,
		nil,
	)
	if err := kernel.BeginModelSample(t.Context(), "sample"); err != nil {
		t.Fatal(err)
	}
	if err := kernel.RequestCancel(protocol.CancelReasonShutdown); err != nil {
		t.Fatal(err)
	}

	finalizeKernelForTest(t, kernel, turnkernel.TerminalDecision{
		Kind:    turnkernel.TerminalCanceled,
		Message: protocol.CancelReasonShutdown,
	})

	if kernel.Snapshot().Phase != turnkernel.PhaseCanceled ||
		len(kernel.Snapshot().PendingEffects) != 0 ||
		kernel.Snapshot().ActiveSampleID != "" {
		t.Fatalf("canceled provider state = %+v", kernel.Snapshot())
	}
	sample := kernel.Snapshot().SampleLedger["sample"]
	if sample.Status != turnkernel.SampleFailed ||
		!strings.Contains(sample.Error, protocol.CancelReasonShutdown) {
		t.Fatalf("canceled sample = %+v", sample)
	}
}

func finalizeKernelForTest(
	t *testing.T,
	kernel *turnkernel.RuntimeKernel,
	decision turnkernel.TerminalDecision,
) {
	t.Helper()
	if decision.Kind != turnkernel.TerminalCompleted {
		if err := kernel.AbortForTerminal(decision.Message); err != nil {
			t.Fatal(err)
		}
	}
	request := turnkernel.TerminalRequested{}
	if decision.Kind == turnkernel.TerminalCanceled {
		request.CancelReason = decision.Message
	} else if decision.Kind == turnkernel.TerminalFailed {
		request.FailureCode = decision.Code
		request.FailureMessage = decision.Message
	}
	if _, err := kernel.RequestTerminal(request); err != nil {
		t.Fatal(err)
	}
	if kind, ok := kernel.JournalEffectKind(); ok {
		effect, err := kernel.StartJournal(kind)
		if err != nil {
			t.Fatal(err)
		}
		status := turnkernel.JournalRolledBack
		if kind == turnkernel.EffectCommitJournal {
			status = turnkernel.JournalCommitted
		}
		if err := kernel.FinishJournal(effect, status, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := kernel.FinishTerminal(); err != nil {
		t.Fatal(err)
	}
}

func assertKernelHealthy(
	t *testing.T,
	kernel *turnkernel.RuntimeKernel,
	records []turnkernel.TransitionRecord,
	want turnkernel.Phase,
) {
	t.Helper()
	if kernel.Snapshot().Phase != want {
		t.Fatalf("phase = %s, want %s", kernel.Snapshot().Phase, want)
	}
	for _, record := range records {
		if record.Drift != "" || record.StateDigest == "" {
			t.Fatalf("record = %+v", record)
		}
	}
}
