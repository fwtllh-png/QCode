package search

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/typed"
	"github.com/fwtllh-png/QCode/internal/persist/repoindex"
	"github.com/fwtllh-png/QCode/internal/platform/repowalk"
	"github.com/fwtllh-png/QCode/internal/platform/symbols"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

type Tool struct {
	typed.Contract[searchInput, tool.Result]
	root      string
	kind      string
	workspace *sandbox.Workspace
	backend   sandbox.Backend
	walker    *repowalk.Walker
}

// RegisterWithBackend registers the text search tools. Sessions without a
// repository index get only these.
func RegisterWithBackend(registry *tool.Registry, root string, backend sandbox.Backend) error {
	return RegisterWithIndex(registry, root, backend, nil)
}

// RegisterWithIndex registers the text search tools and, when an index is
// configured, the symbol tools that read it. A nil index still registers them so
// the model is told why they are unavailable rather than left to guess.
func RegisterWithIndex(
	registry *tool.Registry, root string, backend sandbox.Backend, index *repoindex.Index,
) error {
	return RegisterWithProviders(registry, root, backend, index, nil)
}

// RegisterWithProviders adds a semantic symbol provider in front of the
// repository index. The index remains the explicit lexical fallback.
func RegisterWithProviders(
	registry *tool.Registry,
	root string,
	backend sandbox.Backend,
	index *repoindex.Index,
	semantic symbols.Provider,
) error {
	if backend == nil {
		return fmt.Errorf("search tools require an injected sandbox backend")
	}
	backend, err := sandbox.BindPolicy(backend, sandbox.Options{WorkspaceRoot: root})
	if err != nil {
		return err
	}
	workspace, err := sandbox.NewWorkspace(root)
	if err != nil {
		return err
	}
	walker, err := repowalk.New(workspace.Root(), backend)
	if err != nil {
		return err
	}
	registry.SetSandboxBackend(backend)
	for _, kind := range []string{"search_text", "search_files", "search_project"} {
		executor := &Tool{
			root: workspace.Root(), kind: kind,
			workspace: workspace, backend: backend, walker: walker,
		}
		contract, err := typed.NewResultContract(typed.ResultSpec[searchInput]{
			Name: kind, Disposition: tool.DispositionWaitForTeardown,
			Decode: parseSearchInput, Run: executor.run,
		})
		if err != nil {
			return err
		}
		executor.Contract = contract
		if err := registry.Register(executor); err != nil {
			return err
		}
	}
	for _, kind := range []string{KindSymbol, KindDefinition, KindReferences, KindRelatedTests} {
		executor, err := newSymbolTool(kind, index, walker, semantic)
		if err != nil {
			return err
		}
		if err := registry.Register(executor); err != nil {
			return err
		}
	}
	return nil
}

func (t *Tool) Descriptor() tool.Descriptor {
	stringOrStrings := map[string]any{
		"oneOf": []any{
			map[string]any{"type": "string", "minLength": float64(1)},
			map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		},
	}
	properties := map[string]any{
		"query":            map[string]any{"type": "string", "minLength": float64(1)},
		"pattern":          map[string]any{"type": "string", "minLength": float64(1)}, // alias of query
		"regex":            map[string]any{"type": "boolean"},
		"case_insensitive": map[string]any{"type": "boolean"},
		"case_sensitive":   map[string]any{"type": "boolean"},
		"include":          stringOrStrings,
		"exclude":          stringOrStrings,
		"glob":             map[string]any{"type": "string"},
		"file_pattern":     map[string]any{"type": "string"},
		"path":             map[string]any{"type": "string"},
		"cwd":              map[string]any{"type": "string"},
		"root":             map[string]any{"type": "string"},
		"max_file_bytes":   map[string]any{"type": "integer"},
		"max_results":      map[string]any{"type": "integer"},
		"limit":            map[string]any{"type": "integer"}, // alias of max_results
		"description":      map[string]any{"type": "string"},
	}
	if t.kind == "search_text" || t.kind == "search_project" {
		properties["before"] = map[string]any{"type": "integer"}
		properties["after"] = map[string]any{"type": "integer"}
		properties["context"] = map[string]any{"type": "integer"} // sets both before/after
	}
	return tool.Descriptor{
		Name: t.kind, Description: searchDescription(t.kind), Visibility: tool.VisibleModel,
		DiscoveryTerms: searchDiscoveryTerms(t.kind),
		IdentityKeys:   []string{"query", "pattern"},
		Capability:     tool.CapabilityRead, AccessMode: tool.AccessTree,
		ResourceResolver: tool.ResourceResolver{Templates: []tool.ResourceTemplate{{
			Kind: "repo", ID: ".", Access: tool.AccessRead, Tree: true,
		}}},
		ParallelPolicy: tool.ParallelConcurrent, RepeatPolicy: tool.RepeatReplaySameTurn,
		SandboxRequirement: tool.SandboxNone, Availability: tool.AvailabilityAvailable,
		InputSchema: map[string]any{
			"type": "object", "properties": properties,
			"anyOf": []any{
				map[string]any{"required": []string{"query"}},
				map[string]any{"required": []string{"pattern"}},
			},
			"additionalProperties": false,
		},
	}
}

