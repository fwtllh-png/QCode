package search

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/persist/repoindex"
	"github.com/fwtllh-png/QCode/internal/platform/repowalk"
	"github.com/fwtllh-png/QCode/internal/platform/symbols"
)

type referenceMatch struct {
	File               string                 `json:"file"`
	Line               int                    `json:"line"`
	Character          int                    `json:"character,omitempty"`
	Text               string                 `json:"text,omitempty"`
	Source             string                 `json:"source"`
	Resolution         string                 `json:"resolution"`
	RelationType       string                 `json:"relation_type"`
	SourceDigest       string                 `json:"source_digest,omitempty"`
	TargetFile         string                 `json:"target_file,omitempty"`
	Site               *symbols.ReferenceSite `json:"site,omitempty"`
	SnippetUnavailable string                 `json:"snippet_unavailable,omitempty"`
}

type referenceCoverage struct {
	Status           string         `json:"status"`
	Scope            string         `json:"scope"`
	IndexTruncated   bool           `json:"index_truncated"`
	ResultsTruncated bool           `json:"results_truncated"`
	ScopedFiles      int            `json:"scoped_files"`
	TextFiles        int            `json:"text_files"`
	TruncatedFiles   int            `json:"reference_sites_truncated_files"`
	Skipped          map[string]int `json:"skipped_files"`
	Limitations      []string       `json:"limitations"`
}

func (t *symbolTool) readReferenceFile(ctx context.Context, path string) (repowalk.Content, string, error) {
	if err := ctx.Err(); err != nil {
		return repowalk.Content{}, "", err
	}
	budget := tool.ResultTokenBudget(ctx)
	maxBytes := int64(min(budget, uint64(math.MaxInt64/4)) * 4)
	content, reason, err := t.walker.Read(repowalk.Entry{Path: path}, maxBytes)
	return content, string(reason), err
}

