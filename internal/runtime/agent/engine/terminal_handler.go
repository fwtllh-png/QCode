package engine

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	"github.com/fwtllh-png/QCode/internal/security/policy"
)

// turnEmitter deduplicates Engine Event projection for one turn.
type turnEmitter struct {
	turn            uint64
	emitted         bool
	recoveryPending bool
	contextBudget   *ContextBudgetSnapshot
	primaryCode     protocol.ErrorCode
	primaryError    string
	primaryFault    *protocol.FaultMetadata
	secondary       []TerminalIssue
	emitFunc        func(Event) error
	committed       func() error
	release         func() error
	released        bool
	releaseReported bool
	cancelReason    func() string
	decision        func() (turnkernel.TerminalDecision, bool)
	phase           func() turnkernel.Phase
}

func newTurnEmitter(turn uint64, emit func(Event) error) *turnEmitter {
	return &turnEmitter{turn: turn, emitFunc: emit}
}

func (h *turnEmitter) setCancelReason(source func() string) { h.cancelReason = source }

func (h *turnEmitter) setTerminalDecision(source func() (turnkernel.TerminalDecision, bool)) {
	h.decision = source
}

func (h *turnEmitter) setPhase(source func() turnkernel.Phase) { h.phase = source }

// checkStatePhase validates a host state against the authoritative kernel
// phase using the phaseStates declaration. A mismatch is a projection
// invariant violation, not a business failure: the event is still emitted and
// the violation is recorded so the terminal envelope exposes the drift.
func (h *turnEmitter) checkStatePhase(state State) {
	if h.phase == nil {
		return
	}
	if phase := h.phase(); !stateAllowedForPhase(state, phase) {
		h.addSecondary("state_phase_consistency", statePhaseMismatch(state, phase))
	}
}

func (h *turnEmitter) send(state State, event Event) error {
	h.checkStatePhase(state)
	event.State, event.Turn = state, h.turn
	terminal := state == Completed || state == Failed || state == Canceled
	if terminal {
		if h.emitted {
			return nil
		}
		if err := h.releaseTurn(); err != nil {
			h.addReleaseIssue(err)
		}
		event.SecondaryIssues = mergeTerminalIssues(
			event.SecondaryIssues,
			h.secondary,
		)
		event.ContextBudget = h.contextBudget
	}
	if err := h.emitFunc(event); err != nil {
		if !terminal &&
			state != Preparing &&
			state != AwaitingApproval &&
			state != AwaitingInput {
			h.addSecondary("event_projection", err)
			return nil
		}
		if !terminal {
			return protocol.NewFault(
				protocol.CodeUnavailable,
				"required host event could not be projected",
				true,
				protocol.FaultMetadata{
					Origin:         protocol.FaultOriginProjection,
					Disposition:    protocol.FaultResumeTurn,
					SideEffects:    protocol.SideEffectDraft,
					RecoveryAction: "restore event projection and resume the turn",
				},
				err,
			)
		}
		return protocol.NewFault(
			protocol.CodeUnavailable,
			"terminal envelope could not be committed",
			true,
			protocol.FaultMetadata{
				Origin:         protocol.FaultOriginPersistence,
				Disposition:    protocol.FaultRetryStep,
				SideEffects:    protocol.SideEffectUnknown,
				RecoveryAction: "retry the idempotent terminal commit",
			},
			err,
		)
	}
	if terminal {
		h.emitted = true
		if h.committed != nil {
			if err := h.committed(); err != nil {
				h.addSecondary("session_delta_apply", err)
			}
		}
	}
	return nil
}

func (h *turnEmitter) setContextBudget(snapshot ContextBudgetSnapshot) { h.contextBudget = &snapshot }

func (h *turnEmitter) setCommitted(apply func() error) { h.committed = apply }

func (h *turnEmitter) setRelease(release func() error) { h.release = release }

func (h *turnEmitter) releaseTurn() error {
	if h.released || h.release == nil {
		return nil
	}
	if err := h.release(); err != nil {
		return err
	}
	h.released = true
	return nil
}

