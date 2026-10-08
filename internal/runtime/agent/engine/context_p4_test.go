package engine

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	providerfixture "github.com/fwtllh-png/QCode/internal/adapter/provider/fixture"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/interact"
	turnhistory "github.com/fwtllh-png/QCode/internal/adapter/tool/turnhistory"
	"github.com/fwtllh-png/QCode/internal/common/contextsnapshot"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestContextContinuityP4LongCheckpointHistoryKeepsDefinition(t *testing.T) {
	for _, window := range []uint64{8192, 16384} {
		t.Run(fmt.Sprint(window), func(t *testing.T) {
			runtime := &scriptedProvider{streams: []provider.Stream{textStream("done")}}
			e := newEngine(t, runtime, tool.NewRegistry(nil, nil))
			e.options.Workspace = t.TempDir()
			e.options.Route = mustTestRouteWithContext(t, window)
			e.options.Routes, _ = model.NewRouteSet(e.options.Route, nil, false)
			e.options.MaxOutputTokens = 128
			e.options.Context.SemanticNarrative = "off"
			e.options.Context.CheckpointMaxBytes = 512
			e.turn = 1000
			source := agentcontext.IndexConversationAnswer("thread", "report", 1, "1. 第一项\n2. 必须保留原定义与编号的尾项")
			conversation := &agentcontext.ConversationState{}
			if err := conversation.Add(source); err != nil {
				t.Fatal(err)
			}
			if err := conversation.Select(agentcontext.NewConversationSelection(nil, []string{source.Items[1].ID}, 1000, "select", "继续第二项"), nil); err != nil {
				t.Fatal(err)
			}
			e.context.SetConversation(conversation)
			for turn := uint64(1); turn <= 1000; turn++ {
				cp, err := agentcontext.RenderTurnCheckpoint(agentcontext.CheckpointRenderInput{Turn: turn, Status: agentcontext.CheckpointCompleted, Goal: strings.Repeat("old task ", 20), Budget: 512})
				if err != nil {
					t.Fatal(err)
				}
				e.turnCheckpoints = append(e.turnCheckpoints, cp)
			}
			before := agentcontext.CloneTurnCheckpoints(e.turnCheckpoints)
			if _, err := e.RunForTurn(t.Context(), "continue", "继续第二项", nil); err != nil {
				t.Fatal(err)
			}
			if len(runtime.requests) != 1 {
				t.Fatal("unexpected recovery calls")
			}
			text := joinMessageText(runtime.requests[0].Messages)
			if !strings.Contains(text, "必须保留原定义与编号的尾项") || !strings.Contains(text, source.Items[1].ID) {
				t.Fatal("pressure lost selected definition")
			}
			bytes := 0
			for _, message := range runtime.requests[0].Messages {
				if strings.Contains(message.Text(), agentcontext.CheckpointMarkerStart) {
					bytes += len(message.Text())
				}
			}
			if bytes > 512 {
				t.Fatalf("checkpoint projection grew to %d bytes", bytes)
			}
			if !reflect.DeepEqual(before, e.turnCheckpoints[:1000]) {
				t.Fatal("sampling rewrote durable checkpoints")
			}
		})
	}
}

