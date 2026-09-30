package trace_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/common/tracecontext"
	"github.com/fwtllh-png/QCode/internal/observability/trace"
)

// clock is the injected time source. Latency assertions have to be exact, and a
// real clock cannot promise that a span "took 2 seconds".
type clock struct {
	mu sync.Mutex
	at time.Time
}

func newClock() *clock {
	return &clock{at: time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)}
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *clock) advance(step time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(step)
}

// TestRecorderNestsSpansUnderTheTurn pins the shape a reader depends on: the
// first span is the root, later spans without an explicit parent hang off it,
// and a child names its parent.
func TestRecorderNestsSpansUnderTheTurn(t *testing.T) {
	recorder := trace.NewRecorder(newClock().now)
	turn := recorder.Start(trace.NameTurn, 0, nil)
	call := recorder.Start(trace.NameModelCall, 0, map[string]any{"sample": uint32(1)})
	tool := recorder.Start(trace.NameTool, 0, map[string]any{"tool": "shell"})
	approval := recorder.Start(trace.NameApprovalWait, tool.ID(), nil)

	spans := recorder.Spans()
	if len(spans) != 4 {
		t.Fatalf("spans = %d, want 4", len(spans))
	}
	if spans[0].ID != turn.ID() || spans[0].ParentID != 0 {
		t.Fatalf("root = %+v, want id %d with no parent", spans[0], turn.ID())
	}
	if spans[1].ParentID != turn.ID() || spans[2].ParentID != turn.ID() {
		t.Fatalf("model call and tool parents = %d/%d, want %d",
			spans[1].ParentID, spans[2].ParentID, turn.ID())
	}
	if spans[3].ParentID != tool.ID() {
		t.Fatalf("approval parent = %d, want the tool %d", spans[3].ParentID, tool.ID())
	}
	if spans[1].Attributes["sample"] != uint32(1) || spans[2].Attributes["tool"] != "shell" {
		t.Fatalf("attributes lost: %+v / %+v", spans[1].Attributes, spans[2].Attributes)
	}
	if !spans[0].Open() || spans[0].Status != trace.StatusOpen {
		t.Fatalf("an unfinished span reports %q and open=%v", spans[0].Status, spans[0].Open())
	}
	_ = call
	_ = approval
}

func TestRecorderPropagatesLocalSpanContextWithoutExporter(t *testing.T) {
	recorder := trace.NewRecorder(newClock().now)
	root := recorder.Start(trace.NameTurn, 0, nil)
	ctx := recorder.Context(context.Background(), root.ID())
	link, ok := tracecontext.Current(ctx)
	if !ok || len(link.TraceID) != 32 || len(link.SpanID) != 16 {
		t.Fatalf("trace context = %+v, present=%v", link, ok)
	}
}

// TestLatencySumsEachPhase is the receipt's arithmetic. Provider time adds up
// across calls, and the approval wait stays inside the tool that parked for it.
func TestLatencySumsEachPhase(t *testing.T) {
	clock := newClock()
	recorder := trace.NewRecorder(clock.now)

	turn := recorder.Start(trace.NameTurn, 0, nil)
	first := recorder.Start(trace.NameModelCall, 0, nil)
	clock.advance(200 * time.Millisecond)
	recorder.NoteFirstOutput()
	clock.advance(300 * time.Millisecond)
	first.End(trace.StatusOK)

	tool := recorder.Start(trace.NameTool, 0, nil)
	approval := recorder.Start(trace.NameApprovalWait, tool.ID(), nil)
	clock.advance(4 * time.Second)
	approval.End(trace.StatusOK)
	clock.advance(time.Second)
	tool.End(trace.StatusOK)

	second := recorder.Start(trace.NameModelCall, 0, nil)
	clock.advance(700 * time.Millisecond)
	second.End(trace.StatusOK)

	gate := recorder.Start(trace.NameVerify, 0, nil)
	clock.advance(1500 * time.Millisecond)
	gate.End(trace.StatusOK)
	turn.End(trace.StatusOK)

	latency := recorder.Latency()
	if latency.Total != 7700*time.Millisecond {
		t.Fatalf("total = %s, want 7.7s", latency.Total)
	}
	if latency.FirstToken == nil || *latency.FirstToken != 200*time.Millisecond {
		t.Fatalf("first token = %v, want 200ms", latency.FirstToken)
	}
	if latency.Provider != 1200*time.Millisecond {
		t.Fatalf("provider = %s, want 1.2s across both calls", latency.Provider)
	}
	if latency.Tool != 5*time.Second {
		t.Fatalf("tool = %s, want 5s", latency.Tool)
	}
	if latency.ApprovalWait != 4*time.Second {
		t.Fatalf("approval wait = %s, want 4s", latency.ApprovalWait)
	}
	if latency.ApprovalWait > latency.Tool {
		t.Fatalf("approval wait %s escaped its tool %s", latency.ApprovalWait, latency.Tool)
	}
	if latency.Verify != 1500*time.Millisecond {
		t.Fatalf("verify = %s, want 1.5s", latency.Verify)
	}
}

