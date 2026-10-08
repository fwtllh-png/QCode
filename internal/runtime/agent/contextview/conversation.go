package contextview

import (
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"sort"
)

// ConversationOmissions explains sources outside the current dependency set.
// The directory contains identities only and is never injected as source text.
func ConversationOmissions(state *agentcontext.ConversationState, selected []agentcontext.ReferenceCoverage, recoveryOnly bool) []agentcontext.ReferenceCoverage {
	if state == nil {
		return nil
	}
	covered := map[string]bool{}
	for _, reference := range selected {
		covered[reference.SourceID] = true
	}
	var ids []string
	for id := range state.Sources {
		if !covered[id] {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	var rows []agentcontext.ReferenceCoverage
	for _, id := range ids {
		source := state.Sources[id]
		reason := "not_current_selection"
		if state.Superseded(id) {
			reason = "source_superseded"
		} else if recoveryOnly {
			reason = "awaiting_reference_binding"
		}
		rows = append(rows, agentcontext.ReferenceCoverage{SourceID: id, SourceTurn: source.Turn,
			ContentDigest: source.ContentDigest, Representation: "extractive", Status: "omitted", Reason: reason})
	}
	return rows
}

// SelectConversation closes source dependencies independently of the ordinary
// history ceiling. Final request admission still bounds every byte selected.
func SelectConversation(state *agentcontext.ConversationState, plan agentcontext.Plan, history []provider.Message) []agentcontext.ConversationExcerpt {
	var selected []agentcontext.ConversationExcerpt
	for _, source := range state.CandidateSources(plan) {
		items := state.SelectedItems(source, plan)
		coverage := agentcontext.ReferenceCoverage{SourceID: source.ID, SourceTurn: source.Turn, ContentDigest: source.ContentDigest, Representation: "extractive"}
		for _, item := range items {
			coverage.ItemIDs = append(coverage.ItemIDs, item.ID)
		}
		coverage.Ranges = agentcontext.ConversationDependencyRanges(source, coverage.ItemIDs)
		if state.Selection == nil || containsGroup(state.Selection.GroupIDs, source.ID) {
			coverage.Ranges = []agentcontext.ReferenceRange{{Start: 0, End: len(source.Text)}}
		}
		for _, message := range history {
			if message.Turn == source.Turn && message.Role == provider.RoleAssistant && message.Text() == source.Text {
				coverage.Representation = "raw_history"
				break
			}
		}
		selected = append(selected, agentcontext.ConversationExcerpt{Source: source, Coverage: coverage})
	}
	return selected
}

func containsGroup(ids []string, id string) bool {
	for _, candidate := range ids {
		if candidate == id {
			return true
		}
	}
	return false
}
