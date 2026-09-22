package skill

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/fwtllh-png/QCode/internal/platform/tokenestimate"
)

const (
	DefaultSelectionLimit  = 20
	MaxSelectionLimit      = 50
	MaxSelectionQueryBytes = 4 << 10
	MaxSelectionTerms      = 64
	MaxSelectionCandidates = 1000
	maxSelectionCacheItems = 256
)

type SelectionMode string

const (
	SelectionShadow    SelectionMode = "shadow"
	SelectionCandidate SelectionMode = "candidate"
)

var (
	ErrSkillHandleInvalid = errors.New("skill handle is invalid or stale")
	ErrSkillAmbiguous     = errors.New("skill name is ambiguous")
	ErrSelectionBudget    = errors.New("skill selection budget exceeded")
)

type SelectionRequest struct {
	Query       string
	Explicit    []string
	Required    []string
	UsedHandles []string
	Limit       int
	Mode        SelectionMode
}

type SelectionMetrics struct {
	Method                string  `json:"method"`
	CatalogSize           int     `json:"catalog_size"`
	CandidateSize         int     `json:"candidate_size"`
	VisibleSize           int     `json:"visible_size"`
	ExplicitMatches       int     `json:"explicit_matches"`
	QueryTerms            int     `json:"query_terms"`
	QueryTruncated        bool    `json:"query_truncated"`
	CandidateSetTruncated bool    `json:"candidate_set_truncated"`
	OriginalTokens        uint64  `json:"original_tokens"`
	ProjectedTokens       uint64  `json:"projected_tokens"`
	TokenSavings          float64 `json:"token_savings"`
	Recall                float64 `json:"recall"`
	Precision             float64 `json:"precision"`
	CacheHit              bool    `json:"cache_hit"`
}

type Selection struct {
	Candidates []Summary        `json:"candidates"`
	Visible    []Summary        `json:"visible"`
	Metrics    SelectionMetrics `json:"metrics"`
}

type ControlSummary struct {
	Summary Summary
	Enabled bool
}

type scoredSkill struct {
	summary  Summary
	score    uint32
	explicit bool
}

func (c *Catalog) Select(
	ctx context.Context,
	request SelectionRequest,
) (Selection, error) {
	if c == nil {
		return Selection{}, errors.New("skill catalog is required")
	}
	request = normalizeSelectionRequest(request)
	summaries, err := c.selectionSummaries(ctx)
	if err != nil {
		return Selection{}, err
	}
	cacheKey := selectionCacheKey(summaries, request)
	if cached, ok := c.cachedSelection(cacheKey); ok {
		cached.Metrics.CacheHit = true
		return cached, nil
	}
	result, err := selectSummaries(summaries, request)
	if err != nil {
		return Selection{}, err
	}
	c.cacheSelection(cacheKey, result)
	return cloneSelection(result), nil
}

func (c *Catalog) ListHandles(ctx context.Context) ([]Summary, error) {
	return c.selectionSummaries(ctx)
}

func (c *Catalog) ControlSummaries(
	ctx context.Context,
) ([]ControlSummary, error) {
	if c == nil {
		return nil, errors.New("skill catalog is required")
	}
	entries, order, _ := c.snapshot()
	state, stateErr := c.stateSnapshot()
	if stateErr != nil {
		return nil, stateErr
	}
	locked := c.lockEntries()
	result := make([]ControlSummary, 0, len(order))
	for _, name := range order {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		item := entries[name]
		result = append(result, ControlSummary{
			Summary: c.summary(item, lockMatches(item, locked[name])),
			Enabled: enabledFor(item, state, nil),
		})
	}
	return result, nil
}

func (c *Catalog) HandleForName(
	ctx context.Context,
	name string,
) (string, error) {
	if !namePattern.MatchString(name) {
		return "", errors.New("skill name is invalid")
	}
	summaries, err := c.selectionSummaries(ctx)
	if err != nil {
		return "", err
	}
	var matched string
	for _, summary := range summaries {
		if summary.Name != name {
			continue
		}
		if matched != "" {
			return "", fmt.Errorf("%w: %q", ErrSkillAmbiguous, name)
		}
		matched = summary.Handle
	}
	if matched == "" {
		return "", fmt.Errorf("%w: skill %q", ErrSkillHandleInvalid, name)
	}
	return matched, nil
}

func (c *Catalog) LoadHandle(
	ctx context.Context,
	handle string,
) ([]Loaded, error) {
	// Resolve the handle and its dependency plan from the same inventory.
	// A concurrent refresh must not swap a new package under an old handle.
	view := c.frozen()
	item, err := view.candidateForHandle(ctx, handle)
	if err != nil {
		return nil, err
	}
	return view.LoadPlan(ctx, item.metadata.Name)
}

