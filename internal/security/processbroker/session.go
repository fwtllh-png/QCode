package processbroker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"time"

	"github.com/fwtllh-png/QCode/internal/platform/process"
	"github.com/fwtllh-png/QCode/internal/security/authority"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

type SessionRequest struct {
	Lease      authority.ExecutionLease
	Validation authority.LeaseValidation
	Options    process.SessionOptions
}

// SessionCommandDigest binds the concrete launch and owner. The prepared
// environment is immutable; neither this function nor launch recaptures HOME.
func SessionCommandDigest(options process.SessionOptions) (string, error) {
	env, err := (process.Options{Env: options.Env, Environment: options.Environment}).BoundEnvironment()
	if err != nil {
		return "", err
	}
	material := struct {
		Command, DisplayCommand, Directory, Thread, Turn, Call, Target, Session, Task string
		Environment                                                                   []string
		PTY, Detached                                                                 bool
		Rows, Cols                                                                    uint16
		Timeout                                                                       time.Duration
	}{options.Command, options.DisplayCommand, options.Dir, options.ThreadID, options.TurnID, options.CallID, options.ExecutionTarget, options.SessionID, options.LinkedTaskID, env, options.PTY, options.DetachFromCaller, options.Rows, options.Cols, options.Timeout}
	encoded, err := json.Marshal(material)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

// StartSession consumes one launch lease. Subsequent interaction uses the
// existing SessionManager owner checks and its durable process lifecycle.
func (b *Broker) StartSession(ctx context.Context, manager *process.SessionManager, request SessionRequest) (string, error) {
	if b == nil || b.authority == nil || manager == nil {
		return "", errors.New("session broker and manager are required")
	}
	if err := request.Validation.Operation.Validate(); err != nil {
		return "", err
	}
	digest, err := SessionCommandDigest(request.Options)
	if err != nil {
		return "", err
	}
	intent := request.Validation.Operation.Process
	if intent == nil || intent.PreparedCommandDigest != digest {
		return "", errors.New("prepared session does not match the execution operation")
	}
	opts := request.Options
	if opts.DirFile == nil || opts.Environment == nil {
		return "", errors.New("host session requires a pinned cwd and prepared environment")
	}
	pinned, err := opts.DirFile.Stat()
	if err != nil {
		return "", err
	}
	current, err := os.Stat(opts.Dir)
	if err != nil {
		return "", err
	}
	if !pinned.IsDir() || !os.SameFile(pinned, current) {
		return "", errors.New("host session cwd does not match its pinned directory")
	}
	if opts.ExecutionTarget != "host" || opts.Sandbox != nil || opts.RequireSandbox || opts.WorkspaceReadOnly || len(opts.WorkspaceWritePaths) != 0 || opts.DenyNetwork || opts.SessionProxyPort != 0 || opts.SessionProxyCredential != "" || opts.Network != nil || opts.TrustedRuntimeHelper {
		return "", errors.New("host session cannot claim sandbox restrictions or trusted-helper authority")
	}
	var id string
	_, err = b.authority.RunSettled(request.Lease, request.Validation, "session_start_failed", time.Now, func(settlement *authority.Settlement) error {
		snapshot, err := b.authority.Snapshot(request.Lease)
		if err != nil {
			return err
		}
		a := snapshot.PermissionProfile.ExecutionAuthorityFor(request.Validation.Operation)
		if !a.HostExecution {
			return errors.New("session lease does not authorize host execution")
		}
		runCtx, err := sandbox.WithExecutionAuthority(ctx, a)
		if err != nil {
			return err
		}
		id, err = manager.Create(runCtx, opts)
		if err == nil {
			settlement.Status, settlement.Reason = "succeeded", "session_started"
		}
		return err
	})
	return id, err
}
