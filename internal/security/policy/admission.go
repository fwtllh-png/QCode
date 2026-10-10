package policy

import "errors"

// WithRevision serializes the final start against policy updates. The callback
// may only create the process; it must not reenter Runtime or wait for it.
func (r *Runtime) WithRevision(revision uint64, start func() error) error {
	if r == nil {
		return errors.New("policy unavailable at process start")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refreshUserRulesLocked()
	if r.Revision != revision {
		return errors.New("policy changed before process start")
	}
	if r.userSource != nil {
		source, ok := r.userSource.(interface {
			WithUserRulesVersion(uint64, func() error) error
		})
		if !ok {
			return errors.New("user rule source cannot fence process start")
		}
		return source.WithUserRulesVersion(r.userSourceVersion, start)
	}
	return start()
}

func (s *PermissionsStore) WithUserRulesVersion(version uint64, start func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.snapshot.Load()
	if current == nil || current.version != version {
		return errors.New("user rules changed before process start")
	}
	return start()
}
