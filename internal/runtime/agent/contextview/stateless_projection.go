package contextview

import (
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
)

type StatelessProjector struct {
	incremental bool
}

func NewStatelessProjector(incremental bool) *StatelessProjector {
	return &StatelessProjector{incremental: incremental}
}

func (p *StatelessProjector) Project(messages []provider.Message) []provider.Message {
	if p.incremental {
		return messages
	}
	return ProjectStatelessHistory(messages)
}

// ProjectStatelessHistory clones complete-history provider input. Closed-round
// assistant text stays visible so later samples still see decisions that are
// not in the tool result. Window pressure, not this projector, may later fold
// that text into a sourced non-authoritative summary. Reasoning stays attached
// to tool calls because some OpenAI-compatible thinking APIs require it.
func ProjectStatelessHistory(
	messages []provider.Message,
) []provider.Message {
	projected := make([]provider.Message, 0, len(messages))
	for _, source := range messages {
		message := agentcontext.CloneMessage(source)
		if len(message.Blocks) != 0 {
			projected = append(projected, message)
		}
	}
	return projected
}
