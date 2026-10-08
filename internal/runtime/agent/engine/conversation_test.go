package engine

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/interact"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestConversationUnfinishedAnswerIsNotACompletedSource(t *testing.T) {
	for _, terminal := range []error{errors.New("provider failed"), context.Canceled} {
		t.Run(terminal.Error(), func(t *testing.T) {
			runtime := &scriptedProvider{streams: []provider.Stream{&eventErrorStream{
				events: []provider.StreamEvent{{Type: provider.EventTextDelta, Text: "1. 尚未完成的报告"}},
				err:    terminal,
			}}}
			engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
			engine.options.Workspace = t.TempDir()
			engine.options.MaxRetries = 0
			_, err := engine.Execute(t.Context(), TurnRequest{Prompt: "分析问题"}, nil)
			if err == nil {
				t.Fatal("fixture unexpectedly completed")
			}
			if state := engine.context.Conversation(); state != nil && len(state.Sources) != 0 {
				t.Fatal("unfinished answer became a completed report")
			}
		})
	}
}

func TestConversationPreservesTerminalAnswerBytes(t *testing.T) {
	answer := "\n1. 完整定义\n2. 尾部定义\n\n"
	runtime := &scriptedProvider{streams: []provider.Stream{textStream(answer)}}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	engine.options.Workspace = t.TempDir()
	if _, err := engine.Execute(t.Context(), TurnRequest{Prompt: "报告"}, nil); err != nil {
		t.Fatal(err)
	}
	state := engine.context.Conversation()
	if state == nil || len(state.Sources) != 1 {
		t.Fatal("completed source missing")
	}
	for _, source := range state.Sources {
		if source.Text != answer {
			t.Fatalf("terminal bytes changed: %q", source.Text)
		}
	}
}

func TestConversationHistoryRecoversIDsAfterFocusClear(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{textStream("1. 第一项\n2. 第二项定义")}}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	engine.options.Workspace = t.TempDir()
	if _, err := engine.Execute(t.Context(), TurnRequest{Prompt: "分析"}, nil); err != nil {
		t.Fatal(err)
	}
	state := engine.context.Conversation()
	source := state.SourcesForTurn(1)[0]
	if err := state.Select(agentcontext.NewConversationSelection(nil, nil, 2, "clear", "切换任务"), nil); err != nil {
		t.Fatal(err)
	}
	engine.context.SetConversation(state)
	// The source owner remains available even when the ordinary raw history
	// and full transcript archive are absent.
	engine.history = nil
	entry, err := engine.lookupTurnHistoryEntry(t.Context(), 1)
	if err != nil || entry == nil {
		t.Fatalf("source recovery failed: %v", err)
	}
	if !strings.Contains(entry.Transcript, "第二项定义") || !strings.Contains(entry.Transcript, "full turn transcript is unavailable") || !strings.Contains(entry.FindingsIndex, source.Items[1].ID) {
		t.Fatalf("source identity or scope missing: %+v", entry)
	}
	if err := state.Select(agentcontext.NewConversationSelection(nil, []string{source.Items[1].ID}, 3, "return", "回到第二项"), nil); err != nil {
		t.Fatal(err)
	}
	engine.options.TurnContexts = &withdrawalContextStore{withdrawn: true}
	if entry, err := engine.lookupTurnHistoryEntry(t.Context(), 1); err != nil || entry != nil {
		t.Fatal("source body bypassed withdrawal")
	}
}

