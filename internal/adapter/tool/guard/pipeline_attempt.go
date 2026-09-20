package guard

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/persist/workspacejournal"
	"github.com/fwtllh-png/QCode/internal/security/authority"
	"github.com/fwtllh-png/QCode/internal/security/controlmatrix"
	"github.com/fwtllh-png/QCode/internal/security/egress"
	"github.com/fwtllh-png/QCode/internal/security/policy"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

type retryKind string

const (
	retryNone       retryKind = ""
	retryPermission retryKind = "additional_permission"
	retryEgress     retryKind = "egress_approval"
	retryAuthority  retryKind = "authority_refresh"
)

type attemptRun struct {
	result       tool.Result
	outcome      tool.Outcome
	err          error
	retry        retryKind
	target       tool.NetworkTarget
	receipt      tool.AttemptReceipt
	dispatchWait time.Duration
	claimWait    time.Duration
	teardown     tool.TeardownReport
	aborted      bool
	profile      authority.EffectivePermissionProfile
	permission   authority.AdditionalPermissionRequest
}

func (g *Guard) executePipeline(
	ctx context.Context,
	callID, name string,
	raw json.RawMessage,
	binding tool.CatalogBinding,
) (tool.Result, error) {
	ctx, closeNetworkScope := egress.WithScope(ctx)
	defer closeNetworkScope()
	mode := SandboxModeStrong
	egressRetried := false
	permissionRetried := false
	authorityRetried := false
	var retryPrepared *preparedExecution
	var retryProfile *authority.EffectivePermissionProfile
	var receipt tool.ExecutionReceipt
	for {
		var prepared preparedExecution
		if retryPrepared != nil {
			prepared = *retryPrepared
			retryPrepared = nil
		} else {
			authorized, err := g.authorize(ctx, callID, name, raw, binding)
			receipt.ApprovalWait += authorized.waited
			if err != nil {
				if errors.Is(err, errWorkspaceUnchanged) {
					result := workspaceUnchangedResult()
					if authorized.invocation.Ref.Name != "" {
						receipt.Tool = authorized.invocation.Ref
						receipt.Source = authorized.invocation.Source
						receipt.Disposition = authorized.invocation.Disposition
						setExecutionTerminal(
							&receipt,
							tool.OutcomeSucceeded,
							tool.TerminalOwnerGuard,
							tool.TeardownReport{},
						)
						attachExecutionReceipt(&result, receipt)
					}
					g.afterAttempt(ctx, authorized.invocation, result, nil)
					return result, nil
				}
				result := tool.Result{IsError: true}
				if authorized.invocation.Ref.Name != "" {
					receipt.Tool = authorized.invocation.Ref
					receipt.Source = authorized.invocation.Source
					receipt.Disposition = authorized.invocation.Disposition
					receipt.VerificationEvidenceAuthorized =
						authorized.invocation.Binding.ProducesVerificationEvidence
					setExecutionTerminal(
						&receipt,
						terminalStatus(err, result),
						tool.TerminalOwnerGuard,
						tool.TeardownReport{},
					)
					attachExecutionReceipt(&result, receipt)
				}
				return result, err
			}
			prepared = authorized
		}
		raw = append(json.RawMessage(nil), prepared.arguments...)
		if prepared.invocation.Binding.SandboxRequirement == tool.SandboxNone {
			mode = SandboxModeNone
		}
		if receipt.Tool.Name == "" {
			receipt.Tool = prepared.invocation.Ref
			receipt.Source = prepared.invocation.Source
			receipt.Disposition = prepared.invocation.Disposition
			receipt.VerificationEvidenceAuthorized =
				prepared.invocation.Binding.ProducesVerificationEvidence
		}
		attempt := g.runAttempt(
			ctx,
			prepared,
			mode,
			uint32(len(receipt.Attempts)+1),
			egressRetried,
			permissionRetried,
			authorityRetried,
			retryProfile,
		)
		retryProfile = nil
		receipt.Attempts = append(receipt.Attempts, attempt.receipt)
		receipt.DispatchWait += attempt.dispatchWait
		receipt.ClaimWait += attempt.claimWait
		switch attempt.retry {
		case retryPermission:
			waited, approval, approvalErr := g.approveAdditionalPermission(
				ctx,
				prepared.invocation,
				callID,
				attempt.permission,
			)
			receipt.ApprovalWait += waited
			if approvalErr != nil {
				setAmendmentDecision(
					&receipt,
					amendmentDecision(approvalErr),
					"",
				)
				setExecutionTerminal(
					&receipt,
					terminalStatus(approvalErr, attempt.result),
					tool.TerminalOwnerGuard,
					attempt.teardown,
				)
				attachExecutionReceipt(&attempt.result, receipt)
				g.afterAttempt(ctx, prepared.invocation, attempt.result, approvalErr)
				return attempt.result, approvalErr
			}
			amended, amendErr := authority.Amend(
				attempt.profile,
				attempt.permission,
				uint64(len(receipt.Attempts)+1),
			)
			if amendErr != nil {
				setAmendmentDecision(&receipt, "failed", "")
				setExecutionTerminal(
					&receipt,
					tool.OutcomeFailed,
					tool.TerminalOwnerGuard,
					attempt.teardown,
				)
				attachExecutionReceipt(&attempt.result, receipt)
				g.afterAttempt(ctx, prepared.invocation, attempt.result, amendErr)
				return attempt.result, amendErr
			}
			updated, updateErr := g.reauthorizeAdditionalPermission(
				ctx,
				prepared,
				mode,
				attempt.profile,
				attempt.permission.Permission,
				approval,
			)
			if updateErr != nil {
				setAmendmentDecision(
					&receipt,
					amendmentDecision(updateErr),
					"",
				)
				setExecutionTerminal(
					&receipt,
					terminalStatus(updateErr, attempt.result),
					tool.TerminalOwnerGuard,
					attempt.teardown,
				)
				attachExecutionReceipt(&attempt.result, receipt)
				g.afterAttempt(ctx, prepared.invocation, attempt.result, updateErr)
				return attempt.result, updateErr
			}
			setAmendmentDecision(&receipt, "approved", amended.Digest)
			retryPrepared = &updated
			retryProfile = &amended
			permissionRetried = true
			continue
		case retryEgress:
			egressRetried = true
			started := g.now()
			approvalErr := g.approveEgressTarget(
				ctx,
				prepared.invocation,
				callID,
				attempt.target,
			)
			receipt.ApprovalWait += g.now().Sub(started)
			if approvalErr == nil {
				continue
			}
			if softFailEgressApproval(approvalErr) {
				setExecutionTerminal(
					&receipt,
					attempt.receipt.Status,
					attempt.receipt.TerminalOwner,
					attempt.teardown,
				)
				attachExecutionReceipt(&attempt.result, receipt)
				g.afterAttempt(ctx, prepared.invocation, attempt.result, nil)
				if attempt.err != nil {
					return attempt.result, attempt.err
				}
				return attempt.result, nil
			}
			setExecutionTerminal(
				&receipt,
				terminalStatus(approvalErr, attempt.result),
				tool.TerminalOwnerGuard,
				attempt.teardown,
			)
			attachExecutionReceipt(&attempt.result, receipt)
			g.afterAttempt(ctx, prepared.invocation, attempt.result, approvalErr)
			return attempt.result, approvalErr
		case retryAuthority:
			authorityRetried = true
			continue
		}
		setExecutionTerminal(
			&receipt,
			attempt.receipt.Status,
			attempt.receipt.TerminalOwner,
			attempt.teardown,
		)
		attachExecutionReceipt(&attempt.result, receipt)
		g.afterAttempt(ctx, prepared.invocation, attempt.result, attempt.err)
		if attempt.err != nil {
			return attempt.result, attempt.err
		}
		return attempt.result, nil
	}
}

