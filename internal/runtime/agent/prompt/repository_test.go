package prompt

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/persist/repoindex"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
)

type countingRepositoryIndex struct {
	files int
}

func (c *countingRepositoryIndex) Files(
	context.Context,
) (map[string]repoindex.File, repoindex.Snapshot, error) {
	c.files++
	return map[string]repoindex.File{
		"internal/store/store.go": {
			Path: "internal/store/store.go", Language: "go", SymbolCount: 3,
		},
	}, repoindex.Snapshot{Status: repoindex.StatusReady}, nil
}

func (c *countingRepositoryIndex) Symbols(
	context.Context,
	repoindex.Query,
) ([]repoindex.Symbol, repoindex.Snapshot, error) {
	return nil, repoindex.Snapshot{Status: repoindex.StatusReady}, nil
}

func (c *countingRepositoryIndex) SymbolsFrom(
	_ context.Context,
	snapshot repoindex.Snapshot,
	_ repoindex.Query,
) ([]repoindex.Symbol, repoindex.Snapshot, error) {
	return nil, snapshot, nil
}

func repositoryEntries(paths ...string) []agentcontext.WorkingSetEntry {
	result := make([]agentcontext.WorkingSetEntry, 0, len(paths))
	for _, path := range paths {
		result = append(result, agentcontext.WorkingSetEntry{
			Path: path, Sources: []agentcontext.WorkingSetSource{agentcontext.SourceRead}, LastTurn: 1,
		})
	}
	return result
}

func repositoryState(turn uint64, paths ...string) TurnState {
	return TurnState{Turn: turn, WorkingSet: repositoryEntries(paths...)}
}

func repositoryText(assembled TurnContext) string {
	var builder strings.Builder
	for _, message := range assembled.Messages {
		builder.WriteString(message.Text())
	}
	return builder.String()
}

func TestRepositoryProviderCachesMapPerTurnAndRendersPerSample(t *testing.T) {
	index := &countingRepositoryIndex{}
	provider := NewRepositoryProvider(index, RepositoryOptions{
		RepoMap: true, WorkingSet: true,
	})

	first := provider.Build(context.Background(), repositoryState(1, "a.go"))
	second := provider.Build(
		context.Background(),
		repositoryState(1, "a.go", "b.go"),
	)
	if index.files != 1 {
		t.Fatalf("index reads = %d, want one per turn", index.files)
	}
	if strings.Contains(repositoryText(first), "b.go") ||
		!strings.Contains(repositoryText(second), "b.go") {
		t.Fatal("the working set must follow the sample, not the cached map")
	}

	provider.Build(context.Background(), repositoryState(2, "a.go"))
	if index.files != 2 {
		t.Fatalf("index reads = %d, want a fresh read for the new turn", index.files)
	}
}

func TestRepositoryProviderHonorsDisabledSections(t *testing.T) {
	index := &countingRepositoryIndex{}
	mapOnly := NewRepositoryProvider(index, RepositoryOptions{RepoMap: true})
	assembled := mapOnly.Build(context.Background(), repositoryState(1, "a.go"))
	if body := repositoryText(assembled); !strings.Contains(body, "repo_map") ||
		strings.Contains(body, "working_set") {
		t.Fatalf("map-only tail = %q", body)
	}

	setOnly := NewRepositoryProvider(index, RepositoryOptions{WorkingSet: true})
	assembled = setOnly.Build(context.Background(), repositoryState(1, "a.go"))
	if body := repositoryText(assembled); strings.Contains(body, "repo_map") ||
		!strings.Contains(body, "working_set") {
		t.Fatalf("set-only tail = %q", body)
	}
	if index.files != 1 {
		t.Fatalf("index reads = %d, want none for a disabled map", index.files-1)
	}

	off := NewRepositoryProvider(nil, RepositoryOptions{})
	if assembled = off.Build(
		context.Background(),
		repositoryState(1),
	); len(assembled.Messages) != 0 {
		t.Fatalf("tail with both sections off = %+v", assembled.Messages)
	}
}

