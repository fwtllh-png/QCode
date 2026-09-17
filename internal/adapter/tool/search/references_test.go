package search

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/persist/repoindex"
	"github.com/fwtllh-png/QCode/internal/platform/repowalk"
	"github.com/fwtllh-png/QCode/internal/platform/symbols"
)

func referencePayload(t *testing.T, r tool.Result) ([]referenceMatch, referenceCoverage) {
	t.Helper()
	var p struct {
		Matches  []referenceMatch  `json:"matches"`
		Coverage referenceCoverage `json:"completeness"`
	}
	if err := json.Unmarshal([]byte(r.Content), &p); err != nil {
		t.Fatal(err)
	}
	return p.Matches, p.Coverage
}

func TestReferenceEvidenceAliasesAndFallback(t *testing.T) {
	source := "import {run as launch} from './engine';\n// 中文\nlaunch(); launch();\nfunction local(launch:()=>void){launch();}\n"
	registry := indexedRegistry(t, map[string]string{
		"use.ts": source, "engine.ts": "export function run(){}", "other.ts": "export function run(){}", "notes.py": "# run is mentioned here\n",
	})
	result := execute(t, registry, KindReferences, map[string]any{"name": "run"})
	matches, coverage := referencePayload(t, result)
	if len(matches) != 3 || coverage.Status != "partial" || coverage.ScopedFiles != 3 || coverage.TextFiles != 1 {
		t.Fatalf("result=%s", result.Content)
	}
	for _, m := range matches[:2] {
		if m.Source != "repoindex_scoped" || m.TargetFile != "engine.ts" || m.Line != 3 || m.Text != "launch(); launch();" || m.Site == nil || source[m.Site.StartByte:m.Site.EndByte] != "launch" || m.SourceDigest != repowalk.Digest([]byte(source)) {
			t.Fatalf("match=%+v", m)
		}
	}
	if matches[2].Source != "text" || matches[2].RelationType != "text_match" {
		t.Fatalf("fallback=%+v", matches[2])
	}
	alias := execute(t, registry, KindReferences, map[string]any{"name": "launch", "path_prefix": "use"})
	if got, _ := referencePayload(t, alias); len(got) != 2 {
		t.Fatalf("alias=%s", alias.Content)
	}
	limited := execute(t, registry, KindReferences, map[string]any{"name": "run", "max_results": 1})
	got, c := referencePayload(t, limited)
	if len(got) != 1 || got[0].Source != "repoindex_scoped" || !limited.Truncated || !c.ResultsTruncated || limited.Metadata["matches"] != 3 {
		t.Fatalf("limited=%+v", limited)
	}
}

func TestReferenceEvidenceZeroDoesNotUndoShadowing(t *testing.T) {
	registry := indexedRegistry(t, map[string]string{"use.go": "package p\nfunc Use(Run func()){Run()}\n// Run\n", "run.go": "package p\nfunc Run(){}\n"})
	r := execute(t, registry, KindReferences, map[string]any{"name": "Run"})
	if m, c := referencePayload(t, r); len(m) != 0 || c.Status != "partial" {
		t.Fatalf("result=%s", r.Content)
	}
	text := execute(t, registry, KindReferences, map[string]any{"name": "Run", "mode": "text"})
	if m, _ := referencePayload(t, text); len(m) != 2 || m[0].Source != "text" {
		t.Fatalf("text=%s", text.Content)
	}
	defs := execute(t, registry, KindReferences, map[string]any{"name": "Run", "include_definitions": true})
	if m, _ := referencePayload(t, defs); len(m) != 1 || m[0].RelationType != "declaration" {
		t.Fatalf("defs=%s", defs.Content)
	}
	if hits := evidenceHits(t, defs); len(hits) != 1 || hits[0].Kind != tool.EvidenceDefinition {
		t.Fatalf("hits=%+v", hits)
	}
}

