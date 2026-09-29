package app

import (
	"sort"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

// PendingApproval returns the authoritative identity for one unresolved
// approval. Hosts use it to route a decision to a child thread without
// weakening Session ownership checks.
func (r *EventService) PendingApproval(requestID string) (PendingApproval, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	pending, ok := r.approvals[requestID]
	return pending, ok
}

// PendingInput returns the authoritative identity for one unresolved input.
func (r *EventService) PendingInput(requestID string) (PendingInput, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	pending, ok := r.inputs[requestID]
	return pending, ok
}

func (r *EventService) terminalKind(turnID protocol.TurnID) (protocol.EventKind, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	kind, ok := r.terminals[turnID]
	return kind, ok
}

func (r *EventService) pendingTotals() (approvals, inputs int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.approvals), len(r.inputs)
}

// pendingCounts counts unresolved approvals and inputs owned by threadIDs.
func (r *EventService) pendingCounts(threadIDs []protocol.ThreadID) (approvals, inputs int) {
	threads := threadSet(threadIDs)
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, approval := range r.approvals {
		if _, ok := threads[approval.ThreadID]; ok {
			approvals++
		}
	}
	for _, input := range r.inputs {
		if _, ok := threads[input.ThreadID]; ok {
			inputs++
		}
	}
	return approvals, inputs
}

// forgetThreadInteractions drops unresolved approvals and inputs of deleted
// Threads; no terminal event will ever clear them.
func (r *EventService) forgetThreadInteractions(threadIDs []protocol.ThreadID) {
	threads := threadSet(threadIDs)
	r.mu.Lock()
	defer r.mu.Unlock()
	for requestID, approval := range r.approvals {
		if _, ok := threads[approval.ThreadID]; ok {
			delete(r.approvals, requestID)
			delete(r.approvalItems, eventItemOwner(approval.TurnID, requestID))
		}
	}
	for requestID, input := range r.inputs {
		if _, ok := threads[input.ThreadID]; ok {
			delete(r.inputs, requestID)
			delete(r.inputItems, eventItemOwner(input.TurnID, requestID))
		}
	}
}

// pendingInteractions returns unresolved approvals and inputs ordered by
// request ID for deterministic engine restoration.
func (r *EventService) pendingInteractions() ([]PendingApproval, []PendingInput) {
	r.mu.Lock()
	approvals := make([]PendingApproval, 0, len(r.approvals))
	for _, approval := range r.approvals {
		approvals = append(approvals, approval)
	}
	inputs := make([]PendingInput, 0, len(r.inputs))
	for _, input := range r.inputs {
		inputs = append(inputs, input)
	}
	r.mu.Unlock()
	sort.Slice(approvals, func(i, j int) bool {
		return approvals[i].RequestID < approvals[j].RequestID
	})
	sort.Slice(inputs, func(i, j int) bool {
		return inputs[i].RequestID < inputs[j].RequestID
	})
	return approvals, inputs
}

// restore reinstalls event indexes from a predecessor Runtime before
// activation.
func (r *EventService) restore(recovery RecoveryState) {
	r.runtime.hub.Restore(recovery.LastSequence)
	r.mu.Lock()
	defer r.mu.Unlock()
	for turnID, kind := range recovery.Terminals {
		r.terminals[turnID] = kind
	}
	for requestID, approval := range recovery.PendingApprovals {
		r.approvals[requestID] = approval
		if approval.ItemID != "" {
			r.approvalItems[eventItemOwner(approval.TurnID, requestID)] = approval.ItemID
		}
	}
	for requestID, input := range recovery.PendingInputs {
		r.inputs[requestID] = input
		if input.ItemID != "" {
			r.inputItems[eventItemOwner(input.TurnID, requestID)] = input.ItemID
		}
	}
	for owner, itemID := range recovery.ToolItems {
		if owner.TurnID != "" && owner.LocalID != "" && itemID != "" {
			r.toolItems[owner] = itemID
		}
	}
}

// recoveryState copies the event-derived part of RecoveryState. publishMu
// fences in-flight publications so LastSequence, the indexes, and the queue
// projection describe the same cut.
func (r *EventService) recoveryState() RecoveryState {
	r.publishMu.Lock()
	defer r.publishMu.Unlock()
	result := RecoveryState{
		LastSequence:       r.runtime.hub.Snapshot().LastSequence,
		PendingQueuedTurns: r.runtime.TurnQueueService.snapshotMap(),
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	result.Terminals = make(map[protocol.TurnID]protocol.EventKind, len(r.terminals))
	for turnID, kind := range r.terminals {
		result.Terminals[turnID] = kind
	}
	result.PendingApprovals = make(map[string]PendingApproval, len(r.approvals))
	for requestID, approval := range r.approvals {
		result.PendingApprovals[requestID] = approval
	}
	result.PendingInputs = make(map[string]PendingInput, len(r.inputs))
	for requestID, input := range r.inputs {
		result.PendingInputs[requestID] = input
	}
	return result
}

func threadSet(threadIDs []protocol.ThreadID) map[protocol.ThreadID]struct{} {
	threads := make(map[protocol.ThreadID]struct{}, len(threadIDs))
	for _, threadID := range threadIDs {
		threads[threadID] = struct{}{}
	}
	return threads
}
