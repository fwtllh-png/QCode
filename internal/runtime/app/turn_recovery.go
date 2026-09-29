package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func (r StartTurnHandler) validateStart(payload *protocol.StartTurnPayload) error {
	if payload.Recovery == nil {
		return nil
	}
	events, err := r.events.Replay(r.ctx, 0)
	if err != nil {
		return fmt.Errorf("validate Turn recovery source: %w", err)
	}
	source := payload.Recovery.SourceTurnID
	terminal := false
	var startedOperationID protocol.OperationID
	var sourceThreadID protocol.ThreadID
	for _, event := range events {
		if event.TurnID != source {
			continue
		}
		if sourceThreadID == "" {
			sourceThreadID = event.ThreadID
		} else if event.ThreadID != sourceThreadID {
			return protocol.NewProblem(
				protocol.CodeConflict,
				"Turn recovery source has inconsistent Thread identity",
				false,
				nil,
			)
		}
		switch data := event.Data.(type) {
		case *protocol.TurnStartedData:
			startedOperationID = event.OperationID
		case *protocol.OperationRejectedData:
			terminal = terminal ||
				(event.OperationID == startedOperationID &&
					protocol.FaultAllowsTurnRecovery(data.Fault))
		default:
			terminal = terminal || protocol.IsTerminalEvent(event.Kind)
		}
	}
	if sourceThreadID != "" && sourceThreadID != payload.ThreadID {
		if r.sessionLifecycle == nil {
			return protocol.NewProblem(
				protocol.CodeConflict,
				"Turn recovery source belongs to another Thread",
				false,
				nil,
			)
		}
		sourceSession, sourceErr := r.sessionLifecycle.SessionForThread(
			r.ctx,
			sourceThreadID,
		)
		targetSession, targetErr := r.sessionLifecycle.SessionForThread(
			r.ctx,
			payload.ThreadID,
		)
		if sourceErr != nil || targetErr != nil || sourceSession != targetSession {
			return protocol.NewProblem(
				protocol.CodeConflict,
				"Turn recovery source belongs to another Session",
				false,
				nil,
			)
		}
	}
	if err := r.requireRetainedTurn(r.ctx, sourceThreadID, source); err != nil {
		return err
	}
	if !terminal {
		return protocol.NewProblem(
			protocol.CodeConflict,
			"Turn recovery source is unavailable or not terminal",
			false,
			nil,
		)
	}
	return nil
}

func (r *RecoveryService) recoverPendingTurns(ctx context.Context) error {
	if restorer, ok := r.runtime.engine.(interface {
		RestorePendingApproval(PendingApproval) error
		RestorePendingInput(PendingInput) error
	}); ok {
		approvals, inputs := r.runtime.EventService.pendingInteractions()
		for _, approval := range approvals {
			if err := restorer.RestorePendingApproval(approval); err != nil {
				return err
			}
		}
		for _, input := range inputs {
			if err := restorer.RestorePendingInput(input); err != nil {
				return err
			}
		}
	}
	pending := r.runtime.OperationService.pendingOperations()
	sort.Slice(pending, func(i, j int) bool {
		return pending[i].ID < pending[j].ID
	})
	// Every recovered operation is enqueued so the dispatch loop settles it:
	// a StartTurn with a restorable fact chain resumes, everything else is
	// committed or rejected rather than left accepted without an owner.
	for _, pendingOperation := range pending {
		operation, err := decodePendingOperation(pendingOperation)
		if err != nil {
			return err
		}
		accepted := acceptedOperation{
			operation:      operation,
			idempotencyKey: pendingOperation.IdempotencyKey,
			canonical: append(
				[]byte(nil),
				pendingOperation.Canonical...,
			),
		}
		if operation.Kind != protocol.OperationStartTurn {
			accepted.settleOnRecovery = restartInterruptedProblem(operation.Kind)
		} else if err := r.classifyRecoveredStart(
			ctx,
			pendingOperation,
			&accepted,
		); err != nil {
			return err
		}
		if err := r.runtime.OperationService.enqueueRecovered(ctx, accepted); err != nil {
			return err
		}
	}
	return nil
}

