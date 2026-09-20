package wire

import (
	"errors"

	toolguard "github.com/fwtllh-png/QCode/internal/adapter/tool/guard"
	"github.com/fwtllh-png/QCode/internal/persist/workspacejournal"
	"github.com/fwtllh-png/QCode/internal/platform/process"
	agentengine "github.com/fwtllh-png/QCode/internal/runtime/agent/engine"
	"github.com/fwtllh-png/QCode/internal/runtime/app"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type runtimeCoreBuilder struct {
	seed                agentengine.Options
	guardFactory        guardFactory
	approvalObserver    toolguard.ApprovalObserver
	workspaceIdentity   protocol.WorkspaceIdentity
	childTools          *childToolsets
	turnProcessReleaser func(*process.SessionManager, string) func(agentengine.TurnIdentity)
}

func (b runtimeCoreBuilder) BuildMain() (*app.EngineAdapter, error) {
	options := b.seed
	options.Security = cloneThreadSecurity(b.seed.Security)
	return b.build(options, nil)
}

func (b runtimeCoreBuilder) BuildChild(spec app.ChildSpec) (*app.EngineAdapter, error) {
	options := childEngineOptions(b.seed, spec)
	openedToolset := ""
	if !spec.Serialized && (!spec.ReadOnly || spec.Workspace != spec.HostWorkspace) {
		if b.childTools == nil {
			return nil, errors.New("child toolsets are not configured")
		}
		toolset, err := b.childTools.open(spec.Workspace, spec.HostSeeded)
		if err != nil {
			return nil, err
		}
		openedToolset = spec.Workspace
		options.Tools = toolset.registry
		options.TurnSnapshots.SkillSelection = func(
			query string,
		) ([]agentengine.SkillSummary, agentengine.SkillSelectionMetrics, error) {
			return selectTurnSkills(toolset.skillCatalog, query)
		}
		options.Journal, options.InputHost = toolset.journal, toolset.inputHost
		options.ReadTracker = workspacejournal.NewReadTracker()
		options.Diagnostics = toolset.diagnostics
		options.Verify.Runner = toolset.verify
		if b.turnProcessReleaser != nil {
			options.ReleaseTurnResources = b.turnProcessReleaser(toolset.processes, "child")
		}
	}
	restrictChildTools(options.Security, spec, b.seed.Tools, options.Tools)
	var source *protocol.ApprovalSource
	if spec.AgentID != "" && spec.AgentPath != "" {
		source = &protocol.ApprovalSource{
			Kind: "agent", AgentID: spec.AgentID,
			AgentPath: spec.AgentPath, ParentPath: spec.ParentPath,
			Role: spec.Role, SessionID: spec.SessionID,
			WorkspaceRoot: spec.HostWorkspace,
		}
	}
	adapter, err := b.build(options, source)
	if err != nil && openedToolset != "" {
		b.childTools.release(openedToolset)
	}
	return adapter, err
}

func (b runtimeCoreBuilder) build(
	options agentengine.Options, source *protocol.ApprovalSource,
) (*app.EngineAdapter, error) {
	bindEngineGuardFactory(&options, b.guardFactory, b.approvalObserver)
	worker, err := agentengine.New(options)
	if err != nil {
		return nil, err
	}
	adapter := app.AdaptEngineWithWorkspaceIdentity(worker, b.workspaceIdentity)
	if source != nil {
		adapter.SetApprovalSource(*source)
	}
	return adapter, nil
}
