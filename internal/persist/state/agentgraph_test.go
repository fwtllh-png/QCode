package state

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/orchestration/subagent"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestAppendAgentEventAllocatesSequencesAtomically(t *testing.T) {
	store, err := Open(t.Context(), Options{
		DataDir: t.TempDir(), BusyTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.CloseAll(context.Background()) })

	const count = 32
	errors := make(chan error, count)
	var group sync.WaitGroup
	for index := range count {
		group.Add(1)
		go func() {
			defer group.Done()
			errors <- store.AppendAgentEvent(t.Context(), &protocol.AgentSpawnedData{
				AgentID: fmt.Sprintf("agent-%d", index), WorkspaceRoot: "/workspace",
				SessionID: "session", Role: "explore", Profile: "explore",
				Stance: "read_only",
			})
		}()
	}
	group.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	events, err := store.Replay(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != count {
		t.Fatalf("events = %d, want %d", len(events), count)
	}
}

func TestAgentGraphProjectsAndListsAfterRestart(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := Open(ctx, Options{DataDir: root, BusyTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}

	gate := passGate{}
	manager, err := subagent.Open(subagent.Options{
		Root: t.TempDir(), Gate: gate,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.AttachGraph(NewAgentGraph(store, "/workspace/a", "session-a")); err != nil {
		t.Fatal(err)
	}
	parent, err := manager.Spawn("", subagent.RoleGeneral, "root")
	if err != nil {
		t.Fatal(err)
	}
	child, err := manager.Spawn(parent.ID, subagent.RoleExplore, "map")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Mailbox().Deliver(parent.ID, child.ID, json.RawMessage(`{"hi":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := manager.Complete(child.ID, "done"); err != nil {
		t.Fatal(err)
	}
	if err := store.CloseAll(ctx); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(ctx, Options{DataDir: root, BusyTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.CloseAll(context.Background()) })

	events, err := reopened.Replay(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	var sawSpawn, sawStatus, sawMessage bool
	for _, event := range events {
		switch event.Kind {
		case protocol.EventAgentSpawned:
			sawSpawn = true
		case protocol.EventAgentStatus:
			sawStatus = true
		case protocol.EventAgentMessage:
			sawMessage = true
		}
	}
	if !sawSpawn || !sawStatus || !sawMessage {
		t.Fatalf("durable agent events missing: spawn=%v status=%v message=%v events=%d",
			sawSpawn, sawStatus, sawMessage, len(events))
	}

	fresh, err := subagent.Open(subagent.Options{
		Root: t.TempDir(), Gate: gate,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := fresh.AttachGraph(NewAgentGraph(
		reopened, "/workspace/a", "session-restarted",
	)); err != nil {
		t.Fatal(err)
	}
	if pending := fresh.Mailbox().Drain(child.ID); len(pending) != 1 ||
		pending[0].Kind != subagent.MessageContext {
		t.Fatalf("child mailbox after restart = %+v", pending)
	}
	if completion := fresh.Mailbox().Drain(parent.ID); len(completion) != 1 ||
		completion[0].Kind != subagent.MessageCompletion {
		t.Fatalf("completion mailbox after restart = %+v", completion)
	}
	listed := fresh.List(subagent.ListFilter{ParentID: parent.ID, IncludeClosed: true})
	if len(listed) != 1 || listed[0].ID != child.ID {
		t.Fatalf("list children after restart = %+v, want %s", listed, child.ID)
	}
	if listed[0].Status != subagent.StatusCompleted {
		t.Fatalf("child status = %q, want completed", listed[0].Status)
	}
	if listed[0].Workspace != "/workspace/a" || listed[0].SessionID != "session-a" {
		t.Fatalf("child identity = workspace %q session %q",
			listed[0].Workspace, listed[0].SessionID)
	}
}

func TestAgentGraphIsolatesWorkspacesWithCollidingAgentIDs(t *testing.T) {
	store, err := Open(t.Context(), Options{
		DataDir: t.TempDir(), BusyTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.CloseAll(context.Background()) })

	openManager := func(workspace, session string) *subagent.Manager {
		manager, openErr := subagent.Open(subagent.Options{
			Root: t.TempDir(), Gate: passGate{},
		})
		if openErr != nil {
			t.Fatal(openErr)
		}
		if attachErr := manager.AttachGraph(NewAgentGraph(
			store, workspace, session,
		)); attachErr != nil {
			t.Fatal(attachErr)
		}
		return manager
	}
	first := openManager("/workspace/a", "session-a")
	second := openManager("/workspace/b", "session-b")
	firstAgent, err := first.Spawn("", subagent.RoleExplore, "first")
	if err != nil {
		t.Fatal(err)
	}
	secondAgent, err := second.Spawn("", subagent.RoleReview, "second")
	if err != nil {
		t.Fatal(err)
	}
	if firstAgent.ID != "agent-1" || secondAgent.ID != "agent-1" {
		t.Fatalf("colliding ids = %q, %q", firstAgent.ID, secondAgent.ID)
	}

	firstRows, err := store.ListAgentChildren(t.Context(), "/workspace/a", "")
	if err != nil {
		t.Fatal(err)
	}
	secondRows, err := store.ListAgentChildren(t.Context(), "/workspace/b", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(firstRows) != 1 || firstRows[0].Role != string(subagent.RoleExplore) ||
		firstRows[0].SessionID != "session-a" {
		t.Fatalf("workspace a rows = %+v", firstRows)
	}
	if len(secondRows) != 1 || secondRows[0].Role != string(subagent.RoleReview) ||
		secondRows[0].SessionID != "session-b" {
		t.Fatalf("workspace b rows = %+v", secondRows)
	}
}

func TestAgentGraphIsolatesSessionsWithinWorkspace(t *testing.T) {
	store, err := Open(t.Context(), Options{
		DataDir: t.TempDir(), BusyTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.CloseAll(context.Background()) })
	const workspace = "/workspace/shared"
	openControl := func(sessionID string) *subagent.AgentControl {
		control, err := subagent.OpenControl(subagent.Options{
			Root: t.TempDir(), Gate: passGate{}, Budget: subagent.Budget{
				MaxParallel: 1, MaxResident: 1, MaxTotal: 1,
			}, SessionID: sessionID,
		}, subagent.DelegationExplicit)
		if err != nil {
			t.Fatal(err)
		}
		if err := control.AttachGraph(NewAgentGraph(
			store, workspace, sessionID,
		)); err != nil {
			t.Fatal(err)
		}
		return control
	}
	first := openControl("session-a")
	firstAgent, err := first.SpawnSystem(
		"first", "", subagent.RoleExplore, "inspect", "report",
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Complete(firstAgent.ID, "a"); err != nil {
		t.Fatal(err)
	}
	second := openControl("session-b")
	secondAgent, err := second.SpawnSystem(
		"second", "", subagent.RoleExplore, "inspect", "report",
	)
	if err != nil {
		t.Fatalf("session-b inherited session-a max_total: %v", err)
	}
	if firstAgent.ID == secondAgent.ID {
		t.Fatalf("workspace agent ids collided: %s", firstAgent.ID)
	}
	if got := second.List(subagent.ListFilter{
		SessionID: "session-b",
	}); len(got) != 1 || got[0].ID != secondAgent.ID {
		t.Fatalf("session-b tree = %+v", got)
	}
	if got := second.List(subagent.ListFilter{
		SessionID: "session-a",
	}); len(got) != 1 || got[0].ID != firstAgent.ID {
		t.Fatalf("session-a tree = %+v", got)
	}
	restarted := openControl("session-c")
	for _, sessionID := range []string{"session-a", "session-b"} {
		if ledger := restarted.SessionBudget(sessionID); ledger.TotalSpawned != 1 {
			t.Fatalf("%s hydrated ledger = %+v", sessionID, ledger)
		}
	}
}

func TestAgentTerminalCommitIsAtomicAndCASGuarded(t *testing.T) {
	store, err := Open(t.Context(), Options{
		DataDir: t.TempDir(), BusyTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.CloseAll(context.Background()) })
	graph := NewAgentGraph(store, "/workspace/atomic", "session-atomic")
	control, err := subagent.OpenControl(subagent.Options{
		Root: t.TempDir(), Gate: passGate{}, Budget: subagent.Budget{MaxTokens: 20, MaxParallel: 4},
	}, subagent.DelegationExplicit)
	if err != nil {
		t.Fatal(err)
	}
	if err := control.AttachGraph(graph); err != nil {
		t.Fatal(err)
	}
	agent, err := control.SpawnSystem(
		"inspect runtime", "", subagent.RoleExplore, "inspect", "report",
	)
	if err != nil {
		t.Fatal(err)
	}
	if agent.Path != "/root/inspect_runtime" || agent.Revision != 1 {
		t.Fatalf("spawned agent = %+v", agent)
	}
	terminal := subagent.Result{
		AgentID: agent.ID, ThreadID: agent.ThreadID, TurnID: "turn-child",
		Status: subagent.StatusCompleted, Summary: "done",
		Usage: subagent.ResultUsage{
			InputTokens: 13, OutputTokens: 8,
			CostMicrounits: 5, CostKnown: true,
		},
	}
	if err := control.Settle(terminal); err != nil {
		t.Fatal(err)
	}
	if err := control.Settle(terminal); err != nil {
		t.Fatal(err)
	}
	rows, err := store.ListAgentChildren(t.Context(), "/workspace/atomic", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != string(subagent.StatusCompleted) ||
		rows[0].Revision != 2 ||
		rows[0].SpentTokens != 21 ||
		rows[0].SpentMicros != 5 {
		t.Fatalf("terminal node = %+v", rows)
	}
	if result, ok, err := store.LoadAgentResult(
		t.Context(), "/workspace/atomic", agent.ID,
	); err != nil || !ok || result.Summary != "done" {
		t.Fatalf("durable result = %+v, ok=%v, err=%v", result, ok, err)
	}
	messages, err := store.ListAgentMessages(
		t.Context(), "/workspace/atomic", subagent.SessionParentID,
	)
	if err != nil || len(messages) != 1 ||
		messages[0].Kind != subagent.MessageCompletion {
		t.Fatalf("completion outbox = %+v, err=%v", messages, err)
	}
	if unpublished, err := store.ListUnpublishedAgentCompletions(
		t.Context(), "/workspace/atomic",
	); err != nil || len(unpublished) != 0 {
		t.Fatalf("unpublished live completions = %+v, err=%v", unpublished, err)
	}
	err = graph.RecordTransition(subagent.GraphTransition{
		AgentID: agent.ID, Path: agent.Path, ExpectedRevision: 1,
		Status: subagent.StatusFailed, OperationID: "stale",
		Actor: "test", CreatedAt: time.Now().UTC(),
	})
	if err == nil {
		t.Fatal("stale transition was accepted")
	}
	if _, err := control.SpawnSystem(
		"over budget", "", subagent.RoleExplore, "inspect", "report",
	); err == nil {
		t.Fatal("spent token budget did not block a new child")
	}
	restarted, err := subagent.OpenControl(subagent.Options{
		Root: t.TempDir(), Gate: passGate{}, Budget: subagent.Budget{MaxTokens: 20, MaxParallel: 4},
	}, subagent.DelegationExplicit)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.AttachGraph(graph); err != nil {
		t.Fatal(err)
	}
	recovered, ok := restarted.Agent(agent.ID)
	if !ok || recovered.SpentTokens != 21 || recovered.SpentMicros != 5 {
		t.Fatalf("recovered Agent lifecycle spend = %+v, ok=%v", recovered, ok)
	}
	if ledger := restarted.SessionBudget(agent.SessionID); ledger.ReservedSlots != 0 ||
		ledger.SpentTokens != 21 || ledger.SpentMicros != 5 || ledger.TotalSpawned != 1 {
		t.Fatalf("hydrated tree ledger = %+v", ledger)
	}
	if _, err := restarted.SpawnSystem(
		"still over budget", "", subagent.RoleExplore, "inspect", "report",
	); err == nil {
		t.Fatal("restarted tree forgot spent token budget")
	}
}

func TestAgentGraphRetainsIntegrationResultAcrossFailedFollowUp(t *testing.T) {
	root := t.TempDir()
	store, err := Open(t.Context(), Options{
		DataDir: root, BusyTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	graph := NewAgentGraph(store, "/workspace/result-baseline", "session-baseline")
	control, err := subagent.OpenControl(subagent.Options{
		Root: t.TempDir(), Gate: passGate{},
	}, subagent.DelegationExplicit)
	if err != nil {
		t.Fatal(err)
	}
	if err := control.AttachGraph(graph); err != nil {
		t.Fatal(err)
	}
	agent, err := control.SpawnIntent(subagent.DelegationIntent{
		TaskName: "writer", Role: subagent.RoleImplementer,
		Objective: "write report", ExpectedOutput: "report",
		OwnedPaths: []string{"result.txt"}, Trigger: subagent.TriggerSystem,
	})
	if err != nil {
		t.Fatal(err)
	}
	success := subagent.Result{
		AgentID: agent.ID, ThreadID: agent.ThreadID, TurnID: "turn-success",
		Status: subagent.StatusCompleted, Summary: "wrote report",
		Diff: []protocol.ReceiptChange{{
			Path: "result.txt", Tool: "file_write", Kind: "created",
		}},
	}
	if err := control.Settle(success); err != nil {
		t.Fatal(err)
	}
	if _, err := control.FollowUp(t.Context(), agent.ID, "recheck"); err != nil {
		t.Fatal(err)
	}
	if err := control.Settle(subagent.Result{
		AgentID: agent.ID, ThreadID: agent.ThreadID, TurnID: "turn-failed",
		Status: subagent.StatusFailed, Summary: "recheck failed",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.CloseAll(t.Context()); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(t.Context(), Options{
		DataDir: root, BusyTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.CloseAll(context.Background()) })
	fresh, err := subagent.OpenControl(subagent.Options{
		Root: t.TempDir(), Gate: passGate{},
	}, subagent.DelegationExplicit)
	if err != nil {
		t.Fatal(err)
	}
	if err := fresh.AttachGraph(NewAgentGraph(
		reopened, "/workspace/result-baseline", "session-baseline",
	)); err != nil {
		t.Fatal(err)
	}
	current, ok := fresh.Result(agent.ID)
	if !ok || current.Status != subagent.StatusFailed {
		t.Fatalf("current Result = %+v, ok=%v", current, ok)
	}
	baseline, ok := fresh.IntegrationResult(agent.ID)
	if !ok || baseline.Status != subagent.StatusCompleted ||
		baseline.TurnID != success.TurnID {
		t.Fatalf("Integration Result = %+v, ok=%v", baseline, ok)
	}
	if owner, claimed := fresh.WriteOwner("result.txt"); !claimed ||
		owner != agent.ID {
		t.Fatalf("write owner = %q, claimed=%v", owner, claimed)
	}
}

func TestAgentIntegrationCandidatePersistsAndRejectsRevisionGap(t *testing.T) {
	store, err := Open(t.Context(), Options{
		DataDir: t.TempDir(), BusyTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.CloseAll(context.Background()) })
	graph := NewAgentGraph(store, "/workspace/integration", "session-integration")
	control, err := subagent.OpenControl(subagent.Options{
		Root: t.TempDir(), Gate: passGate{},
	}, subagent.DelegationExplicit)
	if err != nil {
		t.Fatal(err)
	}
	if err := control.AttachGraph(graph); err != nil {
		t.Fatal(err)
	}
	agent, err := control.SpawnIntent(subagent.DelegationIntent{
		TaskName: "writer", Role: subagent.RoleExplore, Objective: "inspect",
		ExpectedOutput: "report", OwnedPaths: []string{"result.txt"},
		Trigger: subagent.TriggerSystem,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := control.Settle(subagent.Result{
		AgentID: agent.ID, ThreadID: agent.ThreadID, TurnID: "turn-result",
		Status: subagent.StatusCompleted, Summary: "done",
		Diff: []protocol.ReceiptChange{{
			Path: "result.txt", Tool: "file_write", Kind: "created",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	candidate := integrationCandidate(agent, strings.Repeat("a", 64))
	if err := control.SaveIntegration(candidate); err != nil {
		t.Fatal(err)
	}
	loaded, ok, err := store.LoadAgentIntegration(
		t.Context(), "/workspace/integration", agent.ID, candidate.PreviewDigest,
	)
	if err != nil || !ok || loaded.Status != subagent.IntegrationPreviewed ||
		loaded.Revision != 1 || loaded.AttemptID != candidate.AttemptID {
		t.Fatalf("loaded candidate = %+v, ok=%v, err=%v", loaded, ok, err)
	}
	candidate.Status = subagent.IntegrationApplying
	candidate.Revision = 3
	if err := graph.RecordIntegration(candidate); err == nil {
		t.Fatal("integration revision gap was accepted")
	}
}

func TestAgentIntegrationRecoveryConvergesInterruptedApply(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	store, err := Open(ctx, Options{DataDir: root, BusyTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	graph := NewAgentGraph(store, "/workspace/apply-crash", "session-apply-crash")
	control, err := subagent.OpenControl(subagent.Options{
		Root: t.TempDir(), Gate: passGate{},
	}, subagent.DelegationExplicit)
	if err != nil {
		t.Fatal(err)
	}
	if err := control.AttachGraph(graph); err != nil {
		t.Fatal(err)
	}
	agent, err := control.SpawnIntent(subagent.DelegationIntent{
		TaskName: "writer", Role: subagent.RoleExplore, Objective: "inspect",
		ExpectedOutput: "report", OwnedPaths: []string{"result.txt"},
		Trigger: subagent.TriggerSystem,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := control.Settle(subagent.Result{
		AgentID: agent.ID, ThreadID: agent.ThreadID, TurnID: "turn-result",
		Status: subagent.StatusCompleted, Summary: "done",
		Diff: []protocol.ReceiptChange{{
			Path: "result.txt", Tool: "file_write", Kind: "created",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	candidate := integrationCandidate(agent, strings.Repeat("b", 64))
	if err := control.SaveIntegration(candidate); err != nil {
		t.Fatal(err)
	}
	if err := control.BeginIntegration(agent.ID); err != nil {
		t.Fatal(err)
	}
	candidate.Status = subagent.IntegrationApplying
	if err := control.SaveIntegration(candidate); err != nil {
		t.Fatal(err)
	}
	if err := store.CloseAll(ctx); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(ctx, Options{DataDir: root, BusyTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.CloseAll(context.Background()) })
	fresh, err := subagent.OpenControl(subagent.Options{
		Root: t.TempDir(), Gate: passGate{},
	}, subagent.DelegationExplicit)
	if err != nil {
		t.Fatal(err)
	}
	if err := fresh.AttachGraph(NewAgentGraph(
		reopened, "/workspace/apply-crash", "session-apply-crash",
	)); err != nil {
		t.Fatal(err)
	}
	recovered, ok, err := fresh.Integration(agent.ID, candidate.PreviewDigest)
	if err != nil || !ok || recovered.Status != subagent.IntegrationFailed {
		t.Fatalf("recovered candidate = %+v, ok=%v, err=%v", recovered, ok, err)
	}
	node, ok := fresh.Agent(agent.ID)
	if !ok || node.Status != subagent.StatusIntegrationFailed {
		t.Fatalf("recovered agent = %+v, ok=%v", node, ok)
	}
	if len(node.OwnedPaths) != 1 || node.OwnedPaths[0] != "result.txt" {
		t.Fatalf("recovered owned paths = %#v", node.OwnedPaths)
	}
	if owner, claimed := fresh.WriteOwner("result.txt"); !claimed || owner != agent.ID {
		t.Fatalf("recovered write owner = %q, claimed=%v", owner, claimed)
	}
}

func TestAgentIntegrationRecoveryCompletesAppliedCandidate(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	store, err := Open(ctx, Options{DataDir: root, BusyTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	graph := NewAgentGraph(store, "/workspace/applied-crash", "session-applied-crash")
	control, err := subagent.OpenControl(subagent.Options{
		Root: t.TempDir(), Gate: passGate{},
	}, subagent.DelegationExplicit)
	if err != nil {
		t.Fatal(err)
	}
	if err := control.AttachGraph(graph); err != nil {
		t.Fatal(err)
	}
	agent, err := control.SpawnSystem(
		"writer", "", subagent.RoleExplore, "inspect", "report",
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := control.Complete(agent.ID, "done"); err != nil {
		t.Fatal(err)
	}
	candidate := integrationCandidate(agent, strings.Repeat("c", 64))
	if err := control.SaveIntegration(candidate); err != nil {
		t.Fatal(err)
	}
	if err := control.BeginIntegration(agent.ID); err != nil {
		t.Fatal(err)
	}
	candidate.Status = subagent.IntegrationApplying
	if err := control.SaveIntegration(candidate); err != nil {
		t.Fatal(err)
	}
	candidate.Status = subagent.IntegrationApplied
	candidate.Receipt = &subagent.IntegrationReceipt{
		ChangedPaths: []string{"result.txt"},
		Verification: protocol.ReceiptVerification{
			Diagnostics: protocol.ReceiptNotEvaluated,
			Tests:       protocol.ReceiptPassed,
			Verify:      protocol.ReceiptPassed,
		},
		AppliedAt: time.Now().UTC(),
	}
	if err := control.SaveIntegration(candidate); err != nil {
		t.Fatal(err)
	}
	if err := store.CloseAll(ctx); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(ctx, Options{DataDir: root, BusyTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.CloseAll(context.Background()) })
	fresh, err := subagent.OpenControl(subagent.Options{
		Root: t.TempDir(), Gate: passGate{},
	}, subagent.DelegationExplicit)
	if err != nil {
		t.Fatal(err)
	}
	if err := fresh.AttachGraph(NewAgentGraph(
		reopened, "/workspace/applied-crash", "session-applied-crash",
	)); err != nil {
		t.Fatal(err)
	}
	node, ok := fresh.Agent(agent.ID)
	if !ok || node.Status != subagent.StatusIntegrated {
		t.Fatalf("recovered applied agent = %+v, ok=%v", node, ok)
	}
	recovered, ok, err := fresh.Integration(agent.ID, candidate.PreviewDigest)
	if err != nil || !ok || recovered.Status != subagent.IntegrationApplied ||
		recovered.Receipt == nil ||
		recovered.Receipt.Verification.Verify != protocol.ReceiptPassed {
		t.Fatalf("recovered receipt = %+v, ok=%v, err=%v", recovered, ok, err)
	}
}

func integrationCandidate(
	agent *subagent.Agent,
	digest string,
) subagent.IntegrationCandidate {
	return subagent.IntegrationCandidate{
		AgentID: agent.ID, AgentPath: agent.Path,
		ParentID: agent.Parent, ParentPath: agent.ParentPath,
		AttemptID: "attempt-" + agent.ID, PreviewDigest: digest,
		Status:       subagent.IntegrationPreviewed,
		BaseRevision: "base", ResultTurnID: "turn-result",
		Paths: []string{"result.txt"},
		Changes: []subagent.IntegrationChange{{
			Op: "write", Path: "result.txt", Content: "done\n",
		}},
		Verification: protocol.ReceiptVerification{
			Diagnostics: protocol.ReceiptNotEvaluated,
			Tests:       protocol.ReceiptNotEvaluated,
			Verify:      protocol.ReceiptNotEvaluated,
		},
	}
}

func TestAgentRestartReconcilesMissingAndActiveTurns(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	store, err := Open(ctx, Options{DataDir: root, BusyTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	graph := NewAgentGraph(store, "/workspace/recovery", "session-recovery")
	manager, err := subagent.Open(subagent.Options{
		Root: t.TempDir(), Gate: passGate{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.AttachGraph(graph); err != nil {
		t.Fatal(err)
	}
	missing, err := manager.Spawn("", subagent.RoleExplore, "missing turn")
	if err != nil {
		t.Fatal(err)
	}
	active, err := manager.Spawn("", subagent.RoleExplore, "active turn")
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := manager.Spawn("", subagent.RoleImplementer, "waiting approval")
	if err != nil {
		t.Fatal(err)
	}
	if err := seedActiveAgentTurn(
		ctx, store, active.ThreadID, "turn-active", "session-recovery",
	); err != nil {
		t.Fatal(err)
	}
	if err := graph.RecordTransition(subagent.GraphTransition{
		AgentID: active.ID, Path: active.Path, ExpectedRevision: 1,
		Status: subagent.StatusStarting, TurnID: "turn-active",
		OperationID: "start-active", Actor: "test",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := seedActiveAgentTurn(
		ctx, store, waiting.ThreadID, "turn-waiting", "session-recovery",
	); err != nil {
		t.Fatal(err)
	}
	for _, transition := range []subagent.GraphTransition{
		{
			AgentID: waiting.ID, Path: waiting.Path, ExpectedRevision: 1,
			Status: subagent.StatusStarting, TurnID: "turn-waiting",
			OperationID: "start-waiting", Actor: "test",
			CreatedAt: time.Now().UTC(),
		},
		{
			AgentID: waiting.ID, Path: waiting.Path, ExpectedRevision: 2,
			Status: subagent.StatusRunning, TurnID: "turn-waiting",
			OperationID: "run-waiting", Actor: "test",
			CreatedAt: time.Now().UTC(),
		},
		{
			AgentID: waiting.ID, Path: waiting.Path, ExpectedRevision: 3,
			Status: subagent.StatusWaiting, TurnID: "turn-waiting",
			OperationID: "await-approval", Actor: "test",
			CreatedAt: time.Now().UTC(),
		},
	} {
		if err := graph.RecordTransition(transition); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.CloseAll(ctx); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, Options{DataDir: root, BusyTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.CloseAll(context.Background()) })
	fresh, err := subagent.Open(subagent.Options{
		Root: t.TempDir(), Gate: passGate{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := fresh.AttachGraph(NewAgentGraph(
		reopened, "/workspace/recovery", "session-recovery",
	)); err != nil {
		t.Fatal(err)
	}
	missingNode, ok := fresh.Agent(missing.ID)
	if !ok || missingNode.Status != subagent.StatusFailed ||
		missingNode.Result == nil {
		t.Fatalf("missing-turn reconciliation = %+v, ok=%v", missingNode, ok)
	}
	activeNode, ok := fresh.Agent(active.ID)
	if !ok || activeNode.Status != subagent.StatusRunning ||
		activeNode.TurnID != "turn-active" {
		t.Fatalf("active-turn reconciliation = %+v, ok=%v", activeNode, ok)
	}
	waitingNode, ok := fresh.Agent(waiting.ID)
	if !ok || waitingNode.Status != subagent.StatusWaiting ||
		waitingNode.TurnID != "turn-waiting" {
		t.Fatalf("waiting-turn reconciliation = %+v, ok=%v", waitingNode, ok)
	}
	completions := fresh.Mailbox().Receive("parent")
	if len(completions) != 1 || completions[0].From != missing.ID {
		t.Fatalf("recovery completions = %+v", completions)
	}
	if repeated := fresh.Mailbox().Receive("parent"); len(repeated) != 1 ||
		repeated[0].ID != completions[0].ID {
		t.Fatalf("unacknowledged completion was not redelivered = %+v", repeated)
	}
	if err := fresh.Mailbox().Ack(completions); err != nil {
		t.Fatal(err)
	}
	if err := fresh.Mailbox().Ack(completions); err != nil {
		t.Fatalf("duplicate completion ack: %v", err)
	}
	if again := fresh.Mailbox().Receive("parent"); len(again) != 0 {
		t.Fatalf("acknowledged completion remained pending = %+v", again)
	}
	if err := reopened.CloseAll(ctx); err != nil {
		t.Fatal(err)
	}
	third, err := Open(ctx, Options{DataDir: root, BusyTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = third.CloseAll(context.Background()) })
	afterDelivery, err := subagent.Open(subagent.Options{
		Root: t.TempDir(), Gate: passGate{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := afterDelivery.AttachGraph(NewAgentGraph(
		third, "/workspace/recovery", "session-recovery",
	)); err != nil {
		t.Fatal(err)
	}
	if replayed := afterDelivery.Mailbox().Drain("parent"); len(replayed) != 0 {
		t.Fatalf("acknowledged completion replayed after restart = %+v", replayed)
	}
	next, err := afterDelivery.Mailbox().Deliver(
		"test", subagent.SessionParentID, json.RawMessage(`{"after":"restart"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	if next.Sequence != 2 {
		t.Fatalf("post-restart mailbox sequence = %d, want 2", next.Sequence)
	}
}

func TestFollowUpPersistsTaskBeforeStartingTurn(t *testing.T) {
	store, err := Open(t.Context(), Options{
		DataDir: t.TempDir(), BusyTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.CloseAll(context.Background()) })
	runtime := &capturingAgentRuntime{}
	manager, err := subagent.Open(subagent.Options{
		Root: t.TempDir(), Gate: passGate{}, Runtime: runtime,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.AttachGraph(NewAgentGraph(
		store, "/workspace/followup", "session-followup",
	)); err != nil {
		t.Fatal(err)
	}
	agent, err := manager.Spawn("", subagent.RoleExplore, "inspect")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Complete(agent.ID, "first result"); err != nil {
		t.Fatal(err)
	}
	turnID, err := manager.FollowUp(t.Context(), agent.ID, "inspect the retry")
	if err != nil {
		t.Fatal(err)
	}
	if turnID != "turn-followup" ||
		!strings.Contains(runtime.prompt, `"kind":"task"`) ||
		!strings.Contains(runtime.prompt, "inspect the retry") {
		t.Fatalf("follow-up turn=%q prompt=%q", turnID, runtime.prompt)
	}
	var delivered int
	if err := store.SQLite().DB().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM agent_messages
		WHERE workspace_root = '/workspace/followup'
		  AND to_agent_id = ? AND kind = 'task' AND delivered_at IS NOT NULL`,
		agent.ID,
	).Scan(&delivered); err != nil {
		t.Fatal(err)
	}
	if delivered != 1 {
		t.Fatalf("durable delivered task messages = %d, want 1", delivered)
	}
}

func TestOrphanedWorktreeRecoversOwningSession(t *testing.T) {
	store, err := Open(t.Context(), Options{
		DataDir: t.TempDir(), BusyTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.CloseAll(context.Background()) })
	root := t.TempDir()
	worktree := filepath.Join(root, "worktrees", "agent-7")
	if err := os.MkdirAll(worktree, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(worktree, ".qcode-worktree"),
		[]byte("agent-7\n"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	allocationDirectory := filepath.Join(root, "worktree-allocations")
	if err := os.MkdirAll(allocationDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	allocation, err := json.Marshal(map[string]any{
		"version": 1,
		"edge": subagent.GraphEdge{
			ChildID: "agent-7", Path: "/root/recovered_agent_7",
			ParentPath: "/root", Workspace: "/workspace/orphan",
			SessionID: "session-owner", ThreadID: "thread-agent-7",
			Status: subagent.StatusRequested, Revision: 1,
			Role: subagent.RoleExplore, Profile: "explore",
			Stance: subagent.StanceReadOnly, TaskName: "recovered_agent_7",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(allocationDirectory, "agent-7.json"),
		allocation, 0o600,
	); err != nil {
		t.Fatal(err)
	}
	manager, err := subagent.Open(subagent.Options{
		Root: root, Gate: passGate{}, SessionID: "process-restarted",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.AttachGraph(NewAgentGraph(
		store, "/workspace/orphan", "process-restarted",
	)); err != nil {
		t.Fatal(err)
	}
	recovered, ok := manager.Agent("agent-7")
	if !ok || recovered.Status != subagent.StatusFailed ||
		recovered.Result == nil ||
		recovered.Path != "/root/recovered_agent_7" ||
		recovered.SessionID != "session-owner" {
		t.Fatalf("recovered orphan = %+v, ok=%v", recovered, ok)
	}
	sessions, err := store.ListAgentSessions(t.Context(), "/workspace/orphan")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0] != "session-owner" {
		t.Fatalf("recovered sessions = %v, want [session-owner]", sessions)
	}
	completions := manager.Mailbox().PendingSession("session-owner", "parent")
	if len(completions) != 1 ||
		len(manager.Mailbox().PendingSession("process-restarted", "parent")) != 0 {
		t.Fatalf("recovered completions = %+v", completions)
	}
	if _, err := os.Stat(worktree); err != nil {
		t.Fatalf("orphan evidence was removed before explicit close: %v", err)
	}
	if err := manager.Close(recovered.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(worktree); !os.IsNotExist(err) {
		t.Fatalf("closed orphan worktree still exists: %v", err)
	}
}

func TestOrphanedWorktreeWithoutAllocationIsQuarantined(t *testing.T) {
	store, err := Open(t.Context(), Options{
		DataDir: t.TempDir(), BusyTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.CloseAll(context.Background()) })
	root := t.TempDir()
	worktree := filepath.Join(root, "worktrees", "agent-8")
	if err := os.MkdirAll(worktree, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(worktree, ".qcode-worktree"),
		[]byte("agent-8\n"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	manager, err := subagent.Open(subagent.Options{
		Root: root, Gate: passGate{}, SessionID: "process-restarted",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.AttachGraph(NewAgentGraph(
		store, "/workspace/orphan", "process-restarted",
	)); err != nil {
		t.Fatal(err)
	}
	if sessions, listErr := store.ListAgentSessions(
		t.Context(), "/workspace/orphan",
	); listErr != nil || len(sessions) != 0 {
		t.Fatalf("unowned orphan sessions = %v, err=%v", sessions, listErr)
	}
	if _, ok := manager.Agent("agent-8"); ok {
		t.Fatal("quarantined orphan was claimed as an Agent")
	}
	raw, err := os.ReadFile(filepath.Join(
		root, "worktree-quarantine", "agent-8.json",
	))
	if err != nil {
		t.Fatal(err)
	}
	var quarantine struct {
		AgentID string `json:"agent_id"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal(raw, &quarantine); err != nil {
		t.Fatal(err)
	}
	if quarantine.AgentID != "agent-8" ||
		!strings.Contains(quarantine.Reason, "worktree-allocations") {
		t.Fatalf("quarantine = %+v", quarantine)
	}
	next, err := manager.Spawn("", subagent.RoleExplore, "continue safely")
	if err != nil {
		t.Fatal(err)
	}
	if next.ID != "agent-9" {
		t.Fatalf("next Agent = %s, want agent-9", next.ID)
	}
}

func TestCompletionOutboxReplaysAfterPublishCrash(t *testing.T) {
	store, err := Open(t.Context(), Options{
		DataDir: t.TempDir(), BusyTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.CloseAll(context.Background()) })
	graph := NewAgentGraph(store, "/workspace/outbox", "session-outbox")
	manager, err := subagent.Open(subagent.Options{
		Root: t.TempDir(), Gate: passGate{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.AttachGraph(graph); err != nil {
		t.Fatal(err)
	}
	agent, err := manager.Spawn("", subagent.RoleExplore, "crash")
	if err != nil {
		t.Fatal(err)
	}
	result := subagent.Result{
		AgentID: agent.ID, ThreadID: agent.ThreadID,
		Status: subagent.StatusFailed, Summary: "crashed before publish",
	}
	transition := subagent.GraphTransition{
		AgentID: agent.ID, Path: agent.Path, ExpectedRevision: 1,
		Status: subagent.StatusFailed, Message: result.Summary,
		OperationID: "terminal-before-publish", Actor: "test",
		Result: &result, CreatedAt: time.Now().UTC(),
	}
	detail, err := json.Marshal(transition)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AppendAgentEvent(t.Context(), &protocol.AgentStatusData{
		AgentID: agent.ID, WorkspaceRoot: "/workspace/outbox",
		SessionID: "session-outbox", Status: string(subagent.StatusFailed),
		Message: result.Summary, Detail: detail,
	}); err != nil {
		t.Fatal(err)
	}
	if pending, err := store.ListUnpublishedAgentCompletions(
		t.Context(), "/workspace/outbox",
	); err != nil || len(pending) != 1 {
		t.Fatalf("unpublished completions = %+v, err=%v", pending, err)
	}
	restarted, err := subagent.Open(subagent.Options{
		Root: t.TempDir(), Gate: passGate{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.AttachGraph(NewAgentGraph(
		store, "/workspace/outbox", "session-outbox",
	)); err != nil {
		t.Fatal(err)
	}
	if pending, err := store.ListUnpublishedAgentCompletions(
		t.Context(), "/workspace/outbox",
	); err != nil || len(pending) != 0 {
		t.Fatalf("completion outbox was not published: %+v, err=%v", pending, err)
	}
	completions := restarted.Mailbox().Receive("parent")
	if len(completions) != 1 || completions[0].Kind != subagent.MessageCompletion {
		t.Fatalf("replayed completion mailbox = %+v", completions)
	}
}

func seedActiveAgentTurn(
	ctx context.Context,
	store *Store,
	threadID, turnID, sessionID string,
) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	db := store.SQLite().DB()
	if _, err := db.ExecContext(ctx, `
		INSERT OR IGNORE INTO workspaces(id, root_path, created_at, updated_at)
		VALUES ('workspace-recovery', '/workspace/recovery', ?, ?)`,
		now, now,
	); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `
		INSERT OR IGNORE INTO sessions(id, workspace_id, status, created_at, updated_at)
		VALUES (?, 'workspace-recovery', 'open', ?, ?)`,
		sessionID, now, now,
	); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO threads(id, session_id, title, status, created_at, updated_at)
		VALUES (?, ?, 'child', 'open', ?, ?)`,
		threadID, sessionID, now, now,
	); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO turns(id, thread_id, ordinal, status, created_at, updated_at)
		VALUES (?, ?, 1, 'active', ?, ?)`,
		turnID, threadID, now, now,
	)
	return err
}

type passGate struct{}

func (passGate) Execute(context.Context, string, string, json.RawMessage) (tool.Result, error) {
	return tool.Result{Content: "ok"}, nil
}

type capturingAgentRuntime struct {
	prompt string
}

func (r *capturingAgentRuntime) StartTurn(
	_ context.Context, _ string, prompt string,
) (string, error) {
	r.prompt = prompt
	return "turn-followup", nil
}

func (*capturingAgentRuntime) CancelTurn(
	context.Context, string, string,
) error {
	return nil
}