func searchDiscoveryTerms(kind string) []string {
	switch kind {
	case "search_files":
		return []string{"find file", "search files", "查找文件", "文件名"}
	case "search_project":
		return []string{"search project", "搜索项目", "搜索代码"}
	default:
		return []string{"search text", "grep", "搜索文本", "查找内容"}
	}
}

func searchDescription(kind string) string {
	switch kind {
	case "search_files":
		return "Find files by path/name. Pattern is regex by default; query uses fuzzy matching unless regex=true. " +
			"Aliases: glob/file_pattern→include, path/cwd/root→scope, limit→max_results."
	case "search_project":
		return "Search file contents and paths in the workspace. Supports regex and context lines. " +
			"Pattern is regex by default; query is literal unless regex=true. " +
			"Aliases: glob/file_pattern→include, path/cwd/root→scope, limit→max_results, context→before/after."
	default:
		return "Search file contents in the workspace. Supports regex and context lines. " +
			"Pattern is regex by default; query is literal unless regex=true. " +
			"Aliases: glob/file_pattern→include, path/cwd/root→scope, limit→max_results, context→before/after. " +
			"A path that names one file is scanned up to the public walk byte ceiling " +
			"even when the result-token budget would otherwise skip it as large. " +
			"Empty matches include skipped counts; skipped.large does not mean the symbol is absent. " +
			"When a scoped file has line hits, start file_read with that window; " +
			"expand only as needed for read-only analysis or edits."
	}
}

type searchInput struct {
	Query           string
	Regex           bool
	Include         []string
	Exclude         []string
	MaxFileBytes    int64
	MaxResults      int
	CaseInsensitive bool
	Before          int
	After           int
	Scope           string
}

func parseSearchInput(raw json.RawMessage) (searchInput, error) {
	var loose map[string]any
	if err := json.Unmarshal(raw, &loose); err != nil {
		return searchInput{}, err
	}
	query := stringField(loose, "query")
	if query == "" {
		query = stringField(loose, "pattern")
	}
	if query == "" {
		return searchInput{}, fmt.Errorf("query (or pattern) is required")
	}
	include := stringListField(loose, "include")
	for _, key := range []string{"glob", "file_pattern"} {
		if value := stringField(loose, key); value != "" {
			include = append(include, value)
		}
	}
	scope := firstNonEmpty(
		stringField(loose, "path"),
		stringField(loose, "cwd"),
		stringField(loose, "root"),
	)
	scope = filepath.ToSlash(strings.Trim(scope, "/"))
	if scope == "." {
		scope = ""
	}
	maxResults := intField(loose, "max_results")
	if maxResults <= 0 {
		maxResults = intField(loose, "limit")
	}
	before := intField(loose, "before")
	after := intField(loose, "after")
	if context := intField(loose, "context"); context > 0 {
		if before <= 0 {
			before = context
		}
		if after <= 0 {
			after = context
		}
	}
	caseInsensitive := boolField(loose, "case_insensitive")
	if _, hasCI := loose["case_insensitive"]; !hasCI {
		if _, hasCS := loose["case_sensitive"]; hasCS {
			caseInsensitive = !boolField(loose, "case_sensitive")
		}
	}
	regex := boolField(loose, "regex")
	if _, explicit := loose["regex"]; !explicit {
		_, regex = loose["pattern"]
	}
	return searchInput{
		Query: query, Regex: regex,
		Include: include, Exclude: stringListField(loose, "exclude"),
		MaxFileBytes: int64(intField(loose, "max_file_bytes")), MaxResults: maxResults,
		CaseInsensitive: caseInsensitive, Before: before, After: after, Scope: scope,
	}, nil
}

