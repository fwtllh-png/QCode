package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	toolguard "github.com/fwtllh-png/QCode/internal/adapter/tool/guard"
	"github.com/fwtllh-png/QCode/internal/observability/trace"
	"github.com/fwtllh-png/QCode/internal/security/policy"
)

// latencyClock is the turn's only clock. The scripted provider, tool, verifier
// and approver each advance it by a known amount, so every duration the turn
// reports is exact instead of "about right".
type latencyClock struct {
	mu sync.Mutex
	at time.Time
}

func newLatencyClock() *latencyClock {
	return &latencyClock{at: time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC)}
}

func (c *latencyClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *latencyClock) advance(step time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(step)
}

// timedEvent is a stream event and the time the provider spends before it.
type timedEvent struct {
	after time.Duration
	event provider.StreamEvent
}

type timedProvider struct {
	clock  *latencyClock
	calls  [][]timedEvent
	served int
}

func (p *timedProvider) Stream(
	context.Context, provider.ModelRequest,
) (provider.Stream, error) {
	if p.served >= len(p.calls) {
		return nil, context.DeadlineExceeded
	}
	events := p.calls[p.served]
	p.served++
	return &timedStream{clock: p.clock, events: events}, nil
}

type timedStream struct {
	clock  *latencyClock
	events []timedEvent
	index  int
}

func (s *timedStream) Recv() (provider.StreamEvent, error) {
	if s.index >= len(s.events) {
		return provider.StreamEvent{}, context.DeadlineExceeded
	}
	next := s.events[s.index]
	s.index++
	s.clock.advance(next.after)
	return next.event, nil
}

func (*timedStream) Close() error { return nil }

// latencyTool needs approval (it writes) and takes a known amount of time. It
// writes a real file so the journal and verify gate observe the same change.
type latencyTool struct {
	root  string
	clock *latencyClock
	spent time.Duration
}

func (latencyTool) Descriptor() tool.Descriptor {
	return tool.Descriptor{
		Name: "write", Description: "test write", Visibility: tool.VisibleModel,
		Capability: tool.CapabilityWrite, AccessMode: tool.AccessWrite,
		ParallelPolicy: tool.ParallelConcurrent, SandboxRequirement: tool.SandboxNone,
		Availability: tool.AvailabilityAvailable,
		ResourceResolver: tool.ResourceResolver{Templates: []tool.ResourceTemplate{{
			Kind: "file", Field: "path", Access: tool.AccessWrite,
		}}},
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string"},
			},
			"required": []string{"path"}, "additionalProperties": false,
		},
	}
}

func (t latencyTool) Execute(_ context.Context, raw json.RawMessage) (tool.Result, error) {
	t.clock.advance(t.spent)
	if err := os.WriteFile(filepath.Join(t.root, "a.txt"), []byte("changed\n"), 0o600); err != nil {
		return tool.Result{}, err
	}
	return tool.Result{
		Content: string(raw),
		Outcome: &tool.Outcome{Facts: &tool.OutcomeFacts{
			WorkspaceChanges: []tool.WorkspaceChange{
				{Path: "a.txt", Kind: tool.WorkspaceModified, Added: 1},
			},
		}},
	}, nil
}

// timedVerifier spends a known amount of time in the gate.

