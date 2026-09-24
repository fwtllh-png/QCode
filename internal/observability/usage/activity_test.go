package usage

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestActivityCountsDurableExecutionWithoutUsageOrPagination(t *testing.T) {
	r := testRepository(t)
	addTurn(t, r, "turn-2", 1)
	addTurn(t, r, "turn-3", 2)
	addChildUsageTurn(t, r)
	type indexed struct {
		thread, turn, item string
		kind               protocol.EventKind
	}
	events := []indexed{
		{"thread-1", "turn-1", "turn", protocol.EventTurnStarted},
		{"thread-1", "turn-1", "turn", protocol.EventTurnCompleted},
		{"thread-1", "turn-2", "turn", protocol.EventTurnStarted},
		{"thread-1", "turn-2", "turn", protocol.EventTurnFailed},
		{"thread-1", "turn-3", "turn", protocol.EventTurnStarted},
		{"thread-1", "turn-3", "turn", protocol.EventTurnCanceled},
		{"thread-1", "turn-1", "tool-1", protocol.EventToolResult},
		{"thread-1", "turn-1", "tool-1", protocol.EventToolResult}, // repeated delivery
		{"thread-1", "turn-2", "tool-1", protocol.EventToolResult}, // different turn
		{"thread-1", "turn-3", "unfinished", protocol.EventToolStart},
		{"thread-child", "turn-child", "turn", protocol.EventTurnStarted},
		{"thread-child", "turn-child", "turn", protocol.EventTurnCompleted},
		{"thread-child", "turn-child", "tool-1", protocol.EventToolResult},
	}
	for i, event := range events {
		_, err := r.db.ExecContext(t.Context(), `
			INSERT INTO event_index(sequence, event_id, thread_id, turn_id, item_id,
				kind, log_offset, log_length, sha256, created_at)
			VALUES (?, ?, ?, ?, ?, ?, 0, 1, ?, ?)`,
			i+1, fmt.Sprintf("event-%d", i), event.thread, event.turn, event.item,
			event.kind, strings.Repeat("a", 64), time.Now().UTC().Format(time.RFC3339Nano))
		if err != nil {
			t.Fatal(err)
		}
	}
	// A follow-up changes the graph's latest turn; earlier child work still belongs.
	if _, err := r.db.ExecContext(t.Context(),
		`UPDATE agent_nodes SET turn_id = 'child-followup'`); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		filter Query
		want   *Activity
	}{
		{"direct", Query{SessionID: "session-1", Limit: 1},
			&Activity{Turns: 3, Completed: 1, Failed: 1, Canceled: 1, ToolCalls: 2}},
		{"children", Query{SessionID: "session-1", IncludeChildren: true, Limit: 1},
			&Activity{Turns: 4, Completed: 2, Failed: 1, Canceled: 1, ToolCalls: 3}},
		{"thread", Query{SessionID: "session-1", IncludeChildren: true, ThreadID: "thread-1"},
			&Activity{Turns: 3, Completed: 1, Failed: 1, Canceled: 1, ToolCalls: 2}},
		{"turn", Query{TurnID: "turn-2"}, &Activity{Turns: 1, Failed: 1, ToolCalls: 1}},
		{"other workspace", Query{WorkspaceRoot: "/other"}, &Activity{}},
		{"other session", Query{SessionID: "other", IncludeChildren: true}, &Activity{}},
		{"billing model filter", Query{Model: "model"}, nil},
		{"billing time filter", Query{Start: time.Now().Add(-time.Hour)}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := r.QueryRollup(t.Context(), tc.filter)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(result.Activity, tc.want) {
				t.Fatalf("activity = %+v; want %+v", result.Activity, tc.want)
			}
			if result.Turns != 0 || result.Calls != 0 {
				t.Fatalf("execution events must not create billing: %+v", result)
			}
		})
	}
}

func TestInclusiveUsageKeepsEarlierChildTurnsAfterFollowup(t *testing.T) {
	r := testRepository(t)
	addChildUsageTurn(t, r)
	for i, data := range []protocol.EventData{
		&protocol.TurnStartedData{Provider: "provider", Model: "model"},
		&protocol.UsageData{Sample: 1, InputTokens: 20},
	} {
		event := testEventForTurn(t, protocol.Cursor(i+1), "turn-child", data)
		event.ThreadID = "thread-child"
		if err := r.Project(t.Context(), event); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.db.ExecContext(t.Context(),
		`UPDATE agent_nodes SET turn_id = 'child-followup'`); err != nil {
		t.Fatal(err)
	}
	result, err := r.QueryRollup(t.Context(), Query{SessionID: "session-1", IncludeChildren: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.InputTokens != 20 || result.Turns != 1 {
		t.Fatalf("earlier child usage lost after followup: %+v", result)
	}
}
