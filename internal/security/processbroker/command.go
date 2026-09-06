package processbroker

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/fwtllh-png/QCode/internal/platform/process"
	"github.com/fwtllh-png/QCode/internal/security/authority"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

type CommandRequest struct {
	Lease      authority.ExecutionLease
	Validation authority.LeaseValidation
	Options    process.Options
	Identity   Identity
}

func (b *Broker) RunCommand(
	ctx context.Context,
	request CommandRequest,
) (Result, error) {
	if b == nil || b.authority == nil {
		return Result{}, errors.New("process broker is required")
	}
	if err := validateCommand(request); err != nil {
		return Result{}, err
	}
	var result Result
	settlement, err := b.authority.RunSettled(
		request.Lease,
		request.Validation,
		"runner_failure",
		time.Now,
		func(settlement *authority.Settlement) error {
			var runErr error
			result, runErr = b.runCommandConsumed(ctx, request, settlement)
			return runErr
		},
	)
	result.Settlement = settlement
	return result, err
}

func (b *Broker) runCommandConsumed(
	ctx context.Context,
	request CommandRequest,
	settlement *authority.Settlement,
) (result Result, resultErr error) {
	leaseSnapshot, err := b.authority.Snapshot(request.Lease)
	if err != nil {
		return Result{}, err
	}
	runCtx, err := sandbox.WithExecutionAuthority(
		ctx,
		leaseSnapshot.PermissionProfile.ExecutionAuthorityFor(
			request.Validation.Operation,
		),
	)
	if err != nil {
		return Result{}, err
	}
	running, err := process.StartManaged(runCtx, request.Options)
	if err != nil {
		return Result{}, err
	}
	generation := b.generation.Add(1)
	processID := strconv.Itoa(running.PID())
	handle, err := b.authority.IssueProcessHandle(
		request.Lease,
		authority.ProcessHandleRequest{
			SessionID: request.Identity.SessionID,
			ThreadID:  request.Identity.ThreadID, TurnID: request.Identity.TurnID,
			ProcessID: processID, Generation: generation,
			Actions: []authority.ProcessAction{
				authority.ProcessObserve, authority.ProcessWait, authority.ProcessCancel,
			},
		},
	)
	if err != nil {
		_ = running.Cancel()
		_, _ = running.Wait(context.WithoutCancel(ctx))
		return Result{}, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, b.authority.CompleteProcessHandle(handle))
	}()
	processResult, waitErr := running.Wait(ctx)
	if waitErr != nil && ctx.Err() != nil {
		if validateErr := b.authority.ValidateProcessHandle(
			handle,
			request.Identity.SessionID, request.Identity.ThreadID, request.Identity.TurnID,
			processID, generation, authority.ProcessCancel,
		); validateErr != nil {
			return Result{}, validateErr
		}
		_ = running.Cancel()
		processResult, _ = running.Wait(context.WithoutCancel(ctx))
		settlement.Status, settlement.Reason = "canceled", "context_canceled"
		return Result{Process: processResult}, ctx.Err()
	}
	if err := b.authority.ValidateProcessHandle(
		handle,
		request.Identity.SessionID, request.Identity.ThreadID, request.Identity.TurnID,
		processID, generation, authority.ProcessWait,
	); err != nil {
		return Result{}, err
	}
	settlement.Status, settlement.Reason = "succeeded", "command_completed"
	if waitErr != nil || processResult.ExitCode != 0 {
		settlement.Status, settlement.Reason = "failed", "command_failed"
	}
	if err := b.authority.CompleteProcessHandle(handle); err != nil {
		return Result{}, err
	}
	handleSnapshot, err := b.authority.ProcessHandleSnapshot(handle)
	if err != nil {
		return Result{}, err
	}
	return Result{
		Process: processResult, Handle: handleSnapshot,
	}, waitErr
}

func validateCommand(request CommandRequest) error {
	operation := request.Validation.Operation
	if err := operation.Validate(); err != nil {
		return err
	}
	if operation.Process == nil {
		return errors.New("process operation has no process intent")
	}
	digest, err := authority.ManagedProcessArgumentsDigest(
		request.Options.Path,
		request.Options.Args,
		request.Options.Env,
		request.Options.Dir,
	)
	if err != nil {
		return err
	}
	if operation.Process.ArgumentsDigest != digest {
		return errors.New("process command does not match the execution operation")
	}
	return nil
}
