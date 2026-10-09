package guard

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/interact"
	"github.com/fwtllh-png/QCode/internal/security/authority"
	"github.com/fwtllh-png/QCode/internal/security/policy"
)

func TestSubmittedPlanUnlocksAutoApprovedActTurn(t *testing.T) {
	workspace := t.TempDir()
	registry := tool.NewRegistry(nil, nil)
	if err := interact.Register(registry, interact.Options{
		Host: interact.NewHost(0), Workspace: workspace,
	}); err != nil {
		t.Fatal(err)
	}
	write := &testExecutor{descriptor: writeDescriptor()}
	if err := registry.Register(write); err != nil {
		t.Fatal(err)
	}
	runtime := policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass)
	runtime.ConfigurePlanning(policy.PlanningRequired)
	guard, err := New(Options{
		Registry: registry, Policy: runtime, Workspace: workspace,
	})
	if err != nil {
		t.Fatal(err)
	}
	writeArgs := json.RawMessage(`{"path":"target.txt","value":"updated"}`)
	if _, err := guard.Execute(t.Context(), "write-before-plan", "write", writeArgs); err == nil ||
		!strings.Contains(err.Error(), "plan_required") {
		t.Fatalf("write before plan error = %v", err)
	}
	plan := json.RawMessage(`{"version":1,"objective":"Update target","steps":[` +
		`{"id":"edit","title":"Edit target","affected_files":["target.txt"]}]}`)
	if _, err := guard.Execute(t.Context(), "submit-plan", "submit_plan", plan); err != nil {
		t.Fatal(err)
	}
	if _, err := guard.Execute(t.Context(), "write-after-plan", "write", writeArgs); err != nil {
		t.Fatalf("write after plan: %v", err)
	}
	if write.calls.Load() != 1 {
		t.Fatalf("write calls = %d, want 1", write.calls.Load())
	}
}

