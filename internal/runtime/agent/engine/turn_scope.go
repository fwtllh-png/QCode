package engine

import (
	"context"
	"errors"
	"sync"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	toolguard "github.com/fwtllh-png/QCode/internal/adapter/tool/guard"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/interact"
	"github.com/fwtllh-png/QCode/internal/observability/diagnostics"
	"github.com/fwtllh-png/QCode/internal/observability/trace"
	"github.com/fwtllh-png/QCode/internal/observability/verify"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	promptcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/prompt"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

// Scope owns one frozen TurnSpec and its execution lifetime. Session state
// remains on Engine and is applied only after the terminal commit succeeds.
//
// s.mu ranks directly below Engine.mu in the lock hierarchy documented on
// Engine: it may be held while acquiring Engine.scopeMu (for example
// advanceTokenWindow resolving capacity) or Engine.planMu (projectWorldState
// reading the current plan), and must never be acquired while any other
// Engine lock is held.
type Scope struct {
	engine          *Engine
	spec            TurnSpec
	emit            func(Event) error
	persistedTurnID string
	once            sync.Once
	mu              sync.Mutex
	state           scopeState
}

type scopeState struct {
	samples              uint32
	toolSamples          map[uint32]toolSpend
	approvalEmit         func(Event) error
	kernel               *turnkernel.RuntimeKernel
	recorder             *trace.Recorder
	toolSpans            map[string]uint64
	scheduler            *turnkernel.ToolScheduler
	diff                 *turnkernel.TurnDiffTracker
	contextSeen          []promptcontext.Receipt
	selections           []promptcontext.Selection
	catalog              tool.CatalogSnapshot
	catalogProjected     tool.CatalogSnapshot
	sampledCatalog       tool.CatalogSnapshot
	sampledTools         map[string]bool
	contextLedger        *agentcontext.MessageLedger
	mcpProjected         bool
	diagnostics          []diagnostics.Receipt
	verification         []verify.Evidence
	rollback             []string
	budgetStage          uint8
	toolSurfaceMaxBytes  int
	toolSurfaceItemBytes int
	mailbox              *turnkernel.Mailbox[PendingInput]
	requests             *turnkernel.RequestLedger
	cancel               context.CancelCauseFunc
	cancelReason         string
	delta                *SessionDelta
	context              agentcontext.Authority
	contextUsage         provider.Usage
	contextCost          float64
}

type ScopeSnapshot struct {
	Identity       TurnIdentity
	PendingInputs  int
	Samples        uint32
	ToolCalls      int
	Diagnostics    int
	Verification   int
	TerminalStaged bool
}

func newScopeState(engine *Engine) scopeState {
	return scopeState{
		scheduler: turnkernel.NewToolScheduler(engine.options.MaxToolConcurrent),
		diff:      turnkernel.NewTurnDiffTracker(),
		mailbox:   turnkernel.NewMailbox[PendingInput](0),
		requests:  turnkernel.NewRequestLedger(),
		context:   engine.context.Clone(),
	}
}

// Spec returns an isolated copy of the immutable turn input.
func (s *Scope) Spec() TurnSpec {
	if s == nil {
		return TurnSpec{}
	}
	spec := s.spec
	spec.Request.Attachments = append(
		[]provider.Attachment(nil),
		spec.Request.Attachments...,
	)
	if spec.Request.Recovery != nil {
		recovery := *spec.Request.Recovery
		spec.Request.Recovery = &recovery
	}
	spec.World = agentcontext.CloneWorldBaseline(spec.World)
	spec.Window = agentcontext.CloneWindowLedger(spec.Window)
	spec.Skills = append([]SkillSummary(nil), spec.Skills...)
	spec.MCP = append([]MCPHealthSnapshot(nil), spec.MCP...)
	spec.Memory.SelectedIDs = append([]string(nil), spec.Memory.SelectedIDs...)
	return spec
}

func (s *Scope) Snapshot() ScopeSnapshot {
	if s == nil {
		return ScopeSnapshot{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return ScopeSnapshot{
		Identity: s.spec.Identity, PendingInputs: s.state.mailbox.Len(),
		Samples: s.state.samples, ToolCalls: len(s.state.diff.Snapshot()),
		Diagnostics:    len(s.state.diagnostics),
		Verification:   len(s.state.verification),
		TerminalStaged: s.state.delta != nil,
	}
}

// Close releases Engine admission exactly once.
func (s *Scope) Close() {
	if s == nil || s.engine == nil {
		return
	}
	s.once.Do(func() {
		s.engine.finishScope(s)
	})
}
func (e *Engine) publishScope(scope *Scope) {
	e.scopeMu.Lock()
	e.activeScope = scope
	e.scopeMu.Unlock()
}
func (e *Engine) finishScope(scope *Scope) {
	if release := e.options.ReleaseTurnResources; release != nil {
		release(scope.spec.Identity)
	}
	scope.mu.Lock()
	var held []PendingInput
	for _, item := range scope.state.mailbox.Drain() {
		if item.Source == PendingMailbox {
			held = append(held, item)
		}
	}
	scope.state.cancel = nil
	scope.mu.Unlock()
	e.scopeMu.Lock()
	if e.activeScope == scope {
		e.activeScope = nil
		e.lastScope = scope
		e.mailboxHold = append(e.mailboxHold, held...)
	}
	e.scopeMu.Unlock()
}
func (e *Engine) currentScope() *Scope {
	if e == nil {
		return nil
	}
	e.scopeMu.Lock()
	defer e.scopeMu.Unlock()
	if e.activeScope != nil {
		return e.activeScope
	}
	return e.lastScope
}
func (e *Engine) executionScope() *Scope {
	return e.currentScope()
}

func (e *Engine) TurnKernelPhase(turnID string) (turnkernel.Phase, bool) {
	scope := e.runningScope()
	if scope == nil || scope.spec.Identity.TurnID != turnID {
		return "", false
	}
	scope.mu.Lock()
	kernel := scope.state.kernel
	scope.mu.Unlock()
	if kernel == nil {
		return "", false
	}
	return kernel.Phase(), true
}
func (e *Engine) workingLedger() *agentcontext.WorkingSetLedger {
	return e.contextAuthority().WorkingSet()
}
func (e *Engine) evidenceSet() *agentcontext.EvidenceSet {
	return e.contextAuthority().Evidence()
}
func (e *Engine) failureLedger() *agentcontext.Failures {
	return e.contextAuthority().Failures()
}
func (e *Engine) compactionTotal() int {
	return e.contextAuthority().Compaction().Count
}
func (e *Engine) noteCompaction() {
	e.contextAuthority().NoteCompaction()
}

func (e *Engine) compactionState() agentcontext.Compaction {
	return e.contextAuthority().Compaction()
}

func (e *Engine) stageContextCompaction(
	state *agentcontext.CompactionState,
) {
	current := e.contextAuthority().Compaction()
	current.State = state
	e.contextAuthority().SetCompaction(current)
}
func (e *Engine) runningScope() *Scope {
	if e == nil {
		return nil
	}
	e.scopeMu.Lock()
	defer e.scopeMu.Unlock()
	return e.activeScope
}
func (s *Scope) kernel() (*turnkernel.RuntimeKernel, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.kernel == nil {
		return nil, ErrTurnCoordinatorNotActive
	}
	return s.state.kernel, nil
}

// ControlPort is the only mutable surface of an active Scope.
type ControlPort interface {
	Cancel(reason string) error
	Steer(prompt string) error
	ResolveApproval(toolguard.ApprovalDecision) error
	ResolveInput(interact.Reply) error
}

func (s *Scope) Control() ControlPort {
	return s
}

func (e *Engine) Control() (ControlPort, error) {
	scope := e.runningScope()
	if scope == nil {
		return nil, ErrTurnCoordinatorNotActive
	}
	return scope.Control(), nil
}
func (s *Scope) Cancel(reason string) error {
	if s == nil || s.engine.runningScope() != s {
		return ErrTurnCoordinatorNotActive
	}
	kernel, err := s.kernel()
	if err != nil {
		return err
	}
	kernelErr := kernel.RequestCancel(reason)
	s.mu.Lock()
	s.state.cancelReason = reason
	cancel := s.state.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel(errors.New(reason))
	}
	return kernelErr
}
func (s *Scope) Steer(prompt string) error {
	if prompt == "" {
		return errors.New("steering prompt is required")
	}
	if s == nil || s.engine.runningScope() != s {
		return errors.New("no active turn to steer")
	}
	s.mu.Lock()
	err := s.state.mailbox.Offer(
		PendingInput{Source: PendingSteer, Prompt: prompt},
	)
	cancel := s.state.cancel
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if cancel != nil {
		cancel(errors.New("turn steered"))
	}
	return nil
}

func (s *Scope) ResolveApproval(
	decision toolguard.ApprovalDecision,
) error {
	if s == nil || s.engine.runningScope() != s {
		return errors.New("turn scope is not active")
	}
	engine := s.engine
	if err := s.state.requests.Resolve(
		turnkernel.RequestApproval,
		decision.RequestID,
	); err != nil {
		if engine.queueRecoveredApproval(decision) {
			return nil
		}
		return err
	}
	if err := engine.guard.StageDecision(decision); err != nil {
		return err
	}
	kernel, err := s.kernel()
	if err != nil {
		return err
	}
	if err := kernel.ResolveApproval(decision.RequestID, decision.Canceled); err != nil {
		return err
	}
	if decision.Canceled {
		s.mu.Lock()
		s.state.cancelReason = protocol.CancelReasonApprovalCanceled
		s.mu.Unlock()
	}
	return engine.guard.Resume(decision.RequestID)
}
func (s *Scope) ResolveInput(reply interact.Reply) error {
	if s == nil || s.engine.runningScope() != s {
		return errors.New("turn scope is not active")
	}
	engine := s.engine
	if engine.options.InputHost == nil {
		return interact.HostUnavailableError{}
	}
	if err := s.state.requests.Resolve(
		turnkernel.RequestInput,
		reply.RequestID,
	); err != nil {
		if engine.queueRecoveredInput(reply) {
			return nil
		}
		return err
	}
	if err := engine.options.InputHost.StageReply(reply); err != nil {
		return err
	}
	kernel, err := s.kernel()
	if err != nil {
		return err
	}
	if err := kernel.ResolveInput(reply.RequestID); err != nil {
		return err
	}
	return engine.options.InputHost.Resume(reply.RequestID)
}
