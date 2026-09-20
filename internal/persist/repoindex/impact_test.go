package repoindex

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// The impact tests walk a repository whose files depend on each other in
// known directions: what the reverse closure should reach, how many hops it
// took, and which route found each test.

// impactFixture builds a repository wired as follows:
//
//	core/core.go          the changed file, exports Run
//	mid/mid.go            imports core, exports Wrap (calls Run)
//	app/app.go            imports mid, calls Wrap
//	tests/odd_name.py     imports nothing by name, but references Run —
//	                      a test file no naming convention would pair with core
//	tests/integration.rs  references Wrap — Rust, which has no convention here
func writeImpactFixture(t *testing.T, root string) {
	t.Helper()
	writeFile(t, root, "core/core.go",
		"package core\n\n// Run does the work.\nfunc Run() error { return nil }\n")
	writeFile(t, root, "mid/mid.go", "package mid\n\nimport \"example.com/app/core\"\n\nfunc Wrap() error { return core.Run() }\n")
	writeFile(t, root, "app/app.go", "package main\n\nimport \"example.com/app/mid\"\n\nfunc main() { _ = mid.Wrap() }\n")
	writeFile(t, root, "tests/odd_name.py", "from core import Run\n\ndef check():\n    assert Run() is None\n")
	writeFile(t, root, "tests/integration.rs", "use crate::mid::Wrap;\n\n#[test]\nfn verifies() {\n    let _ = Wrap();\n}\n")
	// Give the Rust test a crate path to reach: a src/ tree beside it.
	writeFile(t, root, "src/mid.rs", "pub fn Wrap() {}\n")
}

func TestImpactWalksReverseDependenciesWithHops(t *testing.T) {
	root := t.TempDir()
	writeImpactFixture(t, root)
	index, _ := newIndex(t, root, Options{})

	snapshot, err := ensureSettled(t, index)
	if err != nil || !snapshot.Ready() {
		t.Fatalf("ensure: %v %+v", err, snapshot)
	}
	impacted, truncated, err := index.Impact(t.Context(), []string{"core/core.go"})
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Fatalf("small fixture reported truncation")
	}
	// mid.go imports core: one hop. app.go imports mid: two hops. The test
	// files reference the exported names: one hop each. The src/mid.rs shadow
	// of Wrap is a declaration, not a dependent of core.
	for path, wantHops := range map[string]int{
		"mid/mid.go":           1,
		"app/app.go":           2,
		"tests/odd_name.py":    1,
		"tests/integration.rs": 2,
	} {
		hit, reached := impacted[path]
		if !reached {
			t.Fatalf("expected %s in the closure, got %#v", path, impacted)
		}
		if hit.Hops != wantHops {
			t.Fatalf("%s hops = %d, want %d", path, hit.Hops, wantHops)
		}
	}
	// A same-named declaration in another language (Wrap lives in both
	// mid/mid.go and src/mid.rs) joins the closure through a reference edge —
	// the documented cost of a lexical graph, carried openly on the edge's
	// kind so a consumer can weigh it.
	if shadow, reached := impacted["src/mid.rs"]; reached {
		if shadow.Via != EdgeReference {
			t.Fatalf("declaration shadow reached by %q, want reference", shadow.Via)
		}
	}
	if _, self := impacted["core/core.go"]; self {
		t.Fatalf("the changed file reached itself")
	}
}

func TestRelatedTestsMergesGraphAndConventionRoutes(t *testing.T) {
	root := t.TempDir()
	writeImpactFixture(t, root)
	// A convention-shaped Go test, which the graph also reaches through its
	// reference of Run.
	writeFile(t, root, "core/core_test.go", "package core\n\nimport \"testing\"\n\nfunc TestRun(t *testing.T) { _ = Run() }\n")
	index, _ := newIndex(t, root, Options{})

	if _, err := ensureSettled(t, index); err != nil {
		t.Fatal(err)
	}
	related, snapshot, err := index.RelatedTests(t.Context(), []string{"core/core.go"})
	if err != nil || !snapshot.Ready() {
		t.Fatalf("related: %v %+v", err, snapshot)
	}
	rows := related["core/core.go"]
	byPath := map[string]RelatedTest{}
	for _, row := range rows {
		byPath[row.Path] = row
	}
	// The graph reaches the irregularly named test and the Rust integration;
	// the convention would pair neither with core.go.
	graph, found := byPath["tests/odd_name.py"]
	if !found || graph.Resolution != TestFromGraph || graph.Hops != 1 {
		t.Fatalf("odd_name.py = %#v, want a one-hop graph hit", graph)
	}
	rust, found := byPath["tests/integration.rs"]
	if !found || rust.Resolution != TestFromGraph {
		t.Fatalf("integration.rs = %#v, want a graph hit — Rust has no convention", rust)
	}
	// The convention-shaped test is reached by both routes; the graph's
	// evidence is the more specific claim and must win the duplicate.
	conventional, found := byPath["core/core_test.go"]
	if !found {
		t.Fatalf("core_test.go missing from %#v", rows)
	}
	if conventional.Resolution != TestFromGraph || conventional.Hops != 1 {
		t.Fatalf("core_test.go = %#v, want the graph route to win the duplicate", conventional)
	}
	// Graph hits answer first, closest first.
	for index := 1; index < len(rows); index++ {
		previous, current := rows[index-1], rows[index]
		if previous.Resolution == TestFromGraph && current.Resolution == TestFromGraph {
			if previous.Hops > current.Hops {
				t.Fatalf("graph rows not ordered by hops: %#v", rows)
			}
		}
	}
}

