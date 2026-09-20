package repoindex

import (
	"math"
	"sort"
	"strings"
	"testing"
)

// The graph tests cover the layers M2 added: specifier parsing per language,
// resolution against a file set, the rank computation's determinism and seed
// behaviour, and the end-to-end path from files on disk to ranks the map reads.

func TestParseImportsPerLanguage(t *testing.T) {
	for _, test := range []struct {
		name     string
		language string
		source   string
		want     []string
	}{
		{
			name: "go single and group", language: "go",
			source: "package a\n\nimport \"fmt\"\n\nimport (\n\t\"os\"\n\t_ \"embed\"\n)\n",
			want:   []string{"fmt", "os", "embed"},
		},
		{
			name: "script from and require", language: "typescript",
			source: "import { a } from \"./x\";\nconst b = require(\"../y\");\nimport \"./side\";\nimport z from \"pkg\";\n",
			want:   []string{"./x", "../y", "./side", "pkg"},
		},
		{
			name: "python plain, from and relative", language: "python",
			source: "import os\nimport a.b, c\nfrom .sibling import x\nfrom ..pkg import y\n",
			want:   []string{"os", "a.b", "c", ".sibling", "..pkg"},
		},
		{
			name: "rust crate paths only", language: "rust",
			source: "use crate::engine::run;\nuse std::io;\npub use crate::util;\n",
			want:   []string{"crate::engine::run", "crate::util"},
		},
		{
			name: "java class imports", language: "java",
			source: "import java.util.List;\nimport static org.assertj.core.api.Assertions.assertThat;\n",
			// A static import's specifier carries the member name; the
			// resolver offers the class-only candidate when resolving.
			want: []string{"java.util.List", "org.assertj.core.api.Assertions.assertThat"},
		},
		{
			name: "c quoted includes only", language: "cpp",
			source: "#include \"store.h\"\n#include <vector>\n#include \"nested/util.hpp\"\n",
			want:   []string{"store.h", "nested/util.hpp"},
		},
	} {
		found := parseImports(test.language, []byte(test.source))
		if len(found) != len(test.want) {
			t.Fatalf("%s: imports = %v, want %v", test.name, found, test.want)
		}
		for index := range test.want {
			if found[index] != test.want[index] {
				t.Fatalf("%s: import %d = %q, want %q", test.name, index, found[index], test.want[index])
			}
		}
	}
}

func TestResolveImportOnlyNamesIndexedFiles(t *testing.T) {
	files := map[string]File{
		"internal/store/store.go": {Path: "internal/store/store.go", Language: "go"},
		"internal/store/query.go": {Path: "internal/store/query.go", Language: "go"},
		"web/app.ts":              {Path: "web/app.ts", Language: "typescript"},
		"web/util.ts":             {Path: "web/util.ts", Language: "typescript"},
		"web/components/index.ts": {Path: "web/components/index.ts", Language: "typescript"},
		"pkg/service/__init__.py": {Path: "pkg/service/__init__.py", Language: "python"},
		"src/engine/mod.rs":       {Path: "src/engine/mod.rs", Language: "rust"},
		"com/app/Main.java":       {Path: "com/app/Main.java", Language: "java"},
		"include/util.hpp":        {Path: "include/util.hpp", Language: "cpp"},
	}
	for _, test := range []struct {
		name     string
		language string
		spec     string
		from     string
		want     []string
	}{
		{
			name: "go package fans out", language: "go",
			spec: "example.com/app/internal/store", from: "cmd/main.go",
			want: []string{"internal/store/query.go", "internal/store/store.go"},
		},
		{
			name: "script relative with suffix", language: "typescript",
			spec: "./util", from: "web/app.ts",
			want: []string{"web/util.ts"},
		},
		{
			name: "script directory index", language: "typescript",
			spec: "./components", from: "web/app.ts",
			want: []string{"web/components/index.ts"},
		},
		{
			name: "python package init", language: "python",
			spec: "pkg.service", from: "main.py",
			want: []string{"pkg/service/__init__.py"},
		},
		{
			name: "rust crate module", language: "rust",
			spec: "crate::engine", from: "src/main.rs",
			want: []string{"src/engine/mod.rs"},
		},
		{
			name: "java dotted class", language: "java",
			spec: "com.app.Main", from: "com/app/Other.java",
			want: []string{"com/app/Main.java"},
		},
		{
			name: "cpp include beside the file", language: "cpp",
			spec: "../include/util.hpp", from: "src/engine.cpp",
			want: []string{"include/util.hpp"},
		},
		{
			name: "external package names nothing", language: "typescript",
			spec: "react", from: "web/app.ts",
			want: nil,
		},
	} {
		resolvedSet := map[string]struct{}{}
		resolveImport(test.language, test.spec, test.from, func(candidate string) {
			if _, indexed := files[candidate]; indexed {
				resolvedSet[candidate] = struct{}{}
				return
			}
			// A Go specifier may resolve to a package directory: every file
			// under it is a member, as the graph build itself expands.
			for member := range files {
				if strings.HasPrefix(member, candidate+"/") {
					resolvedSet[member] = struct{}{}
				}
			}
		})
		resolved := make([]string, 0, len(resolvedSet))
		for member := range resolvedSet {
			resolved = append(resolved, member)
		}
		sort.Strings(resolved)
		if len(resolved) != len(test.want) {
			t.Fatalf("%s: resolved = %v, want %v", test.name, resolved, test.want)
		}
		for index := range test.want {
			if resolved[index] != test.want[index] {
				t.Fatalf("%s: resolution %d = %q, want %q",
					test.name, index, resolved[index], test.want[index])
			}
		}
	}
}

