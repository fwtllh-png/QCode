package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

type EventKind string

const (
	EventTurnStarted         EventKind = "turn.started"
	EventOutputDelta         EventKind = "output.delta"
	EventOutputDraft         EventKind = "output.draft"
	EventOutputDiscarded     EventKind = "output.discarded"
	EventReasoningDelta      EventKind = "reasoning.delta"
	EventCommentaryCompleted EventKind = "commentary.completed"
	EventSessionTitleUpdated EventKind = "session.title.updated"
	EventReasoningCompleted  EventKind = "reasoning.completed"
	EventSearchResult        EventKind = "search.result"
	EventCitation            EventKind = "citation"
	EventUsage               EventKind = "usage"
	EventProviderAttempt     EventKind = "provider.attempt"
	EventToolState           EventKind = "tool.state"
	EventToolStart           EventKind = "tool.start"
	EventToolOutput          EventKind = "tool.output"
	EventToolResult          EventKind = "tool.result"
	EventToolCatalogChanged  EventKind = "tool.catalog.changed"
	EventMCPHealthChanged    EventKind = "mcp.health.changed"
	EventExtensionControl    EventKind = "extension.control"
	EventDiagnostics         EventKind = "diagnostics.result"
	EventTurnCompleted       EventKind = "turn.completed"
	EventTurnFailed          EventKind = "turn.failed"
	EventTurnCanceled        EventKind = "turn.canceled"
	EventOperationRejected   EventKind = "operation.rejected"
	EventTurnSteered         EventKind = "turn.steered"
	EventTurnQueued          EventKind = "turn.queued"
	EventQueuedTurnUpdated   EventKind = "turn.queue.updated"
	EventQueuedTurnRemoved   EventKind = "turn.queue.removed"
	EventApprovalRequired    EventKind = "approval.required"
	EventApprovalResolved    EventKind = "approval.resolved"
	EventInputRequired       EventKind = "input.required"
	EventInputResolved       EventKind = "input.resolved"
	EventThreadCompacted     EventKind = "thread.compacted"
	EventThreadForked        EventKind = "thread.forked"
	EventTurnReverted        EventKind = "turn.reverted"
	EventCheckpointCreated   EventKind = "checkpoint.created"
	EventCheckpointRestored  EventKind = "checkpoint.restored"
	EventTurnWithdrawn       EventKind = "turn.withdrawn"
	EventCheckpointForked    EventKind = "checkpoint.forked"
	EventTurnCompaction      EventKind = "turn.compaction"
	EventTurnVerification    EventKind = "turn.verification"
	EventAgentSpawned        EventKind = "agent.spawned"
	EventAgentStatus         EventKind = "agent.status"
	EventAgentMessage        EventKind = "agent.message"
	EventAgentIntegration    EventKind = "agent.integration"
	EventPlanDelta           EventKind = "plan.delta"
	EventCommandExecution    EventKind = "command.execution"
	EventHostCommand         EventKind = "host.command"
	EventExecutionReceipt    EventKind = "turn.receipt"
)

type EventData interface {
	eventKind() EventKind
	validate() error
}

// EventSessionID returns the session identity declared by event payloads that
// are not emitted on a session-owned thread.
func EventSessionID(data EventData) string {
	switch value := data.(type) {
	case *AgentSpawnedData:
		return value.SessionID
	case *AgentStatusData:
		return value.SessionID
	case *AgentMessageData:
		return value.SessionID
	case *AgentIntegrationData:
		return value.SessionID
	default:
		return ""
	}
}

// UnknownEventData preserves one same-version Event kind that this build does
// not understand. Hosts may display it as read-only protocol data, but Runtime
// semantics never infer lifecycle or terminal state from it.
type UnknownEventData struct {
	Kind EventKind       `json:"-"`
	Raw  json.RawMessage `json:"-"`
}

func (d *UnknownEventData) eventKind() EventKind {
	if d == nil {
		return ""
	}
	return d.Kind
}

func (d *UnknownEventData) validate() error {
	if d == nil || d.Kind == "" {
		return errors.New("unknown event kind is required")
	}
	if len(bytes.TrimSpace(d.Raw)) == 0 ||
		bytes.Equal(bytes.TrimSpace(d.Raw), []byte("null")) ||
		!json.Valid(d.Raw) {
		return errors.New("unknown event data must be valid non-null JSON")
	}
	return nil
}

func (d UnknownEventData) MarshalJSON() ([]byte, error) {
	if err := (&d).validate(); err != nil {
		return nil, err
	}
	return append([]byte(nil), d.Raw...), nil
}

type TurnStartedData struct {
	Provider           string                   `json:"provider"`
	Model              string                   `json:"model"`
	ModelMetadata      *ModelMetadataProvenance `json:"model_metadata_provenance,omitempty"`
	QueueID            string                   `json:"queue_id,omitempty"`
	PlanID             string                   `json:"plan_id,omitempty"`
	PlanTransition     PlanTransition           `json:"plan_transition,omitempty"`
	ProfileRevision    uint64                   `json:"profile_revision,omitempty"`
	Intent             TurnIntent               `json:"intent,omitempty"`
	Mode               string                   `json:"mode,omitempty"`
	Posture            string                   `json:"posture,omitempty"`
	Workspace          string                   `json:"workspace,omitempty"`
	WorkspaceIsolation string                   `json:"workspace_isolation,omitempty"`
	Sandbox            string                   `json:"sandbox,omitempty"`
	// Prompt is model-visible durable reconstruction input. Optional for older events.
	Prompt string `json:"prompt,omitempty"`
	// DisplayPrompt omits expanded editor context and is safe for chat projection.
	DisplayPrompt string `json:"display_prompt,omitempty"`
	// EditorContext is the Runtime-validated, model-visible context audit.
	EditorContext []EditorContextReceipt `json:"editor_context,omitempty"`
	// Images are the validated image inputs shown with the durable user message.
	Images []EditorContextReference `json:"images,omitempty"`
}

func (*TurnStartedData) eventKind() EventKind { return EventTurnStarted }

func (d *TurnStartedData) validate() error {
	var planErr error
	if d.PlanID != "" || d.PlanTransition != "" {
		if !validProfileIdentifier(d.PlanID) {
			planErr = errors.New("turn started plan id is invalid")
		} else if d.PlanTransition != PlanTransitionImplement &&
			d.PlanTransition != PlanTransitionAutopilot {
			planErr = errors.New("turn started plan transition is invalid")
		} else if d.ProfileRevision == 0 {
			planErr = errors.New("turn started plan profile revision is required")
		}
	}
	var metadataErr error
	if d.ModelMetadata != nil {
		metadataErr = d.ModelMetadata.Validate()
	}
	return errors.Join(
		require(d.Provider != "" && d.Model != "", "turn started provider and model are required"),
		planErr,
		metadataErr,
		require(NormalizeTurnIntent(d.Intent).Valid(), "turn started intent is invalid"),
		require(slices.Contains([]string{"", "shared", "worktree"}, d.WorkspaceIsolation), "turn started workspace isolation is invalid"),
		require(!slices.ContainsFunc(d.Images, func(value EditorContextReference) bool { return value.Kind != EditorContextImage }), "turn images must contain only image context"),
		validateEditorContextReceipts(d.EditorContext),
		validateEditorContextReferences(d.Images, "turn images"),
	)
}

type TextDeltaData struct {
	Text string `json:"text"`
}

func (d *TextDeltaData) validate() error { return require(d.Text != "", "delta text is required") }

type OutputDeltaData TextDeltaData

func (*OutputDeltaData) eventKind() EventKind { return EventOutputDelta }

func (d *OutputDeltaData) validate() error { return (*TextDeltaData)(d).validate() }

// OutputDraftData is the live, provisional body text of one model sample. It
// streams while the turn is still running and is never persisted: the durable
// output.delta projection publishes the committed answer after the terminal
// envelope settles. A consumer renders drafts optimistically and retracts them
// on OutputDiscardedData or the terminal event.
type OutputDraftData struct {
	Text     string `json:"text"`
	SampleID string `json:"sample_id,omitempty"`
}

func (*OutputDraftData) eventKind() EventKind { return EventOutputDraft }

func (d *OutputDraftData) validate() error {
	return require(d.Text != "", "draft delta text is required")
}

// OutputDiscardedData retracts the provisional output.draft text already
// streamed for one model sample. Drafts are optimistic: text that arrives
// with tool calls is narration rather than the final answer, so the sample's
// streamed draft is retracted before the tool phase. The narration itself is
// republished through its durable owner (commentary), and the event is
// transient and never enters the durable event log.
type OutputDiscardedData struct {
	SampleID string `json:"sample_id"`
	Reason   string `json:"reason,omitempty"`
}

func (*OutputDiscardedData) eventKind() EventKind { return EventOutputDiscarded }

func (d *OutputDiscardedData) validate() error {
	return require(strings.TrimSpace(d.SampleID) != "", "discarded output sample id is required")
}