func TestImpactRespectsTheResultBound(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "core/core.go", "package core\n\nfunc Run() error { return nil }\n")
	for index := 0; index < 12; index++ {
		writeFile(t, root, fmt.Sprintf("dep/dep%02d.go", index),
			fmt.Sprintf("package dep\n\nimport \"example.com/app/core\"\n\nfunc Use%02d() { _ = core.Run() }\n", index))
	}
	index, _ := newIndex(t, root, Options{
		Impact: ImpactOptions{MaxDepth: 3, MaxResults: 4},
	})
	if _, err := ensureSettled(t, index); err != nil {
		t.Fatal(err)
	}
	impacted, truncated, err := index.Impact(t.Context(), []string{"core/core.go"})
	if err != nil {
		t.Fatal(err)
	}
	if !truncated {
		t.Fatalf("expected truncation with %d hits under the bound", len(impacted))
	}
	if len(impacted) > 4 {
		t.Fatalf("closure size %d exceeds the bound", len(impacted))
	}
}

func TestRelatedTestsKeepsConventionWhenGraphIsSilent(t *testing.T) {
	root := t.TempDir()
	// No import specifiers and no shared names: the graph has nothing to say,
	// and the convention answers alone.
	writeFile(t, root, "api.go", "package api\n\nfunc Serve() {}\n")
	writeFile(t, root, "api_test.go", "package api\n\nimport \"testing\"\n\nfunc TestServe(t *testing.T) {}\n")
	index, _ := newIndex(t, root, Options{})
	if _, err := ensureSettled(t, index); err != nil {
		t.Fatal(err)
	}
	related, _, err := index.RelatedTests(t.Context(), []string{"api.go"})
	if err != nil {
		t.Fatal(err)
	}
	rows := related["api.go"]
	if len(rows) != 1 || rows[0].Path != "api_test.go" ||
		rows[0].Resolution != TestFromConvention {
		t.Fatalf("rows = %#v, want api_test.go by convention", rows)
	}
}

func TestRelatedTestsAttributesEachSourceAndKeepsEvidenceChain(t *testing.T) {
	root := t.TempDir()
	for path, source := range map[string]string{
		"a/core.go":       "package a\nfunc Run(){}\n",
		"b/core.go":       "package b\nfunc Other(){}\n",
		"mid/mid.go":      "package mid\nimport a \"example/a\"\nfunc Wrap(){a.Run()}\n",
		"tests/a_test.go": "package tests\nimport m \"example/mid\"\nfunc TestWrap(){m.Wrap()}\n",
		"tests/b_test.go": "package tests\nimport b \"example/b\"\nfunc TestOther(){b.Other()}\n",
	} {
		writeFile(t, root, path, source)
	}
	index, _ := newIndex(t, root, Options{})
	if _, err := ensureSettled(t, index); err != nil {
		t.Fatal(err)
	}
	related, coverage, _, err := index.RelatedTestsWithEvidence(t.Context(), []string{"a/core.go", "b/core.go"})
	if err != nil {
		t.Fatal(err)
	}
	a, b := related["a/core.go"], related["b/core.go"]
	if len(a) != 1 || a[0].Path != "tests/a_test.go" || len(b) != 1 || b[0].Path != "tests/b_test.go" {
		t.Fatalf("cross-attributed: %+v", related)
	}
	if len(a[0].Chain) != 2 || a[0].Reason != "scoped_reference_candidate" {
		t.Fatalf("a=%+v", a)
	}
	chain := a[0].Chain
	if chain[0].Dependency != "a/core.go" || chain[0].Dependent != "mid/mid.go" || chain[1].Dependency != "mid/mid.go" || chain[1].Dependent != "tests/a_test.go" {
		t.Fatalf("chain=%+v", chain)
	}
	for _, step := range chain {
		if step.Kind != EdgeImportReference || step.Evidence == nil || step.Evidence.Site.Line != 3 {
			t.Fatalf("step=%+v", step)
		}
	}
	if coverage["a/core.go"].ResultsTruncated || coverage["a/core.go"].DepthTruncated {
		t.Fatalf("coverage=%+v", coverage)
	}
}

