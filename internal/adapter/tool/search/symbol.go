package search

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strings"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/typed"
	"github.com/fwtllh-png/QCode/internal/persist/repoindex"
	"github.com/fwtllh-png/QCode/internal/platform/repowalk"
	"github.com/fwtllh-png/QCode/internal/platform/symbols"
)

// Symbol tool names.
const (
	KindSymbol       = "search_symbol"
	KindDefinition   = "search_definition"
	KindReferences   = "search_references"
	KindRelatedTests = "search_related_tests"
)

// symbolTool answers a question about declarations from the repository index.
// Every one of them is read-only over the repository tree and reports its
// results with their extraction tier and reference provenance.
type symbolTool struct {
	typed.Contract[symbolInput, tool.Result]
	kind     string
	index    *repoindex.Index
	walker   *repowalk.Walker
	semantic symbols.Provider
}

type symbolInput struct {
	Mode               string   `json:"mode"`
	Query              string   `json:"query"`
	Name               string   `json:"name"`
	Kinds              []string `json:"kinds"`
	ExportedOnly       bool     `json:"exported_only"`
	Path               string   `json:"path"`
	PathPrefix         string   `json:"path_prefix"`
	Paths              []string `json:"paths"`
	IncludeDefinitions bool     `json:"include_definitions"`
	MaxResults         int      `json:"max_results"`
	Line               int      `json:"line"`
	Character          int      `json:"character"`
}

func newSymbolTool(
	kind string,
	index *repoindex.Index,
	walker *repowalk.Walker,
	semantic symbols.Provider,
) (*symbolTool, error) {
	executor := &symbolTool{
		kind: kind, index: index, walker: walker, semantic: semantic,
	}
	contract, err := typed.NewResultContract(typed.ResultSpec[symbolInput]{
		Name: kind, Disposition: tool.DispositionWaitForTeardown,
		Run: executor.run,
	})
	if err != nil {
		return nil, err
	}
	executor.Contract = contract
	return executor, nil
}

func (t *symbolTool) Descriptor() tool.Descriptor {
	descriptor := tool.Descriptor{
		Name: t.kind, Description: symbolDescription(t.kind), Visibility: tool.VisibleModel,
		DiscoveryTerms: symbolDiscoveryTerms(t.kind),
		Capability:     tool.CapabilityRead, AccessMode: tool.AccessTree,
		ResourceResolver: tool.ResourceResolver{Templates: []tool.ResourceTemplate{{
			Kind: "repo", ID: ".", Access: tool.AccessRead, Tree: true,
		}}},
		ParallelPolicy:     tool.ParallelConcurrent,
		SandboxRequirement: tool.SandboxNone,
		Availability:       tool.AvailabilityAvailable,
		InputSchema:        symbolSchema(t.kind),
	}
	// A tool the session cannot serve is declared unavailable rather than left to
	// fail at call time, so the model never plans around it.
	switch snapshot := t.index.Snapshot(); {
	case snapshot.Status == repoindex.StatusDisabled:
		descriptor.Availability = tool.AvailabilityUnavailable
		descriptor.UnavailableReason = "the repository index is disabled for this session"
	case snapshot.Status == repoindex.StatusDegraded:
		descriptor.Availability = tool.AvailabilityUnavailable
		descriptor.UnavailableReason = "the repository index is unavailable: " + snapshot.Detail
	}
	return descriptor
}

func symbolDiscoveryTerms(kind string) []string {
	switch kind {
	case KindDefinition:
		return []string{"definition", "declaration", "定义", "声明", "跳转"}
	case KindReferences:
		return []string{"references", "usages", "引用", "调用"}
	case KindRelatedTests:
		return []string{"related tests", "tests for", "关联测试", "相关测试"}
	default:
		return []string{"symbol", "function", "class", "符号", "函数", "类型", "类"}
	}
}