func TestContextContinuityP4RecoversExactItemAndCompleteArchive(t *testing.T) {
	e := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	source := agentcontext.IndexConversationAnswer("thread", "report", 7, "1. unrelated definition\n2. 精确恢复的中文定义\n3. another unrelated item")
	state := &agentcontext.ConversationState{}
	if err := state.Add(source); err != nil {
		t.Fatal(err)
	}
	e.context.SetConversation(state)
	entry, err := e.lookupConversationEntry(t.Context(), turnhistory.ReferenceRequest{ItemID: source.Items[1].ID})
	if err != nil || entry == nil || !strings.Contains(entry.Transcript, "精确恢复的中文定义") || strings.Contains(entry.Transcript, "unrelated definition") {
		t.Fatalf("item recovery: %+v %v", entry, err)
	}
	if entry, err := e.lookupConversationEntry(t.Context(), turnhistory.ReferenceRequest{SourceID: "another-thread-source"}); err != nil || entry != nil {
		t.Fatal("foreign source was readable")
	}
	e.turnIDs["report"] = 7
	e.history = []provider.Message{messageWithText(provider.RoleAssistant, "partial fragment", 7)}
	e.options.TurnTranscriptArchive = &archiveFixture{turnID: "report", history: []provider.Message{messageWithText(provider.RoleUser, "complete archived request", 7), messageWithText(provider.RoleAssistant, source.Text, 7)}}
	entry, err = e.lookupTurnHistoryEntry(t.Context(), 7)
	if err != nil || entry == nil || !strings.Contains(entry.Transcript, "complete archived request") || strings.Contains(entry.Transcript, "partial fragment") {
		t.Fatalf("partial memory hid archive: %+v %v", entry, err)
	}
	e.options.TurnTranscriptArchive = nil
	entry, err = e.lookupTurnHistoryEntry(t.Context(), 7)
	if err != nil || entry == nil || !strings.Contains(entry.Transcript, "completeness is not established") {
		t.Fatal("memory fragment advertised complete transcript")
	}
	e.options.TurnContexts = &withdrawalContextStore{withdrawn: true}
	if entry, err := e.lookupConversationEntry(t.Context(), turnhistory.ReferenceRequest{ItemID: source.Items[1].ID}); err != nil || entry != nil {
		t.Fatal("withdrawn source was readable")
	}
	if entry, err := e.lookupTurnHistoryEntry(t.Context(), 7); err != nil || entry != nil {
		t.Fatal("withdrawn memory was readable")
	}
}

func TestContextContinuityP4ParentExportsOnlyBoundDefinitions(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{textStream("1. 第一项\n2. 已绑定定义"), textStream("done")}}
	e := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	e.options.Workspace = t.TempDir()
	if _, err := e.RunForTurn(t.Context(), "report", "分析", nil); err != nil {
		t.Fatal(err)
	}
	state := e.context.Conversation()
	source := state.SourcesForTurn(1)[0]
	if err := state.Select(agentcontext.NewConversationSelection(nil, []string{source.Items[1].ID}, 1, "report", "第二项"), nil); err != nil {
		t.Fatal(err)
	}
	e.context.SetConversation(state)
	snapshot, err := e.ParentContextSnapshot(contextsnapshot.SourceRef{TurnID: "report"})
	if err != nil || len(snapshot.References) != 1 || !strings.Contains(snapshot.References[0].Text, "已绑定定义") || strings.Contains(snapshot.References[0].Text, "第一项") {
		t.Fatalf("parent references: %+v %v", snapshot.References, err)
	}
}

func TestContextContinuityP4RestartRetainsAcceptedFocusAndPlan(t *testing.T) {
	var itemID string
	providerB, engineB, _, _ := restartTestEngines(t, "workspace-restart", func(e *Engine, p *scriptedProvider) {
		source := agentcontext.IndexConversationAnswer("thread", "report", 1, "1. 第一项\n2. 重启后必须保留的定义")
		state := &agentcontext.ConversationState{}
		if err := state.Add(source); err != nil {
			t.Fatal(err)
		}
		e.context.SetConversation(state)
		itemID = source.Items[1].ID
		if err := interact.Register(e.options.Tools, interact.Options{OnPlan: e.ApplyPlan}); err != nil {
			t.Fatal(err)
		}
		args, _ := json.Marshal(map[string]any{"context_selection": interact.ContextSelection{ItemIDs: []string{itemID}}, "steps": []interact.PlanStep{{ID: "active", Title: "解释第二项", Status: interact.StepInProgress, ReferenceItemIDs: []string{itemID}}}})
		p.streams[0] = toolCallStream("focus", "update_plan", string(args))
	})
	if _, err := engineB.RunForTurn(t.Context(), "turn-restart", "inspect the parser", nil); err != nil {
		t.Fatal(err)
	}
	text := joinMessageText(providerB.requests[0].Messages)
	if !strings.Contains(text, "重启后必须保留的定义") || !strings.Contains(text, itemID) {
		t.Fatal("restart lost accepted focus")
	}
	plan := engineB.currentPlan()
	if len(plan.Steps) != 1 || plan.Steps[0].ID != "active" || !reflect.DeepEqual(plan.Steps[0].ReferenceItemIDs, []string{itemID}) {
		t.Fatalf("restart lost plan linkage: %+v", plan)
	}
}

