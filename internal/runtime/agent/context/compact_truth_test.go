package agentcontext

import (
	"strings"
	"testing"
)

func TestTruthCapsuleUsesCurrentOwnerSnapshotAcrossCompactions(t *testing.T) {
	compatibility := Compatibility{
		SchemaVersion: TruthSchemaVersion,
		Adapter:       "deepseek", Provider: "deepseek", Model: "deepseek-chat",
		ContextTokens: 64_000, ToolCalls: true,
		SummaryMaxBytes: 8192, MaxDigestEntries: 120,
		DownshiftPolicy: DownshiftRuntimeTruthOnly,
	}.Hash()
	var previous []TruthCapsule
	for generation, path := range []string{"a.go:10", "b.go:20", "c.go:30"} {
		current := truthFixture(compatibility, "deepseek-chat", 64_000)
		current.Entities = []TruthEntity{
			NewTruthEntity(EntityFact, path, "definition "+path, "runtime.evidence"),
		}
		current.Seal()
		merged, receipt, err := MergeTruthCapsules(current, previous...)
		if err != nil {
			t.Fatal(err)
		}
		if receipt.Generation != uint64(generation+1) {
			t.Fatalf("generation=%d receipt=%+v", generation, receipt)
		}
		previous = []TruthCapsule{merged}
	}
	final := previous[0]
	facts := 0
	for _, entity := range final.Entities {
		if entity.Kind == EntityFact {
			facts++
		}
	}
	if facts != 1 || final.Generation != 3 ||
		final.Entities[0].Value != "definition c.go:30" {
		t.Fatalf("final capsule=%+v", final)
	}
}

func TestTruthCapsuleAuthorityEquivalenceIgnoresGenerationMetadata(t *testing.T) {
	required := testTruthCapsule(1, []TruthEntity{
		NewTruthEntity(EntityGoal, "active", "finish recovery", "runtime.plan"),
		NewTruthEntity(EntityCriticalPath, "runtime.go", "runtime.go", "runtime.working_set"),
	})
	retained := testTruthCapsule(3, append(
		append([]TruthEntity(nil), required.Entities...),
		NewTruthEntity(EntityFact, "extra", "extra evidence", "runtime.evidence"),
	))
	if err := retained.ContainsAuthority(required); err != nil {
		t.Fatal(err)
	}
	first, err := required.AuthorityDigest()
	if err != nil {
		t.Fatal(err)
	}
	required.Generation = 9
	required.Seal()
	second, err := required.AuthorityDigest()
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("authority digest changed with generation: %q != %q", first, second)
	}
}

func TestTruthCapsuleAuthorityEquivalenceRejectsLossOrMutation(t *testing.T) {
	required := testTruthCapsule(1, []TruthEntity{
		NewTruthEntity(EntityGoal, "active", "finish recovery", "runtime.plan"),
		NewTruthEntity(EntityTodo, "verify", "verify", "runtime.plan"),
	})
	lost := testTruthCapsule(2, required.Entities[:1])
	if err := lost.ContainsAuthority(required); err == nil {
		t.Fatal("lost authority entity was accepted")
	}
	changed := append([]TruthEntity(nil), required.Entities...)
	changed[0].Value = "different goal"
	mutated := testTruthCapsule(2, changed)
	if err := mutated.ContainsAuthority(required); err == nil {
		t.Fatal("mutated authority entity was accepted")
	}
}

func TestTruthCapsuleAuthorityIgnoresProtectedAndRefreshableRetention(t *testing.T) {
	required := testTruthCapsule(1, []TruthEntity{
		NewTruthEntity(EntityGoal, "active", "finish recovery", "runtime.plan"),
		NewTruthEntity(EntityCriticalPath, "runtime.go", "runtime.go", "runtime.working_set"),
		NewTruthEntity(EntityFact, "fact", "refreshable", "runtime.evidence"),
	})
	retained := testTruthCapsule(2, []TruthEntity{
		NewTruthEntity(EntityGoal, "active", "finish recovery", "runtime.plan"),
	})
	if err := retained.ContainsAuthority(required); err != nil {
		t.Fatal(err)
	}
	first, err := required.AuthorityDigest()
	if err != nil {
		t.Fatal(err)
	}
	second, err := retained.AuthorityDigest()
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("optional retention changed authority digest: %q != %q", first, second)
	}
}

func TestMergeTruthCapsulesDoesNotMutateAuthorityInput(t *testing.T) {
	current := testTruthCapsule(1, []TruthEntity{
		NewTruthEntity(EntityGoal, "active", "finish recovery", "runtime.plan"),
	})
	before := current.Entities[0]
	previous := testTruthCapsule(2, []TruthEntity{
		NewTruthEntity(EntityFact, "old", "old fact", "runtime.evidence"),
	})
	if _, _, err := MergeTruthCapsules(current, previous); err != nil {
		t.Fatal(err)
	}
	if len(current.Entities) != 1 || current.Entities[0] != before {
		t.Fatalf("authority input mutated = %+v", current.Entities)
	}
}

