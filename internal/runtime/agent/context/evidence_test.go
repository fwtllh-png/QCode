package agentcontext

import (
	"reflect"
	"strings"
	"testing"
)

func TestEvidenceDeltaRoundTrip(t *testing.T) {
	set := NewEvidenceSet()
	set.BeginTurn(3)
	set.Observe(EvidenceFact{Kind: KindDefinition, Path: "a.go", Line: 2, Turn: 1})
	set.MarkChanged("a.go", 2, true)
	set.MarkDiagnostics("a.go", true)
	set.NoteRead("b.go", "digest")
	set.NoteHandle("result-1", "read")
	restored := ApplyEvidenceDelta(set.Delta())
	if !reflect.DeepEqual(restored.Delta(), set.Delta()) {
		t.Fatalf("restored = %+v, want %+v", restored.Delta(), set.Delta())
	}
}

func TestNilSetAcceptsObservationsAndReportsNothing(t *testing.T) {
	var set *EvidenceSet
	set.BeginTurn(1)
	set.Observe(EvidenceFact{Kind: KindDefinition, Path: "a.go"})
	set.MarkChanged("a.go", 1, false)
	set.MarkVerified([]string{"a.go"})
	set.MarkDiagnostics("a.go", true)
	set.NoteCall("search_text", "{}")
	set.NoteRead("a.go", "sha")
	set.NoteHandle("h1", "search_text")
	set.ConsumeHandle("h1")
	if snapshot := set.Snapshot(10); !snapshot.Empty() {
		t.Fatalf("nil set reported %+v", snapshot)
	}
	if paths := set.UnverifiedPaths(); paths != nil {
		t.Fatalf("nil set reported unverified paths %v", paths)
	}
	if changes := set.Changes(); changes != nil {
		t.Fatalf("nil set reported changes %v", changes)
	}
	if set.Clone() != nil {
		t.Fatal("cloning a nil set produced a set")
	}
}

func TestObserveDeduplicatesAndKeepsEarliestTurn(t *testing.T) {
	set := NewEvidenceSet()
	set.BeginTurn(1)
	set.Observe(EvidenceFact{Kind: KindDefinition, Path: "a.go", Line: 12, Turn: 3, Tool: "search_symbol"})
	set.Observe(EvidenceFact{
		Kind: KindDefinition, Path: "a.go", Line: 12, Turn: 1,
		Symbol: "Verify", Tool: "search_definition",
	})
	snapshot := set.Snapshot(0)
	if len(snapshot.Facts) != 1 {
		t.Fatalf("expected one fact, got %+v", snapshot.Facts)
	}
	fact := snapshot.Facts[0]
	if fact.Turn != 1 {
		t.Fatalf("expected the earliest turn, got %d", fact.Turn)
	}
	// The second observation named the symbol the first one did not.
	if fact.Symbol != "Verify" {
		t.Fatalf("expected the symbol to be filled in, got %q", fact.Symbol)
	}
}

func TestObserveRejectsUnknownKindAndEmptyPath(t *testing.T) {
	set := NewEvidenceSet()
	set.Observe(EvidenceFact{Kind: EvidenceKind("guess"), Path: "a.go", Turn: 1})
	set.Observe(EvidenceFact{Kind: KindDefinition, Path: "   ", Turn: 1})
	if snapshot := set.Snapshot(0); len(snapshot.Facts) != 0 {
		t.Fatalf("expected no facts, got %+v", snapshot.Facts)
	}
}

func TestSnapshotOrdersFactsByKindThenPath(t *testing.T) {
	set := NewEvidenceSet()
	set.Observe(EvidenceFact{Kind: KindTextMatch, Path: "z.go", Turn: 1})
	set.Observe(EvidenceFact{Kind: KindReference, Path: "b.go", Turn: 1})
	set.Observe(EvidenceFact{Kind: KindDefinition, Path: "c.go", Turn: 1})
	set.Observe(EvidenceFact{Kind: KindDefinition, Path: "a.go", Turn: 1})
	snapshot := set.Snapshot(0)
	var got []string
	for _, fact := range snapshot.Facts {
		got = append(got, string(fact.Kind)+":"+fact.Path)
	}
	want := "definition:a.go definition:c.go reference:b.go text_match:z.go"
	if strings.Join(got, " ") != want {
		t.Fatalf("facts ordered %q, want %q", strings.Join(got, " "), want)
	}
}

func TestSnapshotLimitKeepsRecentFactsAndReportsTheRest(t *testing.T) {
	set := NewEvidenceSet()
	set.Observe(EvidenceFact{Kind: KindTextMatch, Path: "old.go", Turn: 1})
	set.Observe(EvidenceFact{Kind: KindTextMatch, Path: "new.go", Turn: 4})
	snapshot := set.Snapshot(1)
	if len(snapshot.Facts) != 1 || snapshot.Facts[0].Path != "new.go" {
		t.Fatalf("expected the recent fact, got %+v", snapshot.Facts)
	}
	if snapshot.OmittedFacts != 1 {
		t.Fatalf("expected one omitted fact, got %d", snapshot.OmittedFacts)
	}
}