func (c *Catalog) SummaryForHandle(
	ctx context.Context,
	handle string,
) (Summary, error) {
	item, err := c.candidateForHandle(ctx, handle)
	if err != nil {
		return Summary{}, err
	}
	locked := c.lockEntries()
	return c.summary(item, lockMatches(item, locked[item.metadata.Name])), nil
}

func (c *Catalog) candidateForHandle(
	ctx context.Context,
	handle string,
) (candidate, error) {
	if !validSkillHandle(handle) {
		return candidate{}, ErrSkillHandleInvalid
	}
	entries, _, _ := c.snapshot()
	state, stateErr := c.stateSnapshot()
	if stateErr != nil {
		return candidate{}, stateErr
	}
	var matched candidate
	found := false
	for _, item := range entries {
		if !candidateMatchesHandle(item, handle) ||
			!enabledFor(item, state, nil) {
			continue
		}
		if found {
			return candidate{}, ErrSkillHandleInvalid
		}
		matched = item
		found = true
	}
	if found {
		return matched, nil
	}
	return candidate{}, ErrSkillHandleInvalid
}

func (c *Catalog) selectionSummaries(
	ctx context.Context,
) ([]Summary, error) {
	entries, order, _ := c.snapshot()
	state, err := c.stateSnapshot()
	if err != nil {
		return nil, err
	}
	locked := c.lockEntries()
	result := make([]Summary, 0, len(order))
	for _, name := range order {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		item := entries[name]
		if !enabledFor(item, state, nil) {
			continue
		}
		result = append(
			result,
			c.summary(item, lockMatches(item, locked[name])),
		)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Name != result[j].Name {
			return result[i].Name < result[j].Name
		}
		if result[i].Source != result[j].Source {
			return result[i].Source < result[j].Source
		}
		return result[i].Handle < result[j].Handle
	})
	return result, nil
}

func selectSummaries(
	summaries []Summary,
	request SelectionRequest,
) (Selection, error) {
	boundedQuery, queryTruncated := boundUTF8(
		request.Query, MaxSelectionQueryBytes,
	)
	queryPhrase := normalizeSelectionText(boundedQuery)
	terms, termsTruncated := selectionTerms(queryPhrase)
	explicit := explicitSkillNames(boundedQuery, request.Explicit)
	for _, summary := range summaries {
		name := normalizeSelectionText(summary.Name)
		if name != "" && containsSelectionPhrase(queryPhrase, name) {
			explicit[summary.Name] = true
		}
	}
	required := make(map[string]bool, len(request.Required))
	for _, name := range request.Required {
		required[name] = true
	}
	used := make(map[string]bool, len(request.UsedHandles))
	for _, handle := range request.UsedHandles {
		used[handle] = true
	}
	visiblePool := boundedSelectionPool(summaries, explicit, required, used)
	candidateSetTruncated := len(visiblePool) < len(summaries)
	scored := make([]scoredSkill, 0, len(visiblePool))
	requiredMatched := make(map[string]bool, len(required))
	mandatoryMatches := 0
	for _, summary := range visiblePool {
		if summaryDisabledForModel(summary) && !required[summary.Name] {
			continue
		}
		score := scoreSummary(queryPhrase, terms, summary)
		isExplicit := explicit[summary.Name]
		if isExplicit {
			score = score + 1<<20
		}
		if required[summary.Name] {
			score = score + 1<<19
			requiredMatched[summary.Name] = true
		}
		if used[summary.Handle] {
			score = score + 1<<18
		}
		if score == 0 {
			continue
		}
		scored = append(scored, scoredSkill{
			summary: summary, score: score, explicit: isExplicit,
		})
		if isExplicit || required[summary.Name] {
			mandatoryMatches++
		}
	}
	sort.Slice(scored, func(i, j int) bool {
		if scored[i].score != scored[j].score {
			return scored[i].score > scored[j].score
		}
		if scored[i].summary.Name != scored[j].summary.Name {
			return scored[i].summary.Name < scored[j].summary.Name
		}
		return scored[i].summary.Handle < scored[j].summary.Handle
	})
	if len(requiredMatched) != len(required) {
		return Selection{}, fmt.Errorf(
			"%w: required skill is unavailable", ErrSkillHandleInvalid,
		)
	}
	if mandatoryMatches > request.Limit {
		return Selection{}, ErrSelectionBudget
	}
	if len(scored) > request.Limit {
		scored = scored[:request.Limit]
	}
	candidates := make([]Summary, 0, len(scored))
	explicitMatches := 0
	for _, item := range scored {
		candidates = append(candidates, item.summary)
		if item.explicit {
			explicitMatches++
		}
	}
	visible := candidates
	if request.Mode == SelectionShadow {
		visible = append([]Summary(nil), summaries...)
	}
	originalTokens := estimateMetadataTokens(summaries)
	projectedTokens := estimateMetadataTokens(visible)
	savings := 0.0
	if originalTokens != 0 {
		savings = 1 - float64(projectedTokens)/float64(originalTokens)
	}
	recall := 1.0
	if len(required)+len(explicit) != 0 {
		recall = float64(mandatoryMatches) / float64(len(required)+len(explicit))
	}
	precision := 1.0
	if len(candidates) == 0 {
		precision = 0
	}
	return Selection{
		Candidates: candidates, Visible: append([]Summary(nil), visible...),
		Metrics: SelectionMetrics{
			Method: "weighted_lexical_v1", CatalogSize: len(summaries),
			CandidateSize: len(candidates), VisibleSize: len(visible),
			ExplicitMatches: explicitMatches, QueryTerms: len(terms),
			QueryTruncated:        queryTruncated || termsTruncated,
			CandidateSetTruncated: candidateSetTruncated,
			OriginalTokens:        originalTokens, ProjectedTokens: projectedTokens,
			TokenSavings: savings, Recall: recall, Precision: precision,
		},
	}, nil
}