// CommentaryCompletedData is a confirmed, non-terminal assistant message.
type CommentaryCompletedData struct {
	MessageID string   `json:"message_id"`
	SampleID  string   `json:"sample_id"`
	Text      string   `json:"text"`
	CallIDs   []string `json:"call_ids"`
}

type SessionTitleUpdatedData struct {
	SessionID     string             `json:"session_id"`
	Title         string             `json:"title"`
	TitleSource   SessionTitleSource `json:"title_source"`
	TitleRevision uint64             `json:"title_revision"`
}

func (*SessionTitleUpdatedData) eventKind() EventKind { return EventSessionTitleUpdated }
func (d *SessionTitleUpdatedData) validate() error {
	if !validProfileIdentifier(d.SessionID) || d.TitleRevision == 0 ||
		(d.TitleSource != SessionTitleTemporary && d.TitleSource != SessionTitleAuto) {
		return errors.New("session title update is invalid")
	}
	return ValidateSessionTitle(d.Title)
}

func (*CommentaryCompletedData) eventKind() EventKind { return EventCommentaryCompleted }

func (d *CommentaryCompletedData) validate() error {
	seen := make(map[string]bool, len(d.CallIDs))
	for _, id := range d.CallIDs {
		if strings.TrimSpace(id) == "" || seen[id] {
			return errors.New("commentary call ids must be nonempty and unique")
		}
		seen[id] = true
	}
	return require(
		strings.TrimSpace(d.MessageID) != "" && strings.TrimSpace(d.SampleID) != "" &&
			strings.TrimSpace(d.Text) != "" && len(d.CallIDs) != 0,
		"commentary message, sample, text and call ids are required",
	)
}

type ReasoningDeltaData struct {
	Text     string `json:"text"`
	SampleID string `json:"sample_id,omitempty"`
}

func (*ReasoningDeltaData) eventKind() EventKind { return EventReasoningDelta }

func (d *ReasoningDeltaData) validate() error {
	return require(d.Text != "", "reasoning delta text is required")
}

// ReasoningCompletedData retains one complete model-sample reasoning block.
// Streaming deltas remain transient and are replaced by this durable fact.
type ReasoningCompletedData struct {
	Text     string `json:"text"`
	SampleID string `json:"sample_id"`
}

func (*ReasoningCompletedData) eventKind() EventKind {
	return EventReasoningCompleted
}

func (d *ReasoningCompletedData) validate() error {
	return require(
		d.Text != "" && d.SampleID != "",
		"completed reasoning text and sample_id are required",
	)
}

type Source struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	URL   string `json:"url"`
}

type SearchResultData struct {
	Query   string   `json:"query"`
	Sources []Source `json:"sources"`
}

func (*SearchResultData) eventKind() EventKind { return EventSearchResult }

func (d *SearchResultData) validate() error {
	if d.Query == "" {
		return errors.New("search query is required")
	}
	for _, source := range d.Sources {
		if source.URL == "" {
			return errors.New("search source url is required")
		}
	}
	return nil
}

type CitationData struct {
	SourceID string `json:"source_id"`
	Title    string `json:"title"`
	URL      string `json:"url"`
	Start    int    `json:"start"`
	End      int    `json:"end"`
}

func (*CitationData) eventKind() EventKind { return EventCitation }

func (d *CitationData) validate() error {
	return require(d.URL != "" && d.Start >= 0 && d.End >= d.Start, "citation url and valid range are required")
}

// UsageData is one provider call's cumulative usage. Aggregators retain the
// latest event per Sample and sum across Samples to avoid double-counting.
type UsageData struct {
	Sample        uint32                   `json:"sample"`
	Provider      string                   `json:"provider,omitempty"`
	Model         string                   `json:"model,omitempty"`
	ModelMetadata *ModelMetadataProvenance `json:"model_metadata_provenance,omitempty"`
	Context       *SampleContextData       `json:"context,omitempty"`

	InputTokens     uint64 `json:"input_tokens"`
	OutputTokens    uint64 `json:"output_tokens"`
	ReasoningTokens uint64 `json:"reasoning_tokens"`
	CachedTokens    uint64 `json:"cached_tokens,omitempty"`
	CostMicrounits  uint64 `json:"cost_microunits,omitempty"`
	CostKnown       bool   `json:"cost_known"`
}

type ProviderProjectionData struct {
	Mode                       string `json:"mode"`
	IncrementalEligible        bool   `json:"incremental_eligible,omitempty"`
	FallbackReason             string `json:"fallback_reason,omitempty"`
	RouteDigest                string `json:"route_digest,omitempty"`
	PropertyDigest             string `json:"property_digest,omitempty"`
	StablePrefixDigest         string `json:"stable_prefix_digest,omitempty"`
	InputDigest                string `json:"input_digest,omitempty"`
	DeltaDigest                string `json:"delta_digest,omitempty"`
	ContextRevision            uint64 `json:"context_revision,omitempty"`
	WindowID                   string `json:"window_id,omitempty"`
	WindowNumber               uint64 `json:"window_number,omitempty"`
	LogicalItems               int    `json:"logical_items,omitempty"`
	TransportItems             int    `json:"transport_items,omitempty"`
	LogicalTransportEquivalent bool   `json:"logical_transport_equivalent"`
}

const (
	ProviderAttemptStarted    = "started"
	ProviderAttemptRetryWait  = "retry_wait"
	ProviderAttemptIncomplete = "incomplete"
	ProviderAttemptCompleted  = "completed"
	ProviderAttemptFailed     = "failed"
)

// ProviderAttemptData is a content-safe transport fact. It distinguishes
// Provider retries, genuine incomplete output recovery, and normal tool-use
// boundaries without relying on assistant-generated prose.
type ProviderAttemptData struct {
	SampleID              string                  `json:"sample_id"`
	Attempt               uint32                  `json:"attempt"`
	Status                string                  `json:"status"`
	Reason                string                  `json:"reason,omitempty"`
	FailureCode           string                  `json:"failure_code,omitempty"`
	ErrorCode             ErrorCode               `json:"error_code,omitempty"`
	HTTPStatus            int                     `json:"http_status,omitempty"`
	ProviderRetryAfterMS  uint64                  `json:"provider_retry_after_ms,omitempty"`
	EffectiveDelayMS      uint64                  `json:"effective_delay_ms,omitempty"`
	RouteCooldownWaitMS   uint64                  `json:"route_cooldown_wait_ms,omitempty"`
	RetryAt               *time.Time              `json:"retry_at,omitempty"`
	PolicyRevision        string                  `json:"policy_revision,omitempty"`
	RequestBytes          uint64                  `json:"request_bytes,omitempty"`
	ProjectedInputTokens  uint64                  `json:"projected_input_tokens,omitempty"`
	Projection            *ProviderProjectionData `json:"projection,omitempty"`
	StopReason            string                  `json:"stop_reason,omitempty"`
	StartedAt             *time.Time              `json:"started_at,omitempty"`
	FinishedAt            *time.Time              `json:"finished_at,omitempty"`
	RateLimitRetries      uint32                  `json:"rate_limit_retries,omitempty"`
	RateLimitRetryLimit   uint32                  `json:"rate_limit_retry_limit,omitempty"`
	RateLimitWaitedMS     uint64                  `json:"rate_limit_waited_ms,omitempty"`
	RateLimitWaitBudgetMS uint64                  `json:"rate_limit_wait_budget_ms,omitempty"`
}

func (*ProviderAttemptData) eventKind() EventKind { return EventProviderAttempt }

func (d *ProviderAttemptData) validate() error {
	if d.SampleID == "" || d.Attempt == 0 {
		return errors.New("provider attempt sample_id and attempt are required")
	}
	if !slices.Contains([]string{
		ProviderAttemptStarted, ProviderAttemptRetryWait,
		ProviderAttemptIncomplete, ProviderAttemptCompleted,
		ProviderAttemptFailed,
	}, d.Status) {
		return errors.New("provider attempt status is invalid")
	}
	if d.Status == ProviderAttemptRetryWait && d.FailureCode == "" {
		return errors.New("provider retry wait failure_code is required")
	}
	if (d.Status == ProviderAttemptIncomplete || d.Status == ProviderAttemptCompleted) &&
		d.StopReason == "" {
		return errors.New("finished provider attempt stop_reason is required")
	}
	return nil
}

