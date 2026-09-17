package history

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestEventBelongsToSessionUsesDeclaredAgentOwnership(t *testing.T) {
	threads := map[protocol.ThreadID]struct{}{"thread-main": {}}
	agent := protocol.Event{
		ThreadID: "thread_external",
		Data: &protocol.AgentStatusData{
			AgentID: "agent-1", WorkspaceRoot: "/workspace",
			SessionID: "session-a", Status: "running",
		},
	}
	if !eventBelongsToSession(agent, "session-a", threads) {
		t.Fatal("session-owned agent event was excluded")
	}
	if eventBelongsToSession(agent, "session-b", threads) {
		t.Fatal("foreign agent event was included")
	}
	if !eventBelongsToSession(protocol.Event{ThreadID: "thread-main"}, "session-a", threads) {
		t.Fatal("session thread event was excluded")
	}
}

type tailReader struct {
	events []protocol.Event
	calls  int
}

type indexedRuntime struct {
	Runtime // Unexpected forward replay fails the test through this nil port.
	reader  *tailReader
	fence   protocol.SessionReadFence
}

func (r indexedRuntime) HistoryEventReader() BackwardReader { return r.reader }
func (r indexedRuntime) HistoryWorkspaceRoot() string       { return "/workspace" }
func (r indexedRuntime) SessionStatus(_ context.Context, sessionID string) (protocol.SessionSummary, error) {
	if sessionID != r.fence.Session.SessionID {
		return protocol.SessionSummary{}, problem(protocol.CodeInvalidArgument, "foreign session")
	}
	return r.fence.Session, nil
}
func (r indexedRuntime) HistoryThreadIDs(context.Context, string) ([]protocol.ThreadID, error) {
	return r.fence.ThreadIDs, nil
}
func (r indexedRuntime) HistoryReadFence(context.Context, string) (protocol.SessionReadFence, error) {
	return r.fence, nil
}

func TestIndexedHistoryKeepsAuthorizationFenceAndPageOrder(t *testing.T) {
	reader := &tailReader{}
	for sequence := protocol.Cursor(5); sequence > 0; sequence-- {
		reader.events = append(reader.events, protocol.Event{
			Version: protocol.Version, ID: protocol.EventID(fmt.Sprint(sequence)),
			Sequence: sequence, ThreadID: "thread", TurnID: "turn",
			OperationID: "operation", ItemID: "item", CreatedAt: time.Now().UTC(),
			Kind: protocol.EventTurnCompleted, Data: &protocol.TurnCompletedData{Text: "done"},
		})
	}
	service := NewService(indexedRuntime{reader: reader, fence: protocol.SessionReadFence{
		Session:   protocol.SessionSummary{SessionID: "session", ThreadID: "thread", Revision: 7},
		ThreadIDs: []protocol.ThreadID{"thread"}, ThroughSequence: 4,
	}})
	page, err := service.History(t.Context(), SessionHistoryQuery{SessionID: "session", Before: 5, Limit: 2})
	if err != nil || !page.MoreBefore || page.Previous != 3 || page.Next != 4 ||
		len(page.Events) != 2 || page.Events[0].Sequence != 3 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	snapshot, err := service.Snapshot(t.Context(), "session")
	if err != nil || len(snapshot.Events) != 4 || snapshot.ThroughSequence != 4 || snapshot.SessionRevision != 7 {
		t.Fatalf("snapshot=%+v err=%v", snapshot, err)
	}
	calls := reader.calls
	if _, err := service.Snapshot(t.Context(), "foreign"); !protocol.IsCode(err, protocol.CodeInvalidArgument) {
		t.Fatalf("foreign snapshot error=%v", err)
	}
	if _, err := service.History(t.Context(), SessionHistoryQuery{SessionID: "foreign", Before: 5, Limit: 2}); !protocol.IsCode(err, protocol.CodeInvalidArgument) {
		t.Fatalf("foreign history error=%v", err)
	}
	if reader.calls != calls {
		t.Fatal("unauthorized query read the log")
	}
}

func (r *tailReader) ReplaySessionBefore(_ context.Context, _ string, _ []protocol.ThreadID, before, through protocol.Cursor, limit int) ([]protocol.Event, bool, error) {
	r.calls++
	var result []protocol.Event
	for _, event := range r.events {
		if event.Sequence <= through && (before == 0 || event.Sequence < before) {
			result = append(result, event)
			if len(result) > limit {
				return result[:limit], true, nil
			}
		}
	}
	return result, false, nil
}

func TestSnapshotTailMatchesForwardBudgetAndFence(t *testing.T) {
	for _, size := range []int{1, 3 << 20, 9 << 20} {
		t.Run(fmt.Sprintf("bytes_%d", size), func(t *testing.T) {
			var forward []protocol.Event
			for sequence := protocol.Cursor(1); sequence <= 6; sequence++ {
				forward = append(forward, protocol.Event{
					Version: protocol.Version, ID: protocol.EventID(strings.Repeat("e", int(sequence))),
					Sequence: sequence, ThreadID: "thread", TurnID: "turn",
					OperationID: "operation", ItemID: "item", CreatedAt: time.Now().UTC(),
					Kind: protocol.EventTurnCompleted,
					Data: &protocol.TurnCompletedData{Text: strings.Repeat("x", size)},
				})
			}
			// Reproduce the existing forward budget contract through sequence 5.
			var want []protocol.Event
			var sizes []int
			total := 0
			truncated := protocol.Cursor(0)
			for _, event := range forward[:5] {
				body, err := json.Marshal(event)
				if err != nil {
					t.Fatal(err)
				}
				want = append(want, event)
				sizes = append(sizes, len(body))
				total += len(body)
				for total > maxPresentationSnapshotBytes && len(want) > 1 {
					truncated = want[0].Sequence
					total -= sizes[0]
					want, sizes = want[1:], sizes[1:]
				}
			}
			slices.Reverse(forward)
			reader := &tailReader{events: forward}
			got, cutoff, err := snapshotTail(t.Context(), reader, "session",
				protocol.SessionReadFence{ThroughSequence: 5, ThreadIDs: []protocol.ThreadID{"thread"}})
			if err != nil || cutoff != truncated || !reflect.DeepEqual(got, want) {
				t.Fatalf("tail count=%d cutoff=%d want count=%d cutoff=%d err=%v", len(got), cutoff, len(want), truncated, err)
			}
			if reader.calls != 1 {
				t.Fatalf("read calls=%d", reader.calls)
			}
		})
	}
}
