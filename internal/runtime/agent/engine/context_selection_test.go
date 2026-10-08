package engine

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	providerfixture "github.com/fwtllh-png/QCode/internal/adapter/provider/fixture"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	promptcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/prompt"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type selectionSchemaTool struct{ echoTool }

func (*selectionSchemaTool) Descriptor() tool.Descriptor {
	definition := (&echoTool{}).Descriptor()
	definition.InputSchema["description"] = strings.Repeat("schema detail ", 300)
	return definition
}

func TestContextSelectionFinalRequestAccountsForToolsAndHints(t *testing.T) {
	for _, reserve := range []uint64{128, 512} {
		t.Run(fmt.Sprint(reserve), func(t *testing.T) {
			runtime := &scriptedProvider{streams: []provider.Stream{&providerfixture.SliceStream{Events: []provider.StreamEvent{
				{Type: provider.EventTextDelta, Text: "done"},
				{Type: provider.EventUsage, Usage: &provider.Usage{OutputTokens: 1}},
				{Type: provider.EventMessageStop, StopReason: provider.StopReasonEndTurn},
			}}}}
			registry := tool.NewRegistry(nil, nil)
			if err := registry.Register(&selectionSchemaTool{}); err != nil {
				t.Fatal(err)
			}
			engine := newEngine(t, runtime, registry)
			engine.options.Workspace = t.TempDir()
			engine.options.Route = mustTestRouteWithContext(t, 4096)
			engine.options.MaxOutputTokens = reserve
			engine.options.Context.RecentTailTurns = 0
			engine.history = []provider.Message{
				messageWithText(provider.RoleUser, "first", 1),
				messageWithText(provider.RoleAssistant, strings.Repeat("old detail ", 500), 1),
				messageWithText(provider.RoleUser, "second", 2),
				messageWithText(provider.RoleAssistant, strings.Repeat("recent detail ", 300), 2),
			}
			engine.turn = 2
			var expected agentcontext.MessageSnapshot
			var measured protocol.SampleContextData
			var sampled *protocol.SampleContextData
			var folds int
			_, err := engine.Execute(t.Context(), TurnRequest{Prompt: "continue"}, func(event Event) error {
				if event.Compaction != nil && event.Compaction.Mode == "view" {
					folds++
				}
				if event.ModelExecution != nil && event.ModelExecution.Status == protocol.ProviderAttemptStarted {
					scope := engine.runningScope()
					scope.mu.Lock()
					snapshot := scope.state.contextLedger.Snapshot()
					scope.mu.Unlock()
					var err error
					expected, _, err = snapshot.Normalize(engine.activeRoute().Model().Capabilities)
					if err != nil {
						return err
					}
					measured, err = expected.Measure("normal", engine.reasoningEffort(), engine.options.TokenEstimator)
					return err
				}
				if event.SampleContext != nil {
					copy := *event.SampleContext
					sampled = &copy
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(runtime.requests) != 1 || sampled == nil {
				t.Fatalf("requests=%d context=%+v", len(runtime.requests), sampled)
			}
			request := runtime.requests[0]
			if !reflect.DeepEqual(request.Messages, expected.Messages()) || !reflect.DeepEqual(request.Tools, expected.Definitions()) {
				t.Fatal("sent request differs from the final measured snapshot")
			}
			if folds == 0 || measured.ToolDefinitionTokens == 0 || measured.DynamicTokens == 0 ||
				measured.EstimatedTokens != sampled.EstimatedTokens || sampled.ContextDigest != measured.ContextDigest ||
				sampled.ContextProjectionDigest == "" || sampled.WindowOutputReserve != request.MaxOutputTokens ||
				sampled.WindowFullActiveTokens+request.MaxOutputTokens > request.Route.Model().Limits.ContextTokens {
				t.Fatalf("incomplete final admission: folds=%d measured=%+v sampled=%+v", folds, measured, sampled)
			}
			joined := joinMessageText(request.Messages)
			if strings.Contains(joined, "old detail") || !strings.Contains(joined, "reason=context_capacity") ||
				!strings.Contains(joined, `turn_history {"turn":`) || !strings.Contains(joined, "continue") {
				t.Fatalf("fold and hint differ from request: %s", joined)
			}
		})
	}
}

func TestContextSelectionTokenOmissionReachesActualProviderRequest(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{textStream("done")}}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	engine.options.Workspace = t.TempDir()
	engine.options.Route = mustTestRouteWithContext(t, 128<<10)
	engine.options.Context.RecentTailTurns = 2
	engine.options.Context.RecentTailMaxTokens = 256
	engine.history = []provider.Message{
		messageWithText(provider.RoleUser, "analyze", 1),
		messageWithText(provider.RoleAssistant, strings.Repeat("omitted report ", 200), 1),
	}
	engine.turn = 1
	if _, err := engine.Execute(t.Context(), TurnRequest{Prompt: "继续第 2 项"}, nil); err != nil {
		t.Fatal(err)
	}
	if len(runtime.requests) != 1 {
		t.Fatalf("requests=%d", len(runtime.requests))
	}
	visible := joinMessageText(runtime.requests[0].Messages)
	if strings.Contains(visible, "omitted report") || !strings.Contains(visible, "turn=1 reason=history_token_ceiling") ||
		!strings.Contains(visible, `turn_history {"turn":1}`) || strings.Contains(visible, "preferred_turn") {
		t.Fatalf("incorrect actual provider omission hint: %s", visible)
	}
}

func TestContextSelectionNormalizesExplicitZeroAndRejectsNegative(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, nil)
	engine.options.Context.RecentTailTurns = 0
	if err := normalizeEngineOptions(&engine.options); err != nil || engine.recentTailTurns() != 0 {
		t.Fatalf("zero replaced or rejected: %v", err)
	}
	engine.options.Context.RecentTailTurns = -1
	if err := normalizeEngineOptions(&engine.options); err == nil {
		t.Fatal("negative turn limit accepted")
	}
}

func TestContextSelectionProviderOverflowRebuildsHint(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		&errorStream{err: protocol.NewProblem(protocol.CodeInvalidArgument, "context is too large", false,
			&provider.Failure{Code: provider.FailureContextWindowExceeded, Message: "context is too large"})},
		textStream("recovered"),
	}}
	engine := newEngine(t, runtime, nil)
	engine.options.Workspace = t.TempDir()
	engine.options.Context.RecentTailTurns = 0
	engine.options.MaxRetries = 1
	engine.history = []provider.Message{
		messageWithText(provider.RoleUser, strings.Repeat("first report ", 300), 1),
		messageWithText(provider.RoleUser, "second report", 2),
	}
	engine.turn = 2
	if _, err := engine.Execute(t.Context(), TurnRequest{Prompt: "continue"}, nil); err != nil {
		t.Fatal(err)
	}
	if len(runtime.requests) != 2 {
		t.Fatalf("requests=%d", len(runtime.requests))
	}
	before := joinMessageText(runtime.requests[0].Messages)
	after := joinMessageText(runtime.requests[1].Messages)
	if strings.Contains(before, "[context_selection]") || !strings.Contains(before, "first report") ||
		strings.Contains(after, "first report") || !strings.Contains(after, "second report") ||
		!strings.Contains(after, "turn=1 reason=provider_overflow") ||
		!strings.Contains(after, `turn_history {"turn":1}`) || !strings.Contains(after, "continue") ||
		!strings.Contains(engine.history[0].Text(), "first report") {
		t.Fatalf("retry selection mismatch: before=%s after=%s", before, after)
	}
}