func symbolDescription(kind string) string {
	switch kind {
	case KindDefinition:
		return "Find where a symbol is declared, by exact name. " +
			"Results report syntax, lexical or heuristic extraction quality."
	case KindReferences:
		return "Find reference evidence by name or import alias. Auto mode uses scoped cross-file " +
			"candidates and text fallback for unsupported files; results report source and completeness. " +
			"A document position uses LSP when available. Use mode=text for whole-word matches, " +
			"including local uses, comments and strings."
	case KindRelatedTests:
		return "Recommend tests for changed file paths, with a shortest dependency chain and " +
			"reference evidence where available. Distinguishes scoped candidates, name-based " +
			"associations and naming conventions; reports traversal limits. This is file-level " +
			"impact analysis, not proven function-level test coverage."
	default:
		return "Search declarations (functions, types, classes, constants) by name " +
			"substring. Results report syntax, lexical or heuristic extraction quality."
	}
}

func symbolSchema(kind string) map[string]any {
	switch kind {
	case KindDefinition:
		return map[string]any{
			"type": "object", "additionalProperties": false,
			"required": []string{"name"},
			"properties": map[string]any{
				"name":        map[string]any{"type": "string", "minLength": float64(1)},
				"path":        map[string]any{"type": "string", "minLength": float64(1)},
				"line":        map[string]any{"type": "integer", "minimum": float64(1)},
				"character":   map[string]any{"type": "integer", "minimum": float64(1)},
				"kinds":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"max_results": map[string]any{"type": "integer"},
			},
		}
	case KindReferences:
		return map[string]any{
			"type": "object", "additionalProperties": false,
			"required": []string{"name"},
			"properties": map[string]any{
				"name":                map[string]any{"type": "string", "minLength": float64(1)},
				"path":                map[string]any{"type": "string", "minLength": float64(1)},
				"line":                map[string]any{"type": "integer", "minimum": float64(1)},
				"character":           map[string]any{"type": "integer", "minimum": float64(1)},
				"include_definitions": map[string]any{"type": "boolean"},
				"mode":                map[string]any{"type": "string", "enum": []string{"auto", "text"}, "description": "Defaults to auto. Text mode bypasses scoped and LSP analysis."},
				"path_prefix":         map[string]any{"type": "string"},
				"max_results":         map[string]any{"type": "integer"},
			},
		}
	case KindRelatedTests:
		return map[string]any{
			"type": "object", "additionalProperties": false,
			"required": []string{"paths"},
			"properties": map[string]any{
				"paths": map[string]any{
					"type": "array", "minItems": float64(1),
					"items": map[string]any{"type": "string", "minLength": float64(1)},
				},
			},
		}
	default:
		return map[string]any{
			"type": "object", "additionalProperties": false,
			"required": []string{"query"},
			"properties": map[string]any{
				"query":         map[string]any{"type": "string", "minLength": float64(1)},
				"kinds":         map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"exported_only": map[string]any{"type": "boolean"},
				"path_prefix":   map[string]any{"type": "string"},
				"max_results":   map[string]any{"type": "integer"},
			},
		}
	}
}