func (h *turnEmitter) addReleaseIssue(err error) {
	if err == nil || h.releaseReported {
		return
	}
	h.releaseReported = true
	h.addSecondary("turn_coordinator_release", err)
}

func mergeTerminalIssues(
	eventIssues []TerminalIssue,
	issues []TerminalIssue,
) []TerminalIssue {
	result := append([]TerminalIssue(nil), eventIssues...)
	for _, issue := range issues {
		found := false
		for _, current := range result {
			if current == issue {
				found = true
				break
			}
		}
		if !found {
			result = append(result, issue)
		}
	}
	return result
}

func (h *turnEmitter) suspendForRecovery() { h.recoveryPending = true }

func (h *turnEmitter) setPrimary(err error) {
	if err == nil || h.primaryError != "" {
		return
	}
	primary := firstJoinedError(err)
	problem := protocol.ProblemOf(primary)
	h.primaryCode = problem.Code
	h.primaryError = problem.Message
	h.primaryFault = problem.Fault
}

func (h *turnEmitter) addSecondary(phase string, err error) {
	if err == nil {
		return
	}
	h.secondary = append(h.secondary, TerminalIssue{
		Phase: phase, Code: protocol.CodeOf(err), Message: errorText(err),
	})
}

func firstJoinedError(err error) error {
	for err != nil {
		joined, ok := err.(interface{ Unwrap() []error })
		if !ok {
			return err
		}
		children := joined.Unwrap()
		if len(children) == 0 {
			return err
		}
		err = children[0]
	}
	return nil
}

func (e *Engine) finalizeTerminalContext(
	transaction []provider.Message,
	completed, canceled bool,
	failure error, usage provider.Usage,
	cost float64,
	send func(State, Event) error,
) (ContextBudgetSnapshot, error) {
	candidate := cloneMessages(e.history)
	original := cloneMessages(candidate)
	// A failed transaction is deliberately excluded from candidate. Its last turn
	// is therefore the most recent durable completed turn, which is safe to compact
	// within as long as the compactor preserves closed tool pairs.
	switch {
	case completed:
		candidate = cloneMessages(transaction)
		original = cloneMessages(candidate)
	case canceled:
		candidate = retainCanceledHistory(transaction)
		original = cloneMessages(candidate)
		e.sealClosedTurnMemory(agentcontext.CheckpointCanceled, nil, "canceled")
	case failure != nil && !errors.Is(failure, context.Canceled):
		candidate = append(candidate, e.failedTurnContextMessage(transaction, failure))
		original = cloneMessages(candidate)
		compaction := e.context.Compaction()
		compaction.State = nil
		e.contextAuthority().SetCompaction(compaction)
		e.sealClosedTurnMemory(
			agentcontext.CheckpointFailed,
			nil,
			protocol.ProblemOf(failure).Message,
		)
	}
	maintenanceErr := e.runTerminalCompactGate(&candidate, true, send)
	if maintenanceErr != nil {
		candidate = original
	}
	scope := e.runningScope()
	scope.mu.Lock()
	contextUsage := scope.state.contextUsage
	contextCost := scope.state.contextCost
	scope.mu.Unlock()
	usage.Add(contextUsage)
	cost += contextCost
	snapshot, snapshotErr := e.buildContextSnapshot(
		candidate,
		e.compactionState(),
		e.sessionRevision+1,
		max(uint64(1), e.stateEpoch),
	)
	if snapshotErr != nil {
		return e.contextBudgetSnapshot(candidate),
			errors.Join(maintenanceErr, snapshotErr)
	}
	accounting, err := agentcontext.PrepareAccountingDelta(
		scope.spec.Identity.TurnID,
		usage,
		cost,
	)
	if err != nil {
		return e.contextBudgetSnapshot(candidate), errors.Join(maintenanceErr, err)
	}
	delta, err := agentcontext.NewSessionDelta(
		snapshot,
		accounting,
		agentcontext.ManifestLimits{
			OwnerDeltaMaxSegments: e.options.Context.OwnerDeltaMaxSegments,
			OwnerDeltaMaxBytes:    e.options.Context.OwnerDeltaMaxBytes,
		},
	)
	if err != nil {
		return e.contextBudgetSnapshot(candidate), errors.Join(maintenanceErr, err)
	}
	e.stageSessionDelta(delta)
	return e.contextBudgetSnapshot(candidate), maintenanceErr
}

