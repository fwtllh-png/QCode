package repoindex

import (
	"context"
	"sort"
	"strings"
)

// Impact analysis walks the reference graph backwards from a change: a file is
// affected when it depends on a changed file, transitively, within a bounded
// number of hops. The walk answers "what else must be looked at" — callers,
// callers of callers, and the tests that exercise the change — which naming
// conventions cannot, because a test that imports a source file is a reference
// edge no file name reveals.

// ImpactHit is one file the walk reached.
type ImpactHit struct {
	Path string
	// Hops is the distance from the nearest changed path: 1 is a direct
	// dependent, 2 depends on that one, and so on.
	Hops int
	// Via is the kind of the last edge the walk followed — an import or a
	// reference — reported so a consumer can weigh a structural edge above a
	// name coincidence.
	Via   string
	Chain []ImpactStep
}

// ImpactStep follows impact outward: Dependent depends on Dependency.
// Evidence is one representative occurrence, not an exhaustive call chain.
type ImpactStep struct {
	Dependency string             `json:"dependency"`
	Dependent  string             `json:"dependent"`
	Kind       string             `json:"kind"`
	Evidence   *ReferenceRelation `json:"evidence,omitempty"`
}

type ImpactCoverage struct {
	Status                       string `json:"status"`
	IndexTruncated               bool   `json:"index_truncated"`
	ResultsTruncated             bool   `json:"results_truncated"`
	DepthTruncated               bool   `json:"depth_truncated"`
	MaxDepth                     int    `json:"max_depth"`
	MaxResults                   int    `json:"max_results"`
	GraphUnavailable             bool   `json:"graph_unavailable"`
	ReferenceEvidenceUnavailable bool   `json:"reference_evidence_unavailable"`
	ReferenceSitesTruncated      bool   `json:"reference_sites_truncated"`
}

// ImpactOptions bound the reverse walk.
type ImpactOptions struct {
	// MaxDepth is how many dependency hops a change may reach through.
	MaxDepth int
	// MaxResults bounds how many files one answer may name, so a change in a
	// repository's root package does not return the repository.
	MaxResults int
}

// Defaults for impact options left unset. Three hops covers the direct
// dependent, its dependents and one more layer — beyond that, a lexical graph's
// name-coincidence error accumulates faster than its recall improves.
const (
	DefaultImpactMaxDepth   = 3
	DefaultImpactMaxResults = 200
)

func (o ImpactOptions) withDefaults() ImpactOptions {
	if o.MaxDepth <= 0 {
		o.MaxDepth = DefaultImpactMaxDepth
	}
	if o.MaxResults <= 0 {
		o.MaxResults = DefaultImpactMaxResults
	}
	return o
}

// Impact returns the files that depend on the given paths, transitively, with
// the hop count and last edge kind of the shortest walk to each. The second
// result reports whether the answer was cut short by a depth or result bound: a
// truncated closure is still a closure's beginning, and the caller decides
// whether it is enough.
func (i *Index) Impact(ctx context.Context, paths []string) (map[string]ImpactHit, bool, error) {
	edges, err := i.store.Edges(ctx)
	if err != nil {
		return nil, false, err
	}
	hits, coverage, err := walkImpact(ctx, paths, edges, i.options.Impact.withDefaults(), nil)
	return hits, coverage.ResultsTruncated || coverage.DepthTruncated, err
}

func walkImpact(ctx context.Context, paths []string, edges []graphEdge, options ImpactOptions, evidence map[string]*ReferenceRelation) (map[string]ImpactHit, ImpactCoverage, error) {
	return walkReverseImpact(ctx, paths, impactReverse(edges), options, evidence)
}

func impactReverse(edges []graphEdge) map[string][]graphEdge {
	reverse := map[string][]graphEdge{}
	for _, edge := range edges {
		reverse[edge.Dst] = append(reverse[edge.Dst], edge)
	}
	for path := range reverse {
		sort.Slice(reverse[path], func(a, b int) bool {
			x, y := reverse[path][a], reverse[path][b]
			if x.Src != y.Src {
				return x.Src < y.Src
			}
			// Prefer occurrence-backed evidence when parallel edges join the same files.
			xs, ys := isScopedEdge(x.Kind), isScopedEdge(y.Kind)
			if xs != ys {
				return xs
			}
			return x.Kind < y.Kind
		})
	}
	return reverse
}

