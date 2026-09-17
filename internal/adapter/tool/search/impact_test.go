package search

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/persist/repoindex"
	"github.com/fwtllh-png/QCode/internal/platform/repowalk"
	"github.com/fwtllh-png/QCode/internal/platform/symbols"
)

func TestRelatedTestToolExplainsRecommendations(t *testing.T) {
	registry := indexedRegistry(t, map[string]string{
		"core.go":      "package p\nfunc Run(){}\n",
		"odd_test.go":  "package p\nfunc TestRun(){Run()}\n",
		"core_test.go": "package p\nfunc TestEmpty(){}\n",
	})
	r := execute(t, registry, KindRelatedTests, map[string]any{"paths": []string{"core.go"}})
	var payload struct {
		Coverage []struct {
			Tests        []relatedTestEvidence    `json:"tests"`
			Completeness repoindex.ImpactCoverage `json:"completeness"`
		} `json:"coverage"`
		Claim string `json:"claim"`
	}
	if err := json.Unmarshal([]byte(r.Content), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Claim != "recommended_tests_not_proven_coverage" || len(payload.Coverage) != 1 || len(payload.Coverage[0].Tests) != 2 {
		t.Fatalf("result=%s", r.Content)
	}
	tests := payload.Coverage[0].Tests
	if tests[0].Reason != "scoped_reference_candidate" || len(tests[0].Chain) != 1 || tests[0].Chain[0].Text != "func TestRun(){Run()}" || tests[0].Chain[0].EvidenceStatus != "verified_digest" {
		t.Fatalf("scoped=%+v", tests[0])
	}
	if tests[1].Reason != "naming_convention" || len(tests[1].Chain) != 0 {
		t.Fatalf("convention=%+v", tests[1])
	}
	if payload.Coverage[0].Completeness.Status != "partial" {
		t.Fatalf("result=%s", r.Content)
	}
}

func TestImpactSnippetRefusesStaleEvidence(t *testing.T) {
	source := "package p\nfunc Use(){Run()}\n"
	executor, _ := referenceToolFixture(t, repoindex.Options{}, source)
	start := strings.Index(source, "Run")
	relation := repoindex.ReferenceRelation{Source: "use.go", Destination: "api.go", SourceDigest: repowalk.Digest([]byte("old source")), Site: symbols.ReferenceSite{Name: "Run", StartByte: start, EndByte: start + 3, Line: 2}}
	rows, err := newImpactEvidenceReader(executor).rows(t.Context(), []repoindex.RelatedTest{{Path: "use_test.go", Chain: []repoindex.ImpactStep{{Dependency: "api.go", Dependent: "use.go", Kind: repoindex.EdgePackageReference, Evidence: &relation}}}})
	if err != nil {
		t.Fatal(err)
	}
	step := rows[0].Chain[0]
	if step.Text != "" || step.Evidence != nil || step.EvidenceStatus != "stale_digest" {
		t.Fatalf("stale=%+v", step)
	}
}
