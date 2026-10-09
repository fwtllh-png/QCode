package engine

import (
	"context"
	"fmt"
	"sort"

	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
)

func (e *Engine) availableConversation(ctx context.Context, state *agentcontext.ConversationState, plan agentcontext.Plan) (*agentcontext.ConversationState, map[uint64]bool, []agentcontext.ReferenceCoverage, error) {
	unavailable := make(map[uint64]bool)
	if state == nil {
		return nil, unavailable, nil, nil
	}
	required := make(map[string]bool)
	for _, source := range state.RequiredSources(plan) {
		required[source.ID] = true
	}
	visible := agentcontext.CloneConversation(state)
	var omitted []agentcontext.ReferenceCoverage
	var ids []string
	for id := range state.Sources {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		source := state.Sources[id]
		available, err := e.conversationSourceAvailable(ctx, source)
		if err != nil {
			return nil, nil, nil, err
		}
		if available {
			continue
		}
		if required[id] {
			return nil, nil, nil, fmt.Errorf("required conversation source %s is withdrawn or outside this session", id)
		}
		delete(visible.Sources, id)
		unavailable[source.Turn] = true
		omitted = append(omitted, agentcontext.ReferenceCoverage{SourceID: id, SourceTurn: source.Turn,
			ContentDigest: source.ContentDigest, Representation: "extractive", Status: "omitted", Reason: "source_unavailable"})
	}
	return visible, unavailable, omitted, nil
}
