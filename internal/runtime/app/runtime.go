package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/observability/telemetry"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	"github.com/fwtllh-png/QCode/internal/runtime/app/eventhub"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

var (
	ErrClosed      = protocol.NewProblem(protocol.CodeConflict, "runtime is closed", false, nil)
	ErrQueueFull   = protocol.NewProblem(protocol.CodeResourceExhausted, "runtime operation queue is full", true, nil)
	ErrCursorAhead = protocol.NewProblem(protocol.CodeInvalidArgument, "event cursor is ahead of runtime", false, nil)
	ErrCursorGap   = protocol.NewProblem(protocol.CodeConflict, "event history no longer contains the requested cursor", true, nil)
	ErrReplayLimit = protocol.NewProblem(protocol.CodeResourceExhausted, "event replay exceeds the configured limit", true, nil)
)

type CursorGapError struct {
	Requested       protocol.Cursor `json:"requested"`
	OldestAvailable protocol.Cursor `json:"oldest_available"`
	Latest          protocol.Cursor `json:"latest"`
}

func (e *CursorGapError) Error() string {
	return fmt.Sprintf(
		"event cursor %d is stale; oldest available event is %d and latest is %d",
		e.Requested, e.OldestAvailable, e.Latest,
	)
}

func (e *CursorGapError) Unwrap() error { return ErrCursorGap }

func (e *CursorGapError) RecoveryCursor() protocol.Cursor {
	if e == nil || e.OldestAvailable == 0 {
		return 0
	}
	return e.OldestAvailable - 1
}

type ReplayLimitError struct {
	Requested protocol.Cursor `json:"requested"`
	Limit     int             `json:"limit"`
}

func (e *ReplayLimitError) Error() string {
	return fmt.Sprintf("event replay after cursor %d exceeds limit %d", e.Requested, e.Limit)
}

func (e *ReplayLimitError) Unwrap() error { return ErrReplayLimit }

type SessionProfileStore interface {
	Profile(context.Context, string, protocol.SessionProfile) (protocol.SessionProfile, error)
	EnsureProfile(context.Context, string, protocol.SessionProfile) (protocol.SessionProfile, error)
	UpdateProfile(
		context.Context,
		string,
		uint64,
		protocol.SessionProfile,
		protocol.SessionProfilePatch,
	) (protocol.SessionProfileUpdateResult, error)
}

type AgentPresetStore interface {
	List(context.Context) (protocol.AgentPresetList, error)
	Get(context.Context, string) (protocol.AgentPreset, error)
	Save(
		context.Context,
		protocol.AgentPreset,
		uint64,
	) (protocol.AgentPresetMutationResult, error)
	Delete(
		context.Context,
		string,
		uint64,
	) (protocol.AgentPresetMutationResult, error)
}

type SessionProfileEngine interface {
	ValidateSessionProfile(protocol.ThreadID, protocol.SessionProfile) error
	ApplySessionProfile(protocol.ThreadID, protocol.SessionProfile) error
}

type SessionToolCatalog interface {
	Snapshot() (tool.CatalogSnapshot, error)
}

type SessionLifecycleStore interface {
	ListLifecycle(
		context.Context,
		protocol.SessionListQuery,
	) (protocol.SessionList, error)
	GetLifecycle(
		context.Context,
		string,
	) (protocol.SessionSummary, error)
	ThreadIDs(
		context.Context,
		string,
	) ([]protocol.ThreadID, error)
	SessionForThread(
		context.Context,
		protocol.ThreadID,
	) (string, error)
	ActivateThread(
		context.Context,
		string,
		protocol.ThreadID,
	) (protocol.SessionSummary, error)
	UpdateLifecycle(
		context.Context,
		string,
		uint64,
		protocol.SessionLifecyclePatch,
	) (protocol.SessionSummary, error)
	DeleteLifecycle(
		context.Context,
		string,
		uint64,
	) (protocol.SessionDeleteResult, error)
}

type sessionDiscardStore interface {
	DiscardLifecycle(
		context.Context,
		string,
		uint64,
	) (protocol.SessionDeleteResult, error)
}