// SampleContextData is low-cardinality token attribution for one provider call.
// It contains counts and content-safe digests, never prompt or tool content.
type SampleContextData struct {
	Reason                    string                  `json:"reason"`
	ReasoningEffort           string                  `json:"reasoning_effort,omitempty"`
	ContextRevision           uint64                  `json:"context_revision,omitempty"`
	ContextDigest             string                  `json:"context_digest,omitempty"`
	WorldRevision             uint64                  `json:"world_revision,omitempty"`
	WorldDigest               string                  `json:"world_digest,omitempty"`
	WorldMode                 string                  `json:"world_mode,omitempty"`
	WorldChangedSections      int                     `json:"world_changed_sections,omitempty"`
	PrefixCompared            bool                    `json:"prefix_compared,omitempty"`
	PrefixMonotonic           bool                    `json:"prefix_monotonic,omitempty"`
	PrefixCommonItems         int                     `json:"prefix_common_items,omitempty"`
	PrefixCommonTokens        uint64                  `json:"prefix_common_tokens,omitempty"`
	PrefixFirstDivergence     int                     `json:"prefix_first_divergence,omitempty"`
	PrefixDivergenceKind      string                  `json:"prefix_divergence_kind,omitempty"`
	StablePrefixDigest        string                  `json:"stable_prefix_digest,omitempty"`
	PreviousContextDigest     string                  `json:"previous_context_digest,omitempty"`
	RouteDigest               string                  `json:"route_digest,omitempty"`
	RequestPropertyDigest     string                  `json:"request_property_digest,omitempty"`
	ToolDefinitionDigest      string                  `json:"tool_definition_digest,omitempty"`
	WindowID                  string                  `json:"window_id,omitempty"`
	WindowNumber              uint64                  `json:"window_number,omitempty"`
	WindowObserved            bool                    `json:"window_observed,omitempty"`
	WindowProjectedTokens     uint64                  `json:"window_projected_tokens,omitempty"`
	WindowFullActiveTokens    uint64                  `json:"window_full_active_tokens,omitempty"`
	WindowPrefillTokens       uint64                  `json:"window_prefill_tokens,omitempty"`
	WindowBodyTokens          uint64                  `json:"window_body_tokens,omitempty"`
	WindowPendingTokens       uint64                  `json:"window_pending_tokens,omitempty"`
	WindowOutputReserve       uint64                  `json:"window_output_reserve,omitempty"`
	WindowHardInputTokens     uint64                  `json:"window_hard_input_tokens,omitempty"`
	WindowOutputSource        string                  `json:"window_output_source,omitempty"`
	EconomicRequestedTokens   uint64                  `json:"economic_requested_tokens,omitempty"`
	EconomicGrantedTokens     uint64                  `json:"economic_granted_tokens,omitempty"`
	EconomicInputTokens       uint64                  `json:"economic_input_tokens,omitempty"`
	EconomicHardInputTokens   uint64                  `json:"economic_hard_input_tokens,omitempty"`
	EconomicOperatorTokens    uint64                  `json:"economic_operator_tokens,omitempty"`
	EconomicReason            string                  `json:"economic_reason,omitempty"`
	EconomicSource            string                  `json:"economic_source,omitempty"`
	EconomicProvenance        string                  `json:"economic_provenance,omitempty"`
	EconomicBudgeted          bool                    `json:"economic_budgeted,omitempty"`
	EconomicBudgetMode        string                  `json:"economic_budget_mode,omitempty"`
	EconomicBudgetScope       string                  `json:"economic_budget_scope,omitempty"`
	EconomicBudgetUsed        uint64                  `json:"economic_budget_used,omitempty"`
	EconomicBudgetLimit       uint64                  `json:"economic_budget_limit,omitempty"`
	EconomicBudgetRemaining   uint64                  `json:"economic_budget_remaining,omitempty"`
	FinalizationOutputReserve uint64                  `json:"finalization_output_reserve,omitempty"`
	PairingCalls              int                     `json:"pairing_calls,omitempty"`
	PairingResults            int                     `json:"pairing_results,omitempty"`
	PairingPairs              int                     `json:"pairing_pairs,omitempty"`
	PairingDroppedOrphans     int                     `json:"pairing_dropped_orphans,omitempty"`
	PairingVisibleOrphans     int                     `json:"pairing_visible_orphans,omitempty"`
	ProjectedImages           int                     `json:"projected_images,omitempty"`
	HistoricalImagesReplaced  int                     `json:"historical_images_replaced,omitempty"`
	DroppedReasoning          int                     `json:"dropped_reasoning,omitempty"`
	MaxItemTokens             uint64                  `json:"max_item_tokens,omitempty"`
	AdmissionItems            int                     `json:"admission_items,omitempty"`
	AdmissionSpilledItems     int                     `json:"admission_spilled_items,omitempty"`
	AdmissionOriginalTokens   uint64                  `json:"admission_original_tokens,omitempty"`
	AdmissionRetainedTokens   uint64                  `json:"admission_retained_tokens,omitempty"`
	StableTokens              uint64                  `json:"stable_tokens,omitempty"`
	HistoryUserTokens         uint64                  `json:"history_user_tokens,omitempty"`
	HistoryAssistantTokens    uint64                  `json:"history_assistant_tokens,omitempty"`
	HistoryToolTokens         uint64                  `json:"history_tool_tokens,omitempty"`
	HistoryOtherTokens        uint64                  `json:"history_other_tokens,omitempty"`
	DynamicTokens             uint64                  `json:"dynamic_tokens,omitempty"`
	ContinuationTokens        uint64                  `json:"continuation_tokens,omitempty"`
	TextTokens                uint64                  `json:"text_tokens,omitempty"`
	ImageTokens               uint64                  `json:"image_tokens,omitempty"`
	ToolDefinitionTokens      uint64                  `json:"tool_definition_tokens,omitempty"`
	ProviderFramingTokens     uint64                  `json:"provider_framing_tokens,omitempty"`
	UncachedInputTokens       uint64                  `json:"uncached_input_tokens,omitempty"`
	EstimatedTokens           uint64                  `json:"estimated_tokens,omitempty"`
	MessageCount              int                     `json:"message_count,omitempty"`
	ToolDefinitionCount       int                     `json:"tool_definition_count,omitempty"`
	RequestBytes              uint64                  `json:"request_bytes,omitempty"`
	LogicalRequestDigest      string                  `json:"logical_request_digest,omitempty"`
	TransportPayloadDigest    string                  `json:"transport_payload_digest,omitempty"`
	IncrementalTransport      bool                    `json:"incremental_transport,omitempty"`
	ProviderProjection        *ProviderProjectionData `json:"provider_projection,omitempty"`
}

func (*UsageData) eventKind() EventKind { return EventUsage }

func (d *UsageData) validate() error {
	if d.CachedTokens > d.InputTokens {
		return errors.New("cached tokens cannot exceed input tokens")
	}
	if d.ReasoningTokens > d.OutputTokens {
		return errors.New("reasoning tokens cannot exceed output tokens")
	}
	if d.ModelMetadata != nil {
		if err := d.ModelMetadata.Validate(); err != nil {
			return err
		}
	}
	if d.Context == nil || d.Context.ProviderProjection == nil {
		return nil
	}
	projection := d.Context.ProviderProjection
	incremental := projection.Mode == "incremental_session"
	if d.Context.IncrementalTransport != incremental {
		return errors.New("provider projection mode disagrees with transport attribution")
	}
	switch projection.Mode {
	case "complete_http_sse", "complete_session":
		if projection.IncrementalEligible ||
			projection.FallbackReason == "" {
			return errors.New("complete provider projection requires a fallback reason")
		}
	case "incremental_session":
		if !projection.IncrementalEligible ||
			projection.FallbackReason != "" ||
			projection.RouteDigest == "" ||
			projection.PropertyDigest == "" ||
			projection.StablePrefixDigest == "" ||
			projection.InputDigest == "" ||
			projection.DeltaDigest == "" ||
			!projection.LogicalTransportEquivalent {
			return errors.New("incremental provider projection evidence is incomplete")
		}
	default:
		return errors.New("provider projection mode is invalid")
	}
	return nil
}

type ToolStateData struct {
	State string `json:"state"`
	Text  string `json:"text,omitempty"`
}

func (*ToolStateData) eventKind() EventKind { return EventToolState }

func (d *ToolStateData) validate() error { return require(d.State != "", "tool state is required") }

func require(valid bool, message string) error {
	if !valid {
		return errors.New(message)
	}
	return nil
}