func boundedSelectionPool(
	summaries []Summary,
	explicit, required map[string]bool,
	used map[string]bool,
) []Summary {
	if len(summaries) <= MaxSelectionCandidates {
		return summaries
	}
	result := make([]Summary, 0, MaxSelectionCandidates)
	for _, summary := range summaries {
		if explicit[summary.Name] || required[summary.Name] || used[summary.Handle] {
			result = append(result, summary)
		}
	}
	for _, summary := range summaries {
		if len(result) >= MaxSelectionCandidates {
			break
		}
		if explicit[summary.Name] || required[summary.Name] || used[summary.Handle] {
			continue
		}
		result = append(result, summary)
	}
	return result
}

func normalizeSelectionRequest(request SelectionRequest) SelectionRequest {
	if request.Limit <= 0 {
		request.Limit = DefaultSelectionLimit
	}
	request.Limit = min(request.Limit, MaxSelectionLimit)
	if request.Mode != SelectionShadow {
		request.Mode = SelectionCandidate
	}
	request.Explicit = sortedUnique(request.Explicit)
	request.Required = sortedUnique(request.Required)
	request.UsedHandles = sortedUnique(request.UsedHandles)
	return request
}

func scoreSummary(
	queryPhrase string,
	queryTerms []string,
	summary Summary,
) uint32 {
	name := normalizeSelectionText(summary.Name)
	description := normalizeSelectionText(summary.Description)
	nameTerms := termSet(name, 64)
	descriptionTerms := termSet(description, 256)
	var score uint32
	if name != "" && containsSelectionPhrase(queryPhrase, name) {
		score += 256
	}
	var matches uint32
	for _, term := range queryTerms {
		matched := false
		if name == term {
			score += 128
			matched = true
		} else if nameTerms[term] {
			score += 64
			matched = true
		} else if relatedSelectionTerm(nameTerms, term) {
			score += 24
			matched = true
		}
		if descriptionTerms[term] {
			score += 4
			matched = true
		} else if relatedSelectionTerm(descriptionTerms, term) {
			score++
			matched = true
		}
		if matched {
			matches++
		}
	}
	return score + matches*matches
}

func explicitSkillNames(query string, configured []string) map[string]bool {
	result := make(map[string]bool, len(configured))
	for _, name := range configured {
		if namePattern.MatchString(name) {
			result[name] = true
		}
	}
	fields := strings.FieldsFunc(query, func(value rune) bool {
		return unicode.IsSpace(value) || strings.ContainsRune(",;()[]{}", value)
	})
	for _, field := range fields {
		field = strings.Trim(field, "`'\".!?:")
		if strings.HasPrefix(field, "@") || strings.HasPrefix(field, "$") {
			name := strings.ToLower(strings.TrimLeft(field, "@$"))
			if namePattern.MatchString(name) {
				result[name] = true
			}
		}
	}
	return result
}

func selectionTerms(value string) ([]string, bool) {
	stop := map[string]bool{
		"a": true, "an": true, "and": true, "are": true, "as": true,
		"at": true, "be": true, "by": true, "do": true, "for": true,
		"from": true, "how": true, "i": true, "in": true, "is": true,
		"it": true, "me": true, "my": true, "of": true, "on": true,
		"or": true, "please": true, "that": true, "the": true,
		"this": true, "to": true, "use": true, "we": true, "what": true,
		"when": true, "where": true, "which": true, "with": true,
		"you": true, "your": true,
	}
	seen := make(map[string]bool)
	var result []string
	for _, term := range strings.Fields(value) {
		if len([]rune(term)) < 2 || stop[term] || seen[term] {
			continue
		}
		seen[term] = true
		if len(result) == MaxSelectionTerms {
			return result, true
		}
		result = append(result, term)
	}
	return result, false
}

