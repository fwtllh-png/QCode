package app

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	"sort"
	"time"
)

func (r StartTurnHandler) validateStart(payload *protocol.StartTurnPayload) error {
	if payload.Idle {
		if checker, ok := r.engine.(interface{ AllowIdleTurn() error }); ok {
			if err := checker.AllowIdleTurn(); err != nil {
				return err
			}
		}
	}
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
	if restorer, ok := r.engine.(interface {
		RestorePendingApproval(PendingApproval) error
		RestorePendingInput(PendingInput) error
	}); ok {
		r.EventService.mu.Lock()
		approvals := make([]PendingApproval, 0, len(r.approvals))
		for _, approval := range r.approvals {
			approvals = append(approvals, approval)
		}
		inputs := make([]PendingInput, 0, len(r.inputs))
		for _, input := range r.inputs {
			inputs = append(inputs, input)
		}
		r.EventService.mu.Unlock()
		sort.Slice(approvals, func(i, j int) bool {
			return approvals[i].RequestID < approvals[j].RequestID
		})
		sort.Slice(inputs, func(i, j int) bool {
			return inputs[i].RequestID < inputs[j].RequestID
		})
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
	pending := r.OperationService.pendingOperations()
	sort.Slice(pending, func(i, j int) bool {
		return pending[i].ID < pending[j].ID
	})
	for _, pendingOperation := range pending {
		operation, err := decodePendingOperation(pendingOperation)
		if err != nil {
			return err
		}
		if operation.Kind != protocol.OperationStartTurn {
			continue
		}
		if operation.Kind == protocol.OperationStartTurn {
			threadID, turnID, _ := protocol.OperationReferences(operation)
			if pendingOperation.SessionID != "" && r.profiles != nil {
				if _, err := r.RestoreSessionProfile(
					ctx,
					pendingOperation.SessionID,
					threadID,
				); err != nil {
					return fmt.Errorf(
						"restore profile before interrupted turn %s: %w",
						turnID,
						err,
					)
				}
			}
			facts, err := r.terminalStore.LoadDomainFacts(
				ctx,
				string(turnID),
			)
			if err != nil {
				// A single unrestorable Turn must not block Runtime boot.
				continue
			}
			start, _ := operation.Payload.(*protocol.StartTurnPayload)
			if len(facts) == 0 && (start == nil || start.QueueID == "") {
				continue
			}
		}
		select {
		case r.operations <- acceptedOperation{
			operation:      operation,
			idempotencyKey: pendingOperation.IdempotencyKey,
			canonical: append(
				[]byte(nil),
				pendingOperation.Canonical...,
			),
		}:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
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
	r.hub.Restore(recovery.LastSequence)
	for turnID, kind := range recovery.Terminals {
		r.terminals[turnID] = kind
	}
	for requestID, approval := range recovery.PendingApprovals {
		r.approvals[requestID] = approval
		if approval.ItemID != "" {
			r.approvalItems[eventItemOwner(approval.TurnID, requestID)] = approval.ItemID
		}
	}
	for requestID, input := range recovery.PendingInputs {
		r.inputs[requestID] = input
		if input.ItemID != "" {
			r.inputItems[eventItemOwner(input.TurnID, requestID)] = input.ItemID
		}
	}
	for owner, itemID := range recovery.ToolItems {
		if owner.TurnID != "" && owner.LocalID != "" && itemID != "" {
			r.toolItems[owner] = itemID
		}
	}
	for operationID, pending := range recovery.PendingOperations {
		r.OperationService.accepted[operationID] = pending
		if pending.IdempotencyKey != "" {
			r.OperationService.acceptedKeys[pending.IdempotencyKey] = operationID
		}
	}
	r.TurnQueueService.Restore(
		recovery.PendingQueuedTurns,
		recovery.PendingOperations,
	)
}
