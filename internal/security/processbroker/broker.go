package processbroker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/fwtllh-png/QCode/internal/platform/process"
	"github.com/fwtllh-png/QCode/internal/security/artifactbroker"
	"github.com/fwtllh-png/QCode/internal/security/authority"
)

type Broker struct {
	authority  *authority.LeaseAuthority
	generation atomic.Uint64
}

type Identity struct {
	SessionID string
	ThreadID  string
	TurnID    string
}

type Request struct {
	Lease          authority.ExecutionLease
	Validation     authority.LeaseValidation
	Artifact       artifactbroker.Snapshot
	Args           []string
	Dir            string
	Identity       Identity
	MinimumRuntime time.Duration
}

type Result struct {
	Process    process.Result
	Survived   bool
	Handle     authority.ProcessHandleSnapshot
	Settlement authority.Settlement
}

func New(manager *authority.LeaseAuthority) (*Broker, error) {
	if manager == nil {
		return nil, errors.New("process broker requires a Lease Authority")
	}
	return &Broker{authority: manager}, nil
}

func (b *Broker) RunSmoke(
	ctx context.Context,
	request Request,
) (Result, error) {
	if b == nil || b.authority == nil {
		return Result{}, errors.New("process broker is required")
	}
	if request.MinimumRuntime <= 0 {
		return Result{}, errors.New("minimum process runtime must be positive")
	}
	if err := validateArtifact(request.Artifact, request.Validation); err != nil {
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
			result, runErr = b.runSmokeConsumed(ctx, request, settlement)
			return runErr
		},
	)
	result.Settlement = settlement
	return result, err
}

func (b *Broker) runSmokeConsumed(
	ctx context.Context,
	request Request,
	settlement *authority.Settlement,
) (result Result, resultErr error) {
	running, err := process.StartManaged(ctx, process.Options{
		Path: request.Artifact.ExecutablePath,
		Args: append([]string(nil), request.Args...),
		Dir:  request.Dir, OutputLimitBytes: process.ModelOutputLimitBytes,
	})
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
	timer := time.NewTimer(request.MinimumRuntime)
	defer timer.Stop()
	waitContext, cancelWait := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelWait()
	type completed struct {
		result process.Result
		err    error
	}
	finished := make(chan completed, 1)
	go func() {
		result, waitErr := running.Wait(waitContext)
		finished <- completed{result: result, err: waitErr}
	}()
	survived := false
	var completedRun completed
	select {
	case completedRun = <-finished:
	case <-timer.C:
		survived = true
		if err := b.validateHandle(request, handle, processID, generation, authority.ProcessCancel); err != nil {
			return Result{}, err
		}
		_ = running.Cancel()
		completedRun = <-finished
	case <-ctx.Done():
		if err := b.validateHandle(request, handle, processID, generation, authority.ProcessCancel); err != nil {
			return Result{}, err
		}
		_ = running.Cancel()
		completedRun = <-finished
		settlement.Status, settlement.Reason = "canceled", "context_canceled"
		return Result{Process: completedRun.result, Settlement: *settlement}, ctx.Err()
	}
	if err := b.validateHandle(request, handle, processID, generation, authority.ProcessWait); err != nil {
		return Result{}, err
	}
	settlement.Status, settlement.Reason = "succeeded", "minimum_runtime_observed"
	if !survived {
		settlement.Status, settlement.Reason = "failed", "command_exited_early"
	}
	if err := b.authority.CompleteProcessHandle(handle); err != nil {
		return Result{}, err
	}
	handleSnapshot, err := b.authority.ProcessHandleSnapshot(handle)
	if err != nil {
		return Result{}, err
	}
	return Result{
		Process: completedRun.result, Survived: survived,
		Handle: handleSnapshot, Settlement: *settlement,
	}, nil
}

func (b *Broker) validateHandle(
	request Request,
	handle authority.ProcessHandleCapability,
	processID string,
	generation uint64,
	action authority.ProcessAction,
) error {
	return b.authority.ValidateProcessHandle(
		handle,
		request.Identity.SessionID, request.Identity.ThreadID, request.Identity.TurnID,
		processID, generation, action,
	)
}

func validateArtifact(
	snapshot artifactbroker.Snapshot,
	validation authority.LeaseValidation,
) error {
	if err := snapshot.Manifest.Validate(); err != nil {
		return err
	}
	if validation.Operation.Artifact == nil ||
		validation.Operation.Artifact.ManifestDigest != snapshot.Manifest.Digest ||
		validation.Operation.Artifact.Generation != snapshot.Manifest.Generation ||
		validation.WorkspaceID != snapshot.Manifest.SourceWorkspaceID ||
		validation.WorkspaceGeneration != snapshot.Manifest.SourceWorkspaceGeneration {
		return errors.New("process artifact does not match the execution operation")
	}
	if filepath.Clean(snapshot.ExecutablePath) != filepath.Join(snapshot.Root, "payload") {
		return errors.New("process executable is outside the artifact snapshot")
	}
	info, err := os.Lstat(snapshot.ExecutablePath)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("process artifact executable is not a regular file")
	}
	file, err := os.Open(snapshot.ExecutablePath)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return err
	}
	entry := snapshot.Manifest.Entries[0]
	if size != entry.Size || hex.EncodeToString(hash.Sum(nil)) != entry.Digest ||
		!entry.Executable {
		return errors.New("process artifact snapshot changed after preparation")
	}
	return nil
}
