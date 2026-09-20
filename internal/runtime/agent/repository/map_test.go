package repository

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/fwtllh-png/QCode/internal/persist/repoindex"
)

type stubIndex struct {
	files    map[string]repoindex.File
	symbols  []repoindex.Symbol
	snapshot repoindex.Snapshot
	err      error
	queries  []repoindex.Query
	// refreshingQueries counts calls that took the refreshing Symbols path
	// instead of the snapshot-pinned SymbolsFrom path.
	refreshingQueries int
}

func (s *stubIndex) Files(context.Context) (map[string]repoindex.File, repoindex.Snapshot, error) {
	return s.files, s.snapshot, s.err
}

func (s *stubIndex) SymbolsFrom(
	_ context.Context,
	_ repoindex.Snapshot,
	query repoindex.Query,
) ([]repoindex.Symbol, repoindex.Snapshot, error) {
	return s.match(query)
}

func (s *stubIndex) Symbols(_ context.Context, query repoindex.Query) ([]repoindex.Symbol, repoindex.Snapshot, error) {
	s.refreshingQueries++
	return s.match(query)
}

func (s *stubIndex) match(query repoindex.Query) ([]repoindex.Symbol, repoindex.Snapshot, error) {
	s.queries = append(s.queries, query)
	if s.err != nil {
		return nil, s.snapshot, s.err
	}
	var found []repoindex.Symbol
	for _, symbol := range s.symbols {
		for _, candidate := range query.Paths {
			if symbol.Path == candidate {
				found = append(found, symbol)
				break
			}
		}
	}
	return found, s.snapshot, nil
}

func readyIndex(files map[string]repoindex.File, symbols []repoindex.Symbol) *stubIndex {
	return &stubIndex{
		files: files, symbols: symbols,
		snapshot: repoindex.Snapshot{Status: repoindex.StatusReady},
	}
}

func file(path, language string, symbols int) repoindex.File {
	return repoindex.File{Path: path, Language: language, SymbolCount: symbols, Size: 100,
		EntryPoint: repoindex.EntryPoint(path)}
}

func TestBuildSummarizesDirectoriesBuildFilesAndEntries(t *testing.T) {
	index := readyIndex(map[string]repoindex.File{
		"go.mod":                   file("go.mod", "", 0),
		"Makefile":                 file("Makefile", "", 0),
		"cmd/app/main.go":          file("cmd/app/main.go", "go", 1),
		"internal/store/store.go":  file("internal/store/store.go", "go", 9),
		"internal/store/query.go":  file("internal/store/query.go", "go", 4),
		"internal/store/notes.md":  file("internal/store/notes.md", "", 0),
		"internal/render/html.go":  file("internal/render/html.go", "go", 3),
		"internal/render/html.ts":  file("internal/render/html.ts", "typescript", 2),
		"internal/render/util.tsx": file("internal/render/util.tsx", "typescript", 1),
	}, nil)

	built := Build(context.Background(), index, nil, Options{})
	if !built.Ready() {
		t.Fatalf("status = %q, detail = %q", built.Status, built.Detail)
	}
	if built.FileCount != 9 || built.SymbolCount != 20 {
		t.Fatalf("files = %d, symbols = %d", built.FileCount, built.SymbolCount)
	}
	if len(built.Build) != 2 || built.Build[0] != "Makefile" || built.Build[1] != "go.mod" {
		t.Fatalf("build = %v", built.Build)
	}
	if len(built.Entries) != 1 || built.Entries[0] != "cmd/app/main.go" {
		t.Fatalf("entries = %v", built.Entries)
	}

	byPath := map[string]Directory{}
	for _, directory := range built.Directories {
		byPath[directory.Path] = directory
	}
	root, found := byPath["."]
	if !found || root.Files != 2 {
		t.Fatalf("root group = %+v, want the two manifests", root)
	}
	store, found := byPath["internal/store"]
	if !found || store.Files != 3 || store.Symbols != 13 {
		t.Fatalf("internal/store = %+v", store)
	}
	if len(store.Languages) != 1 || store.Languages[0] != "go" {
		t.Fatalf("internal/store languages = %v, want only the language it has", store.Languages)
	}
	render := byPath["internal/render"]
	if len(render.Languages) != 2 || render.Languages[0] != "typescript" || render.Languages[1] != "go" {
		t.Fatalf("internal/render languages = %v, want the commonest first", render.Languages)
	}
	// Directories are listed by path, not by rank, once membership is decided.
	for index := 1; index < len(built.Directories); index++ {
		if built.Directories[index-1].Path >= built.Directories[index].Path {
			t.Fatalf("directories are not path-ordered: %+v", built.Directories)
		}
	}
}

