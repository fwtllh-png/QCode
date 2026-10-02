package app

import (
	"context"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/persist/snapshot"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	agentengine "github.com/fwtllh-png/QCode/internal/runtime/agent/engine"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type SessionArtifactStore interface {
	SaveCheckpoint(
		context.Context,
		protocol.SessionCheckpoint,
		[]protocol.CompactedMessage,
		protocol.SessionProfile,
	) (protocol.SessionCheckpoint, error)
	GetCheckpoint(
		context.Context,
		string,
	) (
		protocol.SessionCheckpoint,
		[]protocol.CompactedMessage,
		protocol.SessionProfile,
		error,
	)
	ListCheckpoints(
		context.Context,
		string,
		int,
	) ([]protocol.SessionCheckpoint, error)
	CountCheckpoints(context.Context, string) (int, error)
	SavePlan(
		context.Context,
		protocol.SessionPlanArtifact,
	) (protocol.SessionPlanArtifact, error)
	GetPlan(context.Context, string) (protocol.SessionPlanArtifact, error)
	LatestPlan(
		context.Context,
		string,
		protocol.ThreadID,
	) (protocol.SessionPlanArtifact, bool, error)
}

// SessionCheckpointSummaryStore optionally batches the sidebar projection.
type SessionCheckpointSummaryStore interface {
	CheckpointSummaries(context.Context, []string) (map[string]snapshot.CheckpointSummary, error)
}

type ContextSessionArtifactStore interface {
	SessionArtifactStore
	SaveContextCheckpoint(
		context.Context,
		protocol.SessionCheckpoint,
		[]protocol.CompactedMessage,
		agentcontext.ContextSnapshot,
		protocol.SessionProfile,
	) (protocol.SessionCheckpoint, error)
	GetContextCheckpoint(
		context.Context,
		string,
	) (
		protocol.SessionCheckpoint,
		agentcontext.ContextSnapshot,
		protocol.SessionProfile,
		error,
	)
}

type CheckpointEngine interface {
	History(protocol.ThreadID) ([]provider.Message, error)
	RestoreCheckpoint(protocol.ThreadID, []provider.Message) error
	ForkCheckpoint(
		protocol.ThreadID,
		protocol.ThreadID,
		[]provider.Message,
	) error
	Release(protocol.ThreadID)
}

type ContextCheckpointEngine interface {
	CheckpointEngine
	ContextSnapshot(protocol.ThreadID) (agentcontext.ContextSnapshot, error)
	RestoreContext(
		protocol.ThreadID,
		agentcontext.ContextSnapshot,
	) (agentcontext.ReconciliationReceipt, error)
	ForkContext(
		protocol.ThreadID,
		protocol.ThreadID,
		agentcontext.ContextSnapshot,
	) (agentcontext.ReconciliationReceipt, error)
}

type ContextRebaseStore interface {
	CommitContextRebase(
		context.Context,
		agentcontext.ContextRebaseEnvelope,
	) error
	LatestContextSnapshot(
		context.Context,
		protocol.ThreadID,
	) (agentcontext.ContextSnapshot, bool, error)
}

type CurrentContextStore interface {
	CommitCurrentContext(
		context.Context,
		agentcontext.CurrentContextCommit,
	) error
	DeleteCurrentContext(
		context.Context,
		protocol.ThreadID,
		string,
		bool,
	) error
}

type ContextMaintenanceEngine interface {
	// PreparePostTurnNarrative captures the narrative input synchronously and
	// returns a runner that settles it off the turn queue's critical path. A
	// nil runner with a nil error means no narrative is scheduled. The engine
	// joins the pending narrative before its next turn starts.
	PreparePostTurnNarrative(
		protocol.ThreadID,
		protocol.TurnID,
	) (agentengine.PostTurnNarrativeRunner, error)
}

type PlanExecutionPreparation struct {
	Artifact protocol.SessionPlanArtifact
	Prompt   string
}

type TurnRecoveryPreparation struct {
	Prompt         string
	DisplayPrompt  string
	Intent         protocol.TurnIntent
	IdempotencyKey string
	Recovery       protocol.TurnRecoveryContext
}
