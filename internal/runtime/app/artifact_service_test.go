package app

import (
	"context"
	"errors"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/persist/snapshot"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type memoryArtifactStore struct {
	checkpointReads int
	checkpoint      protocol.SessionCheckpoint
	history         []protocol.CompactedMessage
	profile         protocol.SessionProfile
	plan            protocol.SessionPlanArtifact
	context         agentcontext.ContextSnapshot
}

func (s *memoryArtifactStore) SaveCheckpoint(
	context.Context,
	protocol.SessionCheckpoint,
	[]protocol.CompactedMessage,
	protocol.SessionProfile,
) (protocol.SessionCheckpoint, error) {
	return protocol.SessionCheckpoint{}, errors.New("unexpected Checkpoint save")
}

func (s *memoryArtifactStore) GetCheckpoint(
	context.Context,
	string,
) (snapshot.CheckpointState, error) {
	s.checkpointReads++
	loaded := snapshot.CheckpointState{
		Checkpoint: s.checkpoint,
		History:    append([]protocol.CompactedMessage(nil), s.history...),
		Profile:    s.profile,
	}
	if s.checkpoint.ContextDigest != "" {
		value := agentcontext.CloneContextSnapshot(s.context)
		loaded.Context = &value
	}
	return loaded, nil
}

func (s *memoryArtifactStore) SaveContextCheckpoint(
	context.Context,
	protocol.SessionCheckpoint,
	agentcontext.ContextSnapshot,
	protocol.SessionProfile,
) (protocol.SessionCheckpoint, error) {
	return protocol.SessionCheckpoint{}, errors.New("unexpected Context Checkpoint save")
}

func (s *memoryArtifactStore) ListCheckpoints(
	context.Context,
	string,
	int,
) ([]protocol.SessionCheckpoint, error) {
	return []protocol.SessionCheckpoint{s.checkpoint}, nil
}

func (s *memoryArtifactStore) CountCheckpoints(
	context.Context,
	string,
) (int, error) {
	return 1, nil
}

func (s *memoryArtifactStore) SavePlan(
	context.Context,
	protocol.SessionPlanArtifact,
) (protocol.SessionPlanArtifact, error) {
	return protocol.SessionPlanArtifact{}, errors.New("unexpected Plan save")
}

func (s *memoryArtifactStore) GetPlan(
	context.Context,
	string,
) (protocol.SessionPlanArtifact, error) {
	return s.plan, nil
}

func (s *memoryArtifactStore) LatestPlan(
	context.Context,
	string,
	protocol.ThreadID,
) (protocol.SessionPlanArtifact, bool, error) {
	return s.plan, s.plan.ID != "", nil
}

type artifactTestEngine struct {
	profileTestEngine
	history      []provider.Message
	historyReads int
	contextReads int
	restores     int
	restoreErrAt int
	restoreErr   error
	forks        map[protocol.ThreadID][]provider.Message
	contexts     map[protocol.ThreadID]agentcontext.ContextSnapshot
}

type artifactFailingEventStore struct {
	EventStore
	kind protocol.EventKind
	err  error
}

func (s artifactFailingEventStore) Append(
	ctx context.Context,
	event protocol.Event,
) error {
	if event.Kind == s.kind {
		return s.err
	}
	return s.EventStore.Append(ctx, event)
}

func (e *artifactTestEngine) History(
	protocol.ThreadID,
) ([]provider.Message, error) {
	e.historyReads++
	return append([]provider.Message(nil), e.history...), nil
}

func (e *artifactTestEngine) RestoreCheckpoint(
	_ protocol.ThreadID,
	history []provider.Message,
) error {
	e.restores++
	if e.restoreErrAt == e.restores {
		return e.restoreErr
	}
	e.history = append([]provider.Message(nil), history...)
	return nil
}

func (e *artifactTestEngine) ForkCheckpoint(
	_ protocol.ThreadID,
	child protocol.ThreadID,
	history []provider.Message,
) error {
	if e.forks == nil {
		e.forks = make(map[protocol.ThreadID][]provider.Message)
	}
	e.forks[child] = append([]provider.Message(nil), history...)
	return nil
}

