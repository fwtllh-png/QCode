package engine

import (
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/mcp"
	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	providerwire "github.com/fwtllh-png/QCode/internal/adapter/provider/wire"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	toolguard "github.com/fwtllh-png/QCode/internal/adapter/tool/guard"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/interact"
	"github.com/fwtllh-png/QCode/internal/observability/diagnostics"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	promptcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/prompt"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type State string

type Event struct {
	State         State                             `json:"state"`
	Turn          uint64                            `json:"turn"`
	Provider      string                            `json:"provider,omitempty"`
	Model         string                            `json:"model,omitempty"`
	ModelMetadata *protocol.ModelMetadataProvenance `json:"model_metadata_provenance,omitempty"`
	// Purpose is which route the turn's samples go to, and so why this provider
	// and model rather than the session's default pair.
	Purpose            string                 `json:"purpose,omitempty"`
	ProfileRevision    uint64                 `json:"profile_revision,omitempty"`
	Mode               string                 `json:"mode,omitempty"`
	Posture            string                 `json:"posture,omitempty"`
	Workspace          string                 `json:"workspace,omitempty"`
	WorkspaceIsolation string                 `json:"workspace_isolation,omitempty"`
	Sandbox            string                 `json:"sandbox,omitempty"`
	Text               string                 `json:"text,omitempty"`
	Block              *provider.ContentBlock `json:"block,omitempty"`
	ToolCall           *provider.ToolCall     `json:"tool_call,omitempty"`
	Result             *tool.Result           `json:"result,omitempty"`
	Search             *provider.SearchResult `json:"search,omitempty"`
	Citation           *provider.Citation     `json:"citation,omitempty"`
	Usage              *provider.Usage        `json:"usage,omitempty"`
	CostUSD            float64                `json:"cost_usd,omitempty"`
	// CostKnown reports whether the model has pricing at all, so a consumer can
	// tell a free call from an unpriced one instead of reading both as zero.
	CostKnown bool `json:"cost_known,omitempty"`
	// Sample is which provider call within the turn a usage report belongs to.
	// Usage is cumulative within a sample, so a consumer keeps the last report
	// per sample rather than adding them up.
	Sample             uint32                            `json:"sample,omitempty"`
	SampleID           string                            `json:"sample_id,omitempty"`
	SampleContext      *protocol.SampleContextData       `json:"sample_context,omitempty"`
	ErrorCode          protocol.ErrorCode                `json:"error_code,omitempty"`
	Error              string                            `json:"error,omitempty"`
	Fault              *protocol.FaultMetadata           `json:"fault,omitempty"`
	Convergence        *protocol.TurnConvergence         `json:"convergence,omitempty"`
	CancelReason       string                            `json:"cancel_reason,omitempty"`
	SecondaryIssues    []TerminalIssue                   `json:"secondary_issues,omitempty"`
	Compaction         *CompactionReceipt                `json:"compaction,omitempty"`
	ContextBudget      *ContextBudgetSnapshot            `json:"context_budget,omitempty"`
	Approval           *toolguard.ApprovalRequest        `json:"approval,omitempty"`
	ApprovalResolution *ApprovalResolution               `json:"approval_resolution,omitempty"`
	Input              *interact.Request                 `json:"input,omitempty"`
	Diagnostics        []diagnostics.Receipt             `json:"diagnostics,omitempty"`
	FileChanges        []tool.WorkspaceChange            `json:"file_changes,omitempty"`
	Verification       *VerificationReceipt              `json:"verification,omitempty"`
	Completion         *tool.CompletionDeclaration       `json:"completion,omitempty"`
	ProviderRetry      *ProviderRetry                    `json:"provider_retry,omitempty"`
	ModelExecution     *ModelExecution                   `json:"model_execution,omitempty"`
	ReasoningCompleted *ModelReasoning                   `json:"reasoning_completed,omitempty"`
	OutputDiscarded    *ModelOutputDiscarded             `json:"output_discarded,omitempty"`
	Commentary         *protocol.CommentaryCompletedData `json:"commentary,omitempty"`
	ToolOutput         *ToolOutput                       `json:"tool_output,omitempty"`
	CatalogChanged     *CatalogChanged                   `json:"catalog_changed,omitempty"`
	MCPHealthChanged   *MCPHealthChanged                 `json:"mcp_health_changed,omitempty"`
}

func modelMetadataProvenance(
	value model.MetadataProvenance,
) *protocol.ModelMetadataProvenance {
	return &protocol.ModelMetadataProvenance{
		CanonicalID:  string(value.CanonicalID),
		WireID:       string(value.WireID),
		Limits:       string(value.Limits),
		Capabilities: string(value.Capabilities),
		Pricing:      string(value.Pricing),
	}
}

type ApprovalResolution struct {
	RequestID string `json:"request_id"`
	Decision  string `json:"decision"`
	Reason    string `json:"reason,omitempty"`
}

type ModelExecution struct {
	Kind                 string                     `json:"kind"`
	SampleID             string                     `json:"sample_id"`
	Attempt              uint32                     `json:"attempt,omitempty"`
	Status               string                     `json:"status,omitempty"`
	Reason               string                     `json:"reason,omitempty"`
	ProjectedInputTokens uint64                     `json:"projected_input_tokens,omitempty"`
	Transport            provider.TransportMetadata `json:"transport,omitempty"`
	StopReason           provider.StopReason        `json:"stop_reason,omitempty"`
	StartedAt            time.Time                  `json:"started_at,omitempty"`
	FinishedAt           time.Time                  `json:"finished_at,omitempty"`
	RateLimitRetries     uint32                     `json:"rate_limit_retries,omitempty"`
	RateLimitRetryLimit  uint32                     `json:"rate_limit_retry_limit,omitempty"`
	RateLimitWaited      time.Duration              `json:"rate_limit_waited,omitempty"`
	RateLimitWaitBudget  time.Duration              `json:"rate_limit_wait_budget,omitempty"`
}

type ModelReasoning struct {
	SampleID string `json:"sample_id"`
	Text     string `json:"text"`
}

// ModelOutputDiscarded retracts the provisional text deltas a sample streamed
// to the host. Deltas are optimistic: a transport retry restarts the sample,
// and text that arrives with tool calls is narration that the durable
// commentary projection republishes. Reason names the retraction cause.
type ModelOutputDiscarded struct {
	SampleID string `json:"sample_id"`
	Reason   string `json:"reason"`
}

type ProviderRetry = providerwire.RetryDecision

// TerminalIssue is a cleanup/finalization failure that happened after the
// primary turn outcome was already known.
type TerminalIssue struct {
	Phase   string             `json:"phase"`
	Code    protocol.ErrorCode `json:"code"`
	Message string             `json:"message"`
}

type CatalogChanged struct {
	CatalogID  string
	Generation uint64
	Digest     string
	Added      []tool.CatalogChange
	Replaced   []tool.CatalogChange
	Revoked    []tool.CatalogChange
}

type MCPHealthSnapshot = mcp.HealthSnapshot
type MCPHealthChanged = mcp.ProjectedHealthChange

// ToolOutput is one piece of a tool's output, delivered while the tool is still
// running. It exists because a command that takes a minute used to produce nothing
// observable until it finished.
type ToolOutput struct {
	Tool   string `json:"tool"`
	CallID string `json:"call_id"`
	Stream string `json:"stream"`
	Chunk  string `json:"chunk"`
	// Cursor is the byte count of this stream through the end of this chunk, so a
	// consumer can tell that it missed something.
	Cursor uint64 `json:"cursor"`
	// Truncated marks the last chunk a call streams once it has spent its
	// streaming budget. The full output still arrives with the tool result; what
	// stops is the live commentary.
	Truncated bool `json:"truncated,omitempty"`
}

type CompactionReceipt = promptcontext.CompactionReceipt

const (
	CompactionPhasePreSampling = "pre_sampling"
	CompactionPhaseMidTurn     = "mid_turn"
	CompactionPhasePostTurn    = "post_turn"
)

// ContextBudgetSnapshot freezes the exact retained context visible when a
// terminal event is emitted. Receipts consume this snapshot instead of racing
// a later read of mutable engine history.
type ContextBudgetSnapshot = agentcontext.BudgetSnapshot

type Budget struct {
	MaxTokens     uint64
	MaxTurnTokens uint64
	MaxCostUSD    float64
}

type CompactWindowPolicy = agentcontext.WindowPolicy

type TokenEstimator interface {
	Estimate([]provider.Message) (uint64, error)
}

type HeuristicTokenEstimator struct{}

func (HeuristicTokenEstimator) Estimate(messages []provider.Message) (uint64, error) {
	return agentcontext.EstimateMessageTokens(messages), nil
}

func (HeuristicTokenEstimator) EstimateImage(
	attachment provider.Attachment,
) (uint64, error) {
	return agentcontext.EstimateImageTokens(attachment), nil
}

type Result struct {
	Turn         uint64                  `json:"turn"`
	Text         string                  `json:"text"`
	Reasoning    string                  `json:"reasoning,omitempty"`
	State        State                   `json:"state"`
	Usage        provider.Usage          `json:"usage"`
	CostUSD      float64                 `json:"cost_usd"`
	Tools        []provider.ToolCall     `json:"tools,omitempty"`
	Searches     []provider.SearchResult `json:"searches,omitempty"`
	Citations    []provider.Citation     `json:"citations,omitempty"`
	Verification *VerificationReceipt    `json:"verification,omitempty"`
}

// PendingSource tags why an input was enqueued into the turn-local queue (N1).
type PendingSource string

const (
	PendingSteer   PendingSource = "steer"
	PendingMailbox PendingSource = "mailbox"
)

// PendingInput is one typed pending-work item drained into the active turn history.
type PendingInput struct {
	Source      PendingSource
	Prompt      string
	TriggerTurn bool // mailbox only; non-trigger stays in mailboxHold until the next turn
}

// BudgetSnapshot is the pool this engine draws from: what previous turns spent
// and the ceilings they were spent against. A zero ceiling is no ceiling.
//
// It counts finished turns only, because a turn's own usage is folded into the
// pool when it completes. A caller reporting "remaining" for the turn in flight
// has to add that turn's own spend, which it has and the engine does not.
//
// Child agents and background turns run their own engines and therefore their
// own pools; this reports one pool and never merges them.
func (e *Engine) BudgetSnapshot() BudgetSnapshot {
	if e == nil {
		return BudgetSnapshot{}
	}

	usage, cost := e.accountedUsage()
	return BudgetSnapshot{
		TokensUsed: usage.Total(), MaxTokens: e.options.Budget.MaxTokens,
		CostUSD: cost, MaxCostUSD: e.options.Budget.MaxCostUSD,
	}
}

// BudgetSnapshot is what a host needs to show how much budget is left without
// recomputing the engine's accounting for itself.
type BudgetSnapshot struct {
	TokensUsed uint64
	MaxTokens  uint64
	CostUSD    float64
	MaxCostUSD float64
}
