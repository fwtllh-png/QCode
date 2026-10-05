package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestSessionLifecycleOverlaysLiveStateAndProtectsArchiveDelete(t *testing.T) {
	store := &memorySessionLifecycleStore{summary: protocol.SessionSummary{
		Version: protocol.SessionLifecycleVersion, Revision: 1,
		SessionID: "session-life", ThreadID: "thread-life",
		Title: "Lifecycle", Status: protocol.SessionStatusCompleted,
		Isolation: "shared", WorkspaceRoot: "/workspace",
		ExecutionTarget: "local",
		WorkspaceLabel:  "workspace",
		CreatedAt:       time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}}
	runtime := NewRuntime(Options{SessionLifecycle: store})
	t.Cleanup(func() { closeRuntime(t, runtime) })
	lease, err := runtime.active.Reserve(
		"thread-life",
		"turn-life",
		"operation-life",
		"item-life",
	)
	if err != nil {
		t.Fatal(err)
	}
	runtime.EventService.mu.Lock()
	runtime.approvals["approval-life"] = PendingApproval{
		RequestID: "approval-life", ThreadID: "thread-life", TurnID: "turn-life",
	}
	runtime.EventService.mu.Unlock()

	list, err := runtime.ListSessions(t.Context(), protocol.SessionListQuery{
		WorkspaceRoot: "/workspace",
		Status:        protocol.SessionStatusAwaitingApproval,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Sessions) != 1 ||
		list.Sessions[0].Status != protocol.SessionStatusAwaitingApproval ||
		list.Sessions[0].PendingApprovals != 1 {
		t.Fatalf("live lifecycle = %+v", list)
	}
	archived := true
	if _, err := runtime.UpdateSessionLifecycle(
		t.Context(),
		"session-life",
		1,
		protocol.SessionLifecyclePatch{Archived: &archived},
	); err == nil {
		t.Fatal("active session was archived")
	}
	if _, err := runtime.DeleteSession(
		t.Context(),
		"session-life",
		1,
	); err == nil {
		t.Fatal("active session was deleted")
	}

	if err := runtime.active.Release(lease); err != nil {
		t.Fatal(err)
	}
	runtime.EventService.mu.Lock()
	delete(runtime.approvals, "approval-life")
	runtime.EventService.mu.Unlock()
	updated, err := runtime.UpdateSessionLifecycle(
		t.Context(),
		"session-life",
		1,
		protocol.SessionLifecyclePatch{Archived: &archived},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !updated.Session.Archived || updated.Session.Revision != 2 {
		t.Fatalf("updated lifecycle = %+v", updated)
	}
	if _, err := runtime.DeleteSession(
		t.Context(),
		"session-life",
		2,
	); err != nil {
		t.Fatal(err)
	}
	if !store.deleted {
		t.Fatal("lifecycle store did not delete the session")
	}
}

func TestDiscardSessionRejectsLiveTurnAndClearsOrphanedApproval(t *testing.T) {
	store := &memorySessionLifecycleStore{summary: protocol.SessionSummary{
		Version: protocol.SessionLifecycleVersion, Revision: 1,
		SessionID: "session-discard", ThreadID: "thread-discard",
		Title: "Discard", Status: protocol.SessionStatusCompleted,
		Isolation: "shared", WorkspaceRoot: "/workspace",
		ExecutionTarget: "local", WorkspaceLabel: "workspace",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}}
	runtime := NewRuntime(Options{SessionLifecycle: store})
	t.Cleanup(func() { closeRuntime(t, runtime) })
	lease, err := runtime.active.Reserve(
		"thread-discard",
		"turn-discard",
		"operation-discard",
		"item-discard",
	)
	if err != nil {
		t.Fatal(err)
	}
	runtime.EventService.mu.Lock()
	runtime.approvals["approval-discard"] = PendingApproval{
		RequestID: "approval-discard", ThreadID: "thread-discard",
		TurnID: "turn-discard",
	}
	runtime.EventService.mu.Unlock()

	if _, err := runtime.DiscardSession(
		t.Context(),
		store.summary.SessionID,
		store.summary.Revision,
	); err == nil || !protocol.IsCode(err, protocol.CodeConflict) {
		t.Fatalf("live discard error = %v", err)
	}
	if store.deleted {
		t.Fatal("live session was discarded")
	}
	if err := runtime.active.Release(lease); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.DiscardSession(
		t.Context(),
		store.summary.SessionID,
		store.summary.Revision,
	); err != nil {
		t.Fatal(err)
	}
	if !store.deleted || !store.discarded {
		t.Fatal("orphaned session was not explicitly discarded")
	}
	if snapshot := runtime.Snapshot(t.Context()); snapshot.PendingApprovals != 0 {
		t.Fatalf("orphaned approvals were not cleared: %+v", snapshot)
	}
}

func TestSessionSearchProjectsPreciseTurnMatchAndSnippet(t *testing.T) {
	now := time.Now().UTC()
	store := &memorySessionLifecycleStore{searchMiss: true, summary: protocol.SessionSummary{
		Version: protocol.SessionLifecycleVersion, Revision: 1,
		SessionID: "session-search", ThreadID: "thread-search",
		LatestTurnID: "turn-search",
		Title:        "Parser", Status: protocol.SessionStatusCompleted,
		Isolation: "shared", WorkspaceRoot: "/workspace",
		WorkspaceLabel: "workspace", ExecutionTarget: "local",
		CreatedAt: now, UpdatedAt: now,
	}}
	events := NewMemoryEventStore(16)
	started, err := protocol.NewEvent(protocol.EventMeta{
		Sequence: 1, OperationID: "op-search", ThreadID: "thread-search",
		TurnID: "turn-search", ItemID: "item-search",
	}, &protocol.TurnStartedData{
		Provider: "fixture", Model: "fixture",
		DisplayPrompt: "Review the parser implementation",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := events.Append(t.Context(), started); err != nil {
		t.Fatal(err)
	}
	completed, err := protocol.NewEvent(protocol.EventMeta{
		Sequence: 2, OperationID: "op-search", ThreadID: "thread-search",
		TurnID: "turn-search", ItemID: "item-search",
	}, &protocol.TurnCompletedData{Text: "The parser handles Unicode safely."})
	if err != nil {
		t.Fatal(err)
	}
	if err := events.Append(t.Context(), completed); err != nil {
		t.Fatal(err)
	}
	runtime := NewRuntime(Options{
		EventStore: events, SessionLifecycle: store,
	})
	t.Cleanup(func() { closeRuntime(t, runtime) })
	list, err := runtime.ListSessions(t.Context(), protocol.SessionListQuery{
		Query: "Unicode",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Matches) != 1 ||
		list.Matches[0].Kind != "agent_output" ||
		list.Matches[0].TurnID != "turn-search" ||
		list.Matches[0].Snippet != "The parser handles Unicode safely." {
		t.Fatalf("search matches = %+v", list.Matches)
	}
}

func TestSessionHistoryPagesBackwardWithinSession(t *testing.T) {
	now := time.Now().UTC()
	store := &memorySessionLifecycleStore{summary: protocol.SessionSummary{
		Version: protocol.SessionLifecycleVersion, Revision: 1,
		SessionID: "session-history", ThreadID: "thread-history",
		Title: "History", Status: protocol.SessionStatusCompleted,
		Isolation: "shared", WorkspaceRoot: "/workspace",
		WorkspaceLabel: "workspace", ExecutionTarget: "local",
		CreatedAt: now, UpdatedAt: now,
	}}
	events := NewMemoryEventStore(16)
	for sequence := 1; sequence <= 7; sequence++ {
		threadID := protocol.ThreadID("thread-history")
		if sequence == 4 {
			threadID = "thread-foreign"
		}
		event, err := protocol.NewEvent(protocol.EventMeta{
			Sequence:    protocol.Cursor(sequence),
			OperationID: protocol.OperationID(fmt.Sprintf("op-%d", sequence)),
			ThreadID:    threadID,
			TurnID:      protocol.TurnID(fmt.Sprintf("turn-%d", sequence)),
			ItemID:      protocol.ItemID(fmt.Sprintf("item-%d", sequence)),
		}, &protocol.TurnCompletedData{Text: fmt.Sprintf("event %d", sequence)})
		if err != nil {
			t.Fatal(err)
		}
		if err := events.Append(t.Context(), event); err != nil {
			t.Fatal(err)
		}
	}
	runtime := NewRuntime(Options{EventStore: events, SessionLifecycle: store})
	t.Cleanup(func() { closeRuntime(t, runtime) })

	page, err := runtime.History(t.Context(), SessionHistoryQuery{
		SessionID: "session-history",
		Before:    7,
		Limit:     2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 2 ||
		page.Events[0].Sequence != 5 ||
		page.Events[1].Sequence != 6 ||
		page.Previous != 5 ||
		!page.MoreBefore {
		t.Fatalf("backward page = %+v", page)
	}
	if _, err := runtime.History(t.Context(), SessionHistoryQuery{
		SessionID: "session-history",
		Since:     1,
		Before:    7,
		Limit:     2,
	}); !protocol.IsCode(err, protocol.CodeInvalidArgument) {
		t.Fatalf("mixed cursor error = %v", err)
	}
}

func TestSessionControlCreatesActivatesAndSubmitsWithStableIdentity(t *testing.T) {
	store := &memorySessionLifecycleStore{}
	runtime := NewRuntime(Options{
		Engine:           NewThreadManager(nil),
		SessionLifecycle: store,
		DefaultProfile: protocol.SessionProfile{
			Provider: "fixture", Model: "fixture",
		},
	})
	t.Cleanup(func() { closeRuntime(t, runtime) })
	binding, err := runtime.CreateSession(t.Context(), CreateSessionRequest{
		SessionID: "session-web", WorkspaceRoot: "/workspace",
		Isolation: "shared", IdempotencyKey: "create-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if binding.SessionID != "session-web" ||
		binding.ThreadID == "" ||
		binding.Provider != "fixture" ||
		binding.Model != "fixture" {
		t.Fatalf("binding = %+v", binding)
	}
	replayed, err := runtime.CreateSession(t.Context(), CreateSessionRequest{
		SessionID: "session-web", WorkspaceRoot: "/workspace",
		Isolation: "shared", IdempotencyKey: "create-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if replayed != binding {
		t.Fatalf("idempotent create differs: first=%+v replay=%+v", binding, replayed)
	}
	snapshot, err := runtime.HistoryService.Snapshot(t.Context(), binding.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Events == nil || len(snapshot.Events) != 0 {
		t.Fatalf("empty presentation events = %#v, want non-nil empty slice", snapshot.Events)
	}
	if snapshot.SessionRevision != 1 {
		t.Fatalf("presentation revision = %d, want 1", snapshot.SessionRevision)
	}
	exported, err := runtime.HistoryService.Export(t.Context(), binding.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if exported.Session.SessionID != binding.SessionID ||
		exported.Session.Revision != exported.Snapshot.SessionRevision ||
		exported.Snapshot.Events == nil ||
		exported.Integrity.Algorithm != "sha256" ||
		len(exported.Integrity.Digest) != 64 {
		t.Fatalf("session export = %+v", exported)
	}
	activated, err := runtime.ActivateSession(
		t.Context(),
		ActivateSessionRequest{
			SessionID: "session-web", WorkspaceRoot: "/workspace",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if activated.ThreadID != binding.ThreadID {
		t.Fatalf("activated = %+v, want thread %s", activated, binding.ThreadID)
	}
	identity, err := protocol.NewWorkspaceIdentity(
		"file:///workspace",
		"/workspace",
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	submit := SubmitSessionOperation{
		SessionID: "session-web", Kind: protocol.OperationStartTurn,
		Payload:        &protocol.StartTurnPayload{Prompt: "hello"},
		IdempotencyKey: "request-1", WorkspaceIdentity: &identity,
	}
	first, err := runtime.SubmitForSession(t.Context(), submit)
	if err != nil {
		t.Fatal(err)
	}
	secondPayload := &protocol.StartTurnPayload{Prompt: "hello"}
	submit.Payload = secondPayload
	second, err := runtime.SubmitForSession(t.Context(), submit)
	if err != nil {
		t.Fatal(err)
	}
	if first.OperationID != second.OperationID ||
		first.TurnID != second.TurnID ||
		first.ItemID != second.ItemID {
		t.Fatalf("idempotent receipts differ: first=%+v second=%+v", first, second)
	}
	if store.summary.Title != "New Chat" || store.summary.Revision != 1 {
		t.Fatalf("prompt replaced the title without an LLM result: %+v", store.summary)
	}
	replayed, err = runtime.CreateSession(t.Context(), CreateSessionRequest{
		SessionID: "session-web", WorkspaceRoot: "/workspace",
		Isolation: "shared", IdempotencyKey: "create-1",
	})
	if err != nil || replayed != binding {
		t.Fatalf("automatic naming broke create replay: %+v %v", replayed, err)
	}
}

func TestSessionControlPreservesExplicitTitle(t *testing.T) {
	store := &memorySessionLifecycleStore{}
	runtime := NewRuntime(Options{
		Engine: NewThreadManager(nil), SessionLifecycle: store,
		DefaultProfile: protocol.SessionProfile{
			Provider: "fixture", Model: "fixture",
		},
	})
	t.Cleanup(func() { closeRuntime(t, runtime) })
	if _, err := runtime.CreateSession(t.Context(), CreateSessionRequest{
		SessionID: "session-titled", WorkspaceRoot: "/workspace",
		Title: "Pinned title", Isolation: "shared", IdempotencyKey: "create-titled",
	}); err != nil {
		t.Fatal(err)
	}
	identity, err := protocol.NewWorkspaceIdentity(
		"file:///workspace",
		"/workspace",
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.SubmitForSession(t.Context(), SubmitSessionOperation{
		SessionID: "session-titled", Kind: protocol.OperationStartTurn,
		Payload: &protocol.StartTurnPayload{
			Prompt: "this must not replace the title",
		},
		IdempotencyKey: "request-titled", WorkspaceIdentity: &identity,
	}); err != nil {
		t.Fatal(err)
	}
	if store.summary.Title != "Pinned title" || store.summary.Revision != 1 {
		t.Fatalf("explicit title summary = %+v", store.summary)
	}
}

func TestSessionControlRejectsForeignWorkspaceAndArchivedOperations(t *testing.T) {
	now := time.Now().UTC()
	store := &memorySessionLifecycleStore{summary: protocol.SessionSummary{
		Version: protocol.SessionLifecycleVersion, Revision: 1,
		SessionID: "session-foreign", ThreadID: "thread-foreign",
		Title: "Foreign", Status: protocol.SessionStatusIdle,
		Isolation: "shared", WorkspaceRoot: "/workspace/other",
		WorkspaceLabel: "other", ExecutionTarget: "local",
		CreatedAt: now, UpdatedAt: now,
	}}
	runtime := NewRuntime(Options{
		WorkspaceRoot:    "/workspace/current",
		SessionLifecycle: store,
	})
	t.Cleanup(func() { closeRuntime(t, runtime) })
	if _, err := runtime.SessionStatus(t.Context(), store.summary.SessionID); err == nil {
		t.Fatal("foreign workspace session was exposed")
	}

	store.summary.WorkspaceRoot = "/workspace/current"
	store.summary.Archived = true
	identity, err := protocol.NewWorkspaceIdentity(
		"file:///workspace/current",
		"/workspace/current",
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = runtime.SubmitForSession(t.Context(), SubmitSessionOperation{
		SessionID:         store.summary.SessionID,
		Kind:              protocol.OperationStartTurn,
		Payload:           &protocol.StartTurnPayload{Prompt: "must not run"},
		WorkspaceIdentity: &identity,
	})
	if err == nil {
		t.Fatal("archived session accepted a new operation")
	}
}

func TestHistoryRejectsForeignWorkspaceSession(t *testing.T) {
	store := &memorySessionLifecycleStore{summary: protocol.SessionSummary{
		Version: protocol.SessionLifecycleVersion, Revision: 1,
		SessionID: "session-foreign", ThreadID: "thread-foreign",
		Title: "Foreign", Status: protocol.SessionStatusIdle,
		Isolation: "shared", WorkspaceRoot: "/workspace/foreign",
		WorkspaceLabel: "foreign", ExecutionTarget: "local",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}}
	runtime := NewRuntime(Options{
		WorkspaceRoot:    "/workspace/current",
		SessionLifecycle: store,
	})
	t.Cleanup(func() { closeRuntime(t, runtime) })

	_, err := runtime.History(t.Context(), SessionHistoryQuery{
		SessionID: store.summary.SessionID,
		Limit:     100,
	})
	if err == nil {
		t.Fatal("foreign workspace history was exposed")
	}
}

func TestDeleteSessionProtectsAndDiscardsWorktree(t *testing.T) {
	store := &memorySessionLifecycleStore{summary: protocol.SessionSummary{
		Version: protocol.SessionLifecycleVersion, Revision: 1,
		SessionID: "session-worktree", ThreadID: "thread-worktree",
		Title: "Worktree", Status: protocol.SessionStatusCompleted,
		Isolation: "worktree", WorkspaceRoot: "/workspace",
		WorkspaceLabel: "workspace",
		CreatedAt:      time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}}
	workspaces := &memorySessionWorkspaces{
		plan: tool.EditPlan{
			ID: "plan", Files: []tool.EditPlanFile{{Path: "changed.go"}},
		},
	}
	runtime := NewRuntime(Options{
		SessionLifecycle:  store,
		SessionWorkspaces: workspaces,
	})
	t.Cleanup(func() { closeRuntime(t, runtime) })
	if _, err := runtime.DeleteSession(
		t.Context(),
		store.summary.SessionID,
		store.summary.Revision,
	); err == nil || !protocol.IsCode(err, protocol.CodeConflict) {
		t.Fatal("session with unmerged worktree changes was deleted")
	}
	if store.deleted || workspaces.discarded {
		t.Fatal("protected worktree deletion changed durable state")
	}
	workspaces.plan.Files = nil
	workspaces.planErr = errors.New("merge preview failed")
	if _, err := runtime.DeleteSession(
		t.Context(),
		store.summary.SessionID,
		store.summary.Revision,
	); err == nil ||
		!protocol.IsCode(err, protocol.CodeConflict) ||
		err.Error() != "cannot delete session while its isolated worktree has unresolved changes" {
		t.Fatalf("worktree validation error = %v", err)
	}
	if store.deleted || workspaces.discarded {
		t.Fatal("failed worktree validation changed durable state")
	}
	workspaces.plan.Files = nil
	workspaces.planErr = ErrSessionWorkspaceClean
	if _, err := runtime.DeleteSession(
		t.Context(),
		store.summary.SessionID,
		store.summary.Revision,
	); err != nil {
		t.Fatal(err)
	}
	if !store.deleted || !workspaces.discarded {
		t.Fatal("clean deleted worktree was not discarded")
	}
}

func TestDiscardSessionRevertsOwnedWorkspaceDraft(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "value.txt")
	if err := os.WriteFile(path, []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	journal := newTestWorkspaceJournal(t, root)
	if err := journal.Begin("turn-draft"); err != nil {
		t.Fatal(err)
	}
	if err := journal.Before(t.Context(), path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("draft\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := journal.After(path); err != nil {
		t.Fatal(err)
	}
	if err := journal.Suspend("turn-draft"); err != nil {
		t.Fatal(err)
	}
	events := NewMemoryEventStore(4)
	started, err := protocol.NewEvent(protocol.EventMeta{
		Sequence: 1, OperationID: "operation-draft",
		ThreadID: "thread-draft", TurnID: "turn-draft", ItemID: "item-draft",
	}, &protocol.TurnStartedData{Provider: "test", Model: "test", Prompt: "edit"})
	if err != nil {
		t.Fatal(err)
	}
	if err := events.Append(t.Context(), started); err != nil {
		t.Fatal(err)
	}
	canceled, err := protocol.NewEvent(protocol.EventMeta{
		Sequence: 2, OperationID: "operation-draft",
		ThreadID: "thread-draft", TurnID: "turn-draft", ItemID: "item-draft",
	}, &protocol.TurnCanceledData{Reason: "user_interrupted"})
	if err != nil {
		t.Fatal(err)
	}
	if err := events.Append(t.Context(), canceled); err != nil {
		t.Fatal(err)
	}
	threads := NewThreadManager(nil)
	threads.SetHostJournal(journal)
	store := &memorySessionLifecycleStore{summary: protocol.SessionSummary{
		Version: protocol.SessionLifecycleVersion, Revision: 2,
		SessionID: "session-draft", ThreadID: "thread-draft",
		Title: "Draft", Status: protocol.SessionStatusCompleted,
		Isolation: "shared", WorkspaceRoot: root,
		WorkspaceLabel: "workspace",
		CreatedAt:      time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}}
	runtime := NewRuntime(Options{
		Engine:           threads,
		EventStore:       events,
		SessionLifecycle: store,
	})
	t.Cleanup(func() { closeRuntime(t, runtime) })
	for _, discard := range []bool{false, true} {
		_, err := runtime.SessionService.deleteSession(
			t.Context(), store.summary.SessionID, 1, discard,
		)
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if store.deleted || !journal.HasDraft("turn-draft") || string(data) != "draft\n" {
			t.Fatalf("stale deletion (discard=%t) changed draft or session: deleted=%t, draft=%t, file=%q",
				discard, store.deleted, journal.HasDraft("turn-draft"), data)
		}
		var problem *protocol.Problem
		if !errors.As(err, &problem) || problem.Code != protocol.CodeConflict ||
			problem.Details == nil || problem.Details.ResourceID != store.summary.SessionID ||
			problem.Details.Reason != protocol.ProblemReasonStaleSessionRevision ||
			problem.Details.ExpectedRevision != 1 || problem.Details.ActualRevision != 2 {
			t.Fatalf("stale deletion (discard=%t) error = %+v", discard, err)
		}
	}
	if _, err := runtime.DiscardSession(
		t.Context(),
		store.summary.SessionID,
		store.summary.Revision,
	); err != nil {
		t.Fatal(err)
	}
	if journal.HasDraft("turn-draft") {
		t.Fatal("deleted session left a workspace draft")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "before\n" {
		t.Fatalf("reverted draft = %q", data)
	}
}

func TestDiscardSessionRejectsAcceptedOperationBeforeDispatch(t *testing.T) {
	for _, tc := range []struct {
		durable, submitForSession bool
	}{{false, true}, {false, false}, {true, true}, {true, false}} {
		t.Run(fmt.Sprintf("durable=%t/submit_for_session=%t", tc.durable, tc.submitForSession), func(t *testing.T) {
			store := &memorySessionLifecycleStore{summary: protocol.SessionSummary{
				Version: protocol.SessionLifecycleVersion, Revision: 1,
				SessionID: "session-pending", ThreadID: "thread-pending",
				Title: "Pending", Status: protocol.SessionStatusIdle,
				Isolation: "shared", WorkspaceRoot: "/workspace",
				ExecutionTarget: "local", WorkspaceLabel: "workspace",
				CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
			}, threadIDs: []protocol.ThreadID{"thread-pending", "thread-child"}}
			options := Options{SessionLifecycle: store}
			if tc.durable {
				options.Lifecycle = &faultyLifecycle{sessionID: store.summary.SessionID}
			}
			runtime, err := PrepareRuntime(t.Context(), options)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { closeRuntime(t, runtime) })
			// Admit the operation without dispatching it, so no active lease can
			// mask the accepted-operation guard.
			runtime.OperationService.open()
			if tc.submitForSession {
				identity, err := protocol.NewWorkspaceIdentity("file:///workspace", "/workspace", "")
				if err != nil {
					t.Fatal(err)
				}
				_, err = runtime.SubmitForSession(t.Context(), SubmitSessionOperation{
					SessionID: store.summary.SessionID, Kind: protocol.OperationStartTurn,
					Payload: &protocol.StartTurnPayload{Prompt: "queued"}, WorkspaceIdentity: &identity,
				})
				if err != nil {
					t.Fatal(err)
				}
			} else {
				operation, err := protocol.NewOperation(&protocol.StartTurnPayload{
					ThreadID: "thread-child", TurnID: "turn-child", ItemID: "item-child", Prompt: "queued",
				})
				if err != nil {
					t.Fatal(err)
				}
				if err := runtime.Submit(t.Context(), operation); err != nil {
					t.Fatal(err)
				}
			}
			if runtime.Snapshot(t.Context()).PendingOperations != 1 {
				t.Fatal("operation was not accepted")
			}
			if _, err := runtime.DiscardSession(t.Context(), store.summary.SessionID, store.summary.Revision); !protocol.IsCode(err, protocol.CodeConflict) || store.deleted {
				t.Fatalf("discard of accepted operation = %v, deleted=%t", err, store.deleted)
			}
		})
	}
}

func TestDiscardSessionDiscardsUnmergedWorktree(t *testing.T) {
	store := &memorySessionLifecycleStore{summary: protocol.SessionSummary{
		Version: protocol.SessionLifecycleVersion, Revision: 1,
		SessionID: "session-worktree-discard", ThreadID: "thread-worktree-discard",
		Title: "Worktree discard", Status: protocol.SessionStatusCompleted,
		Isolation: "worktree", WorkspaceRoot: "/workspace",
		WorkspaceLabel: "workspace",
		CreatedAt:      time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}}
	workspaces := &memorySessionWorkspaces{
		plan: tool.EditPlan{
			ID: "plan", Files: []tool.EditPlanFile{{Path: "changed.go"}},
		},
	}
	runtime := NewRuntime(Options{
		SessionLifecycle:  store,
		SessionWorkspaces: workspaces,
	})
	t.Cleanup(func() { closeRuntime(t, runtime) })

	if _, err := runtime.DiscardSession(
		t.Context(),
		store.summary.SessionID,
		store.summary.Revision,
	); err != nil {
		t.Fatal(err)
	}
	if !store.deleted || !store.discarded || !workspaces.discarded {
		t.Fatal("explicit discard did not remove the session and worktree")
	}
}

type memorySessionLifecycleStore struct {
	summary    protocol.SessionSummary
	threadIDs  []protocol.ThreadID
	deleted    bool
	discarded  bool
	searchMiss bool
}

func (s *memorySessionLifecycleStore) CreateLifecycle(
	_ context.Context,
	seed protocol.SessionCreateSeed,
) (protocol.SessionSummary, error) {
	if s.summary.SessionID != "" {
		return protocol.SessionSummary{}, errors.New("session already exists")
	}
	now := time.Now().UTC()
	s.summary = protocol.SessionSummary{
		Version: protocol.SessionLifecycleVersion, Revision: 1,
		SessionID: seed.SessionID, ThreadID: seed.ThreadID,
		Title: seed.Title, Status: protocol.SessionStatusIdle,
		TitleSource: seed.TitleSource, TitleRevision: 1,
		Isolation: seed.Isolation, WorkspaceRoot: seed.WorkspaceRoot,
		WorkspaceLabel: seed.WorkspaceLabel,
		Provider:       seed.Provider, Model: seed.Model,
		ExecutionTarget: "local", CreatedAt: now, UpdatedAt: now,
	}
	return s.summary, nil
}

type memorySessionWorkspaces struct {
	plan      tool.EditPlan
	planErr   error
	discarded bool
	restored  bool
}

func (m *memorySessionWorkspaces) Provision(
	context.Context,
	string,
	protocol.ThreadID,
) (SessionWorkspace, error) {
	return SessionWorkspace{}, nil
}

func (m *memorySessionWorkspaces) Restore(
	context.Context,
	string,
	protocol.ThreadID,
) (SessionWorkspace, error) {
	m.restored = true
	return SessionWorkspace{
		Mode: SessionIsolationWorktree,
		Root: "/workspace/.worktree",
	}, nil
}

func (m *memorySessionWorkspaces) Discard(
	context.Context,
	string,
	protocol.ThreadID,
) error {
	m.discarded = true
	return nil
}

func (m *memorySessionWorkspaces) PlanMerge(
	context.Context,
	string,
	protocol.ThreadID,
) (tool.EditPlan, error) {
	return m.plan, m.planErr
}

func (m *memorySessionWorkspaces) ApplyMerge(
	context.Context,
	string,
	protocol.ThreadID,
	string,
) (tool.EditPlan, error) {
	return tool.EditPlan{}, nil
}

func (s *memorySessionLifecycleStore) ListLifecycle(
	_ context.Context,
	query protocol.SessionListQuery,
) (protocol.SessionList, error) {
	if s.deleted {
		return protocol.SessionList{Version: protocol.SessionLifecycleVersion}, nil
	}
	if s.searchMiss && query.Query != "" {
		return protocol.SessionList{
			Version: protocol.SessionLifecycleVersion,
			Query:   query.Query,
		}, nil
	}
	return protocol.SessionList{
		Version:  protocol.SessionLifecycleVersion,
		Sessions: []protocol.SessionSummary{s.summary},
	}, nil
}

func (s *memorySessionLifecycleStore) GetLifecycle(
	_ context.Context,
	sessionID string,
) (protocol.SessionSummary, error) {
	if s.deleted || sessionID != s.summary.SessionID {
		return protocol.SessionSummary{}, errors.New("session not found")
	}
	return s.summary, nil
}

func (s *memorySessionLifecycleStore) ThreadIDs(
	_ context.Context,
	sessionID string,
) ([]protocol.ThreadID, error) {
	if sessionID != s.summary.SessionID {
		return nil, errors.New("session not found")
	}
	if len(s.threadIDs) != 0 {
		return append([]protocol.ThreadID(nil), s.threadIDs...), nil
	}
	return []protocol.ThreadID{s.summary.ThreadID}, nil
}

func (s *memorySessionLifecycleStore) PresentationReadFence(
	_ context.Context,
	sessionID string,
) (protocol.SessionReadFence, error) {
	if s.deleted || sessionID != s.summary.SessionID {
		return protocol.SessionReadFence{}, errors.New("session not found")
	}
	return protocol.SessionReadFence{
		Session:         s.summary,
		ThreadIDs:       []protocol.ThreadID{s.summary.ThreadID},
		ThroughSequence: s.summary.LatestSequence,
	}, nil
}

func (s *memorySessionLifecycleStore) SessionForThread(
	_ context.Context,
	threadID protocol.ThreadID,
) (string, error) {
	if len(s.threadIDs) != 0 {
		for _, candidate := range s.threadIDs {
			if candidate == threadID {
				return s.summary.SessionID, nil
			}
		}
	} else if threadID == s.summary.ThreadID {
		return s.summary.SessionID, nil
	}
	return "", errors.New("thread not found")
}

func (s *memorySessionLifecycleStore) ActivateThread(
	_ context.Context,
	sessionID string,
	threadID protocol.ThreadID,
) (protocol.SessionSummary, error) {
	if sessionID != s.summary.SessionID {
		return protocol.SessionSummary{}, errors.New("session not found")
	}
	if threadID == s.summary.ThreadID {
		return s.summary, nil
	}
	s.summary.ParentThreadID = s.summary.ThreadID
	s.summary.ThreadID = threadID
	s.summary.Revision++
	return s.summary, nil
}

func (s *memorySessionLifecycleStore) UpdateLifecycle(
	_ context.Context,
	sessionID string,
	expectedRevision uint64,
	patch protocol.SessionLifecyclePatch,
) (protocol.SessionSummary, error) {
	if sessionID != s.summary.SessionID ||
		expectedRevision != s.summary.Revision {
		return protocol.SessionSummary{}, errors.New("lifecycle conflict")
	}
	if patch.Archived != nil {
		s.summary.Archived = *patch.Archived
	}
	if patch.Title != nil {
		s.summary.Title = *patch.Title
		s.summary.TitleSource = protocol.SessionTitleManual
		s.summary.TitleRevision++
	}
	s.summary.Revision++
	return s.summary, nil
}

func (s *memorySessionLifecycleStore) UpdateGeneratedTitle(
	_ context.Context, sessionID string, threadID protocol.ThreadID,
	expected uint64, source protocol.SessionTitleSource, title string,
) (protocol.SessionSummary, bool, error) {
	previous := protocol.SessionTitleDefault
	if source == protocol.SessionTitleAuto {
		previous = protocol.SessionTitleTemporary
	}
	if s.deleted || s.summary.SessionID != sessionID {
		return protocol.SessionSummary{}, false, errors.New("session not found")
	}
	if s.summary.ThreadID != threadID || s.summary.TitleSource != previous ||
		s.summary.TitleRevision != expected || s.summary.Archived {
		return s.summary, false, nil
	}
	s.summary.Title, s.summary.TitleSource = title, source
	s.summary.TitleRevision++
	s.summary.Revision++
	return s.summary, true, nil
}

func (s *memorySessionLifecycleStore) DeleteLifecycle(
	_ context.Context,
	sessionID string,
	expectedRevision uint64,
) (protocol.SessionDeleteResult, error) {
	if sessionID != s.summary.SessionID ||
		expectedRevision != s.summary.Revision {
		return protocol.SessionDeleteResult{}, errors.New("lifecycle conflict")
	}
	s.deleted = true
	return protocol.SessionDeleteResult{
		Version:   protocol.SessionLifecycleVersion,
		SessionID: sessionID, ThreadID: s.summary.ThreadID,
		DeletedAt: time.Now().UTC(),
	}, nil
}

func (s *memorySessionLifecycleStore) DiscardLifecycle(
	ctx context.Context,
	sessionID string,
	expectedRevision uint64,
) (protocol.SessionDeleteResult, error) {
	s.discarded = true
	return s.DeleteLifecycle(ctx, sessionID, expectedRevision)
}
