package repoindex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/platform/repowalk"
	"github.com/fwtllh-png/QCode/internal/platform/symbols"
)

func TestEnsureBuildsThenRefreshesOnlyWhatChanged(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "api.go", "package api\n\nfunc Serve() {}\n")
	writeFile(t, root, "util.go", "package api\n\nfunc Helper() {}\n")
	index, store := newIndex(t, root, Options{})

	snapshot, err := ensureSettled(t, index)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Ready() || snapshot.Meta.FileCount != 2 || snapshot.Meta.SymbolCount != 2 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if snapshot.Meta.IndexerVersion != IndexerVersion {
		t.Fatalf("indexer version = %d", snapshot.Meta.IndexerVersion)
	}
	before, err := store.Files(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	// Only the changed file is read again: the untouched one keeps the exact row
	// the first build wrote, timestamp included.
	writeFile(t, root, "api.go", "package api\n\nfunc Serve() {}\n\nfunc Close() {}\n")
	if _, err := ensureSettled(t, index); err != nil {
		t.Fatal(err)
	}
	after, err := store.Files(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if after["util.go"] != before["util.go"] {
		t.Fatalf("untouched file was reindexed:\n%+v\n%+v", before["util.go"], after["util.go"])
	}
	if after["api.go"].Digest == before["api.go"].Digest || after["api.go"].SymbolCount != 2 {
		t.Fatalf("changed file = %+v", after["api.go"])
	}
	found, _, err := index.Symbols(t.Context(), Query{Name: "Close", Exact: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].Path != "api.go" || found[0].Line != 5 {
		t.Fatalf("symbols = %#v", found)
	}
}

func TestEnsurePrunesDeletedFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "kept.go", "package api\n\nfunc Kept() {}\n")
	writeFile(t, root, "gone.go", "package api\n\nfunc Gone() {}\n")
	index, _ := newIndex(t, root, Options{})
	if _, err := ensureSettled(t, index); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(filepath.Join(root, "gone.go")); err != nil {
		t.Fatal(err)
	}
	snapshot, err := ensureSettled(t, index)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Meta.FileCount != 1 || snapshot.Meta.SymbolCount != 1 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	found, _, err := index.Symbols(t.Context(), Query{Name: "Gone", Exact: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Fatalf("deleted file still findable: %#v", found)
	}
}

func TestEnsureReusesGraphUntilContentChanges(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "api.go", "package api\nfunc Serve() {}\n")
	index, store := newIndex(t, root, Options{})
	if _, err := ensureSettled(t, index); err != nil {
		t.Fatal(err)
	}
	// A write-rejecting trigger makes accidental rebuilds observable even though
	// graph errors intentionally do not fail symbol queries.
	if _, err := store.db.ExecContext(t.Context(), `
		CREATE TRIGGER reject_rank_write BEFORE INSERT ON repo_index_file_rank
		BEGIN SELECT RAISE(FAIL, 'graph write rejected'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureSettled(t, index); err != nil || index.graphFiles == nil {
		t.Fatalf("unchanged graph rebuilt: %v", err)
	}
	now := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(root, "api.go"), now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureSettled(t, index); err != nil || index.graphFiles == nil {
		t.Fatalf("timestamp-only change rebuilt graph: %v", err)
	}
	writeFile(t, root, "api.go", "package api\nfunc Changed() {}\n")
	if _, err := ensureSettled(t, index); err != nil || index.graphFiles != nil {
		t.Fatalf("failed graph build was cached: %v", err)
	}
	if _, err := store.db.ExecContext(t.Context(), "DROP TRIGGER reject_rank_write"); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureSettled(t, index); err != nil || index.graphFiles == nil {
		t.Fatalf("graph build did not recover: %v", err)
	}
	if err := os.Remove(filepath.Join(root, "api.go")); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureSettled(t, index); err != nil || len(index.graphFiles) != 0 {
		t.Fatalf("deleted graph node retained: %v", err)
	}
}

func TestEnsureRebuildsWhenTheIndexerVersionMoved(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "api.go", "package api\n\nfunc Serve() {}\n")
	index, store := newIndex(t, root, Options{})
	if _, err := ensureSettled(t, index); err != nil {
		t.Fatal(err)
	}
	// Rows produced by rules that no longer exist are discarded rather than mixed
	// with new ones, even though the files on disk did not change.
	if err := store.SetMeta(t.Context(), Meta{IndexerVersion: IndexerVersion - 1}); err != nil {
		t.Fatal(err)
	}
	fresh, err := NewIndex(store, newWalker(t, root), Options{})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := fresh.Ensure(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Meta.IndexerVersion != IndexerVersion || snapshot.Meta.FileCount != 1 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	found, _, err := fresh.Symbols(t.Context(), Query{Name: "Serve", Exact: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 {
		t.Fatalf("symbols after rebuild = %#v", found)
	}
}

func TestEnsureRecordsNoCompletionWhenCancelled(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a.go", "b.go", "c.go"} {
		writeFile(t, root, name, "package api\n\nfunc F() {}\n")
	}
	index, store := newIndex(t, root, Options{})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	snapshot, err := index.Ensure(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if snapshot.Ready() {
		t.Fatalf("cancelled build reported ready: %+v", snapshot)
	}
	if _, found, err := store.Meta(t.Context()); err != nil || found {
		t.Fatalf("meta found = %v after cancellation, err = %v", found, err)
	}

	// The next uncancelled call completes, so a cancelled build costs work and not
	// correctness.
	if snapshot, err = ensureSettled(t, index); err != nil || !snapshot.Ready() {
		t.Fatalf("snapshot = %+v, err = %v", snapshot, err)
	}
	if snapshot.Meta.FileCount != 3 {
		t.Fatalf("file count = %d", snapshot.Meta.FileCount)
	}
}

func TestEnsureDegradesWhenTheStoreCannotBeRead(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "api.go", "package api\n\nfunc Serve() {}\n")
	index, store := newIndex(t, root, Options{})
	if _, err := store.db.ExecContext(t.Context(), "DROP TABLE repo_index_symbols"); err != nil {
		t.Fatal(err)
	}

	snapshot, err := ensureSettled(t, index)
	if err != nil {
		t.Fatalf("a broken index failed the caller: %v", err)
	}
	if snapshot.Status != StatusDegraded || snapshot.Detail == "" {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	found, degraded, err := index.Symbols(t.Context(), Query{Name: "Serve"})
	if err != nil || len(found) != 0 || degraded.Ready() {
		t.Fatalf("symbols = %#v, snapshot = %+v, err = %v", found, degraded, err)
	}
	// Repeated calls stop rebuilding once the store has failed twice: a database
	// that cannot be read will not start working within one session.
	for attempt := 0; attempt < 3; attempt++ {
		if _, err := ensureSettled(t, index); err != nil {
			t.Fatal(err)
		}
	}
	if index.failures != maxResetAttempts {
		t.Fatalf("failures = %d", index.failures)
	}
}

func TestEnsureReportsTruncationAtTheFileCeiling(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a.go", "b.go", "c.go"} {
		writeFile(t, root, name, "package api\n\nfunc F() {}\n")
	}
	index, _ := newIndex(t, root, Options{MaxFiles: 2})
	snapshot, err := ensureSettled(t, index)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Ready() || !snapshot.Meta.Truncated || snapshot.Meta.FileCount != 2 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestEnsureIndexesUnsupportedAndRejectedFilesWithoutSymbols(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "README.md", "# title\n")
	writeFile(t, root, "big.go", "package api\n\nfunc Huge() {}\n")
	index, store := newIndex(t, root, Options{MaxFileBytes: 8})
	if _, err := ensureSettled(t, index); err != nil {
		t.Fatal(err)
	}
	files, err := store.Files(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("files = %#v", files)
	}
	// A file with no known language is recorded under the generic tier —
	// every file has a language row now, just not always a rule table.
	if files["README.md"].Language != symbols.LanguageGeneric ||
		files["README.md"].SymbolCount != 0 {
		t.Fatalf("markdown file = %+v", files["README.md"])
	}
	// A file the read policy rejected is still listed, with the reason in place of
	// a digest, so the next refresh does not read it again.
	if files["big.go"].Digest != "skipped:large" || files["big.go"].SymbolCount != 0 {
		t.Fatalf("large file = %+v", files["big.go"])
	}
	if files["big.go"].Language != symbols.LanguageGo {
		t.Fatalf("large file language = %q", files["big.go"].Language)
	}
}

func TestNilIndexBehavesAsDisabled(t *testing.T) {
	var index *Index
	snapshot, err := ensureSettled(t, index)
	if err != nil || snapshot.Status != StatusDisabled {
		t.Fatalf("snapshot = %+v, err = %v", snapshot, err)
	}
	found, snapshot, err := index.Symbols(t.Context(), Query{Name: "any"})
	if err != nil || len(found) != 0 || snapshot.Status != StatusDisabled {
		t.Fatalf("symbols = %#v, snapshot = %+v, err = %v", found, snapshot, err)
	}
	if index.Root() != "" || index.Snapshot().Status != StatusDisabled {
		t.Fatalf("root = %q, snapshot = %+v", index.Root(), index.Snapshot())
	}
}

func TestSnapshotReportsPendingBeforeTheFirstBuild(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "api.go", "package api\n\nfunc Serve() {}\n")
	index, _ := newIndex(t, root, Options{})
	// A pending index is not a failure: consumers stay available and the first
	// query builds it.
	if snapshot := index.Snapshot(); snapshot.Status != StatusPending || snapshot.Ready() {
		t.Fatalf("snapshot before the first build = %+v", snapshot)
	}
	if _, err := ensureSettled(t, index); err != nil {
		t.Fatal(err)
	}
	if snapshot := index.Snapshot(); !snapshot.Ready() {
		t.Fatalf("snapshot after the first build = %+v", snapshot)
	}
}

func TestNewIndexRequiresItsDependencies(t *testing.T) {
	root := t.TempDir()
	if _, err := NewIndex(nil, newWalker(t, root), Options{}); err == nil {
		t.Fatal("missing store accepted")
	}
	if _, err := NewIndex(openStore(t), nil, Options{}); err == nil {
		t.Fatal("missing walker accepted")
	}
}

func TestRelatedTestsFollowsNamingConventions(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{
		"api.go", "api_test.go", "helper_test.go",
		"pkg/service.py", "pkg/test_service.py",
		"web/app.ts", "web/__tests__/app.test.ts",
		"src/main/java/app/Service.java", "src/test/java/app/ServiceTest.java",
		"notes.md",
	} {
		writeFile(t, root, name, "content\n")
	}
	index, _ := newIndex(t, root, Options{})

	related, snapshot, err := index.RelatedTests(t.Context(), []string{
		"api.go", "pkg/service.py", "web/app.ts",
		"src/main/java/app/Service.java", "notes.md", "api_test.go",
	})
	if err != nil || !snapshot.Ready() {
		t.Fatalf("snapshot = %+v, err = %v", snapshot, err)
	}
	// The fixture files reference no symbols across files, so the graph route
	// contributes nothing here and every row arrives by convention.
	for source, want := range map[string][]string{
		// Go tests are package scoped, so every test file in the directory counts.
		"api.go":                         {"api_test.go", "helper_test.go"},
		"pkg/service.py":                 {"pkg/test_service.py"},
		"web/app.ts":                     {"web/__tests__/app.test.ts"},
		"src/main/java/app/Service.java": {"src/test/java/app/ServiceTest.java"},
		"api_test.go":                    {"api_test.go"},
	} {
		got := Paths(map[string][]RelatedTest{source: related[source]})[source]
		if !equal(got, want) {
			t.Errorf("related[%q] = %#v, want %#v", source, got, want)
		}
	}
	for source, tests := range related {
		for _, test := range tests {
			if test.Resolution != TestFromConvention && test.Resolution != TestFromGraph {
				t.Errorf("related[%q][%q] resolution = %q", source, test.Path, test.Resolution)
			}
		}
	}
	// A language with no convention this package knows is absent rather than
	// mapped to nothing, so a caller can tell "no tests" from "cannot tell".
	if _, present := related["notes.md"]; present {
		t.Fatalf("markdown mapped to %#v", related["notes.md"])
	}
}

// ensureSettled refreshes and waits out the background graph builder, so a
// test's graph and rank assertions see the committed result.
func ensureSettled(t *testing.T, index *Index) (Snapshot, error) {
	t.Helper()
	snapshot, err := index.Ensure(t.Context())
	index.waitGraphSettled()
	return snapshot, err
}

func newIndex(t *testing.T, root string, options Options) (*Index, *Store) {
	t.Helper()
	store, err := NewStore(openDatabase(t), root)
	if err != nil {
		t.Fatal(err)
	}
	index, err := NewIndex(store, newWalker(t, root), options)
	if err != nil {
		t.Fatal(err)
	}
	return index, store
}

func newWalker(t *testing.T, root string) *repowalk.Walker {
	t.Helper()
	walker, err := repowalk.New(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	return walker
}

func writeFile(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	// Keep modification times apart so an incremental refresh can see a change on
	// filesystems with coarse timestamps.
	stamp := time.Now().Add(time.Duration(len(content)) * time.Millisecond)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureRecordsLayeredDetailForEveryLanguage(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "api.go", "package api\n\n// Serve answers.\nfunc Serve(path string) error {\n\treturn nil\n}\n")
	writeFile(t, root, "engine.cpp", "namespace engine {\n\n// Run starts the engine.\nint Run(int argc) {\n\treturn 0;\n}\n\n}\n")
	writeFile(t, root, "tool.kt", "class Tool {\n    fun apply(x: Int) {}\n}\n")
	index, _ := newIndex(t, root, Options{})

	if _, err := ensureSettled(t, index); err != nil {
		t.Fatal(err)
	}
	found, snapshot, err := index.Symbols(t.Context(), Query{Name: "", Paths: []string{"api.go"}})
	if err != nil || !snapshot.Ready() {
		t.Fatalf("symbols: %v %+v", err, snapshot)
	}
	if len(found) != 1 || found[0].Name != "Serve" {
		t.Fatalf("go symbols = %#v", found)
	}
	if found[0].Signature != "func Serve(path string) error" {
		t.Fatalf("signature = %q", found[0].Signature)
	}
	if found[0].Docstring != "Serve answers." {
		t.Fatalf("docstring = %q", found[0].Docstring)
	}
	if found[0].Resolution != symbols.ResolutionSyntax {
		t.Fatalf("resolution = %q", found[0].Resolution)
	}

	found, _, err = index.Symbols(t.Context(), Query{Name: "", Paths: []string{"engine.cpp"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].Name != "Run" || found[0].Container != "engine" {
		t.Fatalf("cpp symbols = %#v", found)
	}
	if found[0].Docstring != "Run starts the engine." {
		t.Fatalf("cpp docstring = %q", found[0].Docstring)
	}

	// A language without a rule table still yields rows, marked heuristic.
	found, _, err = index.Symbols(t.Context(), Query{Name: "", Paths: []string{"tool.kt"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 2 || found[0].Name != "Tool" || found[1].Name != "apply" ||
		found[1].Resolution != symbols.ResolutionHeuristic {
		t.Fatalf("kotlin symbols = %#v", found)
	}
}

func TestStoreRoundTripsReferences(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "api.go", "package api\n\nfunc Serve() error {\n\treturn Helper(1)\n}\n")
	index, store := newIndex(t, root, Options{})
	if _, err := ensureSettled(t, index); err != nil {
		t.Fatal(err)
	}
	rows, err := store.db.Query(
		"SELECT name, use_count FROM repo_index_references WHERE root_path = ? AND path = ?",
		store.root, "api.go",
	)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var name string
		var uses int
		if err := rows.Scan(&name, &uses); err != nil {
			t.Fatal(err)
		}
		counts[name] = uses
	}
	if counts["Serve"] == 0 || counts["Helper"] == 0 {
		t.Fatalf("references = %#v", counts)
	}
	if _, counted := counts["return"]; counted {
		t.Fatalf("stop word counted: %#v", counts)
	}
}

func TestSyntaxIndexPersistsDeclarationsAndImports(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "web/app.tsx", "import {\n render\n} from './view';\nexport const App = () => <div/>;\nconst text = `\nfunction Fake(){}\n`;\n")
	writeFile(t, root, "web/view.ts", "export function render() {}\n")
	index, store := newIndex(t, root, Options{})
	if _, err := ensureSettled(t, index); err != nil {
		t.Fatal(err)
	}
	found, _, err := index.Symbols(t.Context(), Query{Name: "App", Exact: true})
	if err != nil || len(found) != 1 || found[0].Resolution != ResolutionSyntax || found[0].Kind != symbols.KindFunction {
		t.Fatalf("symbols=%+v err=%v", found, err)
	}
	fake, _, err := index.Symbols(t.Context(), Query{Name: "Fake", Exact: true})
	if err != nil || len(fake) != 0 {
		t.Fatalf("literal symbols=%+v err=%v", fake, err)
	}
	// Read the persisted specifier, not an in-memory extractor result.
	imports, err := store.Imports(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(imports["web/app.tsx"]) != 1 || imports["web/app.tsx"][0] != "./view" {
		t.Fatalf("imports=%+v", imports)
	}
}

func TestWeakestResolutionIncludesSyntax(t *testing.T) {
	for _, tt := range []struct {
		values []Symbol
		want   string
	}{
		{[]Symbol{{Resolution: ResolutionSyntax}}, ResolutionSyntax},
		{[]Symbol{{Resolution: ResolutionSyntax}, {Resolution: ResolutionLexical}}, ResolutionLexical},
		{[]Symbol{{Resolution: ResolutionLexical}, {Resolution: ResolutionSyntax}}, ResolutionLexical},
		{[]Symbol{{Resolution: ResolutionSyntax}, {Resolution: ResolutionHeuristic}}, ResolutionHeuristic},
	} {
		if got := WeakestResolution(tt.values); got != tt.want {
			t.Errorf("resolution=%s want=%s", got, tt.want)
		}
	}
}