func (t *symbolTool) run(ctx context.Context, input symbolInput) (tool.Result, error) {
	limit := input.MaxResults
	if budget := tool.ResultTokenBudget(ctx); budget != 0 &&
		(limit <= 0 || uint64(limit) > budget) {
		limit = int(min(budget, uint64(math.MaxInt)))
	}
	if limit <= 0 && t.kind != KindRelatedTests {
		return tool.Result{}, errors.New(
			"symbol search requires runtime result budget or explicit max_results",
		)
	}
	// Index query methods refresh themselves; do not walk the repository twice.
	snapshot := t.index.Snapshot()
	if snapshot.Status == repoindex.StatusDisabled {
		return unavailableResult(snapshot)
	}
	switch t.kind {
	case KindDefinition:
		if query, ok := semanticQuery(input.Path, input.Line, input.Character); ok &&
			t.semantic != nil {
			found, semanticErr := t.semantic.Definition(ctx, query)
			if semanticErr == nil {
				return semanticDefinitions(found, input.Name, limit)
			}
			result, err := t.declarations(ctx, snapshot, repoindex.Query{
				Name: input.Name, Exact: true, Kinds: input.Kinds,
				Limit: limit,
			}, "", false)
			return semanticFallback(result, semanticErr), err
		}
		return t.declarations(ctx, snapshot, repoindex.Query{
			Name: input.Name, Exact: true, Kinds: input.Kinds,
			Limit: limit,
		}, "", false)
	case KindReferences:
		if input.Mode != "" && input.Mode != "auto" && input.Mode != "text" {
			return tool.Result{}, tool.Precondition(errors.New("mode must be auto or text"))
		}
		if query, ok := semanticQuery(input.Path, input.Line, input.Character); ok &&
			t.semantic != nil && input.Mode != "text" {
			found, semanticErr := t.semantic.References(
				ctx, query, input.IncludeDefinitions,
			)
			if semanticErr == nil {
				return t.semanticReferenceEvidence(ctx, found, input.Name, input.PathPrefix, limit)
			}
			result, err := t.references(
				ctx, snapshot, input.Name, input.PathPrefix,
				input.IncludeDefinitions, limit, input.Mode,
			)
			return semanticFallback(result, semanticErr), err
		}
		return t.references(ctx, snapshot, input.Name, input.PathPrefix,
			input.IncludeDefinitions, limit, input.Mode)
	case KindRelatedTests:
		return t.relatedTests(ctx, snapshot, input.Paths)
	default:
		return t.declarations(ctx, snapshot, repoindex.Query{
			Name: input.Query, Kinds: input.Kinds,
			Limit: limit,
		}, input.PathPrefix, input.ExportedOnly)
	}
}

type symbolMatch struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	File      string `json:"file"`
	Line      int    `json:"line"`
	Character int    `json:"character,omitempty"`
	Container string `json:"container,omitempty"`
	Exported  bool   `json:"exported"`
	// Signature is the bounded declaration text; Resolution says which
	// extraction tier produced the row, so a heuristic match from the generic
	// engine never reads as a rule-table one.
	Signature  string `json:"signature,omitempty"`
	Resolution string `json:"resolution,omitempty"`
}

func (t *symbolTool) declarations(
	ctx context.Context, snapshot repoindex.Snapshot,
	query repoindex.Query, pathPrefix string, exportedOnly bool,
) (tool.Result, error) {
	// Ask for more rows than the reply needs, so filtering by path or visibility
	// does not silently shrink a full page of results.
	limit := query.Limit
	query.Limit = limit * 4
	found, current, err := t.index.Symbols(ctx, query)
	if err != nil {
		return tool.Result{}, err
	}
	if !current.Ready() {
		return unavailableResult(current)
	}
	snapshot = current
	matches := make([]symbolMatch, 0, min(limit, len(found)))
	total := 0
	for _, symbol := range found {
		if exportedOnly && !symbol.Exported {
			continue
		}
		if pathPrefix != "" && !strings.HasPrefix(symbol.Path, pathPrefix) {
			continue
		}
		total++
		if len(matches) >= limit {
			continue
		}
		matches = append(matches, symbolMatch{
			Name: symbol.Name, Kind: symbol.Kind, File: symbol.Path,
			Line: symbol.Line, Container: symbol.Container, Exported: symbol.Exported,
			Signature: symbol.Signature, Resolution: symbol.Resolution,
		})
	}
	truncated := total > len(matches)
	hits := make([]tool.EvidenceHit, 0, len(matches))
	for _, match := range matches {
		hits = append(hits, tool.EvidenceHit{
			Kind: tool.EvidenceDefinition, Path: match.File,
			Line: match.Line, Symbol: match.Name,
		})
	}
	return marshalResult(map[string]any{
		"matches": matches, "total": total, "truncated": truncated,
		"resolution": repoindex.WeakestResolution(found), "source": "repoindex",
		"version": repoindex.IndexerVersion, "confidence": "low",
	}, truncated, attach(map[string]any{
		"matches": total, "returned": len(matches),
		"resolution": repoindex.WeakestResolution(found), "index_source": snapshot.Meta.Source,
		"index_files": snapshot.Meta.FileCount, "source": "repoindex",
		"version": repoindex.IndexerVersion, "confidence": "low",
	}, hits))
}

