package agentcontext

import "github.com/fwtllh-png/QCode/internal/adapter/provider"

// DefaultRecentTailTurns selects by capacity. Positive values are explicit
// operator ceilings; defaults are applied by configuration, not here.
const DefaultRecentTailTurns = 0

func ResolveRecentTailTurns(turns int) int {
	return max(0, turns)
}

func SafeTailStart(history []provider.Message, turns int) int {
	if turns == 0 {
		return 0
	}
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
