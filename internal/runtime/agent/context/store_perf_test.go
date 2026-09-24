package agentcontext

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
)

func largeLedger(messages int) (*MessageLedger, LedgerProjection) {
	stable := []provider.Message{provider.TextMessage(provider.RoleSystem, "stable prefix")}
	history := make([]provider.Message, 0, messages)
	for index := 0; index < messages; index++ {
		history = append(history, provider.TextMessage(
			provider.RoleUser,
			"question about module alpha "+string(rune('a'+index%26))+
				" number "+string(rune('0'+index%10))+" with surrounding detail",
		))
		history = append(history, provider.TextMessage(
			provider.RoleAssistant,
			"answer "+string(rune('0'+index%10))+" with tool evidence and a short conclusion",
		))
	}
	dynamic := []provider.Message{provider.TextMessage(provider.RoleSystem, "dynamic")}
	projection := LedgerProjection{
		Stable: stable, History: history, Dynamic: dynamic,
		Definitions: []provider.ToolDefinition{
			{Name: "alpha", Description: "a", InputSchema: map[string]any{"type": "object"}},
			{Name: "beta", Description: "b", InputSchema: map[string]any{"type": "object"}},
		},
	}
	ledger := NewMessageLedger(LedgerInput{
		Stable: projection.Stable, History: projection.History,
		Dynamic: projection.Dynamic, Definitions: projection.Definitions,
	})
	return ledger, projection
}

// TestMemoizedSnapshotMatchesRebuiltProjection locks the equivalence the
// memoized fast path relies on: an unchanged projection must return a
// snapshot with the same items, messages and definitions as a fresh rebuild,
// and caller-side mutation of the submitted slices must not leak into a
// later memoized snapshot.
func TestMemoizedSnapshotMatchesRebuiltProjection(t *testing.T) {
	ledger, projection := largeLedger(24)
	first := ledger.Project(projection)
	second := ledger.Project(projection)
	if first.Revision() != second.Revision() {
		t.Fatalf("revisions drifted: %d vs %d", first.Revision(), second.Revision())
	}
	if !reflect.DeepEqual(first.Items(), second.Items()) {
		t.Fatal("memoized snapshot items differ from first projection")
	}
	if !reflect.DeepEqual(first.Messages(), second.Messages()) {
		t.Fatal("memoized snapshot messages differ from first projection")
	}
	if !reflect.DeepEqual(first.Definitions(), second.Definitions()) {
		t.Fatal("memoized snapshot definitions differ from first projection")
	}
	firstDigest, err := first.Digest()
	if err != nil {
		t.Fatal(err)
	}
	secondDigest, err := second.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if firstDigest != secondDigest {
		t.Fatal("memoized snapshot digest differs")
	}

	// Mutating the caller's slices and re-projecting submits new content (the
	// revision advances), but the earlier memoized snapshot — already handed
	// out — must keep its own clones and stay unaffected.
	projection.History[0].Blocks[0].Text = "mutated after projection"
	stale := ledger.Project(projection)
	if stale.Revision() == second.Revision() {
		t.Fatal("post-projection mutation was not detected as a change")
	}
	if second.Messages()[1].Text() == "mutated after projection" {
		t.Fatal("caller mutation leaked into the earlier snapshot")
	}
}

// TestWithHistoryPreservesItemIdentityAndIsolation covers the aliasing fast
// path: retained items keep their identity, and the rewritten snapshot never
// reflects later mutations of values reachable from the source snapshot's
// accessors.
func TestWithHistoryPreservesItemIdentityAndIsolation(t *testing.T) {
	ledger, projection := largeLedger(8)
	source := ledger.Project(projection)
	retained := source.Partition(KindHistory)
	rewritten := source.WithHistory(retained[2:])
	if rewritten.Revision() != source.Revision()+1 {
		t.Fatalf("rewrite revision = %d", rewritten.Revision())
	}
	for index, item := range rewritten.Items() {
		if item.Kind != KindHistory {
			continue
		}
		want := source.Items()[index+2].ID
		if item.ID != want {
			t.Fatalf("retained item %d identity %s, want %s", index, item.ID, want)
		}
	}
	// Mutating a clone handed out by the source must not appear in the
	// rewrite: accessors clone, and the rewrite owns its history copy.
	sourceMessages := source.Messages()
	sourceMessages[1].Blocks[0].Text = "mutated clone"
	if rewritten.Messages()[1].Text() == "mutated clone" {
		t.Fatal("mutation of an accessor clone leaked into the rewrite")
	}
	encoded, err := json.Marshal(rewritten.Messages())
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) == 0 {
		t.Fatal("rewrite messages empty")
	}
}

func BenchmarkProjectUnchanged(b *testing.B) {
	ledger, projection := largeLedger(250)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		snapshot := ledger.Project(projection)
		if snapshot.Revision() == 0 {
			b.Fatal("invalid revision")
		}
	}
}

func BenchmarkProjectAppendOneMessage(b *testing.B) {
	ledger, projection := largeLedger(250)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		grown := append(append([]provider.Message{}, projection.History...),
			provider.TextMessage(provider.RoleUser, "one more"))
		snapshot := ledger.Project(LedgerProjection{
			Stable: projection.Stable, History: grown,
			Dynamic: projection.Dynamic, Definitions: projection.Definitions,
		})
		if snapshot.Revision() == 0 {
			b.Fatal("invalid revision")
		}
	}
}

func BenchmarkWithHistoryTailRewrite(b *testing.B) {
	ledger, projection := largeLedger(250)
	snapshot := ledger.Project(projection)
	history := snapshot.Partition(KindHistory)
	tail := history[len(history)/2:]
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		rewritten := snapshot.WithHistory(tail)
		if rewritten.Revision() <= snapshot.Revision() {
			b.Fatal("rewrite did not advance revision")
		}
	}
}