func selectionProjector(t *testing.T, engine *Engine) agentcontext.HistoryProjector {
	t.Helper()
	return func(history []provider.Message) []provider.Message {
		selection := engine.contextProjection(history)
		hint, err := promptcontext.ContextSelectionHint(selection, engine.sessionStateBudget())
		if err != nil {
			t.Fatal(err)
		}
		if hint != nil {
			selection.Messages = append(selection.Messages, *hint)
		}
		return selection.Messages
	}
}

func TestContextSelectionFoldSearchIncludesHintCost(t *testing.T) {
	for _, beneficial := range []bool{true, false} {
		t.Run(fmt.Sprint(beneficial), func(t *testing.T) {
			engine := newEngine(t, &scriptedProvider{}, nil)
			engine.options.Context.RecentTailTurns = 0
			history := []provider.Message{messageWithText(provider.RoleUser, "tiny", 1)}
			if beneficial {
				history = append(history, messageWithText(provider.RoleUser, strings.Repeat("large older turn ", 100), 2))
			}
			history = append(history, messageWithText(provider.RoleUser, "current", 3))
			original := cloneMessages(history)
			project := selectionProjector(t, engine)
			before := project(history)
			input := agentcontext.NewMessageLedger(agentcontext.LedgerInput{
				History: before, Continuation: []provider.Message{provider.TextMessage(provider.RoleAssistant, "unfinished output")},
			}).Snapshot()
			window, err := engine.measureTokenWindow(input, 128, 0)
			if err != nil {
				t.Fatal(err)
			}
			// One tiny turn cannot pay for the newly required recovery hint.
			engine.foldOldestVisibleTail(history, agentcontext.OmittedCapacity)
			oneFold, err := engine.measureTokenWindow(input.WithHistory(project(history)), 128, 0)
			if err != nil || oneFold.total < window.total {
				t.Fatalf("fixture must grow after its first fold: before=%+v after=%+v err=%v", window, oneFold, err)
			}
			engine.resetViewFold()
			after, final, folded, err := engine.foldForNetReduction(history, input, 128, 0,
				agentcontext.OmittedCapacity, project, before, window)
			if err != nil || folded != beneficial || !reflect.DeepEqual(history, original) {
				t.Fatalf("folded=%t expected=%t err=%v history=%+v", folded, beneficial, err, history)
			}
			if beneficial {
				if final.total >= window.total || engine.viewFold.start != 2 ||
					!strings.Contains(joinMessageText(after), "turns=1-2 reason=context_capacity") ||
					!strings.Contains(joinMessageText(input.WithHistory(after).Messages()), "unfinished output") {
					t.Fatalf("incorrect net reduction: %+v %+v", final, after)
				}
			} else if engine.viewFold.start != 0 || engine.viewFold.folded || !reflect.DeepEqual(after, before) {
				t.Fatal("unsuccessful fold was not rolled back")
			}
		})
	}
}