type ToolStartData struct {
	Tool      string          `json:"tool"`
	CallID    string          `json:"call_id"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

func (*ToolStartData) eventKind() EventKind { return EventToolStart }

func (d *ToolStartData) validate() error {
	if d.Tool == "" || d.CallID == "" {
		return errors.New("tool start tool and call_id are required")
	}
	if len(d.Arguments) != 0 && !json.Valid(d.Arguments) {
		return errors.New("tool start arguments must be valid JSON")
	}
	return nil
}

type ToolResultData struct {
	Tool                string                 `json:"tool"`
	CallID              string                 `json:"call_id"`
	Output              string                 `json:"output"`
	IsError             bool                   `json:"is_error"`
	Execution           *ToolExecutionReceipt  `json:"execution,omitempty"`
	Changes             []FileChange           `json:"changes,omitempty"`
	Recovery            *ToolRecovery          `json:"recovery,omitempty"`
	Completion          *CompletionDeclaration `json:"completion,omitempty"`
	WorkspaceWriteScope string                 `json:"workspace_write_scope,omitempty"`
	ObservedChanges     *int                   `json:"observed_changes,omitempty"`
	Truncated           bool                   `json:"truncated,omitempty"`
}

type CompletionDeclaration struct {
	Status              string   `json:"status"`
	Summary             string   `json:"summary"`
	OutputMode          string   `json:"output_mode,omitempty"`
	ChangedPaths        []string `json:"changed_paths"`
	VerificationCallIDs []string `json:"verification_call_ids"`
	PendingActions      []string `json:"pending_actions"`
	MutationRevision    uint64   `json:"mutation_revision"`
	CallID              string   `json:"call_id"`
	Accepted            bool     `json:"accepted"`
	Rejection           string   `json:"rejection,omitempty"`
}

type ToolRecovery struct {
	ErrorCategory  string `json:"error_category"`
	RequiredAction string `json:"required_action"`
	Path           string `json:"path,omitempty"`
	RetryOriginal  bool   `json:"retry_original"`
}

type FileChange struct {
	Path    string `json:"path"`
	Kind    string `json:"kind"`
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
}

type ToolCatalogChange struct {
	Name     string `json:"name"`
	Source   string `json:"source,omitempty"`
	Revision uint64 `json:"revision"`
}

type ToolCatalogChangedData struct {
	CatalogID  string              `json:"catalog_id"`
	Generation uint64              `json:"generation"`
	Digest     string              `json:"digest"`
	Added      []ToolCatalogChange `json:"added,omitempty"`
	Replaced   []ToolCatalogChange `json:"replaced,omitempty"`
	Revoked    []ToolCatalogChange `json:"revoked,omitempty"`
}

type MCPHealthChangedData struct {
	Server              string     `json:"server"`
	PreviousState       string     `json:"previous_state,omitempty"`
	State               string     `json:"state"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	LastError           string     `json:"last_error,omitempty"`
	ChangedAt           time.Time  `json:"changed_at"`
	RetryAt             *time.Time `json:"retry_at,omitempty"`
}

func (*MCPHealthChangedData) eventKind() EventKind { return EventMCPHealthChanged }

func (d *MCPHealthChangedData) validate() error {
	if d.Server == "" || d.State == "" || d.ChangedAt.IsZero() {
		return errors.New("MCP health change requires server, state, and changed_at")
	}
	switch d.State {
	case "starting", "healthy", "degraded", "open", "half_open", "removed":
	default:
		return fmt.Errorf("invalid MCP health state %q", d.State)
	}
	if d.PreviousState != "" {
		switch d.PreviousState {
		case "starting", "healthy", "degraded", "open", "half_open":
		default:
			return fmt.Errorf("invalid previous MCP health state %q", d.PreviousState)
		}
	}
	if d.ConsecutiveFailures < 0 {
		return errors.New("MCP health consecutive_failures cannot be negative")
	}
	return nil
}

type ExtensionControlData struct {
	OperationID string                 `json:"operation_id"`
	Action      ExtensionControlAction `json:"action"`
	Kind        ExtensionControlKind   `json:"kind"`
	Name        string                 `json:"name,omitempty"`
	Status      string                 `json:"status"`
	Revision    uint64                 `json:"revision"`
	Digest      string                 `json:"digest"`
	OccurredAt  time.Time              `json:"occurred_at"`
}

func (*ExtensionControlData) eventKind() EventKind { return EventExtensionControl }

func (d *ExtensionControlData) validate() error {
	if !extensionControlIDPattern.MatchString(d.OperationID) ||
		d.Revision == 0 || d.OccurredAt.IsZero() ||
		d.Status == "" || !validSHA256(d.Digest) {
		return errors.New("extension control event is invalid")
	}
	operation := ExtensionControlOperation{
		Version: Version, ID: d.OperationID, Kind: d.Kind, Action: d.Action,
		Name: d.Name, CreatedAt: d.OccurredAt,
	}
	if operation.Query() {
		return errors.New("extension control event cannot represent query")
	}
	return operation.Validate()
}

func (*ToolCatalogChangedData) eventKind() EventKind { return EventToolCatalogChanged }

func (d *ToolCatalogChangedData) validate() error {
	if d.CatalogID == "" || d.Generation == 0 || d.Digest == "" {
		return errors.New("tool catalog changed requires catalog_id, generation, and digest")
	}
	for _, changes := range [][]ToolCatalogChange{d.Added, d.Replaced, d.Revoked} {
		for _, change := range changes {
			if change.Name == "" || change.Revision == 0 {
				return errors.New("tool catalog change requires name and revision")
			}
		}
	}
	return nil
}

func (*ToolResultData) eventKind() EventKind { return EventToolResult }

func (d *ToolResultData) validate() error {
	if d.Tool == "" || d.CallID == "" {
		return errors.New("tool result tool and call_id are required")
	}
	for _, change := range d.Changes {
		if strings.TrimSpace(change.Path) == "" ||
			(change.Kind != "created" &&
				change.Kind != "modified" &&
				change.Kind != "deleted") ||
			change.Added < 0 ||
			change.Removed < 0 {
			return errors.New("tool result contains an invalid file change")
		}
	}
	if d.Recovery != nil &&
		(d.Recovery.ErrorCategory == "" || d.Recovery.RequiredAction == "") {
		return errors.New("tool result recovery requires category and action")
	}
	if d.Completion != nil {
		if err := d.Completion.validate(); err != nil {
			return err
		}
	}
	if err := d.Execution.validate(); err != nil {
		return err
	}
	return nil
}

func (d *CompletionDeclaration) validate() error {
	if strings.TrimSpace(d.Summary) == "" {
		return errors.New("completion declaration is incomplete")
	}
	switch d.OutputMode {
	case "", "exact", "preserve_provisional":
	default:
		return errors.New("completion declaration output mode is invalid")
	}
	switch d.Status {
	case "complete":
		if len(d.PendingActions) != 0 {
			return errors.New("complete declaration has pending actions")
		}
	case "incomplete":
		if len(d.PendingActions) == 0 || d.Accepted {
			return errors.New("incomplete declaration is inconsistent")
		}
		if d.OutputMode == "preserve_provisional" {
			return errors.New("incomplete declaration cannot preserve provisional output")
		}
		for _, action := range d.PendingActions {
			if strings.TrimSpace(action) == "" {
				return errors.New("completion pending action is required")
			}
		}
	default:
		return errors.New("completion declaration has invalid status")
	}
	for _, path := range d.ChangedPaths {
		if strings.TrimSpace(path) == "" {
			return errors.New("completion declaration changed path is required")
		}
	}
	if d.Accepted {
		if d.Status != "complete" {
			return errors.New("accepted completion declaration is not complete")
		}
		readOnly := d.MutationRevision == 0 && len(d.ChangedPaths) == 0
		mutated := d.MutationRevision != 0 && len(d.ChangedPaths) != 0
		if (!readOnly && !mutated) || d.CallID == "" || d.Rejection != "" {
			return errors.New("accepted completion declaration is inconsistent")
		}
	} else if d.Rejection == "" {
		return errors.New("rejected completion declaration requires a reason")
	}
	return nil
}

// ToolOutputData is a live output chunk. Replay reconstructs the call from
// tool.start and tool.result rather than persisting these chunks.
type ToolOutputData struct {
	Tool   string `json:"tool"`
	CallID string `json:"call_id"`
	// Stream is "stdout" or "stderr". A pty merges them and reports stdout.
	Stream string `json:"stream"`
	Chunk  string `json:"chunk"`
	// Cursor is how many bytes of this stream have been produced through the end
	// of this chunk, so a client can detect a gap instead of rendering one.
	Cursor uint64 `json:"cursor"`
	// Truncated marks the last chunk of a call that spent its streaming budget.
	Truncated bool `json:"truncated,omitempty"`
}

func (*ToolOutputData) eventKind() EventKind { return EventToolOutput }

func (d *ToolOutputData) validate() error {
	if d.Tool == "" || d.CallID == "" {
		return errors.New("tool output tool and call_id are required")
	}
	if d.Stream != "stdout" && d.Stream != "stderr" {
		return errors.New("tool output stream must be stdout or stderr")
	}
	return nil
}

type DiagnosticPosition struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type DiagnosticRange struct {
	Start DiagnosticPosition `json:"start"`
	End   DiagnosticPosition `json:"end"`
}

type Diagnostic struct {
	Path     string          `json:"path"`
	Range    DiagnosticRange `json:"range"`
	Severity string          `json:"severity"`
	Code     string          `json:"code,omitempty"`
	Message  string          `json:"message"`
	Source   string          `json:"source"`
}

type DiagnosticReceipt struct {
	Path          string       `json:"path"`
	Status        string       `json:"status"`
	Runner        string       `json:"runner,omitempty"`
	Diagnostics   []Diagnostic `json:"diagnostics"`
	Message       string       `json:"message,omitempty"`
	ErrorCategory string       `json:"error_category,omitempty"`
	ExitCode      int          `json:"exit_code,omitempty"`
}

