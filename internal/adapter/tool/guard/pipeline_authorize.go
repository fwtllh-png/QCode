package guard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/security/pathpolicy"
	"github.com/fwtllh-png/QCode/internal/security/policy"
)

var errWorkspaceUnchanged = errors.New("workspace edit produces no changes")

type preparedExecution struct {
	invocation Invocation
	executor   tool.Executor
	arguments  json.RawMessage
	runtime    *policy.Runtime
	decision   policy.Decision
	waited     time.Duration
}

// authorize prepares immutable arguments and resources, evaluates policy, and
// completes every initial approval before execution admission.
func (g *Guard) authorize(
	ctx context.Context,
	callID, name string,
	raw json.RawMessage,
	binding tool.CatalogBinding,
) (preparedExecution, error) {
	arguments := append(json.RawMessage(nil), raw...)
	invocation, executor, err := g.prepare(ctx, name, callID, arguments, binding)
	if err != nil {
		return preparedExecution{}, err
	}
	var approvalWait time.Duration
	for {
		// An approval wait may change policy or the catalog. Recheck them against
		// the frozen invocation without resolving paths or assessing it again.
		if _, err := g.registry.ResolveTrustedBinding(invocation.Ref); err != nil {
			return preparedExecution{}, err
		}
		policyInvocation := g.policyInput(callID, invocation)
		started := g.now()
		runtime, err := g.samplePolicy()
		if err != nil {
			return preparedExecution{}, err
		}
		decision := runtime.Decide(policyInvocation)
		reviewLatency := g.now().Sub(started)
		prepared := preparedExecution{
			invocation: invocation, executor: executor,
			arguments: arguments, runtime: runtime,
			decision: decision, waited: approvalWait,
		}
		g.observeApproval("evaluated", policyInvocation, decision, 0)
		switch decision.Action {
		case policy.ActionDeny, policy.ActionHold:
			g.observeApproval("denied", policyInvocation, decision, 0)
			return prepared, g.decisionError(decision)
		case policy.ActionAllow, policy.ActionAsk:
		default:
			return preparedExecution{}, errors.New("tool guard received invalid policy action")
		}
		if err := g.preflightFileWrites(invocation); err != nil {
			return prepared, err
		}
		if decision.Action == policy.ActionAllow {
			if decision.Code == "auto_review_allowed" {
				g.observeApproval("auto_allowed", policyInvocation, decision, reviewLatency)
			}
			g.grantNetworkHosts(ctx, policyInvocation)
			return prepared, nil
		}
		authorized, replacement, waited, err := g.authorizeAsk(
			ctx,
			invocation,
			executor,
			policyInvocation,
			decision,
			reviewLatency,
		)
		approvalWait += waited
		prepared.waited = approvalWait
		if err != nil {
			return prepared, err
		}
		if authorized {
			if _, err := g.registry.ResolveTrustedBinding(invocation.Ref); err != nil {
				return prepared, err
			}
			if invocation.Binding.Capability == tool.CapabilityNetwork {
				g.grantNetworkHosts(ctx, policyInvocation)
			}
			return prepared, nil
		}
		if replacement != nil {
			invocation, executor, arguments = replacement.invocation, replacement.executor, replacement.arguments
		}
	}
}

// decisionError reports a terminal policy decision. A protected write into
// Git metadata also points the model at the dedicated Git tools.
func (g *Guard) decisionError(decision policy.Decision) error {
	err := &policy.DecisionError{Code: decision.Code, Reason: decision.Reason}
	if decision.Code != "control_plane_protected" || decision.Resource == "" {
		return err
	}
	classification, protected, classifyErr := g.controlPlane.Classify(decision.Resource)
	if classifyErr != nil || !protected || classification.Root != pathpolicy.GitDir {
		return err
	}
	return tool.WithRecoveryHint(err, tool.RecoveryHint{
		ErrorCategory:  "control_plane_protected",
		RequiredAction: "use_git_tool",
		RetryOriginal:  false,
	})
}