func (g *Guard) runAttempt(
	ctx context.Context,
	prepared preparedExecution,
	mode SandboxMode,
	sequence uint32,
	egressRetried bool,
	permissionRetried bool,
	authorityRetried bool,
	profileOverride *authority.EffectivePermissionProfile,
) (run attemptRun) {
	invocation := prepared.invocation
	started := g.now()
	var profile authority.EffectivePermissionProfile
	var err error
	if profileOverride == nil {
		profile, err = g.compileAuthority(prepared, mode, uint64(sequence))
	} else {
		profile = *profileOverride
		err = profile.Validate()
	}
	if err != nil || profile.Revision != uint64(sequence) {
		if err == nil {
			err = errors.New("additional permission profile revision is invalid")
		}
		run.err = err
		run.receipt = attemptReceipt(
			sequence, mode, started, g.now(), tool.OutcomeRejected, "authority_compile",
			run.profile,
		)
		return run
	}
	run.profile = profile
	brokerExecutor, brokerAware := prepared.executor.(AuthorizedProcessExecutor)
	fileExecutor, fileBrokerAware := prepared.executor.(AuthorizedFileExecutor)
	fileBrokerAware = fileBrokerAware &&
		fileExecutor.IsAuthorizedFileMutation(prepared.invocation)
	brokerManaged := brokerAware || fileBrokerAware
	var artifactBinding authority.ArtifactBinding
	var artifactIntent *authority.ArtifactIntent
	var fileBinding authority.FileBinding
	if brokerAware {
		preliminary, buildErr := g.buildExecutionOperation(prepared, profile, nil, "")
		if buildErr != nil {
			run.err = buildErr
			run.receipt = attemptReceipt(
				sequence, mode, started, g.now(), tool.OutcomeRejected,
				"artifact_operation", run.profile,
			)
			return run
		}
		artifactBinding, err = brokerExecutor.PrepareAuthorizedProcess(
			ctx, prepared.invocation, preliminary.Digest,
		)
		if err != nil {
			run.err = err
			run.receipt = attemptReceipt(
				sequence, mode, started, g.now(), tool.OutcomeRejected,
				"artifact_prepare", run.profile,
			)
			return run
		}
		artifactIntent = &authority.ArtifactIntent{
			ManifestDigest: artifactBinding.ManifestDigest,
			Generation:     artifactBinding.Generation,
		}
	}
	if fileBrokerAware {
		fileBinding, err = fileExecutor.PrepareAuthorizedFile(
			ctx, prepared.invocation,
		)
		if err != nil {
			run.err = err
			run.receipt = attemptReceipt(
				sequence, mode, started, g.now(), tool.OutcomeRejected,
				"file_plan", run.profile,
			)
			return run
		}
		if fileBinding.Noop {
			run.result = workspaceUnchangedResult()
			run.outcome = tool.OutcomeFromResult(run.result)
			run.receipt = attemptReceipt(
				sequence, mode, started, g.now(), tool.OutcomeSucceeded,
				"workspace_unchanged", run.profile,
			)
			run.receipt.TerminalOwner = tool.TerminalOwnerGuard
			return run
		}
	}
	operation, lease, leaseSnapshot, err := g.issueExecutionLease(
		ctx,
		prepared,
		profile,
		uint64(sequence),
		artifactIntent,
		fileBinding.MutationDigest,
		!brokerManaged,
	)
	if err != nil {
		run.err = err
		run.receipt = attemptReceipt(
			sequence, mode, started, g.now(), tool.OutcomeRejected, "lease_issue",
			run.profile,
		)
		return run
	}
	defer func() {
		var snapshot authority.LeaseSnapshot
		var settleErr error
		if brokerManaged {
			snapshot, settleErr = g.leaseAuthority.Snapshot(lease)
			if settleErr == nil && snapshot.State == authority.LeaseIssued {
				settleErr = g.leaseAuthority.Revoke(lease)
				if settleErr == nil {
					snapshot, settleErr = g.leaseAuthority.Snapshot(lease)
				}
			}
		} else {
			snapshot, settleErr = settleExecutionLease(
				g.leaseAuthority,
				lease,
				run.receipt.Status,
				run.receipt.Reason,
				g.now(),
			)
		}
		if settleErr != nil {
			run.err = errors.Join(run.err, settleErr)
			run.receipt.Status = tool.OutcomeFailed
			run.receipt.Reason = "lease_settlement"
			run.receipt.TerminalOwner = tool.TerminalOwnerGuard
			snapshot = leaseSnapshot
		}
		bindAttemptLease(&run.receipt, operation, snapshot)
		if settleErr == nil {
			if releaseErr := g.leaseAuthority.Release(lease); releaseErr != nil {
				run.err = errors.Join(run.err, releaseErr)
				run.receipt.Status = tool.OutcomeFailed
				run.receipt.Reason = "lease_release"
				run.receipt.TerminalOwner = tool.TerminalOwnerGuard
			}
		}
		if brokerAware {
			if releaseErr := brokerExecutor.ReleaseAuthorizedProcess(
				context.WithoutCancel(ctx),
				artifactBinding,
			); releaseErr != nil {
				run.err = errors.Join(run.err, releaseErr)
				run.receipt.Status = tool.OutcomeFailed
				run.receipt.Reason = "artifact_release"
				run.receipt.TerminalOwner = tool.TerminalOwnerGuard
			}
		}
	}()
	dispatchStarted := g.now()
	releaseAdmission, err := tool.AdmitExecution(ctx, invocation.Binding.ParallelPolicy)
	run.dispatchWait = g.now().Sub(dispatchStarted)
	if err != nil {
		run.err = err
		run.receipt = attemptReceipt(
			sequence, mode, started, g.now(), tool.OutcomeCanceled, "dispatch", run.profile,
		)
		return run
	}
	claimStarted := g.now()
	releaseClaims, err := g.registry.Claims().AcquireResources(ctx, invocation.Resources)
	run.claimWait = g.now().Sub(claimStarted)
	if err != nil {
		releaseAdmission()
		run.err = err
		run.receipt = attemptReceipt(
			sequence, mode, started, g.now(), tool.OutcomeCanceled, "claim", run.profile,
		)
		return run
	}
	release := func() {
		releaseClaims()
		releaseAdmission()
	}
	writePaths := invocationWritePaths(invocation)
	requireRead := invocation.Binding.Effect.RequireReadBeforeWrite
	var expectedWrites map[string]workspacejournal.Fingerprint
	if requireRead {
		err = g.recordExactEditProofs(
			ctx,
			prepared.executor,
			prepared.arguments,
			writePaths,
		)
	}
	if err != nil {
		release()
		run.err = err
		run.receipt = attemptReceipt(
			sequence, mode, started, g.now(), tool.OutcomeRejected, "prepare_writes",
			run.profile,
		)
		return run
	}
	if fileBrokerAware {
		expectedWrites, err = g.validateFileWrites(writePaths, requireRead)
	} else {
		expectedWrites, err = g.prepareFileWrites(ctx, writePaths, requireRead)
	}
	if err != nil {
		release()
		run.err = err
		run.receipt = attemptReceipt(
			sequence, mode, started, g.now(), tool.OutcomeRejected, "prepare_writes",
			run.profile,
		)
		return run
	}
	executeContext := workspacejournal.WithExpectedWrites(ctx, expectedWrites)
	readBefore, err := g.snapshotReadTarget(invocation)
	if err != nil {
		release()
		run.err = err
		run.receipt = attemptReceipt(
			sequence, mode, started, g.now(), tool.OutcomeFailed, "snapshot_read",
			run.profile,
		)
		return run
	}
	runContext, err := sandbox.WithExecutionAuthority(
		executeContext,
		profile.ExecutionAuthorityFor(operation),
	)
	if err != nil {
		release()
		run.err = err
		run.receipt = attemptReceipt(
			sequence, mode, started, g.now(), tool.OutcomeRejected, "authority_context",
			run.profile,
		)
		return run
	}
	runContext = WithSandboxAttempt(runContext, SandboxAttempt{Mode: mode})
	var teardownMu sync.Mutex
	runContext = tool.WithTeardownObserver(
		runContext,
		func(report tool.TeardownReport) {
			teardownMu.Lock()
			run.teardown.Duration += report.Duration
			run.teardown.TimedOut = run.teardown.TimedOut || report.TimedOut
			teardownMu.Unlock()
		},
	)
	if brokerAware {
		sandboxPolicyID, policyErr := sandboxPolicyBinding(
			profile, prepared.invocation.Tool,
		)
		if policyErr != nil {
			run.err = policyErr
		} else {
			run.result, run.outcome, run.err =
				brokerExecutor.ExecuteAuthorizedProcess(
					runContext,
					prepared.invocation,
					authority.AuthorizedProcessGrant{
						Operation: operation,
						Lease:     lease,
						Validation: leaseValidation(
							operation,
							prepared.runtime.Revision,
							sandboxPolicyID,
							uint64(sequence),
						),
						Artifact: artifactBinding.Value,
					},
				)
		}
	} else if fileBrokerAware {
		sandboxPolicyID, policyErr := sandboxPolicyBinding(
			profile, prepared.invocation.Tool,
		)
		if policyErr != nil {
			run.err = policyErr
		} else {
			run.result, run.outcome, run.err =
				fileExecutor.ExecuteAuthorizedFile(
					runContext,
					prepared.invocation,
					authority.AuthorizedFileGrant{
						Operation: operation,
						Lease:     lease,
						Validation: leaseValidation(
							operation,
							prepared.runtime.Revision,
							sandboxPolicyID,
							uint64(sequence),
						),
						Plan: fileBinding.Value,
					},
					g.leaseAuthority,
					g.journal,
				)
		}
	} else {
		run.result, run.outcome, run.err, run.aborted = g.executePrepared(
			runContext,
			prepared,
		)
	}
	if invocation.Binding.RecordsWorkspaceRead && run.err == nil {
		if recordErr := g.recordFileRead(&run.result, invocation, readBefore); recordErr != nil {
			run.err = recordErr
		}
	}
	if len(writePaths) != 0 && !fileBrokerAware {
		if finishErr := g.finishFileWrites(
			ctx,
			writePaths,
			expectedWrites,
			&run.result,
			run.err == nil,
			invocation.Binding.Journaled(),
			invocation.Binding.Journaled(),
		); finishErr != nil && run.err == nil {
			run.err = finishErr
		}
	}
	if len(writePaths) != 0 && fileBrokerAware {
		if finishErr := g.finishBrokerFileWrites(
			ctx, writePaths, &run.result, run.err == nil,
		); finishErr != nil && run.err == nil {
			run.err = finishErr
		}
	}
	release()
	teardownMu.Lock()
	teardown := run.teardown
	teardownMu.Unlock()
	run.teardown = teardown
	status := run.outcome.Status
	if status == "" {
		status = tool.OutcomeFromResult(run.result).Status
	}
	reason := ""
	denial, denied := SandboxDenial(run.err, run.outcome)
	if isManagedProxyAuthorityMismatch(denial) && !authorityRetried {
		run.retry = retryAuthority
		reason = string(retryAuthority)
	} else if isManagedProxyAuthorityMismatch(denial) {
		run.err = annotateManagedProxyMismatch(run.err)
		reason = "sandbox_denied_fail_closed"
	} else if denied &&
		mode == SandboxModeStrong &&
		!permissionRetried &&
		g.canEscalate(invocation) {
		request, requestErr := authority.RequestFromDenial(profile, denial)
		if requestErr == nil &&
			additionalPermissionAllowed(invocation, request.Permission) {
			run.retry, run.permission = retryPermission, request
			reason = string(retryPermission)
		} else {
			reason = "sandbox_denied_fail_closed"
		}
	} else if !egressRetried {
		if target, ok := egressDeniedTarget(run.outcome, run.err); ok {
			run.retry, reason = retryEgress, string(retryEgress)
			run.target = target
		} else if run.err != nil {
			reason = "execute_error"
		} else if run.result.IsError {
			reason = "tool_error"
		}
	} else if run.err != nil {
		reason = "execute_error"
	} else if run.result.IsError {
		reason = "tool_error"
	}
	if errors.Is(run.err, context.Canceled) {
		status = tool.OutcomeCanceled
		reason = "context_canceled"
	} else if run.err != nil {
		status = tool.OutcomeFailed
	}
	run.receipt = attemptReceipt(
		sequence, mode, started, g.now(), status, reason, run.profile,
	)
	if denied {
		run.receipt.Denial = &denial
	}
	if run.retry == retryPermission {
		run.receipt.Amendment = amendmentReceipt(run.permission)
	}
	run.receipt.TerminalOwner = tool.TerminalOwnerExecutor
	if run.aborted {
		run.receipt.TerminalOwner = tool.TerminalOwnerGuard
	}
	run.receipt.Teardown = run.teardown.Duration
	run.receipt.TeardownMS = run.teardown.Duration.Milliseconds()
	run.receipt.TeardownTimedOut = run.teardown.TimedOut
	return run
}