type Options struct {
	OperationBuffer     int
	EventHistory        int
	SubscriberBuffer    int
	WorkspaceRoot       string
	Engine              Engine
	EventStore          EventStore
	ContentStore        ContentStore
	Lifecycle           DurableLifecycle
	Recovery            *RecoveryState
	Observability       RuntimeObservability
	SessionProfiles     SessionProfileStore
	AgentPresets        AgentPresetStore
	DefaultProfile      protocol.SessionProfile
	ProfileCapabilities protocol.SessionProfileCapabilities
	ProfileModels       map[string]protocol.ModelCapabilities
	ToolCatalog         SessionToolCatalog
	SessionLifecycle    SessionLifecycleStore
	SessionWorkspaces   SessionWorkspaceManager
	SessionArtifacts    SessionArtifactStore
	TerminalStore       turnkernel.TerminalEnvelopeStore
	ContextRebaseStore  ContextRebaseStore
}

type Snapshot struct {
	LastSequence         protocol.Cursor          `json:"last_sequence"`
	OperationsProcessed  uint64                   `json:"operations_processed"`
	Subscribers          int                      `json:"subscribers"`
	ActiveTurns          int                      `json:"active_turns"`
	ActiveProviderCalls  int                      `json:"active_provider_calls"`
	ActiveToolExecutions int                      `json:"active_tool_executions"`
	PendingApprovals     int                      `json:"pending_approvals"`
	PendingInputs        int                      `json:"pending_inputs"`
	PendingOperations    int                      `json:"pending_operations"`
	Closed               bool                     `json:"closed"`
	Metrics              telemetry.MetricSnapshot `json:"metrics"`
}

type acceptedOperation struct {
	operation      protocol.Operation
	idempotencyKey string
	canonical      []byte
}

type Runtime struct {
	ctx                 context.Context
	cancel              context.CancelFunc
	opts                Options
	engine              Engine
	events              EventStore
	hub                 *eventhub.Hub
	content             ContentStore
	lifecycle           DurableLifecycle
	metrics             *telemetry.Metrics
	logger              *slog.Logger
	profiles            SessionProfileStore
	agentPresets        AgentPresetStore
	defaultProfile      protocol.SessionProfile
	profileCapabilities protocol.SessionProfileCapabilities
	profileModels       map[string]protocol.ModelCapabilities
	toolCatalog         SessionToolCatalog
	sessionLifecycle    SessionLifecycleStore
	sessionWorkspaces   SessionWorkspaceManager
	sessionArtifacts    SessionArtifactStore
	terminalStore       turnkernel.TerminalEnvelopeStore
	contextRebaseStore  ContextRebaseStore
	terminal            *TerminalPublisher
	workspaceRoot       string
	lifecycleMu         sync.Mutex
	*SessionService
	*OperationService
	*EventService
	*RecoveryService
	*HistoryService
	*ArtifactService
	*AgentPresetService
	*TurnService
	*TurnQueueService
	TraceQuery RuntimeTraceQuery

	done      chan struct{}
	startOnce sync.Once
	startErr  error
	durable   bool

	closed bool

	contextManifests sync.Map
}

