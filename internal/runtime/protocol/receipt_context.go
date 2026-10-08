package protocol

// ReceiptContextProjection is the exact selection sealed before a provider
// attempt. It contains identities and byte ranges, never source text. Its
// source directory belongs to session diagnostics, not metric labels.
type ReceiptContextProjection struct {
	Digest              string                         `json:"digest"`
	SourceHistoryDigest string                         `json:"source_history_digest"`
	WindowID            string                         `json:"window_id,omitempty"`
	WindowNumber        uint64                         `json:"window_number,omitempty"`
	RouteDigest         string                         `json:"route_digest,omitempty"`
	ContextDigest       string                         `json:"context_digest,omitempty"`
	ContextRevision     uint64                         `json:"context_revision,omitempty"`
	InputTokens         uint64                         `json:"input_tokens"`
	OutputReserve       uint64                         `json:"output_reserve"`
	RawTokens           uint64                         `json:"raw_tokens"`
	RawTokenLimit       uint64                         `json:"raw_token_limit,omitempty"`
	RawTokenLimited     bool                           `json:"raw_token_limited,omitempty"`
	TailStart           int                            `json:"tail_start"`
	LimitingConstraint  string                         `json:"limiting_constraint,omitempty"`
	RecoveryOnly        bool                           `json:"recovery_only"`
	Selected            []ReceiptContextSource         `json:"selected,omitempty"`
	Omissions           []ReceiptContextOmission       `json:"omissions,omitempty"`
	References          []ReceiptContextRepresentation `json:"references,omitempty"`
}

type ReceiptContextSource struct {
	Index  int    `json:"index"`
	Turn   uint64 `json:"turn,omitempty"`
	Digest string `json:"digest"`
}

type ReceiptContextOmission struct {
	Source        ReceiptContextSource `json:"source"`
	Reason        string               `json:"reason"`
	RetrievalTurn uint64               `json:"retrieval_turn,omitempty"`
}

// Ranges use UTF-8 byte offsets in the source identified by ContentDigest.
// Coverage is mechanical evidence, not a claim of semantic faithfulness.
type ReceiptContextRepresentation struct {
	SourceID       string                `json:"source_id"`
	SourceTurn     uint64                `json:"source_turn,omitempty"`
	ContentDigest  string                `json:"content_digest"`
	ItemIDs        []string              `json:"item_ids,omitempty"`
	Ranges         []ReceiptContextRange `json:"ranges,omitempty"`
	Representation string                `json:"representation"`
	Status         string                `json:"status"`
	Reason         string                `json:"reason,omitempty"`
}

type ReceiptContextRange struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// ReceiptContextRecovery counts completed foreground retrieval calls. Bytes
// count the returned UTF-8 tool surface (including metadata); failed calls have
// no recovered bytes. These are observations, not a necessity classifier.
type ReceiptContextRecovery struct {
	Calls  int    `json:"calls"`
	Failed int    `json:"failed"`
	Bytes  uint64 `json:"bytes"`
}