func workspaceUnchangedResult() tool.Result {
	return tool.Result{
		Content: "no changes",
		Metadata: map[string]any{
			"observed_changes": 0,
		},
	}
}

func isManagedProxyAuthorityMismatch(denial sandbox.Denial) bool {
	return denial.Operation == sandbox.DenialNetwork &&
		denial.Resource == "managed_proxy" &&
		denial.ReasonCode == sandbox.ReasonAuthorityUnverified
}

const (
	managedProxyMismatchCategory = "managed_proxy_mismatch"
	managedProxyMismatchAction   = "keep_allow_loopback_omit_network_targets"
)

func annotateManagedProxyMismatch(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := tool.RecoveryHintFromError(err); ok {
		return err
	}
	return tool.WithRecoveryHint(err, tool.RecoveryHint{
		ErrorCategory:  managedProxyMismatchCategory,
		RequiredAction: managedProxyMismatchAction,
		RetryOriginal:  false,
	})
}

func additionalPermissionAllowed(
	invocation Invocation,
	permission authority.AdditionalPermission,
) bool {
	switch permission.Kind {
	case authority.AdditionalPathRead:
		return true
	case authority.AdditionalPathWrite:
		return invocation.Binding.AccessMode != tool.AccessRead
	case authority.AdditionalNetwork:
		return invocation.Binding.Capability == tool.CapabilityNetwork ||
			invocation.Binding.Capability == tool.CapabilityProcess ||
			invocation.Binding.Capability == tool.CapabilityExternal
	case authority.AdditionalProcess:
		return invocation.Binding.Capability == tool.CapabilityProcess ||
			invocation.Binding.Capability == tool.CapabilityExternal
	default:
		return false
	}
}