func TestChangeIsRiskyUntilVerified(t *testing.T) {
	set := NewEvidenceSet()
	set.BeginTurn(1)
	set.MarkChanged("a.go", 1, true)
	risks := set.Snapshot(0).Risks
	if len(risks) != 1 || risks[0].Kind != RiskUnverifiedChange || risks[0].Path != "a.go" {
		t.Fatalf("expected one unverified change, got %+v", risks)
	}
	if paths := set.UnverifiedPaths(); len(paths) != 1 || paths[0] != "a.go" {
		t.Fatalf("expected a.go unverified, got %v", paths)
	}
	set.MarkVerified([]string{"a.go"})
	if risks := set.Snapshot(0).Risks; len(risks) != 0 {
		t.Fatalf("verification left risks %+v", risks)
	}
	if paths := set.UnverifiedPaths(); len(paths) != 0 {
		t.Fatalf("verification left unverified paths %v", paths)
	}
}

func TestWritingAgainInvalidatesAnEarlierVerification(t *testing.T) {
	set := NewEvidenceSet()
	set.MarkChanged("a.go", 1, true)
	set.MarkVerified([]string{"a.go"})
	set.MarkChanged("a.go", 2, true)
	risks := set.Snapshot(0).Risks
	if len(risks) != 1 || risks[0].Kind != RiskUnverifiedChange || risks[0].Turn != 2 {
		t.Fatalf("expected the new write to be unverified, got %+v", risks)
	}
}

func TestChangeWithoutReadAndOpenDiagnosticsAreSeparateRisks(t *testing.T) {
	set := NewEvidenceSet()
	set.MarkChanged("a.go", 1, false)
	set.MarkDiagnostics("a.go", true)
	kinds := map[string]bool{}
	for _, risk := range set.Snapshot(0).Risks {
		kinds[risk.Kind] = true
	}
	for _, want := range []string{RiskUnverifiedChange, RiskBlindChange, RiskOpenDiagnostics} {
		if !kinds[want] {
			t.Fatalf("missing risk %q in %+v", want, set.Snapshot(0).Risks)
		}
	}
	set.MarkDiagnostics("a.go", false)
	for _, risk := range set.Snapshot(0).Risks {
		if risk.Kind == RiskOpenDiagnostics {
			t.Fatal("a clean diagnostics run left the risk standing")
		}
	}
}

// Changes keeps reporting a path after verification clears its risk, because a
// summary has to say what the thread touched, not only what it still owes.
func TestChangesReportEveryMarkAfterRisksClear(t *testing.T) {
	set := NewEvidenceSet()
	set.MarkChanged("blind.go", 1, false)
	set.MarkChanged("checked.go", 2, true)
	set.MarkDiagnostics("blind.go", true)
	set.MarkVerified([]string{"checked.go"})

	changes := set.Changes()
	if len(changes) != 2 {
		t.Fatalf("changes = %+v", changes)
	}
	blind, checked := changes[0], changes[1]
	if blind.Path != "blind.go" || blind.Read || blind.Verified || !blind.Diagnostics || blind.Turn != 1 {
		t.Fatalf("blind change = %+v", blind)
	}
	if checked.Path != "checked.go" || !checked.Read || !checked.Verified || checked.Diagnostics {
		t.Fatalf("checked change = %+v", checked)
	}
	if paths := set.UnverifiedPaths(); len(paths) != 1 || paths[0] != "blind.go" {
		t.Fatalf("unverified = %v", paths)
	}
}

func TestDiagnosticsOnAnUnchangedPathIsNotARisk(t *testing.T) {
	set := NewEvidenceSet()
	set.MarkDiagnostics("untouched.go", true)
	if risks := set.Snapshot(0).Risks; len(risks) != 0 {
		t.Fatalf("expected no risks for a path the turn never wrote, got %+v", risks)
	}
}

func TestRepeatedCallRemindsOnceAndClearsNextTurn(t *testing.T) {
	set := NewEvidenceSet()
	set.BeginTurn(1)
	set.NoteCall("search_text", `{"query":"Verify","regex":false}`)
	if reminders := set.Snapshot(0).Reminders; len(reminders) != 0 {
		t.Fatalf("a single call reminded: %+v", reminders)
	}
	set.NoteCall("search_text", ` { "regex": false, "query": "Verify" } `)
	reminders := set.Snapshot(0).Reminders
	if len(reminders) != 1 || reminders[0].Kind != ReminderRepeatedCall {
		t.Fatalf("expected one repeated-call reminder, got %+v", reminders)
	}
	if !strings.Contains(reminders[0].Detail, "search_text ran 2 times") {
		t.Fatalf("reminder does not name the tool and count: %q", reminders[0].Detail)
	}
	set.BeginTurn(2)
	if reminders := set.Snapshot(0).Reminders; len(reminders) != 0 {
		t.Fatalf("call counts survived the turn: %+v", reminders)
	}
}

