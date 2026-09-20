package wire

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/fwtllh-png/QCode/internal/platform/envprobe"

	sessionhistory "github.com/fwtllh-png/QCode/internal/persist/history"
	persiststate "github.com/fwtllh-png/QCode/internal/persist/state"

	reverttool "github.com/fwtllh-png/QCode/internal/adapter/tool/revert"
	"github.com/fwtllh-png/QCode/internal/observability/verify"
	"github.com/fwtllh-png/QCode/internal/orchestration/subagent"
	agentengine "github.com/fwtllh-png/QCode/internal/runtime/agent/engine"
	promptcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/prompt"
	"github.com/fwtllh-png/QCode/internal/runtime/app"
	apppersistence "github.com/fwtllh-png/QCode/internal/runtime/app/persistence"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	"github.com/fwtllh-png/QCode/internal/security/policy"
)

type agentModule struct{}

func (agentModule) Name() string { return "agent" }

func (agentModule) Build(ctx context.Context, state *buildState) error {
	session := state.session
	execution := state.config.execution
	snapshot := state.config.snapshot
	route := state.provider.route
	delegation := ""
	if state.orchestration.subagents != nil {
		delegation = state.orchestration.subagents.Policy().Instructions()
	}
	toolPrefix := promptcontext.ToolInstructions(execution.Tools, delegation)
	budgets := routePromptBudgets(
		state.options.PromptBudgets, route, execution.MaxOutputTokens,
		execution.TurnBudgetTokens,
		execution.BudgetTokens,
	)
	baseSystem := strings.TrimSpace(execution.BaseSystem)
	if baseSystem == "" {
		baseSystem = promptcontext.DefaultBaseSystem(
			execution.Workspace, envprobe.Fingerprint(),
		)
	}
	home := ""
	if resolved, err := os.UserHomeDir(); err == nil {
		home = resolved
	}
	prompt, err := promptcontext.Assemble(promptcontext.Options{
		BaseSystem: baseSystem,

		ToolPrefix: toolPrefix,
		Budgets:    budgets,
		Home:       home,

		MemoryEnabled: false,
		Constitution:  session.constitutionPrompt, WorkingSet: promptWorkingSet(state.options.WorkingSet), Workspace: execution.Workspace,
	})
	if err != nil {
		return fmt.Errorf("assemble prompt context: %w", err)
	}
	workspaceID, err := bindMemoryScopes(
		session.memory,
		execution.Workspace,
		state.options.WorkspaceIdentity.RootID,
	)
	if err != nil {
		return err
	}
	repoContext := newRepoContext(
		state.platform.repositoryIndex,
		snapshot.Config.Context,
		budgets,
		execution.Workspace,
	)
	workspaceTurnGate, approvalPosture := engineSecurityPolicy(state)
	reasoningEffort := effectiveReasoningEffort(route, execution.ReasoningEffort)
	if err := validateRouteReasoning(route, reasoningEffort); err != nil {
		return err
	}
	modelCapabilities := route.Model().Capabilities
	// The sample gate capacity is the operator-declared provider concurrency
	// (execution.max_concurrent) — the same ceiling the provider HTTP client
	// enforces below this gate.
	sharedRateLimit := agentengine.NewSharedRateLimit(execution.MaxConcurrent)
	if state.orchestration.subagents != nil {
		state.orchestration.subagents.BindProviderGate(sharedRateLimit.Hot)
	}
	contextRuntime, err := buildContextRuntime(
		state.options.PersistentStore,
		state.config.runtimeSessionID,
	)
	if err != nil {
		return fmt.Errorf("create context runtime: %w", err)
	}
	session.turnCoordinators = contextRuntime.durable
	catalog := state.tools.skillCatalog
	seedOptions := agentengine.Options{ProviderConfig: agentengine.ProviderConfig{Provider: state.provider.provider,
		Route:            route,
		Routes:           state.provider.routes,
		SelectableRoutes: state.provider.selectableRoutes,

		MaxOutputTokens: execution.MaxOutputTokens,

		ReasoningEffort: reasoningEffort,
		NativeSearch:    execution.NativeSearch,

		MaxSteps:                   execution.MaxSteps,
		ImplementNoProgressSamples: execution.ImplementNoProgressSamples,
		MaxRetries:                 execution.ProviderRetryLimit,
		MaxRetryDelay:              execution.Timeout,
		RateLimitMaxRetries:        execution.RateLimitRetryLimit,
		RateLimitMaxWait:           execution.RateLimitWaitBudget(),
		SharedRateLimit:            sharedRateLimit,
		TokensPerMinute:            execution.TokensPerMinute,
	}, ContextConfig: agentengine.ContextConfig{StaticContext: prompt.Messages,
		ContextBudgets: budgets,
		CodingPolicy:   execution.Tools && snapshot.Config.Context.CodingPolicy.Enabled,

		Budget: agentengine.Budget{
			MaxTokens:     execution.BudgetTokens,
			MaxTurnTokens: execution.TurnBudgetTokens,
			MaxCostUSD:    execution.BudgetUSD,
		},

		WorkingSet:            prompt.WorkingSet,
		CriticalPaths:         prompt.CriticalPaths,
		StaticContextReceipts: prompt.Receipts,
		RepoContext:           repoContext,
		WorkingSetLimit:       snapshot.Config.Context.WorkingSet.MaxEntries,
		EvidenceLimit:         snapshot.Config.Context.Evidence.MaxEntries,
		SummaryMaxBytes:       snapshot.Config.Context.Compact.SummaryMaxBytes,
		MaxDigestEntries:      snapshot.Config.Context.Compact.MaxDigestEntries,
		Context:               engineContextPolicy(snapshot.Config.Context),

		PromptCacheKey: promptcontext.StickyCacheKey(
			state.config.runtimeSessionID,
			execution.Workspace,
		),

		TurnSnapshots: agentengine.TurnSnapshotSources{
			Memory: memorySnapshotSource(
				session.memory,
				snapshot.Config.Memory,
				budgets[promptcontext.PartitionUserMemory],
			),
			MCP: func() []agentengine.MCPHealthSnapshot {
				if session.mcpPool == nil {
					return nil
				}
				snapshots := session.mcpPool.HealthSnapshots()
				result := make([]agentengine.MCPHealthSnapshot, 0, len(snapshots))
				for _, snapshot := range snapshots {
					result = append(result, agentengine.MCPHealthSnapshot{
						Server:              snapshot.Server,
						State:               snapshot.State,
						ConsecutiveFailures: snapshot.ConsecutiveFailures,
						LastError:           snapshot.LastError,
						ChangedAt:           snapshot.ChangedAt,
						RetryAt:             snapshot.RetryAt,
					})
				}
				return result
			},
			SkillSelection: func(
				query string,
			) ([]agentengine.SkillSummary, agentengine.SkillSelectionMetrics, error) {
				return selectTurnSkills(catalog, query)
			},
		}}, ToolConfig: agentengine.ToolConfig{Tools: state.tools.registry,

		OnNetworkAllow: state.security.guardFactory.onNetworkAllow,

		Diagnostics: state.security.diagnostics,
		Verify: agentengine.VerifyOptions{
			Mode:           execution.Verify.Mode,
			Scope:          verify.Scope(execution.Verify.Scope),
			OnFailure:      execution.Verify.OnFailure,
			MaxRepairSteps: execution.Verify.MaxRepairSteps,
			Timeout:        execution.Verify.Timeout,
			Runner:         state.security.verify,
		},
		RequireCompletionDeclaration: execution.Tools,

		ToolCatalogSync: func() error {
			if session.mcpPrewarm == nil {
				return nil
			}
			return session.mcpPrewarm.SyncCatalog()
		}}, SecurityConfig: agentengine.SecurityConfig{Security: state.security.runtime,
		ProfilePermissionCeiling: approvalPosture,
		Workspace:                execution.Workspace,
		WorkspaceIdentity:        workspaceID,
		WorkspaceIsolation:       "shared",

		Journal:           state.security.journal,
		WorkspaceTurnGate: workspaceTurnGate}, TelemetryConfig: agentengine.TelemetryConfig{Metrics: session.metrics,
		Observability: engineObservability(state)}, LifecycleConfig: agentengine.LifecycleConfig{TurnCoordinatorRuntime: contextRuntime.coordinator,
		DelegationMode: execution.Subagent.Delegation,
		ReleaseTurnResources: session.turnProcessReleaser(
			session.processes,
			"main",
		),

		SessionID: state.config.runtimeSessionID,
		InputHost: session.inputHost},
	}
	if store := state.options.PersistentStore; store != nil {
		seedOptions.TurnContexts = apppersistence.NewContextRebaseRepository(store)
		seedOptions.SessionForTurn = func(
			ctx context.Context,
			turnID string,
		) (string, bool) {
			sessionID, err := store.SessionForTurn(ctx, turnID)
			return sessionID, err == nil && sessionID != ""
		}
		// turn_history falls back to the durable terminal envelopes when
		// compaction removes closed turns from memory.
		seedOptions.TurnTranscriptArchive = app.NewTurnTranscriptArchive(
			persiststate.NewWorkspaceTerminalStore(
				store.SQLite(), execution.Workspace,
			),
			persiststate.NewSharedContentStore(store.Content()),
		)
		// A restarted process rebuilds the running Turn's accepted
		// conversation from the same durable content store.
		seedOptions.TurnContinuations = persiststate.NewSharedContentStore(
			store.Content(),
		)
	}
	defaultProfile := protocol.SessionProfile{
		Version: protocol.SessionProfileVersion, Revision: 1,
		Mode:                execution.Mode,
		PlanningPolicy:      "adaptive",
		Provider:            route.ProviderID(),
		Model:               route.Model().ID,
		ReasoningEffort:     reasoningEffort,
		ApprovalPosture:     string(approvalPosture),
		ExecutionTarget:     "local",
		MaxSteps:            execution.MaxSteps,
		PromptCacheRevision: 1,
	}
	session.configuration.profile = defaultProfile
	profileModels, modelFields := runtimeProfileModels(
		state.provider.modelCatalog,
		defaultProfile.Provider,
		state.provider.modelCapabilities,
	)
	mutableFields := mutableSessionProfileFields(
		modelFields, modelCapabilities.ToolCalls,
		approvalPosture != policy.PermissionNever,
	)
	profileCapabilities := protocol.SessionProfileCapabilities{
		Provider:          defaultProfile.Provider,
		Model:             defaultProfile.Model,
		ModelCapabilities: state.provider.modelCapabilities,
		MutableFields:     mutableFields,
	}
	workspaceIdentity := state.options.WorkspaceIdentity
	childToolsets := state.orchestration.childToolsets
	coreBuilder := runtimeCoreBuilder{
		seed: seedOptions, guardFactory: state.security.guardFactory,
		approvalObserver:    session.metrics.Approval,
		workspaceIdentity:   workspaceIdentity,
		childTools:          childToolsets,
		turnProcessReleaser: session.turnProcessReleaser,
	}
	threadManager := app.NewThreadManager(coreBuilder.BuildMain)
	if state.security.journal != nil {
		threadManager.SetHostJournal(state.security.journal)
	}
	session.threads = threadManager
	subagent.BindRuntimeContext(state.orchestration.subagents, threadManager)
	threadManager.SetChildFactory(coreBuilder.BuildChild)
	session.chatWorkspaces = buildChatWorkspaces(
		state, threadManager, workspaceTurnGate,
	)
	if state.options.PersistentStore != nil {
		store := state.options.PersistentStore
		threadManager.SetWindowRestorer(
			func(
				ctx context.Context,
				threadID protocol.ThreadID,
			) (*protocol.ThreadCompactedData, error) {
				return sessionhistory.LatestThreadHistorySeed(ctx, store, threadID)
			},
		)
		threadManager.SetSequenceReader(
			func(ctx context.Context) (protocol.Cursor, error) {
				return store.LastSequence(ctx)
			},
		)
	}
	if state.security.journal != nil {
		if err := reverttool.Register(
			state.tools.registry,
			reverttool.Options{Reverter: reverttool.EngineReverter{
				RevertFn: func(
					ctx context.Context,
					targetTurnID string,
				) ([]string, []string, error) {
					receipt, revertErr := threadManager.RevertWorkspace(
						ctx,
						targetTurnID,
					)
					conflicts := make([]string, len(receipt.Conflicts))
					for index, conflict := range receipt.Conflicts {
						conflicts[index] = conflict.Path + ": " + conflict.Reason
					}
					return receipt.Restored, conflicts, revertErr
				},
				DefaultTurnFn: threadManager.LastTurnID,
			}},
		); err != nil {
			return fmt.Errorf("revert_turn tool: %w", err)
		}
	}
	session.applyPlan = threadManager.ApplyPlan
	state.agent = agentBuildState{
		defaultProfile:      defaultProfile,
		profileCapabilities: profileCapabilities,
		profileModels:       profileModels,
		threads:             threadManager,
	}
	return nil
}

