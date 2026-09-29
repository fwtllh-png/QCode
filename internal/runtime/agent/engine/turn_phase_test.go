package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	providerfixture "github.com/fwtllh-png/QCode/internal/adapter/provider/fixture"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/observability/verify"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type cancelingProbe struct {
	engine *Engine
	reason string
}

func (p *cancelingProbe) Descriptor() tool.Descriptor {
	descriptor := (&interruptingProbe{}).Descriptor()
	descriptor.Name = "cancel_probe"
	return descriptor
}

func (p *cancelingProbe) Execute(
	context.Context,
	json.RawMessage,
) (tool.Result, error) {
	if control, err := p.engine.Control(); err == nil {
		_ = control.Cancel(p.reason)
	}
	return tool.Result{Content: "probe output"}, nil
}

// A cancel whose reason rolls the workspace journal back must not keep the
// turn's tool results in history: the next turn would otherwise believe
// edits exist that the journal already reverted.
func TestNonSuspendingCancelDropsTurnHistoryLikeItsJournal(t *testing.T) {
	registry := tool.NewRegistry(nil, nil)
	probe := &cancelingProbe{reason: protocol.CancelReasonHostInterrupted}
	if err := registry.Register(probe); err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, &scriptedProvider{streams: []provider.Stream{
		toolCallStream("call-1", "cancel_probe", `{"text":"probe"}`),
	}}, registry)
	probe.engine = engine

	var terminal Event
	result, err := engine.Run(t.Context(), "inspect the probe", func(event Event) error {
		if event.State == Canceled {
			terminal = event
		}
		return nil
	})
	if err != nil || result.State != Canceled {
		t.Fatalf("Run() = (%v, %v), want canceled", result.State, err)
	}
	if terminal.CancelReason != protocol.CancelReasonHostInterrupted {
		t.Fatalf("cancel reason = %q", terminal.CancelReason)
	}
	suspends := turnkernel.CancelSuspendsDraft(protocol.CancelReasonHostInterrupted)
	if suspends {
		t.Fatal("host_interrupted unexpectedly suspends the journal draft")
	}
	for _, message := range engine.History() {
		for _, block := range message.Blocks {
			if block.ToolResult != nil || block.ToolCall != nil {
				t.Fatalf("rolled-back turn left tool traffic in history: %+v", engine.History())
			}
		}
	}
}

// A steer that lands after the model handler's own drain but before the
// turn loop records the sample must follow the assistant output it reacts
// to; the provider must never see the user's correction before the answer.
func TestLateSteerFollowsTheSampleItInterrupts(t *testing.T) {
	scripted := &scriptedProvider{streams: []provider.Stream{
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventReasoningDelta, Text: "think"},
			{Type: provider.EventTextDelta, Text: "draft answer"},
			{Type: provider.EventMessageStop},
		}},
		textStream("final"),
	}}
	engine := newEngine(t, scripted, tool.NewRegistry(nil, nil))
	var steered atomic.Bool
	result, err := engine.Run(t.Context(), "question", func(event Event) error {
		if event.ReasoningCompleted != nil && steered.CompareAndSwap(false, true) {
			if err := mustControl(t, engine).Steer("late steer"); err != nil {
				t.Errorf("Steer() error = %v", err)
			}
		}
		return nil
	})
	if err != nil || result.State != Completed {
		t.Fatalf("Run() = (%v, %v)", result.State, err)
	}
	if len(scripted.requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(scripted.requests))
	}
	second := agentcontext.StripWorldState(scripted.requests[1].Messages)
	answer, steer := -1, -1
	for index, message := range second {
		switch message.Text() {
		case "draft answer":
			answer = index
		case "late steer":
			steer = index
		}
	}
	if answer < 0 || steer < 0 || answer > steer {
		t.Fatalf("assistant index %d, steer index %d: %+v", answer, steer, second)
	}
}

type terminalRejectingFacts struct {
	turnkernel.DomainFactStore
	rejected atomic.Bool
}

func (s *terminalRejectingFacts) AppendDomainFacts(
	ctx context.Context,
	turnID string,
	sequence uint64,
	facts []turnkernel.DomainFact,
) error {
	for _, fact := range facts {
		if fact.Command == turnkernel.CommandName(turnkernel.TerminalRequested{}) &&
			s.rejected.CompareAndSwap(false, true) {
			return errors.New("injected terminal request rejection")
		}
	}
	return s.DomainFactStore.AppendDomainFacts(ctx, turnID, sequence, facts)
}

