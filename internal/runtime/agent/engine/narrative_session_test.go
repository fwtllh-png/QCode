package engine

import (
	"context"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
)

func TestNarrativeSnapshotUsesDurableSessionAndRechecksOwners(t *testing.T) {
	for _, tc := range []struct {
		name, changedTurn, owner string
		withdrawn                bool
		fallback                 string
	}{
		{name: "same-session"},
		{name: "source-moved", changedTurn: "turn-1", owner: "session-other", fallback: "source_turn_unavailable"},
		{name: "source-deleted", changedTurn: "turn-1", fallback: "source_turn_unavailable"},
		{name: "anchor-moved", changedTurn: "turn-3", owner: "session-other", fallback: "source_turn_unavailable"},
		{name: "anchor-deleted", changedTurn: "turn-3", fallback: "source_turn_unavailable"},
		{name: "source-withdrawn", withdrawn: true, fallback: "source_turn_withdrawn_or_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			owners := map[string]string{"turn-1": "session-real", "turn-2": "session-real", "turn-3": "session-real"}
			store := &withdrawalContextStore{}
			p := &narrativeFunctionProvider{summary: func(_ context.Context, request provider.ModelRequest) (provider.Stream, error) {
				// Ownership can change while the optional provider call is in flight.
				if tc.changedTurn != "" {
					if tc.owner == "" {
						delete(owners, tc.changedTurn)
					} else {
						owners[tc.changedTurn] = tc.owner
					}
				}
				store.withdrawn = tc.withdrawn
				return coveredNarrativeStream(narrativeInputOf(t, request)), nil
			}}
			e := newEngine(t, p, tool.NewRegistry(nil, nil))
			e.options.SessionID = "process-session"
			e.options.SessionForTurn = func(_ context.Context, turn string) (string, bool) {
				owner, found := owners[turn]
				return owner, found
			}
			e.options.Context.SemanticNarrative = "post_turn"
			e.options.Context.Digest = "ledger+narrative"
			e.options.TurnContexts = store
			seedOmittedHistory(e)
			e.turnIDs = map[string]uint64{"turn-1": 1, "turn-2": 2, "turn-3": 3}
			prepared := e.PreparePostTurnNarrative("thread-1", "turn-3")
			if prepared == nil || prepared.snapshot.sessionID != "session-real" {
				t.Fatal("background snapshot did not capture its durable session")
			}
			// Background execution need not carry the original invocation context.
			result, err := prepared.Run(tool.WithSessionIdentity(t.Context(), "unrelated-callback-session"))
			if err != nil || result.Fallback != (tc.fallback != "") || result.FailureReason != tc.fallback {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if (e.context.Compaction().Digest != nil) != (tc.fallback == "") {
				t.Fatal("narrative installation ignored source ownership")
			}
		})
	}
}

func TestNarrativeSnapshotRejectsUnavailableSession(t *testing.T) {
	for _, found := range []bool{false, true} {
		e := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
		e.options.SessionID = "process-session"
		e.options.SessionForTurn = func(context.Context, string) (string, bool) { return "", found }
		e.options.Context.SemanticNarrative = "post_turn"
		e.options.Context.Digest = "ledger+narrative"
		seedOmittedHistory(e)
		if prepared := e.PreparePostTurnNarrative("thread-1", "turn-3"); prepared != nil {
			t.Fatal("unavailable durable session fell back to the process seed")
		}
	}
}
