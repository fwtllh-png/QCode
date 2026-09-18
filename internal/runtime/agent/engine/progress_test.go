package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	completiontool "github.com/fwtllh-png/QCode/internal/adapter/tool/completion"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/interact"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestProgressSignatureDoesNotCountReadsWhenImplementWorkIsOpen(
	t *testing.T,
) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	engine.turn = 7
	engine.setPlan(interact.Plan{Steps: []interact.PlanStep{
		{Title: "audit", Status: interact.StepDone},
		{Title: "fix overflow", Status: interact.StepPending},
	}})
	answer := newEngineTurnKernel(
		protocol.TurnIntentAnswer,
		"act",
		nil,
		0,
		nil,
		nil,
	)
	before := engine.progressSignature(answer)
	if err := answer.BindWorkItem(turnkernel.BindWorkItem{
		KnownReads: map[string]turnkernel.WorkItemRead{"a.go": {Window: "full"}},
	}); err != nil {
		t.Fatal(err)
	}
	if after := engine.progressSignature(answer); after != before {
		t.Fatal("new file_read path renewed implement-work progress")
	}
}

func TestApplyImplementProgressLeaseTightensFinishOnly(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	engine.options.ImplementNoProgressSamples = 6
	engine.setPlan(interact.Plan{Steps: []interact.PlanStep{
		{Title: "audit", Status: interact.StepDone},
		{Title: "fix overflow", Status: interact.StepPending},
	}})
	spec := TurnSpec{
		Kernel: turnkernel.Policy{
			Convergence: turnkernel.ConvergencePolicyForStepLimit(64),
		},
	}
	engine.applyImplementProgressLease(&spec)
	if spec.Kernel.ImplementNoProgressSamples != 6 {
		t.Fatalf("implement lease samples = %d", spec.Kernel.ImplementNoProgressSamples)
	}
	if spec.Kernel.Convergence != turnkernel.ConvergencePolicyForStepLimit(64) {
		t.Fatalf("prepare-time lease mutated convergence: %+v", spec.Kernel.Convergence)
	}
	idle := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	idle.options.ImplementNoProgressSamples = 0
	unchanged := TurnSpec{
		Kernel: turnkernel.Policy{
			Convergence: turnkernel.ConvergencePolicyForStepLimit(64),
		},
	}
	want := unchanged.Kernel
	idle.applyImplementProgressLease(&unchanged)
	if unchanged.Kernel != want {
		t.Fatalf(
			"zero implement lease changed %+v, want %+v",
			unchanged.Kernel,
			want,
		)
	}
}

func TestProgressSignatureCountsResearchReadsOnlyForResearchTurns(
	t *testing.T,
) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	engine.turn = 7
	answer := newEngineTurnKernel(
		protocol.TurnIntentAnswer,
		"act",
		nil,
		0,
		nil,
		nil,
	)
	workspace := newEngineTurnKernel(
		protocol.TurnIntentWorkspaceChange,
		"act",
		nil,
		0,
		nil,
		nil,
	)
	answerBefore := engine.progressSignature(answer)
	workspaceBefore := engine.progressSignature(workspace)

	bind := turnkernel.BindWorkItem{
		KnownReads: map[string]turnkernel.WorkItemRead{"a.go": {Window: "full"}},
	}
	if err := answer.BindWorkItem(bind); err != nil {
		t.Fatal(err)
	}
	if err := workspace.BindWorkItem(bind); err != nil {
		t.Fatal(err)
	}

	if answerAfter := engine.progressSignature(answer); answerAfter == answerBefore {
		t.Fatal("new research path did not advance answer progress")
	}
	if workspaceAfter := engine.progressSignature(workspace); workspaceAfter != workspaceBefore {
		t.Fatal("read-only exploration advanced workspace-change progress")
	}
}

