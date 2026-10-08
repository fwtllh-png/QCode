package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/config"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type continuityReplayRow struct {
	Configuration         string   `json:"configuration"`
	Turns                 int      `json:"turns"`
	RequiredDefinitions   int      `json:"required_definitions"`
	DefinitionsRetained   int      `json:"definitions_retained"`
	NumberingRetained     int      `json:"numbering_retained"`
	ProgressRetained      int      `json:"progress_retained"`
	ProviderCalls         int      `json:"provider_calls"`
	RecoveryCalls         int      `json:"recovery_calls"`
	FileReads             int      `json:"file_reads"`
	BackgroundJobs        int      `json:"background_jobs"`
	EstimatedInputTokens  uint64   `json:"estimated_input_tokens"`
	EstimatedOutputTokens uint64   `json:"estimated_output_tokens"`
	NonmonotonicPrefixes  int      `json:"nonmonotonic_prefixes"`
	ElapsedUS             int64    `json:"elapsed_us"`
	FirstResponseUS       []int64  `json:"first_response_us"`
	Sources               []string `json:"source_ids"`
	CostUSD               *float64 `json:"cost_usd"`
}

// This compares explicit reference configuration with shipped defaults on the
// same frozen model and scripted answers. It measures transport inputs, not
// semantic faithfulness or billable provider usage.
func TestContextContinuityP5Replay(t *testing.T) {
	data, err := os.ReadFile("testdata/context_continuity_zh.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture continuityBaselineFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	defaults := config.Defaults().Context
	var report []continuityReplayRow
	for _, scenario := range []struct {
		name   string
		turns  int
		digest string
	}{
		{"reference_explicit_tail_2", 2, "ledger"},
		{"p5_defaults", defaults.View.RecentTailTurns, defaults.View.Digest},
	} {
		p := &scriptedProvider{}
		for _, turn := range fixture.Turns {
			p.streams = append(p.streams, textStream(turn.Answer))
		}
		e := newEngine(t, p, tool.NewRegistry(nil, nil))
		e.options.Workspace = t.TempDir()
		e.conversationOrigin = "continuity-replay"
		route := mustTestRouteWithContext(t, fixture.ContextTokens)
		e.options.Route = route
		e.options.Routes, err = model.NewRouteSet(route, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		e.options.MaxOutputTokens = fixture.OutputReserveTokens
		e.options.Context = ContextPolicy{RecentTailTurns: scenario.turns, Digest: scenario.digest,
			SemanticNarrative: defaults.View.NarrativeMode, NarrativeTimeout: defaults.Compact.SemanticNarrativeTimeout}
		row := continuityReplayRow{Configuration: scenario.name, Turns: len(fixture.Turns)}
		started := time.Now()
		for index, turn := range fixture.Turns {
			var observed *protocol.ReceiptContextProjection
			turnStarted := time.Now()
			first := false
			_, err := e.Execute(t.Context(), TurnRequest{TurnID: fmt.Sprintf("replay-%d", index+1), Prompt: turn.Prompt}, func(event Event) error {
				if event.ContextProjection != nil {
					observed = event.ContextProjection
					row.EstimatedInputTokens += observed.InputTokens
					if event.InputContext.PrefixCompared && !event.InputContext.PrefixMonotonic {
						row.NonmonotonicPrefixes++
					}
				}
				if event.Text != "" && event.State == Streaming && !first {
					row.FirstResponseUS = append(row.FirstResponseUS, time.Since(turnStarted).Microseconds())
					first = true
				}
				if event.ToolCall != nil && event.Result != nil {
					if event.ToolCall.Name == "turn_history" || event.ToolCall.Name == "result_get" {
						row.RecoveryCalls++
					}
					if event.ToolCall.Name == "file_read" {
						row.FileReads++
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if observed == nil || observed.RecoveryOnly || observed.Digest == "" {
				t.Fatal("missing admitted projection")
			}
			request := p.requests[index]
			if request.Route.Model().Limits.ContextTokens != fixture.ContextTokens || request.MaxOutputTokens != fixture.OutputReserveTokens {
				t.Fatal("replay route was not frozen")
			}
			visible := joinMessageText(request.Messages)
			if !strings.Contains(visible, turn.Prompt) {
				t.Fatal("current request omitted")
			}
			if turn.RequiredDefinition != "" {
				row.RequiredDefinitions++
				if strings.Contains(visible, turn.RequiredDefinition) {
					row.DefinitionsRetained++
				}
				if strings.Contains(visible, fmt.Sprintf("%d. %s", index, turn.RequiredDefinition)) {
					row.NumberingRetained++
				}
			}
			if index > 0 && strings.Contains(visible, fixture.Turns[index-1].Answer) {
				row.ProgressRetained++
			}
			row.EstimatedOutputTokens += e.estimateTokens([]provider.Message{provider.TextMessage(provider.RoleAssistant, turn.Answer)})
			if job := e.PreparePostTurnNarrative("continuity-replay", protocol.TurnID(fmt.Sprintf("replay-%d", index+1))); job != nil {
				row.BackgroundJobs++
				e.CancelContextMaintenance()
			}
		}
		row.ElapsedUS = time.Since(started).Microseconds()
		row.ProviderCalls = len(p.requests)
		for _, source := range e.context.Conversation().Sources {
			row.Sources = append(row.Sources, source.ID)
		}
		sort.Strings(row.Sources)
		if row.DefinitionsRetained != 3 || row.NumberingRetained != 3 || row.ProgressRetained != 3 || row.ProviderCalls != 4 || row.RecoveryCalls != 0 || row.FileReads != 0 || row.BackgroundJobs != 0 {
			t.Fatalf("replay regression: %+v", row)
		}
		report = append(report, row)
	}
	if !reflect.DeepEqual(report[0].Sources, report[1].Sources) {
		t.Fatal("configuration changed stable report identities")
	}
	encoded, err := json.MarshalIndent(struct {
		SchemaVersion           int                   `json:"schema_version"`
		Provider                string                `json:"provider"`
		ContextTokens           uint64                `json:"context_tokens"`
		OutputReserve           uint64                `json:"output_reserve_tokens"`
		SemanticQualityMeasured bool                  `json:"semantic_quality_measured"`
		Rows                    []continuityReplayRow `json:"rows"`
	}{1, "scripted; no billable usage", fixture.ContextTokens, fixture.OutputReserveTokens, false, report}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	t.Log(string(encoded))
	if path := os.Getenv("QCODE_CONTEXT_REPLAY_REPORT"); path != "" {
		if err := os.WriteFile(path, append(encoded, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNarrativeP5AutomaticGenerationRequiresBothSwitches(t *testing.T) {
	for _, digest := range []string{"ledger", "ledger+narrative"} {
		for _, mode := range []string{"off", "post_turn"} {
			e := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
			seedOmittedHistory(e)
			e.options.Context.Digest, e.options.Context.SemanticNarrative = digest, mode
			job := e.PreparePostTurnNarrative("thread", "turn-3")
			if (job != nil) != (digest == "ledger+narrative" && mode == "post_turn") {
				t.Fatalf("%s/%s scheduled=%t", digest, mode, job != nil)
			}
			e.CancelContextMaintenance()
		}
	}
}

func TestNarrativeP5LifecyclePublishesLateInstallationAndReusesCacheWhenOff(t *testing.T) {
	p := &narrativeFunctionProvider{scriptedProvider: scriptedProvider{streams: []provider.Stream{textStream("continue")}}, summary: func(_ context.Context, r provider.ModelRequest) (provider.Stream, error) {
		return coveredNarrativeStream(narrativeInputOf(t, r)), nil
	}}
	e := newEngine(t, p, tool.NewRegistry(nil, nil))
	seedOmittedHistory(e)
	e.options.Context.Digest, e.options.Context.SemanticNarrative = "ledger+narrative", "post_turn"
	job := e.PreparePostTurnNarrative("thread-1", "turn-3")
	if job == nil {
		t.Fatal("job missing")
	}
	events := make(chan NarrativeGenerationResult, 3)
	job.Observe(func(r NarrativeGenerationResult) { events <- r })
	e.mu.Lock()
	done := make(chan NarrativeGenerationResult, 1)
	go func() { result, _ := job.Run(t.Context()); done <- result }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		e.mu.Unlock()
		t.Fatal("foreground lock blocked generation")
	}
	e.options.Context.SemanticNarrative = "off"
	e.mu.Unlock()
	var sampled *protocol.ReceiptContextProjection
	if _, err := e.Run(t.Context(), "continue", func(event Event) error {
		if event.ContextProjection != nil {
			sampled = event.ContextProjection
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"started", "prepared", "completed"} {
		select {
		case event := <-events:
			if event.Receipt == nil || event.Receipt.Status != status || len(event.Calls) != 0 || event.Receipt.NarrativeForegroundWaitMS != 0 {
				t.Fatalf("event=%+v", event)
			}
		case <-time.After(time.Second):
			t.Fatalf("missing %s event", status)
		}
	}
	if sampled == nil {
		t.Fatal("missing sample")
	}
	semantic := false
	for _, r := range sampled.References {
		if r.Representation == "semantic" && r.Status == "covered" {
			semantic = true
		}
	}
	if !semantic || !strings.Contains(joinMessageText(p.requests[0].Messages), "保留原始问题编号和定义") {
		t.Fatal("off discarded valid cache")
	}
}

func TestNarrativeP5InvalidCandidateReportsFailureBeforeInstallation(t *testing.T) {
	p := &narrativeFunctionProvider{summary: func(context.Context, provider.ModelRequest) (provider.Stream, error) { return textStream("{}"), nil }}
	e := newEngine(t, p, tool.NewRegistry(nil, nil))
	seedOmittedHistory(e)
	e.options.Context.Digest, e.options.Context.SemanticNarrative = "ledger+narrative", "post_turn"
	job := e.PreparePostTurnNarrative("thread-1", "turn-3")
	if job == nil {
		t.Fatal("job missing")
	}
	states := make(chan NarrativeGenerationResult, 3)
	job.Observe(func(result NarrativeGenerationResult) { states <- result })
	e.mu.Lock()
	done := make(chan struct{})
	go func() { defer close(done); _, _ = job.Run(t.Context()) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		e.mu.Unlock()
		t.Fatal("invalid candidate waited for foreground")
	}
	e.mu.Unlock()
	<-states // started
	failed := <-states
	if failed.Receipt.Status != "fallback" || failed.Receipt.FallbackReason == "awaiting_safe_boundary" || failed.Receipt.FallbackReason == "" {
		t.Fatalf("failed candidate hidden: %+v", failed.Receipt)
	}
	e.CancelContextMaintenance()
}
