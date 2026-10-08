package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/interact"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type withdrawalContextStore struct {
	baseline  agentcontext.ContextSnapshot
	current   agentcontext.ContextSnapshot
	withdrawn bool
	fail      error
}

func (s *withdrawalContextStore) SaveTurnBaseline(_ context.Context, _ protocol.ThreadID, _ protocol.TurnID, snapshot agentcontext.ContextSnapshot) error {
	if s.baseline.Digest == "" {
		s.baseline = agentcontext.CloneContextSnapshot(snapshot)
	}
	return s.fail
}
func (s *withdrawalContextStore) TurnBaseline(context.Context, protocol.ThreadID, protocol.TurnID) (agentcontext.ContextSnapshot, bool, error) {
	return agentcontext.CloneContextSnapshot(s.baseline), s.baseline.Digest != "", nil
}
func (s *withdrawalContextStore) TurnWithdrawn(context.Context, protocol.ThreadID, protocol.TurnID) (bool, error) {
	return s.withdrawn, nil
}
func (s *withdrawalContextStore) CommitTurnWithdrawal(_ context.Context, _ protocol.ThreadID, _ protocol.TurnID, snapshot agentcontext.ContextSnapshot) error {
	if s.fail != nil {
		return s.fail
	}
	s.current = agentcontext.CloneContextSnapshot(snapshot)
	s.withdrawn = true
	return nil
}

func TestWithdrawTurnRestoresContextWithoutUndoingFilesOrAccounting(t *testing.T) {
	model := &scriptedProvider{}
	engine := newEngine(t, model, tool.NewRegistry(nil, nil))
	engine.options.Workspace = t.TempDir()
	engine.options.WorkspaceIdentity = "workspace:test"
	engine.turn, engine.sessionRevision = 1, 1
	engine.history = []provider.Message{messageWithText(provider.RoleUser, "valid request", 1)}
	engine.historyTurns = map[string]uint64{"previous": 1}
	if err := engine.ApplyPlan(interact.Plan{
		Objective: "valid plan", Steps: []interact.PlanStep{{Title: "valid step", Status: interact.StepPending}},
	}); err != nil {
		t.Fatal(err)
	}
	baselineSource := agentcontext.IndexConversationAnswer("thread", "previous", 1, "1. valid definition")
	conversation := &agentcontext.ConversationState{}
	if err := conversation.Add(baselineSource); err != nil {
		t.Fatal(err)
	}
	engine.context.SetConversation(conversation)
	baseline, err := engine.ExportContextSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	store := &withdrawalContextStore{baseline: baseline}
	engine.options.TurnContexts = store
	engine.turn, engine.sessionRevision = 2, 2
	engine.history = append(engine.history, messageWithText(provider.RoleUser, "mistaken request", 2))
	engine.historyTurns["mistake"] = 2
	engine.turnIDs["mistake"] = 2
	if err := conversation.Add(agentcontext.IndexConversationAnswer("thread", "mistake", 2, "1. mistaken source")); err != nil {
		t.Fatal(err)
	}
	engine.context.SetConversation(conversation)
	engine.usage.InputTokens, engine.costUSD = 123, 1.25
	if err := engine.ApplyPlan(interact.Plan{
		Objective: "mistaken plan", Steps: []interact.PlanStep{{Title: "mistaken step", Status: interact.StepPending}},
	}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(engine.options.Workspace, "kept.txt")
	if err := os.WriteFile(path, []byte("keep this change"), 0o600); err != nil {
		t.Fatal(err)
	}
	engine.context.Evidence().MarkChanged("kept.txt", 2, false)
	engine.sealClosedTurnMemory(agentcontext.CheckpointCanceled, nil, "mistaken conclusion")
	store.fail = errors.New("disk failure")
	if err := engine.WithdrawTurn(t.Context(), "thread", "mistake"); !errors.Is(err, store.fail) {
		t.Fatalf("write failure: %v", err)
	}
	if len(engine.History()) != 2 {
		t.Fatal("failed commit changed live history")
	}
	store.fail = nil
	if err := engine.WithdrawTurn(t.Context(), "thread", "mistake"); err != nil {
		t.Fatal(err)
	}
	if err := engine.WithdrawTurn(t.Context(), "thread", "mistake"); err != nil {
		t.Fatal(err)
	}
	restored, err := engine.ExportContextSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if restored.Conversation == nil || len(restored.Conversation.Sources) != 1 || restored.Conversation.Sources[baselineSource.ID].Text != baselineSource.Text {
		t.Fatal("withdrawal did not restore source baseline")
	}
	raw, err := json.Marshal(restored)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "mistaken") || !strings.Contains(string(raw), "valid request") ||
		!strings.Contains(string(raw), "valid plan") {
		t.Fatalf("restored context: %s", raw)
	}
	if usage, cost := engine.Usage(); usage.InputTokens != 123 || cost != 1.25 {
		t.Fatal("accounting was rolled back")
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "keep this change" {
		t.Fatal("file was rolled back")
	}
	if len(restored.Evidence.Changes) != 1 || !restored.Evidence.Changes[0].Stale {
		t.Fatal("retained changes lost risk evidence")
	}
	if len(restored.TurnCheckpoints) != 0 || restored.Epoch <= baseline.Epoch || restored.Revision <= 2 {
		t.Fatal("stale checkpoints or state fence")
	}
	if messages, _, err := engine.lookupTurnHistoryData(t.Context(), 2); err != nil || len(messages) != 0 {
		t.Fatal("withdrawn turn remained retrievable")
	}
	// The next real model request must not contain the rejected instruction.
	engine.options.TurnContexts = nil
	_, _ = engine.Execute(t.Context(), TurnRequest{TurnID: "next", Prompt: "new request"}, nil)
	if len(model.requests) == 0 {
		t.Fatal("next turn did not sample")
	}
	request, _ := json.Marshal(model.requests[0])
	if strings.Contains(string(request), "mistaken") {
		t.Fatalf("model received withdrawn context: %s", request)
	}
}

func TestTurnBaselinePrecedesPromptAndFailsClosed(t *testing.T) {
	model := &scriptedProvider{}
	engine := newEngine(t, model, tool.NewRegistry(nil, nil))
	engine.options.Workspace = t.TempDir()
	engine.options.WorkspaceIdentity = "workspace:test"
	store := &withdrawalContextStore{fail: errors.New("baseline unavailable")}
	engine.options.TurnContexts = store
	ctx := tool.WithInvocationIdentity(t.Context(), tool.InvocationIdentity{ThreadID: "thread"})
	_, err := engine.Execute(ctx, TurnRequest{TurnID: "mistake", Prompt: "mistaken request"}, nil)
	if !errors.Is(err, store.fail) || len(model.requests) != 0 {
		t.Fatalf("baseline failure sampled: %v", err)
	}
	if len(store.baseline.History) != 0 {
		t.Fatal("baseline contains new prompt")
	}
}