func TestBuildKeepsTheDirectoriesHoldingTheMostCode(t *testing.T) {
	files := map[string]repoindex.File{}
	for index := 0; index < 10; index++ {
		path := fmt.Sprintf("pkg/mod%02d/code.go", index)
		files[path] = file(path, "go", index)
	}
	built := Build(context.Background(), readyIndex(files, nil), nil, Options{MaxDirectories: 3})
	if len(built.Directories) != 3 || built.OmittedDirectories != 7 {
		t.Fatalf("directories = %+v, omitted = %d", built.Directories, built.OmittedDirectories)
	}
	kept := map[string]bool{}
	for _, directory := range built.Directories {
		kept[directory.Path] = true
	}
	for _, want := range []string{"pkg/mod07", "pkg/mod08", "pkg/mod09"} {
		if !kept[want] {
			t.Fatalf("directories = %+v, want the symbol-richest kept", built.Directories)
		}
	}
}

func TestBuildGroupsAtTheConfiguredDepth(t *testing.T) {
	files := map[string]repoindex.File{
		"internal/a/b/c/deep.go": file("internal/a/b/c/deep.go", "go", 1),
	}
	shallow := Build(context.Background(), readyIndex(files, nil), nil, Options{Depth: 1})
	if shallow.Directories[0].Path != "internal" {
		t.Fatalf("depth 1 = %+v", shallow.Directories)
	}
	deep := Build(context.Background(), readyIndex(files, nil), nil, Options{Depth: 3})
	if deep.Directories[0].Path != "internal/a/b" {
		t.Fatalf("depth 3 = %+v", deep.Directories)
	}
}

func TestBuildOutlinesTheFocusedFilesInLineOrder(t *testing.T) {
	index := readyIndex(
		map[string]repoindex.File{"a.go": file("a.go", "go", 3), "b.go": file("b.go", "go", 1)},
		[]repoindex.Symbol{
			{Path: "a.go", Name: "Second", Kind: "function", Line: 20},
			{Path: "a.go", Name: "First", Kind: "type", Line: 4},
			{Path: "b.go", Name: "Other", Kind: "function", Line: 1},
			{Path: "c.go", Name: "Unfocused", Kind: "function", Line: 1},
		},
	)
	built := Build(context.Background(), index, []string{"a.go", "missing.go", "a.go", ""}, Options{})
	if len(built.Outlines) != 1 {
		t.Fatalf("outlines = %+v, want only the focused file the index knows", built.Outlines)
	}
	outline := built.Outlines[0]
	if outline.Path != "a.go" || len(outline.Symbols) != 2 {
		t.Fatalf("outline = %+v", outline)
	}
	if outline.Symbols[0].Name != "First" || outline.Symbols[1].Name != "Second" {
		t.Fatalf("outline order = %+v, want line order", outline.Symbols)
	}
	if len(index.queries) != 1 || len(index.queries[0].Paths) != 2 {
		t.Fatalf("queries = %+v, want one query over the deduped paths", index.queries)
	}
}

func TestBuildMarksAnOutlineItHadToCut(t *testing.T) {
	var symbols []repoindex.Symbol
	for line := 1; line <= 5; line++ {
		symbols = append(symbols, repoindex.Symbol{
			Path: "a.go", Name: fmt.Sprintf("Sym%d", line), Kind: "function", Line: line,
		})
	}
	index := readyIndex(map[string]repoindex.File{"a.go": file("a.go", "go", 5)}, symbols)
	built := Build(context.Background(), index, []string{"a.go"}, Options{MaxOutlineSymbols: 2})
	outline := built.Outlines[0]
	if len(outline.Symbols) != 2 || !outline.Truncated {
		t.Fatalf("outline = %+v", outline)
	}
}

