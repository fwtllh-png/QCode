package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	appextension "github.com/fwtllh-png/QCode/internal/runtime/app/extension"

	sessionhistory "github.com/fwtllh-png/QCode/internal/persist/history"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/interact"
	"github.com/fwtllh-png/QCode/internal/persist/workspacejournal"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	agentengine "github.com/fwtllh-png/QCode/internal/runtime/agent/engine"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	"github.com/fwtllh-png/QCode/internal/security/policy"
)

// ThreadManager routes Engine operations to a per-ThreadID EngineAdapter so
// concurrent threads keep isolated model history.
type ThreadManager struct {
	factory   func() (*EngineAdapter, error)
	children  ChildFactory
	restorer  WindowRestorer
	sequences SequenceReader
	deltas    SessionDeltaRestorer
	register  func(protocol.ThreadID, ChildSpec) error
	session   func(context.Context, protocol.ThreadID) (string, error)
	journal   *workspacejournal.Manager

	mu        sync.Mutex
	threads   map[protocol.ThreadID]*EngineAdapter
	turns     map[string]protocol.ThreadID
	running   map[protocol.ThreadID]int
	windows   map[protocol.ThreadID]*compactWindow
	childSpec map[protocol.ThreadID]ChildSpec
	sessions  map[protocol.ThreadID]string
	retired   map[*agentengine.Engine]struct{}
	createMu  sync.Mutex // serialize factory calls (shared Options seeds)
	closeOnce sync.Once
	closeErr  error
}

func (m *ThreadManager) ActivitySnapshot() agentengine.ActivitySnapshot {
	m.mu.Lock()
	engines := make([]*EngineAdapter, 0, len(m.threads))
	for _, engine := range m.threads {
		engines = append(engines, engine)
	}
	m.mu.Unlock()
	var snapshot agentengine.ActivitySnapshot
	for _, engine := range engines {
		current := engine.Underlying().ActivitySnapshot()
		snapshot.ProviderCalls += current.ProviderCalls
		snapshot.ToolExecutions += current.ToolExecutions
	}
	return snapshot
}

// ChildSpec describes a child-agent thread whose Engine must differ from the
// host template: its own workspace root, step quota and spend budget. The host
// template cannot express these because every host thread shares one seed.
type ChildSpec struct {
	AgentID        string
	ParentThreadID protocol.ThreadID
	AgentPath      string
	ParentPath     string
	Role           string
	Stance         string
	Workspace      string // isolation root; empty means the host workspace
	HostWorkspace  string
	SessionID      string
	ReadOnly       bool // no journal, writes denied by policy
	AllowedTools   []string
	CanDelegate    bool
	// Serialized means this child deliberately shares the host workspace and
	// inherits its gate; HostSeeded leaves durable creation to the Host protocol.
	Serialized, HostSeeded bool
	MaxSteps               int
	MaxTokens              uint64
	MaxCostUSD             float64
}

// ChildFactory builds the Engine for a thread registered via RegisterChild.
type ChildFactory func(spec ChildSpec) (*EngineAdapter, error)

// WindowRestorer loads the latest compacted window for a thread (e.g. from eventlog).
type WindowRestorer func(ctx context.Context, threadID protocol.ThreadID) (*protocol.ThreadCompactedData, error)

// SequenceReader returns the durable eventlog high-watermark (used as fork source cursor).
type SequenceReader func(ctx context.Context) (protocol.Cursor, error)

type SessionDeltaRestorer func(
	context.Context,
	protocol.ThreadID,
) (json.RawMessage, error)

type compactWindow struct {
	Number   uint64
	FirstID  string
	Current  string
	restored bool
}

func NewThreadManager(factory func() (*EngineAdapter, error)) *ThreadManager {
	if factory == nil {
		factory = func() (*EngineAdapter, error) {
			return nil, errors.New("thread engine factory is required")
		}
	}
	return &ThreadManager{
		factory:   factory,
		threads:   make(map[protocol.ThreadID]*EngineAdapter),
		turns:     make(map[string]protocol.ThreadID),
		running:   make(map[protocol.ThreadID]int),
		windows:   make(map[protocol.ThreadID]*compactWindow),
		childSpec: make(map[protocol.ThreadID]ChildSpec),
		sessions:  make(map[protocol.ThreadID]string),
		retired:   make(map[*agentengine.Engine]struct{}),
	}
}

