// Repository map projection turns the repository index into the orientation an agent needs
// before it starts looking: which directories hold code, how the project is
// built, where it starts, and what the files currently in play declare.
//
// It is deliberately a summary. The index knows every file and every
// declaration; a prompt can afford neither, so the map reports shape and counts
// and leaves reading the code to the search and read tools.
package repository

import (
	"context"
	"path"
	"sort"
	"strings"

	"github.com/fwtllh-png/QCode/internal/persist/repoindex"
)

// Index is what a map needs from the repository index. *repoindex.Index
// satisfies it; tests and degraded sessions can supply anything else.
type Index interface {
	Files(context.Context) (map[string]repoindex.File, repoindex.Snapshot, error)
	Symbols(context.Context, repoindex.Query) ([]repoindex.Symbol, repoindex.Snapshot, error)
	// SymbolsFrom queries rows a snapshot this chain already confirmed, so one
	// build refreshes once and queries many times.
	SymbolsFrom(context.Context, repoindex.Snapshot, repoindex.Query) ([]repoindex.Symbol, repoindex.Snapshot, error)
}

// Defaults for options a caller leaves unset.
const (
	// DefaultDepth groups files two segments deep. One segment is useless in a
	// repository that keeps everything under internal/ or src/; two separates
	// the subsystems without listing every leaf.
	DefaultDepth             = 2
	DefaultMaxDirectories    = 24
	DefaultMaxEntryPoints    = 6
	DefaultMaxOutlineFiles   = 8
	DefaultMaxOutlineSymbols = 12
)

// Options bound how much of the repository the map describes.
type Options struct {
	Depth             int
	MaxDirectories    int
	MaxEntryPoints    int
	MaxOutlineFiles   int
	MaxOutlineSymbols int
}

func (o Options) normalized() Options {
	if o.Depth <= 0 {
		o.Depth = DefaultDepth
	}
	if o.MaxDirectories <= 0 {
		o.MaxDirectories = DefaultMaxDirectories
	}
	if o.MaxEntryPoints <= 0 {
		o.MaxEntryPoints = DefaultMaxEntryPoints
	}
	if o.MaxOutlineFiles <= 0 {
		o.MaxOutlineFiles = DefaultMaxOutlineFiles
	}
	if o.MaxOutlineSymbols <= 0 {
		o.MaxOutlineSymbols = DefaultMaxOutlineSymbols
	}
	return o
}

// Directory is one grouping of files with how much code it holds.
type Directory struct {
	Path    string `json:"path"`
	Files   int    `json:"files"`
	Symbols int    `json:"symbols"`
	// Languages are the most common languages in the directory.
	Languages []string `json:"languages,omitempty"`
}

// Outline is the declarations of one file the agent is currently working on.
type Outline struct {
	Path    string             `json:"path"`
	Symbols []repoindex.Symbol `json:"symbols"`
	// Truncated marks a file with more declarations than the outline shows.
	Truncated bool `json:"truncated,omitempty"`
}