// This fixture estimator gives unsupported image placeholders a visibly
// larger cost than their source block, exercising normalization before gating.
type selectionTextEstimator struct{}

func (selectionTextEstimator) Estimate(messages []provider.Message) (uint64, error) {
	var tokens uint64
	for _, message := range messages {
		tokens += uint64(len(message.Text()))
	}
	return tokens, nil
}

func TestContextSelectionGateMeasuresNormalizedContinuation(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, nil)
	engine.options.Route = mustTestRouteWithContext(t, 16384)
	engine.options.MaxOutputTokens = 128
	engine.options.Context.RecentTailTurns = 0
	engine.options.TokenEstimator = selectionTextEstimator{}
	history := []provider.Message{
		{Role: provider.RoleUser, Turn: 1, Blocks: []provider.ContentBlock{{
			Type: provider.ContentImage, Attachment: &provider.Attachment{
				Name: strings.Repeat("image-name", 2000), MediaType: "image/png", Data: []byte("image"),
			},
		}}},
		messageWithText(provider.RoleUser, "current", 2),
	}
	project := selectionProjector(t, engine)
	input := agentcontext.NewMessageLedger(agentcontext.LedgerInput{
		History: project(history), Continuation: []provider.Message{
			provider.TextMessage(provider.RoleAssistant, strings.Repeat("continuation ", 500)),
		},
	}).Snapshot()
	raw, err := input.Measure("", "", engine.options.TokenEstimator)
	if err != nil {
		t.Fatal(err)
	}
	before, err := engine.measureTokenWindow(input, 128, 0)
	if err != nil || engine.contextProjection(history).TailStart != 0 ||
		raw.EstimatedTokens+128 >= before.hardLimit || before.total <= before.hardLimit {
		t.Fatalf("fixture must exceed capacity only after normalization: raw=%+v normalized=%+v err=%v", raw, before, err)
	}
	window, err := engine.runCompactGate(t.Context(), &history, input, 128,
		CompactionPhasePreSampling, true, func(State, Event) error { return nil }, 0, project)
	if err != nil || window.total > window.hardLimit || engine.contextProjection(history).TailStart != 1 {
		t.Fatalf("normalized admission did not shrink: %+v err=%v", window, err)
	}
	final := input.WithHistory(project(history))
	if !reflect.DeepEqual(final.Partition(agentcontext.KindContinuation), input.Partition(agentcontext.KindContinuation)) ||
		!strings.Contains(joinMessageText(final.Messages()), `turn_history {"turn":1}`) || len(history) != 2 {
		t.Fatal("fold changed continuation, omitted the recovery address, or rewrote history")
	}
}

func TestContextSelectionGateDistinguishesOperatorAndEconomicCeilings(t *testing.T) {
	for _, cause := range []agentcontext.OmissionReason{agentcontext.OmittedOperatorCeiling, agentcontext.OmittedEconomicBudget} {
		t.Run(string(cause), func(t *testing.T) {
			engine := newEngine(t, &scriptedProvider{}, nil)
			engine.options.Context.RecentTailTurns = 0
			history := []provider.Message{
				messageWithText(provider.RoleUser, strings.Repeat("old report ", 400), 1),
				messageWithText(provider.RoleUser, "current", 2),
			}
			project := selectionProjector(t, engine)
			input := agentcontext.NewMessageLedger(agentcontext.LedgerInput{History: project(history)}).Snapshot()
			before, err := engine.measureTokenWindow(input, 128, 0)
			if err != nil {
				t.Fatal(err)
			}
			var economicInput uint64
			if cause == agentcontext.OmittedEconomicBudget {
				economicInput = before.active
			} else {
				engine.options.Context.Window.AutoTokens = before.active
			}
			// An exactly fitting request must not trigger an early fold.
			_, err = engine.runCompactGate(t.Context(), &history, input, 128,
				CompactionPhasePreSampling, true, func(State, Event) error { return nil }, economicInput, project)
			if err != nil || engine.viewFold.folded {
				t.Fatalf("exact ceiling folded: %v", err)
			}
			if economicInput != 0 {
				economicInput--
			} else {
				engine.options.Context.Window.AutoTokens--
			}
			_, err = engine.runCompactGate(t.Context(), &history, input, 128,
				CompactionPhasePreSampling, true, func(State, Event) error { return nil }, economicInput, project)
			selection := engine.contextProjection(history)
			if err != nil || len(selection.Omissions) != 1 || selection.Omissions[0].Reason != cause {
				t.Fatalf("incorrect ceiling cause: %+v err=%v", selection, err)
			}
		})
	}
}
