package shell

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/observability/verify"
	"github.com/fwtllh-png/QCode/internal/platform/process"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

type isolatedCommand struct {
	workspace *sandbox.Workspace
	backend   sandbox.Backend
	session   tool.IsolatedWorkspace
	// shadow marks a discard-only isolate: Settle summarizes planned
	// changes and the caller attaches discard metadata instead of
	// settlement facts.
	shadow bool
}

// pendingExecution is what a still-running exec_command leaves behind: the
// isolated workspace to settle on its final poll, and the verification
// evidence that must reach a terminal status once the process exits. Both
// are reclaimed when the session closes without a final poll (turn release,
// timeout): the isolate is closed and the evidence is dropped.
type pendingExecution struct {
	isolated isolatedCommand
	evidence *verify.Evidence
}

func (p *commandProtocol) storePendingExecution(
	id string,
	isolated isolatedCommand,
	evidence *verify.Evidence,
) {
	if p == nil || id == "" || (isolated.session == nil && evidence == nil) {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.isolates == nil {
		p.isolates = make(map[string]pendingExecution)
	}
	p.isolates[id] = pendingExecution{isolated: isolated, evidence: evidence}
}

func (p *commandProtocol) takePendingExecution(id string) (pendingExecution, bool) {
	if p == nil || id == "" {
		return pendingExecution{}, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	pending, ok := p.isolates[id]
	if ok {
		delete(p.isolates, id)
	}
	return pending, ok
}

// reclaimAbandonedExecution is the session OnClose hook: an exec_command
// whose session closed without a final write_stdin poll (turn release,
// timeout, capacity eviction) must not leak its isolated workspace — the
// worktree and scratch copy are discarded. Abandoned verification evidence
// is dropped; it never observed a final exit status.
func (p *commandProtocol) reclaimAbandonedExecution(id string) {
	pending, ok := p.takePendingExecution(id)
	if !ok {
		return
	}
	if pending.isolated.session != nil {
		_ = pending.isolated.Close()
	}
}

// beginIsolatedCommand prepares an isolated workspace for the command's
// write trees. The returned degraded flag reports the apply-mode fallback
// where write trees exist but no isolator is bound: the command then runs
// in place under its exact write grants and the caller must say so in the
// result metadata. Shadow (settle=discard) never degrades silently: without
// an isolable tree there is no way to honor "writes are dropped", so it
// fails closed instead of touching the real workspace.
func (p *commandProtocol) beginIsolatedCommand(
	ctx context.Context,
	writePaths []string,
	shadow bool,
) (isolatedCommand, bool, error) {
	trees := existingWriteTrees(p.workspace, writePaths)
	if len(trees) == 0 {
		if shadow {
			return isolatedCommand{}, false, errors.New(
				"settle=discard requires write_paths to include an existing " +
					"directory tree so the writes can be isolated and dropped; " +
					"exact-file paths cannot be discarded",
			)
		}
		return isolatedCommand{}, false, nil
	}
	isolator := tool.IsolatorFrom(ctx)
	if isolator == nil {
		if shadow {
			return isolatedCommand{}, false, errors.New(
				"settle=discard requires a workspace isolator and none is bound",
			)
		}
		return isolatedCommand{}, true, nil
	}
	identity := tool.InvocationIdentityFrom(ctx)
	id := identity.CallID
	if id == "" {
		id = identity.TurnID
	}
	if id == "" {
		id = identity.SessionID
	}
	var session tool.IsolatedWorkspace
	var err error
	if shadow {
		session, err = isolator.BeginShadow(ctx, id, trees)
	} else {
		session, err = isolator.Begin(ctx, id, trees)
	}
	if err != nil {
		return isolatedCommand{}, false, fmt.Errorf("isolate write trees: %w", err)
	}
	workspace, err := sandbox.NewWorkspace(session.Root())
	if err != nil {
		_ = session.Close()
		return isolatedCommand{}, false, err
	}
	backend, _, err := session.PrepareBackend(p.backend)
	if err != nil {
		_ = session.Close()
		return isolatedCommand{}, false, err
	}
	return isolatedCommand{
		workspace: workspace,
		backend:   backend,
		session:   session,
		shadow:    shadow,
	}, false, nil
}

func existingWriteTrees(workspace *sandbox.Workspace, paths []string) []string {
	if workspace == nil {
		return nil
	}
	var trees []string
	for _, path := range paths {
		resolved, err := workspace.Resolve(path, sandbox.AllowMissing)
		if err != nil {
			continue
		}
		info, err := os.Lstat(resolved)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if resolved == workspace.Root() {
			continue
		}
		relative, err := filepath.Rel(workspace.Root(), resolved)
		if err != nil {
			continue
		}
		trees = append(trees, filepath.ToSlash(relative))
	}
	return trees
}

func (s isolatedCommand) Close() error {
	if s.session == nil {
		return nil
	}
	return s.session.Close()
}

func attachIsolatedCWD(result *tool.Result, session tool.IsolatedWorkspace) {
	if result == nil || session == nil {
		return
	}
	if result.Metadata == nil {
		result.Metadata = make(map[string]any)
	}
	result.Metadata["isolated_cwd"] = session.Root()
}

func attachIsolatedSettlement(
	result *tool.Result,
	session tool.IsolatedWorkspace,
	changes []tool.WorkspaceChange,
) {
	attachIsolatedCWD(result, session)
	if result.Metadata == nil {
		result.Metadata = make(map[string]any)
	}
	result.Metadata["workspace_settlement"] = "isolated_three_way"
	if len(changes) == 0 {
		return
	}
	facts := tool.EnsureOutcomeFacts(result)
	facts.WorkspaceChanges = append(facts.WorkspaceChanges, changes...)
	result.Metadata["observed_changes"] = len(facts.WorkspaceChanges)
}

func settleIsolated(
	ctx context.Context,
	isolated isolatedCommand,
	result *tool.Result,
) error {
	if isolated.session == nil {
		return nil
	}
	changes, err := isolated.session.Settle(ctx)
	if err != nil {
		_ = isolated.Close()
		return err
	}
	if isolated.shadow {
		// Discarded writes are diagnostics only: they must not enter
		// workspace settlement facts, observed changes, or the evidence
		// ledger, or the turn would count mutations that never landed.
		attachShadowDiscard(result, changes)
	} else {
		attachIsolatedSettlement(result, isolated.session, changes)
	}
	return isolated.Close()
}

func (p *commandProtocol) settlePendingExecution(
	ctx context.Context,
	id string,
	result *tool.Result,
	wait process.SessionWait,
) error {
	pending, ok := p.takePendingExecution(id)
	if !ok {
		return nil
	}
	return settleTakenPending(ctx, p.workspace.Root(), pending, result, wait)
}

// settleTakenPending finalizes one taken pending execution: verification
// evidence first reaches its terminal status from the final wait, then the
// isolated workspace settles, then covered-path invalidation re-judges
// passing evidence against settled writes. Callers that must take the
// pending state before closing the session (the write_stdin close path,
// where OnClose would otherwise reclaim it) use this directly.
func settleTakenPending(
	ctx context.Context,
	workspaceRoot string,
	pending pendingExecution,
	result *tool.Result,
	wait process.SessionWait,
) error {
	if pending.evidence != nil {
		attachVerification(result, pending.evidence, wait)
	}
	if err := settleIsolated(ctx, pending.isolated, result); err != nil {
		return err
	}
	if pending.evidence != nil {
		invalidateVerificationOnCoveredWrites(result, pending.evidence, workspaceRoot)
	}
	return nil
}

// shadowSummaryMaxPaths bounds the discarded-change path list on the result:
// a summary names the interesting paths without mirroring a whole build
// tree into metadata. Public contract constant; boundary tests pin it.
const shadowSummaryMaxPaths = 20

func attachShadowDiscard(result *tool.Result, changes []tool.WorkspaceChange) {
	if result == nil {
		return
	}
	if result.Metadata == nil {
		result.Metadata = make(map[string]any)
	}
	result.Metadata["workspace_settlement"] = "shadow_discarded"
	result.Metadata["discarded_changes"] = len(changes)
	if len(changes) == 0 {
		return
	}
	paths := make([]string, 0, min(len(changes), shadowSummaryMaxPaths))
	for _, change := range changes {
		if len(paths) >= shadowSummaryMaxPaths {
			break
		}
		paths = append(paths, change.Path)
	}
	result.Metadata["discarded_change_paths"] = paths
}
