package contextview

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
)

func TestSelectionOmissionsMatchActualDifference(t *testing.T) {
	history := []provider.Message{
		textTurn(provider.RoleUser, "one", 1),
		textTurn(provider.RoleAssistant, "first", 1),
		textTurn(provider.RoleUser, "two", 3),
		textTurn(provider.RoleAssistant, "second", 3),
		textTurn(provider.RoleUser, "current", 5),
	}
	before := agentcontext.CloneMessages(history)
	estimate := func(messages []provider.Message) uint64 { return uint64(len(messages)) }
	for _, scenario := range []struct {
		name    string
		policy  SelectionPolicy
		start   int
		reasons []agentcontext.OmissionReason
	}{
		{name: "capacity_available", policy: SelectionPolicy{}, start: 0},
		{name: "turns", policy: SelectionPolicy{RecentTurns: 2}, start: 2,
			reasons: []agentcontext.OmissionReason{agentcontext.OmittedTurnLimit, agentcontext.OmittedTurnLimit}},
		{name: "tokens", policy: SelectionPolicy{MaxTokens: 1, Limited: true, TokenLimitReason: agentcontext.OmittedTokenLimit}, start: 4,
			reasons: []agentcontext.OmissionReason{agentcontext.OmittedTokenLimit, agentcontext.OmittedTokenLimit, agentcontext.OmittedTokenLimit, agentcontext.OmittedTokenLimit}},
		{name: "both", policy: SelectionPolicy{RecentTurns: 2, MaxTokens: 1, Limited: true, TokenLimitReason: agentcontext.OmittedCapacity}, start: 4,
			reasons: []agentcontext.OmissionReason{agentcontext.OmittedTurnLimit, agentcontext.OmittedTurnLimit, agentcontext.OmittedCapacity, agentcontext.OmittedCapacity}},
		{name: "provider_fold", policy: SelectionPolicy{FoldStart: 2, Folds: []HistoryFold{{End: 2, Reason: agentcontext.OmittedProviderOverflow}}}, start: 2,
			reasons: []agentcontext.OmissionReason{agentcontext.OmittedProviderOverflow, agentcontext.OmittedProviderOverflow}},
		{name: "successive_causes", policy: SelectionPolicy{FoldStart: 4, Folds: []HistoryFold{
			{End: 2, Reason: agentcontext.OmittedCapacity}, {Start: 2, End: 4, Reason: agentcontext.OmittedThroughput},
		}}, start: 4,
			reasons: []agentcontext.OmissionReason{agentcontext.OmittedCapacity, agentcontext.OmittedCapacity, agentcontext.OmittedThroughput, agentcontext.OmittedThroughput}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			scenario.policy.Estimate = estimate
			result := SelectHistory(history, scenario.policy)
			if result.TailStart != scenario.start || !reflect.DeepEqual(result.Messages, history[scenario.start:]) ||
				len(result.Omissions) != scenario.start || len(result.Selected) != len(history)-scenario.start {
				t.Fatalf("selection differs from actual input: %+v", result)
			}
			seen := make(map[int]bool)
			for i, omission := range result.Omissions {
				if omission.Source.Index != i || omission.Source.Digest != provider.MessageContentDigest(history[i]) ||
					omission.Reason != scenario.reasons[i] || omission.Retrieval == nil || omission.Retrieval.Turn != history[i].Turn {
					t.Fatalf("incorrect omission: %+v", omission)
				}
				seen[omission.Source.Index] = true
			}
			for _, selected := range result.Selected {
				if seen[selected.Index] || selected.Digest != provider.MessageContentDigest(history[selected.Index]) {
					t.Fatalf("duplicate or incorrect selected source: %+v", selected)
				}
				seen[selected.Index] = true
			}
			if len(seen) != len(history) || result.SourceHistoryDigest != agentcontext.HistoryDigest(history) {
				t.Fatal("selection and omissions do not cover the source")
			}
			again := SelectHistory(history, scenario.policy)
			if result.Digest != again.Digest {
				t.Fatal("identical selection changed digest")
			}
			encoded, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			var restored agentcontext.ProjectionResult
			if err := json.Unmarshal(encoded, &restored); err != nil {
				t.Fatal(err)
			}
			restored.Seal()
			if result.Digest != restored.Digest {
				t.Fatal("selection metadata did not round trip")
			}
			result.Messages[0].Blocks[0].Text = "changed"
			if !reflect.DeepEqual(history, before) {
				t.Fatal("selection mutated durable history")
			}
		})
	}
}

func TestSelectionCapacityGrowthPreservesSourcesAndToolPairs(t *testing.T) {
	history := []provider.Message{
		textTurn(provider.RoleUser, "old", 1),
		{Role: provider.RoleAssistant, Turn: 2, Blocks: []provider.ContentBlock{{
			Type: provider.ContentToolCall, ToolCall: &provider.ToolCall{ID: "read", Name: "file_read", Arguments: "{}"},
		}}},
		{Role: provider.RoleTool, Turn: 3, Blocks: []provider.ContentBlock{{
			Type: provider.ContentToolResult, ToolResult: &provider.ToolResult{CallID: "read", Content: "body"},
		}}},
		textTurn(provider.RoleUser, "current", 4),
	}
	previous := len(history)
	for budget := uint64(0); budget <= uint64(len(history)); budget++ {
		result := SelectHistory(history, SelectionPolicy{MaxTokens: budget, Limited: true,
			Estimate: func(messages []provider.Message) uint64 { return uint64(len(messages)) },
		})
		if result.TailStart > previous || !agentcontext.ToolPairsClosed(result.Messages) ||
			result.Messages[len(result.Messages)-1].Text() != "current" {
			t.Fatalf("budget=%d broke monotonicity, pairing or current request: %+v", budget, result)
		}
		previous = result.TailStart
	}
}
