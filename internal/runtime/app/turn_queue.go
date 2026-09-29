package app

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

// TurnQueueService owns the queued-Turn projection and drain claims. Its mu is
// a leaf lock: nothing else is acquired and no I/O runs while it is held.
type TurnQueueService struct {
	runtime *Runtime
	mu      sync.Mutex
	items   map[string]protocol.QueuedTurn
	claims  map[protocol.OperationID]string
}

func newTurnQueueService(runtime *Runtime) *TurnQueueService {
	return &TurnQueueService{
		runtime: runtime,
		items:   make(map[string]protocol.QueuedTurn),
		claims:  make(map[protocol.OperationID]string),
	}
}

func ApplyTurnQueueEvent(
	items map[string]protocol.QueuedTurn,
	event protocol.Event,
) error {
	if items == nil {
		return errors.New("turn queue projection is required")
	}
	switch data := event.Data.(type) {
	case *protocol.TurnQueuedData:
		if _, exists := items[data.QueueID]; exists {
			return fmt.Errorf("queued turn %s already exists", data.QueueID)
		}
		item := protocol.QueuedTurn{
			QueueID: data.QueueID, ThreadID: event.ThreadID,
			SourceTurnID: event.TurnID, Prompt: data.Prompt,
			DisplayPrompt: data.DisplayPrompt, Intent: data.Intent,
			WorkspaceIdentity: cloneWorkspaceIdentity(data.WorkspaceIdentity),
			Context:           append([]protocol.EditorContextReference(nil), data.Context...),
			AddedSequence:     event.Sequence,
			CreatedAt:         event.CreatedAt,
			UpdatedAt:         event.CreatedAt,
		}
		if err := item.Validate(); err != nil {
			return err
		}
		items[data.QueueID] = item
	case *protocol.QueuedTurnUpdatedData:
		item, exists := items[data.QueueID]
		if !exists {
			return fmt.Errorf("queued turn %s does not exist", data.QueueID)
		}
		item.Prompt = data.Prompt
		item.DisplayPrompt = data.DisplayPrompt
		item.UpdatedAt = event.CreatedAt
		items[data.QueueID] = item
	case *protocol.QueuedTurnRemovedData:
		if _, exists := items[data.QueueID]; !exists {
			return fmt.Errorf("queued turn %s does not exist", data.QueueID)
		}
		delete(items, data.QueueID)
	case *protocol.TurnStartedData:
		if data.QueueID != "" {
			delete(items, data.QueueID)
		}
	case *protocol.TurnSteeredData:
		if data.QueueID != "" {
			delete(items, data.QueueID)
		}
	case *protocol.OperationRejectedData:
		consumeFailedQueuedStart(items, event)
	default:
		if protocol.IsTerminalEvent(event.Kind) {
			consumeFailedQueuedStart(items, event)
		}
	}
	return nil
}

// consumeFailedQueuedStart removes the queue item whose drained StartTurn
// settled before turn.started. Its prompt now belongs to the failed Turn;
// keeping the item would redrain the same idempotent operation forever.
func consumeFailedQueuedStart(
	items map[string]protocol.QueuedTurn,
	event protocol.Event,
) {
	for queueID, item := range items {
		if item.ThreadID != event.ThreadID {
			continue
		}
		if _, operationID := queuedTurnStart(queueID, item.ThreadID); operationID == event.OperationID {
			delete(items, queueID)
			return
		}
	}
}

func cloneWorkspaceIdentity(
	identity *protocol.WorkspaceIdentity,
) *protocol.WorkspaceIdentity {
	if identity == nil {
		return nil
	}
	copy := *identity
	return &copy
}

func (s *TurnQueueService) Apply(event protocol.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ApplyTurnQueueEvent(s.items, event); err != nil {
		return err
	}
	switch data := event.Data.(type) {
	case *protocol.TurnStartedData:
		if data.QueueID != "" {
			delete(s.claims, event.OperationID)
		}
	case *protocol.TurnSteeredData:
		if data.QueueID != "" {
			delete(s.claims, event.OperationID)
		}
	case *protocol.OperationRejectedData:
		delete(s.claims, event.OperationID)
	default:
		if protocol.IsTerminalEvent(event.Kind) {
			delete(s.claims, event.OperationID)
		}
	}
	return nil
}