// TestTurnReportsEveryLatencyPhase is the T3 acceptance: a turn that called a
// model, ran a tool, waited for a human and verified itself reports all five
// numbers, and each one is the stretch that phase actually spent.
func TestTurnReportsEveryLatencyPhase(t *testing.T) {
	clock := newLatencyClock()
	engine := newLatencyEngine(t, latencyEngineOptions{
		clock:   clock,
		posture: policy.PermissionSuggest,
		tool:    latencyTool{clock: clock, spent: time.Second},
		calls: [][]timedEvent{{
			// 200ms to the first token, then 100ms to finish the call.
			{after: 200 * time.Millisecond, event: provider.StreamEvent{
				Type: provider.EventTextDelta, Text: "working",
			}},
			{after: 100 * time.Millisecond, event: provider.StreamEvent{
				Type: provider.EventToolCallDelta, ToolCall: &provider.ToolCallFragment{
					ID: "call-1", Name: "write", Arguments: `{"path":"a.txt"}`,
				},
			}},
			{event: provider.StreamEvent{Type: provider.EventMessageStop}},
		}, {
			{after: 500 * time.Millisecond, event: provider.StreamEvent{
				Type: provider.EventTextDelta, Text: "done",
			}},
			{event: provider.StreamEvent{Type: provider.EventMessageStop}},
		}},
	})

	result, err := engine.RunForTurn(t.Context(), "turn-1", "edit the file",
		approveAfter(t, engine, clock, 4*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if result.State != Completed {
		t.Fatalf("state = %q, want the turn to complete", result.State)
	}

	latency := engine.TurnLatency()
	if latency == nil {
		t.Fatal("a turn that measured its phases reported no latency")
	}
	if latency.FirstToken == nil || *latency.FirstToken != 200*time.Millisecond {
		t.Fatalf("first token = %v, want 200ms", latency.FirstToken)
	}
	if latency.Provider != 800*time.Millisecond {
		t.Fatalf("provider = %s, want 800ms across both calls", latency.Provider)
	}
	if latency.Tool != 5*time.Second {
		t.Fatalf("tool = %s, want 5s (1s of work behind a 4s wait)", latency.Tool)
	}
	if latency.ApprovalWait != 4*time.Second {
		t.Fatalf("approval wait = %s, want 4s", latency.ApprovalWait)
	}
	if latency.ApprovalWait > latency.Tool {
		t.Fatalf("approval wait %s is not inside its tool %s", latency.ApprovalWait, latency.Tool)
	}
	if latency.Verify != 0 {
		t.Fatalf("verify = %s, want 1.5s", latency.Verify)
	}
	if latency.Total != 5800*time.Millisecond {
		t.Fatalf("total = %s, want 7.3s", latency.Total)
	}

	spans := engine.TurnSpans()
	byName := map[string]trace.Record{}
	for _, span := range spans {
		byName[span.Name] = span
		if span.Open() {
			t.Fatalf("persisted span %q was never closed", span.Name)
		}
	}
	for _, name := range []string{
		trace.NameTurn, trace.NameModelCall, trace.NameTool,
		trace.NameApprovalWait,
	} {
		if _, exists := byName[name]; !exists {
			t.Fatalf("no %q span was persisted; got %+v", name, spans)
		}
	}
	if byName[trace.NameApprovalWait].ParentID != byName[trace.NameTool].ID {
		t.Fatalf("approval wait hangs off %d, want the tool span %d",
			byName[trace.NameApprovalWait].ParentID, byName[trace.NameTool].ID)
	}
	if byName[trace.NameTool].Attributes["tool"] != "write" {
		t.Fatalf("tool span attributes = %+v", byName[trace.NameTool].Attributes)
	}
	if byName[trace.NameTurn].Status != trace.StatusOK {
		t.Fatalf("turn span status = %q, want ok", byName[trace.NameTurn].Status)
	}
}

// TestTurnWithoutToolsSaysZeroRatherThanNothing is the honesty half of T3: a turn
// that ran no tool and asked nobody reports zero for those phases, because zero
// is a fact about the turn and a missing number would be a fact about us.
func TestTurnWithoutToolsSaysZeroRatherThanNothing(t *testing.T) {
	clock := newLatencyClock()
	engine := newLatencyEngine(t, latencyEngineOptions{
		clock: clock,
		calls: [][]timedEvent{{
			{after: 300 * time.Millisecond, event: provider.StreamEvent{
				Type: provider.EventTextDelta, Text: "just an answer",
			}},
			{event: provider.StreamEvent{Type: provider.EventMessageStop}},
		}},
	})

	if _, err := engine.RunForTurn(t.Context(), "turn-1", "answer me", nil); err != nil {
		t.Fatal(err)
	}
	latency := engine.TurnLatency()
	if latency == nil {
		t.Fatal("no latency reported")
	}
	if latency.Tool != 0 || latency.ApprovalWait != 0 || latency.Verify != 0 {
		t.Fatalf("phases that never happened reported time: %+v", latency)
	}
	if latency.FirstToken == nil || *latency.FirstToken != 300*time.Millisecond {
		t.Fatalf("first token = %v, want 300ms", latency.FirstToken)
	}
	if latency.Provider != 300*time.Millisecond || latency.Total != 300*time.Millisecond {
		t.Fatalf("provider/total = %s/%s, want 300ms", latency.Provider, latency.Total)
	}
}

// TestTurnWithoutModelOutputReportsNoFirstToken is the other side of the pointer:
// a model that produced nothing has no first token, and reporting zero would read
// as one that arrived instantly.
func TestTurnWithoutModelOutputReportsNoFirstToken(t *testing.T) {
	clock := newLatencyClock()
	engine := newLatencyEngine(t, latencyEngineOptions{
		clock: clock,
		calls: [][]timedEvent{{
			{after: 700 * time.Millisecond, event: provider.StreamEvent{
				Type: provider.EventUsage, Usage: &provider.Usage{InputTokens: 10},
			}},
		}},
	})

	if _, err := engine.RunForTurn(t.Context(), "turn-1", "answer me", nil); err == nil {
		t.Fatal("a stream that never stopped should fail the turn")
	}
	latency := engine.TurnLatency()
	if latency == nil {
		t.Fatal("a failed turn reported no latency")
	}
	if latency.FirstToken != nil {
		t.Fatalf("first token = %s, want unreported", *latency.FirstToken)
	}
	// Usage arrives before content on some providers, so the call still took time.
	if latency.Provider != 700*time.Millisecond {
		t.Fatalf("provider = %s, want the failed call's 700ms", latency.Provider)
	}
}

// TestAnUnnamedTurnIsMeasuredButNotPersisted covers Run: there is no durable turn
// row to hang spans off, so the trace stays in memory rather than failing a write.
func TestAnUnnamedTurnIsMeasuredButNotPersisted(t *testing.T) {
	clock := newLatencyClock()
	engine := newLatencyEngine(t, latencyEngineOptions{
		clock: clock,
		calls: [][]timedEvent{{
			{after: 250 * time.Millisecond, event: provider.StreamEvent{
				Type: provider.EventTextDelta, Text: "done",
			}},
			{event: provider.StreamEvent{Type: provider.EventMessageStop}},
		}},
	})

	if _, err := engine.Run(t.Context(), "answer me", nil); err != nil {
		t.Fatal(err)
	}
	if latency := engine.TurnLatency(); latency == nil || latency.Total != 250*time.Millisecond {
		t.Fatalf("latency = %+v, want 250ms measured in memory", latency)
	}
	var core, transitions int
	for _, span := range engine.TurnSpans() {
		if span.Name == trace.NameTurnKernelTransition {
			transitions++
		} else {
			core++
		}
	}
	if core != 2 || transitions == 0 {
		t.Fatalf(
			"spans = core:%d transitions:%d, want two latency spans and kernel transitions",
			core,
			transitions,
		)
	}
}

func TestActivitySnapshotCountsOnlyOpenProviderAndToolSpans(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, nil)
	scope := attachTestScope(t, engine)
	recorder := trace.NewRecorder(nil)
	root := recorder.Start(trace.NameTurn, 0, nil)
	model := recorder.Start(trace.NameModelCall, root.ID(), nil)
	toolSpan := recorder.Start(trace.NameTool, root.ID(), nil)
	scope.state.recorder = recorder
	if got := engine.ActivitySnapshot(); got.ProviderCalls != 1 ||
		got.ToolExecutions != 1 {
		t.Fatalf("active snapshot = %+v", got)
	}
	model.End(trace.StatusOK)
	toolSpan.End(trace.StatusOK)
	if got := engine.ActivitySnapshot(); got != (ActivitySnapshot{}) {
		t.Fatalf("terminal snapshot = %+v", got)
	}
}

type latencyEngineOptions struct {
	clock   *latencyClock
	posture policy.Permission
	tool    tool.Executor
	calls   [][]timedEvent
}

func newLatencyEngine(t *testing.T, options latencyEngineOptions) *Engine {
	t.Helper()
	root := t.TempDir()
	registry := tool.NewRegistry(nil, nil)
	if value, ok := options.tool.(latencyTool); ok {
		value.root = root
		options.tool = value
	}
	if options.tool != nil {
		if err := registry.Register(options.tool); err != nil {
			t.Fatal(err)
		}
	}
	posture := options.posture
	if posture == "" {
		posture = policy.PermissionBypass
	}
	engineOptions := Options{ProviderConfig: ProviderConfig{Provider: &timedProvider{clock: options.clock, calls: options.calls},
		Route: testRoute(t), MaxOutputTokens: 128, MaxSteps: 8}, ToolConfig: ToolConfig{Tools: registry,

		Diagnostics: fakeDiagnosticRunner{}}, SecurityConfig: SecurityConfig{Security: policy.DefaultRuntime(policy.ModeAct, posture),
		Workspace: root,
		Journal:   newTestWorkspaceJournal(t, root)}, TelemetryConfig: TelemetryConfig{Observability: trace.Runtime{
		Clock: options.clock.now,
	}},
	}
	engine, err := newTestEngine(engineOptions)
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

// approveAfter answers every approval request after the clock has moved on, which
// is the wait the trace has to attribute to the human rather than to the tool.
func approveAfter(
	t *testing.T, engine *Engine, clock *latencyClock, waited time.Duration,
) func(Event) error {
	t.Helper()
	return func(event Event) error {
		if event.State != AwaitingApproval || event.Approval == nil {
			return nil
		}
		clock.advance(waited)
		// A zero expiry takes the request's own, which the guard requires the
		// decision to stay inside.
		return mustControl(t, engine).ResolveApproval(toolguard.ApprovalDecision{
			RequestID: event.Approval.RequestID, Approved: true,
			Scope: policy.ApprovalOnce,
		})
	}
}