func TestContextContinuityP4DirectoryRecoveryPrecedesBusinessTools(t *testing.T) {
	source := agentcontext.IndexConversationAnswer("thread", "report", 1, "# 报告\n共同前提必须保留\n1. "+strings.Repeat("long original definition ", 4000)+"\n2. 恢复并绑定后的完整定义")
	args, _ := json.Marshal(map[string]any{"context_selection": interact.ContextSelection{ItemIDs: []string{source.Items[len(source.Items)-1].ID}}})
	read, _ := json.Marshal(map[string]any{"item_id": source.Items[len(source.Items)-1].ID, "max_bytes": 4096})
	mixed := &providerfixture.SliceStream{Events: []provider.StreamEvent{
		{Type: provider.EventToolCallDelta, ToolCall: &provider.ToolCallFragment{Index: 0, ID: "bind", Name: "update_plan", Arguments: string(args)}},
		{Type: provider.EventToolCallDelta, ToolCall: &provider.ToolCallFragment{Index: 1, ID: "too-early", Name: "echo", Arguments: `{ "text":"blocked" }`}},
		{Type: provider.EventMessageStop, StopReason: provider.StopReasonToolUse},
	}}
	runtime := &scriptedProvider{streams: []provider.Stream{
		toolCallStream("empty-focus", "update_plan", `{"context_selection":{}}`),
		toolCallStream("before-read", "echo", `{"text":"blocked"}`),
		toolCallStream("read", "turn_history", string(read)), mixed,
		toolCallStream("after-bind", "echo", `{"text":"allowed"}`), textStream("done"),
	}}
	registry := tool.NewRegistry(nil, nil)
	echo := &echoTool{}
	if err := registry.Register(echo); err != nil {
		t.Fatal(err)
	}
	e := newEngine(t, runtime, registry)
	e.options.Workspace = t.TempDir()
	e.options.Route = mustTestRouteWithContext(t, 16384)
	e.options.Routes, _ = model.NewRouteSet(e.options.Route, nil, false)
	e.options.Context.SemanticNarrative = "off"
	if err := interact.Register(registry, interact.Options{OnPlan: e.ApplyPlan}); err != nil {
		t.Fatal(err)
	}
	state := &agentcontext.ConversationState{}
	if err := state.Add(source); err != nil {
		t.Fatal(err)
	}
	e.context.SetConversation(state)
	var blocked int
	if _, err := e.RunForTurn(t.Context(), "continue", "继续第二项", func(event Event) error {
		if event.ToolCall != nil && event.ToolCall.ID == "empty-focus" && event.Result != nil && !event.Result.IsError {
			t.Fatal("empty focus bypassed required definition recovery")
		}
		if event.Result != nil && event.Result.Metadata["error_category"] == "context_reference_required" {
			blocked++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if blocked != 2 || echo.calls.Load() != 1 || len(runtime.requests) != 6 {
		t.Fatalf("recovery execution: blocked=%d calls=%d requests=%d", blocked, echo.calls.Load(), len(runtime.requests))
	}
	for i, request := range runtime.requests {
		advertised := false
		for _, definition := range request.Tools {
			if definition.Name == "echo" {
				advertised = true
			}
		}
		if advertised != (i >= 4) {
			t.Fatalf("request %d advertised business tool=%v", i, advertised)
		}
		if i < 4 && !requestContains(request, "[conversation_catalog]") {
			t.Fatalf("request %d lost recovery state", i)
		}
	}
	if !requestContainsToolResult(runtime.requests[3], "恢复并绑定后的完整定义") || !requestContains(runtime.requests[4], source.Items[len(source.Items)-1].ID) {
		t.Fatal("precise recovery and binding did not reach the next sample")
	}
}

func TestContextContinuityP4ForkSelectionAndModelChange(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{textStream("done")}}
	e := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	e.options.Workspace = t.TempDir()
	e.options.Route = mustTestRouteWithContext(t, 32768)
	e.options.Routes, _ = model.NewRouteSet(e.options.Route, nil, false)
	e.options.Context.SemanticNarrative = "off"
	source := agentcontext.IndexConversationAnswer("thread", "report", 1, "1. 父线程定义\n2. 子线程切换模型后仍然完整的定义")
	state := &agentcontext.ConversationState{}
	if err := state.Add(source); err != nil {
		t.Fatal(err)
	}
	if err := state.Select(agentcontext.NewConversationSelection(nil, []string{source.Items[0].ID}, 1, "report", "第一项"), nil); err != nil {
		t.Fatal(err)
	}
	e.context.SetConversation(state)
	snapshot, err := e.ExportContextSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	fork, _, err := e.ForkFromContextSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	forkState := fork.context.Conversation()
	if err := forkState.Select(agentcontext.NewConversationSelection(nil, []string{source.Items[1].ID}, 2, "child", "第二项"), nil); err != nil {
		t.Fatal(err)
	}
	fork.context.SetConversation(forkState)
	fork.options.Route = mustTestRouteWithContext(t, 4096)
	fork.options.Routes, _ = model.NewRouteSet(fork.options.Route, nil, false)
	fork.options.MaxOutputTokens = 256
	if _, err := fork.RunForTurn(t.Context(), "child", "继续第二项", nil); err != nil {
		t.Fatal(err)
	}
	if len(runtime.requests) != 1 || !requestContains(runtime.requests[0], source.Items[1].ID) || !requestContains(runtime.requests[0], "子线程切换模型后仍然完整的定义") {
		t.Fatal("model change lost immutable reference or definition")
	}
	if got := e.context.Conversation().Selection.ItemIDs; !reflect.DeepEqual(got, []string{source.Items[0].ID}) {
		t.Fatal("fork focus mutated parent")
	}
}

func TestContextContinuityP4RestartRejectsMissingSourceOrRevision(t *testing.T) {
	for _, failure := range []string{"missing_source", "revision", "epoch"} {
		t.Run(failure, func(t *testing.T) {
			p, e, _, _ := restartTestEngines(t, "workspace-restart", func(e *Engine, p *scriptedProvider) {
				source := agentcontext.IndexConversationAnswer("thread", "report", 1, "1. 续跑必需定义")
				state := &agentcontext.ConversationState{}
				if err := state.Add(source); err != nil {
					t.Fatal(err)
				}
				e.context.SetConversation(state)
			})
			switch failure {
			case "missing_source":
				store := e.options.TurnContinuations.(*mapBlobStore)
				store.mu.Lock()
				for key, data := range store.data {
					if strings.Contains(string(data), "续跑必需定义") {
						delete(store.data, key)
					}
				}
				store.mu.Unlock()
			case "revision":
				e.sessionRevision++
			case "epoch":
				e.stateEpoch = 2
			}
			if _, err := e.RunForTurn(t.Context(), "turn-restart", "inspect the parser", nil); err == nil {
				t.Fatal("resumed with unavailable accepted source state")
			}
			if len(p.requests) != 0 {
				t.Fatal("provider sampled after failed context recovery")
			}
		})
	}
}

func TestContextContinuityP4PressureAdmission(t *testing.T) {
	for _, mode := range []string{"hard", "economic", "observed"} {
		t.Run(mode, func(t *testing.T) {
			runtime := &scriptedProvider{streams: []provider.Stream{usageStream("done", provider.Usage{OutputTokens: 1})}}
			e := newEngine(t, runtime, tool.NewRegistry(nil, nil))
			e.options.Workspace = t.TempDir()
			e.options.Route = mustTestRouteWithContext(t, 4096)
			e.options.Routes, _ = model.NewRouteSet(e.options.Route, nil, false)
			e.options.Context.SemanticNarrative = "off"
			e.options.Context.RecentTailTurns = 0
			if mode == "economic" {
				e.options.Budget.MaxTurnTokens = 3000
			}
			if mode == "observed" {
				window := e.context.Window()
				window.Observe(protocol.SampleContextData{ContextDigest: "previous", EstimatedTokens: 1000}, 2000, 0)
				e.context.SetWindow(window)
			}
			source := agentcontext.IndexConversationAnswer("thread", "report", 1, "1. 无关定义\n2. 压力下必须完整提供的当前定义")
			state := &agentcontext.ConversationState{}
			if err := state.Add(source); err != nil {
				t.Fatal(err)
			}
			if err := state.Select(agentcontext.NewConversationSelection(nil, []string{source.Items[1].ID}, 1, "report", "第二项"), nil); err != nil {
				t.Fatal(err)
			}
			e.context.SetConversation(state)
			for turn := uint64(1); turn <= 8; turn++ {
				e.history = append(e.history, messageWithText(provider.RoleUser, "old request", turn), messageWithText(provider.RoleAssistant, strings.Repeat("old explanation ", 1000), turn))
			}
			e.turn = 8
			cp, err := agentcontext.RenderTurnCheckpoint(agentcontext.CheckpointRenderInput{Turn: 8, Status: agentcontext.CheckpointCompleted, Goal: strings.Repeat("checkpoint text ", 1000), Budget: 6000})
			if err != nil {
				t.Fatal(err)
			}
			e.turnCheckpoints = []agentcontext.TurnCheckpoint{cp}
			var sample *protocol.SampleContextData
			if _, err := e.RunForTurn(t.Context(), "continue", "继续第2项，保留当前约束", func(event Event) error {
				if event.SampleContext != nil {
					copy := *event.SampleContext
					sample = &copy
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if len(runtime.requests) != 1 || sample == nil {
				t.Fatal("missing provider sample")
			}
			request := runtime.requests[0]
			if !requestContains(request, "压力下必须完整提供的当前定义") || !requestContains(request, "保留当前约束") || !requestContains(request, source.Items[1].ID) {
				t.Fatal("pressure discarded required context")
			}
			if sample.WindowFullActiveTokens+request.MaxOutputTokens > request.Route.Model().Limits.ContextTokens {
				t.Fatal("optional checkpoint exceeded capacity")
			}
			if mode == "economic" && sample.WindowFullActiveTokens+request.MaxOutputTokens > e.options.Budget.MaxTurnTokens {
				t.Fatal("optional checkpoint exceeded economic budget")
			}
		})
	}
}

func TestContextContinuityP4SamplingRejectsUnavailableSource(t *testing.T) {
	p := &scriptedProvider{streams: []provider.Stream{textStream("must not run")}}
	e := newEngine(t, p, tool.NewRegistry(nil, nil))
	e.options.Workspace = t.TempDir()
	state := &agentcontext.ConversationState{}
	source := agentcontext.IndexConversationAnswer("thread", "withdrawn", 1, "1. 已撤回的问题定义")
	if err := state.Add(source); err != nil {
		t.Fatal(err)
	}
	if err := state.Select(agentcontext.NewConversationSelection(nil, []string{source.Items[0].ID}, 1, "withdrawn", "第一项"), nil); err != nil {
		t.Fatal(err)
	}
	e.context.SetConversation(state)
	e.options.TurnContexts = &withdrawalContextStore{withdrawn: true}
	if _, err := e.RunForTurn(t.Context(), "continue", "继续第一项", nil); err == nil {
		t.Fatal("sampled withdrawn definition")
	}
	if len(p.requests) != 0 {
		t.Fatal("withdrawn definition reached provider")
	}
}

func TestContextContinuityP4ForkToolCallbacksStayWithChild(t *testing.T) {
	registry := tool.NewRegistry(nil, nil)
	runtime := &scriptedProvider{}
	parent := newEngine(t, runtime, registry)
	parent.options.Workspace = t.TempDir()
	parent.options.Route = mustTestRouteWithContext(t, 8192)
	parent.options.Routes, _ = model.NewRouteSet(parent.options.Route, nil, false)
	parent.setPlan(agentcontext.Plan{Steps: []agentcontext.PlanStep{{ID: "parent", Title: "parent plan", Status: interact.StepDone}}})
	if err := interact.Register(registry, interact.Options{OnPlan: parent.ApplyPlan}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := parent.ExportContextSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	child, _, err := parent.ForkFromContextSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	child.options.Route = mustTestRouteWithContext(t, 8192)
	child.options.Routes, _ = model.NewRouteSet(child.options.Route, nil, false)
	source := agentcontext.IndexConversationAnswer("child", "child-report", 1, "1. 只属于子线程的完整定义")
	state := &agentcontext.ConversationState{}
	if err := state.Add(source); err != nil {
		t.Fatal(err)
	}
	child.context.SetConversation(state)
	read, _ := json.Marshal(map[string]any{"item_id": source.Items[0].ID, "max_bytes": 4096})
	update, _ := json.Marshal(map[string]any{
		"context_selection": interact.ContextSelection{ItemIDs: []string{source.Items[0].ID}},
		"steps":             []interact.PlanStep{{ID: "child", Title: "child plan", Status: interact.StepDone, ReferenceItemIDs: []string{source.Items[0].ID}}},
	})
	runtime.streams = []provider.Stream{toolCallStream("read-child", "turn_history", string(read)), toolCallStream("bind-child", "update_plan", string(update)), textStream("done")}
	results := 0
	if _, err := child.RunForTurn(t.Context(), "child-continue", "继续子线程事项", func(event Event) error {
		if event.ToolCall != nil && event.Result != nil {
			results++
			if event.Result.IsError {
				t.Fatalf("fork tool used wrong runtime: %s", event.Result.Content)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if results != 2 || !requestContainsToolResult(runtime.requests[1], "只属于子线程的完整定义") {
		t.Fatal("child recovery did not use child source")
	}
	if child.currentPlan().Steps[0].ID != "child" || child.context.Conversation().Selection.ItemIDs[0] != source.Items[0].ID {
		t.Fatal("child plan/focus not applied")
	}
	if parent.currentPlan().Steps[0].ID != "parent" || parent.context.Conversation() != nil {
		t.Fatal("child invocation changed parent context")
	}
}

func TestContextContinuityP4ForkKeepsArchivePointers(t *testing.T) {
	parent := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	parent.options.Workspace = t.TempDir()
	parent.options.TurnTranscriptArchive = &archiveFixture{turnID: "report", history: []provider.Message{messageWithText(provider.RoleAssistant, "complete parent transcript", 1)}}
	parent.turn = 1
	parent.historyTurns = map[string]uint64{"report": 1}
	parent.history = []provider.Message{messageWithText(provider.RoleAssistant, "partial", 1)}
	snapshot, err := parent.ExportContextSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	child, _, err := parent.ForkFromContextSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := child.lookupTurnHistoryEntry(t.Context(), 1)
	if err != nil || entry == nil || !strings.Contains(entry.Transcript, "complete parent transcript") || strings.Contains(entry.Transcript, "partial") {
		t.Fatalf("fork lost inherited archive pointer: %+v %v", entry, err)
	}
}