type preparedOutcome struct {
	result  tool.Result
	outcome tool.Outcome
	err     error
}

func (g *Guard) executePrepared(
	ctx context.Context,
	prepared preparedExecution,
) (tool.Result, tool.Outcome, error, bool) {
	invocation := prepared.invocation
	if invocation.Disposition != tool.DispositionAbortImmediately {
		result, outcome, err := g.registry.ExecutePreparedOutcome(
			ctx,
			invocation.Tool,
			invocation.Arguments,
			prepared.executor,
		)
		return result, outcome, err, false
	}
	if err := ctx.Err(); err != nil {
		return tool.Result{}, tool.Outcome{Status: tool.OutcomeCanceled}, err, true
	}
	done := make(chan preparedOutcome, 1)
	go func() {
		result, outcome, err := g.registry.ExecutePreparedOutcome(
			ctx,
			invocation.Tool,
			invocation.Arguments,
			prepared.executor,
		)
		done <- preparedOutcome{result: result, outcome: outcome, err: err}
	}()
	select {
	case completed := <-done:
		return completed.result, completed.outcome, completed.err, false
	case <-ctx.Done():
		return tool.Result{},
			tool.Outcome{Status: tool.OutcomeCanceled},
			ctx.Err(),
			true
	}
}

func (g *Guard) snapshotReadTarget(
	invocation Invocation,
) (*workspacejournal.Fingerprint, error) {
	if !invocation.Binding.RecordsWorkspaceRead {
		return nil, nil
	}
	for _, resource := range invocation.Resources {
		if resource.Kind != "file" || resource.Path == "" {
			continue
		}
		value, _, _, err := workspacejournal.Snapshot(resource.Path)
		if err != nil {
			return nil, err
		}
		return &value, nil
	}
	return nil, nil
}