// TestLatencyDistinguishesNothingHappenedFromNothingMeasured is the honesty
// requirement. A turn that ran no tools reports zero tool time — it was measured
// and cost nothing — while a turn whose model never spoke reports no first token
// at all, because zero there would read as "instant".
func TestLatencyDistinguishesNothingHappenedFromNothingMeasured(t *testing.T) {
	clock := newClock()
	recorder := trace.NewRecorder(clock.now)
	turn := recorder.Start(trace.NameTurn, 0, nil)
	call := recorder.Start(trace.NameModelCall, 0, nil)
	clock.advance(time.Second)
	call.End(trace.StatusError)
	turn.End(trace.StatusError)

	latency := recorder.Latency()
	if latency.FirstToken != nil {
		t.Fatalf("first token = %s, want unreported for a turn with no output", *latency.FirstToken)
	}
	if latency.Tool != 0 || latency.ApprovalWait != 0 || latency.Verify != 0 {
		t.Fatalf("phases that never ran reported time: %+v", latency)
	}
	if latency.Provider != time.Second {
		t.Fatalf("provider = %s, want 1s", latency.Provider)
	}
}

// TestLatencyCountsOpenSpansSoFar matters because the receipt is built before
// the turn's terminal event, so the root span is still open when it is read.
func TestLatencyCountsOpenSpansSoFar(t *testing.T) {
	clock := newClock()
	recorder := trace.NewRecorder(clock.now)
	recorder.Start(trace.NameTurn, 0, nil)
	recorder.Start(trace.NameTool, 0, nil)
	clock.advance(3 * time.Second)

	latency := recorder.Latency()
	if latency.Total != 3*time.Second || latency.Tool != 3*time.Second {
		t.Fatalf("open spans = %+v, want 3s so far", latency)
	}
}

// TestCloseEndsOpenSpansAndSaysTheyWereOpen keeps a crashed turn's trace
// readable: the span gets an end time so a duration exists, and keeps the status
// that says nobody closed it.
func TestCloseEndsOpenSpansAndSaysTheyWereOpen(t *testing.T) {
	clock := newClock()
	recorder := trace.NewRecorder(clock.now)
	turn := recorder.Start(trace.NameTurn, 0, nil)
	recorder.Start(trace.NameTool, 0, nil)
	clock.advance(2 * time.Second)
	turn.End(trace.StatusOK)

	spans := recorder.Close()
	if len(spans) != 2 {
		t.Fatalf("spans = %d, want 2", len(spans))
	}
	if spans[0].Status != trace.StatusOK || spans[0].Duration() != 2*time.Second {
		t.Fatalf("closed root = %q / %s", spans[0].Status, spans[0].Duration())
	}
	if spans[1].Status != trace.StatusOpen {
		t.Fatalf("abandoned span status = %q, want %q", spans[1].Status, trace.StatusOpen)
	}
	if spans[1].Duration() != 2*time.Second {
		t.Fatalf("abandoned span duration = %s, want 2s", spans[1].Duration())
	}
}

func TestTerminalFreezeMakesLatencyImmutableAndSeparatesCleanup(t *testing.T) {
	clock := newClock()
	recorder := trace.NewRecorder(clock.now)
	recorder.Start(trace.NameTurn, 0, nil)
	model := recorder.Start(trace.NameModelCall, 0, nil)
	clock.advance(2 * time.Second)
	model.End(trace.StatusOK)
	clock.advance(time.Second)
	frozen := recorder.FreezeTerminal(trace.StatusOK)
	if !frozen.Recorded ||
		frozen.Latency.Total != 3*time.Second ||
		frozen.Latency.Provider != 2*time.Second {
		t.Fatalf("frozen = %+v", frozen)
	}
	clock.advance(5 * time.Second)
	spans := recorder.CloseWithCleanup(trace.StatusOK)
	if latency := recorder.Latency(); latency != frozen.Latency {
		t.Fatalf("latency changed after freeze: %+v", latency)
	}
	cleanup := spans[len(spans)-1]
	if cleanup.Name != trace.NameCleanup ||
		cleanup.Duration() != 5*time.Second ||
		cleanup.Attributes["excluded_from_terminal_measurement"] != true {
		t.Fatalf("cleanup = %+v", cleanup)
	}
	if spans[0].Duration() != 3*time.Second {
		t.Fatalf("root duration = %s", spans[0].Duration())
	}
	if repeated := recorder.CloseWithCleanup(trace.StatusOK); len(repeated) !=
		len(spans) {
		t.Fatalf("cleanup close is not idempotent: %d, %d", len(spans), len(repeated))
	}
}

// TestRecorderTakesConcurrentSpans exists because the tool phase runs several
// tools at once; under -race this is the assertion that matters.
func TestRecorderTakesConcurrentSpans(t *testing.T) {
	recorder := trace.NewRecorder(newClock().now)
	recorder.Start(trace.NameTurn, 0, nil)
	var group sync.WaitGroup
	for index := 0; index < 8; index++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			span := recorder.Start(trace.NameTool, 0, map[string]any{"call_id": index})
			span.Set("is_error", false)
			span.End(trace.StatusOK)
		}(index)
	}
	group.Wait()
	if spans := recorder.Spans(); len(spans) != 9 {
		t.Fatalf("spans = %d, want 9", len(spans))
	}
	if latency := recorder.Latency(); latency.Tool != 0 {
		t.Fatalf("a frozen clock reported %s of tool time", latency.Tool)
	}
}

// TestSpanHandlesTolerateNil keeps the engine call sites free of guards.
func TestSpanHandlesTolerateNil(t *testing.T) {
	var recorder *trace.Recorder
	span := recorder.Start(trace.NameTurn, 0, nil)
	span.Set("key", "value")
	span.End(trace.StatusOK)
	recorder.NoteFirstOutput()
	if span.ID() != 0 {
		t.Fatalf("nil recorder produced span %d", span.ID())
	}
	if spans := recorder.Spans(); spans != nil {
		t.Fatalf("nil recorder produced spans %+v", spans)
	}
	if latency := recorder.Latency(); latency != (trace.Latency{}) {
		t.Fatalf("nil recorder produced latency %+v", latency)
	}
}