type DiagnosticsData struct {
	Tool     string              `json:"tool"`
	CallID   string              `json:"call_id"`
	Receipts []DiagnosticReceipt `json:"receipts"`
}

func (*DiagnosticsData) eventKind() EventKind { return EventDiagnostics }

func (d *DiagnosticsData) validate() error {
	if d.Tool == "" || d.CallID == "" || len(d.Receipts) == 0 {
		return errors.New("diagnostics tool, call_id, and receipts are required")
	}
	for _, receipt := range d.Receipts {
		if receipt.Path == "" || receipt.Status == "" {
			return errors.New("diagnostics receipt path and status are required")
		}
	}
	return nil
}

type TurnCompletedData struct {
	Text            string          `json:"text"`
	Outcome         TurnOutcome     `json:"outcome,omitempty"`
	SecondaryIssues []TerminalIssue `json:"secondary_issues,omitempty"`
}

func (*TurnCompletedData) eventKind() EventKind { return EventTurnCompleted }

func (d *TurnCompletedData) validate() error {
	switch d.Outcome {
	case "", TurnOutcomeAnswered, TurnOutcomePlanned, TurnOutcomeChanged, TurnOutcomeOperated:
		for _, issue := range d.SecondaryIssues {
			if issue.Phase == "" || issue.Code == "" ||
				issue.Message == "" {
				return errors.New(
					"turn completion secondary issue requires phase, code, and message",
				)
			}
		}
		return nil
	default:
		return errors.New("turn completed outcome is invalid")
	}
}

type TurnFailedData struct {
	Code            ErrorCode        `json:"code"`
	Message         string           `json:"message"`
	Fault           *FaultMetadata   `json:"fault,omitempty"`
	Convergence     *TurnConvergence `json:"convergence,omitempty"`
	SecondaryIssues []TerminalIssue  `json:"secondary_issues,omitempty"`
}

func (*TurnFailedData) eventKind() EventKind { return EventTurnFailed }

func (d *TurnFailedData) validate() error {
	if d.Code == "" || d.Message == "" {
		return errors.New("turn failure code and message are required")
	}
	for _, issue := range d.SecondaryIssues {
		if issue.Phase == "" || issue.Code == "" || issue.Message == "" {
			return errors.New("turn failure secondary issue requires phase, code, and message")
		}
	}
	if d.Convergence != nil {
		if err := d.Convergence.validate(); err != nil {
			return err
		}
	}
	return nil
}

type TurnConvergence struct {
	Cause          string   `json:"cause"`
	Used           uint32   `json:"used"`
	Limit          uint32   `json:"limit"`
	RepairKind     string   `json:"repair_kind,omitempty"`
	Summary        string   `json:"summary"`
	PendingActions []string `json:"pending_actions"`
}

func (c TurnConvergence) validate() error {
	switch c.Cause {
	case "declared_incomplete":
		if c.Used != 0 || c.Limit != 0 || c.RepairKind != "" {
			return errors.New("declared incomplete convergence is invalid")
		}
	case "output_limit", "no_progress", "repair_budget", "step_limit":
		if c.Used == 0 || c.Limit == 0 || c.Used < c.Limit {
			return errors.New("turn convergence budget is invalid")
		}
	default:
		return errors.New("turn convergence cause is invalid")
	}
	if strings.TrimSpace(c.Summary) == "" ||
		len(c.PendingActions) == 0 {
		return errors.New("turn convergence outcome is incomplete")
	}
	if (c.Cause == "repair_budget") != (c.RepairKind != "") {
		return errors.New("turn convergence repair kind is inconsistent")
	}
	for _, action := range c.PendingActions {
		if strings.TrimSpace(action) == "" {
			return errors.New("turn convergence pending action is empty")
		}
	}
	return nil
}

// TerminalIssue records a failure in cleanup or finalization after the primary
// turn result was already known.
type TerminalIssue struct {
	Phase   string    `json:"phase"`
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
}

type TurnCanceledData struct {
	Reason string `json:"reason"`
}

func (*TurnCanceledData) eventKind() EventKind { return EventTurnCanceled }

func (d *TurnCanceledData) validate() error {
	d.Reason = NormalizeCancelReason(d.Reason)
	return nil
}

type OperationRejectedData struct {
	Code    ErrorCode      `json:"code"`
	Message string         `json:"message"`
	Fault   *FaultMetadata `json:"fault,omitempty"`
}

func (*OperationRejectedData) eventKind() EventKind { return EventOperationRejected }

func (d *OperationRejectedData) validate() error {
	if d.Code == "" || d.Message == "" {
		return errors.New("operation rejection code and message are required")
	}
	return nil
}

type TurnSteeredData struct {
	Prompt  string `json:"prompt"`
	QueueID string `json:"queue_id,omitempty"`
}

func (*TurnSteeredData) eventKind() EventKind { return EventTurnSteered }

func (d *TurnSteeredData) validate() error {
	if d.Prompt == "" {
		return errors.New("steered prompt is required")
	}
	return nil
}

type ApprovalResolvedData struct {
	RequestID string           `json:"request_id"`
	Decision  ApprovalDecision `json:"decision"`
	Problem   *Problem         `json:"problem,omitempty"`
	Source    *ApprovalSource  `json:"source,omitempty"`
}

func (*ApprovalResolvedData) eventKind() EventKind { return EventApprovalResolved }

func (d *ApprovalResolvedData) validate() error {
	if d.Decision != ApprovalApprove && d.Decision != ApprovalDeny && d.Decision != ApprovalCancel {
		return errors.New("resolved approval decision is invalid")
	}
	if d.RequestID == "" {
		return errors.New("resolved approval request_id is required")
	}
	if d.Problem != nil &&
		(d.Problem.Version != ProblemVersion || !ValidErrorCode(d.Problem.Code) ||
			d.Problem.Message == "") {
		return errors.New("resolved approval problem is invalid")
	}
	if err := validateApprovalSource(d.Source); err != nil {
		return err
	}
	return nil
}