func (g *Guard) approveAdditionalPermission(
	ctx context.Context,
	invocation Invocation,
	callID string,
	request authority.AdditionalPermissionRequest,
) (time.Duration, ApprovalDecision, error) {
	escalation := policyInput(callID, invocation)
	escalation.Resources = append(
		append([]tool.Resource(nil), invocation.Resources...),
		authority.PermissionResource(request.Permission),
	)
	now := g.now()
	started := g.now()
	approval, err := g.waitForApproval(
		ctx,
		invocation,
		escalation,
		now,
		approvalAsk{
			Code:                 ApprovalReasonAdditionalPermission,
			AllowedScopes:        []policy.ApprovalScope{policy.ApprovalOnce},
			DisableReplace:       true,
			AdditionalPermission: &request,
		},
	)
	waited := g.now().Sub(started)
	if err != nil {
		return waited, ApprovalDecision{}, err
	}
	return waited, approval, nil
}

func (g *Guard) reauthorizeAdditionalPermission(
	ctx context.Context,
	prepared preparedExecution,
	mode SandboxMode,
	base authority.EffectivePermissionProfile,
	permission authority.AdditionalPermission,
	approval ApprovalDecision,
) (preparedExecution, error) {
	runtime := g.Policy().CloneSampling()
	baseline := prepared
	baseline.runtime = runtime
	baseline.decision = runtime.Evaluate(
		policyInput(prepared.invocation.CallID, prepared.invocation),
	)
	current, err := g.compileAuthority(baseline, mode, base.Revision)
	if err != nil || current.Digest != base.Digest {
		return preparedExecution{}, &policy.DecisionError{
			Code:   "authorization_changed",
			Reason: "tool authorization changed before the amended retry",
		}
	}
	resource := authority.PermissionResource(permission)
	resources := append(
		append([]tool.Resource(nil), prepared.invocation.Resources...),
		resource,
	)
	if writeErr := g.checkControlPlaneWrites(resources); writeErr != nil {
		return preparedExecution{}, writeErr
	}
	prepared.invocation.Resources = resources
	invocation := policyInput(
		prepared.invocation.CallID,
		prepared.invocation,
	)
	decision := runtime.Evaluate(invocation)
	switch decision.Action {
	case policy.ActionDeny, policy.ActionHold:
		return preparedExecution{}, &policy.DecisionError{
			Code: decision.Code, Reason: decision.Reason,
		}
	case policy.ActionAsk:
		if err := g.cacheApproval(ctx, invocation, approval); err != nil {
			return preparedExecution{}, err
		}
		if runtime.Approvals == nil ||
			!runtime.Approvals.MatchInvocation(invocation, g.now()) {
			return preparedExecution{}, &policy.DecisionError{
				Code:   "approval_expired",
				Reason: "amended tool approval is no longer valid",
			}
		}
	case policy.ActionAllow:
	default:
		return preparedExecution{}, errors.New(
			"tool guard received invalid amended policy action",
		)
	}
	prepared.runtime = runtime
	prepared.decision = decision
	g.grantNetworkHosts(ctx, invocation)
	return prepared, nil
}