func testTruthCapsule(
	generation uint64,
	entities []TruthEntity,
) TruthCapsule {
	capsule := TruthCapsule{
		SchemaVersion: TruthSchemaVersion,
		Generation:    generation, CompatibilityHash: "sha256:compat",
		ModelID: "model", ContextTokens: 8192,
		DownshiftPolicy: DownshiftRuntimeTruthOnly,
		Entities:        append([]TruthEntity(nil), entities...),
	}
	capsule.Seal()
	return capsule
}

func TestTruthCapsuleRejectsInventedVerification(t *testing.T) {
	capsule := truthFixture("sha256:compat", "fixture-model", 4096)
	change := NewTruthEntity(
		EntityChange, "a.go", "a.go", "runtime.evidence",
	)
	change.Verified = true
	change.VerificationSource = "model.narrative"
	capsule.Entities = []TruthEntity{change}
	capsule.Seal()
	if err := capsule.Validate(); err == nil ||
		!strings.Contains(err.Error(), "runtime evidence") {
		t.Fatalf("invented verification accepted: %v", err)
	}
}

func TestTruthCapsuleSealCanonicalizesDuplicateEntities(t *testing.T) {
	capsule := truthFixture("sha256:compat", "fixture-model", 4096)
	first := NewTruthEntity(EntityTodo, "same step", "first", "runtime.plan")
	first.Status = "pending"
	last := NewTruthEntity(EntityTodo, "same step", "last", "runtime.plan")
	last.Status = "done"
	capsule.Entities = []TruthEntity{first, last}
	capsule.Seal()
	if err := capsule.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(capsule.Entities) != 1 ||
		capsule.Entities[0].Value != "last" ||
		capsule.Entities[0].Status != "done" {
		t.Fatalf("entities=%+v", capsule.Entities)
	}
}

func TestTruthMergeDetectsCompatibilityAndModelDownshift(t *testing.T) {
	previous := truthFixture("sha256:old", "large-model", 128_000)
	previous.Entities = []TruthEntity{
		NewTruthEntity(
			EntityCriticalPath, "critical.go", "critical.go",
			"runtime.working_set",
		),
	}
	previous.Seal()
	current := truthFixture("sha256:new", "small-model", 32_000)
	current.Seal()
	merged, receipt, err := MergeTruthCapsules(current, previous)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.CompatibilityMatched || !receipt.ModelDownshifted ||
		receipt.CriticalEntityCount != 0 || len(merged.Entities) != 0 {
		t.Fatalf("merged=%+v receipt=%+v", merged, receipt)
	}
}

func TestStructuredRenderMakesTruthMandatoryAndNarrativeOptional(t *testing.T) {
	capsule := truthFixture("sha256:compat", "fixture-model", 4096)
	capsule.Entities = []TruthEntity{
		NewTruthEntity(EntityGoal, "active", "finish CE5", "runtime.plan"),
	}
	capsule.Seal()
	summary := Summary{Window: 12, Goal: "finish CE5"}
	full, err := RenderStructured(
		summary,
		capsule,
		Narrative{Lines: []string{"assistant: optional detail"}},
		4096,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !full.NarrativeIncluded ||
		!strings.Contains(full.Text, TruthMarkerStart) ||
		!strings.Contains(full.Text, "optional detail") {
		t.Fatalf("render=%+v", full)
	}
	_, found, err := ParseTruthCapsule(full.Text)
	if err != nil || !found {
		t.Fatalf("parse found=%t err=%v", found, err)
	}
	if _, err := RenderStructured(summary, capsule, Narrative{}, 32); err == nil {
		t.Fatal("truth capsule fit an impossible budget")
	}
}

func TestStructuredRenderKeepsDigestForCarry(t *testing.T) {
	capsule := truthFixture("sha256:compat", "fixture-model", 4096)
	capsule.Seal()
	summary := Summary{
		Window:        4,
		Goal:          "keep going",
		Digest:        []string{"user: newest kept", "assistant: older kept"},
		OmittedDigest: 2,
	}
	rendered, err := RenderStructured(summary, capsule, Narrative{}, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rendered.Text, "user: newest kept") ||
		!strings.Contains(rendered.Text, "(2 more not listed)") {
		t.Fatalf("structured render dropped digest:\n%s", rendered.Text)
	}
	lines, omitted, ok := CarriedDigest(rendered.Text)
	if !ok || omitted != 2 || len(lines) != 2 || lines[0] != "user: newest kept" {
		t.Fatalf("carried=%v omitted=%d ok=%v", lines, omitted, ok)
	}
}

func truthFixture(
	compatibility string,
	modelID string,
	contextTokens uint64,
) TruthCapsule {
	value := TruthCapsule{
		SchemaVersion: TruthSchemaVersion, Generation: 1,
		CompatibilityHash: compatibility,
		ModelID:           modelID, ContextTokens: contextTokens,
		DownshiftPolicy: DownshiftRuntimeTruthOnly,
	}
	value.Seal()
	return value
}