func (s *TurnQueueService) Restore(
	items map[string]protocol.QueuedTurn,
	pending map[protocol.OperationID]PendingOperation,
) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for queueID, item := range items {
		item.WorkspaceIdentity = cloneWorkspaceIdentity(item.WorkspaceIdentity)
		item.Context = append([]protocol.EditorContextReference(nil), item.Context...)
		s.items[queueID] = item
	}
	for operationID, value := range pending {
		operation, err := decodePendingOperation(value)
		if err != nil {
			continue
		}
		if payload, ok := operation.Payload.(*protocol.StartTurnPayload); ok &&
			payload.QueueID != "" {
			s.claims[operationID] = payload.QueueID
		}
	}
}

func (s *TurnQueueService) List(
	ctx context.Context,
	sessionID string,
) (protocol.TurnQueue, error) {
	if _, err := s.runtime.SessionStatus(ctx, sessionID); err != nil {
		return protocol.TurnQueue{}, err
	}
	threadIDs, err := s.runtime.sessionLifecycle.ThreadIDs(ctx, sessionID)
	if err != nil {
		return protocol.TurnQueue{}, err
	}
	allowed := make(map[protocol.ThreadID]struct{}, len(threadIDs))
	for _, threadID := range threadIDs {
		allowed[threadID] = struct{}{}
	}
	s.mu.Lock()
	items := make([]protocol.QueuedTurn, 0, len(s.items))
	for _, item := range s.items {
		if _, ok := allowed[item.ThreadID]; !ok {
			continue
		}
		item.WorkspaceIdentity = cloneWorkspaceIdentity(item.WorkspaceIdentity)
		item.Context = append([]protocol.EditorContextReference(nil), item.Context...)
		items = append(items, item)
	}
	s.mu.Unlock()
	sort.Slice(items, func(i, j int) bool {
		if items[i].AddedSequence == items[j].AddedSequence {
			return items[i].QueueID < items[j].QueueID
		}
		return items[i].AddedSequence < items[j].AddedSequence
	})
	result := protocol.TurnQueue{Version: protocol.TurnQueueVersion, Items: items}
	return result, result.Validate()
}

func (s *TurnQueueService) item(
	threadID protocol.ThreadID,
	queueID string,
) (protocol.QueuedTurn, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.items[queueID]
	if !ok || item.ThreadID != threadID {
		return protocol.QueuedTurn{}, false
	}
	item.WorkspaceIdentity = cloneWorkspaceIdentity(item.WorkspaceIdentity)
	item.Context = append([]protocol.EditorContextReference(nil), item.Context...)
	return item, true
}

func (s *TurnQueueService) claimed(queueID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, value := range s.claims {
		if value == queueID {
			return true
		}
	}
	return false
}

func (s *TurnQueueService) hasItems() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.items) != 0
}

func (s *TurnQueueService) snapshotMap() map[string]protocol.QueuedTurn {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make(map[string]protocol.QueuedTurn, len(s.items))
	for queueID, item := range s.items {
		item.WorkspaceIdentity = cloneWorkspaceIdentity(item.WorkspaceIdentity)
		item.Context = append([]protocol.EditorContextReference(nil), item.Context...)
		result[queueID] = item
	}
	return result
}

