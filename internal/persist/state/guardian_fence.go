package state

import "context"

// WithAuthorizationEvents fences event publication through the short physical
// start callback. Ordinary appends retain their concurrent group-commit path.
// The callback may read the Store but must never append events.
func (s *Store) WithAuthorizationEvents(ctx context.Context, start func() error) error {
	s.authorizationEvents.Lock()
	defer s.authorizationEvents.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return start()
}

func (s *WorkspaceEventStore) WithAuthorizationEvents(ctx context.Context, start func() error) error {
	return s.store.WithAuthorizationEvents(ctx, start)
}