// EnsurePlanExecutionReady closes the terminal-publication race.
func (r *Runtime) EnsurePlanExecutionReady(
	ctx context.Context,
	sessionID string,
	threadID protocol.ThreadID,
	planTurnID protocol.TurnID,
) error {
	summary, err := r.sessionLifecycle.GetLifecycle(ctx, sessionID)
	if err != nil {
		return err
	}
	if r.workspaceRoot != "" &&
		!sameWorkspaceRoot(r.workspaceRoot, summary.WorkspaceRoot) {
		return runtimeProblem(
			protocol.CodeConflict,
			"session does not belong to this Runtime workspace",
			nil,
		)
	}
	threadIDs, err := r.sessionLifecycle.ThreadIDs(ctx, sessionID)
	if err != nil {
		return err
	}
	threadFound, active := false, false
	for _, candidate := range threadIDs {
		threadFound = threadFound || candidate == threadID
		if _, found := r.active.LookupThread(candidate); found {
			active = true
		}
	}
	if !threadFound {
		return runtimeProblem(
			protocol.CodeInvalidArgument,
			"Plan Artifact does not belong to the active Session Thread",
			nil,
		)
	}
	r.EventService.mu.Lock()
	terminal := r.terminals[planTurnID]
	pendingApprovals, pendingInputs := 0, 0
	for _, approval := range r.approvals {
		if slices.Contains(threadIDs, approval.ThreadID) {
			pendingApprovals++
		}
	}
	for _, input := range r.inputs {
		if slices.Contains(threadIDs, input.ThreadID) {
			pendingInputs++
		}
	}
	r.EventService.mu.Unlock()
	summary.PendingApprovals = pendingApprovals
	summary.PendingInputs = pendingInputs
	switch {
	case pendingApprovals > 0:
		summary.Status = protocol.SessionStatusAwaitingApproval
	case pendingInputs > 0:
		summary.Status = protocol.SessionStatusAwaitingInput
	case active ||
		summary.Status == protocol.SessionStatusRunning &&
			terminal != protocol.EventTurnCompleted:
		summary.Status = protocol.SessionStatusRunning
	default:
		return nil
	}
	return ensureSessionQuiescent(summary, "implement Plan")
}

func (r *Runtime) Submit(ctx context.Context, operation protocol.Operation) error {
	return r.OperationService.SubmitWithKey(ctx, operation, "")
}

// SubmitWithKey adds a caller-scoped idempotency key with conflict detection.
func (r *Runtime) SubmitWithKey(
	ctx context.Context,
	operation protocol.Operation,
	idempotencyKey string,
) error {
	return r.OperationService.SubmitWithKey(ctx, operation, idempotencyKey)
}

func (r *Runtime) Events(ctx context.Context, cursor protocol.Cursor) (<-chan protocol.Event, error) {
	return r.hub.Events(ctx, cursor, 0)
}

// EventsLimited atomically replays and subscribes, rejecting oversized replay.
func (r *Runtime) EventsLimited(
	ctx context.Context,
	cursor protocol.Cursor,
	limit int,
) (<-chan protocol.Event, error) {
	if limit <= 0 {
		return nil, runtimeProblem(protocol.CodeInvalidArgument, "event replay limit must be positive", nil)
	}
	return r.hub.Events(ctx, cursor, limit)
}

// ReplayEvents pages committed history without registering a subscriber.
func (r *Runtime) ReplayEvents(
	ctx context.Context,
	cursor protocol.Cursor,
	limit int,
) ([]protocol.Event, bool, error) {
	if limit <= 0 {
		return nil, false, runtimeProblem(protocol.CodeInvalidArgument, "event replay limit must be positive", nil)
	}
	return r.hub.Replay(ctx, cursor, limit)
}

func (r *Runtime) Snapshot(context.Context) Snapshot {
	r.EventService.mu.Lock()
	events := r.hub.Snapshot()
	snapshot := Snapshot{
		LastSequence: events.LastSequence,
		Subscribers:  events.Subscribers, Metrics: r.metrics.Snapshot(),
		PendingApprovals: len(r.approvals), PendingInputs: len(r.inputs),
	}
	r.EventService.mu.Unlock()
	r.lifecycleMu.Lock()
	snapshot.Closed = r.closed
	r.lifecycleMu.Unlock()
	snapshot.OperationsProcessed, snapshot.PendingOperations =
		r.OperationService.snapshot()
	snapshot.ActiveTurns = r.active.Snapshot().Turns
	if manager, ok := r.engine.(*ThreadManager); ok {
		activity := manager.ActivitySnapshot()
		snapshot.ActiveProviderCalls = activity.ProviderCalls
		snapshot.ActiveToolExecutions = activity.ToolExecutions
	}
	return snapshot
}