func (g *Guard) authorizeAsk(
	ctx context.Context,
	invocation Invocation,
	executor tool.Executor,
	policyInvocation policy.Invocation,
	decision policy.Decision,
	reviewLatency time.Duration,
) (authorized bool, replacement *preparedExecution, waited time.Duration, err error) {
	now := g.now()
	if decision.Approval == policy.ApprovalReusable &&
		g.policy.Approvals != nil &&
		g.policy.Approvals.MatchInvocation(policyInvocation, now) {
		g.observeApproval("grant_hit", policyInvocation, decision, 0)
		return true, nil, 0, nil
	}
	editPlan, err := g.planApprovalEdit(ctx, invocation, executor)
	if err != nil {
		return false, nil, 0, err
	}
	ask := networkApprovalAsk(policyInvocation, invocation.Binding.Capability)
	if ask.Code == "" {
		ask.Code = decision.Code
	}
	if editPlan != nil {
		ask.AllowedScopes = []policy.ApprovalScope{policy.ApprovalOnce}
		ask.DisableReplace = true
		ask.EditPlan = editPlan
	}
	if decision.Approval != policy.ApprovalReusable {
		ask.AllowedScopes = []policy.ApprovalScope{policy.ApprovalOnce}
		ask.DisableReplace = true
	}
	g.observeApproval("human_required", policyInvocation, decision, reviewLatency)
	waitStarted := g.now()
	approval, err := g.waitForApproval(ctx, invocation, policyInvocation, now, ask)
	waited = g.now().Sub(waitStarted)
	if err != nil {
		return false, nil, waited, err
	}
	if editPlan != nil {
		if err := revalidateApprovedEdit(ctx, executor, invocation, *editPlan, approval); err != nil {
			return false, nil, waited, err
		}
		return true, nil, waited, nil
	}
	if decision.Approval != policy.ApprovalReusable {
		return true, nil, waited, nil
	}
	if len(approval.ReplacementArguments) != 0 {
		arguments := append(json.RawMessage(nil), approval.ReplacementArguments...)
		prepared, executor, prepareErr := g.prepare(
			ctx, invocation.Tool, invocation.CallID, arguments, invocation.Ref.Binding(),
		)
		if prepareErr != nil {
			return false, nil, waited, fmt.Errorf("replacement arguments: %w", prepareErr)
		}
		replacement = &preparedExecution{invocation: prepared, executor: executor, arguments: arguments}
		replacementInvocation := g.policyInput(invocation.CallID, prepared)
		runtime, err := g.samplePolicy()
		if err != nil {
			return false, nil, waited, err
		}
		replacementDecision := runtime.Decide(replacementInvocation)
		switch replacementDecision.Action {
		case policy.ActionAllow:
			return false, replacement, waited, nil
		case policy.ActionAsk:
			if err := g.cacheApproval(ctx, replacementInvocation, approval); err != nil {
				return false, nil, waited, err
			}
			return false, replacement, waited, nil
		default:
			return false, nil, waited, g.decisionError(replacementDecision)
		}
	}
	if err := g.cacheApproval(ctx, policyInvocation, approval); err != nil {
		return false, nil, waited, err
	}
	return false, nil, waited, nil
}

func (g *Guard) planApprovalEdit(
	ctx context.Context,
	invocation Invocation,
	executor tool.Executor,
) (*tool.EditPlan, error) {
	if !invocation.Binding.Journaled() {
		return nil, nil
	}
	planner, ok := executor.(tool.EditPlanner)
	if !ok {
		return nil, &policy.DecisionError{
			Code:   "edit_plan_unavailable",
			Reason: "workspace writer cannot produce a safe edit preview",
		}
	}
	plan, err := planner.PlanEdit(ctx, invocation.Arguments)
	if err != nil {
		return nil, fmt.Errorf("plan workspace edit: %w", err)
	}
	if len(plan.Files) == 0 {
		return nil, errWorkspaceUnchanged
	}
	return &plan, nil
}

func revalidateApprovedEdit(
	ctx context.Context,
	executor tool.Executor,
	invocation Invocation,
	editPlan tool.EditPlan,
	approval ApprovalDecision,
) error {
	if approval.PlanID != editPlan.ID {
		return &policy.DecisionError{
			Code:   "edit_plan_mismatch",
			Reason: "approval does not identify the displayed edit plan",
		}
	}
	// Re-planning on purpose: the approval paused while the workspace could
	// have moved, and a fresh plan whose ID differs from the approved one is
	// the drift signal. Caching the preview here would silence exactly the
	// check this function exists to make.
	current, err := executor.(tool.EditPlanner).PlanEdit(ctx, invocation.Arguments)
	if err != nil {
		return fmt.Errorf("revalidate workspace edit: %w", err)
	}
	if current.ID != editPlan.ID {
		return &policy.DecisionError{
			Code:   "edit_plan_stale",
			Reason: "workspace changed after edit preview",
		}
	}
	return nil
}