func TestConversationSelectionReprojectsWithinTurnAndPersists(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{textStream("1. 锁范围定义\n2. 请求重试定义"), textStream("1. 另一份报告定义\n2. 分页游标定义")}}
	registry := tool.NewRegistry(nil, nil)
	engine := newEngine(t, runtime, registry)
	engine.options.Workspace = t.TempDir()
	engine.options.Context.RecentTailTurns = 1
	engine.options.Context.SemanticNarrative = "off"
	if err := interact.Register(registry, interact.Options{Workspace: engine.options.Workspace, OnPlan: engine.ApplyPlan}); err != nil {
		t.Fatal(err)
	}
	ctx := tool.WithInvocationIdentity(t.Context(), tool.InvocationIdentity{ThreadID: "thread"})
	for _, id := range []string{"report-a", "report-b"} {
		if _, err := engine.Execute(ctx, TurnRequest{TurnID: id, Prompt: "分析问题"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	state := engine.context.Conversation()
	if state == nil || len(state.Sources) != 2 {
		t.Fatal("terminal sources not committed without narrative")
	}
	var first agentcontext.ConversationSource
	for _, source := range state.Sources {
		if source.Turn == 1 {
			first = source
		}
	}
	args, _ := json.Marshal(map[string]any{"context_selection": interact.ContextSelection{ItemIDs: []string{first.Items[1].ID}}})
	runtime.streams = append(runtime.streams, toolCallStream("focus", "update_plan", string(args)), textStream("已经定位原报告第二项。"))
	var selectionResults int
	if _, err := engine.Execute(ctx, TurnRequest{TurnID: "followup", Prompt: "继续第一份报告第2项，先解释"}, func(event Event) error {
		if event.ToolCall != nil && event.ToolCall.Name == "update_plan" && event.Result != nil {
			selectionResults++
			if event.Result.IsError || event.Result.Metadata["plan_delta"] != false {
				t.Fatalf("selection created plan event: %+v", event.Result)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if selectionResults != 1 || len(runtime.requests) != 4 {
		t.Fatalf("selection results=%d requests=%d", selectionResults, len(runtime.requests))
	}
	before := joinMessageText(runtime.requests[2].Messages)
	if !strings.Contains(before, "请求重试定义") || !strings.Contains(before, "分页游标定义") {
		t.Fatal("ambiguous candidates not exposed")
	}
	after := joinMessageText(runtime.requests[3].Messages)
	if !strings.Contains(after, "请求重试定义") || strings.Contains(after, "分页游标定义") || strings.Contains(after, "锁范围定义") {
		t.Fatalf("focus did not immediately change references: %s", after)
	}
	if len(engine.currentPlan().Steps) != 0 {
		t.Fatal("focus created an execution obligation")
	}
	snapshot, err := engine.ExportContextSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Conversation.Selection.SourceTurnID != "followup" || snapshot.Conversation.Selection.UserRequestDigest == "" {
		t.Fatal("missing user provenance")
	}
	restored := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	restored.options.Workspace = engine.options.Workspace
	if _, err := restored.RestoreContextSnapshot(snapshot); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored.context.Conversation(), snapshot.Conversation) {
		t.Fatal("snapshot lost selection or immutable IDs")
	}
	fork, _, err := engine.ForkFromContextSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fork.context.Conversation(), snapshot.Conversation) {
		t.Fatal("fork lost sources")
	}
}

func TestConversationPlanIdentityAndInvalidReferencesAreAtomic(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	source := agentcontext.IndexConversationAnswer("thread", "report", 1, "1. 原始第一项\n2. 原始第二项")
	state := &agentcontext.ConversationState{}
	if err := state.Add(source); err != nil {
		t.Fatal(err)
	}
	engine.context.SetConversation(state)
	steps := []interact.PlanStep{
		{ID: "a", Title: "相同标题", Status: interact.StepPending, ReferenceItemIDs: []string{source.Items[0].ID}},
		{ID: "b", Title: "相同标题", Status: interact.StepPending, ReferenceItemIDs: []string{source.Items[1].ID}},
	}
	if err := engine.ApplyPlan(interact.Plan{Steps: steps}); err != nil {
		t.Fatal(err)
	}
	steps[1].Title = "第二项的新标题"
	steps[0].Status = interact.StepDone
	if err := engine.ApplyPlan(interact.Plan{Steps: []interact.PlanStep{steps[1], steps[0]}}); err != nil {
		t.Fatal(err)
	}
	plan := engine.currentPlan()
	if plan.Steps[0].ID != "b" || plan.Steps[0].ReferenceItemIDs[0] != source.Items[1].ID || plan.Steps[1].ID != "a" {
		t.Fatal("reorder changed identity")
	}
	steps[1].ReferenceItemIDs[0] = "foreign"
	if err := engine.ApplyPlan(interact.Plan{Steps: steps}); err == nil {
		t.Fatal("unknown source accepted")
	}
	if !reflect.DeepEqual(engine.currentPlan(), plan) {
		t.Fatal("rejected plan changed state or caller alias escaped")
	}
}

func TestConversationRequiredSourceRespectsCapacity(t *testing.T) {
	for _, selected := range []bool{false, true} {
		t.Run(map[bool]string{false: "all-too-large", true: "selected-item-fits"}[selected], func(t *testing.T) {
			runtime := &scriptedProvider{streams: []provider.Stream{textStream("done")}}
			engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
			engine.options.Workspace = t.TempDir()
			engine.options.Route = mustTestRouteWithContext(t, 4096)
			engine.options.MaxOutputTokens = 128
			engine.options.Context.RecentTailTurns = 1
			source := agentcontext.IndexConversationAnswer("thread", "report", 1, "1. "+strings.Repeat("long original definition ", 3000)+"\n2. 尾部完整定义")
			state := &agentcontext.ConversationState{}
			if err := state.Add(source); err != nil {
				t.Fatal(err)
			}
			if selected {
				if err := state.Select(agentcontext.NewConversationSelection(nil, []string{source.Items[1].ID}, 2, "select", "只处理第2项"), nil); err != nil {
					t.Fatal(err)
				}
			}
			if !selected {
				if err := state.Select(agentcontext.NewConversationSelection([]string{source.ID}, nil, 2, "select", "整个报告"), nil); err != nil {
					t.Fatal(err)
				}
			}
			engine.context.SetConversation(state)
			engine.turn = 2
			_, err := engine.Execute(t.Context(), TurnRequest{Prompt: "继续第2项"}, nil)
			if !selected {
				if err == nil || protocol.CodeOf(err) != protocol.CodeResourceExhausted || len(runtime.requests) != 0 {
					t.Fatalf("uncovered source admitted: err=%v requests=%d", err, len(runtime.requests))
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if len(runtime.requests) != 1 || !strings.Contains(joinMessageText(runtime.requests[0].Messages), "尾部完整定义") || strings.Contains(joinMessageText(runtime.requests[0].Messages), "long original definition") {
					t.Fatal("selected source was truncated or duplicated")
				}
			}
		})
	}
}