func (g *Guard) afterAttempt(
	ctx context.Context,
	invocation Invocation,
	result tool.Result,
	err error,
) {
	if err == nil && !result.IsError {
		if submitted, _ := result.Metadata["submitted_plan"].(bool); submitted {
			g.Policy().SubmitPlan()
		}
	}
}

func attemptReceipt(
	sequence uint32,
	mode SandboxMode,
	started, completed time.Time,
	status tool.OutcomeStatus,
	reason string,
	profile authority.EffectivePermissionProfile,
) tool.AttemptReceipt {
	if status == "" {
		status = tool.OutcomeFailed
	}
	receipt := tool.AttemptReceipt{
		Sequence: sequence, Sandbox: string(mode), Status: status,
		TerminalOwner: tool.TerminalOwnerGuard, Reason: reason,
		StartedAt: started, CompletedAt: completed,
		DurationMS: completed.Sub(started).Milliseconds(),
	}
	bindAttemptAuthority(&receipt, profile)
	return receipt
}

func bindAttemptAuthority(
	receipt *tool.AttemptReceipt,
	profile authority.EffectivePermissionProfile,
) {
	if receipt == nil || profile.Validate() != nil {
		return
	}
	receipt.PermissionSchemaVersion = profile.SchemaVersion
	receipt.PermissionRevision = profile.Revision
	receipt.PermissionDigest = profile.Digest
	receipt.PermissionCapability = profile.Capability
	receipt.PermissionAccess = profile.Access
	receipt.Enforcement = profile.Process.Enforcement
	receipt.Backend = profile.Process.Backend
	receipt.EffectiveControls = profile.Controls
	receipt.WorkspaceRoot = profile.Filesystem.WorkspaceRoot
	receipt.ReadRoots = append([]string(nil), profile.Filesystem.ReadRoots...)
	receipt.WritePaths = append([]string(nil), profile.Filesystem.WritePaths...)
	receipt.DeniedWriteRoots = append(
		[]string(nil),
		profile.Filesystem.DeniedWriteRoots...,
	)
	receipt.WorkspaceBaseWrite = profile.Filesystem.WorkspaceBaseWrite
	receipt.NetworkMode = profile.Network.Mode
	receipt.NetworkTargets = append([]string(nil), profile.Network.Targets...)
	receipt.ManagedProxyPort = profile.Network.ProxyPort
	receipt.LoopbackAllowed = profile.Network.Loopback
	receipt.ProcessAllowed = profile.Process.Allowed
	receipt.Provenance = make(
		[]tool.PermissionProvenance,
		len(profile.Provenance),
	)
	for index, source := range profile.Provenance {
		receipt.Provenance[index] = tool.PermissionProvenance{
			Kind: source.Kind, Value: source.Value,
			Digest: source.Digest, Revision: source.Revision,
		}
	}
}

