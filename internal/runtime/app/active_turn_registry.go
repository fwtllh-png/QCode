package app

import (
	"context"
	"errors"
	"sync"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type ActiveTurnLease struct {
	ID       protocol.OperationID
	token    uint64
	threadID protocol.ThreadID
	turnID   protocol.TurnID
}
type ActiveTurnHandle struct {
	ThreadID    protocol.ThreadID
	TurnID      protocol.TurnID
	OperationID protocol.OperationID
	ItemID      protocol.ItemID
	cancel      context.CancelFunc
}
type ActiveTurnSnapshot struct{ Turns int }
type ActiveTurnRegistry struct {
	mu                 sync.Mutex
	next               uint64
	byTurn             map[protocol.TurnID]activeTurnEntry
	byThread           map[protocol.ThreadID]protocol.TurnID
	profiles           map[protocol.ThreadID]uint64
	workspaceExclusive bool
}
type activeTurnEntry struct {
	lease  ActiveTurnLease
	handle ActiveTurnHandle
}

func NewActiveTurnRegistry() *ActiveTurnRegistry {
	return &ActiveTurnRegistry{
		byTurn: make(map[protocol.TurnID]activeTurnEntry), byThread: make(map[protocol.ThreadID]protocol.TurnID),
		profiles: make(map[protocol.ThreadID]uint64)}
}
func (r *ActiveTurnRegistry) Reserve(threadID protocol.ThreadID, turnID protocol.TurnID, operationID protocol.OperationID, itemID protocol.ItemID) (ActiveTurnLease, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.workspaceExclusive {
		return ActiveTurnLease{}, retryableProblem(protocol.CodeConflict, "a Workspace Git operation is active")
	}
	if _, exists := r.byTurn[turnID]; exists {
		return ActiveTurnLease{}, errors.New("turn is already active")
	}
	if _, exists := r.byThread[threadID]; exists {
		return ActiveTurnLease{}, errors.New("thread already has an active turn")
	}
	r.next++
	lease := ActiveTurnLease{ID: operationID, token: r.next, threadID: threadID, turnID: turnID}
	r.byTurn[turnID] = activeTurnEntry{
		lease: lease,
		handle: ActiveTurnHandle{
			ThreadID: threadID, TurnID: turnID,
			OperationID: operationID, ItemID: itemID,
		},
	}
	r.byThread[threadID] = turnID
	return lease, nil
}

func (r *ActiveTurnRegistry) acquireWorkspace() (func(), error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.workspaceExclusive || len(r.byTurn) != 0 {
		return nil, retryableProblem(protocol.CodeConflict, "finish active work before changing Git state")
	}
	r.workspaceExclusive = true
	return func() {
		r.mu.Lock()
		r.workspaceExclusive = false
		r.mu.Unlock()
	}, nil
}
func (r *ActiveTurnRegistry) BindControl(turnID protocol.TurnID, cancel context.CancelFunc) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.byTurn[turnID]
	if !ok {
		return errors.New("turn is not active")
	}
	entry.handle.cancel = cancel
	r.byTurn[turnID] = entry
	return nil
}
func (r *ActiveTurnRegistry) LookupTurn(turnID protocol.TurnID) (ActiveTurnHandle, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.byTurn[turnID]
	return entry.handle, ok
}
func (r *ActiveTurnRegistry) LookupThread(threadID protocol.ThreadID) (ActiveTurnHandle, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	turnID, ok := r.byThread[threadID]
	if !ok {
		return ActiveTurnHandle{}, false
	}
	entry, ok := r.byTurn[turnID]
	return entry.handle, ok
}
func (r *ActiveTurnRegistry) RecordCancel(turnID protocol.TurnID, operationID protocol.OperationID, itemID protocol.ItemID) (context.CancelFunc, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.byTurn[turnID]
	if !ok || entry.handle.cancel == nil {
		return nil, errors.New("turn is not active")
	}
	entry.handle.OperationID = operationID
	entry.handle.ItemID = itemID
	r.byTurn[turnID] = entry
	return entry.handle.cancel, nil
}
func (r *ActiveTurnRegistry) Release(lease ActiveTurnLease) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.byTurn[lease.turnID]
	if !ok || entry.lease.token != lease.token {
		return errors.New("active turn lease is stale")
	}
	delete(r.byTurn, lease.turnID)
	delete(r.byThread, lease.threadID)
	return nil
}
func (r *ActiveTurnRegistry) Snapshot() ActiveTurnSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return ActiveTurnSnapshot{Turns: len(r.byTurn)}
}

func (r *ActiveTurnRegistry) activeThreads(wanted map[protocol.ThreadID]struct{}) []protocol.ThreadID {
	r.mu.Lock()
	defer r.mu.Unlock()
	threads := make([]protocol.ThreadID, 0, min(len(wanted), len(r.byThread)))
	for thread := range wanted {
		if _, ok := r.byThread[thread]; ok {
			threads = append(threads, thread)
		}
	}
	return threads
}

func (r *ActiveTurnRegistry) CancelAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, entry := range r.byTurn {
		if entry.handle.cancel != nil {
			entry.handle.cancel()
		}
	}
}