func pathInScope(relative, scope string) bool {
	relative = filepath.ToSlash(relative)
	scope = filepath.ToSlash(strings.Trim(scope, "/"))
	if scope == "" || scope == "." {
		return true
	}
	return relative == scope || strings.HasPrefix(relative, scope+"/")
}

func readLimitForEntry(entry repowalk.Entry, scope string, maxFileBytes int64) int64 {
	if scope != "" && entry.Path == scope &&
		entry.Size > maxFileBytes &&
		entry.Size <= repowalk.DefaultMaxFileBytes {
		return entry.Size
	}
	return maxFileBytes
}

func visibleSkipCounts(skips repowalk.Skips) map[string]int {
	if skips.Large == 0 && skips.Binary == 0 &&
		skips.Encoding == 0 && skips.Missing == 0 &&
		skips.Linked == 0 {
		return nil
	}
	return map[string]int{
		"large": skips.Large, "binary": skips.Binary,
		"encoding": skips.Encoding, "missing": skips.Missing,
		"linked": skips.Linked,
	}
}

func stringField(values map[string]any, key string) string {
	raw, ok := values[key]
	if !ok || raw == nil {
		return ""
	}
	text, _ := raw.(string)
	return strings.TrimSpace(text)
}

func boolField(values map[string]any, key string) bool {
	raw, ok := values[key]
	if !ok || raw == nil {
		return false
	}
	switch typed := raw.(type) {
	case bool:
		return typed
	case string:
		return strings.EqualFold(typed, "true") || typed == "1"
	default:
		return false
	}
}

func intField(values map[string]any, key string) int {
	raw, ok := values[key]
	if !ok || raw == nil {
		return 0
	}
	switch typed := raw.(type) {
	case float64:
		return int(typed)
	case json.Number:
		value, _ := typed.Int64()
		return int(value)
	case int:
		return typed
	case int64:
		return int(typed)
	default:
		return 0
	}
}

