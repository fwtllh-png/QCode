package authority

import "errors"

// Start consumes the launch right of an already-consumed lease. Keeping the
// authority lock across process creation orders starts, revocations and expiry.
// The callback must not reenter LeaseAuthority or wait for process completion.
func (a *LeaseAuthority) Start(lease ExecutionLease, start func() error) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	record, err := a.authenticRecord(lease)
	if err != nil {
		return err
	}
	if record.state != LeaseConsumed || record.started || !lease.expiresAt.After(a.now()) {
		return errors.New("execution lease is not eligible for process start")
	}
	record.started = true
	return start()
}
