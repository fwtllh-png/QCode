package artifact

import (
	"context"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	agentengine "github.com/fwtllh-png/QCode/internal/runtime/agent/engine"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type ArtifactRuntime interface {
	SessionStatus(context.Context, string) (protocol.SessionSummary, error)
	SessionProfile(context.Context, string) (protocol.SessionProfileSnapshot, error)
	SessionProfilesAvailable() bool
	RestoreSessionProfile(
		context.Context,
		string,
		protocol.ThreadID,
	) (protocol.SessionProfileSnapshot, error)
	UpdateSessionProfile(
		context.Context,
		string,
		protocol.ThreadID,
		uint64,
		protocol.SessionProfilePatch,
	) (protocol.SessionProfileUpdateResult, error)

	ReplayArtifactEvents(context.Context, protocol.Cursor) ([]protocol.Event, error)
	ReplayArtifactTurn(context.Context, protocol.TurnID) ([]protocol.Event, error)
	ReplayArtifactKind(context.Context, protocol.EventKind) ([]protocol.Event, error)
	PublishArtifactEvent(
		protocol.OperationID,
		protocol.ThreadID,
		protocol.TurnID,
		protocol.ItemID,
		protocol.EventData,
	) error
	ArtifactStore() SessionArtifactStore
	CheckpointRuntime() any
	Durable() bool
	ContextRebaseStore() ContextRebaseStore
	SessionPersistenceAvailable() bool
	SessionForThread(context.Context, protocol.ThreadID) (string, error)
	ActivateThread(
		context.Context,
		string,
		protocol.ThreadID,
	) (protocol.SessionSummary, error)
	StoredProfile(
		context.Context,
		string,
		protocol.SessionProfile,
	) (protocol.SessionProfile, error)
	DefaultProfile() protocol.SessionProfile
	ReportArtifactError(string, protocol.Event, error)
}

type Service struct {
	ArtifactRuntime
}

func NewArtifactService(runtime ArtifactRuntime) *Service {
	return &Service{ArtifactRuntime: runtime}
}

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

type SessionCheckpointSummary struct {
	Count        int
	ChangedFiles int
}

// SessionCheckpointSummaryStore optionally batches the sidebar projection.
type SessionCheckpointSummaryStore interface {
	CheckpointSummaries(context.Context, []string) (map[string]SessionCheckpointSummary, error)
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