// When the kernel rejects the completed terminal request, the emitted
// envelope is Failed; the SessionDelta it commits must describe that failure
// rather than the completion transcript staged before the rejection.
func TestRejectedCompletionCommitsFailureHistory(t *testing.T) {
	facts := &terminalRejectingFacts{
		DomainFactStore: turnkernel.NewMemoryTerminalEnvelopeStore(nil, nil),
	}
	coordinators, err := turnkernel.NewStoreCoordinatorRuntime(facts)
	if err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, &scriptedProvider{streams: []provider.Stream{
		textStream("done"),
	}}, tool.NewRegistry(nil, nil))
	engine.options.TurnCoordinatorRuntime = coordinators

	var states []State
	result, _ := engine.Run(t.Context(), "answer", func(event Event) error {
		states = append(states, event.State)
		return nil
	})
	if !facts.rejected.Load() {
		t.Fatal("terminal request was never rejected")
	}
	assertOneTerminal(t, states, Failed)
	if result.State != Failed {
		t.Fatalf("result state = %v, want %v", result.State, Failed)
	}
	history := engine.History()
	var sawFailureNote bool
	for _, message := range history {
		if message.Role == provider.RoleSystem &&
			strings.HasPrefix(message.Text(), "[turn_terminal]") {
			sawFailureNote = true
		}
	}
	if !sawFailureNote {
		t.Fatalf("failed turn committed the completion transcript: %+v", history)
	}
}

type steerDuringToolProbe struct {
	engine   *Engine
	observed atomic.Value
}

func (p *steerDuringToolProbe) Descriptor() tool.Descriptor {
	descriptor := (&interruptingProbe{}).Descriptor()
	descriptor.Name = "steer_probe"
	return descriptor
}

func (p *steerDuringToolProbe) Execute(
	ctx context.Context,
	_ json.RawMessage,
) (tool.Result, error) {
	control, err := p.engine.Control()
	if err != nil {
		return tool.Result{}, err
	}
	if err := control.Steer("steer while tools run"); err != nil {
		return tool.Result{}, err
	}
	p.observed.Store(context.Cause(ctx) == nil)
	return tool.Result{Content: "tool finished"}, nil
}

// Steering redirects the next sample; it must not abort a running tool batch
// (a build, an install, a pending approval) the way Cancel does.
func TestSteerDoesNotAbortRunningTools(t *testing.T) {
	registry := tool.NewRegistry(nil, nil)
	probe := &steerDuringToolProbe{}
	if err := registry.Register(probe); err != nil {
		t.Fatal(err)
	}
	scripted := &scriptedProvider{streams: []provider.Stream{
		toolCallStream("call-1", "steer_probe", `{"text":"probe"}`),
		textStream("final"),
	}}
	engine := newEngine(t, scripted, registry)
	probe.engine = engine
	result, err := engine.Run(t.Context(), "run the probe", nil)
	if err != nil || result.State != Completed {
		t.Fatalf("Run() = (%v, %v)", result.State, err)
	}
	if intact, _ := probe.observed.Load().(bool); !intact {
		t.Fatal("steer canceled the running tool batch")
	}
	second := agentcontext.StripWorldState(scripted.requests[1].Messages)
	result1, steer := -1, -1
	for index, message := range second {
		for _, block := range message.Blocks {
			if block.ToolResult != nil &&
				strings.Contains(block.ToolResult.Content, "tool finished") {
				result1 = index
			}
		}
		if message.Text() == "steer while tools run" {
			steer = index
		}
	}
	if result1 < 0 || steer < 0 || result1 > steer {
		t.Fatalf("tool result index %d, steer index %d: %+v", result1, steer, second)
	}
}

// A steer accepted after the loop's last drain but before the completion
// step (here: while verification runs) must still reach the model; the
// completion yields to it and samples again.
func TestSteerDuringVerificationContinuesTheTurn(t *testing.T) {
	fixture := newVerifyGateFixture(t, VerifyOptions{
		Mode: VerifyModeHard, Scope: verify.ScopeDiagnostics,
	}, &scriptedVerifier{receipts: []verify.Receipt{passedReceipt()}}, 1, 6)
	var steered bool
	result, err := fixture.engine.RunForTurn(t.Context(), "turn-1", "edit", func(event Event) error {
		if event.State == Verifying && event.Verification != nil && !steered {
			steered = true
			if err := mustControl(t, fixture.engine).Steer("verify steer"); err != nil {
				t.Errorf("Steer() during verification error = %v", err)
			}
		}
		return nil
	})
	if err != nil || result.State != Completed {
		t.Fatalf("Run() = (%+v, %v)", result, err)
	}
	if !steered {
		t.Fatal("verification never ran")
	}
	requests := fixture.provider.requests
	if len(requests) != 4 || !requestContains(requests[3], "verify steer") {
		t.Fatalf("provider requests = %d; the steer never reached the model", len(requests))
	}
}

// Once the turn starts terminalizing, a steer can no longer reach the model;
// accepting it would report success for a message that is then dropped.
func TestSteerIsRejectedOnceTheTurnTerminalizes(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{streams: []provider.Stream{
		textStream("done"),
	}}, tool.NewRegistry(nil, nil))
	var steerErr error
	var attempted bool
	result, err := engine.Run(t.Context(), "answer", func(event Event) error {
		if event.State == Completed && !attempted {
			attempted = true
			control, controlErr := engine.Control()
			if controlErr != nil {
				steerErr = controlErr
				return nil
			}
			steerErr = control.Steer("too late")
		}
		return nil
	})
	if err != nil || result.State != Completed {
		t.Fatalf("Run() = (%v, %v)", result.State, err)
	}
	if !attempted {
		t.Fatal("terminal event was not observed")
	}
	if steerErr == nil {
		t.Fatal("Steer() during terminalization succeeded and would be dropped")
	}
}