// Map is the assembled summary. A map whose Status is not ready carries Detail
// instead of contents, so a reader is told the difference between "no such code"
// and "the index could not answer".
type Map struct {
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`

	FileCount   int `json:"file_count"`
	SymbolCount int `json:"symbol_count"`

	// Build lists the build and dependency manifests found at the root.
	Build []string `json:"build,omitempty"`
	// Entries are likely program entry points.
	Entries []string `json:"entries,omitempty"`

	Directories []Directory `json:"directories,omitempty"`
	// OmittedDirectories counts the directories the limit cut.
	OmittedDirectories int `json:"omitted_directories,omitempty"`

	Outlines []Outline `json:"outlines,omitempty"`
}

// Ready reports whether the map describes the repository rather than a failure.
func (m Map) Ready() bool { return m.Status == repoindex.StatusReady }

// Build assembles the map. focus are the paths currently in the working set,
// whose declarations become the outline section.
//
// It never fails: an index that cannot answer produces a map that says so, and
// the caller keeps its turn.
func Build(ctx context.Context, index Index, focus []string, options Options) Map {
	options = options.normalized()
	if index == nil {
		return Map{Status: repoindex.StatusDisabled, Detail: "no repository index is configured"}
	}
	files, snapshot, err := index.Files(ctx)
	switch {
	case err != nil:
		return Map{Status: repoindex.StatusDegraded, Detail: err.Error()}
	case !snapshot.Ready():
		return Map{Status: snapshot.Status, Detail: snapshot.Detail}
	}

	result := Map{Status: repoindex.StatusReady, FileCount: len(files)}
	grouped := make(map[string]*Directory, len(files))
	languages := make(map[string]map[string]int, len(files))
	// Path order keeps the float sums below deterministic: the same
	// repository must aggregate to the same scores, or the ranking is not a
	// ranking but a coin flip between builds.
	ordered := make([]string, 0, len(files))
	for candidate := range files {
		ordered = append(ordered, candidate)
	}
	sort.Strings(ordered)
	scores := make(map[string]float64, len(grouped))
	for _, candidate := range ordered {
		file := files[candidate]
		result.SymbolCount += file.SymbolCount
		if manifest, found := buildManifest(file.Path); found {
			result.Build = append(result.Build, manifest)
		}
		if file.EntryPoint {
			result.Entries = append(result.Entries, file.Path)
		}
		key := group(file.Path, options.Depth)
		directory, known := grouped[key]
		if !known {
			directory = &Directory{Path: key}
			grouped[key] = directory
			languages[key] = make(map[string]int)
		}
		directory.Files++
		directory.Symbols += file.SymbolCount
		scores[key] += file.Rank
		if file.Language != "" {
			languages[key][file.Language]++
		}
	}

	sort.Strings(result.Build)
	sort.Strings(result.Entries)
	if len(result.Entries) > options.MaxEntryPoints {
		result.Entries = result.Entries[:options.MaxEntryPoints]
	}

	directories := make([]Directory, 0, len(grouped))
	for key, directory := range grouped {
		directory.Languages = topLanguages(languages[key])
		directories = append(directories, *directory)
	}
	// The graph decides what survives the limit: a directory whose files a
	// walk from the entry points reaches carries more rank than one that
	// merely holds many declarations. Rank is zero before a graph build has
	// run, and the comparison below then falls through to declaration count —
	// the previous behaviour, kept as the fallback rather than as a second
	// ranking to configure.
	sort.Slice(directories, func(i, j int) bool {
		left, right := directories[i], directories[j]
		if scores[left.Path] != scores[right.Path] {
			return scores[left.Path] > scores[right.Path]
		}
		if left.Symbols != right.Symbols {
			return left.Symbols > right.Symbols
		}
		if left.Files != right.Files {
			return left.Files > right.Files
		}
		return left.Path < right.Path
	})
	if len(directories) > options.MaxDirectories {
		result.OmittedDirectories = len(directories) - options.MaxDirectories
		directories = directories[:options.MaxDirectories]
	}
	// Read as a listing rather than a ranking, so path order is what a reader
	// wants once the ranking has chosen the members.
	sort.Slice(directories, func(i, j int) bool { return directories[i].Path < directories[j].Path })
	result.Directories = directories
	result.Outlines = outlines(ctx, index, snapshot, focus, options)
	return result
}

// outlines lists the declarations of the focused files against the snapshot the
// build already confirmed — the query must not trigger a second refresh of the
// same state. A file the index does not know simply contributes nothing: the
// working set section still names it.
func outlines(
	ctx context.Context,
	index Index,
	snapshot repoindex.Snapshot,
	focus []string,
	options Options,
) []Outline {
	paths := make([]string, 0, len(focus))
	seen := make(map[string]struct{}, len(focus))
	for _, candidate := range focus {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if _, found := seen[candidate]; found {
			continue
		}
		seen[candidate] = struct{}{}
		paths = append(paths, candidate)
		if len(paths) == options.MaxOutlineFiles {
			break
		}
	}
	if len(paths) == 0 {
		return nil
	}
	limit := options.MaxOutlineFiles * (options.MaxOutlineSymbols + 1)
	found, _, err := index.SymbolsFrom(
		ctx,
		snapshot,
		repoindex.Query{Paths: paths, Limit: limit},
	)
	if err != nil || len(found) == 0 {
		return nil
	}
	byPath := make(map[string][]repoindex.Symbol, len(paths))
	for _, symbol := range found {
		byPath[symbol.Path] = append(byPath[symbol.Path], symbol)
	}
	result := make([]Outline, 0, len(byPath))
	for _, candidate := range paths {
		symbols, known := byPath[candidate]
		if !known {
			continue
		}
		sort.Slice(symbols, func(i, j int) bool {
			if symbols[i].Line != symbols[j].Line {
				return symbols[i].Line < symbols[j].Line
			}
			return symbols[i].Name < symbols[j].Name
		})
		outline := Outline{Path: candidate}
		if len(symbols) > options.MaxOutlineSymbols {
			outline.Symbols = symbols[:options.MaxOutlineSymbols]
			outline.Truncated = true
		} else {
			outline.Symbols = symbols
		}
		result = append(result, outline)
	}
	return result
}

// group returns the directory a path is summarized under.
func group(candidate string, depth int) string {
	directory := path.Dir(candidate)
	if directory == "." || directory == "/" {
		return "."
	}
	segments := strings.Split(directory, "/")
	if len(segments) > depth {
		segments = segments[:depth]
	}
	return strings.Join(segments, "/")
}

// buildManifest reports a manifest at the repository root. A manifest deeper in
// the tree describes a subpackage, which is more detail than orientation needs.
//
// The list of manifest names lives in the index, which also classifies paths for
// the evidence ledger; sharing it keeps the map and the ledger from calling the
// same file different things.
func buildManifest(candidate string) (string, bool) {
	if strings.Contains(candidate, "/") {
		return "", false
	}
	if repoindex.IsBuildManifest(candidate) {
		return candidate, true
	}
	return "", false
}

// topLanguages returns at most two languages, most files first.
func topLanguages(counts map[string]int) []string {
	if len(counts) == 0 {
		return nil
	}
	names := make([]string, 0, len(counts))
	for name := range counts {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if counts[names[i]] != counts[names[j]] {
			return counts[names[i]] > counts[names[j]]
		}
		return names[i] < names[j]
	})
	return names[:min(2, len(names))]
}
