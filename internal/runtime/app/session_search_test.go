package app

import (
	"context"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type indexedSearchEventStore struct {
	*MemoryEventStore
	replays int
	threads map[protocol.ThreadID]string
	query   string
}

func (s *indexedSearchEventStore) Replay(ctx context.Context, cursor protocol.Cursor) ([]protocol.Event, error) {
	s.replays++
	return s.MemoryEventStore.Replay(ctx, cursor)
}

func (s *indexedSearchEventStore) SearchSessionEvents(_ context.Context, threads map[protocol.ThreadID]string, query string) ([]protocol.SessionSearchMatch, error) {
	s.threads, s.query = threads, query
	return []protocol.SessionSearchMatch{{
		SessionID: "session-search", TurnID: "turn-search",
		Kind: "agent_output", Snippet: "中文搜索",
	}}, nil
}

func TestSessionSearchUsesIndexedEventStore(t *testing.T) {
	now := time.Now().UTC()
	store := &memorySessionLifecycleStore{searchMiss: true, summary: protocol.SessionSummary{
		Version: protocol.SessionLifecycleVersion, Revision: 1,
		SessionID: "session-search", ThreadID: "thread-search",
		Title: "Parser", Status: protocol.SessionStatusCompleted,
		Isolation: "shared", WorkspaceRoot: "/workspace",
		WorkspaceLabel: "workspace", ExecutionTarget: "local",
		CreatedAt: now, UpdatedAt: now,
	}}
	events := &indexedSearchEventStore{MemoryEventStore: NewMemoryEventStore(16)}
	runtime := NewRuntime(Options{EventStore: events, SessionLifecycle: store})
	t.Cleanup(func() { closeRuntime(t, runtime) })
	events.replays = 0
	list, err := runtime.ListSessions(t.Context(), protocol.SessionListQuery{Query: " 中文 "})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Sessions) != 1 || len(list.Matches) != 1 ||
		list.Matches[0].Snippet != "中文搜索" || events.replays != 0 ||
		events.query != "中文" || events.threads["thread-search"] != "session-search" {
		t.Fatalf("list=%+v replays=%d threads=%+v query=%q", list, events.replays, events.threads, events.query)
	}
}
