package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type continuityBaselineFixture struct {
	SchemaVersion       int    `json:"schema_version"`
	Description         string `json:"description"`
	ContextTokens       uint64 `json:"context_tokens"`
	OutputReserveTokens uint64 `json:"output_reserve_tokens"`
	RecentTailTurns     int    `json:"recent_tail_turns"`
	Turns               []struct {
		Prompt             string `json:"prompt"`
		Answer             string `json:"answer"`
		SourceTurn         uint64 `json:"source_turn"`
		RequiredDefinition string `json:"required_definition"`
	} `json:"turns"`
}

// B1 now requires the original definition even with the positive turn ceiling.
func TestContextContinuityP2FollowupWithoutPressure(t *testing.T) {
	runContinuityProjectionFixture(t, false)
}

func TestContextContinuityP1CapacityFollowupWithoutPressure(t *testing.T) {
	runContinuityProjectionFixture(t, true)
}

func runContinuityProjectionFixture(t *testing.T, capacitySelection bool) {
	t.Helper()
	data, err := os.ReadFile("testdata/context_continuity_zh.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture continuityBaselineFixture
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.SchemaVersion != 2 || len(fixture.Turns) != 4 {
		t.Fatalf("unsupported continuity fixture: %+v", fixture)
	}
	for _, scenario := range []struct {
		mode  string
		turns int
	}{
		// The first two post-turn jobs currently have no omitted input. The
		// third request must retain its definition before any later job.
		{mode: "post_turn", turns: 3},
		// Continue through another item with optional generation disabled.
		{mode: "off", turns: len(fixture.Turns)},
	} {
		t.Run(scenario.mode, func(t *testing.T) {
			runtime := &scriptedProvider{}
			for _, turn := range fixture.Turns[:scenario.turns] {
				runtime.streams = append(runtime.streams, textStream(turn.Answer))
			}
			engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
			engine.options.Workspace = t.TempDir()
			engine.options.Route = mustTestRouteWithContext(t, fixture.ContextTokens)
			engine.options.MaxOutputTokens = fixture.OutputReserveTokens
			engine.options.Context.SemanticNarrative = scenario.mode
			engine.options.Context.Digest = "ledger+narrative"
			engine.options.Context.RecentTailTurns = fixture.RecentTailTurns
			if capacitySelection {
				engine.options.Context.RecentTailTurns = 0
			}
			engine.options.Context.RecentTailMaxTokens = 0
			engine.options.Context.Window = CompactWindowPolicy{}
			var compactions int
			emit := func(event Event) error {
				if event.State == Compacting {
					compactions++
				}
				return nil
			}
			for index, turn := range fixture.Turns[:scenario.turns] {
				turnID := fmt.Sprintf("continuity-turn-%d", index+1)
				if _, err := engine.Execute(t.Context(), TurnRequest{
					TurnID: turnID, Prompt: turn.Prompt,
				}, emit); err != nil {
					t.Fatalf("turn %d: %v", index+1, err)
				}
				if len(runtime.requests) != index+1 {
					t.Fatalf("turn %d made additional provider calls: %d", index+1, len(runtime.requests))
				}
				visible := joinMessageText(runtime.requests[index].Messages)
				if !strings.Contains(visible, turn.Prompt) {
					t.Fatalf("turn %d lost current user request (C1)", index+1)
				}
				if index > 0 && !strings.Contains(visible, fixture.Turns[index-1].Answer) {
					t.Fatalf("turn %d lost the most recent answer", index+1)
				}
				if turn.RequiredDefinition != "" {
					if turn.SourceTurn == 0 || turn.SourceTurn > uint64(index) ||
						!strings.Contains(fixture.Turns[turn.SourceTurn-1].Answer, turn.RequiredDefinition) ||
						strings.Contains(turn.Prompt, turn.RequiredDefinition) {
						t.Fatalf("turn %d has no unambiguous earlier definition", index+1)
					}
					present := strings.Contains(visible, turn.RequiredDefinition)
					if !present {
						t.Fatalf("B1 source definition missing at turn %d", index+1)
					}
					entry, err := engine.lookupTurnHistoryEntry(t.Context(), turn.SourceTurn)
					if err != nil || entry == nil || !strings.Contains(entry.FindingsIndex, turn.RequiredDefinition) {
						t.Fatalf("turn %d lost source findings: entry=%+v err=%v", index+1, entry, err)
					}
					if !present && !strings.Contains(visible, "[context_selection]") {
						t.Fatalf("turn %d also lost the source recovery hint", index+1)
					}
					if capacitySelection && strings.Contains(visible, "[context_selection]") {
						t.Fatalf("turn %d advertises omissions despite retaining all source history", index+1)
					}
					t.Logf("B1 capacity=%t turn=%d definition_visible=%t target=true source_turn=%d findings_intact=true", capacitySelection, index+1, present, turn.SourceTurn)
				}
				// Include the unprojected history (and this turn's answer) so
				// the fixture cannot confuse capacity pressure with selection.
				all := append(append([]provider.Message{}, engine.promptMessages()...), engine.History()...)
				fullEstimate := engine.estimateTokens(all)
				hard := engine.contextCapacity().HardInputTokens
				if fullEstimate >= hard || compactions != 0 {
					t.Fatalf("fixture under pressure: estimate=%d hard=%d compactions=%d", fullEstimate, hard, compactions)
				}
				t.Logf("B1 turn=%d unprojected_tokens=%d hard_input=%d compactions=%d", index+1, fullEstimate, hard, compactions)
				// Closed-turn sealing is required even when generation is off.
				if index < scenario.turns-1 {
					if job := engine.PreparePostTurnNarrative("continuity-thread", protocol.TurnID(turnID)); job != nil {
						t.Fatalf("B1 turn %d scheduled an unnecessary summary; evaluate C5 before updating", index+1)
					}
					if scenario.mode == "post_turn" && len(engine.contextProjection(engine.history).Omissions) != 0 {
						t.Fatalf("B1 turn %d unexpectedly has omitted summary input", index+1)
					}
				}
			}
		})
	}
}

// B3 is promoted to C7 acceptance: the actual selection owns recovery metadata.
func TestContextContinuityP1TokenCeilingOmission(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	engine.options.Workspace = t.TempDir()
	engine.options.Route = mustTestRouteWithContext(t, 128<<10)
	engine.options.Context.RecentTailTurns = 2
	engine.history = []provider.Message{
		messageWithText(provider.RoleUser, "请分析三个问题。", 1),
		messageWithText(provider.RoleAssistant, strings.Repeat("问题分析及对应证据。", 200), 1),
		messageWithText(provider.RoleUser, "继续优化第 2 个问题。", 2),
	}
	engine.turn = 2
	before := joinMessageText(engine.history)
	if visible := engine.projectSelectedHistoryForTest(nil)(engine.history); len(visible) != len(engine.history) {
		t.Fatal("fixture already omits history without the explicit token ceiling")
	}
	// A fixture input, not a proposed runtime default or model tier.
	engine.options.Context.RecentTailMaxTokens = 256
	visible := engine.projectSelectedHistoryForTest(nil)(engine.history)
	if len(visible) != 1 || visible[0].Turn != 2 || visible[0].Text() != engine.history[2].Text() {
		t.Fatalf("B3 token ceiling no longer drops exactly turn 1: %+v", visible)
	}
	projection := engine.contextProjection(engine.history)
	if len(projection.Omissions) != 2 {
		t.Fatalf("omissions disagree with selected history: %+v", projection)
	}
	for _, omission := range projection.Omissions {
		if omission.Reason != "history_token_ceiling" || omission.Retrieval == nil || omission.Retrieval.Turn != 1 {
			t.Fatalf("incorrect omission reason or recovery parameters: %+v", omission)
		}
	}
	if joinMessageText(engine.history) != before {
		t.Fatal("projection modified source history")
	}
	t.Log("B3 actual_omitted_turns=[1] reported_omitted_turns=[1] reason=history_token_ceiling retrieval={turn:1}")
}