func bindAttemptLease(
	receipt *tool.AttemptReceipt,
	operation authority.ExecutionOperation,
	lease authority.LeaseSnapshot,
) {
	if receipt == nil || operation.Validate() != nil || lease.ID == "" {
		return
	}
	receipt.OperationSchemaVersion = operation.SchemaVersion
	receipt.OperationDigest = operation.Digest
	receipt.LeaseID = lease.ID
	receipt.LeaseState = string(lease.State)
	receipt.LeaseAttempt = lease.Attempt
	receipt.WorkspaceID = lease.WorkspaceID
	receipt.WorkspaceGeneration = lease.WorkspaceGeneration
	receipt.SubjectKind = string(operation.Subject.Kind)
	receipt.SubjectID = operation.Subject.ID
	receipt.SubjectDigest = lease.SubjectDigest
	receipt.SubjectGeneration = lease.SubjectGeneration
	receipt.PolicyRevision = lease.PolicyRevision
	receipt.SandboxPolicyID = lease.SandboxPolicyID
	receipt.EffectKind = string(operation.Effect.Kind)
	receipt.EffectRisk = string(operation.Effect.Risk)
	receipt.EffectReversibility = string(operation.Effect.Reversibility)
	receipt.WorkspaceTransaction = string(operation.Effect.WorkspaceTransaction)
	if operation.Artifact != nil {
		receipt.EffectiveControls.ArtifactOrigin =
			controlmatrix.ArtifactOriginBrokerSnapshot
	}
	if operation.Effect.WorkspaceTransaction ==
		authority.WorkspaceTransactionBeforeImage {
		receipt.EffectiveControls.DurableRecovery =
			controlmatrix.DurableRecoveryExternalJournal
	}
}