func TestDifferentArgumentsAreNotARepeatedCall(t *testing.T) {
	set := NewEvidenceSet()
	set.BeginTurn(1)
	set.NoteCall("search_text", `{"query":"a"}`)
	set.NoteCall("search_text", `{"query":"b"}`)
	if reminders := set.Snapshot(0).Reminders; len(reminders) != 0 {
		t.Fatalf("two different searches reminded: %+v", reminders)
	}
}

func TestRepeatedReadRemindsOnlyWhenContentIsUnchanged(t *testing.T) {
	set := NewEvidenceSet()
	set.BeginTurn(1)
	set.NoteRead("a.go", "sha-1")
	set.NoteRead("a.go", "sha-2")
	if reminders := set.Snapshot(0).Reminders; len(reminders) != 0 {
		t.Fatalf("a changed file reminded: %+v", reminders)
	}
	set.NoteRead("a.go", "sha-2")
	reminders := set.Snapshot(0).Reminders
	if len(reminders) != 1 || reminders[0].Kind != ReminderRepeatedRead {
		t.Fatalf("expected one repeated-read reminder, got %+v", reminders)
	}
	set.BeginTurn(2)
	if reminders := set.Snapshot(0).Reminders; len(reminders) != 0 {
		t.Fatalf("an old re-read is still reported: %+v", reminders)
	}
}

func TestUnconsumedHandleRemindsAfterTheTurnThatIssuedIt(t *testing.T) {
	set := NewEvidenceSet()
	set.BeginTurn(1)
	set.NoteHandle("h1", "search_project")
	if reminders := set.Snapshot(0).Reminders; len(reminders) != 0 {
		t.Fatalf("the issuing turn reminded: %+v", reminders)
	}
	set.BeginTurn(2)
	reminders := set.Snapshot(0).Reminders
	if len(reminders) != 1 || reminders[0].Kind != ReminderUnconsumedResult {
		t.Fatalf("expected one unconsumed-result reminder, got %+v", reminders)
	}
	if !strings.Contains(reminders[0].Detail, "h1") {
		t.Fatalf("reminder does not name the handle: %q", reminders[0].Detail)
	}
	set.ConsumeHandle("h1")
	if reminders := set.Snapshot(0).Reminders; len(reminders) != 0 {
		t.Fatalf("a consumed handle is still reported: %+v", reminders)
	}
}

func TestStaleHandleStopsBeingReported(t *testing.T) {
	set := NewEvidenceSet()
	set.BeginTurn(1)
	set.NoteHandle("h1", "search_project")
	set.BeginTurn(1 + handleStaleTurns + 1)
	if reminders := set.Snapshot(0).Reminders; len(reminders) != 0 {
		t.Fatalf("a stale handle is still reported: %+v", reminders)
	}
}

func TestCloneIsIndependent(t *testing.T) {
	set := NewEvidenceSet()
	set.BeginTurn(2)
	set.Observe(EvidenceFact{Kind: KindDefinition, Path: "a.go", Turn: 1})
	set.MarkChanged("a.go", 2, true)
	clone := set.Clone()
	set.MarkVerified([]string{"a.go"})
	set.Observe(EvidenceFact{Kind: KindReference, Path: "b.go", Turn: 2})

	snapshot := clone.Snapshot(0)
	if len(snapshot.Facts) != 1 {
		t.Fatalf("the clone saw the parent's later fact: %+v", snapshot.Facts)
	}
	if len(snapshot.Risks) != 1 {
		t.Fatalf("the clone saw the parent's later verification: %+v", snapshot.Risks)
	}
	if snapshot.Turn != 2 {
		t.Fatalf("clone turn is %d, want 2", snapshot.Turn)
	}
}

func TestRetainedDeltaKeepsReadsOutsideFactAndChangeSet(t *testing.T) {
	set := NewEvidenceSet()
	set.BeginTurn(1)
	set.Observe(EvidenceFact{Kind: KindDefinition, Path: "a.go", Line: 2, Turn: 1})
	set.NoteRead("orphan.go", "digest")
	delta := set.RetainedDelta(1, 32, 32)
	if len(delta.Facts) != 1 || delta.Facts[0].Path != "a.go" {
		t.Fatalf("facts = %+v", delta.Facts)
	}
	if len(delta.Reads) != 1 || delta.Reads[0].Path != "orphan.go" ||
		delta.Reads[0].Digest != "digest" {
		t.Fatalf("reads = %+v, want the orphan SourceRead", delta.Reads)
	}
}

func TestPassingVerificationClearsRestoredStaleChange(t *testing.T) {
	set := ApplyEvidenceDelta(EvidenceDelta{Changes: []EvidenceChange{{
		Path: "a.go", Turn: 1, Read: true, Stale: true,
	}}})
	set.MarkVerified([]string{"a.go"})
	changes := set.Changes()
	if len(changes) != 1 || !changes[0].Verified || changes[0].Stale {
		t.Fatalf("changes=%+v", changes)
	}
}
