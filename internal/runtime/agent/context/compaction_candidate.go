package agentcontext

import (
	"sort"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
)

const DefaultMaxDigestEntries = 120

type SummaryRequest struct {
	Plan             Plan
	Removed          []provider.Message
	Turn             uint64
	WorkingSetLimit  int
	EvidenceLimit    int
	MaxDigestEntries int
	SummaryLineBytes int
}

type SummaryResult struct {
	Summary       Summary
	WorkingSet    []string
	CriticalPaths []string
}

func (a Authority) BuildSummary(request SummaryRequest) SummaryResult {
	summary := Summary{Window: len(request.Removed)}
	summary.Goal = request.Plan.Objective
	if summary.Goal == "" {
		summary.Goal = request.Plan.Title
	}
	open, done := request.Plan.OutstandingSteps()
	summary.DoneTodos = done
	for _, step := range open {
		summary.Todos = append(summary.Todos, Todo{
			Title: step.Title, Status: step.Status,
		})
	}
	summary.Failures = a.Failures().List()
	for _, change := range a.Evidence().Changes() {
		summary.Changes = append(summary.Changes, CompactionChange{
			Path: change.Path, Turn: change.Turn, Read: change.Read,
			Verified: change.Verified, Diagnostics: change.Diagnostics,
			Stale: change.Stale,
		})
	}
	SortChanges(summary.Changes)
	var paths, critical []string
	for _, entry := range a.WorkingSet().Select(
		request.Turn,
		request.WorkingSetLimit,
	) {
		paths = append(paths, entry.Path)
		if entry.Critical {
			critical = append(critical, entry.Path)
		}
	}
	sort.Strings(paths)
	sort.Strings(critical)
	summary.CriticalPaths = append([]string(nil), critical...)
	snapshot := a.Evidence().Snapshot(request.EvidenceLimit)
	for _, fact := range snapshot.Facts {
		summary.Facts = append(summary.Facts, CompactionFact{
			Line: fact.Describe(),
		})
	}
	summary.OmittedFacts = snapshot.OmittedFacts
	limit := request.MaxDigestEntries
	if limit <= 0 {
		limit = DefaultMaxDigestEntries
	}
	lineBytes := request.SummaryLineBytes
	if lineBytes <= 0 {
		lineBytes = 512
	}
	for index := len(request.Removed) - 1; index >= 0; index-- {
		message := request.Removed[index]
		if _, ok := Carry(message.Text()); ok {
			continue
		}
		if len(summary.Digest) == limit {
			continue
		}
		summary.Digest = append(
			summary.Digest,
			SummaryLine(message, lineBytes),
		)
	}
	return SummaryResult{
		Summary: summary, WorkingSet: paths, CriticalPaths: critical,
	}
}

type CompactionCandidate struct {
	Cut                 int
	History             []provider.Message
	SourceHistory       []provider.Message
	Removed             []provider.Message
	ToSummarize         []provider.Message
	Rendered            string
	SourceBytes         int
	RetainedBytes       int
	SourceTokens        uint64
	RetainedTokens      uint64
	SummaryTruncated    bool
	Sections            []string
	Truth               MergeReceipt
	Capsule             TruthCapsule
	Authority           TruthCapsule
	CompatibilityHash   string
	AuthorityDigest     string
	NarrativeIncluded   bool
	CapsuleBytes        int
	Retention           RetentionReceipt
	SourceWindowID      string
	SourceContextDigest string
	SourceHistoryDigest string
	RequiredKinds       []string
}

type CompactionCandidateInput struct {
	Cut              int
	Removed          []provider.Message
	ToSummarize      []provider.Message
	Tail             []provider.Message
	OriginalHistory  []provider.Message
	Summary          Summary
	CurrentTruth     TruthCapsule
	RetentionPolicy  RetentionPolicy
	Turn             uint64
	SummaryMaxBytes  int
	IncludeNarrative bool
	PinnedUser       *provider.Message
}

