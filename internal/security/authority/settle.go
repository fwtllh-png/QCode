package authority

import (
	"errors"
	"time"
)

// RunSettled is the broker transaction skeleton: it consumes the lease, runs
// fn with a settlement that starts failed (failureReason names the broker
// operation that failed), and settles exactly once with CompletedAt stamped.
// Settle's error joins fn's error so a settlement failure can never be
// swallowed, and the settlement is returned for evidence projection even when
// fn failed. A Consume failure returns before any settlement exists, matching
// the never-consumed lease semantics.
//
// Brokers execute side effects through this helper instead of open-coding
// Consume/defer-Settle, so a new broker cannot forget to settle.
func (a *LeaseAuthority) RunSettled(
	lease ExecutionLease,
	validation LeaseValidation,
	failureReason string,
	now func() time.Time,
	fn func(settlement *Settlement) error,
) (Settlement, error) {
	if err := a.Consume(lease, validation); err != nil {
		return Settlement{}, err
	}
	settlement := Settlement{Status: "failed", Reason: failureReason}
	err := fn(&settlement)
	settlement.CompletedAt = now().UTC()
	return settlement, errors.Join(err, a.Settle(lease, settlement))
}