// TurnQuarantineStore retires a durable active Turn that recovery cannot
// restore, releasing its Thread and coordinator lease.
type TurnQuarantineStore interface {
	QuarantineActiveTurn(context.Context, string) error
}

func (r *RecoveryService) classifyRecoveredStart(
	ctx context.Context,
	pending PendingOperation,
	accepted *acceptedOperation,
) error {
	threadID, turnID, _ := protocol.OperationReferences(accepted.operation)
	if pending.SessionID != "" && r.runtime.profiles != nil {
		if _, err := r.runtime.RestoreSessionProfile(
			ctx,
			pending.SessionID,
			threadID,
		); err != nil {
			return fmt.Errorf(
				"restore profile before interrupted turn %s: %w",
				turnID,
				err,
			)
		}
	}
	facts, err := r.runtime.terminalStore.LoadDomainFacts(ctx, string(turnID))
	if err == nil && len(facts) != 0 {
		err = turnkernel.ValidateDomainFacts(string(turnID), facts)
	}
	if err != nil {
		accepted.settleOnRecovery = r.quarantineTurn(ctx, turnID, err)
		return nil
	}
	start, _ := accepted.operation.Payload.(*protocol.StartTurnPayload)
	accepted.interruptedBeforeStart = len(facts) == 0 &&
		(start == nil || start.QueueID == "")
	return nil
}

// quarantineTurn retires a Turn whose durable facts cannot be restored. One
// such Turn must not block boot, hold its Thread, or be retried on every
// restart, so it is failed durably and its operation is rejected.
func (r *RecoveryService) quarantineTurn(
	ctx context.Context,
	turnID protocol.TurnID,
	cause error,
) error {
	r.runtime.metrics.Error()
	if store, ok := r.runtime.terminalStore.(TurnQuarantineStore); ok {
		if err := store.QuarantineActiveTurn(ctx, string(turnID)); err != nil {
			// The rejection projection still fails the Turn row.
			cause = errors.Join(cause, fmt.Errorf("quarantine turn: %w", err))
		}
	}
	if r.runtime.logger != nil {
		r.runtime.logger.Error(
			"unrestorable turn quarantined during recovery",
			"turn_id", turnID, "error", cause,
		)
	}
	return protocol.NewFault(
		protocol.CodeInternal,
		"turn state could not be restored after a Runtime restart; the turn was quarantined",
		false,
		protocol.FaultMetadata{
			Origin:      protocol.FaultOriginKernel,
			Disposition: protocol.FaultRetryTurn,
			SideEffects: protocol.SideEffectUnknown,
		},
		cause,
	)
}

func decodePendingOperation(
	pending PendingOperation,
) (protocol.Operation, error) {
	var envelope struct {
		Kind    protocol.OperationKind `json:"kind"`
		Payload json.RawMessage        `json:"payload"`
	}
	if err := json.Unmarshal(pending.Canonical, &envelope); err != nil {
		return protocol.Operation{}, fmt.Errorf(
			"decode pending operation %s: %w",
			pending.ID,
			err,
		)
	}
	payload, err := protocol.DecodeOperationPayload(
		envelope.Kind,
		envelope.Payload,
	)
	if err != nil {
		return protocol.Operation{}, err
	}
	operation := protocol.Operation{
		Version:   protocol.Version,
		ID:        pending.ID,
		Kind:      envelope.Kind,
		CreatedAt: time.Unix(0, 1).UTC(),
		Payload:   payload,
	}
	return operation, operation.Validate()
}

func (r *RecoveryService) restore(recovery RecoveryState) {
	r.runtime.EventService.restore(recovery)
	r.runtime.OperationService.restore(recovery.PendingOperations)
	r.runtime.TurnQueueService.Restore(
		recovery.PendingQueuedTurns,
		recovery.PendingOperations,
	)
}