// references prefers occurrence-backed cross-file candidates. A successfully
// scoped file with no candidates must not silently become a name-only match.
func (t *symbolTool) references(ctx context.Context, snapshot repoindex.Snapshot, name, pathPrefix string, includeDefinitions bool, limit int, mode string) (tool.Result, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return tool.Result{}, tool.Precondition(errors.New("a symbol name is required"))
	}
	files, relations, declared, current, err := t.index.ReferenceEvidence(ctx, name)
	if err != nil {
		return tool.Result{}, err
	}
	if !current.Ready() {
		return unavailableResult(current)
	}
	snapshot = current
	coverage := referenceCoverage{Status: "partial", Scope: "indexed_files", IndexTruncated: snapshot.Meta.Truncated, Skipped: map[string]int{}, Limitations: []string{"Candidates are not compiler bindings; local uses, dynamic calls and unsupported module resolution may be absent. Use mode=text for broader name matching."}}
	if mode == "text" {
		coverage.Limitations = []string{"Whole-word text matches may include comments, strings and unrelated names; declaration exclusion is line-based."}
	}
	byFile := map[string][]repoindex.ReferenceRelation{}
	for _, r := range relations {
		if r.Site.Target == name || r.Site.Name == name {
			byFile[r.Source] = append(byFile[r.Source], r)
		}
	}
	declarations := map[string]map[int]repoindex.Symbol{}
	for _, d := range declared {
		if d.Name != name {
			continue
		}
		if declarations[d.Path] == nil {
			declarations[d.Path] = map[int]repoindex.Symbol{}
		}
		declarations[d.Path][d.Line] = d
	}
	paths := make([]string, 0, len(files))
	for path := range files {
		if pathPrefix == "" || strings.HasPrefix(path, pathPrefix) {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	matches := make([]referenceMatch, 0)
	total := 0
	add := func(m referenceMatch) {
		total++
		if len(matches) < limit {
			matches = append(matches, m)
		}
	}
	// Structured evidence gets the result budget before fallback text matches.
	for _, scopedPass := range []bool{true, false} {
		for _, path := range paths {
			if err := ctx.Err(); err != nil {
				return tool.Result{}, err
			}
			file := files[path]
			scoped := file.ScopeAware && mode != "text"
			if scoped != scopedPass {
				continue
			}
			if scoped {
				coverage.ScopedFiles++
				if file.ReferenceSitesTruncated {
					coverage.TruncatedFiles++
				}
			} else {
				coverage.TextFiles++
			}
			if scoped && len(byFile[path]) == 0 && (!includeDefinitions || len(declarations[path]) == 0) {
				continue
			}
			content, reason, err := t.readReferenceFile(ctx, path)
			if err != nil {
				return tool.Result{}, err
			}
			if reason != string(repowalk.SkipNone) {
				coverage.Skipped[reason]++
				continue
			}
			if content.Digest != file.Digest {
				coverage.Skipped["stale_digest"]++
				continue
			}
			lines := strings.Split(string(content.Data), "\n")
			if scoped {
				for _, r := range byFile[path] {
					site := r.Site
					if r.SourceDigest != content.Digest || site.StartByte < 0 || site.EndByte < site.StartByte || site.EndByte > len(content.Data) || string(content.Data[site.StartByte:site.EndByte]) != site.Name || site.Line < 1 || site.Line > len(lines) {
						coverage.Skipped["invalid_evidence"]++
						continue
					}
					add(referenceMatch{File: path, Line: site.Line, Text: lines[site.Line-1], Source: "repoindex_scoped", Resolution: "syntax", RelationType: site.Kind, SourceDigest: content.Digest, TargetFile: r.Destination, Site: &site})
				}
			}
			for offset, text := range lines {
				line := offset + 1
				declaration, isDeclaration := declarations[path][line]
				if isDeclaration && includeDefinitions {
					add(referenceMatch{File: path, Line: line, Text: text, Source: "repoindex", Resolution: declaration.Resolution, RelationType: "declaration", SourceDigest: content.Digest})
					continue
				}
				if scoped || isDeclaration || !containsWord(text, name) {
					continue
				}
				add(referenceMatch{File: path, Line: line, Text: text, Source: "text", Resolution: "lexical", RelationType: "text_match", SourceDigest: content.Digest})
			}
		}
	}
	coverage.ResultsTruncated = total > len(matches)
	resolution := "syntax"
	if coverage.TextFiles > 0 {
		resolution = "lexical"
	}
	return referenceResult(matches, total, name, coverage, map[string]any{"resolution": resolution, "source": "repoindex", "version": repoindex.IndexerVersion, "confidence": "low", "index_source": snapshot.Meta.Source})
}

func referenceResult(matches []referenceMatch, total int, name string, coverage referenceCoverage, provenance map[string]any) (tool.Result, error) {
	hits := make([]tool.EvidenceHit, 0, len(matches))
	seen := map[string]bool{}
	for _, m := range matches {
		kind := tool.EvidenceReference
		if m.RelationType == "declaration" {
			kind = tool.EvidenceDefinition
		}
		key := fmt.Sprintf("%s:%s", m.File, kind)
		if seen[key] {
			continue
		}
		seen[key] = true
		hits = append(hits, tool.EvidenceHit{Kind: kind, Path: m.File, Line: m.Line, Symbol: name})
	}
	payload := map[string]any{"matches": matches, "total": total, "truncated": coverage.ResultsTruncated, "completeness": coverage}
	for k, v := range provenance {
		payload[k] = v
	}
	provenance["matches"] = total
	provenance["returned"] = len(matches)
	provenance["completeness"] = coverage
	return marshalResult(payload, coverage.ResultsTruncated, attach(provenance, hits))
}

func (t *symbolTool) semanticReferenceEvidence(ctx context.Context, found symbols.SemanticResult, name, pathPrefix string, limit int) (tool.Result, error) {
	files, snapshot, err := t.index.Files(ctx)
	if err != nil {
		return tool.Result{}, err
	}
	if !snapshot.Ready() {
		return unavailableResult(snapshot)
	}
	coverage := referenceCoverage{Status: "provider_reported", Scope: "provider_locations", Skipped: map[string]int{}, Limitations: []string{"Completeness is determined by the language provider; QCode cannot independently verify it."}}
	matches := make([]referenceMatch, 0)
	total := 0
	cache := map[string]repowalk.Content{}
	unavailable := map[string]string{}
	for _, loc := range found.Locations {
		if pathPrefix != "" && !strings.HasPrefix(loc.Path, pathPrefix) {
			continue
		}
		total++
		if len(matches) >= limit {
			continue
		}
		m := referenceMatch{File: loc.Path, Line: loc.Line, Character: loc.Character, Source: found.Source, Resolution: "semantic", RelationType: "semantic_reference"}
		if _, ok := files[loc.Path]; !ok {
			unavailable[loc.Path] = "not_indexed"
		}
		if _, ok := cache[loc.Path]; !ok && unavailable[loc.Path] == "" {
			content, reason, err := t.readReferenceFile(ctx, loc.Path)
			if err != nil {
				return tool.Result{}, err
			}
			if reason != string(repowalk.SkipNone) {
				unavailable[loc.Path] = reason
			} else {
				cache[loc.Path] = content
			}
		}
		if reason := unavailable[loc.Path]; reason != "" {
			m.SnippetUnavailable = reason
		} else {
			content := cache[loc.Path]
			lines := strings.Split(string(content.Data), "\n")
			if loc.Line < 1 || loc.Line > len(lines) {
				m.SnippetUnavailable = "invalid_location"
			} else {
				m.Text = lines[loc.Line-1]
				m.SourceDigest = content.Digest
			}
		}
		matches = append(matches, m)
	}
	for _, reason := range unavailable {
		coverage.Skipped[reason]++
	}
	coverage.ResultsTruncated = total > len(matches)
	return referenceResult(matches, total, name, coverage, semanticProvenance(found))
}
