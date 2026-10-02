package agentcontext

import "github.com/fwtllh-png/QCode/internal/adapter/provider"

// DefaultRecentTailTurns is the public working-set bound for raw history.
// Older turns stay in the durable journal; the model sees only this tail.
const DefaultRecentTailTurns = 2

// RecentToolResultStart is the earliest message that still holds one of the
// keep most recent tool results. keep<=0 means no extra keep.
func RecentToolResultStart(history []provider.Message, keep int) int {
	if keep <= 0 {
		return len(history)
	}
	seen := 0
	for index := len(history) - 1; index >= 0; index-- {
		if !messageHasToolResult(history[index]) {
			continue
		}
		seen++
		if seen >= keep {
			return index
		}
	}
	return 0
}

// UnconsumedToolResultStart is the first tool result after the latest
// user or assistant message. Those results are the current open round.
// A later user or assistant message consumes every earlier result.
func UnconsumedToolResultStart(history []provider.Message) int {
	anchor := -1
	for index, message := range history {
		if IsWorldStateMessage(message) {
			continue
		}
		if message.Role == provider.RoleAssistant ||
			message.Role == provider.RoleUser {
			anchor = index
		}
	}
	for index := anchor + 1; index < len(history); index++ {
		if messageHasToolResult(history[index]) {
			return index
		}
	}
	return len(history)
}

// WorkingSetGCStart is the first message after the write-once prefix.
// keep<=0 keeps only the unconsumed round. A positive keep also retains
// the last keep tool results. The open round is never split. Sample
// projection does not rewrite messages before this point.
func WorkingSetGCStart(history []provider.Message, keep int) int {
	start := UnconsumedToolResultStart(history)
	if keep > 0 {
		if extra := RecentToolResultStart(history, keep); extra < start {
			return extra
		}
	}
	return start
}

func messageHasToolResult(message provider.Message) bool {
	for _, block := range message.Blocks {
		if block.Type == provider.ContentToolResult && block.ToolResult != nil {
			return true
		}
	}
	return false
}

func ResolveRecentTailTurns(turns int) int {
	if turns <= 0 {
		return DefaultRecentTailTurns
	}
	return turns
}

func SafeTailStart(history []provider.Message, turns int) int {
	start := recentTurnStart(history, ResolveRecentTailTurns(turns))
	if start <= 0 {
		return 0
	}
	for start > 0 && !SafeToolBoundary(history, start) {
		start--
	}
	return start
}

// LastNonWorldTurn returns the latest conversation turn, ignoring world-state
// projections and messages that do not belong to a turn.
func LastNonWorldTurn(history []provider.Message) uint64 {
	for index := len(history) - 1; index >= 0; index-- {
		if IsWorldStateMessage(history[index]) {
			continue
		}
		if history[index].Turn != 0 {
			return history[index].Turn
		}
	}
	return 0
}

func IsWorldStateMessage(message provider.Message) bool {
	_, _, ok := InspectWorldMessage(message)
	return ok
}