func amendmentReceipt(
	request authority.AdditionalPermissionRequest,
) *tool.PermissionAmendmentReceipt {
	permission := request.Permission
	return &tool.PermissionAmendmentReceipt{
		BasePermissionDigest: request.BaseProfileDigest,
		Kind:                 string(permission.Kind), Resource: permission.Resource,
		Protocol: permission.Protocol, Port: permission.Port,
		Capability: permission.Capability, Decision: "requested",
	}
}

func amendmentDecision(err error) string {
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	return "rejected"
}

func setAmendmentDecision(
	receipt *tool.ExecutionReceipt,
	decision,
	amendedDigest string,
) {
	if receipt == nil || len(receipt.Attempts) == 0 {
		return
	}
	amendment := receipt.Attempts[len(receipt.Attempts)-1].Amendment
	if amendment == nil {
		return
	}
	amendment.Decision = decision
	amendment.AmendedPermissionDigest = amendedDigest
}

func setExecutionTerminal(
	receipt *tool.ExecutionReceipt,
	status tool.OutcomeStatus,
	owner tool.TerminalOwner,
	teardown tool.TeardownReport,
) {
	if receipt == nil || receipt.TerminalStatus != "" {
		return
	}
	if status == "" {
		status = tool.OutcomeFailed
	}
	if owner == "" {
		owner = tool.TerminalOwnerGuard
	}
	receipt.TerminalStatus = status
	receipt.TerminalOwner = owner
	receipt.Teardown = teardown.Duration
	receipt.TeardownMS = teardown.Duration.Milliseconds()
	receipt.TeardownTimedOut = teardown.TimedOut
}

func terminalStatus(err error, result tool.Result) tool.OutcomeStatus {
	if errors.Is(err, context.Canceled) {
		return tool.OutcomeCanceled
	}
	if err != nil {
		var decision *policy.DecisionError
		if errors.As(err, &decision) {
			if decision.Code == "approval_canceled" {
				return tool.OutcomeCanceled
			}
			return tool.OutcomeRejected
		}
		return tool.OutcomeFailed
	}
	return tool.OutcomeFromResult(result).Status
}

func attachExecutionReceipt(result *tool.Result, receipt tool.ExecutionReceipt) {
	if result == nil {
		return
	}
	result.Execution = tool.CloneExecutionReceipt(&receipt)
}
