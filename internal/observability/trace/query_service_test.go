package trace

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestProjectTraceTurnUsesPublicKindsAndAllowlistedIdentity(t *testing.T) {
	started := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	projected := projectTraceTurn("turn-1", []Record{
		{
			ID: 1, Name: NameTurn, Started: started,
			Ended: started.Add(time.Second), Status: StatusOK,
			Attributes: map[string]any{"prompt": "secret prompt"},
		},
		{
			ID: 2, ParentID: 1, Name: NameTool, Started: started,
			Ended: started.Add(250 * time.Millisecond), Status: StatusOK,
			Attributes: map[string]any{
				"call_id": "call-1",
				"tool":    "file_read",
				"output":  "secret output",
			},
		},
		{
			ID: 3, ParentID: 1, Name: NameTurnKernelTransition,
			Started: started, Ended: started, Status: StatusOK,
		},
	})

	if projected.Status != "ok" || len(projected.Spans) != 2 {
		t.Fatalf("projected trace = %+v", projected)
	}
	if projected.Spans[1].Kind != "tool" ||
		projected.Spans[1].CallID != "call-1" ||
		projected.Spans[1].DurationMS == nil ||
		*projected.Spans[1].DurationMS != 250 {
		t.Fatalf("tool span = %+v", projected.Spans[1])
	}
	encoded, err := json.Marshal(projected)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "secret") {
		t.Fatalf("trace query leaked non-allowlisted attributes: %s", encoded)
	}
}

func TestTraceServiceValidatesSessionOwnershipAndDeduplicatesTurns(t *testing.T) {
	store := &queryStore{records: map[protocol.TurnID][]Record{
		"turn-1": {{
			ID: 1, Name: NameTurn,
			Started: time.Now().UTC(), Status: StatusOpen,
		}},
	}}
	service := NewQueryService(
		sessionReader{sessionID: "session-1"},
		store,
		Runtime{},
	)
	result, err := service.Query(t.Context(), TraceQuery{
		SessionID: "session-1",
		TurnIDs:   []protocol.TurnID{"turn-1", "turn-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Turns) != 1 || store.calls != 1 {
		t.Fatalf("result=%+v calls=%d", result, store.calls)
	}
	if _, err := service.Query(t.Context(), TraceQuery{
		SessionID:       "session-1",
		TurnIDs:         []protocol.TurnID{"turn-1"},
		ThroughSequence: 11,
	}); err == nil {
		t.Fatal("future trace watermark was accepted")
	}

	store.foreign = true
	if _, err := service.Query(t.Context(), TraceQuery{
		SessionID: "session-1",
		TurnIDs:   []protocol.TurnID{"turn-foreign"},
	}); err == nil {
		t.Fatal("foreign turn was accepted")
	}
}

func TestUnsigned32RejectsOverflowAndFractions(t *testing.T) {
	for _, value := range []any{-1, 1.5, uint64(1) << 40, "1"} {
		if _, ok := unsigned32(value); ok {
			t.Fatalf("unsigned32(%v) accepted invalid value", value)
		}
	}
	if value, ok := unsigned32(uint32(7)); !ok || value != 7 {
		t.Fatalf("unsigned32 valid = %d, %t", value, ok)
	}
}

func TestTraceCompleteRequiresDurableEndedSpansAndReleasedRecorder(t *testing.T) {
	active := NewRuntime(nil)
	recorder := active.NewTurnRecorder(t.Context(), "session-1", "turn-1")
	recorder.Start(NameTurn, 0, nil)
	recorder.FreezeTerminal(StatusOK)
	store := &queryStore{records: map[protocol.TurnID][]Record{"turn-1": recorder.Spans()}}
	service := NewQueryService(sessionReader{sessionID: "session-1"}, store, active)
	query := TraceQuery{SessionID: "session-1", TurnIDs: []protocol.TurnID{"turn-1"}}
	result, err := service.Query(t.Context(), query)
	if err != nil || result.Turns[0].Complete {
		t.Fatalf("active frozen trace marked complete: %+v %v", result, err)
	}
	recorder.Close()
	result, err = service.Query(t.Context(), query)
	if err != nil || !result.Turns[0].Complete {
		t.Fatalf("durable closed trace not complete: %+v %v", result, err)
	}
	store.records["turn-1"] = nil
	result, err = service.Query(t.Context(), query)
	if err != nil || result.Turns[0].Complete {
		t.Fatalf("unavailable trace marked complete: %+v %v", result, err)
	}
}

type sessionReader struct {
	sessionID     string
	workspaceRoot string
}

func (s sessionReader) GetLifecycle(
	_ context.Context,
	sessionID string,
) (protocol.SessionSummary, error) {
	if sessionID != s.sessionID {
		return protocol.SessionSummary{}, errors.New("session not found")
	}
	return protocol.SessionSummary{
		SessionID: sessionID, WorkspaceRoot: s.workspaceRoot,
		LatestSequence: 10,
	}, nil
}

type queryStore struct {
	records map[protocol.TurnID][]Record
	calls   int
	foreign bool
}

func (s *queryStore) QueryTurnInSession(
	_ context.Context,
	_ string,
	turnID protocol.TurnID,
) ([]Record, error) {
	s.calls++
	if s.foreign {
		return nil, ErrNotFound
	}
	records, ok := s.records[turnID]
	if !ok {
		return nil, errors.New("trace unavailable")
	}
	return records, nil
}