// SetChildFactory installs the builder used for threads registered through
// RegisterChild. Without it RegisterChild is rejected, so a child thread can
// never silently fall back to the host template and write to the parent
// workspace.
func (m *ThreadManager) SetChildFactory(factory ChildFactory) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.children = factory
}

func (m *ThreadManager) SetChildRegistrar(
	register func(protocol.ThreadID, ChildSpec) error,
) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.register = register
}

func (m *ThreadManager) SetSessionDeltaRestorer(
	restorer SessionDeltaRestorer,
) {
	m.mu.Lock()
	m.deltas = restorer
	m.mu.Unlock()
}

func (m *ThreadManager) SetSessionResolver(
	resolver func(context.Context, protocol.ThreadID) (string, error),
) {
	m.mu.Lock()
	m.session = resolver
	m.mu.Unlock()
}

// SetHostJournal installs the shared workspace journal so Session delete can
// revert drafts that outlive their owning Turn rows.
func (m *ThreadManager) SetHostJournal(journal *workspacejournal.Manager) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.journal = journal
}

func (m *ThreadManager) DraftTurnIDs() []string {
	m.mu.Lock()
	journal := m.journal
	m.mu.Unlock()
	if journal == nil {
		return nil
	}
	return journal.DraftTurnIDs()
}

func (m *ThreadManager) RevertWorkspaceDraft(
	ctx context.Context,
	turnID string,
) error {
	m.mu.Lock()
	journal := m.journal
	m.mu.Unlock()
	if journal == nil {
		return errors.New("workspace journal is not configured")
	}
	receipt, err := journal.Revert(ctx, turnID)
	if err != nil {
		return err
	}
	if len(receipt.Conflicts) != 0 {
		return protocol.NewProblem(
			protocol.CodeConflict,
			"cannot revert workspace draft while files have changed after the recorded write",
			false,
			nil,
		)
	}
	return nil
}

