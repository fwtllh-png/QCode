package app

import (
	"context"
	"errors"
	"time"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func (s *OperationService) SubmitWithKey(
	ctx context.Context,
	operation protocol.Operation,
	idempotencyKey string,
) error {
	if err := operation.Validate(); err != nil {
		s.runtime.metrics.Error()
		return protocol.NewProblem(protocol.CodeInvalidArgument, err.Error(), false, err)
	}
	canonical, err := CanonicalOperationPayload(operation)
	if err != nil {
		s.runtime.metrics.Error()
		return protocol.NewProblem(protocol.CodeInvalidArgument, err.Error(), false, err)
	}
	var sessionID string
	var sessionErr error
	if s.runtime.lifecycle == nil && s.runtime.sessionLifecycle != nil {
		threadID, _, _ := protocol.OperationReferences(operation)
		sessionID, sessionErr = s.runtime.sessionLifecycle.SessionForThread(ctx, threadID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.accepting {
		s.runtime.metrics.Error()
		return ErrClosed
	}
	if s.workspaceOperation {
		return retryableProblem(protocol.CodeConflict, "a Workspace Git operation is active")
	}
	if len(s.withdrawing) != 0 {
		return retryableProblem(protocol.CodeConflict, "Turn withdrawal is in progress")
	}
	if len(s.operations) == cap(s.operations) {
		s.runtime.metrics.Error()
		return ErrQueueFull
	}
	// Resolve memory-mode ownership outside the admission lock, but preserve
	// admission failures before reporting an ownership lookup failure.
	if sessionErr != nil {
		s.runtime.metrics.Error()
		return sessionErr
	}
	acceptance, err := s.accept(ctx, operation, idempotencyKey, canonical, sessionID)
	if err != nil {
		s.runtime.metrics.Error()
		return err
	}
	if acceptance.Duplicate {
		return nil
	}
	select {
	case s.operations <- acceptedOperation{
		operation: operation, idempotencyKey: idempotencyKey, canonical: canonical,
	}:
		s.runtime.metrics.OperationSubmitted()
		if s.runtime.logger != nil {
			s.runtime.logger.Info("runtime operation submitted", "operation_id", operation.ID, "kind", operation.Kind)
		}
		return nil
	default:
		return errors.New("runtime queue capacity changed during operation acceptance")
	}
}

func (r *OperationService) operationCommitReceipt(
	operationID protocol.OperationID,
) CommitReceipt {
	return CommitReceipt{
		OperationID:  operationID,
		Status:       "committed",
		LastSequence: r.runtime.hub.Snapshot().LastSequence,
		CompletedAt:  time.Now().UTC(),
	}
}

func (s *OperationService) accept(
	ctx context.Context,
	operation protocol.Operation,
	idempotencyKey string,
	canonical []byte,
	sessionID string,
) (Acceptance, error) {
	if s.runtime.lifecycle != nil {
		acceptance, err := s.runtime.lifecycle.Accept(
			ctx, operation, idempotencyKey, canonical,
		)
		if err != nil {
			if errors.Is(err, ErrOperationConflict) {
				return Acceptance{}, ErrOperationConflict
			}
			return Acceptance{}, err
		}
		if !acceptance.Duplicate {
			pending := PendingOperation{
				ID: operation.ID, SessionID: acceptance.SessionID, IdempotencyKey: idempotencyKey,
				Canonical: append([]byte(nil), canonical...),
			}
			s.accepted[operation.ID] = pending
		}
		return acceptance, nil
	}
	if existing, exists := s.accepted[operation.ID]; exists {
		if string(existing.Canonical) != string(canonical) {
			return Acceptance{}, ErrOperationConflict
		}
		return Acceptance{OperationID: operation.ID, SessionID: existing.SessionID, Duplicate: true}, nil
	}
	if existing, exists := s.committed[operation.ID]; exists {
		if string(existing.Canonical) != string(canonical) {
			return Acceptance{}, ErrOperationConflict
		}
		return Acceptance{
			OperationID: operation.ID, SessionID: existing.SessionID, Duplicate: true, Committed: true,
		}, nil
	}
	if idempotencyKey != "" {
		if existingID, exists := s.acceptedKeys[idempotencyKey]; exists {
			existing, pending := s.accepted[existingID]
			if !pending {
				existing = s.committed[existingID]
			}
			if string(existing.Canonical) != string(canonical) {
				return Acceptance{}, ErrOperationConflict
			}
			return Acceptance{
				OperationID: existingID, SessionID: existing.SessionID, Duplicate: true, Committed: !pending,
			}, nil
		}
	}
	pending := PendingOperation{
		ID: operation.ID, SessionID: sessionID, IdempotencyKey: idempotencyKey,
		Canonical: append([]byte(nil), canonical...),
	}
	s.accepted[operation.ID] = pending
	if idempotencyKey != "" {
		s.acceptedKeys[idempotencyKey] = operation.ID
	}
	return Acceptance{OperationID: operation.ID, SessionID: sessionID}, nil
}

func (s *OperationService) commitLocal(operationID protocol.OperationID) {
	s.mu.Lock()
	if pending, exists := s.accepted[operationID]; exists && s.runtime.lifecycle == nil {
		s.committed[operationID] = pending
	}
	delete(s.accepted, operationID)
	if s.changed != nil {
		close(s.changed)
		s.changed = make(chan struct{})
	}
	s.mu.Unlock()
}