func normalizeSelectionText(value string) string {
	var builder strings.Builder
	separator := true
	for _, character := range value {
		if unicode.IsLetter(character) || unicode.IsDigit(character) {
			builder.WriteRune(unicode.ToLower(character))
			separator = false
		} else if !separator {
			builder.WriteByte(' ')
			separator = true
		}
	}
	return strings.TrimSpace(builder.String())
}

func termSet(value string, limit int) map[string]bool {
	result := make(map[string]bool)
	for _, term := range strings.Fields(value) {
		if len(result) == limit {
			break
		}
		result[term] = true
	}
	return result
}

func relatedSelectionTerm(terms map[string]bool, query string) bool {
	if len([]rune(query)) < 4 {
		return false
	}
	for term := range terms {
		if len([]rune(term)) >= 4 &&
			(strings.HasPrefix(term, query) || strings.HasPrefix(query, term)) {
			return true
		}
	}
	return false
}

func containsSelectionPhrase(haystack, needle string) bool {
	if needle == "" {
		return false
	}
	for offset := 0; offset <= len(haystack)-len(needle); {
		match := strings.Index(haystack[offset:], needle)
		if match < 0 {
			return false
		}
		start := offset + match
		end := start + len(needle)
		leftBoundary := start == 0 || !isASCIISelectionWordByte(haystack[start-1])
		rightBoundary := end == len(haystack) || !isASCIISelectionWordByte(haystack[end])
		if leftBoundary && rightBoundary {
			return true
		}
		offset = start + 1
	}
	return false
}

func isASCIISelectionWordByte(value byte) bool {
	return value >= 'a' && value <= 'z' ||
		value >= '0' && value <= '9'
}

func selectionCacheKey(
	summaries []Summary,
	request SelectionRequest,
) string {
	var builder strings.Builder
	for _, summary := range summaries {
		builder.WriteString(summary.Handle)
		builder.WriteByte('\n')
	}
	builder.WriteString(request.Query)
	builder.WriteByte('\x00')
	builder.WriteString(strings.Join(request.Explicit, "\x00"))
	builder.WriteByte('\x00')
	builder.WriteString(strings.Join(request.Required, "\x00"))
	builder.WriteByte('\x00')
	builder.WriteString(strings.Join(request.UsedHandles, "\x00"))
	fmt.Fprintf(&builder, "\x00%d\x00%s", request.Limit, request.Mode)
	sum := sha256.Sum256([]byte(builder.String()))
	return hex.EncodeToString(sum[:])
}

func (c *Catalog) cachedSelection(key string) (Selection, bool) {
	c.selectionMu.Lock()
	defer c.selectionMu.Unlock()
	value, ok := c.selectionCache[key]
	return cloneSelection(value), ok
}

func (c *Catalog) cacheSelection(key string, value Selection) {
	c.selectionMu.Lock()
	defer c.selectionMu.Unlock()
	if _, exists := c.selectionCache[key]; !exists {
		c.selectionOrder = append(c.selectionOrder, key)
	}
	c.selectionCache[key] = cloneSelection(value)
	for len(c.selectionOrder) > maxSelectionCacheItems {
		delete(c.selectionCache, c.selectionOrder[0])
		c.selectionOrder = c.selectionOrder[1:]
	}
}

func cloneSelection(value Selection) Selection {
	value.Candidates = append([]Summary(nil), value.Candidates...)
	value.Visible = append([]Summary(nil), value.Visible...)
	return value
}

func estimateMetadataTokens(values []Summary) uint64 {
	var tokens uint64
	for _, value := range values {
		tokens += tokenestimate.Text(value.Name) +
			tokenestimate.Text(value.Description) +
			tokenestimate.Text(string(value.Source)) +
			tokenestimate.Text(value.Handle) +
			// Structural overhead of one catalog entry (key names, quotes,
			// separators) beyond its text fields.
			32
	}
	return tokens
}

func summaryDisabledForModel(value Summary) bool {
	return !value.ModelInvocable
}

func sortedUnique(values []string) []string {
	result := append([]string(nil), values...)
	sort.Strings(result)
	write := 0
	for _, value := range result {
		value = strings.TrimSpace(value)
		if value == "" || write != 0 && result[write-1] == value {
			continue
		}
		result[write] = value
		write++
	}
	return result[:write]
}

func boundUTF8(value string, limit int) (string, bool) {
	if len(value) <= limit {
		return value, false
	}
	end := limit
	for end > 0 && !utf8.ValidString(value[:end]) {
		end--
	}
	return value[:end], true
}
