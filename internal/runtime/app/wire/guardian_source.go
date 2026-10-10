package wire

import (
	"context"
	"errors"

	persiststate "github.com/fwtllh-png/QCode/internal/persist/state"
	threadstate "github.com/fwtllh-png/QCode/internal/persist/thread"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func guardianSource(store *persiststate.Store) agentcontext.GuardianSource {
	threads := threadstate.NewRepository(store.SQLite().DB())
	return agentcontext.DurableGuardianSource{Store: store, Delegated: func(ctx context.Context, scope agentcontext.AuthorizationScope) (bool, error) {
		return threads.IsAgentThread(ctx, scope.SessionID, protocol.ThreadID(scope.ThreadID))
	}, Parent: func(ctx context.Context, scope agentcontext.AuthorizationScope) (*agentcontext.AuthorizationScope, error) {
		thread, err := threads.Get(ctx, protocol.ThreadID(scope.ThreadID))
		if err != nil {
			return nil, err
		}
		if thread.SessionID != scope.SessionID || thread.Status != threadstate.ThreadOpen {
			return nil, errors.New("Guardian thread authority is no longer current")
		}
		if thread.ParentThreadID == "" {
			return nil, nil
		}
		parent := scope
		parent.ThreadID = string(thread.ParentThreadID)
		parent.Child = false
		return &parent, nil
	}}
}