// RegisterChild binds a child spec to a thread before its first turn is
// submitted. The engine itself is still created lazily by forThread.
func (m *ThreadManager) RegisterChild(threadID protocol.ThreadID, spec ChildSpec) error {
	if threadID == "" {
		return errors.New("child thread id is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.children == nil {
		return errors.New("child engine factory is not configured")
	}
	if _, ok := m.threads[threadID]; ok {
		return fmt.Errorf("thread %s already has an engine", threadID)
	}
	if m.register != nil {
		if err := m.register(threadID, spec); err != nil {
			return err
		}
	}
	m.childSpec[threadID] = spec
	if spec.SessionID != "" {
		m.sessions[threadID] = spec.SessionID
	}
	return nil
}

func (m *ThreadManager) BindSession(
	threadID protocol.ThreadID,
	sessionID string,
) error {
	if threadID == "" || sessionID == "" {
		return errors.New("thread and session ids are required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing := m.sessions[threadID]; existing != "" && existing != sessionID {
		return fmt.Errorf("thread %s is already bound to session %s", threadID, existing)
	}
	m.sessions[threadID] = sessionID
	return nil
}

// ChildSpecFor reports the spec registered for a thread, if any.
func (m *ThreadManager) ChildSpecFor(threadID protocol.ThreadID) (ChildSpec, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	spec, ok := m.childSpec[threadID]
	return spec, ok
}

// Release drops a thread's engine and bookkeeping. Used when a child agent is
// closed; host threads keep their engine for the process lifetime.
func (m *ThreadManager) Release(threadID protocol.ThreadID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if adapter := m.threads[threadID]; adapter != nil && adapter.Underlying() != nil {
		m.retired[adapter.Underlying()] = struct{}{}
	}
	delete(m.threads, threadID)
	delete(m.windows, threadID)
	delete(m.childSpec, threadID)
	delete(m.sessions, threadID)
	for turnID, owner := range m.turns {
		if owner == threadID {
			delete(m.turns, turnID)
		}
	}
}

func (m *ThreadManager) Close() error {
	if m == nil {
		return nil
	}
	m.closeOnce.Do(func() {
		m.mu.Lock()
		engines := make(map[*agentengine.Engine]struct{}, len(m.threads))
		for _, adapter := range m.threads {
			if adapter != nil && adapter.Underlying() != nil {
				engines[adapter.Underlying()] = struct{}{}
			}
		}
		for engine := range m.retired {
			engines[engine] = struct{}{}
		}
		m.mu.Unlock()
		for engine := range engines {
			m.closeErr = errors.Join(m.closeErr, engine.OptionsSeed().Tools.Close())
		}
	})
	return m.closeErr
}

// SetWindowRestorer installs a resume hook used when a thread engine is first created.
func (m *ThreadManager) SetWindowRestorer(restorer WindowRestorer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.restorer = restorer
}

// SetSequenceReader installs the durable sequence high-watermark reader used
// when forking (source cursor after prior events are flushed).
func (m *ThreadManager) SetSequenceReader(reader SequenceReader) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sequences = reader
}

func (m *ThreadManager) StartTurn(
	ctx context.Context, payload *protocol.StartTurnPayload, sink EngineSink,
) error {
	m.mu.Lock()
	sessionID := m.sessions[payload.ThreadID]
	resolveSession := m.session
	m.mu.Unlock()
	if sessionID == "" && resolveSession != nil {
		resolved, err := resolveSession(ctx, payload.ThreadID)
		if err != nil {
			return fmt.Errorf("resolve turn session identity: %w", err)
		}
		sessionID = resolved
	}
	if sessionID != "" {
		ctx = tool.WithSessionIdentity(ctx, sessionID)
	}
	adapter, err := m.forThread(payload.ThreadID)
	if err != nil {
		return err
	}
	if payload != nil && !payload.Idle {
		if _, child := m.ChildSpecFor(payload.ThreadID); !child {
			if engine := adapter.Underlying(); engine != nil {
				engine.OptionsSeed().SharedRateLimit.BeginUserTurn()
			}
		}
	}
	m.bindTurn(string(payload.TurnID), payload.ThreadID)
	m.enter(payload.ThreadID)
	defer m.leave(payload.ThreadID)
	return adapter.StartTurn(ctx, payload, sink)
}

func (m *ThreadManager) CancelTurn(
	ctx context.Context, payload *protocol.CancelTurnPayload, sink EngineSink,
) error {
	adapter, err := m.forThread(payload.ThreadID)
	if err != nil {
		return err
	}
	return adapter.CancelTurn(ctx, payload, sink)
}

func (m *ThreadManager) SteerTurn(
	ctx context.Context, payload *protocol.SteerTurnPayload, sink EngineSink,
) error {
	adapter, err := m.forThread(payload.ThreadID)
	if err != nil {
		return err
	}
	return adapter.SteerTurn(ctx, payload, sink)
}

func (m *ThreadManager) DecideApproval(
	ctx context.Context, payload *protocol.ApprovalDecisionPayload, sink EngineSink,
) error {
	adapter, err := m.forThread(payload.ThreadID)
	if err != nil {
		return err
	}
	return adapter.DecideApproval(ctx, payload, sink)
}

func (m *ThreadManager) ReplyInput(
	ctx context.Context, payload *protocol.InputReplyPayload, sink EngineSink,
) error {
	adapter, err := m.forThread(payload.ThreadID)
	if err != nil {
		return err
	}
	return adapter.ReplyInput(ctx, payload, sink)
}

// RestorePendingApproval rehydrates the Guard on the authoritative thread.
// Runtime calls this before replaying interrupted turns so the original request
// ID remains valid after restart.
func (m *ThreadManager) RestorePendingApproval(pending PendingApproval) error {
	adapter, err := m.forThread(pending.ThreadID)
	if err != nil {
		return err
	}
	return adapter.RestorePendingApproval(pending)
}

// RestorePendingInput rehydrates interactive waits on their owning thread.
func (m *ThreadManager) RestorePendingInput(pending PendingInput) error {
	adapter, err := m.forThread(pending.ThreadID)
	if err != nil {
		return err
	}
	return adapter.RestorePendingInput(pending)
}

func (m *ThreadManager) ValidateSessionProfile(
	threadID protocol.ThreadID,
	profile protocol.SessionProfile,
) error {
	adapter, err := m.forThread(threadID)
	if err != nil {
		return err
	}
	return adapter.ValidateSessionProfile(profile)
}

func (m *ThreadManager) ApplySessionProfile(
	threadID protocol.ThreadID,
	profile protocol.SessionProfile,
) error {
	adapter, err := m.forThread(threadID)
	if err != nil {
		return err
	}
	return adapter.ApplySessionProfile(profile)
}

func (m *ThreadManager) SetPermission(permission policy.Permission) {
	for _, adapter := range m.adapters() {
		adapter.SetPermission(permission)
	}
}

func (m *ThreadManager) SetGranular(granular policy.Granular) {
	for _, adapter := range m.adapters() {
		adapter.SetGranular(granular)
	}
}

func (m *ThreadManager) adapters() []*EngineAdapter {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]*EngineAdapter, 0, len(m.threads))
	for _, adapter := range m.threads {
		result = append(result, adapter)
	}
	return result
}