func TestProgressSignatureDoesNotRenewForMutationRevisionAlone(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	kernel := newEngineTurnKernel(
		protocol.TurnIntentWorkspaceChange,
		"act",
		nil,
		0,
		nil,
		nil,
	)
	before := engine.progressSignature(kernel)
	var afterFirst string
	for index := range 2 {
		call := provider.ToolCall{
			ID:   fmt.Sprintf("write-%d", index),
			Name: "file_apply",
		}
		if err := kernel.StartTools([]provider.ToolCall{call}); err != nil {
			t.Fatal(err)
		}
		if err := kernel.StartTool(call.ID); err != nil {
			t.Fatal(err)
		}
		if err := kernel.CloseTool(call, tool.Result{
			Outcome: &tool.Outcome{Facts: &tool.OutcomeFacts{
				WorkspaceChanges: []tool.WorkspaceChange{{
					Path: "same.go", Kind: tool.WorkspaceModified,
				}},
			}},
		}, nil); err != nil {
			t.Fatal(err)
		}
		after := engine.progressSignature(kernel)
		if index == 0 {
			if after == before {
				t.Fatal("first edited path did not enter the Work Item signature")
			}
			afterFirst = after
			continue
		}
		if after != afterFirst {
			t.Fatalf("same-path edit renewed progress: first=%q after=%q",
				afterFirst, after)
		}
	}
}

func TestProgressSignatureCountsSuccessfulAgentLifecycleCalls(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	kernel := newEngineTurnKernel(
		protocol.TurnIntentAnswer,
		"act",
		nil,
		0,
		nil,
		nil,
	)
	before := engine.progressSignature(kernel)
	call := provider.ToolCall{ID: "spawn", Name: "spawn_agent"}
	if err := kernel.StartTools([]provider.ToolCall{call}); err != nil {
		t.Fatal(err)
	}
	if err := kernel.StartTool(call.ID); err != nil {
		t.Fatal(err)
	}
	if err := kernel.CloseTool(call, tool.Result{}, nil); err != nil {
		t.Fatal(err)
	}
	if after := engine.progressSignature(kernel); after != before {
		t.Fatal("agent lifecycle call renewed Work Item path-set progress")
	}
}

func TestProgressSignatureOnlyRenewsForMonotonicPlanProgress(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	kernel := newEngineTurnKernel(
		protocol.TurnIntentWorkspaceChange,
		"act",
		nil,
		0,
		nil,
		nil,
	)
	engine.setPlan(interact.Plan{Steps: []interact.PlanStep{{
		Title: "Implement parser", Status: interact.StepPending,
	}}})
	pending := engine.progressSignature(kernel)

	engine.setPlan(interact.Plan{Steps: []interact.PlanStep{{
		Title: "Implement parser", Status: interact.StepInProgress,
	}}})
	inProgress := engine.progressSignature(kernel)

	engine.setPlan(interact.Plan{Steps: []interact.PlanStep{{
		Title: "Implement parser", Status: interact.StepDone,
	}}})
	done := engine.progressSignature(kernel)
	if pending != inProgress || inProgress == done {
		t.Fatalf(
			"plan status signatures renewed without durable progress: pending=%q in_progress=%q done=%q",
			pending,
			inProgress,
			done,
		)
	}
}

func TestFinishOnlyAllowsMutationAndVerificationCommands(t *testing.T) {
	for _, test := range []struct {
		name       string
		capability tool.Capability
		want       bool
	}{
		{name: "file_apply", capability: tool.CapabilityWrite, want: true},
		{name: "exec_command", capability: tool.CapabilityRead, want: true},
		{name: "file_read", capability: tool.CapabilityRead, want: true},
		{name: "exec_command", capability: tool.CapabilityProcess, want: true},
		{name: "write_stdin", capability: tool.CapabilityProcess, want: true},
		{name: "request_user_input", capability: tool.CapabilityRead, want: true},
		{name: "wait_agent", capability: tool.CapabilityRead, want: true},
		{name: "list_agents", capability: tool.CapabilityRead, want: true},
		{name: "shell_read", capability: tool.CapabilityRead, want: false},
		{name: "search_text", capability: tool.CapabilityRead, want: false},
		{name: "git_diff", capability: tool.CapabilityRead, want: false},
		{name: "git_status", capability: tool.CapabilityRead, want: false},
	} {
		if got := tool.FinishOnlyAllowed(test.name, tool.Descriptor{
			Name: test.name, Capability: test.capability,
		}); got != test.want {
			t.Fatalf(
				"tool.FinishOnlyAllowed(%q) = %v, want %v",
				test.name,
				got,
				test.want,
			)
		}
	}
}

