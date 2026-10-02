package app

import (
	"context"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func (r *ArtifactService) replayArtifactTurn(
	ctx context.Context,
	turnID protocol.TurnID,
) ([]protocol.Event, error) {
	if store, ok := r.runtime.events.(IndexedEventReplay); ok {
		return store.ReplayTurn(ctx, turnID)
	}
	if turnID == "" {
		return nil, nil
	}
	events, err := r.runtime.events.Replay(ctx, 0)
	if err != nil {
		return nil, err
	}
	return filterReplayEvents(events, func(event protocol.Event) bool {
		return event.TurnID == turnID
	}), nil
}

func (r *ArtifactService) replayArtifactKind(
	ctx context.Context,
	kind protocol.EventKind,
) ([]protocol.Event, error) {
	if store, ok := r.runtime.events.(IndexedEventReplay); ok {
		return store.ReplayKind(ctx, kind)
	}
	if kind == "" {
		return nil, nil
	}
	events, err := r.runtime.events.Replay(ctx, 0)
	if err != nil {
		return nil, err
	}
	return filterReplayEvents(events, func(event protocol.Event) bool {
		return event.Kind == kind
	}), nil
}

func filterReplayEvents(
	events []protocol.Event,
	keep func(protocol.Event) bool,
) []protocol.Event {
	result := make([]protocol.Event, 0)
	for _, event := range events {
		if keep(event) {
			result = append(result, event)
		}
	}
	return result
}

func (r *ArtifactService) beginContextMutation() (func(), error) {
	unlock := r.runtime.SessionService.lockMutations()
	if r.runtime.OperationService.hasWorkspaceOperation() {
		unlock()
		return nil, retryableProblem(protocol.CodeConflict, "Workspace context is being changed")
	}
	return unlock, nil
}

func (r *ArtifactService) LogArtifactError(
	action string,
	event protocol.Event,
	err error,
) {
	if err == nil {
		return
	}
	r.runtime.metrics.Error()
	if r.runtime.logger == nil {
		return
	}
	r.runtime.logger.Error(
		action,
		"thread_id", event.ThreadID,
		"turn_id", event.TurnID,
		"sequence", event.Sequence,
		"error", err,
	)
}
