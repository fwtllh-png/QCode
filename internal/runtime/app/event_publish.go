package app

import (
	"context"
	"errors"

	"github.com/fwtllh-png/QCode/internal/persist/state/eventlog"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

// ObserveEvents registers an observer for projected events. Observers run in
// event sequence order after the publish locks are released, so an observer
// may publish further events; those are delivered after the current one.
func (r *EventService) ObserveEvents(observer func(protocol.Event)) func() {
	if r == nil || observer == nil {
		return func() {}
	}
	r.observerMu.Lock()
	r.nextObserver++
	id := r.nextObserver
	r.observers[id] = observer
	r.observerMu.Unlock()
	return func() {
		r.observerMu.Lock()
		delete(r.observers, id)
		r.observerMu.Unlock()
	}
}

// observeEvent runs inside the Hub publish critical section, where calling
// back into the Runtime would self-deadlock. It only queues the event for
// dispatchObservers.
func (r *EventService) observeEvent(event protocol.Event) {
	r.observerMu.Lock()
	r.observerQueue = append(r.observerQueue, event)
	r.observerMu.Unlock()
}

// dispatchObservers must be called without EventService.mu held. One caller
// drains at a time; a publish nested inside an observer only queues, and the
// drain already in progress delivers it.
func (r *EventService) dispatchObservers() {
	r.observerMu.Lock()
	if r.observerDispatching {
		r.observerMu.Unlock()
		return
	}
	r.observerDispatching = true
	r.observerMu.Unlock()
	drained := false
	defer func() {
		if !drained {
			r.observerMu.Lock()
			r.observerDispatching = false
			r.observerMu.Unlock()
		}
	}()
	for {
		r.observerMu.Lock()
		if len(r.observerQueue) == 0 {
			r.observerQueue = nil
			r.observerDispatching = false
			r.observerMu.Unlock()
			drained = true
			return
		}
		event := r.observerQueue[0]
		r.observerQueue[0] = protocol.Event{}
		r.observerQueue = r.observerQueue[1:]
		observers := make([]func(protocol.Event), 0, len(r.observers))
		for _, observer := range r.observers {
			observers = append(observers, observer)
		}
		r.observerMu.Unlock()
		for _, observer := range observers {
			observer(event)
		}
	}
}

func (r *EventService) publish(
	operationID protocol.OperationID,
	threadID protocol.ThreadID,
	turnID protocol.TurnID,
	itemID protocol.ItemID,
	data protocol.EventData,
) error {
	return r.publishWithIdentity(
		operationID,
		threadID,
		turnID,
		itemID,
		"",
		data,
	)
}

func (r *EventService) publishStable(
	operationID protocol.OperationID,
	threadID protocol.ThreadID,
	turnID protocol.TurnID,
	itemID protocol.ItemID,
	eventID protocol.EventID,
	data protocol.EventData,
) error {
	if eventID == "" {
		return errors.New("stable event id is required")
	}
	return r.publishWithIdentity(
		operationID,
		threadID,
		turnID,
		itemID,
		eventID,
		data,
	)
}

func (r *EventService) publishWithIdentity(
	operationID protocol.OperationID,
	threadID protocol.ThreadID,
	turnID protocol.TurnID,
	itemID protocol.ItemID,
	eventID protocol.EventID,
	data protocol.EventData,
) error {
	err := r.publishProjected(operationID, threadID, turnID, itemID, eventID, data)
	r.dispatchObservers()
	return err
}

func (r *EventService) publishProjected(
	operationID protocol.OperationID,
	threadID protocol.ThreadID,
	turnID protocol.TurnID,
	itemID protocol.ItemID,
	eventID protocol.EventID,
	data protocol.EventData,
) error {
	r.publishMu.Lock()
	defer r.publishMu.Unlock()
	if plan, ok := data.(*protocol.PlanDeltaData); ok && plan.Done {
		if err := r.runtime.ArtifactService.DecoratePlanArtifact(
			context.Background(),
			threadID,
			turnID,
			plan,
		); err != nil {
			r.runtime.ArtifactService.LogArtifactError(
				"decorate Session Plan Artifact",
				protocol.Event{ThreadID: threadID, TurnID: turnID},
				err,
			)
		}
	}
	kind := protocol.KindOf(data)
	terminal := protocol.IsTerminalEvent(kind)
	r.mu.Lock()
	itemID = r.eventOwnedItemID(turnID, data, itemID)
	if terminal {
		if _, exists := r.terminals[turnID]; exists {
			r.mu.Unlock()
			return nil
		}
		r.terminals[turnID] = kind
	}
	r.mu.Unlock()
	meta := protocol.EventMeta{
		OperationID: operationID, ThreadID: threadID,
		TurnID: turnID, ItemID: itemID,
	}
	project := func(event protocol.Event) error {
		var projectionErr error
		// Streaming noise never reaches the durable log, so lifecycle has no
		// durable projection to mirror — its usage and item switches key on
		// persisted kinds only. Skipping the call keeps a delta at one
		// reservation transaction instead of adding a threads.updated_at
		// commit per delta; persisted events still refresh updated_at.
		if r.runtime.lifecycle != nil && eventlog.ShouldPersist(event.Kind) {
			projectionErr = r.runtime.lifecycle.Project(context.Background(), event)
		}
		if projectionErr == nil {
			projectionErr = r.runtime.TurnQueueService.Apply(event)
		}
		r.mu.Lock()
		switch value := data.(type) {
		case *protocol.ApprovalRequiredData:
			r.approvals[value.RequestID] = PendingApproval{
				RequestID: value.RequestID, ThreadID: threadID,
				TurnID: turnID, ItemID: itemID, Data: *value,
			}
		case *protocol.ApprovalResolvedData:
			delete(r.approvals, value.RequestID)
			delete(r.approvalItems, eventItemOwner(turnID, value.RequestID))
		case *protocol.InputRequiredData:
			r.inputs[value.RequestID] = PendingInput{
				RequestID: value.RequestID, ThreadID: threadID,
				TurnID: turnID, ItemID: itemID, Data: *value,
			}
		case *protocol.InputResolvedData:
			delete(r.inputs, value.RequestID)
			delete(r.inputItems, eventItemOwner(turnID, value.RequestID))
		}
		if terminal {
			r.clearPendingTurn(turnID)
		}
		r.mu.Unlock()
		if projectionErr == nil && !terminal {
			r.runtime.ArtifactService.PersistSessionArtifact(context.Background(), event)
		}
		return projectionErr
	}
	var err error
	if eventID == "" {
		err = r.runtime.hub.Publish(meta, data, project)
	} else {
		err = r.runtime.hub.PublishStable(meta, eventID, data, project)
	}
	if err != nil {
		if terminal {
			r.mu.Lock()
			delete(r.terminals, turnID)
			r.mu.Unlock()
		}
		return err
	}
	return nil
}

// clearPendingTurn requires EventService.mu.
func (r *EventService) clearPendingTurn(turnID protocol.TurnID) {
	for requestID, approval := range r.approvals {
		if approval.TurnID == turnID {
			delete(r.approvals, requestID)
			delete(r.approvalItems, eventItemOwner(turnID, requestID))
		}
	}
	for requestID, input := range r.inputs {
		if input.TurnID == turnID {
			delete(r.inputs, requestID)
			delete(r.inputItems, eventItemOwner(turnID, requestID))
		}
	}
}

// eventOwnedItemID assigns stable ItemIDs for tool/approval/input events so
// lifecycle can project them as first-class items (F5). Caller must hold the
// EventService mutex.
func (r *EventService) eventOwnedItemID(
	turnID protocol.TurnID,
	data protocol.EventData,
	fallback protocol.ItemID,
) protocol.ItemID {
	switch value := data.(type) {
	case *protocol.ToolResultData:
		if value.CallID == "" {
			return fallback
		}
		owner := eventItemOwner(turnID, value.CallID)
		if id, ok := r.toolItems[owner]; ok {
			return id
		}
		id, err := protocol.NewItemID()
		if err != nil {
			return fallback
		}
		r.toolItems[owner] = id
		return id
	case *protocol.ApprovalRequiredData:
		if value.RequestID == "" {
			return fallback
		}
		owner := eventItemOwner(turnID, value.RequestID)
		if id, ok := r.approvalItems[owner]; ok {
			return id
		}
		id, err := protocol.NewItemID()
		if err != nil {
			return fallback
		}
		r.approvalItems[owner] = id
		return id
	case *protocol.ApprovalResolvedData:
		if id, ok := r.approvalItems[eventItemOwner(turnID, value.RequestID)]; ok {
			return id
		}
		return fallback
	case *protocol.InputRequiredData:
		if value.RequestID == "" {
			return fallback
		}
		owner := eventItemOwner(turnID, value.RequestID)
		if id, ok := r.inputItems[owner]; ok {
			return id
		}
		id, err := protocol.NewItemID()
		if err != nil {
			return fallback
		}
		r.inputItems[owner] = id
		return id
	case *protocol.InputResolvedData:
		if id, ok := r.inputItems[eventItemOwner(turnID, value.RequestID)]; ok {
			return id
		}
		return fallback
	default:
		return fallback
	}
}
