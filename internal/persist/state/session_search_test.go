package state

import (
	"context"
	"testing"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestSessionSearchProjectionRecoversAndSearchesWithoutEventLog(t *testing.T) {
	for _, damage := range []string{
		"DROP TABLE session_event_search",
		"DELETE FROM session_event_search",
		"UPDATE session_event_search SET fields_json = '[]', folded_text = 'wrong'",
	} {
		t.Run(damage, func(t *testing.T) {
			ctx := t.Context()
			root := t.TempDir()
			store, err := Open(ctx, Options{DataDir: root})
			if err != nil {
				t.Fatal(err)
			}
			seedThread(t, store, "thread_test", "Search", "open")
			for index, data := range []protocol.EventData{
				&protocol.TurnStartedData{Provider: "fixture", Model: "fixture", Prompt: "hidden", DisplayPrompt: "中文 ÉCLAIR old"},
				&protocol.TurnCompletedData{Text: "中文 ÉCLAIR newest a%b_c.go"},
				&protocol.TurnCompletedData{Text: "unrelated latest event"},
			} {
				event := testEventWithData(t, protocol.Cursor(index+1), data)
				event.TurnID = protocol.TurnID("turn-" + string(rune('1'+index)))
				if err := store.Append(ctx, event); err != nil {
					t.Fatal(err)
				}
			}
			assertSearch := func(store *Store) {
				t.Helper()
				threads := map[protocol.ThreadID]string{"thread_test": "session_1"}
				for _, query := range []string{"中", "éclair", "%b_"} {
					matches, err := store.SearchSessionEvents(ctx, threads, query)
					if err != nil || len(matches) != 1 {
						t.Fatalf("query %q = %+v, %v", query, matches, err)
					}
					if matches[0].TurnID != "turn-2" || matches[0].Kind != "agent_output" ||
						matches[0].Snippet != "中文 ÉCLAIR newest a%b_c.go" {
						t.Fatalf("query %q = %+v", query, matches)
					}
				}
				for _, query := range []string{"hidden", "missing", "axb"} {
					matches, err := store.SearchSessionEvents(ctx, threads, query)
					if err != nil || len(matches) != 0 {
						t.Fatalf("unexpected query %q = %+v, %v", query, matches, err)
					}
				}
				matches, err := store.SearchSessionEvents(ctx,
					map[protocol.ThreadID]string{"thread_test": "foreign-session"}, "中文")
				if err != nil || len(matches) != 0 {
					t.Fatalf("forged ownership = %+v, %v", matches, err)
				}
			}
			assertSearch(store)
			if _, err := store.SQLite().DB().ExecContext(ctx, damage); err != nil {
				t.Fatal(err)
			}
			if err := store.CloseAll(ctx); err != nil {
				t.Fatal(err)
			}
			store, err = Open(ctx, Options{DataDir: root})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.CloseAll(context.Background()) })
			// A valid search must use only the relational projection.
			if err := store.events.Close(ctx); err != nil {
				t.Fatal(err)
			}
			assertSearch(store)
			if _, err := store.SQLite().DB().ExecContext(ctx, "DELETE FROM sessions WHERE id = 'session_1'"); err != nil {
				t.Fatal(err)
			}
			matches, err := store.SearchSessionEvents(ctx,
				map[protocol.ThreadID]string{"thread_test": "session_1"}, "中文")
			if err != nil || len(matches) != 0 {
				t.Fatalf("deleted session = %+v, %v", matches, err)
			}
		})
	}
}

func TestSessionSearchHealthyRecoveryDoesNotRewriteProjection(t *testing.T) {
	root := t.TempDir()
	store, err := Open(t.Context(), Options{DataDir: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(t.Context(), testEvent(t, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQLite().DB().ExecContext(t.Context(), `
		CREATE TRIGGER search_rewrite_guard BEFORE UPDATE ON session_event_search
		BEGIN SELECT RAISE(ABORT, 'healthy search projection rewritten'); END`); err != nil {
		t.Fatal(err)
	}
	if err := store.CloseAll(t.Context()); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(t.Context(), Options{DataDir: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.CloseAll(context.Background()) })
}