func (m *ThreadManager) CompactThread(
	ctx context.Context, payload *protocol.CompactThreadPayload, sink EngineSink,
) error {
	adapter, err := m.forThread(payload.ThreadID)
	if err != nil {
		return err
	}
	engine := adapter.Underlying()
	if engine == nil {
		return errors.New("thread engine is nil")
	}
	beforeID, _ := engine.TokenWindowIdentity()
	result, err := engine.CompactForcedDurable(
		ctx,
		payload.ThreadID,
		payload.TurnID,
		payload.Focus,
	)
	if err != nil {
		return err
	}
	receipt := result.Receipt
	summary := "context already within budget; no messages compacted"
	if receipt != nil {
		summary = appextension.FormatCompactionSummary(receipt)
	}
	history := engine.History()
	encoded, err := sessionhistory.EncodeCompactedHistory(history)
	if err != nil {
		return err
	}
	windowID, windowNumber := engine.TokenWindowIdentity()
	if receipt == nil && windowID == beforeID {
		windowID, windowNumber = engine.AdvanceTokenWindow()
	}
	window := m.recordWindow(
		payload.ThreadID, beforeID, windowID, windowNumber,
	)
	data := &protocol.ThreadCompactedData{
		Summary:            summary,
		ReplacementHistory: encoded,
		WindowNumber:       window.Number,
		FirstWindowID:      window.FirstID,
		PreviousWindowID:   window.previous,
		WindowID:           window.Current,
	}
	appextension.ApplyThreadCompactionTruth(data, receipt)
	return sink.Emit(data)
}

type advancedWindow struct {
	Number   uint64
	FirstID  string
	Current  string
	previous string
}