func TestWorkspaceTurnFinalizesAfterNoProgressBudget(t *testing.T) {
	// The first distinct tool-result digest renews the clock, so the script
	// needs one extra identical echo before the MaxSteps+repair lease
	// (64+5) can exhaust.
	streams := make([]provider.Stream, 0, 70)
	for index := range 69 {
		streams = append(streams, toolCallStream(
			fmt.Sprintf("call-%d", index),
			"echo",
			`{"text":"read"}`,
		))
	}
	streams = append(streams, toolCallStream(
		"incomplete-1",
		completiontool.Name,
		`{"status":"incomplete","summary":"No workspace change was produced.","pending_actions":["Apply the requested workspace change."]}`,
	))
	runtime := &scriptedProvider{streams: streams}
	registry := tool.NewRegistry(nil, nil)
	for _, executor := range []tool.Executor{
		&echoTool{}, &completiontool.Tool{},
	} {
		if err := registry.Register(executor); err != nil {
			t.Fatal(err)
		}
	}
	engine := newEngine(t, runtime, registry)
	engine.options.MaxSteps = 64
	route := mustTestRouteWithContext(t, 65_536)
	engine.options.Route = route
	routes, err := model.NewRouteSet(route, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	engine.options.Routes = routes

	var terminal Event
	_, err = engine.RunForTurnWithIntentAndAttachments(
		t.Context(),
		"no-progress-turn",
		"modify the workspace",
		protocol.TurnIntentWorkspaceChange,
		nil,
		func(event Event) error {
			if event.State == Failed {
				terminal = event
			}
			return nil
		},
	)
	if err == nil ||
		protocol.CodeOf(err) != protocol.CodeConflict ||
		terminal.Convergence == nil ||
		terminal.Convergence.Cause != string(turnkernel.ConvergenceNoProgress) {
		t.Fatalf("Run() error = %v", err)
	}
	if len(runtime.requests) != 70 {
		t.Fatalf("provider requests = %d, want 70", len(runtime.requests))
	}
	assertProgressFeedback := func(requestIndex int, stage string) {
		t.Helper()
		for _, message := range runtime.requests[requestIndex].Messages {
			if message.Role == provider.RoleUser &&
				strings.Contains(message.Text(), "[no_progress]") &&
				strings.Contains(message.Text(), "stage="+stage) {
				return
			}
		}
		t.Fatalf(
			"request %d has no %s progress feedback",
			requestIndex,
			stage,
		)
	}
	assertProgressFeedback(24, "converge")
	assertProgressFeedback(47, "finish_only")
}

func TestReadOnlyTurnEntersFinishOnlyAtDerivedBudget(t *testing.T) {
	streams := make([]provider.Stream, 0, 46)
	for index := range 45 {
		streams = append(streams, toolCallStream(
			fmt.Sprintf("call-%d", index),
			"echo",
			`{"text":"read"}`,
		))
	}
	streams = append(streams, textStream("bounded final answer"))
	runtime := &scriptedProvider{streams: streams}
	registry := tool.NewRegistry(nil, nil)
	if err := registry.Register(&echoTool{}); err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, runtime, registry)
	engine.options.MaxSteps = 64

	result, err := engine.RunForTurnWithIntentAndAttachments(
		t.Context(),
		"bounded-read-only",
		"analyze the repository",
		protocol.TurnIntentAnswer,
		nil,
		func(Event) error { return nil },
	)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Text != "bounded final answer" || len(runtime.requests) != 46 {
		t.Fatalf("result=%+v requests=%d", result, len(runtime.requests))
	}
	if len(runtime.requests[45].Tools) == 0 {
		t.Fatalf("finish-only request stripped the catalog: %+v", runtime.requests[45].Tools)
	}
}