func walkReverseImpact(ctx context.Context, paths []string, reverse map[string][]graphEdge, options ImpactOptions, evidence map[string]*ReferenceRelation) (map[string]ImpactHit, ImpactCoverage, error) {
	found := map[string]ImpactHit{}
	coverage := ImpactCoverage{Status: "partial", MaxDepth: options.MaxDepth, MaxResults: options.MaxResults}
	roots := map[string]bool{}
	queue := make([]ImpactHit, 0, len(paths))
	sorted := append([]string(nil), paths...)
	sort.Strings(sorted)
	for _, path := range sorted {
		if !roots[path] {
			roots[path] = true
			queue = append(queue, ImpactHit{Path: path})
		}
	}
	for head := 0; head < len(queue); head++ {
		if err := ctx.Err(); err != nil {
			return nil, coverage, err
		}
		current := queue[head]
		for _, edge := range reverse[current.Path] {
			if roots[edge.Src] {
				continue
			}
			if _, ok := found[edge.Src]; ok {
				continue
			}
			if current.Hops >= options.MaxDepth {
				coverage.DepthTruncated = true
				continue
			}
			if len(found) >= options.MaxResults {
				coverage.ResultsTruncated = true
				continue
			}
			chain := append([]ImpactStep(nil), current.Chain...)
			chain = append(chain, ImpactStep{Dependency: edge.Dst, Dependent: edge.Src, Kind: edge.Kind, Evidence: evidence[impactEdgeKey(edge.Src, edge.Dst, edge.Kind)]})
			hit := ImpactHit{Path: edge.Src, Hops: current.Hops + 1, Via: edge.Kind, Chain: chain}
			found[edge.Src] = hit
			queue = append(queue, hit)
		}
	}
	return found, coverage, nil
}

func isScopedEdge(kind string) bool {
	return kind == EdgePackageReference || kind == EdgeImportReference
}
func impactEdgeKey(source, destination, kind string) string {
	return source + "\x00" + destination + "\x00" + kind
}

// RelatedTest is one test file that covers a source path, with how it was
// found. The graph route reaches tests naming conventions miss — a test file
// that imports the source is a reference edge regardless of what it is called —
// and the convention route remains for repositories and languages whose files
// the graph does not yet connect.
type RelatedTest struct {
	Path string `json:"path"`
	// Hops is the graph distance from the source; zero when the convention
	// route found the test.
	Hops int `json:"hops,omitempty"`
	// Via is the last edge kind of the walk that reached the test.
	Via string `json:"via,omitempty"`
	// Resolution says which route found the test: "graph" or "convention".
	Resolution string       `json:"resolution"`
	Reason     string       `json:"reason"`
	Chain      []ImpactStep `json:"chain,omitempty"`
}

// Resolutions a related test can carry.
const (
	TestFromGraph      = "graph"
	TestFromConvention = "convention"
)

// RelatedTests maps each source path to the test files that cover it. Two
// routes answer together: the reverse walk over the reference graph finds
// every indexed test file that depends on the source — the naming convention
// finds the test a project names after it — and a test both routes reach keeps
// the graph's evidence, which is the more specific claim.
//
// Paths that are themselves tests map to themselves. A language the convention
// table does not know can still produce graph rows, which is how Rust gains
// affected-test answers for the first time.
func (i *Index) RelatedTests(
	ctx context.Context, paths []string,
) (map[string][]RelatedTest, Snapshot, error) {
	related, _, snapshot, err := i.RelatedTestsWithEvidence(ctx, paths)
	return related, snapshot, err
}