func TestPersonalPageRankIsDeterministicAndSeeded(t *testing.T) {
	files := map[string]File{
		"main.go":      {Path: "main.go", Language: "go", EntryPoint: true},
		"core.go":      {Path: "core.go", Language: "go"},
		"orphan.go":    {Path: "orphan.go", Language: "go"},
		"unrelated.go": {Path: "unrelated.go", Language: "go"},
	}
	edges := []graphEdge{
		{Src: "main.go", Dst: "core.go", Kind: EdgeImport, Weight: 1},
		{Src: "core.go", Dst: "main.go", Kind: EdgeReference, Weight: 2},
	}
	options := RankOptions{Damping: 0.85, Iterations: 100, Convergence: 1e-6}
	first := personalPageRank(files, edges, options)
	// Same inputs, repeatedly: the aggregation order is fixed, so the floats
	// land identically every time — a ranking that drifts between builds is
	// not a ranking.
	for attempt := 0; attempt < 8; attempt++ {
		again := personalPageRank(files, edges, options)
		for path, rank := range first {
			if again[path] != rank {
				t.Fatalf("rank of %s drifted: %v vs %v", path, rank, again[path])
			}
		}
	}
	// The seeded walk reaches the wired files; the disconnected one does not.
	if first["orphan.go"] >= first["core.go"] {
		t.Fatalf("unreached file outranked a wired one: %v vs %v",
			first["orphan.go"], first["core.go"])
	}
	if first["unrelated.go"] != first["orphan.go"] {
		t.Fatalf("two unreached files hold different rank: %v vs %v",
			first["unrelated.go"], first["orphan.go"])
	}
	total := 0.0
	for _, rank := range first {
		total += rank
	}
	if math.Abs(total-1) > 1e-6 {
		t.Fatalf("ranks sum to %v, want 1", total)
	}
}

func TestPersonalPageRankFallsBackToUniformWithoutSeeds(t *testing.T) {
	files := map[string]File{
		"hub.go":   {Path: "hub.go", Language: "go"},
		"spoke.go": {Path: "spoke.go", Language: "go"},
	}
	edges := []graphEdge{
		{Src: "spoke.go", Dst: "hub.go", Kind: EdgeReference, Weight: 1},
	}
	ranks := personalPageRank(files, edges, RankOptions{})
	if ranks["hub.go"] <= ranks["spoke.go"] {
		t.Fatalf("depended-upon file should outrank its dependent: %v vs %v",
			ranks["hub.go"], ranks["spoke.go"])
	}
}

func TestEnsureBuildsGraphAndRanksFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "main.go", "package main\n\nimport \"example.com/app/core\"\n\nfunc main() { core.Run() }\n")
	writeFile(t, root, "core/core.go", "package core\n\n// Run does the work.\nfunc Run() error { return nil }\n")
	writeFile(t, root, "misc/notes.md", "# notes\n")
	index, store := newIndex(t, root, Options{})

	snapshot, err := ensureSettled(t, index)
	if err != nil || !snapshot.Ready() {
		t.Fatalf("ensure: %v %+v", err, snapshot)
	}
	files, err := store.Files(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !files["main.go"].EntryPoint {
		t.Fatalf("main.go not classified as an entry point: %+v", files["main.go"])
	}
	if files["main.go"].Rank <= 0 || files["core/core.go"].Rank <= 0 {
		t.Fatalf("ranks missing after graph build: main=%v core=%v",
			files["main.go"].Rank, files["core/core.go"].Rank)
	}
	if files["core/core.go"].Rank <= files["misc/notes.md"].Rank {
		t.Fatalf("wired code should outrank a disconnected note: %v vs %v",
			files["core/core.go"].Rank, files["misc/notes.md"].Rank)
	}

	edges, err := store.Edges(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// main.go imports the core package and references core.Run; both edges
	// should exist, the import by resolution and the reference by its scoped import binding.
	kinds := map[string]string{}
	for _, edge := range edges {
		if edge.Src == "main.go" && edge.Dst == "core/core.go" {
			kinds[edge.Kind] = edge.Dst
		}
	}
	if _, imported := kinds[EdgeImport]; !imported {
		t.Fatalf("import edge missing: %+v", edges)
	}
	if _, referenced := kinds[EdgeImportReference]; !referenced {
		t.Fatalf("reference edge missing: %+v", edges)
	}
}

func TestRustGroupedUseResolvesSymbolToModule(t *testing.T) {
	candidates := map[string]bool{}
	resolveImport("rust", "crate::engine::Runner", "src/main.rs", func(p string) { candidates[p] = true })
	if !candidates["src/engine.rs"] || !candidates["src/engine/mod.rs"] {
		t.Fatalf("module candidates=%v", candidates)
	}
}