func (m *ThreadManager) ForkThread(
	ctx context.Context, payload *protocol.ForkThreadPayload, sink EngineSink,
) error {
	if payload == nil || payload.NewThreadID == "" {
		return errors.New("fork requires a new thread id")
	}
	if payload.NewThreadID == payload.ThreadID {
		return errors.New("fork new thread id must differ from parent")
	}
	parent, err := m.forThread(payload.ThreadID)
	if err != nil {
		return err
	}
	engine := parent.Underlying()
	if engine == nil {
		return errors.New("parent thread engine is unavailable")
	}
	// Flush point: read durable high-watermark before emitting the fork event
	// so SourceCursor points at all parent work already committed.
	var sourceCursor protocol.Cursor
	m.mu.Lock()
	sequences := m.sequences
	m.mu.Unlock()
	if sequences != nil {
		sourceCursor, err = sequences(ctx)
		if err != nil {
			return fmt.Errorf("fork flush sequence: %w", err)
		}
	}
	history, err := sessionhistory.EncodeCompactedHistory(engine.History())
	if err != nil {
		return err
	}
	forked, err := engine.Fork()
	if err != nil {
		return err
	}
	child := AdaptEngineWithWorkspaceIdentity(forked, parent.WorkspaceIdentity())
	childWindowID, childWindowNumber := "", uint64(0)
	if childEngine := child.Underlying(); childEngine != nil {
		childWindowID, childWindowNumber = childEngine.TokenWindowIdentity()
	}
	m.mu.Lock()
	if _, exists := m.threads[payload.NewThreadID]; exists {
		m.mu.Unlock()
		return fmt.Errorf("fork target thread %s already exists", payload.NewThreadID)
	}
	m.threads[payload.NewThreadID] = child
	if childWindowID != "" {
		m.windows[payload.NewThreadID] = &compactWindow{
			Number: childWindowNumber, FirstID: childWindowID, Current: childWindowID,
		}
	}
	m.mu.Unlock()
	return sink.Emit(&protocol.ThreadForkedData{
		NewThreadID:        payload.NewThreadID,
		SourceCursor:       sourceCursor,
		ReplacementHistory: history,
		WindowNumber:       childWindowNumber,
		FirstWindowID:      childWindowID,
		WindowID:           childWindowID,
	})
}

func (m *ThreadManager) RestoreCheckpoint(
	threadID protocol.ThreadID,
	history []provider.Message,
) error {
	if threadID == "" || len(history) == 0 {
		return errors.New("checkpoint Thread and history are required")
	}
	adapter, err := m.forThread(threadID)
	if err != nil {
		return err
	}
	engine := adapter.Underlying()
	if engine == nil {
		return errors.New("checkpoint Thread engine is unavailable")
	}
	engine.ReplaceHistory(history)
	return nil
}

func (m *ThreadManager) ForkCheckpoint(
	parentThreadID, newThreadID protocol.ThreadID,
	history []provider.Message,
) error {
	if parentThreadID == "" || newThreadID == "" ||
		parentThreadID == newThreadID || len(history) == 0 {
		return errors.New("checkpoint Fork identity and history are invalid")
	}
	parent, err := m.forThread(parentThreadID)
	if err != nil {
		return err
	}
	engine := parent.Underlying()
	if engine == nil {
		return errors.New("checkpoint parent engine is unavailable")
	}
	childEngine, err := engine.Fork()
	if err != nil {
		return err
	}
	childEngine.ReplaceHistory(history)
	child := AdaptEngineWithWorkspaceIdentity(
		childEngine,
		parent.WorkspaceIdentity(),
	)
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.threads[newThreadID]; exists {
		return fmt.Errorf("checkpoint Fork target Thread %s already exists", newThreadID)
	}
	m.threads[newThreadID] = child
	if state := m.windows[parentThreadID]; state != nil {
		copy := *state
		m.windows[newThreadID] = &copy
	}
	return nil
}

func (m *ThreadManager) ContextSnapshot(
	threadID protocol.ThreadID,
) (agentcontext.ContextSnapshot, error) {
	adapter, err := m.forThread(threadID)
	if err != nil {
		return agentcontext.ContextSnapshot{}, err
	}
	engine := adapter.Underlying()
	if engine == nil {
		return agentcontext.ContextSnapshot{},
			errors.New("checkpoint Thread engine is unavailable")
	}
	return engine.ExportContextSnapshot()
}

func (m *ThreadManager) WithdrawTurn(ctx context.Context, threadID protocol.ThreadID, turnID protocol.TurnID) error {
	adapter, err := m.forThread(threadID)
	if err != nil {
		return err
	}
	return adapter.Underlying().WithdrawTurn(ctx, threadID, turnID)
}

func (m *ThreadManager) PreparePostTurnNarrative(
	threadID protocol.ThreadID,
	turnID protocol.TurnID,
) (agentengine.PostTurnNarrativeRunner, error) {
	adapter, err := m.forThread(threadID)
	if err != nil {
		return nil, err
	}
	engine := adapter.Underlying()
	if engine == nil {
		return nil, errors.New("context maintenance engine is unavailable")
	}
	prepared := engine.PreparePostTurnNarrative(threadID, turnID)
	if prepared == nil {
		return nil, nil
	}
	return prepared, nil
}

