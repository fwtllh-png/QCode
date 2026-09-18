package session_test

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/persist/session"
	sqlitestate "github.com/fwtllh-png/QCode/internal/persist/state/sqlite"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestLifecyclePageMatchesIndividualSummaries(t *testing.T) {
	store, err := sqlitestate.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	repository := session.NewSQLiteRepository(store)
	now := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.DB().ExecContext(t.Context(), query, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"a", "b", "idle", "outside"} {
		root := "/workspace"
		if id == "outside" {
			root = "/outside"
		}
		_, err := repository.CreateLifecycle(t.Context(), protocol.SessionCreateSeed{
			Version: protocol.SessionLifecycleVersion, SessionID: id, ThreadID: protocol.ThreadID(id),
			WorkspaceID: root, WorkspaceRoot: root, WorkspaceLabel: "fixture",
			Title: id, Provider: "fixture", Model: "model", Isolation: "shared",
		})
		if err != nil {
			t.Fatal(err)
		}
		if id == "idle" {
			continue
		}
		status := "completed"
		if id == "b" {
			status = "blocked"
		}
		for i, thread := range []string{id, id + "-fork"} {
			if i > 0 {
				exec(`INSERT INTO threads(id, session_id, parent_thread_id, title, status, created_at, updated_at)
					VALUES (?, ?, ?, 'Fork', 'open', ?, ?)`, thread, id, id, now, now)
			}
			exec(`INSERT INTO turns(id, thread_id, ordinal, status, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?)`, thread, thread, i+1, status, now, now)
			exec(`INSERT INTO items(id, turn_id, ordinal, kind, payload_json, created_at, updated_at)
				VALUES (?, ?, 1, 'message', '{"text":"needle"}', ?, ?)`, thread, thread, now, now)
			known := id != "b" || i == 0
			exec(`INSERT INTO usage(session_id, thread_id, turn_id, provider, model,
				input_tokens, output_tokens, reasoning_tokens, cost_microunits, cost_known, created_at)
				VALUES (?, ?, ?, 'fixture', 'model', 10, 2, 3, 50, ?, ?)`, id, thread, thread, known, now)
		}
		if _, err := repository.ActivateThread(t.Context(), id, protocol.ThreadID(id+"-fork")); err != nil {
			t.Fatal(err)
		}
	}
	for i, thread := range []string{"a", "a-fork", "b", "idle", "outside"} {
		exec(`INSERT INTO event_index(sequence, event_id, thread_id, kind, log_offset, log_length, sha256, created_at)
			VALUES (?, ?, ?, 'turn.completed', 0, 1, ?, ?)`, i+1, fmt.Sprint(i), thread, strings.Repeat("a", 64), now)
	}
	pinned := true
	if _, err := repository.UpdateLifecycle(t.Context(), "b", 2, protocol.SessionLifecyclePatch{Pinned: &pinned}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		query protocol.SessionListQuery
		ids   []string
	}{
		{protocol.SessionListQuery{WorkspaceRoot: "/workspace", Limit: 2}, []string{"b", "a"}},
		{protocol.SessionListQuery{WorkspaceRoot: "/workspace", Query: "NEEDLE"}, []string{"b", "a"}},
		{protocol.SessionListQuery{WorkspaceRoot: "/workspace", Status: protocol.SessionStatusIdle, Limit: 1}, []string{"idle"}},
		{protocol.SessionListQuery{WorkspaceRoot: "/workspace", PinnedOnly: true}, []string{"b"}},
	} {
		page, err := repository.ListLifecycle(t.Context(), tc.query)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, got := range page.Sessions {
			ids = append(ids, got.SessionID)
			want, err := repository.GetLifecycle(t.Context(), got.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("batch %+v != individual %+v", got, want)
			}
			if got.SessionID == "a" && (got.TotalTokens != 30 || got.CostMicrounits != 100 || !got.CostKnown || got.LatestSequence != 2) {
				t.Fatalf("usage/watermark multiplied or omitted: %+v", got)
			}
			if got.SessionID == "b" && (got.TotalTokens != 30 || got.CostMicrounits != 50 || got.CostKnown) {
				t.Fatalf("mixed pricing: %+v", got)
			}
		}
		if !reflect.DeepEqual(ids, tc.ids) {
			t.Fatalf("query %+v: ids %v, want %v", tc.query, ids, tc.ids)
		}
		if tc.query.Query != "" {
			for i, match := range page.Matches {
				if match.SessionID != tc.ids[i] || string(match.TurnID) != tc.ids[i]+"-fork" {
					t.Fatalf("match order/latest turn: %+v", page.Matches)
				}
			}
			if len(page.Matches) != len(tc.ids) {
				t.Fatalf("missing matches: %+v", page)
			}
		}
	}
	threads, err := repository.ThreadIDsForSessions(t.Context(), []string{"a", "idle", "missing"})
	if err != nil || !reflect.DeepEqual(threads["a"], []protocol.ThreadID{"a", "a-fork"}) ||
		len(threads["idle"]) != 1 || len(threads["outside"]) != 0 || len(threads["missing"]) != 0 {
		t.Fatalf("page thread ownership: %v, %v", threads, err)
	}
}