func (e *Engine) failedTurnContextMessage(transaction []provider.Message, failure error) provider.Message {
	problem := protocol.ProblemOf(failure)
	problem.Message = agentcontext.TruncateUTF8(problem.Message, 512)
	failures := e.contextAuthority().Failures().List()
	if len(failures) > 4 {
		failures = failures[:4]
	}
	payload, _ := json.Marshal(map[string]any{
		"v": 1, "status": "failed",
		"turn_id": e.runningScope().spec.Identity.TurnID,
		"goal":    agentcontext.TruncateUTF8(agentcontext.ActiveTurnGoal(transaction), 512),
		"problem": problem, "failures": failures,
	})
	message := provider.TextMessage(provider.RoleSystem, "[turn_terminal]\n"+string(payload))
	message.Turn = e.turn
	return message
}

func (h *turnEmitter) finish(ctx context.Context, result *Result, resultErr *error) {
	if h.emitted || h.recoveryPending {
		return
	}
	state, event := h.terminalEvent(ctx, *resultErr)
	if h.decision != nil {
		if decision, ok := h.decision(); ok {
			switch decision.Kind {
			case turnkernel.TerminalCompleted:
				state = Completed
				event = Event{}
			case turnkernel.TerminalCanceled:
				state = Canceled
				event = Event{
					Error:        decision.Message,
					CancelReason: decision.Message,
				}
			case turnkernel.TerminalFailed:
				state = Failed
				h.setPrimary(*resultErr)
				event = Event{
					ErrorCode: protocol.ErrorCode(decision.Code),
					Error:     decision.Message,
					Fault:     protocol.CloneFaultMetadata(decision.Fault),
					Convergence: turnkernel.ProtocolConvergence(
						decision.Convergence,
					),
					SecondaryIssues: append(
						[]TerminalIssue(nil),
						h.secondary...,
					),
				}
			}
		}
	}
	if err := h.send(state, event); err != nil {
		*resultErr = errors.Join(*resultErr, err)
		result.State = AwaitingRecovery
		return
	}
	result.State = state
}

func (h *turnEmitter) terminalEvent(ctx context.Context, err error) (State, Event) {
	reason := ""
	if h.cancelReason != nil {
		reason = h.cancelReason()
	}
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return Canceled, Event{
			Error:        "turn canceled",
			CancelReason: protocol.NormalizeCancelReason(reason),
		}
	}
	if reason == protocol.CancelReasonApprovalCanceled {
		return Canceled, Event{
			Error:        "approval canceled",
			CancelReason: reason,
		}
	}
	var decision *policy.DecisionError
	if errors.As(err, &decision) && decision.Code == "approval_canceled" {
		return Canceled, Event{
			Error:        "approval canceled",
			CancelReason: protocol.CancelReasonApprovalCanceled,
		}
	}
	h.setPrimary(err)
	return Failed, Event{
		ErrorCode: h.primaryCode, Error: h.primaryError,
		Fault:           protocol.CloneFaultMetadata(h.primaryFault),
		SecondaryIssues: append([]TerminalIssue(nil), h.secondary...),
	}
}

func (h *turnEmitter) terminalRequest(
	ctx context.Context, err error,
) (State, Event, turnkernel.TerminalRequested) {
	state, event := h.terminalEvent(ctx, err)
	request := turnkernel.TerminalRequested{}
	if state == Canceled {
		request.CancelReason = terminalValue(
			event.CancelReason,
			protocol.CancelReasonShutdown,
		)
	} else {
		request.FailureCode = string(event.ErrorCode)
		request.FailureMessage = terminalValue(event.Error, "turn failed")
		request.Fault = h.primaryFault
	}
	return state, event, request
}

func terminalValue(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}
