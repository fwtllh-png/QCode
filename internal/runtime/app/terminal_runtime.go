package app

import (
	"context"

	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	"github.com/fwtllh-png/QCode/internal/runtime/app/eventhub"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func (r *Runtime) TerminalContent() eventhub.TerminalContentStore {
	return r.content
}

func (r *Runtime) TerminalOperationReceipt(
	operationID protocol.OperationID,
) any {
	return r.OperationService.operationCommitReceipt(operationID)
}

func (r *Runtime) TerminalStore() turnkernel.TerminalEnvelopeStore {
	return r.terminalStore
}

func (r *Runtime) DurableTerminal() bool { return r.lifecycle != nil }

func (r *Runtime) LoadContextManifest(
	threadID protocol.ThreadID,
) (agentcontext.ContextManifest, bool) {
	value, ok := r.contextManifests.Load(threadID)
	if !ok {
		return agentcontext.ContextManifest{}, false
	}
	manifest, ok := value.(agentcontext.ContextManifest)
	return manifest, ok
}

func (r *Runtime) StoreContextManifest(
	threadID protocol.ThreadID,
	manifest agentcontext.ContextManifest,
) {
	r.contextManifests.Store(threadID, manifest)
}

func (r *EventService) PublishTerminalProjection(
	ctx context.Context,
	entry turnkernel.ProjectionOutboxEntry,
	data protocol.EventData,
) error {
	err := r.publishTerminalProjection(ctx, entry, data)
	r.dispatchObservers()
	return err
}

func (r *EventService) publishTerminalProjection(
	_ context.Context,
	entry turnkernel.ProjectionOutboxEntry,
	data protocol.EventData,
) error {
	r.publishMu.Lock()
	defer r.publishMu.Unlock()
	return r.runtime.hub.PublishStable(protocol.EventMeta{
		OperationID: entry.OperationID,
		ThreadID:    entry.ThreadID,
		TurnID:      entry.TurnID,
		ItemID:      entry.ItemID,
	}, entry.EventID, data, func(event protocol.Event) error {
		// Stable events are content-addressed by turn, so thread, turn, and
		// kind identify the same logical event across re-projection. Operation
		// and item identity may legitimately drift when the envelope was
		// committed under a later operation than the live emission; the stored
		// event keeps its original attribution either way.
		if event.ThreadID != entry.ThreadID ||
			event.TurnID != entry.TurnID ||
			string(event.Kind) != entry.Kind {
			return runtimeProblem(
				protocol.CodeConflict,
				"terminal outbox event identity conflict",
				nil,
			)
		}
		var projectionErr error
		if r.runtime.lifecycle != nil {
			projectionErr = r.runtime.lifecycle.Project(context.Background(), event)
		}
		if protocol.IsTerminalEvent(event.Kind) {
			r.mu.Lock()
			r.terminals[event.TurnID] = event.Kind
			r.clearPendingTurn(event.TurnID)
			r.mu.Unlock()
		}
		if projectionErr != nil {
			return projectionErr
		}
		return r.runtime.TurnQueueService.Apply(event)
	})
}