func (m *ThreadManager) GenerateSessionTitle(
	ctx context.Context, threadID protocol.ThreadID, identity, prompt string,
) (agentengine.SessionTitleResult, error) {
	adapter, err := m.forThread(threadID)
	if err != nil {
		return agentengine.SessionTitleResult{}, err
	}
	if adapter.Underlying() == nil {
		return agentengine.SessionTitleResult{}, errors.New("session title engine is unavailable")
	}
	return adapter.Underlying().GenerateSessionTitle(ctx, identity, prompt)
}

func (m *ThreadManager) RestoreContext(
	threadID protocol.ThreadID,
	snapshot agentcontext.ContextSnapshot,
) (agentcontext.ReconciliationReceipt, error) {
	adapter, err := m.forThread(threadID)
	if err != nil {
		return agentcontext.ReconciliationReceipt{}, err
	}
	engine := adapter.Underlying()
	if engine == nil {
		return agentcontext.ReconciliationReceipt{},
			errors.New("checkpoint Thread engine is unavailable")
	}
	return engine.RestoreContextSnapshot(snapshot)
}

func (m *ThreadManager) ForkContext(
	parentThreadID, newThreadID protocol.ThreadID,
	snapshot agentcontext.ContextSnapshot,
) (agentcontext.ReconciliationReceipt, error) {
	if parentThreadID == "" || newThreadID == "" ||
		parentThreadID == newThreadID {
		return agentcontext.ReconciliationReceipt{},
			errors.New("checkpoint Fork identity is invalid")
	}
	parent, err := m.forThread(parentThreadID)
	if err != nil {
		return agentcontext.ReconciliationReceipt{}, err
	}
	engine := parent.Underlying()
	if engine == nil {
		return agentcontext.ReconciliationReceipt{},
			errors.New("checkpoint parent engine is unavailable")
	}
	childEngine, receipt, err := engine.ForkFromContextSnapshot(snapshot)
	if err != nil {
		return agentcontext.ReconciliationReceipt{}, err
	}
	child := AdaptEngineWithWorkspaceIdentity(
		childEngine,
		parent.WorkspaceIdentity(),
	)
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.threads[newThreadID]; exists {
		return agentcontext.ReconciliationReceipt{},
			fmt.Errorf("checkpoint Fork target Thread %s already exists", newThreadID)
	}
	m.threads[newThreadID] = child
	windowID, windowNumber := childEngine.TokenWindowIdentity()
	m.windows[newThreadID] = &compactWindow{
		Number: windowNumber, FirstID: windowID, Current: windowID,
	}
	return receipt, nil
}

func (m *ThreadManager) RevertTurn(
	ctx context.Context, payload *protocol.RevertTurnPayload, sink EngineSink,
) error {
	adapter, err := m.forThread(payload.ThreadID)
	if err != nil {
		return err
	}
	return adapter.RevertTurn(ctx, payload, sink)
}

// History returns a copy of the model-visible history for threadID.
func (m *ThreadManager) History(threadID protocol.ThreadID) ([]provider.Message, error) {
	adapter, err := m.forThread(threadID)
	if err != nil {
		return nil, err
	}
	return adapter.History(), nil
}

func (m *ThreadManager) EstimateFirstWindow(
	threadID protocol.ThreadID,
	prompt string,
) (uint64, uint64, error) {
	adapter, err := m.forThread(threadID)
	if err != nil {
		return 0, 0, err
	}
	engine := adapter.Underlying()
	if engine == nil {
		return 0, 0, errors.New("thread engine is unavailable")
	}
	projected, limit := engine.EstimateFirstWindow(prompt)
	return projected, limit, nil
}