func (e *artifactTestEngine) Release(threadID protocol.ThreadID) {
	delete(e.forks, threadID)
	delete(e.contexts, threadID)
}

func (e *artifactTestEngine) ContextSnapshot(
	threadID protocol.ThreadID,
) (agentcontext.ContextSnapshot, error) {
	e.contextReads++
	snapshot, ok := e.contexts[threadID]
	if !ok {
		return agentcontext.ContextSnapshot{}, errors.New("context snapshot is unavailable")
	}
	return agentcontext.CloneContextSnapshot(snapshot), nil
}

func (e *artifactTestEngine) RestoreContext(
	threadID protocol.ThreadID,
	snapshot agentcontext.ContextSnapshot,
) (agentcontext.ReconciliationReceipt, error) {
	if e.contexts == nil {
		e.contexts = make(map[protocol.ThreadID]agentcontext.ContextSnapshot)
	}
	e.contexts[threadID] = agentcontext.CloneContextSnapshot(snapshot)
	e.history = append([]provider.Message(nil), snapshot.History...)
	return agentcontext.ReconciliationReceipt{BindingMatch: true}, nil
}

func (e *artifactTestEngine) ForkContext(
	_ protocol.ThreadID,
	threadID protocol.ThreadID,
	snapshot agentcontext.ContextSnapshot,
) (agentcontext.ReconciliationReceipt, error) {
	if e.contexts == nil {
		e.contexts = make(map[protocol.ThreadID]agentcontext.ContextSnapshot)
	}
	e.contexts[threadID] = agentcontext.CloneContextSnapshot(snapshot)
	if e.forks == nil {
		e.forks = make(map[protocol.ThreadID][]provider.Message)
	}
	e.forks[threadID] = append([]provider.Message(nil), snapshot.History...)
	return agentcontext.ReconciliationReceipt{BindingMatch: true}, nil
}

type artifactCurrentContextStore struct {
	current map[protocol.ThreadID]agentcontext.CurrentContextCommit
}

func (s *artifactCurrentContextStore) CommitContextRebase(
	context.Context,
	agentcontext.ContextRebaseEnvelope,
) error {
	return nil
}

func (s *artifactCurrentContextStore) LatestContextSnapshot(
	_ context.Context,
	threadID protocol.ThreadID,
) (agentcontext.ContextSnapshot, bool, error) {
	commit, ok := s.current[threadID]
	return agentcontext.CloneContextSnapshot(commit.Snapshot), ok, nil
}

func (s *artifactCurrentContextStore) CommitCurrentContext(
	_ context.Context,
	commit agentcontext.CurrentContextCommit,
) error {
	if err := commit.Validate(); err != nil {
		return err
	}
	if s.current == nil {
		s.current = make(map[protocol.ThreadID]agentcontext.CurrentContextCommit)
	}
	s.current[commit.ThreadID] = commit
	return nil
}

func (s *artifactCurrentContextStore) DeleteCurrentContext(
	_ context.Context,
	threadID protocol.ThreadID,
	commitID string,
	_ bool,
) error {
	if current, ok := s.current[threadID]; ok && current.ID == commitID {
		delete(s.current, threadID)
	}
	return nil
}

func artifactLifecycle() *memorySessionLifecycleStore {
	now := time.Now().UTC()
	return &memorySessionLifecycleStore{summary: protocol.SessionSummary{
		Version:         protocol.SessionLifecycleVersion,
		Revision:        1,
		SessionID:       "session-profile",
		ThreadID:        "thread-profile",
		Title:           "Artifacts",
		Status:          protocol.SessionStatusIdle,
		Isolation:       "shared",
		WorkspaceRoot:   "/workspace",
		WorkspaceLabel:  "workspace",
		ExecutionTarget: "local",
		CreatedAt:       now,
		UpdatedAt:       now,
	}}
}
