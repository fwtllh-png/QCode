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
		s.metrics.Error()
		return protocol.NewProblem(protocol.CodeInvalidArgument, err.Error(), false, err)
	}
	canonical, err := CanonicalOperationPayload(operation)
	if err != nil {
		s.metrics.Error()
		return protocol.NewProblem(protocol.CodeInvalidArgument, err.Error(), false, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.accepting {
		s.metrics.Error()
		return ErrClosed
	}
	if len(s.operations) == cap(s.operations) {
		s.metrics.Error()
		return ErrQueueFull
	}
	acceptance, err := s.accept(ctx, operation, idempotencyKey, canonical)
	if err != nil {
		s.metrics.Error()
		return err
	}
	if acceptance.Duplicate {
		return nil
	}
	select {
	case s.operations <- acceptedOperation{
		operation: operation, idempotencyKey: idempotencyKey, canonical: canonical,
	}:
		s.metrics.OperationSubmitted()
		if s.logger != nil {
			s.logger.Info("runtime operation submitted", "operation_id", operation.ID, "kind", operation.Kind)
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
		LastSequence: r.hub.Snapshot().LastSequence,
		CompletedAt:  time.Now().UTC(),
	}
}

func (s *OperationService) accept(
	ctx context.Context,
	operation protocol.Operation,
	idempotencyKey string,
	canonical []byte,
) (Acceptance, error) {
	if s.lifecycle != nil {
		acceptance, err := s.lifecycle.Accept(
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
				ID: operation.ID, IdempotencyKey: idempotencyKey,
				Canonical: append([]byte(nil), canonical...),
			}
			s.accepted[operation.ID] = pending
			if idempotencyKey != "" {
				s.acceptedKeys[idempotencyKey] = operation.ID
			}
		}
		return acceptance, nil
	}
	if existing, exists := s.accepted[operation.ID]; exists {
		if string(existing.Canonical) != string(canonical) {
			return Acceptance{}, ErrOperationConflict
		}
		return Acceptance{OperationID: operation.ID, Duplicate: true}, nil
	}
	if existing, exists := s.committed[operation.ID]; exists {
		if string(existing.Canonical) != string(canonical) {
			return Acceptance{}, ErrOperationConflict
		}
		return Acceptance{
			OperationID: operation.ID, Duplicate: true, Committed: true,
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
				OperationID: existingID, Duplicate: true, Committed: !pending,
			}, nil
		}
	}
	pending := PendingOperation{
		ID: operation.ID, IdempotencyKey: idempotencyKey,
		Canonical: append([]byte(nil), canonical...),
	}
	s.accepted[operation.ID] = pending
	if idempotencyKey != "" {
		s.acceptedKeys[idempotencyKey] = operation.ID
	}
	return Acceptance{OperationID: operation.ID}, nil
}

func (s *OperationService) commit(operationID protocol.OperationID) {
	receipt := CommitReceipt{
		OperationID:  operationID,
		Status:       "committed",
		LastSequence: s.hub.Snapshot().LastSequence,
		CompletedAt:  time.Now().UTC(),
	}
	if s.lifecycle != nil {
		if err := s.lifecycle.Commit(context.Background(), receipt); err != nil {
			s.metrics.Error()
			if s.logger != nil {
				s.logger.Error(
					"runtime operation commit failed",
					"operation_id", operationID,
					"error", err,
				)
			}
			return
		}
	}
	s.commitLocal(operationID)
}

func (s *OperationService) commitLocal(operationID protocol.OperationID) {
	s.mu.Lock()
	if pending, exists := s.accepted[operationID]; exists {
		s.committed[operationID] = pending
	}
	delete(s.accepted, operationID)
	s.mu.Unlock()
}