type InputRequiredData struct {
	RequestID string    `json:"request_id"`
	CallID    string    `json:"call_id"`
	Tool      string    `json:"tool"`
	Prompt    string    `json:"prompt"`
	Options   []string  `json:"options,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (*InputRequiredData) eventKind() EventKind { return EventInputRequired }

func (d *InputRequiredData) validate() error {
	if d.RequestID == "" || d.CallID == "" || d.Tool == "" {
		return errors.New("input request identity is required")
	}
	if strings.TrimSpace(d.Prompt) == "" || d.ExpiresAt.IsZero() {
		return errors.New("input prompt and expiry are required")
	}
	return nil
}

type InputResolvedData struct {
	RequestID string `json:"request_id"`
	Answer    string `json:"answer,omitempty"`
}

func (*InputResolvedData) eventKind() EventKind { return EventInputResolved }

func (d *InputResolvedData) validate() error {
	if d.RequestID == "" {
		return errors.New("resolved input request_id is required")
	}
	return nil
}

type CanonicalResource struct {
	Kind   string `json:"kind"`
	Path   string `json:"path,omitempty"`
	ID     string `json:"id,omitempty"`
	Access string `json:"access"`
	Tree   bool   `json:"tree,omitempty"`
}

type EditPlan struct {
	ID    string         `json:"id"`
	Diff  string         `json:"diff"`
	Files []EditPlanFile `json:"files"`
}

type EditPlanFile struct {
	Path         string `json:"path"`
	Kind         string `json:"kind"`
	Before       string `json:"before,omitempty"`
	After        string `json:"after,omitempty"`
	BeforeExists bool   `json:"before_exists"`
	AfterExists  bool   `json:"after_exists"`
	BeforeDigest string `json:"before_digest"`
	AfterDigest  string `json:"after_digest"`
}
type ApprovalSource struct {
	Kind          string `json:"kind"`
	AgentID       string `json:"agent_id"`
	AgentPath     string `json:"agent_path"`
	ParentPath    string `json:"parent_path"`
	Role          string `json:"role"`
	SessionID     string `json:"session_id"`
	WorkspaceRoot string `json:"workspace_root"`
}
type ApprovalRequiredData struct {
	RequestID           string                  `json:"request_id"`
	CallID              string                  `json:"call_id"`
	Tool                string                  `json:"tool"`
	Arguments           json.RawMessage         `json:"arguments"`
	ArgumentsDigest     string                  `json:"arguments_digest"`
	Resources           []CanonicalResource     `json:"resources"`
	AllowedScopes       []ApprovalScope         `json:"allowed_scopes"`
	ExpiresAt           time.Time               `json:"expires_at"`
	ReplacementAllowed  bool                    `json:"replacement_allowed"`
	ModifiableArguments []string                `json:"modifiable_arguments"`
	Effect              string                  `json:"effect"`
	Risk                string                  `json:"risk"`
	ReasonCode          string                  `json:"reason_code"`
	Network             *NetworkApprovalPayload `json:"network,omitempty"`
	EditPlan            *EditPlan               `json:"edit_plan,omitempty"`
	GrantPreview        *ApprovalGrantPreview   `json:"grant_preview,omitempty"`
	Source              *ApprovalSource         `json:"source,omitempty"`
}
type ApprovalGrantPreview struct {
	Kind    string `json:"kind"`
	Key     string `json:"key"`
	Summary string `json:"summary"`
}

type NetworkApprovalPayload struct {
	Host     string `json:"host"`
	Protocol string `json:"protocol"`
	Mode     string `json:"mode"`
}

func (*ApprovalRequiredData) eventKind() EventKind { return EventApprovalRequired }

func (d *ApprovalRequiredData) validate() error {
	if d.RequestID == "" || d.CallID == "" || d.Tool == "" || d.ArgumentsDigest == "" {
		return errors.New("approval request identity and arguments digest are required")
	}
	if len(d.Arguments) == 0 {
		return errors.New("approval arguments are required")
	}
	if !validApprovalEffect(d.Effect) || !validApprovalRisk(d.Risk) ||
		d.ReasonCode == "" {
		return errors.New("approval effect, risk, and reason code are required")
	}
	for _, resource := range d.Resources {
		if resource.Kind == "" || (resource.Access != "read" && resource.Access != "write") {
			return errors.New("approval resource kind and access are required")
		}
	}
	for _, scope := range d.AllowedScopes {
		if scope != ApprovalScopeOnce && scope != ApprovalScopeSession &&
			scope != ApprovalScopeAlways {
			return errors.New("approval allowed scope is invalid")
		}
		if scope != ApprovalScopeOnce && d.GrantPreview == nil {
			return errors.New("reusable approval scope requires a grant preview")
		}
	}
	if d.GrantPreview != nil &&
		(d.GrantPreview.Kind == "" || !validSHA256(d.GrantPreview.Key) ||
			d.GrantPreview.Summary == "") {
		return errors.New("approval grant preview is invalid")
	}
	if err := validateApprovalSource(d.Source); err != nil {
		return err
	}
	if d.EditPlan != nil {
		if !validSHA256(d.EditPlan.ID) || len(d.EditPlan.Files) == 0 {
			return errors.New("approval edit plan identity and files are required")
		}
		for _, file := range d.EditPlan.Files {
			if file.Path == "" || file.Kind == "" ||
				(file.BeforeDigest != "missing" && !validSHA256(file.BeforeDigest)) ||
				(file.AfterDigest != "missing" && !validSHA256(file.AfterDigest)) {
				return errors.New("approval edit plan file is invalid")
			}
		}
	}
	return nil
}

func validApprovalEffect(value string) bool {
	switch value {
	case "workspace.read", "workspace.edit", "process.read_only",
		"process.mutating", "network.read", "network.mutating",
		"agent.message", "agent.lifecycle", "external.mutation":
		return true
	default:
		return false
	}
}

func validApprovalRisk(value string) bool {
	return value == "low" || value == "medium" || value == "high" ||
		value == "critical"
}

func validateApprovalSource(source *ApprovalSource) error {
	if source != nil &&
		(source.Kind != "agent" || source.AgentID == "" ||
			source.AgentPath == "" || source.ParentPath == "" ||
			source.Role == "" || source.SessionID == "" ||
			source.WorkspaceRoot == "") {
		return errors.New("approval source is invalid")
	}
	return nil
}

type ThreadCompactedData struct {
	Summary             string                `json:"summary"`
	ReplacementHistory  []CompactedMessage    `json:"replacement_history,omitempty"`
	WindowNumber        uint64                `json:"window_number,omitempty"`
	FirstWindowID       string                `json:"first_window_id,omitempty"`
	PreviousWindowID    string                `json:"previous_window_id,omitempty"`
	WindowID            string                `json:"window_id,omitempty"`
	TruthGeneration     uint64                `json:"truth_generation,omitempty"`
	TruthEntities       int                   `json:"truth_entities,omitempty"`
	CriticalFacts       int                   `json:"critical_facts,omitempty"`
	CompatibilityHash   string                `json:"compatibility_hash,omitempty"`
	AuthorityDigest     string                `json:"authority_digest,omitempty"`
	AuthorityEquivalent bool                  `json:"authority_equivalent,omitempty"`
	ModelDownshifted    bool                  `json:"model_downshifted,omitempty"`
	DownshiftPolicy     string                `json:"downshift_policy,omitempty"`
	NarrativeIncluded   bool                  `json:"narrative_included,omitempty"`
	MandatoryBytes      int                   `json:"mandatory_bytes,omitempty"`
	MandatoryEntities   int                   `json:"mandatory_entities,omitempty"`
	OmissionCount       int                   `json:"omission_count,omitempty"`
	Retention           []TruthRetentionCount `json:"retention,omitempty"`
}

type TruthRetentionCount struct {
	Class      string `json:"class"`
	Candidates int    `json:"candidates"`
	Retained   int    `json:"retained"`
	Omitted    int    `json:"omitted"`
}

// CompactedMessage is durable model-visible history; Turn is retained for resume.
type CompactedMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	Turn    uint64          `json:"turn,omitempty"`
}

func validateCompactedHistory(history []CompactedMessage) error {
	if len(history) == 0 || len(history) > 4096 {
		return errors.New("replacement history size is invalid")
	}
	for _, message := range history {
		switch message.Role {
		case "user", "assistant", "tool", "system":
		default:
			return errors.New("replacement history role is invalid")
		}
		if len(message.Content) == 0 || len(message.Content) > 4<<20 ||
			!json.Valid(message.Content) {
			return errors.New("replacement history content is invalid")
		}
	}
	return nil
}

func (*ThreadCompactedData) eventKind() EventKind { return EventThreadCompacted }

func (d *ThreadCompactedData) validate() error {
	if d.Summary == "" {
		return errors.New("compaction summary is required")
	}
	if len(d.ReplacementHistory) > 0 && d.WindowID == "" {
		return errors.New("compaction window_id is required when replacement_history is set")
	}
	if d.TruthGeneration != 0 &&
		(d.CompatibilityHash == "" || d.DownshiftPolicy == "" ||
			d.AuthorityDigest == "" || !d.AuthorityEquivalent) {
		return errors.New("compaction truth metadata is incomplete")
	}
	return nil
}

// TurnCompactionData is emitted when an in-turn compact gate runs (pre-sampling
// or mid-turn). Distinct from thread.compacted, which installs a durable window.
type TurnCompactionData struct {
	CompactionID    string `json:"compaction_id,omitempty"`
	Status          string `json:"status,omitempty"`
	Mode            string `json:"mode,omitempty"`
	SourceWindowID  string `json:"source_window_id,omitempty"`
	TargetWindowID  string `json:"target_window_id,omitempty"`
	Phase           string `json:"phase"`
	Summary         string `json:"summary"`
	RemovedMessages int    `json:"removed_messages,omitempty"`
	OriginalBytes   int    `json:"original_bytes,omitempty"`
	RetainedBytes   int    `json:"retained_bytes,omitempty"`
	// Sections names the parts of the structured summary that fit in the budget,
	// in the order they were written. It is what tells a compaction that carried
	// the goal and the outstanding work from one that only had room for a
	// transcript of what was dropped.
	Sections []string `json:"sections,omitempty"`
	// SummaryTruncated reports that the summary budget cut sections, so a host can
	// distinguish a complete account of the removed history from a partial one.
	SummaryTruncated      bool                     `json:"summary_truncated,omitempty"`
	RemovedTurns          []uint64                 `json:"removed_turns,omitempty"`
	PrunedToolResults     int                      `json:"pruned_tool_results,omitempty"`
	PrunedBytes           int                      `json:"pruned_bytes,omitempty"`
	TruthGeneration       uint64                   `json:"truth_generation,omitempty"`
	TruthEntities         int                      `json:"truth_entities,omitempty"`
	CriticalFacts         int                      `json:"critical_facts,omitempty"`
	CompatibilityHash     string                   `json:"compatibility_hash,omitempty"`
	CompatibilityMatched  bool                     `json:"compatibility_matched,omitempty"`
	AuthorityDigest       string                   `json:"authority_digest,omitempty"`
	AuthorityEquivalent   bool                     `json:"authority_equivalent,omitempty"`
	ModelDownshifted      bool                     `json:"model_downshifted,omitempty"`
	DownshiftPolicy       string                   `json:"downshift_policy,omitempty"`
	NarrativeIncluded     bool                     `json:"narrative_included,omitempty"`
	NarrativeBytes        int                      `json:"narrative_bytes,omitempty"`
	NarrativeInputTokens  uint64                   `json:"narrative_input_tokens,omitempty"`
	NarrativeOutputTokens uint64                   `json:"narrative_output_tokens,omitempty"`
	NarrativeProvider     string                   `json:"narrative_provider,omitempty"`
	NarrativeModel        string                   `json:"narrative_model,omitempty"`
	NarrativeMetadata     *ModelMetadataProvenance `json:"narrative_metadata_provenance,omitempty"`
	FallbackReason        string                   `json:"fallback_reason,omitempty"`
	CapsuleBytes          int                      `json:"capsule_bytes,omitempty"`
	MandatoryBytes        int                      `json:"mandatory_bytes,omitempty"`
	MandatoryEntities     int                      `json:"mandatory_entities,omitempty"`
	OmissionCount         int                      `json:"omission_count,omitempty"`
	Retention             []TruthRetentionCount    `json:"retention,omitempty"`
}

func (*TurnCompactionData) eventKind() EventKind { return EventTurnCompaction }

func (d *TurnCompactionData) validate() error {
	if d.Phase == "" {
		return errors.New("compaction phase is required")
	}
	if d.Summary == "" {
		return errors.New("compaction summary is required")
	}
	if d.Status != "" {
		switch d.Status {
		case "started", "prepared", "summarizing", "rebasing",
			"completed", "fallback", "folded":
		default:
			return errors.New("compaction status is invalid")
		}
	}
	if d.TruthGeneration != 0 &&
		(d.CompatibilityHash == "" || d.DownshiftPolicy == "" ||
			d.AuthorityDigest == "" || !d.AuthorityEquivalent) {
		return errors.New("turn compaction truth metadata is incomplete")
	}
	if d.NarrativeProvider != "" || d.NarrativeModel != "" ||
		d.NarrativeMetadata != nil {
		if d.NarrativeProvider == "" || d.NarrativeModel == "" ||
			d.NarrativeMetadata == nil {
			return errors.New("turn compaction narrative route is incomplete")
		}
		if err := d.NarrativeMetadata.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// VerificationCheck is one command or analyzer the gate ran.
type VerificationCheck struct {
	Name     string `json:"name"`
	Command  string `json:"command,omitempty"`
	Reason   string `json:"reason,omitempty"`
	Category string `json:"category,omitempty"`
	Status   string `json:"status"`
	ExitCode int    `json:"exit_code,omitempty"`
	Output   string `json:"output,omitempty"`
}

// TurnVerificationData reports one evaluation of the verification gate that runs
// before a turn commits its edits. Action records what the gate did
// with the verdict, so a failed status followed by action=repair reads as "the
// model was asked to fix it" rather than as a failed turn.
type TurnVerificationData struct {
	Scope          string              `json:"scope"`
	Mode           string              `json:"mode"`
	Status         string              `json:"status"`
	Action         string              `json:"action"`
	RepairSteps    int                 `json:"repair_steps"`
	Errors         int                 `json:"errors,omitempty"`
	Warnings       int                 `json:"warnings,omitempty"`
	Paths          []string            `json:"paths,omitempty"`
	UncoveredPaths []string            `json:"uncovered_paths,omitempty"`
	Checks         []VerificationCheck `json:"checks,omitempty"`
	Message        string              `json:"message,omitempty"`
}

func (*TurnVerificationData) eventKind() EventKind { return EventTurnVerification }

func (d *TurnVerificationData) validate() error {
	if d.Scope == "" {
		return errors.New("verification scope is required")
	}
	if d.Action == "" {
		return errors.New("verification action is required")
	}
	switch d.Status {
	case ReceiptPassed, ReceiptFailed, ReceiptUnavailable, ReceiptNotEvaluated:
	default:
		d.Status = ReceiptNotEvaluated
	}
	for _, check := range d.Checks {
		if check.Name == "" {
			return errors.New("verification check name is required")
		}
	}
	return nil
}

type AgentSpawnedData struct {
	AgentID       string          `json:"agent_id"`
	ParentID      string          `json:"parent_id,omitempty"`
	WorkspaceRoot string          `json:"workspace_root"`
	SessionID     string          `json:"session_id"`
	Role          string          `json:"role"`
	Profile       string          `json:"profile,omitempty"`
	Stance        string          `json:"stance,omitempty"`
	Depth         int             `json:"depth"`
	Worktree      string          `json:"worktree,omitempty"`
	Detail        json.RawMessage `json:"detail,omitempty"`
}

func (*AgentSpawnedData) eventKind() EventKind { return EventAgentSpawned }

func (d *AgentSpawnedData) validate() error {
	if d.AgentID == "" || d.WorkspaceRoot == "" || d.SessionID == "" {
		return errors.New("agent spawn identity is required")
	}
	if d.Role == "" {
		return errors.New("agent role is required")
	}
	if len(d.Detail) > 0 && !json.Valid(d.Detail) {
		return errors.New("agent spawn detail is invalid")
	}
	return nil
}

// AgentStatusData records a durable subagent status transition.
type AgentStatusData struct {
	AgentID       string          `json:"agent_id"`
	WorkspaceRoot string          `json:"workspace_root"`
	SessionID     string          `json:"session_id"`
	Status        string          `json:"status"`
	Message       string          `json:"message,omitempty"`
	ReasonCode    string          `json:"reason_code,omitempty"`
	Detail        json.RawMessage `json:"detail,omitempty"`
}

func (*AgentStatusData) eventKind() EventKind { return EventAgentStatus }

func (d *AgentStatusData) validate() error {
	if d.AgentID == "" || d.WorkspaceRoot == "" ||
		d.SessionID == "" || d.Status == "" {
		return errors.New("agent status identity is required")
	}
	if len(d.Detail) > 0 && !json.Valid(d.Detail) {
		return errors.New("agent status detail is invalid")
	}
	return nil
}

// AgentMessageData records an inter-agent mailbox message in the eventlog so
// the in-memory mailbox is not the sole truth source.
type AgentMessageData struct {
	From          string          `json:"from"`
	To            string          `json:"to"`
	WorkspaceRoot string          `json:"workspace_root"`
	SessionID     string          `json:"session_id"`
	Sequence      uint64          `json:"sequence"`
	Body          json.RawMessage `json:"body"`
}

type AgentIntegrationData struct {
	AgentID       string          `json:"agent_id"`
	AgentPath     string          `json:"agent_path"`
	ParentPath    string          `json:"parent_path"`
	WorkspaceRoot string          `json:"workspace_root"`
	SessionID     string          `json:"session_id"`
	Status        string          `json:"status"`
	PreviewDigest string          `json:"preview_digest"`
	Paths         []string        `json:"paths,omitempty"`
	Conflicts     []string        `json:"conflicts,omitempty"`
	Message       string          `json:"message,omitempty"`
	Detail        json.RawMessage `json:"detail,omitempty"`
}

func (*AgentIntegrationData) eventKind() EventKind { return EventAgentIntegration }

func (d *AgentIntegrationData) validate() error {
	if d.AgentID == "" || d.AgentPath == "" || d.ParentPath == "" ||
		d.WorkspaceRoot == "" || d.SessionID == "" || d.Status == "" ||
		!validSHA256(d.PreviewDigest) {
		return errors.New("agent integration identity is invalid")
	}
	if len(d.Detail) > 0 && !json.Valid(d.Detail) {
		return errors.New("agent integration detail is invalid")
	}
	return nil
}

func (*AgentMessageData) eventKind() EventKind { return EventAgentMessage }

func (d *AgentMessageData) validate() error {
	if d.From == "" || d.To == "" ||
		d.WorkspaceRoot == "" || d.SessionID == "" {
		return errors.New("agent message identity is required")
	}
	if d.Sequence == 0 {
		return errors.New("agent message sequence must be positive")
	}
	return nil
}

type PlanDeltaData struct {
	Body            string      `json:"body,omitempty"`
	Purpose         PlanPurpose `json:"purpose,omitempty"`
	Done            bool        `json:"done,omitempty"`
	ArtifactID      string      `json:"artifact_id,omitempty"`
	ProfileRevision uint64      `json:"profile_revision,omitempty"`
	Status          string      `json:"status,omitempty"`
	CanImplement    bool        `json:"can_implement,omitempty"`
	CanAutopilot    bool        `json:"can_autopilot,omitempty"`
}

func (*PlanDeltaData) eventKind() EventKind { return EventPlanDelta }

func (d *PlanDeltaData) validate() error {
	if !d.Done || len(d.Body) > 64<<10 ||
		strings.ContainsRune(d.Body, '\x00') {
		return errors.New("plan delta body is invalid")
	}
	if err := validatePlanPurpose(d.Purpose, d.Body, d.CanImplement, d.CanAutopilot); err != nil {
		return err
	}
	if d.ArtifactID != "" {
		if !d.Done || !validProfileIdentifier(d.ArtifactID) ||
			d.ProfileRevision == 0 ||
			d.Status != string(PlanArtifactReady) ||
			(d.Purpose.Normalize() == PlanPurposeExecution && !d.CanImplement && !d.CanAutopilot) {
			return errors.New("plan delta Artifact projection is invalid")
		}
	}
	return nil
}

type CommandExecutionData struct {
	CallID     string `json:"call_id"`
	SessionID  string `json:"session_id,omitempty"`
	Command    string `json:"command"`
	Status     string `json:"status"` // started|completed|failed|canceled|timed_out
	ExitCode   *int   `json:"exit_code,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	Handle     string `json:"handle,omitempty"`
	OutputTail string `json:"output_tail,omitempty"`
}