func (m *ThreadManager) ContextEngine(threadID string) (*agentengine.Engine, error) {
	if m == nil || threadID == "" {
		return nil, errors.New("thread id is required for context lookup")
	}
	m.mu.Lock()
	adapter := m.threads[protocol.ThreadID(threadID)]
	m.mu.Unlock()
	if adapter == nil {
		return nil, fmt.Errorf("thread %q has no context engine", threadID)
	}
	if engine := adapter.Underlying(); engine != nil {
		return engine, nil
	}
	return nil, errors.New("thread engine is unavailable")
}

// FormatTurnDiff returns the net file-tool diff for threadID's engine (N18).
func (m *ThreadManager) FormatTurnDiff(threadID protocol.ThreadID) string {
	engine, err := m.ContextEngine(string(threadID))
	if err != nil {
		return ""
	}
	return engine.FormatTurnDiff()
}

// ApplyPlan applies a plan to every currently running thread engine (usually one).
func (m *ThreadManager) ApplyPlan(plan interact.Plan) error {
	m.mu.Lock()
	targets := make([]*EngineAdapter, 0, len(m.running))
	for threadID, depth := range m.running {
		if depth <= 0 {
			continue
		}
		if adapter := m.threads[threadID]; adapter != nil {
			targets = append(targets, adapter)
		}
	}
	m.mu.Unlock()
	for _, adapter := range targets {
		if engine := adapter.Underlying(); engine != nil {
			if err := engine.ApplyPlan(plan); err != nil {
				return err
			}
		}
	}
	return nil
}

// RevertWorkspace routes workspace revert to the engine that owns targetTurnID.
func (m *ThreadManager) RevertWorkspace(
	ctx context.Context, targetTurnID string,
) (workspacejournal.Receipt, error) {
	adapter, err := m.engineForTurn(targetTurnID)
	if err != nil {
		return workspacejournal.Receipt{}, err
	}
	engine := adapter.Underlying()
	if engine == nil {
		return workspacejournal.Receipt{}, errors.New("thread engine is nil")
	}
	return engine.RevertWorkspace(ctx, targetTurnID)
}

// LastTurnID returns the journal turn id from a running thread, else any known thread.
func (m *ThreadManager) LastTurnID() (string, error) {
	m.mu.Lock()
	var preferred *EngineAdapter
	for threadID, depth := range m.running {
		if depth > 0 {
			preferred = m.threads[threadID]
			break
		}
	}
	if preferred == nil {
		for _, adapter := range m.threads {
			preferred = adapter
			break
		}
	}
	m.mu.Unlock()
	if preferred == nil || preferred.Underlying() == nil {
		return "", errors.New("no thread engine available")
	}
	return preferred.Underlying().LastTurnID()
}

func (m *ThreadManager) forThread(id protocol.ThreadID) (*EngineAdapter, error) {
	if id == "" {
		return nil, errors.New("thread id is required")
	}
	m.mu.Lock()
	if adapter, ok := m.threads[id]; ok {
		m.mu.Unlock()
		return adapter, nil
	}
	restorer := m.restorer
	deltas := m.deltas
	m.mu.Unlock()

	m.createMu.Lock()
	defer m.createMu.Unlock()

	m.mu.Lock()
	if adapter, ok := m.threads[id]; ok {
		m.mu.Unlock()
		return adapter, nil
	}
	spec, isChild := m.childSpec[id]
	childFactory := m.children
	m.mu.Unlock()

	var (
		adapter *EngineAdapter
		err     error
	)
	if isChild {
		if childFactory == nil {
			return nil, fmt.Errorf("create engine for thread %s: child factory is not configured", id)
		}
		adapter, err = childFactory(spec)
	} else {
		adapter, err = m.factory()
	}
	if err != nil {
		return nil, fmt.Errorf("create engine for thread %s: %w", id, err)
	}
	if adapter == nil {
		return nil, fmt.Errorf("create engine for thread %s: factory returned nil", id)
	}
	restoredWindow, err := m.restoreWindow(
		context.Background(),
		id,
		adapter,
		restorer,
	)
	if err != nil {
		return nil, err
	}
	if deltas != nil {
		raw, err := deltas(context.Background(), id)
		if err != nil {
			return nil, fmt.Errorf("restore session delta for %s: %w", id, err)
		}
		if err := adapter.RestoreSessionDelta(raw); err != nil {
			return nil, fmt.Errorf("apply session delta for %s: %w", id, err)
		}
		if restoredWindow != nil && len(raw) != 0 {
			engine := adapter.Underlying()
			windowID, windowNumber := engine.TokenWindowIdentity()
			if restoredWindow.WindowNumber > windowNumber {
				if err := m.applyRestoredWindow(
					id,
					adapter,
					restoredWindow,
				); err != nil {
					return nil, err
				}
			} else {
				m.syncWindowRecord(
					id,
					restoredWindow.FirstWindowID,
					windowID,
					windowNumber,
				)
			}
		}
	}

	m.mu.Lock()
	m.threads[id] = adapter
	m.mu.Unlock()
	return adapter, nil
}