func TestReadOnlyFinishOnlyCompletesCurrentProcess(t *testing.T) {
	streams := make([]provider.Stream, 0, 47)
	for index := range 45 {
		streams = append(streams, toolCallStream(
			fmt.Sprintf("read-%d", index),
			"echo",
			`{"text":"read"}`,
		))
	}
	streams = append(
		streams,
		toolCallStream("exec", "exec_command", `{}`),
		textStream("process completed"),
	)
	runtime := &scriptedProvider{streams: streams}
	registry := tool.NewRegistry(nil, nil)
	for _, executor := range []tool.Executor{&echoTool{}, finishProcessTool{}} {
		if err := registry.Register(executor); err != nil {
			t.Fatal(err)
		}
	}
	engine := newEngine(t, runtime, registry)
	engine.options.MaxSteps = 64

	result, err := engine.RunForTurnWithIntentAndAttachments(
		t.Context(),
		"finish-current-process",
		"analyze then complete the configured command",
		protocol.TurnIntentAnswer,
		nil,
		func(Event) error { return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "process completed" || len(runtime.requests) != 47 {
		t.Fatalf("result=%+v requests=%d", result, len(runtime.requests))
	}
	names := make(map[string]bool)
	for _, definition := range runtime.requests[45].Tools {
		names[definition.Name] = true
	}
	if !names["exec_command"] || !names["echo"] {
		t.Fatalf("finish-only tools = %+v", names)
	}
}

func TestAcceptedCompletionPublishesSummaryWithoutFinalAnswerSampleAtLimit(
	t *testing.T,
) {
	streams := make([]provider.Stream, 0, 16)
	for index := range 11 {
		streams = append(streams, toolCallStream(
			fmt.Sprintf("read-%d", index),
			"echo",
			fmt.Sprintf(`{"text":"read-%d"}`, index),
		))
	}
	for index := range 4 {
		streams = append(streams, toolCallStream(
			fmt.Sprintf("quality-%d", index),
			"exec_command",
			`{"covered_paths":["a.go"]}`,
		))
	}
	streams = append(
		streams,
		toolCallStream("complete", "turn_complete", `{
			"status":"complete",
			"summary":"analysis complete",
			"pending_actions":[]
		}`),
	)
	runtime := &scriptedProvider{streams: streams}
	registry := declarationRegistry(t, true)
	if err := registry.Register(&echoTool{}); err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, runtime, registry)
	engine.options.MaxSteps = 64

	result, err := engine.RunForTurnWithIntentAndAttachments(
		t.Context(),
		"bounded-completion",
		"analyze the repository",
		protocol.TurnIntentAnswer,
		nil,
		func(Event) error { return nil },
	)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Text != "analysis complete" || len(runtime.requests) != 16 {
		t.Fatalf("result=%+v requests=%d", result, len(runtime.requests))
	}
}

func TestIdenticalToolRepeatsUseImplementLease(t *testing.T) {
	for _, test := range []struct {
		name     string
		executor tool.Executor
	}{
		{"stable output", &echoTool{}},
		{"noisy output", &noisyProgressTool{}},
	} {
		t.Run(test.name, func(t *testing.T) { testIdenticalToolRepeatsUseImplementLease(t, test.executor) })
	}
}

type noisyProgressTool struct{ echoTool }

func (t *noisyProgressTool) Execute(_ context.Context, _ json.RawMessage) (tool.Result, error) {
	attempt := t.calls.Add(1)
	return tool.Result{Content: fmt.Sprintf("same failure at 12:00:%d in /tmp/attempt-%d; elapsed=%dms", attempt, attempt, attempt)}, nil
}

func testIdenticalToolRepeatsUseImplementLease(t *testing.T, executor tool.Executor) {
	streams := make([]provider.Stream, 0, 16)
	for index := range 14 {
		streams = append(streams, toolCallStream(
			fmt.Sprintf("echo-%d", index),
			"echo",
			`{"text":"read"}`,
		))
	}
	streams = append(streams, toolCallStream(
		"incomplete-1",
		completiontool.Name,
		`{"status":"incomplete","summary":"Still repeating the same read.","pending_actions":["Stop repeating echo."]}`,
	))
	runtime := &scriptedProvider{streams: streams}
	registry := tool.NewRegistry(nil, nil)
	for _, executor := range []tool.Executor{
		executor, &completiontool.Tool{},
	} {
		if err := registry.Register(executor); err != nil {
			t.Fatal(err)
		}
	}
	engine := newEngine(t, runtime, registry)
	engine.options.MaxSteps = 64
	engine.options.ImplementNoProgressSamples = 6
	route := mustTestRouteWithContext(t, 65_536)
	engine.options.Route = route
	routes, err := model.NewRouteSet(route, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	engine.options.Routes = routes

	var terminal Event
	_, err = engine.RunForTurnWithIntentAndAttachments(
		t.Context(),
		"identical-echo-lease",
		"analyze the repository",
		protocol.TurnIntentAnswer,
		nil,
		func(event Event) error {
			if event.State == Failed {
				terminal = event
			}
			return nil
		},
	)
	if err == nil ||
		protocol.CodeOf(err) != protocol.CodeConflict ||
		terminal.Convergence == nil ||
		terminal.Convergence.Cause != string(turnkernel.ConvergenceNoProgress) {
		t.Fatalf("Run() error = %v terminal=%+v", err, terminal.Convergence)
	}
	if len(runtime.requests) >= 22 {
		t.Fatalf("identical repeats used max_steps lease: requests=%d", len(runtime.requests))
	}
}