func (s *TurnQueueService) threads() []protocol.ThreadID {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := make(map[protocol.ThreadID]struct{}, len(s.items))
	for _, item := range s.items {
		seen[item.ThreadID] = struct{}{}
	}
	result := make([]protocol.ThreadID, 0, len(seen))
	for threadID := range seen {
		result = append(result, threadID)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

func (s *TurnQueueService) clearThreads(threadIDs []protocol.ThreadID) {
	removed := make(map[protocol.ThreadID]struct{}, len(threadIDs))
	for _, threadID := range threadIDs {
		removed[threadID] = struct{}{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for queueID, item := range s.items {
		if _, ok := removed[item.ThreadID]; ok {
			delete(s.items, queueID)
		}
	}
	for operationID, queueID := range s.claims {
		if item, ok := s.items[queueID]; !ok {
			delete(s.claims, operationID)
		} else if _, remove := removed[item.ThreadID]; remove {
			delete(s.claims, operationID)
		}
	}
}

// claimNext selects the oldest unclaimed item of the Thread and claims it for
// its derived StartTurn in the same critical section, so concurrent drains
// cannot select the same item.
func (s *TurnQueueService) claimNext(threadID protocol.ThreadID) (protocol.QueuedTurn, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	claimed := make(map[string]struct{}, len(s.claims))
	for _, queueID := range s.claims {
		claimed[queueID] = struct{}{}
	}
	var candidate protocol.QueuedTurn
	found := false
	for _, item := range s.items {
		if item.ThreadID != threadID {
			continue
		}
		if _, exists := claimed[item.QueueID]; exists {
			continue
		}
		if !found || item.AddedSequence < candidate.AddedSequence ||
			item.AddedSequence == candidate.AddedSequence &&
				item.QueueID < candidate.QueueID {
			candidate = item
			found = true
		}
	}
	if found {
		candidate.WorkspaceIdentity = cloneWorkspaceIdentity(candidate.WorkspaceIdentity)
		candidate.Context = append([]protocol.EditorContextReference(nil), candidate.Context...)
		_, operationID := queuedTurnStart(candidate.QueueID, threadID)
		s.claims[operationID] = candidate.QueueID
	}
	return candidate, found
}

func (s *TurnQueueService) releaseClaim(operationID protocol.OperationID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.claims, operationID)
}

func (s *TurnQueueService) Drain(threadID protocol.ThreadID) {
	if s.runtime.OperationService.withdrawalInProgress() {
		return
	}
	if _, active := s.runtime.active.LookupThread(threadID); active {
		return
	}
	item, ok := s.claimNext(threadID)
	if !ok {
		return
	}
	key, operationID := queuedTurnStart(item.QueueID, threadID)
	turnID, err := sessionTurnID(key, threadID)
	if err != nil {
		s.releaseClaim(operationID)
		s.logDrainError(item, err)
		return
	}
	itemID, err := sessionItemID(key, protocol.OperationStartTurn, turnID)
	if err != nil {
		s.releaseClaim(operationID)
		s.logDrainError(item, err)
		return
	}
	operation, err := protocol.NewOperation(
		&protocol.StartTurnPayload{
			ThreadID: threadID, TurnID: turnID, ItemID: itemID,
			Prompt: item.Prompt, DisplayPrompt: item.DisplayPrompt,
			Intent: item.Intent, QueueID: item.QueueID,
			WorkspaceIdentity: cloneWorkspaceIdentity(item.WorkspaceIdentity),
			Context:           append([]protocol.EditorContextReference(nil), item.Context...),
		},
	)
	if err != nil {
		s.releaseClaim(operationID)
		s.logDrainError(item, err)
		return
	}
	operation.ID = operationID
	if err := s.runtime.SubmitWithKey(context.Background(), operation, key); err != nil {
		s.releaseClaim(operationID)
		s.logDrainError(item, err)
	}
}

// queuedTurnStart derives the idempotency key and StartTurn operation ID a
// queue item is drained with, so projections can recognize its outcome.
func queuedTurnStart(
	queueID string,
	threadID protocol.ThreadID,
) (string, protocol.OperationID) {
	key := "turn-queue:" + queueID
	return key, protocol.OperationID(sessionDerivedID(
		"op",
		key,
		string(protocol.OperationStartTurn)+":"+string(threadID),
	))
}

func (s *TurnQueueService) logDrainError(item protocol.QueuedTurn, err error) {
	if s.runtime.logger != nil {
		s.runtime.logger.Warn(
			"drain queued turn",
			"queue_id", item.QueueID,
			"thread_id", item.ThreadID,
			"error", err,
		)
	}
}

func normalizeQueuedPrompt(prompt, displayPrompt string) (string, string) {
	prompt = strings.TrimSpace(prompt)
	displayPrompt = strings.TrimSpace(displayPrompt)
	if displayPrompt == "" {
		displayPrompt = prompt
	}
	return prompt, displayPrompt
}

type EnqueueTurnHandler struct{ *Runtime }
type UpdateQueuedTurnHandler struct{ *Runtime }
type RemoveQueuedTurnHandler struct{ *Runtime }
type PromoteQueuedTurnHandler struct{ *Runtime }

func (h EnqueueTurnHandler) Handle(
	_ protocol.Operation,
	payload *protocol.EnqueueTurnPayload,
) OperationOutcome {
	if _, exists := h.TurnQueueService.item(payload.ThreadID, payload.QueueID); exists {
		return finishOutcome(runtimeProblem(
			protocol.CodeConflict,
			"queued turn already exists",
			nil,
		))
	}
	prompt, displayPrompt := normalizeQueuedPrompt(
		payload.Prompt,
		payload.DisplayPrompt,
	)
	return OperationOutcome{
		Kind: OutcomeCommitted,
		Events: []protocol.EventData{&protocol.TurnQueuedData{
			QueueID: payload.QueueID, Prompt: prompt,
			DisplayPrompt: displayPrompt, Intent: payload.Intent,
			WorkspaceIdentity: cloneWorkspaceIdentity(payload.WorkspaceIdentity),
			Context:           append([]protocol.EditorContextReference(nil), payload.Context...),
		}},
		CommitMode: CommitNow,
	}
}

func (h UpdateQueuedTurnHandler) Handle(
	_ protocol.Operation,
	payload *protocol.UpdateQueuedTurnPayload,
) OperationOutcome {
	if _, exists := h.TurnQueueService.item(payload.ThreadID, payload.QueueID); !exists {
		return finishOutcome(runtimeProblem(
			protocol.CodeInvalidArgument,
			"queued turn does not exist",
			nil,
		))
	}
	if h.TurnQueueService.claimed(payload.QueueID) {
		return finishOutcome(retryableProblem(
			protocol.CodeConflict,
			"queued turn is already starting",
		))
	}
	prompt, displayPrompt := normalizeQueuedPrompt(
		payload.Prompt,
		payload.DisplayPrompt,
	)
	return OperationOutcome{
		Kind: OutcomeCommitted,
		Events: []protocol.EventData{&protocol.QueuedTurnUpdatedData{
			QueueID: payload.QueueID,
			Prompt:  prompt, DisplayPrompt: displayPrompt,
		}},
		CommitMode: CommitNow,
	}
}

func (h RemoveQueuedTurnHandler) Handle(
	_ protocol.Operation,
	payload *protocol.RemoveQueuedTurnPayload,
) OperationOutcome {
	if _, exists := h.TurnQueueService.item(payload.ThreadID, payload.QueueID); !exists {
		return finishOutcome(runtimeProblem(
			protocol.CodeInvalidArgument,
			"queued turn does not exist",
			nil,
		))
	}
	if h.TurnQueueService.claimed(payload.QueueID) {
		return finishOutcome(retryableProblem(
			protocol.CodeConflict,
			"queued turn is already starting",
		))
	}
	return OperationOutcome{
		Kind: OutcomeCommitted,
		Events: []protocol.EventData{&protocol.QueuedTurnRemovedData{
			QueueID: payload.QueueID,
			Reason:  "user",
		}},
		CommitMode: CommitNow,
	}
}

func (h PromoteQueuedTurnHandler) Handle(
	operation protocol.Operation,
	payload *protocol.PromoteQueuedTurnPayload,
) OperationOutcome {
	item, exists := h.TurnQueueService.item(payload.ThreadID, payload.QueueID)
	if !exists {
		return finishOutcome(runtimeProblem(
			protocol.CodeInvalidArgument,
			"queued turn does not exist",
			nil,
		))
	}
	if h.TurnQueueService.claimed(payload.QueueID) {
		return finishOutcome(retryableProblem(
			protocol.CodeConflict,
			"queued turn is already starting",
		))
	}
	active, ok := h.active.LookupThread(payload.ThreadID)
	if !ok || active.TurnID != payload.TurnID {
		return finishOutcome(turnNotActiveProblem())
	}
	return SteerTurnHandler{h.Runtime}.Handle(operation, &protocol.SteerTurnPayload{
		ThreadID: payload.ThreadID,
		TurnID:   payload.TurnID,
		ItemID:   payload.ItemID,
		Prompt:   item.Prompt,
		QueueID:  item.QueueID,
	})
}