func (*CommandExecutionData) eventKind() EventKind { return EventCommandExecution }

func (d *CommandExecutionData) validate() error {
	if d.CallID == "" || d.Command == "" {
		return errors.New("command execution call_id and command are required")
	}
	switch d.Status {
	case "started", "completed", "failed", "canceled", "timed_out":
	default:
		return errors.New("command execution status is invalid")
	}
	return nil
}

type HostCommandData struct {
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
	Status  string   `json:"status"`
	Output  string   `json:"output,omitempty"`
}

func (*HostCommandData) eventKind() EventKind { return EventHostCommand }

func (d *HostCommandData) validate() error {
	if d.Command == "" {
		return errors.New("host command is required")
	}
	switch d.Status {
	case "started", "completed", "failed":
	default:
		return errors.New("host command status is invalid")
	}
	return nil
}

type ThreadForkedData struct {
	NewThreadID        ThreadID           `json:"new_thread_id"`
	SourceCursor       Cursor             `json:"source_cursor"`
	ReplacementHistory []CompactedMessage `json:"replacement_history,omitempty"`
	WindowNumber       uint64             `json:"window_number,omitempty"`
	FirstWindowID      string             `json:"first_window_id,omitempty"`
	WindowID           string             `json:"window_id,omitempty"`
}

