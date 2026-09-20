package eventlog

import (
	"testing"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestShouldPersistTable(t *testing.T) {
	cases := []struct {
		kind protocol.EventKind
		want bool
	}{
		{protocol.EventOutputDelta, false},
		{protocol.EventOutputDraft, false},
		{protocol.EventOutputDiscarded, false},
		{protocol.EventReasoningDelta, false},
		{protocol.EventToolState, false},
		{protocol.EventTurnCompaction, true},
		{protocol.EventTurnStarted, true},
		{protocol.EventTurnCompleted, true},
		{protocol.EventTurnFailed, true},
		{protocol.EventTurnCanceled, true},
		{protocol.EventOperationRejected, true},
		{protocol.EventTurnSteered, true},
		{protocol.EventApprovalRequired, true},
		{protocol.EventApprovalResolved, true},
		{protocol.EventInputRequired, true},
		{protocol.EventInputResolved, true},
		{protocol.EventThreadCompacted, true},
		{protocol.EventThreadForked, true},
		{protocol.EventTurnReverted, true},
		{protocol.EventToolResult, true},
		{protocol.EventUsage, true},
		{protocol.EventDiagnostics, true},
		{protocol.EventSearchResult, true},
		{protocol.EventCitation, true},
		{protocol.EventAgentSpawned, true},
		{protocol.EventAgentStatus, true},
		{protocol.EventAgentMessage, true},
		{protocol.EventKind("future.audit"), true},
	}
	for _, test := range cases {
		if got := ShouldPersist(test.kind); got != test.want {
			t.Fatalf("ShouldPersist(%q) = %v, want %v", test.kind, got, test.want)
		}
	}
}

func TestShouldPersistMatchesEveryDeclaredTrait(t *testing.T) {
	for _, kind := range protocol.EventKinds() {
		traits, ok := protocol.Traits(kind)
		if !ok {
			t.Fatalf("event %q has no traits", kind)
		}
		if got, want := ShouldPersist(kind), traits.Durability.Persisted(); got != want {
			t.Fatalf("ShouldPersist(%q) = %t, traits require %t", kind, got, want)
		}
	}
}
