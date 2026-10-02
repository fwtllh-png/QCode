package contextview

import (
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
)

// VisibleTailStart is the projector start after applying one persisted fold.
func VisibleTailStart(history []provider.Message, turns, foldStart int) int {
	start := agentcontext.SafeTailStart(history, turns)
	if foldStart <= start || foldStart > len(history) {
		return start
	}
	if foldStart < len(history) && !agentcontext.SafeToolBoundary(history, foldStart) {
		return start
	}
	return foldStart
}

// OldestVisibleTailFold returns the next safe start after dropping the oldest
// closed turn in the current visible tail. Intra-turn message cuts are ignored
// so a fold cannot hide the current user request. ok is false when the visible
// tail is already a single turn (or empty).
func OldestVisibleTailFold(
	history []provider.Message,
	turns, foldStart int,
	allowCurrentTurn bool,
) (int, bool) {
	_ = allowCurrentTurn
	base := VisibleTailStart(history, turns, foldStart)
	for _, cut := range agentcontext.HistoryCuts(history, false) {
		if cut <= base {
			continue
		}
		if foldHidesCurrentUser(history, cut) {
			continue
		}
		return cut, true
	}
	return base, false
}

func foldHidesCurrentUser(history []provider.Message, cut int) bool {
	current := agentcontext.LastNonWorldTurn(history)
	if current == 0 {
		return false
	}
	for _, message := range history[cut:] {
		if agentcontext.IsWorldStateMessage(message) {
			continue
		}
		if message.Turn == current && message.Role == provider.RoleUser {
			return false
		}
	}
	return true
}

// RawTailMessages is the model-visible raw history after start, excluding
// world partitions that stay mandatory regardless of the tail bound.
func RawTailMessages(history []provider.Message, start int) []provider.Message {
	if start < 0 {
		start = 0
	}
	var tail []provider.Message
	for index, message := range history {
		if index >= start && !agentcontext.IsWorldStateMessage(message) {
			tail = append(tail, message)
		}
	}
	return tail
}

// FillVisibleTailStart walks newest closed groups already inside the turn
// bound and drops the oldest ones until estimate(raw tail) fits maxTokens.
// The current user request is never hidden. limited=false leaves start
// unchanged so a missing capacity or operator ceiling is not invented.
func FillVisibleTailStart(
	history []provider.Message,
	turns, start int,
	maxTokens uint64,
	limited bool,
	estimate func([]provider.Message) uint64,
) int {
	if !limited || estimate == nil {
		return start
	}
	for {
		if estimate(RawTailMessages(history, start)) <= maxTokens {
			return start
		}
		next, ok := OldestVisibleTailFold(history, turns, start, false)
		if !ok || next <= start {
			return start
		}
		start = next
	}
}

// ProjectContextView returns the model-visible raw tail. Durable history is
// not modified.
func ProjectContextView(
	history []provider.Message,
	turns int,
) []provider.Message {
	return ProjectContextViewFrom(history, agentcontext.SafeTailStart(history, turns))
}

func ProjectContextViewFrom(
	history []provider.Message,
	start int,
) []provider.Message {
	if start > len(history) {
		return nil
	}
	start = max(0, start)
	current := agentcontext.LastNonWorldTurn(history)
	latest := make(map[string]int)
	for index, message := range history {
		if entry, _, ok := agentcontext.InspectWorldMessage(message); ok {
			latest[entry.ID] = index
		}
	}
	kept := make([]provider.Message, 0, len(history)-start+4)
	for index, message := range history {
		if entry, _, world := agentcontext.InspectWorldMessage(message); world {
			// Rebuild the historical baseline only at the next Turn boundary.
			// Current-Turn patches remain append-only, including tombstones.
			if current != 0 && message.Turn == current ||
				latest[entry.ID] == index && entry.Present {
				kept = append(kept, agentcontext.CloneMessage(message))
			}
		} else if index >= start {
			kept = append(kept, agentcontext.CloneMessage(message))
		}
	}
	return kept
}
