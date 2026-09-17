package repoindex

import (
	"context"
	"github.com/fwtllh-png/QCode/internal/platform/symbols"
	"path"
	"sort"
)

// The reference graph is derived twice over: once from the import specifiers
// the extractor records, once from the identifier counts meeting the symbol
// table. Both feeds become edges in one table, and the file ranks the map
// consumes are a personalized PageRank over it.
//
// Determinism is part of the contract: nodes iterate in path order, edges are
// built in a fixed order, and the float accumulation follows that order, so
// the same repository yields the same ranks on every build.

// Edge kinds recorded in repo_index_edges.
const (
	EdgeImport           = "import"
	EdgeReference        = "reference"
	EdgeImportReference  = symbols.ReferenceImport
	EdgePackageReference = symbols.ReferencePackage
)

// graphEdge is one directed edge: src depends on dst, so dst's rank grows.
type graphEdge struct {
	Src    string
	Dst    string
	Kind   string
	Weight float64
}

// RankOptions bound the PageRank computation.
type RankOptions struct {
	// Damping is the probability a walk follows an edge instead of jumping
	// back to a seed. The default is the standard value from the original
	// PageRank paper (Brin & Page, 1998).
	Damping float64
	// Iterations bounds the refinement loop.
	Iterations int
	// Convergence stops the loop once the total movement falls below it.
	Convergence float64
}

// Defaults for rank options left unset.
const (
	DefaultRankDamping     = 0.85
	DefaultRankIterations  = 100
	DefaultRankConvergence = 1e-6
)

func (o RankOptions) withDefaults() RankOptions {
	if o.Damping <= 0 || o.Damping >= 1 {
		o.Damping = DefaultRankDamping
	}
	if o.Iterations <= 0 {
		o.Iterations = DefaultRankIterations
	}
	if o.Convergence <= 0 {
		o.Convergence = DefaultRankConvergence
	}
	return o
}

// rebuildGraph resolves the recorded import specifiers into edges, adds the
// reference edges the identifier counts imply, computes file ranks, and
// replaces the stored edge and rank rows for the root. It runs at the end of a
// refresh, when the file set is complete; a failure degrades the ranks (the
// map falls back to declaration counts) without failing the refresh.
func (i *Index) rebuildGraph(ctx context.Context, files map[string]File) {
	edges := i.importEdges(ctx, files)
	edges = append(edges, i.store.referenceEdges(ctx)...)
	scoped, err := i.store.scopedReferenceEdges(ctx)
	if err != nil {
		return
	}
	edges = append(edges, scoped...)
	if err := i.store.ReplaceEdges(ctx, edges); err != nil {
		return
	}
	ranks := personalPageRank(files, edges, i.options.Rank)
	if err := i.store.ReplaceRanks(ctx, ranks); err != nil {
		// The edges stay: consumers of the graph (impact analysis) do not
		// depend on the ranks succeeding.
		return
	}
}

// importEdges resolves every recorded specifier against the file set the
// refresh just confirmed. A Go import names a package directory, so its
// resolution fans out to every indexed file of that package; every other
// language resolves to at most a handful of candidate files. Candidates that
// name nothing indexed record nothing.
func (i *Index) importEdges(ctx context.Context, files map[string]File) []graphEdge {
	specs, err := i.store.Imports(ctx)
	if err != nil {
		return nil
	}
	// Package directories once, so a Go import's fan-out costs a map lookup
	// rather than a scan per specifier.
	packageDirs := make(map[string][]string)
	for _, candidate := range sortedFileKeys(files) {
		if files[candidate].Language != "go" {
			continue
		}
		directory := path.Dir(candidate)
		packageDirs[directory] = append(packageDirs[directory], candidate)
	}
	seen := make([]graphEdge, 0, len(specs)*2)
	for _, source := range sortedPaths(specs) {
		language := files[source].Language
		for _, spec := range specs[source] {
			resolveImport(language, spec, source, func(candidate string) {
				if candidate == source {
					return
				}
				if _, indexed := files[candidate]; indexed {
					seen = append(seen, graphEdge{
						Src: source, Dst: candidate, Kind: EdgeImport, Weight: 1,
					})
					return
				}
				if language != "go" {
					return
				}
				for _, member := range packageDirs[candidate] {
					if member == source {
						continue
					}
					seen = append(seen, graphEdge{
						Src: source, Dst: member, Kind: EdgeImport, Weight: 1,
					})
				}
			})
		}
	}
	// Resolution may emit the same pair several times (several suffixes hit,
	// or a Go package import fanning out); one edge per pair keeps the graph
	// and the stored table in one shape.
	deduplicated := make([]graphEdge, 0, len(seen))
	pair := make(map[string]struct{}, len(seen))
	for _, edge := range seen {
		key := edge.Src + "\x00" + edge.Dst
		if _, duplicate := pair[key]; duplicate {
			continue
		}
		pair[key] = struct{}{}
		deduplicated = append(deduplicated, edge)
	}
	return deduplicated
}

