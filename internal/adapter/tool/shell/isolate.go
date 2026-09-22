package shell

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
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

func (p *commandProtocol) beginIsolatedCommand(
	ctx context.Context,
	writePaths []string,
	shadow bool,
) (isolatedCommand, error) {
	trees := existingWriteTrees(p.workspace, writePaths)
	if len(trees) == 0 {
		return isolatedCommand{}, nil
	}
	isolator := tool.IsolatorFrom(ctx)
	if isolator == nil {
		return isolatedCommand{}, nil
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
		return isolatedCommand{}, fmt.Errorf("isolate write trees: %w", err)
	}
	workspace, err := sandbox.NewWorkspace(session.Root())
	if err != nil {
		_ = session.Close()
		return isolatedCommand{}, err
	}
	backend, _, err := session.PrepareBackend(p.backend)
	if err != nil {
		_ = session.Close()
		return isolatedCommand{}, err
	}
	return isolatedCommand{
		workspace: workspace,
		backend:   backend,
		session:   session,
		shadow:    shadow,
	}, nil
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

func (p *commandProtocol) storeIsolate(id string, isolated isolatedCommand) {
	if p == nil || id == "" || isolated.session == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.isolates == nil {
		p.isolates = make(map[string]isolatedCommand)
	}
	p.isolates[id] = isolated
}

func (p *commandProtocol) takeIsolate(id string) (isolatedCommand, bool) {
	if p == nil || id == "" {
		return isolatedCommand{}, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	isolated, ok := p.isolates[id]
	if ok {
		delete(p.isolates, id)
	}
	return isolated, ok
}

func (p *commandProtocol) settleIsolated(
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

func (p *commandProtocol) settleStoredIsolate(
	ctx context.Context,
	id string,
	result *tool.Result,
) error {
	isolated, ok := p.takeIsolate(id)
	if !ok {
		return nil
	}
	return p.settleIsolated(ctx, isolated, result)
}
