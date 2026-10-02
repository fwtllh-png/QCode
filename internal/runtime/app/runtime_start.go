package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/fwtllh-png/QCode/internal/observability/telemetry"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
)

func NewRuntime(options Options) *Runtime {
	runtime, _ := PrepareRuntime(context.Background(), options)
	_ = runtime.Start(context.Background())
	return runtime
}

// PrepareRuntime constructs an ephemeral Runtime without starting its operation
// loop. The caller must call Start after the surrounding graph is ready.
func PrepareRuntime(ctx context.Context, options Options) (*Runtime, error) {
	return prepareRuntime(ctx, options, false)
}

// PrepareRuntimeWithRecovery restores durable state but defers terminal outbox
// and pending Turn recovery until Start.
func PrepareRuntimeWithRecovery(
	ctx context.Context,
	options Options,
) (*Runtime, error) {
	return prepareRuntime(ctx, options, true)
}
func prepareRuntime(
	ctx context.Context,
	options Options,
	recoverDurable bool,
) (*Runtime, error) {
	options = withDefaults(options)
	if recoverDurable && options.Lifecycle != nil &&
		(options.EventStore == nil ||
			options.ContentStore == nil ||
			options.TerminalStore == nil) {
		return nil, errors.New(
			"durable runtime requires event, content, and terminal stores",
		)
	}
	if options.Observability.Metrics == nil {
		options.Observability.Metrics = telemetry.NewMetrics()
	}
	if options.EventStore == nil {
		options.EventStore = NewMemoryEventStore(options.EventHistory)
	}
	if options.ContentStore == nil {
		options.ContentStore = NewMemoryContentStore()
	}
	if options.TerminalStore == nil {
		options.TerminalStore = turnkernel.NewMemoryTerminalEnvelopeStore(nil, nil)
	}
	configureThreadManager(options)
	if options.SessionProfiles != nil {
		if err := options.DefaultProfile.Validate(); err != nil {
			return nil, fmt.Errorf("default session profile: %w", err)
		}
		if err := options.ProfileCapabilities.Validate(options.DefaultProfile); err != nil {
			return nil, fmt.Errorf("session profile capabilities: %w", err)
		}
	}
	recovery := options.Recovery
	if recoverDurable && options.Lifecycle != nil {
		value, err := options.Lifecycle.Recover(ctx)
		if err != nil {
			return nil, fmt.Errorf("recover runtime lifecycle: %w", err)
		}
		recovery = &value
	}
	runtimeContext, cancel := context.WithCancel(context.Background())
	runtime := &Runtime{
		ctx: runtimeContext, cancel: cancel, opts: options,
		engine: options.Engine, events: options.EventStore,
		content: options.ContentStore, lifecycle: options.Lifecycle,
		metrics: options.Observability.Metrics, logger: options.Observability.Logger,
		profiles: options.SessionProfiles, defaultProfile: options.DefaultProfile,
		agentPresets:        options.AgentPresets,
		profileCapabilities: options.ProfileCapabilities,
		profileModels:       options.ProfileModels,
		toolCatalog:         options.ToolCatalog, sessionLifecycle: options.SessionLifecycle,
		sessionWorkspaces:  options.SessionWorkspaces,
		sessionArtifacts:   options.SessionArtifacts,
		terminalStore:      options.TerminalStore,
		contextRebaseStore: options.ContextRebaseStore,
		workspaceRoot:      strings.TrimSpace(options.WorkspaceRoot),
		done:               make(chan struct{}),
		durable:            recoverDurable,
	}
	installRuntimeServices(runtime, options.OperationBuffer)
	runtime.hub = newEventHub(runtimeContext, runtime)
	runtime.terminal = NewTerminalPublisher(runtime)
	if recovery != nil {
		runtime.RecoveryService.restore(*recovery)
	}
	return runtime, nil
}

// Start activates a prepared Runtime exactly once. Durable projection and Turn
// recovery complete before the Runtime is returned as ready; a terminal outbox
// that cannot be projected yet is left to the settlement retrier.
func (r *Runtime) Start(ctx context.Context) error {
	if r == nil {
		return errors.New("runtime is required")
	}
	r.startOnce.Do(func() {
		r.startErr = r.activate(ctx)
	})
	return r.startErr
}
func (r *Runtime) activate(ctx context.Context) error {
	if r.durable {
		if err := r.terminal.Recover(ctx, r.TurnService.deferRecoveredTerminalProjection); err != nil {
			go r.loop()
			r.OperationService.shutdown(true)
			return fmt.Errorf("recover terminal projections: %w", err)
		}
	}
	r.lifecycleMu.Lock()
	if r.closed {
		r.lifecycleMu.Unlock()
		return errors.New("runtime is closed")
	}
	r.lifecycleMu.Unlock()
	r.OperationService.open()
	go r.loop()
	if !r.durable {
		return nil
	}
	if err := r.recoverPendingTurns(ctx); err != nil {
		r.OperationService.shutdown(false)
		startErr := fmt.Errorf("recover pending turns: %w", err)
		<-r.done
		return startErr
	}
	for _, threadID := range r.TurnQueueService.threads() {
		r.TurnQueueService.Drain(threadID)
	}
	return nil
}
