package shell

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/typed"
	"github.com/fwtllh-png/QCode/internal/common/tokenestimate"
	"github.com/fwtllh-png/QCode/internal/platform/process"
	"github.com/fwtllh-png/QCode/internal/security/authority"
	"github.com/fwtllh-png/QCode/internal/security/processbroker"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func validateExecutionTarget(input execCommandInput) error {
	switch input.ExecutionTarget {
	case "", "sandbox":
		return nil
	case "host":
		if len(input.WritePaths) != 0 || len(input.NetworkTargets) != 0 || input.AllowLoopback || input.Settle == "discard" {
			return errors.New("execution_target=host cannot enforce write_paths, network_targets, allow_loopback or settle=discard; use sandbox for scoped execution")
		}
		return nil
	default:
		return errors.New("execution_target must be sandbox or host")
	}
}

type preparedHostSession struct {
	options process.SessionOptions
}

type hostSessionGrant struct {
	prepared *preparedHostSession
	grant    authority.AuthorizedSessionGrant
	broker   *processbroker.Broker
}

type hostSessionKey struct{}

func (e *protocolExecutor) PrepareAuthorizedSession(ctx context.Context, invocation tool.PreparedInvocation) (authority.SessionBinding, error) {
	input, err := typed.DecodeStrict[execCommandInput](invocation.Arguments)
	if err != nil {
		return authority.SessionBinding{}, fmt.Errorf("%w: %v", tool.ErrInvalidArguments, err)
	}
	if !e.expand || input.ExecutionTarget != "host" {
		return authority.SessionBinding{}, errors.New("executor does not support host session preparation")
	}
	// Host preparation runs before the typed executor. Preserve its argument
	// error classification so a refused call can be corrected within the turn.
	if err := validateExecCommandInput(input); err != nil {
		return authority.SessionBinding{}, fmt.Errorf("%w: %v", tool.ErrInvalidArguments, err)
	}
	timeout, err := processTimeout(input.TimeoutMS)
	if err != nil {
		return authority.SessionBinding{}, err
	}
	if _, err = processOutputTokens(input.OutputTokens); err != nil {
		return authority.SessionBinding{}, err
	}
	prepared := &preparedHostSession{}
	directory, err := e.protocol.workspace.ResolveDirectory(input.CWD)
	if err != nil {
		return authority.SessionBinding{}, err
	}
	base, ok := sandbox.BackendPolicy(e.protocol.backend)
	if !ok {
		return authority.SessionBinding{}, errors.New("host execution requires a prepared environment policy")
	}
	environment, err := process.EnvironmentFromPolicy(base)
	if err != nil {
		return authority.SessionBinding{}, err
	}
	env, err := environmentEntries(input.Env)
	if err != nil {
		return authority.SessionBinding{}, err
	}
	identity := tool.InvocationIdentityFrom(ctx)
	threadID := strings.TrimSpace(identity.ThreadID)
	if threadID == "" {
		threadID = strings.TrimSpace(identity.SessionID)
	}
	if threadID == "" {
		threadID = strings.TrimSpace(identity.CallID)
	}
	if threadID == "" {
		return authority.SessionBinding{}, errors.New("host execution requires an owner thread")
	}
	directoryFile, err := e.protocol.workspace.OpenDirectory(input.CWD)
	if err != nil {
		return authority.SessionBinding{}, err
	}
	command := input.Command
	prepared.options = process.SessionOptions{
		Command: command, DisplayCommand: input.Command, Dir: directory, DirFile: directoryFile,
		Env: env, Environment: environment, ExecutionTarget: "host",
		ThreadID: threadID, TurnID: identity.TurnID, CallID: identity.CallID,
		Rows: input.Rows, Cols: input.Cols, PTY: input.TTY, Timeout: timeout,
		DetachFromCaller: true, OnClose: e.protocol.reclaimAbandonedExecution,
	}
	digest, err := processbroker.SessionCommandDigest(prepared.options)
	if err != nil {
		_ = directoryFile.Close()
		return authority.SessionBinding{}, err
	}
	return authority.SessionBinding{CommandDigest: digest, Value: prepared}, nil
}

func (e *protocolExecutor) ReleaseAuthorizedSession(binding authority.SessionBinding) error {
	prepared, ok := binding.Value.(*preparedHostSession)
	if !ok || prepared == nil {
		return errors.New("invalid prepared host session")
	}
	return prepared.options.DirFile.Close()
}

func (e *protocolExecutor) ExecuteAuthorizedSession(ctx context.Context, invocation tool.PreparedInvocation, grant authority.AuthorizedSessionGrant, leases *authority.LeaseAuthority) (tool.Result, tool.Outcome, error) {
	prepared, ok := grant.Prepared.(*preparedHostSession)
	if !ok || prepared == nil {
		return tool.Result{}, tool.Outcome{}, errors.New("missing prepared host session")
	}
	broker, err := processbroker.New(leases)
	if err != nil {
		return tool.Result{}, tool.Outcome{}, err
	}
	ctx = context.WithValue(ctx, hostSessionKey{}, hostSessionGrant{prepared: prepared, grant: grant, broker: broker})
	return e.runtime.ExecuteOutcome(ctx, invocation.Arguments)
}

func (p *commandProtocol) execHostCommand(ctx context.Context, input execCommandInput) (tool.Result, error) {
	grant, ok := ctx.Value(hostSessionKey{}).(hostSessionGrant)
	if !ok {
		return tool.Result{}, errors.New("host execution requires an authorized session lease")
	}
	if token := unsupportedPOSIXShellSyntax(input.Command); token != "" {
		return unsupportedSyntaxResult(token), nil
	}
	yield, err := processYield(input.YieldTimeMS, defaultExecYield)
	if err != nil {
		return tool.Result{}, err
	}
	tokens, err := processOutputTokens(input.OutputTokens)
	if err != nil {
		return tool.Result{}, err
	}
	id, err := grant.broker.StartSession(ctx, p.manager, processbroker.SessionRequest{
		Lease: grant.grant.Lease, Validation: grant.grant.Validation, Options: grant.prepared.options,
	})
	if err != nil {
		return tool.Result{}, err
	}
	owner := grant.prepared.options.ThreadID
	wait, output, err := p.waitInitial(ctx, id, owner, yield, int(tokenestimate.BytesForTokens(uint64(tokens))))
	if err != nil {
		return tool.Result{}, errors.Join(err, p.manager.Close(id, owner))
	}
	wait.Data = output.String()
	result := sessionResult(id, wait)
	result.Metadata["execution_target"] = "host"
	result.Metadata["enforcement"] = "none"
	attachCommandExecution(&result, id, wait.SessionRead, time.Since(wait.CreatedAt))
	if input.Description != "" {
		result.Metadata["description"] = input.Description
	}
	if output.Omitted() > 0 {
		result.Metadata["omitted_bytes"] = output.Omitted()
	}
	if wait.Running {
		result.Metadata["error_category"] = "process_still_running"
		result.Metadata["required_action"] = "write_stdin"
		result.Metadata["retry_original"] = false
	} else {
		err = p.manager.Close(id, owner)
		delete(result.Metadata, "session_id")
	}
	return result, err
}