func TestApprovalWaitHoldsNeitherAdmissionNorClaims(t *testing.T) {
	descriptor := readDescriptor("approval_release")
	descriptor.Capability = tool.CapabilityProcess
	descriptor.AccessMode = tool.AccessWrite
	descriptor.ParallelPolicy = tool.ParallelSerial
	executor := &testExecutor{descriptor: descriptor}
	registry := newTestRegistry(t, nil, executor)
	runtime := policy.DefaultRuntime(policy.ModeAct, policy.PermissionSuggest)
	runtime.DisableAutoReview = true
	requests := make(chan ApprovalRequest, 1)
	guard, err := New(Options{
		Registry: registry, Policy: runtime, Workspace: t.TempDir(),
		Approvals: func(_ context.Context, request ApprovalRequest) error {
			requests <- request
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var active atomic.Int32
	ctx := tool.WithExecutionAdmission(
		t.Context(),
		func(context.Context, tool.ParallelPolicy) (func(), error) {
			active.Add(1)
			return func() { active.Add(-1) }, nil
		},
	)
	done := make(chan error, 1)
	go func() {
		_, executeErr := guard.Execute(
			ctx, "call-approval-release", "approval_release", json.RawMessage(`{}`),
		)
		done <- executeErr
	}()
	var request ApprovalRequest
	select {
	case request = <-requests:
	case executeErr := <-done:
		t.Fatalf("process smoke ended before approval: %v", executeErr)
	}
	assertApprovalResourcesReleased(t, registry, request, &active)
	mustDecide(t, guard, request, policy.ApprovalOnce, nil)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if active.Load() != 0 {
		t.Fatalf("active admissions = %d", active.Load())
	}
}

func TestHostProcessApprovalIsFreshOnce(t *testing.T) {
	descriptor := readDescriptor("fixture_host_process")
	descriptor.Capability = tool.CapabilityProcess
	descriptor.AccessMode = tool.AccessWrite
	binding := tool.TrustedBindingFromDescriptor(descriptor)
	binding.Effect = tool.EffectContract{
		Mode: tool.EffectFixed, Kind: tool.EffectProcessMutating,
		Risk: tool.RiskHigh, Reversibility: tool.Bounded,
		WorkspaceTransaction: tool.TransactionNone,
		Approval:             tool.ApprovalPolicyOnce,
	}
	executor := &testExecutor{descriptor: descriptor, binding: binding}
	registry := newTestRegistry(t, nil, executor)
	requests := make(chan ApprovalRequest, 1)
	guard, err := New(Options{
		Registry: registry,
		Policy: policy.DefaultRuntime(
			policy.ModeAct,
			policy.PermissionBypass,
		),
		Workspace: t.TempDir(),
		Approvals: func(_ context.Context, request ApprovalRequest) error {
			requests <- request
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, executeErr := guard.Execute(
			context.Background(),
			"process-smoke",
			"fixture_host_process",
			json.RawMessage(`{}`),
		)
		done <- executeErr
	}()
	var request ApprovalRequest
	select {
	case request = <-requests:
	case executeErr := <-done:
		t.Fatalf("process smoke ended before approval: %v", executeErr)
	}
	if request.ReasonCode != "host_process_approval_required" ||
		request.ReplacementAllowed ||
		len(request.AllowedScopes) != 1 ||
		request.AllowedScopes[0] != policy.ApprovalOnce {
		t.Fatalf("host process approval = %+v", request)
	}
	mustDecide(t, guard, request, policy.ApprovalOnce, nil)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestOneShotApprovalRechecksPermissionBeforeExecution(t *testing.T) {
	descriptor := readDescriptor("fixture_host_process")
	descriptor.Capability = tool.CapabilityProcess
	descriptor.AccessMode = tool.AccessWrite
	binding := tool.TrustedBindingFromDescriptor(descriptor)
	binding.Effect = tool.EffectContract{
		Mode: tool.EffectFixed, Kind: tool.EffectProcessMutating,
		Risk: tool.RiskHigh, Reversibility: tool.Bounded,
		WorkspaceTransaction: tool.TransactionNone, Approval: tool.ApprovalPolicyOnce,
	}
	executor := &testExecutor{descriptor: descriptor, binding: binding}
	registry := newTestRegistry(t, nil, executor)
	runtime := policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass)
	requests := make(chan ApprovalRequest, 1)
	guarded := newTestGuard(t, registry, runtime, func(_ context.Context, request ApprovalRequest) error {
		requests <- request
		return nil
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := guarded.Execute(ctx, "changed-permission", "fixture_host_process", json.RawMessage(`{}`))
		done <- err
	}()
	select {
	case request := <-requests:
		runtime.SetPermission(policy.PermissionNever)
		mustDecide(t, guarded, request, policy.ApprovalOnce, nil)
	case err := <-done:
		t.Fatalf("ended before approval: %v", err)
	}
	if err := <-done; err == nil || !strings.Contains(err.Error(), "permission_denied") {
		t.Fatalf("changed permission must reject stale one-shot approval: %v", err)
	}
	if executor.calls.Load() != 0 {
		t.Fatal("executed after permission was tightened")
	}
}

func TestInitialApprovalDenialReturnsGuardTerminalReceipt(t *testing.T) {
	descriptor := readDescriptor("approval_denied_receipt")
	descriptor.Capability = tool.CapabilityProcess
	descriptor.AccessMode = tool.AccessWrite
	registry := newTestRegistry(
		t, nil, &testExecutor{descriptor: descriptor},
	)
	runtime := policy.DefaultRuntime(policy.ModeAct, policy.PermissionSuggest)
	runtime.DisableAutoReview = true
	requests := make(chan ApprovalRequest, 1)
	guard, err := New(Options{
		Registry: registry, Policy: runtime, Workspace: t.TempDir(),
		Approvals: func(_ context.Context, request ApprovalRequest) error {
			requests <- request
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	type execution struct {
		result tool.Result
		err    error
	}
	done := make(chan execution, 1)
	go func() {
		result, executeErr := guard.Execute(
			t.Context(), "call-denied-receipt", "approval_denied_receipt",
			json.RawMessage(`{}`),
		)
		done <- execution{result: result, err: executeErr}
	}()
	request := <-requests
	if err := guard.Decide(ApprovalDecision{RequestID: request.RequestID}); err != nil {
		t.Fatal(err)
	}
	out := <-done
	if out.err == nil || out.result.Execution == nil || !out.result.IsError {
		t.Fatalf("denied result = %+v error=%v", out.result, out.err)
	}
	receipt := out.result.Execution
	if len(receipt.Attempts) != 0 ||
		receipt.Tool.Name != "approval_denied_receipt" ||
		receipt.TerminalStatus != tool.OutcomeRejected ||
		receipt.TerminalOwner != tool.TerminalOwnerGuard {
		t.Fatalf("denied receipt = %+v", receipt)
	}
}

func TestAdditionalPermissionApprovalReleasesAdmissionAndClaims(t *testing.T) {
	executor := &escalateExecutor{descriptor: sandboxedDescriptor("retry_release")}
	registry := newTestRegistry(t, strongBackend{}, executor)
	runtime := policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass)
	requests := make(chan ApprovalRequest, 1)
	guard, err := New(Options{
		Registry: registry, Policy: runtime, Workspace: t.TempDir(),
		Approvals: func(_ context.Context, request ApprovalRequest) error {
			requests <- request
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var active atomic.Int32
	ctx := tool.WithExecutionAdmission(
		t.Context(),
		func(context.Context, tool.ParallelPolicy) (func(), error) {
			active.Add(1)
			return func() { active.Add(-1) }, nil
		},
	)
	type execution struct {
		result tool.Result
		err    error
	}
	done := make(chan execution, 1)
	go func() {
		result, executeErr := guard.Execute(
			ctx, "call-retry-release", "retry_release", json.RawMessage(`{}`),
		)
		done <- execution{result: result, err: executeErr}
	}()
	escalation := <-requests
	if escalation.ReasonCode != ApprovalReasonAdditionalPermission {
		t.Fatalf("escalation reason = %q", escalation.ReasonCode)
	}
	assertApprovalResourcesReleased(t, registry, escalation, &active)
	mustDecide(t, guard, escalation, policy.ApprovalOnce, nil)
	out := <-done
	if out.err != nil {
		t.Fatal(out.err)
	}
	if out.result.Execution == nil ||
		len(out.result.Execution.Attempts) != 2 ||
		out.result.Execution.Attempts[0].Sandbox != string(SandboxModeStrong) ||
		out.result.Execution.Attempts[1].Sandbox != string(SandboxModeStrong) ||
		out.result.Execution.Disposition != tool.DispositionWaitForTeardown ||
		out.result.Execution.TerminalStatus != tool.OutcomeSucceeded ||
		out.result.Execution.TerminalOwner != tool.TerminalOwnerExecutor ||
		out.result.Execution.Tool.Validate() != nil {
		t.Fatalf("execution receipt = %+v", out.result.Execution)
	}
	first, second := out.result.Execution.Attempts[0], out.result.Execution.Attempts[1]
	if first.PermissionRevision != 1 || second.PermissionRevision != 2 ||
		first.PermissionDigest == "" || second.PermissionDigest == "" ||
		first.PermissionDigest == second.PermissionDigest ||
		first.OperationDigest == "" || second.OperationDigest == "" ||
		first.OperationDigest == second.OperationDigest ||
		first.LeaseID == "" || second.LeaseID == "" ||
		first.LeaseID == second.LeaseID ||
		first.LeaseState != string(authority.LeaseSettled) ||
		second.LeaseState != string(authority.LeaseSettled) ||
		first.LeaseAttempt != 1 || second.LeaseAttempt != 2 ||
		first.PolicyRevision == 0 ||
		first.WorkspaceID == "" || first.WorkspaceGeneration == 0 ||
		first.SubjectDigest == "" || first.SubjectGeneration == 0 ||
		first.EffectKind == "" || first.EffectRisk == "" ||
		first.Enforcement != "strong" || second.Enforcement != "strong" ||
		first.Backend != "test" || second.Backend != "test" ||
		first.NetworkMode != "denied" || second.NetworkMode != "denied" ||
		first.WorkspaceRoot == "" || len(first.ReadRoots) == 0 {
		t.Fatalf("attempt authority receipts = %+v", out.result.Execution.Attempts)
	}
	if first.Denial == nil ||
		first.Denial.ReasonCode != "path_write_not_authorized" ||
		first.Amendment == nil ||
		first.Amendment.Decision != "approved" ||
		first.Amendment.BasePermissionDigest != first.PermissionDigest ||
		first.Amendment.AmendedPermissionDigest != second.PermissionDigest ||
		!receiptHasProvenance(first, "grant") ||
		!receiptHasProvenance(second, "amendment") {
		t.Fatalf("attempt authority chain = %+v", out.result.Execution.Attempts)
	}
	if active.Load() != 0 {
		t.Fatalf("active admissions = %d", active.Load())
	}
}

func TestAdditionalPermissionRetryRejectsChangedAuthorization(t *testing.T) {
	executor := &escalateExecutor{
		descriptor: sandboxedDescriptor("retry_reauthorize"),
	}
	registry := newTestRegistry(t, strongBackend{}, executor)
	requests := make(chan ApprovalRequest, 1)
	guard, err := New(Options{
		Registry: registry,
		Policy: policy.DefaultRuntime(
			policy.ModeAct,
			policy.PermissionBypass,
		),
		Workspace: t.TempDir(),
		Approvals: func(_ context.Context, request ApprovalRequest) error {
			requests <- request
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	type execution struct {
		result tool.Result
		err    error
	}
	done := make(chan execution, 1)
	go func() {
		result, executeErr := guard.Execute(
			t.Context(),
			"call-retry-reauthorize",
			"retry_reauthorize",
			json.RawMessage(`{}`),
		)
		done <- execution{result: result, err: executeErr}
	}()
	request := <-requests
	if request.ReasonCode != ApprovalReasonAdditionalPermission {
		t.Fatalf("approval reason = %q", request.ReasonCode)
	}
	guard.SwapPolicy(
		policy.DefaultRuntime(policy.ModeAct, policy.PermissionNever),
	)
	mustDecide(t, guard, request, policy.ApprovalOnce, nil)

	out := <-done
	var denied *policy.DecisionError
	if !errors.As(out.err, &denied) ||
		denied.Code != "authorization_changed" {
		t.Fatalf("result=%+v error=%v", out.result, out.err)
	}
	if executor.calls.Load() != 1 {
		t.Fatalf("executor attempts = %d, want 1", executor.calls.Load())
	}
	receipt := out.result.Execution
	if receipt == nil ||
		len(receipt.Attempts) != 1 ||
		receipt.Attempts[0].Amendment == nil ||
		receipt.Attempts[0].Amendment.Decision != "rejected" ||
		receipt.TerminalStatus != tool.OutcomeRejected ||
		receipt.TerminalOwner != tool.TerminalOwnerGuard {
		t.Fatalf("execution receipt = %+v", receipt)
	}
}

func receiptHasProvenance(receipt tool.AttemptReceipt, kind string) bool {
	for _, source := range receipt.Provenance {
		if source.Kind == kind {
			return true
		}
	}
	return false
}

func TestRejectedAdditionalPermissionRetainsAuthorityEvidence(t *testing.T) {
	executor := &escalateExecutor{descriptor: sandboxedDescriptor("retry_rejected")}
	registry := newTestRegistry(t, strongBackend{}, executor)
	requests := make(chan ApprovalRequest, 1)
	guard, err := New(Options{
		Registry:  registry,
		Policy:    policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass),
		Workspace: t.TempDir(),
		Approvals: func(_ context.Context, request ApprovalRequest) error {
			requests <- request
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	type execution struct {
		result tool.Result
		err    error
	}
	done := make(chan execution, 1)
	go func() {
		result, executeErr := guard.Execute(
			t.Context(), "call-retry-rejected", "retry_rejected", json.RawMessage(`{}`),
		)
		done <- execution{result: result, err: executeErr}
	}()
	request := <-requests
	if err := guard.Decide(ApprovalDecision{RequestID: request.RequestID}); err != nil {
		t.Fatal(err)
	}
	out := <-done
	if out.err == nil || out.result.Execution == nil ||
		len(out.result.Execution.Attempts) != 1 {
		t.Fatalf("rejected execution = %+v error=%v", out.result.Execution, out.err)
	}
	attempt := out.result.Execution.Attempts[0]
	if attempt.PermissionDigest == "" || attempt.Denial == nil ||
		attempt.Amendment == nil ||
		attempt.Amendment.Decision != "rejected" ||
		attempt.Amendment.BasePermissionDigest != attempt.PermissionDigest ||
		attempt.Amendment.AmendedPermissionDigest != "" ||
		out.result.Execution.TerminalStatus != tool.OutcomeRejected ||
		out.result.Execution.TerminalOwner != tool.TerminalOwnerGuard {
		t.Fatalf("rejected authority receipt = %+v", out.result.Execution)
	}
}

func TestReplacementArgumentsRepeatPreparationAndAuthorization(t *testing.T) {
	executor := &testExecutor{descriptor: writeDescriptor()}
	registry := newTestRegistry(t, nil, executor)
	runtime := policy.DefaultRuntime(policy.ModeAct, policy.PermissionSuggest)
	runtime.DisableAutoReview = true
	requests := make(chan ApprovalRequest, 1)
	guard, err := New(Options{
		Registry: registry, Policy: runtime, Workspace: t.TempDir(),
		Approvals: func(_ context.Context, request ApprovalRequest) error {
			requests <- request
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	type execution struct {
		result tool.Result
		err    error
	}
	done := make(chan execution, 1)
	go func() {
		result, executeErr := guard.Execute(
			t.Context(),
			"call-replacement",
			"write",
			json.RawMessage(`{"path":"a.txt","value":"before"}`),
		)
		done <- execution{result: result, err: executeErr}
	}()
	request := <-requests
	mustDecide(
		t,
		guard,
		request,
		policy.ApprovalOnce,
		json.RawMessage(`{"path":"b.txt","value":"after"}`),
	)
	out := <-done
	if out.err != nil {
		t.Fatal(out.err)
	}
	if out.result.Content != `{"path":"b.txt","value":"after"}` {
		t.Fatalf("executed arguments = %s", out.result.Content)
	}
	profiles := executor.profilesSnapshot()
	if len(profiles) != 1 ||
		len(profiles[0].WorkspaceWritePaths) != 1 ||
		!strings.HasSuffix(profiles[0].WorkspaceWritePaths[0], "b.txt") {
		t.Fatalf("replacement authority profiles = %+v", profiles)
	}
	if out.result.Execution == nil ||
		len(out.result.Execution.Attempts) != 1 ||
		out.result.Execution.Attempts[0].PermissionDigest != profiles[0].Digest {
		t.Fatalf(
			"executed profile = %+v receipt = %+v",
			profiles[0],
			out.result.Execution,
		)
	}
}

func assertApprovalResourcesReleased(
	t *testing.T,
	registry *tool.Registry,
	request ApprovalRequest,
	active *atomic.Int32,
) {
	t.Helper()
	if active.Load() != 0 {
		t.Fatalf("approval held %d execution admissions", active.Load())
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	release, err := registry.Claims().AcquireResources(ctx, request.Resources)
	if err != nil {
		t.Fatalf("approval held resource claims: %v", err)
	}
	release()
}
