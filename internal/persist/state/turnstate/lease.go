package turnstate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var ErrTurnLeaseHeld = errors.New("turn coordinator lease is held")

type ActiveTurn struct {
	TurnID   string
	ThreadID string
}

func (s *Store) ClaimActiveTurns(
	ctx context.Context,
	owner string,
	lease time.Duration,
) ([]ActiveTurn, error) {
	if err := validateLeaseRequest(s, owner, lease); err != nil {
		return nil, err
	}
	var claimed []ActiveTurn
	err := s.database.Transaction(ctx, func(tx *sql.Tx) error {
		now := s.now().UTC()
		rows, err := tx.QueryContext(ctx, `
			SELECT t.id, t.thread_id
			FROM turns AS t
			LEFT JOIN turn_terminal_envelopes AS terminal
				ON terminal.turn_id = t.id
			LEFT JOIN turn_coordinator_leases AS lease
				ON lease.turn_id = t.id
			WHERE t.status = 'active'
				AND terminal.turn_id IS NULL
				AND (
					lease.turn_id IS NULL
					OR lease.owner = ?
					OR lease.expires_at <= ?
				)
			ORDER BY t.created_at, t.id`,
			owner,
			formatLeaseTime(now),
		)
		if err != nil {
			return err
		}
		var candidates []ActiveTurn
		for rows.Next() {
			var turn ActiveTurn
			if err := rows.Scan(&turn.TurnID, &turn.ThreadID); err != nil {
				_ = rows.Close()
				return err
			}
			candidates = append(candidates, turn)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, turn := range candidates {
			ok, err := claimTurnLease(
				ctx,
				tx,
				turn.TurnID,
				owner,
				now,
				lease,
			)
			if err != nil {
				return err
			}
			if ok {
				claimed = append(claimed, turn)
			}
		}
		return nil
	})
	return claimed, err
}

// QuarantineActiveTurn releases an unrestorable active Turn so recovery can
// continue. The thread is then free to accept a new Turn. It is idempotent: a
// Turn that is no longer active only loses any leftover coordinator lease.
func (s *Store) QuarantineActiveTurn(
	ctx context.Context,
	turnID string,
) error {
	if s == nil || s.database == nil || strings.TrimSpace(turnID) == "" {
		return errors.New("quarantine active turn is incomplete")
	}
	now := formatLeaseTime(s.now())
	return s.database.Transaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(
			ctx,
			`UPDATE turns
			 SET status = 'failed', updated_at = ?, completed_at = ?
			 WHERE id = ? AND status = 'active'`,
			now,
			now,
			turnID,
		)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(
			ctx,
			`DELETE FROM turn_coordinator_leases WHERE turn_id = ?`,
			turnID,
		)
		return err
	})
}

func (s *Store) ClaimTurn(
	ctx context.Context,
	turnID string,
	owner string,
	lease time.Duration,
) error {
	if err := validateLeaseRequest(s, owner, lease); err != nil {
		return err
	}
	if strings.TrimSpace(turnID) == "" {
		return errors.New("turn coordinator lease turn id is empty")
	}
	return s.database.Transaction(ctx, func(tx *sql.Tx) error {
		now := s.now().UTC()
		ok, err := claimTurnLease(ctx, tx, turnID, owner, now, lease)
		if err != nil {
			return err
		}
		if !ok {
			return ErrTurnLeaseHeld
		}
		return nil
	})
}

func (s *Store) RenewTurns(
	ctx context.Context,
	owner string,
	turnIDs []string,
	lease time.Duration,
) error {
	if err := validateLeaseRequest(s, owner, lease); err != nil {
		return err
	}
	if len(turnIDs) == 0 {
		return nil
	}
	return s.database.Transaction(ctx, func(tx *sql.Tx) error {
		now := s.now().UTC()
		for _, turnID := range turnIDs {
			result, err := tx.ExecContext(ctx, `
				UPDATE turn_coordinator_leases
				SET expires_at = ?, updated_at = ?
				WHERE turn_id = ? AND owner = ?`,
				formatLeaseTime(now.Add(lease)),
				formatLeaseTime(now),
				turnID,
				owner,
			)
			if err != nil {
				return err
			}
			affected, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if affected != 1 {
				return fmt.Errorf(
					"renew turn %q: %w",
					turnID,
					ErrTurnLeaseHeld,
				)
			}
		}
		return nil
	})
}

func (s *Store) ReleaseTurns(
	ctx context.Context,
	owner string,
	turnIDs []string,
) error {
	if s == nil || s.database == nil {
		return errors.New("turn coordinator lease store is nil")
	}
	if strings.TrimSpace(owner) == "" {
		return errors.New("turn coordinator lease owner is empty")
	}
	if len(turnIDs) == 0 {
		return nil
	}
	return s.database.Transaction(ctx, func(tx *sql.Tx) error {
		for _, turnID := range turnIDs {
			if _, err := tx.ExecContext(ctx, `
				DELETE FROM turn_coordinator_leases
				WHERE turn_id = ? AND owner = ?`,
				turnID,
				owner,
			); err != nil {
				return err
			}
		}
		return nil
	})
}

func claimTurnLease(
	ctx context.Context,
	tx *sql.Tx,
	turnID string,
	owner string,
	now time.Time,
	lease time.Duration,
) (bool, error) {
	result, err := tx.ExecContext(ctx, `
		INSERT INTO turn_coordinator_leases(
			turn_id, owner, expires_at, updated_at
		)
		SELECT id, ?, ?, ? FROM turns
		WHERE id = ? AND status = 'active'
		ON CONFLICT(turn_id) DO UPDATE SET
			owner = excluded.owner,
			expires_at = excluded.expires_at,
			updated_at = excluded.updated_at
		WHERE turn_coordinator_leases.owner = excluded.owner
			OR turn_coordinator_leases.expires_at <= ?`,
		owner,
		formatLeaseTime(now.Add(lease)),
		formatLeaseTime(now),
		turnID,
		formatLeaseTime(now),
	)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}

func validateLeaseRequest(
	store *Store,
	owner string,
	lease time.Duration,
) error {
	if store == nil || store.database == nil {
		return errors.New("turn coordinator lease store is nil")
	}
	if strings.TrimSpace(owner) == "" {
		return errors.New("turn coordinator lease owner is empty")
	}
	if lease <= 0 {
		return errors.New("turn coordinator lease duration must be positive")
	}
	return nil
}

func formatLeaseTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000000000Z")
}