// FormatTurnDiff returns the net file-tool turn diff when the engine is a ThreadManager (N18).
func (r *Runtime) FormatTurnDiff(threadID protocol.ThreadID) string {
	if r == nil {
		return ""
	}
	manager, ok := r.engine.(*ThreadManager)
	if !ok || manager == nil {
		return ""
	}
	return manager.FormatTurnDiff(threadID)
}

// RecoveryState returns a copy of the state needed by a replacement Runtime.
func (r *Runtime) RecoveryState(context.Context) RecoveryState {
	r.EventService.mu.Lock()
	result := RecoveryState{
		LastSequence:       r.hub.Snapshot().LastSequence,
		Terminals:          make(map[protocol.TurnID]protocol.EventKind, len(r.terminals)),
		PendingApprovals:   make(map[string]PendingApproval, len(r.approvals)),
		PendingInputs:      make(map[string]PendingInput, len(r.inputs)),
		PendingQueuedTurns: r.TurnQueueService.snapshotMapLocked(),
	}
	for turnID, kind := range r.terminals {
		result.Terminals[turnID] = kind
	}
	for requestID, approval := range r.approvals {
		result.PendingApprovals[requestID] = approval
	}
	for requestID, input := range r.inputs {
		result.PendingInputs[requestID] = input
	}
	r.EventService.mu.Unlock()
	result.PendingOperations = r.OperationService.pendingSnapshot()
	return result
}

func (r *Runtime) Close(ctx context.Context) error {
	startedForClose := false
	r.startOnce.Do(func() {
		startedForClose = true
		r.startErr = errors.New("runtime closed before start")
		go r.loop()
	})
	r.OperationService.mu.Lock()
	if r.OperationService.accepting {
		r.OperationService.accepting = false
		close(r.operations)
	} else if startedForClose {
		close(r.operations)
	}
	r.OperationService.mu.Unlock()
	select {
	case <-r.done:
		return nil
	case <-ctx.Done():
		r.metrics.Error()
		return ctx.Err()
	}
}

func (r *Runtime) loop() {
	for accepted := range r.operations {
		r.dispatch(accepted)
		r.OperationService.mu.Lock()
		r.OperationService.processed++
		r.metrics.OperationProcessed()
		r.OperationService.mu.Unlock()
	}
	r.cancel()
	r.cancelActive()
	r.workers.Wait()
	_ = errors.Join(closeEngine(r.engine), r.hub.Close(context.Background()))
	_ = r.content.Close(context.Background())
	r.lifecycleMu.Lock()
	r.closed = true
	r.lifecycleMu.Unlock()
	close(r.done)
}

func (r *Runtime) dispatch(accepted acceptedOperation) {
	dispatcher := operationDispatcher{runtime: r}
	r.OperationService.Apply(accepted.operation, dispatcher.Dispatch(accepted))
}

func (r *Runtime) turnPhase(threadID protocol.ThreadID, _ protocol.TurnID) TurnPhase {
	handle, active := r.active.LookupThread(threadID)
	if !active {
		return PhaseIdle
	}
	if source, ok := r.engine.(interface {
		TurnPhase(protocol.TurnID) (TurnPhase, bool)
	}); ok {
		if phase, found := source.TurnPhase(handle.TurnID); found {
			return phase
		}
	}
	return PhaseRunning
}

func (r *Runtime) RouteMailbox(threadID protocol.ThreadID, turnID protocol.TurnID, triggerTurn bool) PendingDisposition {
	phase := r.turnPhase(threadID, turnID)
	return RoutePending(phase, PendingItem{Source: SourceMailbox, TriggerTurn: triggerTurn})
}

func (r *Runtime) invoke(
	operation protocol.Operation,
	call func(EngineSink) error,
) error {
	sink := &runtimeSink{runtime: r, operation: operation}
	return call(sink)
}

func (r *Runtime) cancelActive() {
	r.active.CancelAll()
}

func withDefaults(options Options) Options {
	if options.OperationBuffer <= 0 {
		options.OperationBuffer = 64
	}
	if options.EventHistory <= 0 {
		options.EventHistory = 256
	}
	if options.SubscriberBuffer <= 0 {
		options.SubscriberBuffer = 64
	}
	return options
}
