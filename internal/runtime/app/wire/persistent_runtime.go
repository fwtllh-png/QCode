// Package persistence composes durable Runtime repositories and lifecycle.
package wire

import (
	"context"
	"fmt"
	"strings"

	apppersistence "github.com/fwtllh-png/QCode/internal/runtime/app/persistence"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	tracestate "github.com/fwtllh-png/QCode/internal/observability/trace"
	"github.com/fwtllh-png/QCode/internal/persist/state"
	"github.com/fwtllh-png/QCode/internal/runtime/app"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

// Startup recovery quarantines unrestorable Turns through the terminal store.
var _ app.TurnQuarantineStore = (*state.WorkspaceTerminalStore)(nil)

type PersistentRuntimeOptions struct {
	Store               *state.Store
	WorkspaceRoot       string
	Engine              app.Engine
	OperationBuffer     int
	SubscriberBuffer    int
	Observability       app.RuntimeObservability
	DefaultProfile      protocol.SessionProfile
	ProfileCapabilities protocol.SessionProfileCapabilities
	ProfileModels       map[string]protocol.ModelCapabilities
	ToolCatalog         *tool.Registry
	SessionWorkspaces   app.SessionWorkspaceManager
	GitControl          *app.GitControl
}

// PreparePersistentRuntime restores static durable state without starting
// terminal projection or pending Turn recovery.
func PreparePersistentRuntime(
	ctx context.Context,
	options PersistentRuntimeOptions,
) (*app.Runtime, error) {
	repositories, err := apppersistence.NewPersistentRepositories(options.Store, options.WorkspaceRoot)
	if err != nil {
		return nil, err
	}
	presets, err := apppersistence.OpenAgentPresetStore(
		options.Store,
		options.WorkspaceRoot,
	)
	if err != nil {
		return nil, fmt.Errorf("open agent presets: %w", err)
	}
	terminalStore := state.NewWorkspaceTerminalStore(options.Store.SQLite(), options.WorkspaceRoot)
	contextRebases := apppersistence.NewContextRebaseRepository(options.Store)
	options.Observability.TraceQuery = tracestate.NewQueryService(
		repositories.Sessions,
		repositories.Trace,
		options.Observability.Runtime,
	)
	runtimeOptions := app.Options{
		Engine:             options.Engine,
		WorkspaceRoot:      options.WorkspaceRoot,
		EventStore:         state.NewWorkspaceEventStore(options.Store, options.WorkspaceRoot),
		ContentStore:       state.NewSharedContentStore(options.Store.Content()),
		Lifecycle:          repositories.Lifecycle,
		OperationBuffer:    options.OperationBuffer,
		SubscriberBuffer:   options.SubscriberBuffer,
		TerminalStore:      terminalStore,
		ContextRebaseStore: contextRebases,
		AgentPresets:       presets,
		Observability:      options.Observability,
		GitControl:         options.GitControl,
		// A failed durable write already waited out the store's lock budget,
		// so settlement retries are paced by that same budget.
		SettlementRetry: options.Store.SQLite().BusyTimeout(),
	}
	if options.DefaultProfile.Version != 0 {
		runtimeOptions.SessionProfiles = repositories.Sessions
		runtimeOptions.DefaultProfile = options.DefaultProfile
		runtimeOptions.ProfileCapabilities = options.ProfileCapabilities
		runtimeOptions.ProfileModels = options.ProfileModels
		runtimeOptions.ToolCatalog = options.ToolCatalog
		runtimeOptions.SessionLifecycle = repositories.Sessions
		runtimeOptions.SessionWorkspaces = options.SessionWorkspaces
		runtimeOptions.SessionArtifacts = repositories.Snapshots
	}
	return app.PrepareRuntimeWithRecovery(ctx, runtimeOptions)
}

func ConfigurePersistentSubagents(
	manager *app.ThreadManager,
	store *state.Store,
	workspaceRoot, sessionID string,
	runtime *app.Runtime,
	attach func(any) error,
) error {
	manager.SetChildRegistrar(func(threadID protocol.ThreadID, spec app.ChildSpec) error {
		if spec.HostSeeded {
			return nil
		}
		targetSessionID := strings.TrimSpace(spec.SessionID)
		if targetSessionID == "" {
			return fmt.Errorf("child thread %s has no owning session", threadID)
		}
		parentThreadID := spec.ParentThreadID
		if parentThreadID == "" {
			if runtime == nil {
				return fmt.Errorf("child thread %s has no parent thread", threadID)
			}
			summary, err := runtime.SessionStatus(
				context.Background(),
				targetSessionID,
			)
			if err != nil {
				return fmt.Errorf("resolve child parent session: %w", err)
			}
			parentThreadID = summary.ThreadID
		}
		return apppersistence.EnsureChildThread(
			context.Background(),
			store,
			threadID,
			targetSessionID,
			parentThreadID,
		)
	})
	return attach(state.NewAgentGraph(
		store, workspaceRoot, sessionID, runtime,
	))
}
