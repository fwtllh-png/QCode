package app

import (
	"errors"
	"fmt"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type OutcomeKind string

const (
	OutcomeCommitted OutcomeKind = "committed"
	OutcomeRejected  OutcomeKind = "rejected"
	OutcomeAsync     OutcomeKind = "async"
	OutcomeTerminal  OutcomeKind = "terminal"
)

type OperationOutcome struct {
	Kind       OutcomeKind
	Events     []protocol.EventData
	Async      *AsyncTurn
	Problem    *protocol.Problem
	CommitMode CommitMode
}
type CommitMode string

const (
	CommitNow      CommitMode = "now"
	CommitDeferred CommitMode = "deferred"
)

type AsyncTurn struct {
	ThreadID    protocol.ThreadID
	TurnID      protocol.TurnID
	OperationID protocol.OperationID
	ItemID      protocol.ItemID
}
type operationDispatcher struct {
	runtime *Runtime
}

func (d operationDispatcher) Dispatch(accepted acceptedOperation) OperationOutcome {
	if d.runtime == nil {
		return OperationOutcome{
			Kind: OutcomeRejected, Problem: protocol.ProblemOf(errors.New("runtime is not configured")),
			CommitMode: CommitNow,
		}
	}
	operation := accepted.operation
	if d.runtime.engine == nil {
		return OperationOutcome{
			Kind:       OutcomeRejected,
			Problem:    protocol.ProblemOf(errors.New("runtime engine is not configured")),
			CommitMode: CommitNow,
		}
	}
	switch payload := operation.Payload.(type) {
	case *protocol.StartTurnPayload:
		return StartTurnHandler{d.runtime}.Handle(operation, payload)
	case *protocol.CancelTurnPayload:
		return CancelTurnHandler{d.runtime}.Handle(operation, payload)
	case *protocol.SteerTurnPayload:
		return SteerTurnHandler{d.runtime}.Handle(operation, payload)
	case *protocol.EnqueueTurnPayload:
		return EnqueueTurnHandler{d.runtime}.Handle(operation, payload)
	case *protocol.UpdateQueuedTurnPayload:
		return UpdateQueuedTurnHandler{d.runtime}.Handle(operation, payload)
	case *protocol.RemoveQueuedTurnPayload:
		return RemoveQueuedTurnHandler{d.runtime}.Handle(operation, payload)
	case *protocol.PromoteQueuedTurnPayload:
		return PromoteQueuedTurnHandler{d.runtime}.Handle(operation, payload)
	case *protocol.ApprovalDecisionPayload:
		return ApprovalHandler{d.runtime}.Handle(operation, payload)
	case *protocol.InputReplyPayload:
		return InputHandler{d.runtime}.Handle(operation, payload)
	case *protocol.CompactThreadPayload:
		if _, active := d.runtime.active.LookupThread(payload.ThreadID); active {
			return finishOutcome(ErrActiveTurn)
		}
		return finishOutcome(d.runtime.invoke(operation, func(sink EngineSink) error {
			return d.runtime.engine.CompactThread(d.runtime.ctx, payload, sink)
		}))
	case *protocol.ForkThreadPayload:
		if _, active := d.runtime.active.LookupThread(payload.ThreadID); active {
			return finishOutcome(ErrActiveTurn)
		}
		return finishOutcome(d.runtime.invoke(operation, func(sink EngineSink) error {
			return d.runtime.engine.ForkThread(d.runtime.ctx, payload, sink)
		}))
	case *protocol.RevertTurnPayload:
		if _, active := d.runtime.active.LookupThread(payload.ThreadID); active {
			return finishOutcome(ErrActiveTurn)
		}
		return finishOutcome(d.runtime.invoke(operation, func(sink EngineSink) error {
			return d.runtime.engine.RevertTurn(d.runtime.ctx, payload, sink)
		}))
	default:
		return finishOutcome(errors.New("operation payload is not supported"))
	}
}

type StartTurnHandler struct{ *Runtime }
type CancelTurnHandler struct{ *Runtime }
type SteerTurnHandler struct{ *Runtime }
type ApprovalHandler struct{ *Runtime }
type InputHandler struct{ *Runtime }

func (h SteerTurnHandler) Handle(operation protocol.Operation, payload *protocol.SteerTurnPayload) OperationOutcome {
	started, err := h.run(operation, payload)
	if err != nil {
		return finishOutcome(err)
	}
	if started != nil {
		return OperationOutcome{Kind: OutcomeAsync, Async: started, CommitMode: CommitDeferred}
	}
	return OperationOutcome{Kind: OutcomeCommitted, CommitMode: CommitNow}
}
func finishOutcome(err error) OperationOutcome {
	if err != nil {
		return OperationOutcome{Kind: OutcomeRejected, Problem: protocol.ProblemOf(err), CommitMode: CommitNow}
	}
	return OperationOutcome{Kind: OutcomeCommitted, CommitMode: CommitNow}
}
func (s *OperationService) Apply(operation protocol.Operation, outcome OperationOutcome) {
	if err := validateOperationOutcome(outcome); err != nil {
		if s.reject(operation, err) == nil {
			s.commit(operation.ID)
		}
		return
	}
	if outcome.Kind == OutcomeRejected {
		if s.reject(operation, outcome.Problem) == nil {
			s.commit(operation.ID)
		}
		return
	}
	if outcome.Kind == OutcomeCommitted {
		sink := &runtimeSink{runtime: s.Runtime, operation: operation}
		drainThread := protocol.ThreadID("")
		for _, event := range outcome.Events {
			if err := sink.Emit(event); err != nil {
				return
			}
			if _, queued := event.(*protocol.TurnQueuedData); queued {
				drainThread, _, _ = protocol.OperationReferences(operation)
			}
		}
		s.commit(operation.ID)
		if drainThread != "" {
			s.Runtime.TurnQueueService.Drain(drainThread)
		}
	}
}
func validateOperationOutcome(outcome OperationOutcome) error {
	noProblem, noAsync, noEvents := outcome.Problem == nil, outcome.Async == nil, len(outcome.Events) == 0
	valid := outcome.Kind == OutcomeCommitted && noProblem && noAsync && outcome.CommitMode == CommitNow ||
		outcome.Kind == OutcomeRejected && !noProblem && noAsync && noEvents && outcome.CommitMode == CommitNow ||
		outcome.Kind == OutcomeAsync && noProblem && !noAsync && noEvents && outcome.CommitMode == CommitDeferred ||
		outcome.Kind == OutcomeTerminal && noProblem && noAsync && noEvents && outcome.CommitMode == CommitDeferred
	if valid {
		return nil
	}
	return fmt.Errorf(
		"invalid operation outcome kind=%q problem=%t",
		outcome.Kind,
		outcome.Problem != nil,
	)
}

func (r SteerTurnHandler) run(operation protocol.Operation, payload *protocol.SteerTurnPayload) (*AsyncTurn, error) {
	phase := r.turnPhase(payload.ThreadID, payload.TurnID)
	disposition := RoutePending(phase, PendingItem{Source: SourceSteer})
	switch disposition {
	case DispositionInjectCurrent:
		return nil, r.invoke(operation, func(sink EngineSink) error {
			return r.engine.SteerTurn(r.ctx, payload, sink)
		})
	case DispositionStartNewTurn:
		turnID, err := protocol.NewTurnID()
		if err != nil {
			return nil, err
		}
		itemID, err := protocol.NewItemID()
		if err != nil {
			return nil, err
		}
		start := &protocol.StartTurnPayload{
			ThreadID: payload.ThreadID, TurnID: turnID, ItemID: itemID, Prompt: payload.Prompt,
		}
		outcome := (StartTurnHandler{r.Runtime}).Handle(operation, start)
		if outcome.Problem != nil {
			return nil, outcome.Problem
		}
		return outcome.Async, nil
	default:
		return nil, fmt.Errorf(
			"pending-work rejected steer: %s", ExplainPending(phase, PendingItem{Source: SourceSteer}, disposition),
		)
	}
}

func (r ApprovalHandler) Handle(operation protocol.Operation, payload *protocol.ApprovalDecisionPayload) OperationOutcome {
	r.EventService.mu.Lock()
	pending, known := r.approvals[payload.RequestID]
	r.EventService.mu.Unlock()
	if known {
		proxied := *payload
		proxied.ThreadID = pending.ThreadID
		proxied.TurnID = pending.TurnID
		payload = &proxied
		operation.Payload = payload
	}
	phase := r.turnPhase(payload.ThreadID, payload.TurnID)
	if known {
		phase = PhaseAwaitingApproval
	}
	disposition := RoutePending(phase, PendingItem{Source: SourceApproval})
	if disposition != DispositionResumePaused {
		return finishOutcome(fmt.Errorf(
			"pending-work rejected approval: %s",
			ExplainPending(phase, PendingItem{Source: SourceApproval}, disposition),
		))
	}
	return finishOutcome(r.invoke(operation, func(sink EngineSink) error {
		return r.engine.DecideApproval(r.ctx, payload, sink)
	}))
}

func (r InputHandler) Handle(operation protocol.Operation, payload *protocol.InputReplyPayload) OperationOutcome {
	phase := r.turnPhase(payload.ThreadID, payload.TurnID)
	r.EventService.mu.Lock()
	_, known := r.inputs[payload.RequestID]
	r.EventService.mu.Unlock()
	if known {
		phase = PhaseAwaitingInput
	}
	disposition := RoutePending(phase, PendingItem{Source: SourceInput})
	if disposition != DispositionResumePaused {
		return finishOutcome(fmt.Errorf(
			"pending-work rejected input: %s",
			ExplainPending(phase, PendingItem{Source: SourceInput}, disposition),
		))
	}
	return finishOutcome(r.invoke(operation, func(sink EngineSink) error {
		return r.engine.ReplyInput(r.ctx, payload, sink)
	}))
}

func (r StartTurnHandler) Handle(operation protocol.Operation, payload *protocol.StartTurnPayload) OperationOutcome {
	return r.TurnService.Start(operation, payload)
}

func (r CancelTurnHandler) Handle(operation protocol.Operation, payload *protocol.CancelTurnPayload) OperationOutcome {
	return r.TurnService.Cancel(operation, payload)
}