func (m *ThreadManager) restoreWindow(
	ctx context.Context,
	id protocol.ThreadID,
	adapter *EngineAdapter,
	restorer WindowRestorer,
) (*protocol.ThreadCompactedData, error) {
	if restorer == nil {
		return nil, nil
	}
	data, err := restorer(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("restore compacted window for %s: %w", id, err)
	}
	if data == nil || len(data.ReplacementHistory) == 0 {
		return nil, nil
	}
	if err := m.applyRestoredWindow(id, adapter, data); err != nil {
		return nil, err
	}
	return data, nil
}

func (m *ThreadManager) applyRestoredWindow(
	id protocol.ThreadID,
	adapter *EngineAdapter,
	data *protocol.ThreadCompactedData,
) error {
	messages, err := sessionhistory.DecodeCompactedHistory(data.ReplacementHistory)
	if err != nil {
		return err
	}
	if engine := adapter.Underlying(); engine != nil {
		engine.ReplaceHistory(messages)
		if err := engine.RestoreTokenWindow(data.WindowID, data.WindowNumber); err != nil {
			return fmt.Errorf("restore token window for %s: %w", id, err)
		}
	}
	m.syncWindowRecord(
		id,
		data.FirstWindowID,
		data.WindowID,
		data.WindowNumber,
	)
	return nil
}

func (m *ThreadManager) syncWindowRecord(
	id protocol.ThreadID,
	firstID string,
	windowID string,
	windowNumber uint64,
) {
	m.mu.Lock()
	m.windows[id] = &compactWindow{
		Number:   windowNumber,
		FirstID:  firstID,
		Current:  windowID,
		restored: true,
	}
	if m.windows[id].FirstID == "" {
		m.windows[id].FirstID = windowID
	}
	m.mu.Unlock()
}

func (m *ThreadManager) recordWindow(
	threadID protocol.ThreadID,
	previousID string,
	windowID string,
	number uint64,
) advancedWindow {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.windows[threadID]
	if state == nil {
		state = &compactWindow{FirstID: previousID}
		m.windows[threadID] = state
	}
	previous := state.Current
	if previous == "" {
		previous = previousID
	}
	state.Number = number
	state.Current = windowID
	if state.FirstID == "" {
		state.FirstID = previousID
	}
	return advancedWindow{
		Number:   state.Number,
		FirstID:  state.FirstID,
		Current:  state.Current,
		previous: previous,
	}
}

func (m *ThreadManager) bindTurn(turnID string, threadID protocol.ThreadID) {
	if turnID == "" {
		return
	}
	m.mu.Lock()
	m.turns[turnID] = threadID
	m.mu.Unlock()
}

func (m *ThreadManager) enter(threadID protocol.ThreadID) {
	m.mu.Lock()
	m.running[threadID]++
	m.mu.Unlock()
}

func (m *ThreadManager) leave(threadID protocol.ThreadID) {
	m.mu.Lock()
	if m.running[threadID] <= 1 {
		delete(m.running, threadID)
	} else {
		m.running[threadID]--
	}
	m.mu.Unlock()
}

func (m *ThreadManager) engineForTurn(turnID string) (*EngineAdapter, error) {
	m.mu.Lock()
	threadID, ok := m.turns[turnID]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("no thread owns turn %q", turnID)
	}
	return m.forThread(threadID)
}