type CheckpointRestoredData struct {
	CheckpointID         string             `json:"checkpoint_id"`
	SourceThreadID       ThreadID           `json:"source_thread_id"`
	SourceTurnID         TurnID             `json:"source_turn_id"`
	SourceCursor         Cursor             `json:"source_cursor"`
	ReplacementHistory   []CompactedMessage `json:"replacement_history"`
	SideEffectsReplayed  bool               `json:"side_effects_replayed"`
	ExactContext         bool               `json:"exact_context"`
	WorkspaceClaimsValid bool               `json:"workspace_claims_valid"`
	InvalidatedClaims    int                `json:"invalidated_claims,omitempty"`
	StaleClaims          int                `json:"stale_claims,omitempty"`
	ContextCommitID      string             `json:"context_commit_id,omitempty"`
	ContextDigest        string             `json:"context_digest,omitempty"`
	ContextRevision      uint64             `json:"context_revision,omitempty"`
	StateEpoch           uint64             `json:"state_epoch,omitempty"`
}

type CheckpointCreatedData struct {
	Checkpoint SessionCheckpoint `json:"checkpoint"`
}

func (*CheckpointCreatedData) eventKind() EventKind { return EventCheckpointCreated }

func (d *CheckpointCreatedData) validate() error { return d.Checkpoint.Validate() }

func (*CheckpointRestoredData) eventKind() EventKind { return EventCheckpointRestored }

func (d *CheckpointRestoredData) validate() error {
	if !validProfileIdentifier(d.CheckpointID) ||
		!validProfileIdentifier(string(d.SourceThreadID)) ||
		!validProfileIdentifier(string(d.SourceTurnID)) ||
		len(d.ReplacementHistory) == 0 ||
		d.SideEffectsReplayed ||
		d.WorkspaceClaimsValid && !d.ExactContext ||
		!validContextCommitReference(
			d.ExactContext,
			d.ContextCommitID,
			d.ContextDigest,
			d.ContextRevision,
			d.StateEpoch,
		) ||
		d.InvalidatedClaims < 0 || d.StaleClaims < 0 {
		return errors.New("checkpoint restore data is invalid")
	}
	return validateCompactedHistory(d.ReplacementHistory)
}

type CheckpointForkedData struct {
	CheckpointID         string             `json:"checkpoint_id"`
	NewThreadID          ThreadID           `json:"new_thread_id"`
	Title                string             `json:"title"`
	SourceCursor         Cursor             `json:"source_cursor"`
	ReplacementHistory   []CompactedMessage `json:"replacement_history"`
	ExactContext         bool               `json:"exact_context"`
	WorkspaceClaimsValid bool               `json:"workspace_claims_valid"`
	InvalidatedClaims    int                `json:"invalidated_claims,omitempty"`
	StaleClaims          int                `json:"stale_claims,omitempty"`
	ContextCommitID      string             `json:"context_commit_id,omitempty"`
	ContextDigest        string             `json:"context_digest,omitempty"`
	ContextRevision      uint64             `json:"context_revision,omitempty"`
	StateEpoch           uint64             `json:"state_epoch,omitempty"`
}

func (*CheckpointForkedData) eventKind() EventKind { return EventCheckpointForked }

func (d *CheckpointForkedData) validate() error {
	if !validProfileIdentifier(d.CheckpointID) ||
		!validProfileIdentifier(string(d.NewThreadID)) ||
		strings.TrimSpace(d.Title) == "" || len(d.Title) > 256 ||
		strings.ContainsAny(d.Title, "\x00\r\n") ||
		len(d.ReplacementHistory) == 0 ||
		d.WorkspaceClaimsValid && !d.ExactContext ||
		!validContextCommitReference(
			d.ExactContext,
			d.ContextCommitID,
			d.ContextDigest,
			d.ContextRevision,
			d.StateEpoch,
		) ||
		d.InvalidatedClaims < 0 || d.StaleClaims < 0 {
		return errors.New("checkpoint fork data is invalid")
	}
	return validateCompactedHistory(d.ReplacementHistory)
}

func validContextCommitReference(
	exact bool,
	commitID string,
	digest string,
	revision uint64,
	epoch uint64,
) bool {
	present := commitID != "" || digest != "" || revision != 0 || epoch != 0
	if !present {
		return true
	}
	return exact && validProfileIdentifier(commitID) &&
		strings.TrimSpace(digest) != "" && len(digest) <= 256 &&
		revision != 0 && epoch != 0
}

func (*ThreadForkedData) eventKind() EventKind { return EventThreadForked }

func (d *ThreadForkedData) validate() error {
	if d.NewThreadID == "" {
		return errors.New("forked thread id is required")
	}
	if len(d.ReplacementHistory) > 0 && d.WindowID == "" {
		return errors.New("forked thread window_id is required")
	}
	return nil
}

type TurnRevertedData struct {
	TargetTurnID               TurnID           `json:"target_turn_id"`
	Restored                   []string         `json:"restored"`
	Conflicts                  []RevertConflict `json:"conflicts"`
	NonFileSideEffectsReverted bool             `json:"non_file_side_effects_reverted"`
	NonFileSideEffectsNote     string           `json:"non_file_side_effects_note"`
}

type RevertConflict struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

func (*TurnRevertedData) eventKind() EventKind { return EventTurnReverted }

func (d *TurnRevertedData) validate() error {
	if d.TargetTurnID == "" {
		return errors.New("reverted target turn id is required")
	}
	if d.NonFileSideEffectsReverted {
		return errors.New("workspace revert cannot claim non-file side effects were reverted")
	}
	return nil
}

type Event struct {
	Version     int         `json:"version"`
	ID          EventID     `json:"id"`
	Sequence    Cursor      `json:"sequence"`
	OperationID OperationID `json:"operation_id"`
	ThreadID    ThreadID    `json:"thread_id"`
	TurnID      TurnID      `json:"turn_id"`
	ItemID      ItemID      `json:"item_id"`
	Kind        EventKind   `json:"kind"`
	CreatedAt   time.Time   `json:"created_at"`
	Data        EventData   `json:"data"`
}

type EventMeta struct {
	Sequence    Cursor
	OperationID OperationID
	ThreadID    ThreadID
	TurnID      TurnID
	ItemID      ItemID
}

func NewEvent(meta EventMeta, data EventData) (Event, error) {
	if data == nil {
		return Event{}, errors.New("event data is required")
	}
	id, err := newID("evt")
	if err != nil {
		return Event{}, err
	}
	event := Event{
		Version:     Version,
		ID:          EventID(id),
		Sequence:    meta.Sequence,
		OperationID: meta.OperationID,
		ThreadID:    meta.ThreadID,
		TurnID:      meta.TurnID,
		ItemID:      meta.ItemID,
		Kind:        data.eventKind(),
		CreatedAt:   time.Now().UTC(),
		Data:        data,
	}
	return event, event.Validate()
}

func NewEventWithIdentity(
	meta EventMeta,
	id EventID,
	createdAt time.Time,
	data EventData,
) (Event, error) {
	if data == nil {
		return Event{}, errors.New("event data is required")
	}
	event := Event{
		Version:     Version,
		ID:          id,
		Sequence:    meta.Sequence,
		OperationID: meta.OperationID,
		ThreadID:    meta.ThreadID,
		TurnID:      meta.TurnID,
		ItemID:      meta.ItemID,
		Kind:        data.eventKind(),
		CreatedAt:   createdAt.UTC(),
		Data:        data,
	}
	return event, event.Validate()
}

func IsTerminalEvent(kind EventKind) bool {
	traits, ok := Traits(kind)
	return ok && traits.Terminal
}