func TestImpactChainLimitsCyclesAndEvidenceStrength(t *testing.T) {
	edges := []graphEdge{{Src: "mid", Dst: "core", Kind: EdgeImport}, {Src: "test", Dst: "mid", Kind: EdgeReference}, {Src: "core", Dst: "test", Kind: EdgeImport}}
	hits, c, err := walkImpact(t.Context(), []string{"core"}, edges, ImpactOptions{MaxDepth: 1, MaxResults: 10}, nil)
	if err != nil || len(hits) != 1 || !c.DepthTruncated {
		t.Fatalf("hits=%+v coverage=%+v err=%v", hits, c, err)
	}
	hits, c, err = walkImpact(t.Context(), []string{"core"}, edges, ImpactOptions{MaxDepth: 5, MaxResults: 10}, nil)
	if err != nil || len(hits) != 2 || c.DepthTruncated || impactReason(hits["test"].Chain) != "name_based_candidate" || impactReason(hits["mid"].Chain) != "dependency_candidate" {
		t.Fatalf("hits=%+v c=%+v err=%v", hits, c, err)
	}
	_, c, err = walkImpact(t.Context(), []string{"core"}, edges, ImpactOptions{MaxDepth: 5, MaxResults: 1}, nil)
	if err != nil || !c.ResultsTruncated {
		t.Fatalf("coverage=%+v err=%v", c, err)
	}
}

// Read a small allowlist of actual QCode files, not the entire worktree. This
// checks real syntax and avoids accidentally indexing local runbooks/secrets.
func TestQCodeRelatedTestsEvidence(t *testing.T) {
	root := t.TempDir()
	const source = "internal/platform/repowalk/repowalk.go"
	const expected = "internal/platform/repowalk/repowalk_test.go"
	paths := []string{source, expected, "internal/platform/symbols/references_test.go"}
	content := map[string]string{}
	for _, path := range paths {
		data, err := os.ReadFile("../../../" + path)
		if err != nil {
			t.Fatal(err)
		}
		content[path] = string(data)
		writeFile(t, root, path, string(data))
	}
	index, _ := newIndex(t, root, Options{})
	if _, err := ensureSettled(t, index); err != nil {
		t.Fatal(err)
	}
	related, _, _, err := index.RelatedTestsWithEvidence(t.Context(), []string{source})
	if err != nil {
		t.Fatal(err)
	}
	rows := related[source]
	if len(rows) != 1 || rows[0].Path != expected || rows[0].Reason != "scoped_reference_candidate" || len(rows[0].Chain) != 1 {
		t.Fatalf("real QCode result=%+v", rows)
	}
	evidence := rows[0].Chain[0].Evidence
	if evidence == nil {
		t.Fatal("missing real reference")
	}
	site := evidence.Site
	if content[expected][site.StartByte:site.EndByte] != site.Name || !strings.Contains(strings.Split(content[expected], "\n")[site.Line-1], site.Name) {
		t.Fatalf("evidence=%+v", evidence)
	}
	t.Logf("QCode evidence: %s:%d uses %s in %s", evidence.Source, site.Line, site.Name, evidence.Destination)
}

func TestRelatedTestsReportsUnavailableGraphAndPreservesConvention(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "api.go", "package p\nfunc Run(){}\n")
	writeFile(t, root, "api_test.go", "package p\nfunc TestRun(){Run()}\n")
	index, store := newIndex(t, root, Options{})
	if _, err := ensureSettled(t, index); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(t.Context(), "DROP TABLE repo_index_edges"); err != nil {
		t.Fatal(err)
	}
	related, coverage, snapshot, err := index.RelatedTestsWithEvidence(t.Context(), []string{"api.go"})
	if err != nil || !snapshot.Ready() {
		t.Fatalf("snapshot=%+v err=%v", snapshot, err)
	}
	rows := related["api.go"]
	if !coverage["api.go"].GraphUnavailable || len(rows) != 1 || rows[0].Reason != "naming_convention" || len(rows[0].Chain) != 0 {
		t.Fatalf("related=%+v coverage=%+v", related, coverage)
	}
}
