package app

import (
	"context"
	"errors"

	"github.com/fwtllh-png/QCode/internal/runtime/app/eventhub"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

// ObserveEvents registers an observer between projection and external fanout.
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

func (r *EventService) observeEvent(event protocol.Event) {
	r.observerMu.Lock()
	observers := make([]func(protocol.Event), 0, len(r.observers))
	for _, observer := range r.observers {
		observers = append(observers, observer)
	}
	r.observerMu.Unlock()
	for _, observer := range observers {
		observer(event)
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
	r.EventService.mu.Lock()
	defer r.EventService.mu.Unlock()
	itemID = r.eventOwnedItemID(turnID, data, itemID)
	if plan, ok := data.(*protocol.PlanDeltaData); ok && plan.Done {
		if err := r.ArtifactService.DecoratePlanArtifact(
			context.Background(),
			threadID,
			turnID,
			plan,
		); err != nil {
			r.ArtifactService.LogArtifactError(
				"decorate Session Plan Artifact",
				protocol.Event{ThreadID: threadID, TurnID: turnID},
				err,
			)
		}
	}
	kind := eventhub.EventKind(data)
	if protocol.IsTerminalEvent(kind) {
		if _, exists := r.terminals[turnID]; exists {
			return nil
		}
		r.terminals[turnID] = kind
	}
	meta := protocol.EventMeta{
		OperationID: operationID, ThreadID: threadID,
		TurnID: turnID, ItemID: itemID,
	}
	project := func(event protocol.Event) error {
		var projectionErr error
		if r.lifecycle != nil {
			projectionErr = r.lifecycle.Project(context.Background(), event)
		}
		if projectionErr == nil {
			projectionErr = r.TurnQueueService.Apply(event)
		}
		if projectionErr == nil && !protocol.IsTerminalEvent(kind) {
			r.ArtifactService.PersistSessionArtifact(context.Background(), event)
		}
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
		if protocol.IsTerminalEvent(kind) {
			r.clearPendingTurn(turnID)
		}
		return projectionErr
	}
	var err error
	if eventID == "" {
		err = r.hub.Publish(meta, data, project)
	} else {
		err = r.hub.PublishStable(meta, eventID, data, project)
	}
	if err != nil {
		if protocol.IsTerminalEvent(kind) {
			delete(r.terminals, turnID)
		}
		return err
	}
	return nil
}

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