func TestReferenceEvidenceLSPFilterAndFallback(t *testing.T) {
	files := map[string]string{"api.go": "package p\nfunc Run(){}\n", "use.go": "package p\nfunc Use(){Run()}\n"}
	registry := indexedRegistryWithSemantic(t, files, fakeSemanticProvider{references: symbols.SemanticResult{Source: "lsp:test", Confidence: "high", Locations: []symbols.Location{{Path: "api.go", Line: 2, Character: 6}, {Path: "use.go", Line: 2, Character: 12}}}})
	r := execute(t, registry, KindReferences, map[string]any{"name": "Run", "path": "api.go", "line": 2, "character": 6, "path_prefix": "use", "max_results": 1})
	m, c := referencePayload(t, r)
	if len(m) != 1 || m[0].Source != "lsp:test" || m[0].Text != "func Use(){Run()}" || c.Status != "provider_reported" || r.Truncated {
		t.Fatalf("lsp=%s", r.Content)
	}
	failing := indexedRegistryWithSemantic(t, files, fakeSemanticProvider{err: errors.New("provider offline")})
	fallback := execute(t, failing, KindReferences, map[string]any{"name": "Run", "path": "api.go", "line": 2, "character": 6})
	if m, _ := referencePayload(t, fallback); len(m) != 1 || m[0].Source != "repoindex_scoped" || !strings.Contains(fallback.Content, "provider offline") {
		t.Fatalf("fallback=%s", fallback.Content)
	}
	text := execute(t, registry, KindReferences, map[string]any{"name": "Run", "path": "api.go", "line": 2, "character": 6, "mode": "text"})
	if m, _ := referencePayload(t, text); len(m) != 1 || m[0].Source != "text" {
		t.Fatalf("text=%s", text.Content)
	}
}

func referenceToolFixture(t *testing.T, options repoindex.Options, source string) (*symbolTool, string) {
	t.Helper()
	root := t.TempDir()
	write(t, filepath.Join(root, "use.go"), source)
	write(t, filepath.Join(root, "api.go"), "package p\nfunc Run(){}\n")
	store, err := repoindex.NewStore(openIndexDatabase(t), root)
	if err != nil {
		t.Fatal(err)
	}
	walker, err := repowalk.New(root, searchTestBackend{})
	if err != nil {
		t.Fatal(err)
	}
	index, err := repoindex.NewIndex(store, walker, options)
	if err != nil {
		t.Fatal(err)
	}
	executor, err := newSymbolTool(KindReferences, index, walker, nil)
	if err != nil {
		t.Fatal(err)
	}
	return executor, root
}

func TestReferenceEvidenceReportsIncompleteReadsAndIndex(t *testing.T) {
	source := "package p\nfunc Use(){Run();Run()}\n" + strings.Repeat("// padding\n", 30)
	t.Run("occurrence_limit", func(t *testing.T) {
		executor, _ := referenceToolFixture(t, repoindex.Options{ReferenceMaxCount: 1}, source)
		r, err := executor.run(tool.WithResultTokenBudget(t.Context(), 10000), symbolInput{Name: "Run", MaxResults: 10})
		if err != nil {
			t.Fatal(err)
		}
		if m, c := referencePayload(t, r); len(m) != 1 || c.TruncatedFiles != 1 {
			t.Fatalf("result=%s", r.Content)
		}
	})
	t.Run("read_limit", func(t *testing.T) {
		executor, _ := referenceToolFixture(t, repoindex.Options{}, source)
		r, err := executor.run(tool.WithResultTokenBudget(t.Context(), 32), symbolInput{Name: "Run", MaxResults: 10})
		if err != nil {
			t.Fatal(err)
		}
		if m, c := referencePayload(t, r); len(m) != 0 || c.Skipped[string(repowalk.SkipLarge)] != 1 {
			t.Fatalf("result=%s", r.Content)
		}
	})
	t.Run("stale_digest", func(t *testing.T) {
		executor, root := referenceToolFixture(t, repoindex.Options{}, source)
		if _, err := executor.index.Ensure(t.Context()); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, "use.go")
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		write(t, path, strings.ReplaceAll(source, "Run()", "Foo()"))
		if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
			t.Fatal(err)
		}
		r, err := executor.run(tool.WithResultTokenBudget(t.Context(), 10000), symbolInput{Name: "Run", MaxResults: 10})
		if err != nil {
			t.Fatal(err)
		}
		if m, c := referencePayload(t, r); len(m) != 0 || c.Skipped["stale_digest"] != 1 {
			t.Fatalf("result=%s", r.Content)
		}
	})
	t.Run("index_limit", func(t *testing.T) {
		executor, _ := referenceToolFixture(t, repoindex.Options{MaxFiles: 1}, source)
		r, err := executor.run(tool.WithResultTokenBudget(t.Context(), 10000), symbolInput{Name: "Run", MaxResults: 10})
		if err != nil {
			t.Fatal(err)
		}
		if _, c := referencePayload(t, r); !c.IndexTruncated {
			t.Fatalf("result=%s", r.Content)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		executor, _ := referenceToolFixture(t, repoindex.Options{}, source)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := executor.run(ctx, symbolInput{Name: "Run", MaxResults: 10}); !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v", err)
		}
	})
}
