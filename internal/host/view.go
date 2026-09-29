package host

import (
	"time"

	usagestate "github.com/fwtllh-png/QCode/internal/observability/usage"
	"github.com/fwtllh-png/QCode/internal/orchestration/subagent"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type Agent struct {
	ID          string          `json:"id"`
	Path        string          `json:"path"`
	Revision    uint64          `json:"revision"`
	Workspace   string          `json:"workspace"`
	SessionID   string          `json:"session_id"`
	ThreadID    string          `json:"thread_id"`
	ParentID    string          `json:"parent_id,omitempty"`
	ParentPath  string          `json:"parent_path"`
	Role        subagent.Role   `json:"role"`
	Profile     string          `json:"profile,omitempty"`
	Stance      subagent.Stance `json:"stance"`
	Depth       int             `json:"depth"`
	Status      subagent.Status `json:"status"`
	TurnID      string          `json:"turn_id,omitempty"`
	LastMessage string          `json:"last_message,omitempty"`
	Worktree    string          `json:"worktree,omitempty"`
	Isolated    bool            `json:"isolated"`
	Serialized  bool            `json:"serialized"`
	Closed      bool            `json:"closed"`
}

type Usage struct {
	SessionID       string                            `json:"session_id"`
	ThreadID        protocol.ThreadID                 `json:"thread_id"`
	TurnID          protocol.TurnID                   `json:"turn_id"`
	Provider        string                            `json:"provider"`
	Model           string                            `json:"model"`
	ModelMetadata   *protocol.ModelMetadataProvenance `json:"model_metadata_provenance,omitempty"`
	InputTokens     uint64                            `json:"input_tokens"`
	OutputTokens    uint64                            `json:"output_tokens"`
	ReasoningTokens uint64                            `json:"reasoning_tokens"`
	CachedTokens    uint64                            `json:"cached_tokens"`
	CostMicrounits  uint64                            `json:"cost_microunits"`
	PricedCalls     uint64                            `json:"priced_calls"`
	UnpricedCalls   uint64                            `json:"unpriced_calls"`
	Calls           uint64                            `json:"calls"`
	FirstAt         time.Time                         `json:"first_at"`
	LastAt          time.Time                         `json:"last_at"`
}

type UsageRollup struct {
	Activity        *usagestate.Activity `json:"activity,omitempty"`
	Turns           uint64               `json:"turns"`
	Calls           uint64               `json:"calls"`
	InputTokens     uint64               `json:"input_tokens"`
	OutputTokens    uint64               `json:"output_tokens"`
	ReasoningTokens uint64               `json:"reasoning_tokens"`
	CachedTokens    uint64               `json:"cached_tokens"`
	TotalTokens     uint64               `json:"total_tokens"`
	CachedShare     float64              `json:"cached_share"`
	CostMicrounits  uint64               `json:"cost_microunits"`
	PricedCalls     uint64               `json:"priced_calls"`
	UnpricedCalls   uint64               `json:"unpriced_calls"`
	CostKnown       bool                 `json:"cost_known"`
}

func AgentFrom(value subagent.Agent) Agent {
	return Agent{
		ID: value.ID, Path: value.Path, Revision: value.Revision,
		Workspace: value.Workspace, SessionID: value.SessionID,
		ThreadID: value.ThreadID, ParentID: value.Parent, ParentPath: value.ParentPath,
		Role: value.Role, Profile: value.Profile,
		Stance: value.Stance, Depth: value.Depth, Status: value.Status,
		TurnID: value.TurnID, LastMessage: value.LastMessage, Worktree: value.Worktree,
		Isolated: value.Isolated, Serialized: value.Serialized, Closed: value.Closed,
	}
}

func UsageFrom(value usagestate.Aggregate) Usage {
	return Usage{
		SessionID: value.SessionID, ThreadID: value.ThreadID, TurnID: value.TurnID,
		Provider: value.Provider, Model: value.Model,
		ModelMetadata: value.ModelMetadata,
		InputTokens:   value.InputTokens, OutputTokens: value.OutputTokens,
		ReasoningTokens: value.ReasoningTokens, CachedTokens: value.CachedTokens,
		CostMicrounits: value.CostMicrounits,
		PricedCalls:    value.PricedCalls, UnpricedCalls: value.UnpricedCalls,
		Calls: value.Calls, FirstAt: value.FirstAt, LastAt: value.LastAt,
	}
}

func UsageRollupFrom(value usagestate.Rollup) UsageRollup {
	return UsageRollup{
		Activity: value.Activity,
		Turns:    value.Turns, Calls: value.Calls,
		InputTokens: value.InputTokens, OutputTokens: value.OutputTokens,
		ReasoningTokens: value.ReasoningTokens, CachedTokens: value.CachedTokens,
		TotalTokens: value.TotalTokens(), CachedShare: value.CachedShare(),
		CostMicrounits: value.CostMicrounits, PricedCalls: value.PricedCalls,
		UnpricedCalls: value.UnpricedCalls, CostKnown: value.CostKnown(),
	}
}
