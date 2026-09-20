package prompt

import (
	"bytes"
	"context"
	"os"
	"path"
	"path/filepath"
	"sync"

	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/repository"
)

// RepositoryOptions configures the volatile repository context appended to a
// model request.
type RepositoryOptions struct {
	RepoMap    bool
	WorkingSet bool
	Evidence   bool
	Map        repository.Options
	Budgets    map[string]Budget
	Tokens     TokenCounter
	// Root is the absolute workspace root used to read directory-level
	// instruction files. Empty disables the directory layer.
	Root string
}

// DirectoryInstruction is one sub-root instruction file that governs a
// directory this session is working in. Root-level rules are not repeated
// here: they are already part of the stable prefix.
type DirectoryInstruction struct {
	Dir  string `json:"dir"`
	Path string `json:"path"`
	Text string `json:"text"`
}

// directoryInstructionNames are probed per directory; the first file found
// wins, so a directory holding both an AGENTS.md and a CLAUDE.md injects only
// the AGENTS.md family file.
var directoryInstructionNames = []string{"AGENTS.md", "CLAUDE.md"}

// RepositoryProvider renders the repository map, working set, and evidence
// while caching the expensive map build once per turn.
type RepositoryProvider struct {
	index   repository.Index
	options RepositoryOptions

	mu       sync.Mutex
	mapTurn  uint64
	repoMap  repository.Map
	mapKnown bool
}

func NewRepositoryProvider(
	index repository.Index,
	options RepositoryOptions,
) *RepositoryProvider {
	return &RepositoryProvider{index: index, options: options}
}

// Build renders the volatile context for one sample. An unavailable index is
// represented in the context instead of failing the turn.
func (p *RepositoryProvider) Build(
	ctx context.Context,
	state TurnState,
) TurnContext {
	if p == nil {
		return TurnContext{}
	}
	turnOptions := TurnOptions{
		Turn: state.Turn, Budgets: p.options.Budgets, Tokens: p.options.Tokens,
	}
	if p.options.WorkingSet {
		turnOptions.WorkingSet = state.WorkingSet
	}
	if p.options.Evidence {
		turnOptions.Evidence = state.Evidence
	}
	if p.options.RepoMap {
		turnOptions.RepoMap = p.mapFor(ctx, state.Turn, state.WorkingSet)
	}
	if p.options.Root != "" && p.options.WorkingSet {
		turnOptions.DirectoryInstructions = directoryInstructions(
			p.options.Root,
			state.WorkingSet,
		)
	}
	return AssembleTurn(turnOptions)
}

// directoryInstructions selects, for each working-set path, the instruction
// file of the nearest ancestor directory below the workspace root. Reading a
// handful of small files per sample keeps the selection stateless: there is
// no cache whose invalidation could silently hide an edited rule.
func directoryInstructions(
	root string,
	entries []agentcontext.WorkingSetEntry,
) []DirectoryInstruction {
	var result []DirectoryInstruction
	seen := make(map[string]struct{})
	for _, entry := range entries {
		directory := path.Dir(filepath.ToSlash(entry.Path))
		for directory != "." && directory != "/" {
			relative := ""
			var content []byte
			for _, name := range directoryInstructionNames {
				candidate := path.Join(directory, name)
				data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(candidate)))
				if err != nil || len(bytes.TrimSpace(data)) == 0 {
					continue
				}
				relative, content = candidate, data
				break
			}
			if relative != "" {
				if _, duplicate := seen[relative]; !duplicate {
					seen[relative] = struct{}{}
					result = append(result, DirectoryInstruction{
						Dir: directory, Path: relative,
						Text: string(bytes.TrimSpace(content)) + "\n",
					})
				}
				break
			}
			directory = path.Dir(directory)
		}
	}
	return result
}

func (p *RepositoryProvider) mapFor(
	ctx context.Context,
	turn uint64,
	entries []agentcontext.WorkingSetEntry,
) repository.Map {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.mapKnown && p.mapTurn == turn {
		return p.repoMap
	}
	focus := make([]string, 0, len(entries))
	for _, entry := range entries {
		focus = append(focus, entry.Path)
	}
	p.repoMap = repository.Build(ctx, p.index, focus, p.options.Map)
	p.mapTurn, p.mapKnown = turn, true
	return p.repoMap
}