func semanticQuery(path string, line, character int) (symbols.SemanticQuery, bool) {
	path = strings.TrimSpace(path)
	if path == "" || line < 1 || character < 1 {
		return symbols.SemanticQuery{}, false
	}
	return symbols.SemanticQuery{
		Path: path, Line: line, Character: character,
	}, true
}

func semanticDefinitions(
	found symbols.SemanticResult, name string, limit int,
) (tool.Result, error) {
	locations := found.Locations
	if len(locations) > limit {
		locations = locations[:limit]
	}
	matches := make([]symbolMatch, 0, len(locations))
	hits := make([]tool.EvidenceHit, 0, len(locations))
	for _, location := range locations {
		matches = append(matches, symbolMatch{
			Name: name, Kind: "semantic", File: location.Path,
			Line: location.Line, Character: location.Character,
		})
		hits = append(hits, tool.EvidenceHit{
			Kind: tool.EvidenceDefinition, Path: location.Path,
			Line: location.Line, Symbol: name,
		})
	}
	provenance := semanticProvenance(found)
	payload := map[string]any{
		"matches": matches, "total": len(found.Locations),
		"truncated": len(locations) != len(found.Locations),
	}
	for key, value := range provenance {
		payload[key] = value
	}
	provenance["matches"] = len(matches)
	provenance["returned"] = len(matches)
	return marshalResult(
		payload, len(locations) != len(found.Locations), attach(provenance, hits),
	)
}

func semanticProvenance(found symbols.SemanticResult) map[string]any {
	return map[string]any{
		"resolution": "semantic", "source": found.Source,
		"version": found.Version, "confidence": found.Confidence,
	}
}

func semanticFallback(result tool.Result, semanticErr error) tool.Result {
	if semanticErr == nil {
		return result
	}
	if result.Metadata == nil {
		result.Metadata = make(map[string]any)
	}
	message := semanticErr.Error()
	if len(message) > 512 {
		message = message[:512]
	}
	result.Metadata["semantic_fallback"] = message
	var payload map[string]any
	if json.Unmarshal([]byte(result.Content), &payload) == nil && payload != nil {
		payload["semantic_fallback"] = message
		if content, err := json.Marshal(payload); err == nil {
			result.Content = string(content)
		}
	}
	return result
}

