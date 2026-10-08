package engine

import (
	"errors"
	"strings"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
)

// Capture before terminal compaction, inside the same context transaction as
// the completed answer. Failed/canceled turns never publish report sources.
func (e *Engine) captureConversationAnswer(history []provider.Message) error {
	scope := e.runningScope()
	if scope == nil {
		return errors.New("conversation answer requires an active turn")
	}
	var answer string
	for i := len(history) - 1; i >= 0; i-- {
		message := history[i]
		if message.Turn != e.currentTurn() || message.Role != provider.RoleAssistant || agentcontext.IsWorldStateMessage(message) {
			continue
		}
		if text := message.Text(); strings.TrimSpace(text) != "" {
			answer = text
			break
		}
	}
	if answer == "" {
		return nil
	}
	threadID := scope.spec.Identity.ThreadID
	if threadID == "" {
		// Standalone engines use the random identity assigned at engine construction.
		// Restored sources keep their recorded origins; a new fork gets its own
		// origin for new answers. Production hosts supply the durable thread ID.
		threadID = "local-window:" + e.conversationOrigin
	}
	state := e.contextAuthority().Conversation()
	if state == nil {
		state = &agentcontext.ConversationState{}
	}
	if err := state.Add(agentcontext.IndexConversationAnswer(threadID, scope.spec.Identity.TurnID, e.currentTurn(), answer)); err != nil {
		return err
	}
	e.contextAuthority().SetConversation(state)
	return nil
}
