package wire

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	agenttool "github.com/fwtllh-png/QCode/internal/adapter/tool/agent"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/handle"
	"github.com/fwtllh-png/QCode/internal/orchestration/subagent"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	"github.com/fwtllh-png/QCode/internal/testutil/tooltest"
)

func TestCloseAgentSettlesAndReleasesRunningChild(t *testing.T) {
	session := openChildSession(t, "subagent-slow", nil)
	child, err := session.subagents.Spawn("", subagent.RoleExplore, "wait for cancellation")
	if err != nil {
		t.Fatal(err)
	}
	turnID, err := session.subagents.Takeover(t.Context(), child.ID, "wait for cancellation")
	if err != nil {
		t.Fatal(err)
	}
	threadID := protocol.ThreadID(child.ThreadID)
	session.children.mu.Lock()
	running := session.children.turns[threadID]
	session.children.mu.Unlock()
	if running == nil {
		t.Fatal("child turn not tracked")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	select {
	case <-running.startedSignal:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	registry := tool.NewRegistry(nil, nil)
	if err := agenttool.Register(registry, agenttool.Options{
		Control: session.subagents, Handles: handle.NewStore(),
		OnRelease: session.children.release, SessionID: child.SessionID,
	}); err != nil {
		t.Fatal(err)
	}
	arguments, err := json.Marshal(map[string]string{"agent_id": child.ID})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		closed, err := tooltest.Execute(ctx, registry, tool.Call{
			Name: "close_agent", Arguments: arguments,
		})
		if err != nil {
			t.Fatalf("close_agent: %v", err)
		}
		if closed.Metadata["closed"] != true {
			t.Fatalf("close_agent = %+v", closed)
		}
	}
	select {
	case <-running.terminalSignal:
	case <-ctx.Done():
		t.Fatalf("closed child settlement did not finish: %v", ctx.Err())
	}
	result, ok := session.subagents.Result(child.ID)
	if !ok || result.Status != subagent.StatusInterrupted || result.TurnID != turnID {
		t.Fatalf("real cancellation result = %+v", result)
	}
	if _, registered := session.threads.ChildSpecFor(threadID); registered {
		t.Fatal("closed child retained its thread")
	}
	session.children.mu.Lock()
	tracked, failures := len(session.children.turns), len(session.children.settlementErrors)
	session.children.mu.Unlock()
	if tracked != 0 || failures != 0 {
		t.Fatalf("closed child left turns=%d settlementErrors=%d", tracked, failures)
	}
	spent := session.children.governor.Snapshot()
	if spent.InFlight != 0 || spent.SpentTokens != result.Usage.Tokens() {
		t.Fatalf("closed child budget = %+v, usage=%+v", spent, result.Usage)
	}
	messages := session.subagents.Mailbox().ReceiveSession(child.SessionID, subagent.SessionParentID)
	if len(messages) != 1 || messages[0].Kind != subagent.MessageCompletion {
		t.Fatalf("completion messages = %+v", messages)
	}
	next, err := session.subagents.Spawn("", subagent.RoleExplore, "next task")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.subagents.Takeover(ctx, next.ID, "next task"); err != nil {
		t.Fatalf("close blocked another child: %v", err)
	}
	if err := session.subagents.CloseContext(ctx, next.ID); err != nil {
		t.Fatalf("close immediately after acceptance: %v", err)
	}
	session.children.release(next.ID)
}

type closeRetryGraph struct {
	subagent.Graph
	failing atomic.Bool
	attempt chan struct{}
}

func (*closeRetryGraph) Reconcile() error                { return nil }
func (*closeRetryGraph) ListSessions() ([]string, error) { return nil, nil }
func (g *closeRetryGraph) RecordTransition(change subagent.GraphTransition) error {
	if change.Result != nil && g.failing.Load() {
		select {
		case g.attempt <- struct{}{}:
		default:
		}
		return errors.New("settlement storage unavailable")
	}
	return nil
}

func TestCloseRetainsTurnDuringSettlementRetry(t *testing.T) {
	session := openChildSession(t, "subagent-slow", nil)
	child, err := session.subagents.Spawn("", subagent.RoleExplore, "wait")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.subagents.Takeover(t.Context(), child.ID, "wait"); err != nil {
		t.Fatal(err)
	}
	threadID := protocol.ThreadID(child.ThreadID)
	session.children.mu.Lock()
	turn := session.children.turns[threadID]
	session.children.mu.Unlock()
	if turn == nil {
		t.Fatal("child not tracked")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	select {
	case <-turn.startedSignal:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	graph := &closeRetryGraph{attempt: make(chan struct{}, 1)}
	graph.failing.Store(true)
	if err := session.subagents.AttachGraph(graph); err != nil {
		t.Fatal(err)
	}
	closed := make(chan error, 1)
	go func() {
		err := session.subagents.CloseContext(ctx, child.ID)
		if err == nil {
			session.children.release(child.ID)
		}
		closed <- err
	}()
	select {
	case <-graph.attempt:
	case <-ctx.Done():
		t.Fatal("settlement was not attempted")
	}
	current, ok := session.subagents.Agent(child.ID)
	if !ok || current.Closed || current.Result != nil || !subagent.OccupiesSlot(current.Status) {
		t.Fatalf("failed settlement lost active agent: %+v", current)
	}
	session.children.mu.Lock()
	tracked := session.children.turns[threadID] == turn
	session.children.mu.Unlock()
	if !tracked {
		t.Fatal("unsettled turn was removed from tracking")
	}
	if _, ok := session.threads.ChildSpecFor(threadID); !ok {
		t.Fatal("thread released before settlement")
	}
	// Replayed terminal events must not create a second settlement worker.
	spent := session.children.governor.Snapshot().SpentTokens
	session.children.observe(protocol.Event{
		ThreadID: threadID, TurnID: turn.turnID,
		Data: &protocol.TurnCanceledData{Reason: protocol.CancelReasonHostInterrupted},
	})
	if got := session.children.governor.Snapshot().SpentTokens; got != spent {
		t.Fatalf("duplicate terminal charged budget: %d -> %d", spent, got)
	}
	graph.failing.Store(false)
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	select {
	case <-turn.terminalSignal:
	case <-ctx.Done():
		t.Fatal("settlement retry did not complete")
	}
	session.children.mu.Lock()
	remaining, failures := len(session.children.turns), len(session.children.settlementErrors)
	session.children.mu.Unlock()
	if remaining != 0 || failures != 0 {
		t.Fatalf("settlement recovery left turns=%d errors=%d", remaining, failures)
	}
	if _, ok := session.threads.ChildSpecFor(threadID); ok {
		t.Fatal("settled closed child retained its thread")
	}
	messages := session.subagents.Mailbox().ReceiveSession(child.SessionID, subagent.SessionParentID)
	if len(messages) != 1 {
		t.Fatalf("settlement retry produced %d completion messages", len(messages))
	}
}
