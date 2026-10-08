package prompt

import (
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type CompactionReceipt struct {
	CompactionID              string                            `json:"compaction_id,omitempty"`
	Status                    string                            `json:"status,omitempty"`
	Mode                      string                            `json:"mode,omitempty"`
	SourceWindowID            string                            `json:"source_window_id,omitempty"`
	TargetWindowID            string                            `json:"target_window_id,omitempty"`
	Phase                     string                            `json:"phase,omitempty"`
	OriginalMessages          int                               `json:"original_messages"`
	RemovedMessages           int                               `json:"removed_messages"`
	OriginalBytes             int                               `json:"original_bytes"`
	RetainedBytes             int                               `json:"retained_bytes"`
	OriginalTokens            uint64                            `json:"original_tokens"`
	RetainedTokens            uint64                            `json:"retained_tokens"`
	SummaryOriginalBytes      int                               `json:"summary_original_bytes"`
	SummaryRetainedBytes      int                               `json:"summary_retained_bytes"`
	SummaryTruncated          bool                              `json:"summary_truncated"`
	TruncationReason          string                            `json:"truncation_reason,omitempty"`
	PrunedToolResults         int                               `json:"pruned_tool_results,omitempty"`
	PrunedBytes               int                               `json:"pruned_bytes,omitempty"`
	TruthGeneration           uint64                            `json:"truth_generation,omitempty"`
	TruthEntities             int                               `json:"truth_entities,omitempty"`
	CriticalFacts             int                               `json:"critical_facts,omitempty"`
	CompatibilityHash         string                            `json:"compatibility_hash,omitempty"`
	CompatibilityMatched      bool                              `json:"compatibility_matched,omitempty"`
	AuthorityDigest           string                            `json:"authority_digest,omitempty"`
	AuthorityEquivalent       bool                              `json:"authority_equivalent,omitempty"`
	ModelDownshifted          bool                              `json:"model_downshifted,omitempty"`
	DownshiftPolicy           string                            `json:"downshift_policy,omitempty"`
	NarrativeIncluded         bool                              `json:"narrative_included,omitempty"`
	NarrativeDurationMS       int64                             `json:"narrative_duration_ms,omitempty"`
	NarrativeBackground       bool                              `json:"narrative_background,omitempty"`
	NarrativeForegroundWaitMS uint64                            `json:"narrative_foreground_wait_ms"`
	NarrativeCostMicrounits   uint64                            `json:"narrative_cost_microunits,omitempty"`
	NarrativeCostKnown        bool                              `json:"narrative_cost_known"`
	NarrativeBytes            int                               `json:"narrative_bytes,omitempty"`
	NarrativeInputTokens      uint64                            `json:"narrative_input_tokens,omitempty"`
	NarrativeOutputTokens     uint64                            `json:"narrative_output_tokens,omitempty"`
	NarrativeProvider         string                            `json:"narrative_provider,omitempty"`
	NarrativeModel            string                            `json:"narrative_model,omitempty"`
	NarrativeMetadata         *protocol.ModelMetadataProvenance `json:"narrative_metadata_provenance,omitempty"`
	FallbackReason            string                            `json:"fallback_reason,omitempty"`
	CapsuleBytes              int                               `json:"capsule_bytes,omitempty"`
	MandatoryBytes            int                               `json:"mandatory_bytes,omitempty"`
	MandatoryEntities         int                               `json:"mandatory_entities,omitempty"`
	OmissionCount             int                               `json:"omission_count,omitempty"`
	Retention                 []agentcontext.RetentionCount     `json:"retention,omitempty"`
	Sections                  []string                          `json:"sections,omitempty"`
	RemovedTurns              []uint64                          `json:"removed_turns"`
	ContextReceipts           []Receipt                         `json:"context_receipts"`
	WorkingSet                []string                          `json:"working_set"`
	CriticalPaths             []string                          `json:"critical_paths"`
}

func NewPruningReceipt(
	selection agentcontext.CompactionSelection,
	authorityDigest string,
	contextReceipts []Receipt,
) *CompactionReceipt {
	return &CompactionReceipt{
		OriginalMessages:    selection.OriginalMessages,
		OriginalBytes:       selection.OriginalBytes,
		RetainedBytes:       agentcontext.HistoryBytes(selection.History),
		OriginalTokens:      selection.OriginalWindow.Active,
		RetainedTokens:      selection.RetainedWindow.Active,
		TruncationReason:    "tool_result_surface_pruning",
		PrunedToolResults:   selection.Pruning.Results,
		PrunedBytes:         selection.Pruning.Bytes,
		AuthorityDigest:     authorityDigest,
		AuthorityEquivalent: true,
		ContextReceipts:     append([]Receipt(nil), contextReceipts...),
	}
}

func NewCompactionReceipt(
	selection agentcontext.CompactionSelection,
	summaryLineBytes int,
	contextReceipts []Receipt,
	workingSet []string,
	criticalPaths []string,
) *CompactionReceipt {
	selected := selection.Candidate
	if selected == nil {
		return nil
	}
	receipt := &CompactionReceipt{
		OriginalMessages: selection.OriginalMessages,
		RemovedMessages:  selected.Cut,
		OriginalBytes:    selection.OriginalBytes,
		RetainedBytes:    selected.RetainedBytes,
		OriginalTokens:   selection.OriginalWindow.Active,
		RetainedTokens:   selected.RetainedTokens,
		SummaryOriginalBytes: agentcontext.SummaryOriginalBytes(
			selected.ToSummarize,
			summaryLineBytes,
		),
		SummaryRetainedBytes: len(selected.Rendered),
		SummaryTruncated:     selected.SummaryTruncated,
		Sections:             append([]string(nil), selected.Sections...),
		RemovedTurns:         agentcontext.UniqueMessageTurns(selected.Removed),
		PrunedToolResults:    selection.Pruning.Results,
		PrunedBytes:          selection.Pruning.Bytes,
		TruthGeneration:      selected.Truth.Generation,
		TruthEntities:        selected.Truth.EntityCount,
		CriticalFacts:        selected.Truth.CriticalEntityCount,
		CompatibilityHash:    selected.CompatibilityHash,
		CompatibilityMatched: selected.Truth.CompatibilityMatched,
		AuthorityDigest:      selected.AuthorityDigest,
		AuthorityEquivalent:  true,
		ModelDownshifted:     selected.Truth.ModelDownshifted,
		DownshiftPolicy:      agentcontext.DownshiftRuntimeTruthOnly,
		NarrativeIncluded:    selected.NarrativeIncluded,
		CapsuleBytes:         selected.CapsuleBytes,
		MandatoryBytes:       selected.Retention.MandatoryBytes,
		MandatoryEntities:    selected.Retention.MandatoryEntities,
		OmissionCount:        selected.Retention.OmissionCount,
		Retention: append(
			[]agentcontext.RetentionCount(nil),
			selected.Retention.ByClass...,
		),
		ContextReceipts: append([]Receipt(nil), contextReceipts...),
		WorkingSet:      append([]string(nil), workingSet...),
		CriticalPaths:   append([]string(nil), criticalPaths...),
	}
	if selected.SummaryTruncated {
		receipt.TruncationReason = "summary_byte_budget"
	}
	return receipt
}
