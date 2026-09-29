package childrun

import (
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

// TrackedTurn is a read-only view of a child turn the Runner is settling.
type TrackedTurn struct {
	TurnID protocol.TurnID
	// Started closes when the turn's turn.started event is observed.
	Started <-chan struct{}
	// Terminal closes once settlement finished or the Runner stopped retrying.
	Terminal <-chan struct{}
}

func (c *Runner) Tracked(threadID protocol.ThreadID) (TrackedTurn, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	turn := c.turns[threadID]
	if turn == nil {
		return TrackedTurn{}, false
	}
	return TrackedTurn{
		TurnID: turn.turnID, Started: turn.startedSignal, Terminal: turn.terminalSignal,
	}, true
}

// Outstanding counts tracked turns and settlements still awaiting a retry.
func (c *Runner) Outstanding() (turns, settlementFailures int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.turns), len(c.settlementErrors)
}

// Root is the host workspace children read and lease worktrees from.
func (c *Runner) Root() string { return c.root }
