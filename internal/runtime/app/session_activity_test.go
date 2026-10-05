package app

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/persist/snapshot"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type batchLifecycleStore struct {
	memorySessionLifecycleStore
	summaries []protocol.SessionSummary
	reads     int
}

func TestListSessionsProjectsLiveActivityAndRefreshesEachRequest(t *testing.T) {
	lifecycle := &batchLifecycleStore{summaries: activitySummaries(5)}
	runtime := NewRuntime(Options{SessionLifecycle: lifecycle})
	t.Cleanup(func() { closeRuntime(t, runtime) })
	var leases []ActiveTurnLease
	for _, thread := range []protocol.ThreadID{"session-0-fork", "session-1", "session-2-fork", "outside"} {
		lease, err := runtime.active.Reserve(thread, protocol.TurnID(thread), protocol.OperationID(thread), "")
		if err != nil {
			t.Fatal(err)
		}
		leases = append(leases, lease)
	}
	runtime.OperationService.restore(map[protocol.OperationID]PendingOperation{
		"approval": {SessionID: "session-0"},
		"input":    {SessionID: "session-1"},
		// Accepted operations use their Session identity, even before the thread is registered.
		"pending": {SessionID: "session-3"},
		"outside": {SessionID: "outside"},
	})
	runtime.EventService.restore(RecoveryState{
		PendingApprovals: map[string]PendingApproval{
			"main":    {ThreadID: "session-0"},
			"fork":    {ThreadID: "session-0-fork"},
			"outside": {ThreadID: "outside"},
		},
		PendingInputs: map[string]PendingInput{
			"approval": {ThreadID: "session-0-fork"},
			"main":     {ThreadID: "session-1"},
			"fork":     {ThreadID: "session-1-fork"},
			"outside":  {ThreadID: "outside"},
		},
	})
	page, err := runtime.ListSessions(t.Context(), protocol.SessionListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		status            protocol.SessionLifecycleStatus
		approvals, inputs int
	}{
		{protocol.SessionStatusAwaitingApproval, 2, 1},
		{protocol.SessionStatusAwaitingInput, 0, 2},
		{protocol.SessionStatusRunning, 0, 0},
		{protocol.SessionStatusRunning, 0, 0},
		{protocol.SessionStatusCompleted, 0, 0},
	}
	for i, expected := range want {
		got := page.Sessions[i]
		if got.Status != expected.status || got.PendingApprovals != expected.approvals || got.PendingInputs != expected.inputs {
			t.Fatalf("session %d: %+v, want %+v", i, got, expected)
		}
	}
	for _, lease := range leases {
		if err := runtime.active.Release(lease); err != nil {
			t.Fatal(err)
		}
	}
	for _, operation := range []protocol.OperationID{"approval", "input", "pending", "outside"} {
		runtime.OperationService.commitLocal(operation)
	}
	runtime.EventService.forgetThreadInteractions([]protocol.ThreadID{
		"session-0", "session-0-fork", "session-1", "session-1-fork", "outside",
	})
	refreshed, err := runtime.ListSessions(t.Context(), protocol.SessionListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	for _, session := range refreshed.Sessions {
		if session.Status != protocol.SessionStatusCompleted || session.PendingApprovals != 0 || session.PendingInputs != 0 {
			t.Fatalf("stale activity: %+v", session)
		}
	}
	if page.Sessions[0].Status != protocol.SessionStatusAwaitingApproval || lifecycle.reads != 2 {
		t.Fatalf("snapshots were shared or thread ownership was cached: first=%+v reads=%d", page.Sessions[0], lifecycle.reads)
	}
}

func TestListSessionsDuringConcurrentActivityChanges(t *testing.T) {
	lifecycle := &batchLifecycleStore{summaries: activitySummaries(2)}
	runtime := NewRuntime(Options{SessionLifecycle: lifecycle})
	t.Cleanup(func() { closeRuntime(t, runtime) })
	ctx, cancel := context.WithCancel(t.Context())
	var writers sync.WaitGroup
	for _, change := range []func(){
		func() {
			runtime.OperationService.restore(map[protocol.OperationID]PendingOperation{"pending": {SessionID: "session-0"}})
			runtime.OperationService.commitLocal("pending")
		},
		func() {
			lease, err := runtime.active.Reserve("session-0-fork", "active", "active", "")
			if err != nil {
				t.Error(err)
				return
			}
			if err := runtime.active.Release(lease); err != nil {
				t.Error(err)
			}
		},
		func() {
			runtime.EventService.mu.Lock()
			runtime.approvals["approval"] = PendingApproval{ThreadID: "session-0-fork"}
			runtime.inputs["input"] = PendingInput{ThreadID: "session-0"}
			runtime.EventService.mu.Unlock()
			runtime.EventService.forgetThreadInteractions([]protocol.ThreadID{"session-0", "session-0-fork"})
		},
	} {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for ctx.Err() == nil {
				change()
			}
		}()
	}
	defer func() { cancel(); writers.Wait() }()
	for i := 0; i < 256; i++ {
		page, err := runtime.ListSessions(t.Context(), protocol.SessionListQuery{})
		if err != nil {
			t.Fatal(err)
		}
		busy, quiet := page.Sessions[0], page.Sessions[1]
		if busy.PendingApprovals != busy.PendingInputs || busy.PendingApprovals < 0 || busy.PendingApprovals > 1 {
			t.Fatalf("inconsistent interaction snapshot: %+v", busy)
		}
		if busy.PendingApprovals == 1 && busy.Status != protocol.SessionStatusAwaitingApproval {
			t.Fatalf("approval priority lost: %+v", busy)
		}
		if quiet.Status != protocol.SessionStatusCompleted || quiet.PendingApprovals != 0 || quiet.PendingInputs != 0 {
			t.Fatalf("activity crossed Session ownership: %+v", quiet)
		}
	}
}

