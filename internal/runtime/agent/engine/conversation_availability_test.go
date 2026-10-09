package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type selectiveWithdrawalStore struct {
	withdrawalContextStore
	denied  map[protocol.TurnID]bool
	readErr error
}

func (s *selectiveWithdrawalStore) TurnWithdrawn(_ context.Context, _ protocol.ThreadID, turn protocol.TurnID) (bool, error) {
	return s.denied[turn], s.readErr
}

func TestConversationAvailabilityDropsOptionalSourceAcrossProjections(t *testing.T) {
	for _, unavailable := range []string{"withdrawn", "foreign", "mid-turn"} {
		t.Run(unavailable, func(t *testing.T) {
			runtime := &scriptedProvider{streams: []provider.Stream{toolCallStream("inspect", "echo", `{"text":"ok"}`), textStream("done")}}
			registry := tool.NewRegistry(nil, nil)
			if err := registry.Register(&echoTool{}); err != nil {
				t.Fatal(err)
			}
			e := newEngine(t, runtime, registry)
			e.options.Route = mustTestRouteWithContext(t, 8192)
			e.options.Routes, _ = model.NewRouteSet(e.options.Route, nil, false)
			e.options.SessionID = "session"
			e.turn = 2
			bad := agentcontext.IndexConversationAnswer("thread", "bad", 1, "1. unavailable private definition")
			good := agentcontext.IndexConversationAnswer("thread", "good", 2, "1. available definition")
			state := &agentcontext.ConversationState{}
			for _, source := range []agentcontext.ConversationSource{bad, good} {
				if err := state.Add(source); err != nil {
					t.Fatal(err)
				}
			}
			e.context.SetConversation(state)
			e.history = []provider.Message{messageWithText(provider.RoleUser, "unavailable original request", 1), messageWithText(provider.RoleAssistant, bad.Text, 1), messageWithText(provider.RoleUser, "valid prior request", 2), messageWithText(provider.RoleAssistant, good.Text, 2)}
			checkpoint, err := agentcontext.RenderTurnCheckpoint(agentcontext.CheckpointRenderInput{Turn: 1, Status: agentcontext.CheckpointCompleted, Goal: "unavailable private checkpoint", Budget: 4096})
			if err != nil {
				t.Fatal(err)
			}
			e.turnCheckpoints = []agentcontext.TurnCheckpoint{checkpoint}
			store := &selectiveWithdrawalStore{denied: map[protocol.TurnID]bool{"bad": unavailable != "mid-turn"}}
			if unavailable != "foreign" {
				e.options.TurnContexts = store
			} else {
				e.options.SessionForTurn = func(_ context.Context, id string) (string, bool) {
					if id == "bad" {
						return "foreign", true
					}
					return "session", true
				}
			}
			if _, err := e.RunForTurn(t.Context(), "current", "inspect current task", func(event Event) error {
				if unavailable == "mid-turn" && event.ToolCall != nil && event.Result != nil {
					store.denied["bad"] = true
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			for index, request := range runtime.requests {
				text := joinMessageText(request.Messages)
				if unavailable == "mid-turn" && index == 0 {
					if !strings.Contains(text, "unavailable private definition") {
						t.Fatal("initial available source missing")
					}
					continue
				}
				if strings.Contains(text, "unavailable private") || strings.Contains(text, "unavailable original") || !strings.Contains(text, "available definition") || !strings.Contains(text, "inspect current task") {
					t.Fatalf("invalid availability projection: %s", text)
				}
			}
			if _, found := e.context.Conversation().Sources[bad.ID]; !found {
				t.Fatal("projection mutated durable source index")
			}
		})
	}
}

func TestConversationAvailabilityRejectsRequiredSourcesAndStoreErrors(t *testing.T) {
	for _, binding := range []string{"selection", "plan", "store-error"} {
		t.Run(binding, func(t *testing.T) {
			e := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
			source := agentcontext.IndexConversationAnswer("thread", "bad", 1, "1. required definition")
			state := &agentcontext.ConversationState{}
			if err := state.Add(source); err != nil {
				t.Fatal(err)
			}
			var plan agentcontext.Plan
			store := &selectiveWithdrawalStore{denied: map[protocol.TurnID]bool{"bad": true}}
			if binding == "selection" {
				selection := agentcontext.NewConversationSelection(nil, []string{source.Items[0].ID}, 2, "current", "continue")
				state.Selection = &selection
			}
			if binding == "plan" {
				plan.Steps = []agentcontext.PlanStep{{ID: "step", Title: "required step", Status: "pending", ReferenceItemIDs: []string{source.Items[0].ID}}}
			}
			if binding == "store-error" {
				store.readErr = errors.New("storage unavailable")
			}
			e.options.TurnContexts = store
			if _, _, _, err := e.availableConversation(t.Context(), state, plan); err == nil {
				t.Fatal("required source or storage failure silently ignored")
			}
		})
	}
}