// BuildCompactionCandidate performs the deterministic portion of compaction:
// merging prior truth, applying retention, rendering the structured summary and
// rebuilding a tool-pair-safe history window.
func BuildCompactionCandidate(
	input CompactionCandidateInput,
) (CompactionCandidate, error) {
	previous, err := truthCapsules(input.ToSummarize)
	if err != nil {
		return CompactionCandidate{}, err
	}
	current := input.CurrentTruth
	currentHasGoal := false
	for _, entity := range current.Entities {
		currentHasGoal = currentHasGoal || entity.Kind == EntityGoal
	}
	if !currentHasGoal {
		for index := len(previous) - 1; index >= 0 && !currentHasGoal; index-- {
			for _, entity := range previous[index].Entities {
				if entity.Kind != EntityGoal ||
					entity.Retention != RetentionMandatory {
					continue
				}
				current.Entities = append(current.Entities, entity)
				currentHasGoal = true
				break
			}
		}
		if currentHasGoal {
			current.Seal()
		}
	}
	authority := MandatoryCapsule(current)
	capsule, mergeReceipt, err := MergeTruthCapsules(
		current,
		previous...,
	)
	if err != nil {
		return CompactionCandidate{}, err
	}
	capsule, retention, err := PlanRetention(
		capsule,
		input.RetentionPolicy,
		input.Turn,
	)
	if err != nil {
		return CompactionCandidate{}, err
	}
	mergeReceipt.EntityCount = len(capsule.Entities)
	mergeReceipt.CriticalEntityCount = 0
	for _, entity := range capsule.Entities {
		if entity.Kind == EntityFact || entity.Kind == EntityCriticalPath {
			mergeReceipt.CriticalEntityCount++
		}
	}
	narrative := Narrative{}
	renderSummary := input.Summary
	if !input.IncludeNarrative {
		renderSummary = Summary{Window: input.Summary.Window}
	}
	rendered, err := RenderStructured(
		renderSummary,
		capsule,
		narrative,
		input.SummaryMaxBytes,
	)
	if err != nil {
		return CompactionCandidate{}, err
	}
	assemble := func(rendered StructuredRender) []provider.Message {
		compacted := provider.TextMessage(provider.RoleSystem, rendered.Text)
		if input.PinnedUser != nil {
			compacted.Turn = input.PinnedUser.Turn
			return append(
				[]provider.Message{CloneMessage(*input.PinnedUser), compacted},
				CloneMessages(input.Tail)...,
			)
		}
		return append(
			[]provider.Message{compacted},
			CloneMessages(input.Tail)...,
		)
	}
	history := assemble(rendered)
	if HistoryBytes(history) >= HistoryBytes(input.OriginalHistory) &&
		(rendered.NarrativeIncluded || len(rendered.Sections) > 1) {
		rendered, err = RenderStructured(
			Summary{Window: input.Summary.Window},
			capsule,
			Narrative{},
			input.SummaryMaxBytes,
		)
		if err != nil {
			return CompactionCandidate{}, err
		}
		history = assemble(rendered)
	}
	return CompactionCandidate{
		Cut: input.Cut, History: history,
		SourceHistory:    CloneMessages(input.OriginalHistory),
		Removed:          CloneMessages(input.Removed),
		ToSummarize:      CloneMessages(input.ToSummarize),
		Rendered:         rendered.Text,
		SourceBytes:      HistoryBytes(input.OriginalHistory),
		RetainedBytes:    HistoryBytes(history),
		SummaryTruncated: rendered.Truncated,
		Sections:         append([]string(nil), rendered.Sections...),
		Truth:            mergeReceipt, Capsule: capsule, Authority: authority,
		CompatibilityHash:   capsule.CompatibilityHash,
		NarrativeIncluded:   rendered.NarrativeIncluded,
		CapsuleBytes:        rendered.CapsuleBytes,
		Retention:           retention,
		SourceHistoryDigest: HistoryDigest(input.OriginalHistory),
		RequiredKinds:       RequiredNarrativeKinds(input.Summary),
	}, nil
}

func RequiredNarrativeKinds(summary Summary) []string {
	var required []string
	if len(summary.Changes) != 0 || len(summary.CriticalPaths) != 0 {
		required = append(required, NarrativeFileCode)
	}
	if len(summary.Todos) != 0 {
		required = append(required, NarrativeCurrent, NarrativeNextStep)
	}
	sort.Strings(required)
	return required
}

func truthCapsules(messages []provider.Message) ([]TruthCapsule, error) {
	var result []TruthCapsule
	for _, message := range messages {
		capsule, found, err := ParseTruthCapsule(message.Text())
		if err != nil {
			return nil, err
		}
		if found {
			result = append(result, capsule)
		}
	}
	return result, nil
}