func TestRepositoryProviderCarriesEvidenceOnlyWhenEnabled(t *testing.T) {
	snapshot := agentcontext.EvidenceSnapshot{
		Turn: 1,
		Risks: []agentcontext.EvidenceRisk{{
			Kind: agentcontext.RiskUnverifiedChange, Path: "a.go", Turn: 1,
		}},
	}
	withEvidence := NewRepositoryProvider(nil, RepositoryOptions{Evidence: true})
	assembled := withEvidence.Build(context.Background(), TurnState{
		Turn: 1, Evidence: snapshot,
	})
	if !strings.Contains(repositoryText(assembled), "[evidence turn=1]") {
		t.Fatalf("tail = %q", repositoryText(assembled))
	}

	off := NewRepositoryProvider(nil, RepositoryOptions{WorkingSet: true})
	assembled = off.Build(context.Background(), TurnState{
		Turn: 1, WorkingSet: repositoryEntries("a.go"), Evidence: snapshot,
	})
	if strings.Contains(repositoryText(assembled), "evidence") {
		t.Fatalf("a disabled section was rendered: %q", repositoryText(assembled))
	}
}

func TestRepositoryProviderDegradesWithoutIndex(t *testing.T) {
	provider := NewRepositoryProvider(nil, RepositoryOptions{
		RepoMap: true, WorkingSet: true,
	})
	assembled := provider.Build(
		context.Background(),
		repositoryState(4, "a.go"),
	)
	body := repositoryText(assembled)
	if !strings.Contains(body, "No repository map is available") {
		t.Fatalf("tail = %q", body)
	}
	if !strings.Contains(body, "a.go") {
		t.Fatalf("tail = %q", body)
	}
}

func TestNilRepositoryProviderBuildsNothing(t *testing.T) {
	var provider *RepositoryProvider
	if assembled := provider.Build(
		context.Background(),
		repositoryState(1),
	); len(assembled.Messages) != 0 {
		t.Fatalf("assembled = %+v", assembled.Messages)
	}
}

// Directory rules follow the working set: the nearest ancestor instruction
// file below the root wins, the root's own file is not repeated, and two
// paths sharing a directory inject it once.
func TestDirectoryInstructionsSelectNearestPerWorkingSetPath(t *testing.T) {
	root := t.TempDir()
	write := func(relative, content string) {
		t.Helper()
		absolute := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(absolute), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(absolute, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("AGENTS.md", "root rules")
	write("a/AGENTS.md", "package a rules")
	write("a/b/CLAUDE.md", "subpackage b rules")

	provider := NewRepositoryProvider(nil, RepositoryOptions{
		WorkingSet: true, Root: root,
	})
	context := provider.Build(context.Background(), TurnState{
		Turn: 3,
		WorkingSet: []agentcontext.WorkingSetEntry{
			{Path: "a/b/c.go"}, {Path: "a/x.go"}, {Path: "a/b/d.go"},
		},
	})
	var text string
	for _, message := range context.Messages {
		text += message.Text() + "\n---\n"
	}
	if !strings.Contains(text, "[directory_rules]") ||
		!strings.Contains(text, "a/b — from a/b/CLAUDE.md:\nsubpackage b rules") ||
		!strings.Contains(text, "a — from a/AGENTS.md:\npackage a rules") {
		t.Fatalf("directory rules = %q", text)
	}
	if strings.Contains(text, "from AGENTS.md:\nroot rules") {
		t.Fatalf("root instruction leaked into directory layer: %q", text)
	}
	if strings.Count(text, "subpackage b rules") != 1 {
		t.Fatalf("shared directory injected twice: %q", text)
	}
	rulesSection := strings.SplitN(text, "\n---\n", 2)[0]
	if strings.Contains(rulesSection, "turn=3") {
		t.Fatalf("directory rules embed a turn number and bust the digest: %q", rulesSection)
	}
}

func TestDirectoryInstructionsRequireRoot(t *testing.T) {
	provider := NewRepositoryProvider(nil, RepositoryOptions{WorkingSet: true})
	context := provider.Build(context.Background(), TurnState{
		WorkingSet: []agentcontext.WorkingSetEntry{{Path: "a/b/c.go"}},
	})
	if strings.Contains(repositoryText(context), "[directory_rules]") {
		t.Fatalf("directory rules rendered without a root: %+v", context.Messages)
	}
	if instructions := directoryInstructions("", nil); instructions != nil {
		t.Fatalf("instructions = %+v", instructions)
	}
}
