package engine

import (
	"context"
	"errors"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	toolguard "github.com/fwtllh-png/QCode/internal/adapter/tool/guard"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/interact"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	promptcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/prompt"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func (e *Engine) configureApprovalHandlers() {
	e.guard.SetApprovalHandler(e.emitApproval)
	e.guard.SetApprovalRecoveryHandler(e.restoreApprovalWait)
	e.guard.SetApprovalWaitObserver(e.observeApprovalWait)
	e.guard.SetApprovalExpiryHandler(e.expireApprovalWait)
}

func (e *Engine) RestoreApprovalRequest(
	request toolguard.ApprovalRequest,
) error {
	if e.guard == nil {
		return errors.New("approval guard is unavailable")
	}
	if err := e.guard.RestoreApproval(request); err != nil {
		return err
	}
	e.scopeMu.Lock()
	e.approvalRecovery.Mark(request.RequestID)
	e.scopeMu.Unlock()
	return nil
}

func (e *Engine) RestoreInputRequest(request interact.Request) error {
	if e.options.InputHost == nil {
		return interact.HostUnavailableError{}
	}
	if err := e.options.InputHost.RestoreRequest(request); err != nil {
		return err
	}
	e.scopeMu.Lock()
	e.inputRecovery.Mark(request.RequestID)
	e.scopeMu.Unlock()
	return nil
}

func (e *Engine) connectInputHost(
	kernel *turnkernel.RuntimeKernel,
	emit func(Event) error,
) func() {
	if e.options.InputHost == nil {
		return func() {}
	}
	e.options.InputHost.SetEmitter(
		func(_ context.Context, request interact.Request) error {
			if err := kernel.EnsureInput(request.RequestID, request.CallID); err != nil {
				return err
			}
			scope := e.runningScope()
			if scope == nil {
				return errors.New("turn scope is not active")
			}
			if err := scope.state.requests.Register(
				turnkernel.RequestInput,
				request.RequestID,
			); err != nil {
				return err
			}
			copy := request
			return emit(Event{
				State: AwaitingInput,
				Turn:  e.turn,
				Input: &copy,
			})
		},
	)
	e.options.InputHost.SetRecoveryHandler(func(request interact.Request) error {
		if err := kernel.EnsureInput(request.RequestID, request.CallID); err != nil {
			return err
		}
		scope := e.runningScope()
		if scope == nil {
			return errors.New("turn scope is not active")
		}
		if err := scope.state.requests.Register(
			turnkernel.RequestInput,
			request.RequestID,
		); err != nil {
			return err
		}
		reply, queued := e.takeRecoveredInput(request.RequestID)
		if !queued {
			return nil
		}
		return scope.ResolveInput(reply)
	})
	// An input wait that ends without a user reply (TTL expiry or turn
	// cancellation) must resolve the kernel input wait before Wait returns,
	// mirroring the approval expiry path. Best effort: the reducer also
	// accepts the bound tool result as a safety net.
	e.options.InputHost.SetExpiryHandler(e.expireInputWait)
	return func() {
		e.options.InputHost.SetEmitter(nil)
		e.options.InputHost.SetRecoveryHandler(nil)
		e.options.InputHost.SetExpiryHandler(nil)
	}
}

func (e *Engine) expireInputWait(request interact.Request) {
	scope := e.runningScope()
	if scope == nil {
		return
	}
	if err := scope.state.requests.Resolve(
		turnkernel.RequestInput,
		request.RequestID,
	); err != nil {
		return
	}
	kernel, err := scope.kernel()
	if err != nil {
		return
	}
	_ = kernel.ResolveInput(request.RequestID)
}

func (e *Engine) queueRecoveredInput(reply interact.Reply) bool {
	e.scopeMu.Lock()
	defer e.scopeMu.Unlock()
	return e.inputRecovery.Queue(reply.RequestID, reply)
}

func (e *Engine) takeRecoveredInput(
	requestID string,
) (interact.Reply, bool) {
	e.scopeMu.Lock()
	defer e.scopeMu.Unlock()
	return e.inputRecovery.Take(requestID)
}

func (e *Engine) ApplyPlan(plan interact.Plan) error {
	current := e.buildTruthCapsule(e.buildCompactSummary(nil), nil)
	var resolved []string
	for _, entity := range current.Entities {
		if entity.Kind == agentcontext.EntityGoal || entity.Kind == agentcontext.EntityTodo {
			resolved = append(resolved, entity.ID)
		}
	}
	added := agentcontext.PlanTruthEntities(plan, e.turn)
	decision := (agentcontext.ContextAdmissionController{
		Policy: e.options.Context.TruthRetention,
	}).Decide(current, agentcontext.AdmissionRequest{
		BaseContextRevision:  e.sessionRevision,
		RouteCompatibility:   current.CompatibilityHash,
		AddedMandatory:       added,
		ResolvedMandatoryIDs: resolved,
	})
	if !decision.Allowed {
		return protocol.NewProblem(
			protocol.CodeResourceExhausted,
			"context admission rejected plan update: "+decision.Reason,
			false,
			nil,
		)
	}
	e.setPlan(plan)
	for _, path := range plan.CriticalFiles {
		e.contextAuthority().ObservePath(
			e.options.Workspace,
			agentcontext.SourcePlan,
			e.turn,
			path,
		)
	}
	return nil
}

func (e *Engine) setPlan(plan interact.Plan) {
	text := interact.FormatPlan(plan)
	receipt := interact.PlanReceipt(plan)
	e.planMu.Lock()
	e.planText = text
	e.plan = plan
	e.planReceipt = &receipt
	e.planMu.Unlock()
}

func (e *Engine) ContextReceipts() []promptcontext.Receipt {
	return e.contextReceipts()
}

// ContextSnapshot returns the sole model-context authority visible to the most
// recent sample. Before the first sample it returns the equivalent initial
// Stable+History ledger snapshot.
func (e *Engine) ContextSnapshot() agentcontext.MessageSnapshot {
	if scope := e.currentScope(); scope != nil {
		scope.mu.Lock()
		if scope.state.contextLedger != nil {
			snapshot := scope.state.contextLedger.Snapshot()
			scope.mu.Unlock()
			return snapshot
		}
		scope.mu.Unlock()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return agentcontext.NewMessageLedger(agentcontext.LedgerInput{
		Stable:  e.promptMessages(),
		History: cloneMessages(e.history),
	}).Snapshot()
}

// promptMessages is the immutable static prefix of every request. Dynamic
// sections are projected through WorldBaseline and the ContextLedger.
//
// Nothing that changes during a session belongs here: the bytes are identical
// from one sample to the next so a provider can serve them from its prompt cache.
// Scope World State is composed separately; later digest changes enter History.
func (e *Engine) promptMessages() []provider.Message {
	return cloneMessages(e.options.StaticContext)
}

func (e *Engine) contextReceipts() []promptcontext.Receipt {
	receipts := append(
		[]promptcontext.Receipt(nil),
		e.options.StaticContextReceipts...,
	)
	e.planMu.Lock()
	if e.planReceipt != nil {
		filtered := receipts[:0]
		for _, receipt := range receipts {
			if receipt.Kind != promptcontext.PartitionPlan {
				filtered = append(filtered, receipt)
			}
		}
		receipts = append(filtered, *e.planReceipt)
	}
	e.planMu.Unlock()
	return append(receipts, e.TurnContextReceipts()...)
}

func (e *Engine) setApprovalEmit(emit func(Event) error) {
	scope := e.runningScope()
	if scope == nil {
		return
	}
	scope.mu.Lock()
	scope.state.approvalEmit = emit
	scope.mu.Unlock()
}

func (e *Engine) emitApproval(_ context.Context, request toolguard.ApprovalRequest) error {
	scope, err := e.registerApprovalWait(request)
	if err != nil {
		return err
	}
	scope.mu.Lock()
	emit := scope.state.approvalEmit
	scope.mu.Unlock()
	if emit == nil {
		return errors.New("approval host is not connected to an active turn")
	}
	return emit(Event{Approval: &request})
}

func (e *Engine) restoreApprovalWait(request toolguard.ApprovalRequest) error {
	scope, err := e.registerApprovalWait(request)
	if err != nil {
		return err
	}
	decision, queued := e.takeRecoveredApproval(request.RequestID)
	if !queued {
		return nil
	}
	return scope.ResolveApproval(decision)
}

func (e *Engine) registerApprovalWait(
	request toolguard.ApprovalRequest,
) (*Scope, error) {
	scope := e.runningScope()
	if scope == nil {
		return nil, errors.New("approval host is not connected to an active turn")
	}
	kernel, err := scope.kernel()
	if err != nil {
		return nil, err
	}
	if err := kernel.EnsureApproval(request.RequestID, request.CallID); err != nil {
		return nil, err
	}
	if err := scope.state.requests.Register(
		turnkernel.RequestApproval,
		request.RequestID,
	); err != nil {
		return nil, err
	}
	return scope, nil
}

func (e *Engine) expireApprovalWait(wait toolguard.ApprovalWait) error {
	scope := e.runningScope()
	if scope == nil {
		return errors.New("approval host is not connected to an active turn")
	}
	if err := scope.state.requests.Resolve(
		turnkernel.RequestApproval,
		wait.RequestID,
	); err != nil {
		return err
	}
	kernel, err := scope.kernel()
	if err != nil {
		return err
	}
	if err := kernel.ResolveApproval(wait.RequestID, false); err != nil {
		return err
	}
	scope.mu.Lock()
	emit := scope.state.approvalEmit
	scope.mu.Unlock()
	if emit == nil {
		return errors.New("approval host is not connected to an active turn")
	}
	return emit(Event{ApprovalResolution: &ApprovalResolution{
		RequestID: wait.RequestID,
		Decision:  "deny",
		Reason:    "approval_expired",
	}})
}

func (e *Engine) queueRecoveredApproval(
	decision toolguard.ApprovalDecision,
) bool {
	e.scopeMu.Lock()
	defer e.scopeMu.Unlock()
	return e.approvalRecovery.Queue(decision.RequestID, decision)
}

func (e *Engine) takeRecoveredApproval(
	requestID string,
) (toolguard.ApprovalDecision, bool) {
	e.scopeMu.Lock()
	defer e.scopeMu.Unlock()
	return e.approvalRecovery.Take(requestID)
}