// RelatedTestsWithEvidence isolates the closure of each source: a test reached
// from one input must never be attributed to every other input.
func (i *Index) RelatedTestsWithEvidence(ctx context.Context, paths []string) (map[string][]RelatedTest, map[string]ImpactCoverage, Snapshot, error) {
	snapshot, err := i.Ensure(ctx)
	if err != nil || !snapshot.Ready() {
		return nil, nil, snapshot, err
	}
	snapshot = i.Snapshot()
	if !snapshot.Ready() {
		return nil, nil, snapshot, nil
	}
	files, err := i.store.Files(ctx)
	if err != nil {
		return nil, nil, snapshot, err
	}
	// 刷新只排队图构建：查询前先等图追上当前文件集，否则启动后的
	// 第一次 impact 查询会退化为纯命名约定答案。等待不能持有索引锁。
	i.awaitGraph(ctx, files)
	i.mu.Lock()
	defer i.mu.Unlock()
	snapshot = i.Snapshot()
	indexed := map[string]struct{}{}
	directories := map[string][]string{}
	sitesTruncated := false
	for path, file := range files {
		indexed[path] = struct{}{}
		directory, name := splitPath(path)
		directories[directory] = append(directories[directory], name)
		sitesTruncated = sitesTruncated || file.ReferenceSitesTruncated
	}
	edges, graphErr := i.store.Edges(ctx)
	evidence := map[string]*ReferenceRelation{}
	relations, evidenceErr := i.store.ReferenceRelations(ctx)
	for _, relation := range relations {
		key := impactEdgeKey(relation.Source, relation.Destination, relation.Site.Kind)
		if evidence[key] == nil {
			r := relation
			evidence[key] = &r
		}
	}
	related := map[string][]RelatedTest{}
	coverage := map[string]ImpactCoverage{}
	reverse := impactReverse(edges)
	for _, path := range paths {
		if _, done := coverage[path]; done {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, nil, snapshot, err
		}
		hits, c, err := walkReverseImpact(ctx, []string{path}, reverse, i.options.Impact.withDefaults(), evidence)
		if err != nil {
			return nil, nil, snapshot, err
		}
		c.IndexTruncated = snapshot.Meta.Truncated
		c.GraphUnavailable = graphErr != nil
		c.ReferenceEvidenceUnavailable = evidenceErr != nil
		c.ReferenceSitesTruncated = sitesTruncated
		coverage[path] = c
		if matches := relatedTestRows(path, indexed, directories, hits); matches != nil {
			related[path] = matches
		}
	}
	return related, coverage, snapshot, nil
}

// relatedTestRows merges the two routes for one source path: graph hits first
// (with their evidence), then convention hits the graph did not reach. A
// convention miss the graph also missed leaves the path absent from the
// result, which is how a caller tells "no tests" from "cannot tell".
func relatedTestRows(
	path string,
	indexed map[string]struct{},
	directories map[string][]string,
	impacted map[string]ImpactHit,
) []RelatedTest {
	var rows []RelatedTest
	seen := make(map[string]struct{})
	if impacted != nil {
		// Deterministic order over the walk's results: hops then path, so the
		// closest tests answer first. The graph route recognizes a test by
		// where it lives as well as what it is called — the whole point of
		// the route is reaching tests whose names no convention pairs —
		// which admits a helper beside the tests as the price of the recall.
		var hits []ImpactHit
		for _, hit := range impacted {
			if _, isIndexed := indexed[hit.Path]; !isIndexed {
				continue
			}
			if looksLikeTest(hit.Path) {
				hits = append(hits, hit)
			}
		}
		sort.Slice(hits, func(x, y int) bool {
			if hits[x].Hops != hits[y].Hops {
				return hits[x].Hops < hits[y].Hops
			}
			return hits[x].Path < hits[y].Path
		})
		for _, hit := range hits {
			rows = append(rows, RelatedTest{
				Path: hit.Path, Hops: hit.Hops, Via: hit.Via,
				Resolution: TestFromGraph, Reason: impactReason(hit.Chain), Chain: hit.Chain,
			})
			seen[hit.Path] = struct{}{}
		}
	}
	if IsTestPath(path) {
		if _, found := indexed[path]; found {
			if _, duplicated := seen[path]; !duplicated {
				rows = append(rows, RelatedTest{Path: path, Resolution: TestFromGraph, Reason: "input_is_test"})
				seen[path] = struct{}{}
			}
		}
		return rows
	}
	for _, candidate := range relatedTests(path, indexed, directories) {
		if _, duplicated := seen[candidate]; duplicated {
			continue
		}
		rows = append(rows, RelatedTest{Path: candidate, Resolution: TestFromConvention, Reason: "naming_convention"})
		seen[candidate] = struct{}{}
	}
	// An empty answer and an absent one say different things: a path the
	// convention table knows maps to an empty row ("no tests"), a path no
	// route can speak about stays nil ("cannot tell").
	if rows == nil && Convention(path) {
		return []RelatedTest{}
	}
	return rows
}

// looksLikeTest reports whether the graph route should offer a file as a test.
// The naming conventions of IsTestPath answer for the convention route; the
// graph route also claims a file that lives where a project keeps its tests,
// because reaching a test no naming convention would pair with is exactly what
// the route exists for.
func looksLikeTest(path string) bool {
	if IsTestPath(path) {
		return true
	}
	for _, segment := range strings.Split(path, "/") {
		switch segment {
		case "tests", "test", "__tests__", "spec":
			return true
		}
	}
	return false
}

func impactReason(chain []ImpactStep) string {
	reason := "scoped_reference_candidate"
	for _, step := range chain {
		if step.Kind == EdgeReference {
			return "name_based_candidate"
		}
		if !isScopedEdge(step.Kind) {
			reason = "dependency_candidate"
		}
	}
	return reason
}
