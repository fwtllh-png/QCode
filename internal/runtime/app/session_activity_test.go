package app

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/persist/artifact"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type batchLifecycleStore struct {
	memorySessionLifecycleStore
	summaries []protocol.SessionSummary
	reads     int
}

func (s *batchLifecycleStore) ListLifecycle(context.Context, protocol.SessionListQuery) (protocol.SessionList, error) {
	return protocol.SessionList{Version: protocol.SessionLifecycleVersion, Sessions: s.summaries}, nil
}

func (*batchLifecycleStore) ThreadIDs(context.Context, string) ([]protocol.ThreadID, error) {
	panic("per-session thread read")
}

func (s *batchLifecycleStore) ThreadIDsForSessions(_ context.Context, ids []string) (map[string][]protocol.ThreadID, error) {
	s.reads++
	result := make(map[string][]protocol.ThreadID, len(ids))
	for _, id := range ids {
		result[id] = []protocol.ThreadID{protocol.ThreadID(id), protocol.ThreadID(id + "-fork")}
	}
	return result, nil
}

type batchCheckpointStore struct {
	SessionArtifactStore
	reads int
}

func (*batchCheckpointStore) CountCheckpoints(context.Context, string) (int, error) {
	panic("per-session checkpoint read")
}

func (s *batchCheckpointStore) CheckpointSummaries(_ context.Context, ids []string) (map[string]artifact.SessionCheckpointSummary, error) {
	s.reads++
	result := make(map[string]artifact.SessionCheckpointSummary, len(ids))
	for i, id := range ids {
		result[id] = artifact.SessionCheckpointSummary{Count: i + 1, ChangedFiles: i + 2}
	}
	return result, nil
}

type batchWithdrawalStore struct {
	appWithdrawalStore
	reads int
}

func (*batchWithdrawalStore) TurnWithdrawn(context.Context, protocol.ThreadID, protocol.TurnID) (bool, error) {
	panic("per-session withdrawal read")
}

func (s *batchWithdrawalStore) TurnsWithdrawn(_ context.Context, turns map[protocol.ThreadID]protocol.TurnID) (map[protocol.ThreadID]bool, error) {
	s.reads++
	return map[protocol.ThreadID]bool{"session-search": turns["session-search"] != ""}, nil
}

func TestListSessionsBatchesActivityAndReusesThreadOwnershipForSearch(t *testing.T) {
	for _, size := range []int{2, 32} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			lifecycle := &batchLifecycleStore{}
			for i := 0; i < size; i++ {
				id := fmt.Sprint("session-", i)
				if i == 0 {
					id = "session-search"
				}
				lifecycle.summaries = append(lifecycle.summaries, protocol.SessionSummary{
					Version: protocol.SessionLifecycleVersion, Revision: 1,
					SessionID: id, ThreadID: protocol.ThreadID(id), LatestTurnID: protocol.TurnID(id),
					Title: "needle", Status: protocol.SessionStatusCompleted, Isolation: "shared",
					WorkspaceRoot: "/workspace", WorkspaceLabel: "workspace", ExecutionTarget: "local",
					CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
				})
			}
			checkpoints := &batchCheckpointStore{}
			withdrawals := &batchWithdrawalStore{}
			events := &indexedSearchEventStore{MemoryEventStore: NewMemoryEventStore(16)}
			runtime := NewRuntime(Options{
				SessionLifecycle: lifecycle, SessionArtifacts: checkpoints,
				ContextRebaseStore: withdrawals, EventStore: events,
			})
			t.Cleanup(func() { closeRuntime(t, runtime) })
			runtime.EventService.mu.Lock()
			runtime.approvals["approval"] = PendingApproval{ThreadID: "session-1-fork"}
			runtime.EventService.mu.Unlock()
			page, err := runtime.ListSessions(t.Context(), protocol.SessionListQuery{Query: "needle"})
			if err != nil {
				t.Fatal(err)
			}
			if lifecycle.reads != 1 || checkpoints.reads != 1 || withdrawals.reads != 1 {
				t.Fatalf("batch calls threads=%d checkpoints=%d withdrawals=%d", lifecycle.reads, checkpoints.reads, withdrawals.reads)
			}
			if len(events.threads) != size*2 || events.threads["session-1-fork"] != "session-1" {
				t.Fatalf("search ownership: %v", events.threads)
			}
			if len(page.Sessions) != size || !page.Sessions[0].LatestTurnWithdrawn ||
				page.Sessions[0].Status != protocol.SessionStatusIdle ||
				page.Sessions[1].Status != protocol.SessionStatusAwaitingApproval ||
				page.Sessions[1].CheckpointCount != 2 || page.Sessions[1].ChangedFiles != 3 {
				t.Fatalf("activity page: %+v", page)
			}
		})
	}
}