func TestBuildReportsWhyItHasNothingToSay(t *testing.T) {
	empty := Build(context.Background(), readyIndex(map[string]repoindex.File{}, nil), nil, Options{})
	if !empty.Ready() || empty.FileCount != 0 || len(empty.Directories) != 0 {
		t.Fatalf("empty index = %+v", empty)
	}

	if disabled := Build(context.Background(), nil, nil, Options{}); disabled.Status != repoindex.StatusDisabled {
		t.Fatalf("nil index = %+v", disabled)
	}

	degraded := Build(context.Background(), &stubIndex{
		snapshot: repoindex.Snapshot{Status: repoindex.StatusReady},
		err:      errors.New("database is locked"),
	}, nil, Options{})
	if degraded.Status != repoindex.StatusDegraded || degraded.Detail != "database is locked" {
		t.Fatalf("failing index = %+v", degraded)
	}

	pending := Build(context.Background(), &stubIndex{
		snapshot: repoindex.Snapshot{Status: repoindex.StatusPending, Detail: "builds on first use"},
	}, nil, Options{})
	if pending.Status != repoindex.StatusPending || pending.Detail != "builds on first use" {
		t.Fatalf("pending index = %+v", pending)
	}
}

func TestBuildPrefersRankOverDeclarationCountWhenChoosingDirectories(t *testing.T) {
	files := map[string]repoindex.File{}
	// A generated directory holds many declarations but no rank; two wired
	// directories hold fewer declarations but carry graph rank. The limit is
	// two directories: rank decides membership, and only within equal rank
	// does the declaration count speak.
	for index := 0; index < 6; index++ {
		path := "generated/file" + string(rune('a'+index)) + ".go"
		files[path] = repoindex.File{Path: path, Language: "go", SymbolCount: 40,
			EntryPoint: repoindex.EntryPoint(path)}
	}
	files["app/main.go"] = repoindex.File{Path: "app/main.go", Language: "go",
		SymbolCount: 2, Rank: 0.5, EntryPoint: true}
	files["core/core.go"] = repoindex.File{Path: "core/core.go", Language: "go",
		SymbolCount: 3, Rank: 0.3}

	built := Build(context.Background(), readyIndex(files, nil), nil,
		Options{MaxDirectories: 2})
	if !built.Ready() {
		t.Fatalf("status = %q", built.Status)
	}
	if len(built.Directories) != 2 {
		t.Fatalf("directories = %v", built.Directories)
	}
	if built.Directories[0].Path != "app" || built.Directories[1].Path != "core" {
		t.Fatalf("ranked directories = %v, want app and core", built.Directories)
	}
	if built.OmittedDirectories != 1 {
		t.Fatalf("omitted = %d, want the generated directory cut", built.OmittedDirectories)
	}
}

func TestBuildFallsBackToDeclarationCountWithoutRanks(t *testing.T) {
	files := map[string]repoindex.File{
		"small/lib.go": {Path: "small/lib.go", Language: "go", SymbolCount: 1},
		"large/gen.go": {Path: "large/gen.go", Language: "go", SymbolCount: 99},
	}
	// No ranks at all — the graph never ran. Declaration count is the
	// previous behaviour and must remain the tiebreaker.
	built := Build(context.Background(), readyIndex(files, nil), nil,
		Options{MaxDirectories: 1})
	if len(built.Directories) != 1 || built.Directories[0].Path != "large" {
		t.Fatalf("directories = %v, want large by declaration count", built.Directories)
	}
}

// A map build refreshes once (its Files call) and pins that snapshot for the
// outline query: the second query must not take the refreshing path.
func TestBuildPinsItsSnapshotForOutlines(t *testing.T) {
	index := readyIndex(
		map[string]repoindex.File{
			"api.go": {Path: "api.go", Language: "go", SymbolCount: 1},
		},
		[]repoindex.Symbol{{Path: "api.go", Name: "Serve", Line: 3}},
	)
	result := Build(
		t.Context(),
		index,
		[]string{"api.go"},
		Options{Depth: DefaultDepth, MaxDirectories: 4, MaxEntryPoints: 2},
	)
	if !result.Ready() {
		t.Fatalf("map = %+v", result)
	}
	if len(result.Outlines) != 1 || len(result.Outlines[0].Symbols) != 1 {
		t.Fatalf("outlines = %+v", result.Outlines)
	}
	if index.refreshingQueries != 0 {
		t.Fatalf(
			"outline query refreshed %d times, want the pinned snapshot path",
			index.refreshingQueries,
		)
	}
}
