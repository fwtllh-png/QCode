package engine

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/interact"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

// Exercise the actual measured provider request with both raw and recovered
// references. Background stays available to stateless providers on every
// sample, but never follows the current user or the latest tool result.
func TestRuntimeBackgroundPreservesToolLoopPrefixAndRecovery(t *testing.T) {
	for _, recentTurns := range []int{0, 1} {
		t.Run(fmt.Sprint(recentTurns), func(t *testing.T) {
			runtime := &scriptedProvider{}
			registry := tool.NewRegistry(nil, nil)
			if err := registry.Register(&echoTool{}); err != nil {
				t.Fatal(err)
			}
			engine, err := newTestEngine(Options{
				ProviderConfig: ProviderConfig{Provider: runtime, Route: mustTestRouteWithContext(t, 128<<10), MaxOutputTokens: 128},
				ToolConfig:     ToolConfig{Tools: registry, Authorize: func(provider.ToolCall) bool { return true }},
				SecurityConfig: SecurityConfig{Workspace: t.TempDir()},
				ContextConfig:  ContextConfig{Context: ContextPolicy{RecentTailTurns: recentTurns, SemanticNarrative: "off"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			source := agentcontext.IndexConversationAnswer("thread", "report", 1, "1. 保留原始问题定义\n2. 保留验证范围")
			state := &agentcontext.ConversationState{}
			if err := state.Add(source); err != nil {
				t.Fatal(err)
			}
			engine.context.SetConversation(state)
			engine.turn = 1
			engine.history = []provider.Message{
				messageWithText(provider.RoleUser, "分析问题", 1),
				messageWithText(provider.RoleAssistant, source.Text, 1),
			}
			checkpoint, err := agentcontext.RenderTurnCheckpoint(agentcontext.CheckpointRenderInput{
				Turn: 1, Status: agentcontext.CheckpointCompleted, Goal: "分析问题", Budget: 1024,
			})
			if err != nil {
				t.Fatal(err)
			}
			engine.turnCheckpoints = []agentcontext.TurnCheckpoint{checkpoint}

			for _, restored := range []bool{false, true} {
				if restored {
					snapshot, err := engine.ExportContextSnapshot()
					if err != nil {
						t.Fatal(err)
					}
					options := engine.options
					runtime = &scriptedProvider{}
					options.Provider = runtime
					engine, err = newTestEngine(options)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := engine.RestoreContextSnapshot(snapshot); err != nil {
						t.Fatal(err)
					}
				}
				runtime.streams = []provider.Stream{
					toolCallStream(fmt.Sprintf("probe-a-%t", restored), "echo", `{"text":"first result"}`),
					toolCallStream(fmt.Sprintf("probe-b-%t", restored), "echo", `{"text":"second result"}`),
					textStream("验证已完成。"),
				}
				prompt := fmt.Sprintf("继续验证原始问题，restored=%t", restored)
				var contexts []protocol.SampleContextData
				_, err := engine.Execute(t.Context(), TurnRequest{Prompt: prompt}, func(event Event) error {
					if event.InputContext != nil {
						contexts = append(contexts, *event.InputContext)
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				if len(runtime.requests) != 3 || len(contexts) != 3 {
					t.Fatalf("requests=%d measurements=%d", len(runtime.requests), len(contexts))
				}
				for i, request := range runtime.requests {
					assertRuntimeBackgroundBeforeUser(t, request.Messages, prompt, recentTurns == 1)
					if !strings.Contains(joinMessageText(request.Messages), "保留原始问题定义") {
						t.Fatal("background placement dropped the referenced definition")
					}
					if !strings.Contains(joinMessageText(request.Messages), "<qcode_turn_checkpoint>") {
						t.Fatal("background placement dropped the checkpoint")
					}
					if i > 0 {
						assertRequestMessagePrefix(t, runtime.requests[i-1], request)
						if !contexts[i].PrefixMonotonic || contexts[i].PairingVisibleOrphans != 0 ||
							request.Messages[len(request.Messages)-1].Role != provider.RoleTool {
							t.Fatalf("tool-loop prefix or pairing changed at sample %d", i)
						}
					}
				}
				for _, message := range engine.History() {
					if strings.Contains(message.Text(), "[conversation_references]") || strings.Contains(message.Text(), "[context_selection]") {
						t.Fatal("request-local background leaked into durable history")
					}
				}
			}
		})
	}
}

func TestRuntimeBackgroundRefreshesSelectedReferenceWithinTurn(t *testing.T) {
	runtime := &scriptedProvider{}
	registry := tool.NewRegistry(nil, nil)
	engine, err := newTestEngine(Options{
		ProviderConfig: ProviderConfig{Provider: runtime, Route: mustTestRouteWithContext(t, 128<<10), MaxOutputTokens: 128},
		ToolConfig:     ToolConfig{Tools: registry, Authorize: func(provider.ToolCall) bool { return true }},
		SecurityConfig: SecurityConfig{Workspace: t.TempDir()},
		ContextConfig:  ContextConfig{Context: ContextPolicy{RecentTailTurns: 1, SemanticNarrative: "off"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := interact.Register(registry, interact.Options{Workspace: engine.options.Workspace, OnPlan: engine.ApplyPlan}); err != nil {
		t.Fatal(err)
	}
	state := &agentcontext.ConversationState{}
	first := agentcontext.IndexConversationAnswer("thread", "first", 1, "1. 第一份保留项\n2. 第一份选中项")
	second := agentcontext.IndexConversationAnswer("thread", "second", 2, "1. 第二份保留项\n2. 第二份未选项")
	for _, source := range []agentcontext.ConversationSource{first, second} {
		if err := state.Add(source); err != nil {
			t.Fatal(err)
		}
		engine.history = append(engine.history,
			messageWithText(provider.RoleUser, "分析问题", source.Turn),
			messageWithText(provider.RoleAssistant, source.Text, source.Turn))
	}
	engine.turn = 2
	engine.context.SetConversation(state)
	args, err := json.Marshal(map[string]any{"context_selection": interact.ContextSelection{ItemIDs: []string{first.Items[1].ID}}})
	if err != nil {
		t.Fatal(err)
	}
	runtime.streams = []provider.Stream{toolCallStream("focus", "update_plan", string(args)), textStream("已定位第一份选中项。")}
	const prompt = "继续第一份报告第二项"
	ctx := tool.WithInvocationIdentity(t.Context(), tool.InvocationIdentity{ThreadID: "thread"})
	if _, err := engine.Execute(ctx, TurnRequest{TurnID: "selection", Prompt: prompt}, nil); err != nil {
		t.Fatal(err)
	}
	if len(runtime.requests) != 2 {
		t.Fatalf("requests=%d", len(runtime.requests))
	}
	before, after := joinMessageText(runtime.requests[0].Messages), joinMessageText(runtime.requests[1].Messages)
	if !strings.Contains(before, "第二份未选项") || !strings.Contains(after, "第一份选中项") || strings.Contains(after, "第二份未选项") {
		t.Fatalf("reference selection did not replace background: before second=%t, after first=%t second=%t; selection=%+v", strings.Contains(before, "第二份未选项"), strings.Contains(after, "第一份选中项"), strings.Contains(after, "第二份未选项"), engine.context.Conversation().Selection)
	}
	if reflect.DeepEqual(runtime.requests[0].Messages, runtime.requests[1].Messages) {
		t.Fatal("selection reused stale input")
	}
	for _, request := range runtime.requests {
		assertRuntimeBackgroundBeforeUser(t, request.Messages, prompt, true)
	}
}

func assertRuntimeBackgroundBeforeUser(t *testing.T, messages []provider.Message, prompt string, omitted bool) {
	t.Helper()
	user := -1
	counts := map[string]int{}
	for i, message := range messages {
		if message.Role == provider.RoleUser && message.Text() == prompt {
			user = i
		}
		for _, marker := range []string{"[conversation_references]", "[context_selection]", "<qcode_turn_checkpoint>"} {
			if strings.HasPrefix(message.Text(), marker) {
				counts[marker]++
				if user != -1 || message.Role != provider.RoleSystem {
					t.Fatalf("background %s followed the current request or changed role", marker)
				}
			}
		}
	}
	if user == -1 || counts["[conversation_references]"] != 1 || omitted && counts["[context_selection]"] != 1 {
		t.Fatalf("missing user or duplicated/missing background: user=%d counts=%v", user, counts)
	}
}