type runtimeModule struct{}

func (runtimeModule) Name() string { return "runtime" }

func (runtimeModule) Build(
	ctx context.Context,
	state *buildState,
) error {
	session := state.session
	if state.options.PersistentStore != nil {
		runtime, err := PreparePersistentRuntime(ctx, PersistentRuntimeOptions{
			Store:               state.options.PersistentStore,
			WorkspaceRoot:       state.config.execution.Workspace,
			Engine:              state.agent.threads,
			OperationBuffer:     state.config.snapshot.Config.Runtime.OperationBuffer,
			SubscriberBuffer:    state.config.snapshot.Config.Runtime.SubscriberBuffer,
			Observability:       runtimeObservability(state),
			DefaultProfile:      state.agent.defaultProfile,
			ToolCatalog:         state.tools.registry,
			ProfileCapabilities: state.agent.profileCapabilities,
			ProfileModels:       state.agent.profileModels,
			SessionWorkspaces:   session.chatWorkspaces,
			GitControl:          buildGitControl(state),
		})
		if err != nil {
			return fmt.Errorf("create persistent runtime: %w", err)
		}
		session.Runtime = runtime
	} else {
		runtime, err := app.PrepareRuntime(ctx, app.Options{
			Engine:        state.agent.threads,
			WorkspaceRoot: state.config.execution.Workspace,
			ContentStore:  session.content,
			Observability: runtimeObservability(state),
			GitControl:    buildGitControl(state),
		})
		if err != nil {
			return fmt.Errorf("prepare runtime: %w", err)
		}
		session.Runtime = runtime
	}
	if store := state.options.PersistentStore; store != nil &&
		state.orchestration.subagents != nil {
		control := state.orchestration.subagents
		if err := ConfigurePersistentSubagents(
			state.agent.threads, store,
			state.config.execution.Workspace,
			state.config.runtimeSessionID,
			session.Runtime,
			func(graph any) error {
				return control.AttachGraph(graph.(subagent.Graph))
			},
		); err != nil {
			return fmt.Errorf("attach live agent graph: %w", err)
		}
	}
	if state.orchestration.children != nil {
		if err := state.orchestration.children.bind(
			session.Runtime,
			state.agent.threads,
			state.orchestration.subagents,
		); err != nil {
			return fmt.Errorf("bind child runtime: %w", err)
		}
		session.children = state.orchestration.children
		session.subagents = state.orchestration.subagents
	}
	state.runtime.application = session.Runtime
	return nil
}
