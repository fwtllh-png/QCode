package agentcontext

import (
	"encoding/json"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
)

type OmissionReason string

const (
	OmittedTurnLimit         OmissionReason = "recent_tail_turns"
	OmittedTokenLimit        OmissionReason = "history_token_ceiling"
	OmittedCapacity          OmissionReason = "context_capacity"
	OmittedOperatorCeiling   OmissionReason = "operator_context_ceiling"
	OmittedProviderOverflow  OmissionReason = "provider_overflow"
	OmittedEconomicBudget    OmissionReason = "economic_budget"
	OmittedThroughput        OmissionReason = "provider_throughput"
	OmittedSourceUnavailable OmissionReason = "source_unavailable"
)

// ProjectionSource identifies a raw message within SourceHistoryDigest. Index
// is scoped to that immutable source, not a durable conversation-item ID.
type ProjectionSource struct {
	Index  int    `json:"index"`
	Turn   uint64 `json:"turn,omitempty"`
	Digest string `json:"digest"`
}

// TurnRetrieval uses the existing turn_history input contract. A source without
// a turn has no invented recovery address.
type TurnRetrieval struct {
	Turn uint64 `json:"turn"`
}

type ProjectionOmission struct {
	Source    ProjectionSource `json:"source"`
	Reason    OmissionReason   `json:"reason"`
	Retrieval *TurnRetrieval   `json:"retrieval,omitempty"`
}

// ProjectionResult couples raw-history selection with its exact omissions.
// Messages are detached from durable history. World-state rendering and final
// normalized request accounting are installed by the model-input owner.
type ProjectionResult struct {
	References          []ReferenceCoverage  `json:"references,omitempty"`
	Representations     []ReferenceCoverage  `json:"representations,omitempty"`
	SourceHistoryDigest string               `json:"source_history_digest"`
	SourceWindowID      string               `json:"source_window_id,omitempty"`
	SourceWindowNumber  uint64               `json:"source_window_number,omitempty"`
	RouteDigest         string               `json:"route_digest,omitempty"`
	TailStart           int                  `json:"tail_start"`
	Selected            []ProjectionSource   `json:"selected,omitempty"`
	Omissions           []ProjectionOmission `json:"omissions,omitempty"`
	RawTokens           uint64               `json:"raw_tokens"`
	RawTokenLimit       uint64               `json:"raw_token_limit,omitempty"`
	RawTokenLimited     bool                 `json:"raw_token_limited,omitempty"`
	LimitingConstraint  OmissionReason       `json:"limiting_constraint,omitempty"`
	ContextRevision     uint64               `json:"context_revision,omitempty"`
	ContextDigest       string               `json:"context_digest,omitempty"`
	InputTokens         uint64               `json:"input_tokens,omitempty"`
	OutputReserve       uint64               `json:"output_reserve,omitempty"`
	Digest              string               `json:"digest"`
	Messages            []provider.Message   `json:"-"`
}

func (p *ProjectionResult) Seal() {
	p.Digest = ""
	encoded, _ := json.Marshal(p)
	p.Digest = digestString(string(encoded))
}

// ReferenceCoverage describes exact source ranges represented in this request.
// It proves mechanical coverage only, not semantic correctness.
type ReferenceCoverage struct {
	SourceID       string           `json:"source_id"`
	SourceTurn     uint64           `json:"source_turn"`
	ContentDigest  string           `json:"content_digest"`
	ItemIDs        []string         `json:"item_ids,omitempty"`
	Ranges         []ReferenceRange `json:"ranges"`
	Representation string           `json:"representation"`
	Status         string           `json:"status,omitempty"`
	Reason         string           `json:"reason,omitempty"`
}

type ReferenceRange struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

type ConversationExcerpt struct {
	Source   ConversationSource
	Coverage ReferenceCoverage
}
