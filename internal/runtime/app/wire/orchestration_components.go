package wire

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	agenttool "github.com/fwtllh-png/QCode/internal/adapter/tool/agent"
	filetool "github.com/fwtllh-png/QCode/internal/adapter/tool/file"
	interacttool "github.com/fwtllh-png/QCode/internal/adapter/tool/interact"
	"github.com/fwtllh-png/QCode/internal/config"
	workbudget "github.com/fwtllh-png/QCode/internal/orchestration/budget"
	"github.com/fwtllh-png/QCode/internal/orchestration/subagent"
	sessionstate "github.com/fwtllh-png/QCode/internal/persist/session"
	persiststate "github.com/fwtllh-png/QCode/internal/persist/state"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func buildChildOrchestration(
	ctx context.Context,
	state *buildState,
	output *orchestrationBuildState,
) error {
	session, execution := state.session, state.config.execution
	limits := effectiveSubagentLimits(execution.Subagent, execution.TurnBudgetTokens)
	output.childGovernor = newChildGovernor(limits)
	childRoot := childStateRoot(state)
	agentRoot := filepath.Join(childRoot, "agents")
	if err := os.MkdirAll(agentRoot, 0o700); err != nil {
		return fmt.Errorf("agent root: %w", err)
	}
	childTrees, err := newChildWorktrees(
		execution.Workspace, agentRoot, limits.Workspace, state.platform.backend,
		state.platform.leaseAuthority, execution.LeaseTimeout,
	)
	if err != nil {
		return fmt.Errorf("child worktrees: %w", err)
	}
	gitCommonDir, err := childTrees.commonGitDir(ctx)
	if err != nil {
		return fmt.Errorf("resolve repository Git metadata: %w", err)
	}
	output.childToolsets = newChildToolsets(
		state.platform.helperPath, session.content, state.platform.web,
		execution.Verify, execution.Journal, state.config.diagnosticCommands,
		state.config.diagnosticReadRoots, state.config.diagnosticReadFiles,
		gitCommonDir, sandbox.BackendManagedProxyPort(state.platform.backend),
		state.config.workspaceStateRoot,
		state.config.skillPaths,
	)
	output.childToolsets.environment = execution.Environment
	output.childToolsets.bindParentSandbox(state.platform.backend)
	session.childTools = output.childToolsets
	chatRoot := filepath.Join(childRoot, "chats")
	if err := os.MkdirAll(chatRoot, 0o700); err != nil {
		return fmt.Errorf("Chat worktree root: %w", err)
	}
	output.chatTrees, err = newChildWorktrees(
		execution.Workspace, chatRoot, config.SubagentWorkspaceAuto,
		state.platform.backend, state.platform.leaseAuthority, execution.LeaseTimeout,
	)
	if err != nil {
		return fmt.Errorf("Chat worktrees: %w", err)
	}
	output.children = newChildRuntime(
		limits, execution.Workspace, output.childGovernor, output.childToolsets,
	)
	output.workBudget = workbudget.NewLedger()
	output.children.useBudget(output.workBudget)
	workspaceIdentity, err := sessionstate.NormalizeWorkspaceRoot(execution.Workspace)
	if err != nil {
		return fmt.Errorf("normalize agent workspace: %w", err)
	}
	output.subagents, err = subagent.OpenControl(subagent.Options{
		Root: agentRoot, Gate: state.security.guard,

		Runtime: output.children, Worktrees: childTrees, Budget: subagent.Budget{
			MaxTokens: limits.MaxTokens, MaxCostUSD: limits.MaxCostUSD, MaxDepth: limits.MaxDepth, MaxParallel: limits.MaxParallel, MaxResident: limits.MaxResident, MaxTotal: limits.MaxTotal,
		}, Workspace: workspaceIdentity, SessionID: state.config.runtimeSessionID,
	}, subagent.DelegationMode(limits.Delegation))
	if err != nil {
		return fmt.Errorf("agent control: %w", err)
	}
	output.childToolsets.bindAgents(output.subagents, state.config.runtimeSessionID, output.children.release)
	output.parentFiles, err = filetool.NewWithBackend(
		execution.Workspace,
		state.platform.backend,
	)
	if err != nil {
		return fmt.Errorf("parent file tools for integrate_agent: %w", err)
	}
	if err := agenttool.Register(state.tools.registry, agenttool.Options{
		Control: output.subagents, Handles: state.tools.handleStore,

		Root: agentRoot, Gate: state.security.guard,
		Graph: persiststate.NewAgentGraph(
			state.options.PersistentStore, execution.Workspace, state.config.runtimeSessionID,
		),
		Files:   output.parentFiles,
		Sandbox: state.platform.backend, OnRelease: output.children.release, Verify: state.security.verify, Workspace: execution.Workspace, SessionID: state.config.runtimeSessionID,
	}); err != nil {
		return fmt.Errorf("agent tool: %w", err)
	}
	return nil
}

func buildInteractionOrchestration(
	_ context.Context,
	state *buildState,
	output *orchestrationBuildState,
) error {
	execution := state.config.execution
	host := interacttool.NewHost(0)
	var vision interacttool.VisionClient
	if _, configured := state.config.snapshot.Config.Route.Slots[string(model.PurposeVision)]; configured {
		route, err := state.provider.routes.For(model.PurposeVision)
		if err != nil {
			return fmt.Errorf("vision route: %w", err)
		}
		vision = interacttool.RouteVision{
			Provider: state.provider.toolSampler, Route: route,
		}
	}
	session := state.session
	applyPlan := func(plan interacttool.Plan) error {
		if session.applyPlan != nil {
			return session.applyPlan(plan)
		}
		return nil
	}
	if err := interacttool.Register(state.tools.registry, interacttool.Options{
		Host: host, Backend: state.platform.backend,
		Vision: vision,
		OnPlan: applyPlan, Workspace: execution.Workspace,
	}); err != nil {
		return fmt.Errorf("interact tools: %w", err)
	}
	session.inputHost = host
	if tools := session.childTools; tools != nil {
		tools.bindInteractions(vision, applyPlan)
	}
	return nil
}
