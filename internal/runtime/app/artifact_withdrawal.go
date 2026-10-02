package app

import (
	"context"

	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func (r *ArtifactService) requireRetainedSource(ctx context.Context, thread protocol.ThreadID, turn protocol.TurnID) error {
	store, ok := r.runtime.contextRebaseStore.(agentcontext.TurnContextStore)
	if !ok {
		return nil
	}
	withdrawn, err := store.TurnWithdrawn(ctx, thread, turn)
	if err != nil {
		return err
	}
	if withdrawn {
		return runtimeProblem(protocol.CodeConflict, "Source Turn was withdrawn", nil)
	}
	return nil
}