func sortedFileKeys(files map[string]File) []string {
	paths := make([]string, 0, len(files))
	for candidate := range files {
		paths = append(paths, candidate)
	}
	sort.Strings(paths)
	return paths
}

func sortedPaths(specs map[string][]string) []string {
	paths := make([]string, 0, len(specs))
	for path := range specs {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

// personalPageRank ranks files by how much a walk that restarts from the seed
// files reaches them. Seeds are the recognized entry points; a repository with
// none — a library — falls back to a plain uniform PageRank, under which the
// most depended-upon files rank highest, which is the same question asked
// without a starting point.
func personalPageRank(files map[string]File, edges []graphEdge, options RankOptions) map[string]float64 {
	options = options.withDefaults()
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		return nil
	}

	// Adjacency in path order: per node, the destinations its edges reach and
	// the weight each edge carries. Parallel edges (an import plus a
	// reference between the same pair) stay separate — the walk may follow
	// either, and weight is the reference count.
	adjacency := make(map[string][]graphEdge, len(paths))
	for _, edge := range edges {
		if _, src := files[edge.Src]; !src {
			continue
		}
		if _, dst := files[edge.Dst]; !dst {
			continue
		}
		adjacency[edge.Src] = append(adjacency[edge.Src], edge)
	}
	weightOf := make(map[string]float64, len(adjacency))
	for source, outgoing := range adjacency {
		total := 0.0
		for _, edge := range outgoing {
			total += edge.Weight
		}
		weightOf[source] = total
	}

	personalization := make(map[string]float64, len(paths))
	seeds := make([]string, 0, len(paths))
	for _, candidate := range paths {
		if files[candidate].EntryPoint {
			seeds = append(seeds, candidate)
		}
	}
	if len(seeds) == 0 {
		share := 1 / float64(len(paths))
		for _, candidate := range paths {
			personalization[candidate] = share
		}
	} else {
		share := 1 / float64(len(seeds))
		for _, seed := range seeds {
			personalization[seed] = share
		}
	}

	rank := make(map[string]float64, len(paths))
	next := make(map[string]float64, len(paths))
	for _, candidate := range paths {
		rank[candidate] = personalization[candidate]
		next[candidate] = 0
	}
	for iteration := 0; iteration < options.Iterations; iteration++ {
		dangling := 0.0
		for _, candidate := range paths {
			total, hasEdges := weightOf[candidate]
			if !hasEdges {
				// A dead end keeps whatever incoming contributions later
				// iterations write; only its own outgoing share is absent.
				dangling += rank[candidate]
				continue
			}
			for _, edge := range adjacency[candidate] {
				next[edge.Dst] += rank[candidate] * edge.Weight / total
			}
		}
		// A walk that hits a dead end restarts from the seeds, which keeps the
		// total mass at one and the ranking comparable between graphs.
		if dangling != 0 {
			for _, candidate := range paths {
				next[candidate] += dangling * personalization[candidate]
			}
		}
		movement := 0.0
		for _, candidate := range paths {
			next[candidate] = (1-options.Damping)*personalization[candidate] +
				options.Damping*next[candidate]
			movement += abs(next[candidate] - rank[candidate])
			rank[candidate] = 0
		}
		rank, next = next, rank
		if movement < options.Convergence {
			break
		}
	}
	return rank
}

func abs(value float64) float64 {
	if value < 0 {
		return -value
	}
	return value
}
