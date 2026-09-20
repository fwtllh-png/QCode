package guard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
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
	var approvalWait time.Duration
	for {
		invocation, executor, err := g.prepare(ctx, name, callID, arguments, binding)
		if err != nil {
			return preparedExecution{}, err
		}
		if err := g.checkControlPlaneWrites(invocation.Resources); err != nil {
			return preparedExecution{
				invocation: invocation, executor: executor, arguments: arguments,
				waited: approvalWait,
			}, err
		}
		if err := g.preflightFileWrites(invocation); err != nil {
			return preparedExecution{
				invocation: invocation, executor: executor, arguments: arguments,
				waited: approvalWait,
			}, err
		}
		policyInvocation := policyInput(callID, invocation)
		started := g.now()
		runtime := g.Policy().CloneSampling()
		decision := runtime.Evaluate(policyInvocation)
		reviewLatency := g.now().Sub(started)
		if g.forceEditPlanApproval && invocation.Binding.Journaled() &&
			decision.Action == policy.ActionAllow {
			decision.Action = policy.ActionAsk
			decision.Code = "edit_plan_required"
			decision.Reason = "workspace writes require a fresh edit plan approval"
		}
		hostProcessApproval :=
			invocation.Binding.Effect.Approval == tool.ApprovalPolicyOnce
		if hostProcessApproval &&
			decision.Action != policy.ActionDeny &&
			decision.Action != policy.ActionHold {
			decision.Action = policy.ActionAsk
			decision.Code = "host_process_approval_required"
			decision.Reason = "host process execution requires one-time user approval"
		}
		g.observeApproval("evaluated", policyInvocation, decision, 0)
		switch decision.Action {
		case policy.ActionDeny, policy.ActionHold:
			g.observeApproval("denied", policyInvocation, decision, 0)
			return preparedExecution{
					invocation: invocation, executor: executor,
					arguments: arguments, runtime: runtime,
					decision: decision, waited: approvalWait,
				}, &policy.DecisionError{
					Code: decision.Code, Reason: decision.Reason,
				}
		case policy.ActionAllow:
			if decision.Code == "auto_review_allowed" {
				g.observeApproval("auto_allowed", policyInvocation, decision, reviewLatency)
			}
			g.grantNetworkHosts(ctx, policyInvocation)
			return preparedExecution{
				invocation: invocation, executor: executor,
				arguments: arguments, runtime: runtime,
				decision: decision, waited: approvalWait,
			}, nil
		case policy.ActionAsk:
			authorized, replacement, waited, err := g.authorizeAsk(
				ctx,
				invocation,
				executor,
				policyInvocation,
				decision,
				reviewLatency,
			)
			approvalWait += waited
			if err != nil {
				return preparedExecution{
					invocation: invocation, executor: executor,
					arguments: arguments, runtime: runtime,
					decision: decision, waited: approvalWait,
				}, err
			}
			if authorized {
				if invocation.Binding.Capability == tool.CapabilityNetwork {
					g.grantNetworkHosts(ctx, policyInvocation)
				}
				return preparedExecution{
					invocation: invocation, executor: executor,
					arguments: arguments, runtime: runtime,
					decision: decision, waited: approvalWait,
				}, nil
			}
			if len(replacement) != 0 {
				arguments = replacement
			}
		default:
			return preparedExecution{}, errors.New("tool guard received invalid policy action")
		}
	}
}

func (g *Guard) checkControlPlaneWrites(resources []tool.Resource) error {
	for _, resource := range resources {
		if resource.Access != tool.AccessWrite ||
			!isPathKind(resource.Kind) ||
			resource.Path == "" {
			continue
		}
		tree := resource.Tree || resource.Kind != "file"
		if err := g.controlPlane.CheckWrite(resource.Path, tree); err != nil {
			decision := &policy.DecisionError{
				Code:   "control_plane_protected",
				Reason: err.Error(),
			}
			classification, protected, classifyErr :=
				g.controlPlane.Classify(resource.Path)
			if classifyErr == nil && protected && classification.Root == ".git" {
				return tool.WithRecoveryHint(decision, tool.RecoveryHint{
					ErrorCategory:  "control_plane_protected",
					RequiredAction: "use_git_tool",
					RetryOriginal:  false,
				})
			}
			return decision
		}
	}
	return nil
}

func (g *Guard) authorizeAsk(
	ctx context.Context,
	invocation Invocation,
	executor tool.Executor,
	policyInvocation policy.Invocation,
	decision policy.Decision,
	reviewLatency time.Duration,
) (authorized bool, replacement json.RawMessage, waited time.Duration, err error) {
	now := g.now()
	hostProcessApproval :=
		invocation.Binding.Effect.Approval == tool.ApprovalPolicyOnce
	if !g.forceEditPlanApproval && !hostProcessApproval &&
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
	if hostProcessApproval {
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
	if hostProcessApproval {
		return true, nil, waited, nil
	}
	if len(approval.ReplacementArguments) != 0 {
		replacement = append(json.RawMessage(nil), approval.ReplacementArguments...)
		prepared, _, prepareErr := g.prepare(
			ctx, invocation.Tool, invocation.CallID, replacement, invocation.Ref.Binding(),
		)
		if prepareErr != nil {
			return false, nil, waited, fmt.Errorf("replacement arguments: %w", prepareErr)
		}
		replacementInvocation := policyInput(invocation.CallID, prepared)
		replacementDecision := g.policy.Evaluate(replacementInvocation)
		switch replacementDecision.Action {
		case policy.ActionAllow:
			return false, replacement, waited, nil
		case policy.ActionAsk:
			if err := g.cacheApproval(ctx, replacementInvocation, approval); err != nil {
				return false, nil, waited, err
			}
			return false, replacement, waited, nil
		default:
			return false, nil, waited, &policy.DecisionError{
				Code: replacementDecision.Code, Reason: replacementDecision.Reason,
			}
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