func (t *symbolTool) relatedTests(
	ctx context.Context, snapshot repoindex.Snapshot, paths []string,
) (tool.Result, error) {
	if len(paths) == 0 {
		return tool.Result{}, tool.Precondition(errors.New("at least one path is required"))
	}
	related, completeness, current, err := t.index.RelatedTestsWithEvidence(ctx, normalizePaths(paths))
	if err != nil {
		return tool.Result{}, err
	}
	if !current.Ready() {
		return unavailableResult(current)
	}
	snapshot = current
	sources := make([]string, 0, len(related))
	for source := range related {
		sources = append(sources, source)
	}
	sort.Strings(sources)
	type coverage struct {
		Source       string                   `json:"source"`
		Tests        []relatedTestEvidence    `json:"tests"`
		Completeness repoindex.ImpactCoverage `json:"completeness"`
	}
	entries := make([]coverage, 0, len(sources))
	evidenceReader := newImpactEvidenceReader(t)
	truncated := false
	for _, c := range completeness {
		truncated = truncated || c.ResultsTruncated || c.DepthTruncated
	}
	unmapped := make([]string, 0)
	tests := 0
	routes := map[string]struct{}{}
	for _, source := range sources {
		rows, err := evidenceReader.rows(ctx, related[source])
		if err != nil {
			return tool.Result{}, err
		}
		c := completeness[source]
		truncated = truncated || c.ResultsTruncated || c.DepthTruncated
		entries = append(entries, coverage{Source: source, Tests: rows, Completeness: c})
		tests += len(related[source])
		for _, test := range related[source] {
			routes[test.Resolution] = struct{}{}
		}
	}
	for _, source := range normalizePaths(paths) {
		if _, found := related[source]; !found {
			unmapped = append(unmapped, source)
		}
	}
	hits := make([]tool.EvidenceHit, 0, tests)
	for _, entry := range entries {
		for _, test := range entry.Tests {
			hits = append(hits, tool.EvidenceHit{Kind: tool.EvidenceTest, Path: test.Path})
		}
	}
	// The tests say which route found them; the answer-level field says which
	// routes spoke at all, so a graph-only answer never reads as a convention
	// one and vice versa.
	answer := "convention"
	if len(routes) == 2 {
		answer = "graph+convention"
	} else if _, graphOnly := routes[repoindex.TestFromGraph]; graphOnly {
		answer = "graph"
	}
	return marshalResult(map[string]any{
		"coverage": entries, "unmapped": unmapped, "impact_source": answer,
		"completeness": completeness, "truncated": truncated,
		"scope": "file_dependencies", "claim": "recommended_tests_not_proven_coverage",
		"resolution": repoindex.ResolutionLexical,
	}, truncated, attach(map[string]any{
		"completeness": completeness,
		"sources":      len(entries), "tests": tests, "unmapped": len(unmapped),
		"impact_source": answer,
		"resolution":    repoindex.ResolutionLexical, "index_source": snapshot.Meta.Source,
	}, hits))
}

// unavailableResult reports an index that cannot answer. It is a result rather
// than an error so the model reads the reason and falls back to text search
// within the same turn.
func unavailableResult(snapshot repoindex.Snapshot) (tool.Result, error) {
	detail := snapshot.Detail
	if detail == "" {
		detail = "the repository index is " + snapshot.Status
	}
	return marshalResult(map[string]any{
		"status": "unavailable", "detail": detail, "matches": []any{},
		"hint": "fall back to search_text or search_project for this question",
	}, false, map[string]any{
		"status": "unavailable", "index_status": snapshot.Status,
	})
}

func marshalResult(payload any, truncated bool, metadata map[string]any) (tool.Result, error) {
	content, err := json.Marshal(payload)
	if err != nil {
		return tool.Result{}, err
	}
	return tool.Result{Content: string(content), Truncated: truncated, Metadata: metadata}, nil
}

// containsWord reports a whole-word occurrence, so searching for Serve does not
// match Server or reserve.
func containsWord(text, name string) bool {
	for offset := 0; ; {
		index := strings.Index(text[offset:], name)
		if index < 0 {
			return false
		}
		start := offset + index
		end := start + len(name)
		if !wordByte(text, start-1) && !wordByte(text, end) {
			return true
		}
		offset = start + 1
		if offset >= len(text) {
			return false
		}
	}
}

func wordByte(text string, index int) bool {
	if index < 0 || index >= len(text) {
		return false
	}
	symbol := text[index]
	return symbol == '_' || symbol == '$' ||
		(symbol >= '0' && symbol <= '9') ||
		(symbol >= 'a' && symbol <= 'z') ||
		(symbol >= 'A' && symbol <= 'Z')
}

func normalizePaths(paths []string) []string {
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		trimmed := strings.TrimPrefix(strings.TrimSpace(strings.ReplaceAll(path, "\\", "/")), "./")
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}
