package app

import (
	"context"

	"github.com/fwtllh-png/QCode/internal/runtime/app/eventhub"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func installRuntimeServices(runtime *Runtime, operationBuffer int) {
	runtime.EventService = &EventService{
		runtime:       runtime,
		terminals:     make(map[protocol.TurnID]protocol.EventKind),
		approvals:     make(map[string]PendingApproval),
		inputs:        make(map[string]PendingInput),
		observers:     make(map[uint64]func(protocol.Event)),
		toolItems:     make(map[EventItemOwner]protocol.ItemID),
		approvalItems: make(map[EventItemOwner]protocol.ItemID),
		inputItems:    make(map[EventItemOwner]protocol.ItemID),
	}
	runtime.TurnService = &TurnService{
		runtime: runtime,
		active:  NewActiveTurnRegistry(),
	}
	runtime.TurnQueueService = newTurnQueueService(runtime)
	runtime.SessionService = &SessionService{runtime: runtime}
	runtime.AgentPresetService = &AgentPresetService{runtime: runtime}
	runtime.OperationService = &OperationService{
		runtime:     runtime,
		operations:  make(chan acceptedOperation, operationBuffer),
		accepted:    make(map[protocol.OperationID]PendingOperation),
		withdrawing: make(map[protocol.ThreadID]bool),
		settlements: operationSettlements{wake: make(chan struct{}, 1)},
	}
	if runtime.lifecycle == nil {
		runtime.OperationService.acceptedKeys = make(map[string]protocol.OperationID)
		runtime.OperationService.committed = make(map[protocol.OperationID]PendingOperation)
	}
	runtime.RecoveryService = &RecoveryService{runtime: runtime}
	runtime.HistoryService = newHistoryService(runtime)
	runtime.ArtifactService = &ArtifactService{runtime: runtime}
	runtime.TraceQuery = runtime.opts.Observability.TraceQuery
}

func newEventHub(ctx context.Context, runtime *Runtime) *eventhub.Hub {
	return eventhub.New(eventhub.Config{
		Store: runtime.events, Buffer: runtime.opts.SubscriberBuffer,
		Context: ctx, Closed: ErrClosed, CursorAhead: ErrCursorAhead,
		ReplayOverflow: func(cursor protocol.Cursor, limit int) error {
			return &ReplayLimitError{Requested: cursor, Limit: limit}
		},
		OnPublished: runtime.metrics.EventPublished, OnDropped: runtime.metrics.SubscriberDropped,
		OnEvent: runtime.observeEvent,
	})
}