func activitySummaries(size int) []protocol.SessionSummary {
	summaries := make([]protocol.SessionSummary, size)
	base := artifactLifecycle().summary
	for i := range summaries {
		id := fmt.Sprint("session-", i)
		summaries[i] = base
		summaries[i].SessionID, summaries[i].ThreadID = id, protocol.ThreadID(id)
		summaries[i].Status = protocol.SessionStatusCompleted
	}
	return summaries
}

func BenchmarkProjectSessionActivities(b *testing.B) {
	for _, size := range []int{1, 32, 256} {
		for _, owners := range []int{32, 1024} {
			b.Run(fmt.Sprintf("sessions=%d/owners=%d", size, owners), func(b *testing.B) {
				lifecycle := &batchLifecycleStore{summaries: activitySummaries(size)}
				runtime := NewRuntime(Options{SessionLifecycle: lifecycle})
				b.Cleanup(func() {
					if err := runtime.Close(context.Background()); err != nil {
						b.Error(err)
					}
				})
				for i := 0; i < owners; i++ {
					id := fmt.Sprint("session-", i)
					thread := protocol.ThreadID(id + "-fork")
					runtime.accepted[protocol.OperationID(id)] = PendingOperation{SessionID: id}
					runtime.approvals[id] = PendingApproval{ThreadID: thread}
					runtime.inputs[id] = PendingInput{ThreadID: thread}
					if _, err := runtime.active.Reserve(thread, protocol.TurnID(id), protocol.OperationID(id), ""); err != nil {
						b.Fatal(err)
					}
				}
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					if _, _, err := runtime.SessionService.projectSessionActivities(b.Context(), lifecycle.summaries); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
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

func (s *batchCheckpointStore) CheckpointSummaries(_ context.Context, ids []string) (map[string]snapshot.CheckpointSummary, error) {
	s.reads++
	result := make(map[string]snapshot.CheckpointSummary, len(ids))
	for i, id := range ids {
		result[id] = snapshot.CheckpointSummary{Count: i + 1, ChangedFiles: i + 2}
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