func stringListField(values map[string]any, key string) []string {
	raw, ok := values[key]
	if !ok || raw == nil {
		return nil
	}
	switch typed := raw.(type) {
	case string:
		typed = strings.TrimSpace(typed)
		if typed == "" {
			return nil
		}
		return []string{typed}
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			text, _ := item.(string)
			text = strings.TrimSpace(text)
			if text != "" {
				out = append(out, text)
			}
		}
		return out
	case []string:
		return append([]string(nil), typed...)
	default:
		return nil
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func (t *Tool) run(ctx context.Context, input searchInput) (tool.Result, error) {
	var matcher func(string) bool
	if input.Regex {
		expressionText := input.Query
		if input.CaseInsensitive {
			expressionText = "(?i)" + expressionText
		}
		expression, err := regexp.Compile(expressionText)
		if err != nil {
			return tool.Result{}, err
		}
		matcher = expression.MatchString
	} else {
		query := input.Query
		if input.CaseInsensitive {
			query = strings.ToLower(query)
			matcher = func(value string) bool { return strings.Contains(strings.ToLower(value), query) }
		} else {
			matcher = func(value string) bool { return strings.Contains(value, query) }
		}
	}
	if input.Before < 0 || input.After < 0 {
		return tool.Result{}, fmt.Errorf("before and after must not be negative")
	}
	input.Before = min(input.Before, 20)
	input.After = min(input.After, 20)
	if budget := tool.ResultTokenBudget(ctx); budget != 0 {
		maxResults := int(min(budget, uint64(math.MaxInt)))
		maxFileBytes := int64(min(budget, uint64(math.MaxInt64/4)) * 4)
		if input.MaxResults <= 0 || input.MaxResults > maxResults {
			input.MaxResults = maxResults
		}
		if input.MaxFileBytes <= 0 || input.MaxFileBytes > maxFileBytes {
			input.MaxFileBytes = maxFileBytes
		}
	}
	if input.MaxFileBytes <= 0 || input.MaxResults <= 0 {
		return tool.Result{}, fmt.Errorf(
			"search requires runtime result budget or explicit max_file_bytes and max_results",
		)
	}
	textMatches := make([]textMatch, 0)
	fileMatches := make([]fileMatch, 0)
	listing, err := t.walker.List(ctx)
	if err != nil {
		return tool.Result{}, err
	}
	skips := listing.Skips
	var scopedSkip string
	var scopedSize int64
	for _, entry := range listing.Files {
		if err := ctx.Err(); err != nil {
			return tool.Result{}, err
		}
		if input.Scope != "" && !pathInScope(entry.Path, input.Scope) {
			continue
		}
		if !included(entry.Path, input.Include, input.Exclude) {
			continue
		}
		if t.kind == "search_files" {
			if input.Regex {
				if matcher(entry.Path) {
					fileMatches = append(fileMatches, fileMatch{
						Path: entry.Path, Score: len(entry.Path),
					})
				}
				continue
			}
			if score, matched := fuzzyScore(entry.Path, input.Query); matched {
				fileMatches = append(fileMatches, fileMatch{Path: entry.Path, Score: score})
			}
			continue
		}
		content, reason, err := t.walker.Read(
			entry,
			readLimitForEntry(entry, input.Scope, input.MaxFileBytes),
		)
		if err != nil {
			return tool.Result{}, err
		}
		if reason != repowalk.SkipNone {
			skips.Add(reason)
			if input.Scope != "" && entry.Path == input.Scope {
				scopedSkip = string(reason)
				scopedSize = entry.Size
			}
			continue
		}
		lines := strings.Split(string(content.Data), "\n")
		for index, text := range lines {
			if matcher(text) || (t.kind == "search_project" && matcher(entry.Path)) {
				textMatches = append(textMatches, textMatch{
					File: entry.Path, Line: index + 1, Text: text,
					Context: buildContext(lines, index, input.Before, input.After),
				})
			}
		}
	}
	var payload map[string]any
	total := 0
	if t.kind == "search_files" {
		sort.Slice(fileMatches, func(i, j int) bool {
			if fileMatches[i].Score != fileMatches[j].Score {
				return fileMatches[i].Score > fileMatches[j].Score
			}
			return fileMatches[i].Path < fileMatches[j].Path
		})
		total = len(fileMatches)
	} else {
		sort.Slice(textMatches, func(i, j int) bool {
			if textMatches[i].File != textMatches[j].File {
				return textMatches[i].File < textMatches[j].File
			}
			return textMatches[i].Line < textMatches[j].Line
		})
		total = len(textMatches)
	}
	truncated := total > input.MaxResults
	if truncated {
		if t.kind == "search_files" {
			fileMatches = fileMatches[:input.MaxResults]
		} else {
			textMatches = textMatches[:input.MaxResults]
		}
	}
	if t.kind == "search_files" {
		payload = map[string]any{"matches": fileMatches, "total": total, "truncated": truncated}
	} else {
		payload = map[string]any{"matches": textMatches, "total": total, "truncated": truncated}
	}
	if skipped := visibleSkipCounts(skips); skipped != nil {
		payload["skipped"] = skipped
	}
	if scopedSkip != "" {
		payload["note"] = fmt.Sprintf(
			"scoped path %q was skipped (%s): size=%d max_file_bytes=%d. "+
				"Empty matches do not mean the symbol is absent; raise max_file_bytes "+
				"or file_read a window.",
			input.Scope, scopedSkip, scopedSize, input.MaxFileBytes,
		)
	} else if input.Scope != "" && t.kind != "search_files" && total > 0 {
		payload["note"] = "These are line hits in the scoped file. " +
			"Start file_read with the relevant window; " +
			"expand only as needed for read-only analysis or edits."
	}
	content, err := json.Marshal(payload)
	if err != nil {
		return tool.Result{}, err
	}
	returned := len(textMatches)
	hits := textHits(textMatches)
	if t.kind == "search_files" {
		returned = len(fileMatches)
		paths := make([]string, 0, len(fileMatches))
		for _, match := range fileMatches {
			paths = append(paths, match.Path)
		}
		hits = pathHits(paths)
	}
	return tool.Result{
		Content:   string(content),
		Truncated: truncated,
		Metadata: attach(map[string]any{
			"matches": total, "returned": returned,
			// enumeration says which rules produced the file set. Under "git" the
			// ignored files never reach the walk, so skipped_ignored counts only the
			// directories left out by name — vendor and its peers.
			"enumeration":     listing.Source,
			"skipped_ignored": skips.Ignored, "skipped_binary": skips.Binary,
			"skipped_large": skips.Large, "skipped_encoding": skips.Encoding,
			"skipped_symlink": skips.Symlink,
		}, hits),
	}, nil
}

type contextLine struct {
	Line int    `json:"line"`
	Text string `json:"text"`
}

type matchContext struct {
	Before []contextLine `json:"before"`
	After  []contextLine `json:"after"`
}

type textMatch struct {
	File    string       `json:"file"`
	Line    int          `json:"line"`
	Text    string       `json:"text"`
	Context matchContext `json:"context"`
}

type fileMatch struct {
	Path  string `json:"path"`
	Score int    `json:"score"`
}

func buildContext(lines []string, index, before, after int) matchContext {
	context := matchContext{
		Before: make([]contextLine, 0, before),
		After:  make([]contextLine, 0, after),
	}
	for line := max(0, index-before); line < index; line++ {
		context.Before = append(context.Before, contextLine{Line: line + 1, Text: lines[line]})
	}
	for line := index + 1; line < min(len(lines), index+after+1); line++ {
		context.After = append(context.After, contextLine{Line: line + 1, Text: lines[line]})
	}
	return context
}

func fuzzyScore(candidate, query string) (int, bool) {
	candidate = strings.ToLower(filepath.ToSlash(candidate))
	query = strings.ToLower(query)
	if candidate == query {
		return 10000, true
	}
	if index := strings.Index(candidate, query); index >= 0 {
		score := 5000 - index - (len(candidate) - len(query))
		if strings.Contains(strings.ToLower(path.Base(candidate)), query) {
			score += 500
		}
		return score, true
	}
	queryIndex := 0
	score := 0
	consecutive := 0
	for index := 0; index < len(candidate) && queryIndex < len(query); index++ {
		if candidate[index] != query[queryIndex] {
			consecutive = 0
			continue
		}
		queryIndex++
		consecutive++
		score += 10 + consecutive*5
		if index == 0 || candidate[index-1] == '/' || candidate[index-1] == '-' || candidate[index-1] == '_' {
			score += 20
		}
	}
	if queryIndex != len(query) {
		return 0, false
	}
	return score - len(candidate), true
}

func included(relative string, includes, excludes []string) bool {
	if len(includes) != 0 && !matchAny(relative, includes) {
		return false
	}
	return !matchAny(relative, excludes)
}

func matchAny(relative string, patterns []string) bool {
	for _, pattern := range patterns {
		if matched, _ := path.Match(pattern, relative); matched {
			return true
		}
		if !strings.Contains(pattern, "/") {
			if matched, _ := path.Match(pattern, path.Base(relative)); matched {
				return true
			}
		}
		if strings.Contains(pattern, "**") && globRegex(pattern).MatchString(relative) {
			return true
		}
	}
	return false
}

func globRegex(pattern string) *regexp.Regexp {
	var expression strings.Builder
	expression.WriteByte('^')
	for index := 0; index < len(pattern); {
		switch {
		case strings.HasPrefix(pattern[index:], "**"):
			expression.WriteString(".*")
			index += 2
		case pattern[index] == '*':
			expression.WriteString("[^/]*")
			index++
		case pattern[index] == '?':
			expression.WriteString("[^/]")
			index++
		default:
			expression.WriteString(regexp.QuoteMeta(pattern[index : index+1]))
			index++
		}
	}
	expression.WriteByte('$')
	return regexp.MustCompile(expression.String())
}
