package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"syscall"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	providerassembly "github.com/fwtllh-png/QCode/internal/adapter/provider/assembly"
	providerfixture "github.com/fwtllh-png/QCode/internal/adapter/provider/fixture"
	provideropenai "github.com/fwtllh-png/QCode/internal/adapter/provider/openai"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func duplicateToolArgumentsStream() provider.Stream {
	return &eventErrorStream{events: []provider.StreamEvent{
		{Type: provider.EventToolCallDelta, ToolCall: &provider.ToolCallFragment{
			ID: "invalid-edit", Name: "file_edit",
			Arguments: `{"path":"value.txt","old":"after","new":"must not execute","new"`,
		}},
	}}
}

func TestDuplicateToolArgumentMembersRecoverWithoutReplayingCompletedEdits(t *testing.T) {
	fixture := newWorkspaceCompletionFixture(t, 0, 4)
	fixture.engine.options.MaxRetries = 1
	fixture.provider.streams[2] = duplicateToolArgumentsStream()
	fixture.provider.streams = append(fixture.provider.streams,
		toolCallStream("check", "file_read", `{"path":"value.txt"}`), textStream("done"))
	result, err := fixture.engine.RunForTurn(t.Context(), "duplicate-recovery", "edit", nil)
	if err != nil || result.State != Completed || len(fixture.provider.requests) != 5 || len(result.Tools) != 3 {
		t.Fatalf("state=%s tools=%d requests=%d error=%v", result.State, len(result.Tools), len(fixture.provider.requests), err)
	}
	request := fixture.provider.requests[3]
	visible := joinMessageText(request.Messages)
	if !strings.Contains(visible, "[regenerate_tool_arguments]") || strings.Contains(visible, "must not execute") ||
		request.LogicalRequestID != fixture.provider.requests[2].LogicalRequestID || request.TransportAttempt != 2 {
		t.Fatal("repair did not isolate rejected arguments in a fresh response attempt")
	}
	if fixture.journal.HasDraft("duplicate-recovery") || fixture.contents(t) != "after\n" {
		t.Fatal("repair replayed an edit or left a failed draft")
	}
}

func TestRejectedToolArgumentRepairBudgetRetainsDraft(t *testing.T) {
	for _, limit := range []int{0, 2} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			fixture := newWorkspaceCompletionFixture(t, 0, 4)
			fixture.engine.options.MaxRetries = limit
			fixture.provider.streams = fixture.provider.streams[:2]
			for range limit + 1 {
				fixture.provider.streams = append(fixture.provider.streams, duplicateToolArgumentsStream())
			}
			var states []State
			result, err := fixture.engine.RunForTurn(t.Context(), "duplicate-members", "edit", func(event Event) error {
				states = append(states, event.State)
				return nil
			})
			var failure *provider.Failure
			if err == nil || result.State != Failed || !errors.As(err, &failure) ||
				failure.Code != provider.FailureMalformedResponse {
				t.Fatalf("state = %s, error = %v", result.State, err)
			}
			assertOneTerminal(t, states, Failed)
			problem := protocol.ProblemOf(err)
			if problem == nil || problem.Fault == nil || problem.Fault.Disposition != protocol.FaultResumeTurn ||
				problem.Fault.RetryOwner != protocol.FaultRetryOwnerHost ||
				!strings.Contains(problem.Message, "regeneration budget exhausted") {
				t.Fatalf("recovery problem = %+v", problem)
			}
			if len(fixture.provider.requests) != limit+3 || len(result.Tools) != 2 {
				t.Fatalf("requests = %d, executed tools = %d", len(fixture.provider.requests), len(result.Tools))
			}
			if !fixture.journal.HasDraft("duplicate-members") || fixture.contents(t) != "after\n" {
				t.Fatal("failure did not retain the preceding valid edit as a draft")
			}
		})
	}
}

func TestRejectedToolArgumentRepairBudgetSurvivesRestart(t *testing.T) {
	runtime, engine, echo, store := restartTestEngines(t, "workspace-restart", func(engine *Engine, runtime *scriptedProvider) {
		engine.options.MaxRetries = 1
		runtime.streams = []provider.Stream{runtime.streams[0], duplicateToolArgumentsStream(), runtime.streams[1]}
	})
	engine.options.MaxRetries = 1
	runtime.streams = []provider.Stream{duplicateToolArgumentsStream(), textStream("must not be sampled")}
	result, err := engine.RunForTurn(t.Context(), "turn-restart", "inspect the parser", nil)
	if err == nil || result.State != Failed || !strings.Contains(err.Error(), "regeneration budget exhausted (1/1)") ||
		len(runtime.requests) != 1 || echo.calls.Load() != 1 {
		t.Fatalf("restart reset budget or replayed work: result=%s requests=%d calls=%d err=%v", result.State, len(runtime.requests), echo.calls.Load(), err)
	}
	facts, err := store.LoadDomainFacts(t.Context(), "turn-restart")
	if err != nil {
		t.Fatal(err)
	}
	var authorized bool
	for _, fact := range facts {
		for _, sample := range fact.State.SampleLedger {
			if sample.Assembly != nil && len(sample.Assembly.ToolArgumentRepairs) == 1 {
				authorized = true
			}
		}
	}
	if !authorized {
		t.Fatal("tool argument repair authorization was not durable")
	}
}

func TestR3DisconnectAfterConfirmedChunkContinuesWithoutLoss(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		&eventErrorStream{
			events: []provider.StreamEvent{
				{Type: provider.EventMessageStart},
				{Type: provider.EventTextDelta, Text: "partial"},
			},
			err: io.ErrUnexpectedEOF,
		},
		textStream(" answer"),
	}}
	engine := newEngine(t, runtime, nil)
	result, err := engine.Run(t.Context(), "review", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "partial answer" || len(runtime.requests) != 2 {
		t.Fatalf("result=%+v requests=%d", result, len(runtime.requests))
	}
	if runtime.requests[0].Projection.Retry ||
		!runtime.requests[1].Projection.Retry ||
		runtime.requests[0].LogicalRequestID == "" ||
		runtime.requests[0].LogicalRequestID !=
			runtime.requests[1].LogicalRequestID ||
		runtime.requests[0].TransportAttempt != 1 ||
		runtime.requests[1].TransportAttempt != 2 {
		t.Fatalf(
			"request attribution: first=%+v second=%+v",
			runtime.requests[0],
			runtime.requests[1],
		)
	}
	var retained, feedback bool
	for _, message := range runtime.requests[1].Messages {
		switch {
		case message.Role == provider.RoleAssistant &&
			message.Text() == "partial":
			retained = true
		case message.Role == provider.RoleUser &&
			strings.Contains(
				message.Text(),
				"[continue_after_incomplete",
			):
			feedback = true
		}
	}
	if !retained || !feedback {
		t.Fatalf(
			"continuation lost confirmed data: %+v",
			runtime.requests[1].Messages,
		)
	}
}

func TestMaxTokensCreatesQuiescentTransportBoundary(t *testing.T) {
	store := turnkernel.NewMemoryTerminalEnvelopeStore(nil, nil)
	coordinators, err := turnkernel.NewStoreCoordinatorRuntime(store)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &scriptedProvider{streams: []provider.Stream{
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventTextDelta, Text: "partial"},
			{
				Type:       provider.EventMessageStop,
				StopReason: provider.StopReasonMaxTokens,
			},
		}},
		textStream(" answer"),
	}}
	engine := newEngine(t, runtime, nil)
	engine.options.TurnCoordinatorRuntime = coordinators
	result, err := engine.RunForTurn(
		t.Context(),
		"turn-max-token-boundary",
		"review",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "partial answer" {
		t.Fatalf("result = %+v", result)
	}
	facts, err := store.LoadDomainFacts(
		t.Context(),
		"turn-max-token-boundary",
	)
	if err != nil {
		t.Fatal(err)
	}
	var boundary *turnkernel.DomainFact
	for index := range facts {
		if facts[index].Command == "effect_requeued" {
			boundary = &facts[index]
			break
		}
	}
	if boundary == nil ||
		boundary.State.ActiveSampleID != "" ||
		boundary.State.SampleLedger["turn-1-step-1"].ProviderRetries != 0 {
		t.Fatalf("max-token boundary = %+v", boundary)
	}
}

func TestR3IncompleteToolFragmentIsRetainedAndExecutedOnlyAfterClosure(
	t *testing.T,
) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		&eventErrorStream{
			events: []provider.StreamEvent{
				{Type: provider.EventMessageStart},
				{
					Type: provider.EventToolCallDelta,
					ToolCall: &provider.ToolCallFragment{
						Index: 0, ID: "call-1", Name: "echo",
						Arguments: `{"text":`,
					},
				},
			},
			err: syscall.ECONNRESET,
		},
		toolCallStream(
			"call-1",
			"echo",
			`{"text":"evidence"}`,
		),
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventTextDelta, Text: "done"},
			{
				Type:       provider.EventMessageStop,
				StopReason: provider.StopReasonEndTurn,
			},
		}},
	}}
	executor := &echoTool{}
	registry := tool.NewRegistry(nil, nil)
	if err := registry.Register(executor); err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, runtime, registry)
	result, err := engine.Run(t.Context(), "review", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "done" ||
		executor.calls.Load() != 1 ||
		len(result.Tools) != 1 ||
		len(runtime.requests) != 3 {
		t.Fatalf(
			"result=%+v executions=%d requests=%d",
			result,
			executor.calls.Load(),
			len(runtime.requests),
		)
	}
	var rawFragment bool
	for _, message := range runtime.requests[1].Messages {
		if message.Role == provider.RoleUser &&
			strings.Contains(message.Text(), `"arguments":"{\"text\":"`) &&
			strings.Contains(message.Text(), "were not") {
			rawFragment = true
		}
	}
	if !rawFragment {
		t.Fatalf(
			"continuation lost raw tool fragment: %+v",
			runtime.requests[1].Messages,
		)
	}
}

func TestR3SparseProviderSequencesPreserveCompleteToolCalls(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{
				Type: provider.EventReasoningDelta, Text: "inspect",
				Sequenced: true, Sequence: 350,
			},
			{
				Type: provider.EventToolCallDelta,
				ToolCall: &provider.ToolCallFragment{
					Index: 0, ID: "call-1", Name: "echo",
					Arguments: `{"text":"evidence"}`,
				},
				Sequenced: true, Sequence: 377,
			},
			{
				Type:       provider.EventMessageStop,
				StopReason: provider.StopReasonToolUse,
				Sequenced:  true, Sequence: 380,
			},
		}},
		textStream("done"),
	}}
	executor := &echoTool{}
	registry := tool.NewRegistry(nil, nil)
	if err := registry.Register(executor); err != nil {
		t.Fatal(err)
	}
	result, err := newEngine(t, runtime, registry).Run(
		t.Context(),
		"review",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "done" ||
		executor.calls.Load() != 1 ||
		len(result.Tools) != 1 {
		t.Fatalf(
			"result=%+v executions=%d",
			result,
			executor.calls.Load(),
		)
	}
}

func TestR3FineGrainedDeltasAreCoalescedBeforeDurableCheckpoint(t *testing.T) {
	const deltaCount = 1_000
	events := make([]provider.StreamEvent, 0, deltaCount+2)
	for range deltaCount {
		events = append(events, provider.StreamEvent{
			Type: provider.EventReasoningDelta,
			Text: "x",
		})
	}
	events = append(events,
		provider.StreamEvent{
			Type: provider.EventTextDelta,
			Text: "done",
		},
		provider.StreamEvent{
			Type:       provider.EventMessageStop,
			StopReason: provider.StopReasonEndTurn,
		},
	)
	store := turnkernel.NewMemoryTerminalEnvelopeStore(nil, nil)
	coordinators, err := turnkernel.NewStoreCoordinatorRuntime(store)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &scriptedProvider{streams: []provider.Stream{
		&providerfixture.SliceStream{Events: events},
	}}
	engine := newEngine(t, runtime, nil)
	engine.options.TurnCoordinatorRuntime = coordinators
	result, err := engine.RunForTurn(
		t.Context(),
		"turn-r3-coalesced-checkpoint",
		"review",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "done" ||
		result.Reasoning != strings.Repeat("x", deltaCount) {
		t.Fatalf(
			"text=%q reasoning_length=%d",
			result.Text,
			len(result.Reasoning),
		)
	}
	facts, err := store.LoadDomainFacts(
		t.Context(),
		"turn-r3-coalesced-checkpoint",
	)
	if err != nil {
		t.Fatal(err)
	}
	var progress int
	var assembly *providerassembly.ResponseAssembly
	for _, fact := range facts {
		if fact.Command != "model_sample_progress_recorded" {
			continue
		}
		progress++
		assembly = fact.State.SampleLedger["turn-1-step-1"].Assembly
	}
	if progress >= deltaCount/20 || assembly == nil ||
		assembly.State != providerassembly.ResponseComplete ||
		assembly.EventCount() >= deltaCount/20 ||
		len(assembly.ConfirmedBlocks()) != 2 {
		t.Fatalf(
			"progress=%d assembly=%+v facts=%d",
			progress,
			assembly,
			len(facts),
		)
	}
}

func TestR3InterleavedDeltasAreBatchedBeforeDurableCheckpoint(t *testing.T) {
	const deltaCount = 128
	events := make([]provider.StreamEvent, 0, deltaCount+1)
	for index := range deltaCount {
		event := provider.StreamEvent{
			Type: provider.EventReasoningDelta,
			Text: strings.Repeat("r", 1<<10),
		}
		if index%2 != 0 {
			event.Type = provider.EventTextDelta
			event.Text = strings.Repeat("t", 1<<10)
		}
		events = append(events, event)
	}
	events = append(events, provider.StreamEvent{
		Type:       provider.EventMessageStop,
		StopReason: provider.StopReasonEndTurn,
	})
	store := turnkernel.NewMemoryTerminalEnvelopeStore(nil, nil)
	coordinators, err := turnkernel.NewStoreCoordinatorRuntime(store)
	if err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, &scriptedProvider{streams: []provider.Stream{
		&providerfixture.SliceStream{Events: events},
	}}, nil)
	engine.options.TurnCoordinatorRuntime = coordinators
	result, err := engine.RunForTurn(
		t.Context(),
		"turn-r3-interleaved-checkpoint",
		"review",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Text) != deltaCount/2*(1<<10) ||
		len(result.Reasoning) != deltaCount/2*(1<<10) {
		t.Fatalf(
			"text_length=%d reasoning_length=%d",
			len(result.Text),
			len(result.Reasoning),
		)
	}
	facts, err := store.LoadDomainFacts(
		t.Context(),
		"turn-r3-interleaved-checkpoint",
	)
	if err != nil {
		t.Fatal(err)
	}
	var progress int
	var progressBytes int
	for _, fact := range facts {
		if fact.Command == "model_sample_progress_recorded" {
			progress++
			encoded, marshalErr := json.Marshal(fact)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			progressBytes += len(encoded)
		}
	}
	if progress >= deltaCount/4 {
		t.Fatalf(
			"progress=%d deltas=%d, want at least 75%% reduction",
			progress,
			deltaCount,
		)
	}
	if progressBytes >= 2<<20 {
		t.Fatalf(
			"progress payload=%d bytes, want less than 2 MiB",
			progressBytes,
		)
	}
}

func TestR3TransportProgressIsBatchedBeforeDurableCheckpoint(t *testing.T) {
	const progressCount = 1_000
	events := make([]provider.StreamEvent, 0, progressCount+2)
	for range progressCount {
		events = append(events, provider.StreamEvent{
			Type: provider.EventTransportProgress,
		})
	}
	events = append(events,
		provider.StreamEvent{
			Type: provider.EventTextDelta,
			Text: "done",
		},
		provider.StreamEvent{
			Type:       provider.EventMessageStop,
			StopReason: provider.StopReasonEndTurn,
		},
	)
	store := turnkernel.NewMemoryTerminalEnvelopeStore(nil, nil)
	coordinators, err := turnkernel.NewStoreCoordinatorRuntime(store)
	if err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, &scriptedProvider{streams: []provider.Stream{
		&providerfixture.SliceStream{Events: events},
	}}, nil)
	engine.options.TurnCoordinatorRuntime = coordinators
	result, err := engine.RunForTurn(
		t.Context(),
		"turn-r3-transport-progress-checkpoint",
		"review",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "done" {
		t.Fatalf("text=%q", result.Text)
	}
	facts, err := store.LoadDomainFacts(
		t.Context(),
		"turn-r3-transport-progress-checkpoint",
	)
	if err != nil {
		t.Fatal(err)
	}
	var progress int
	var assembly *providerassembly.ResponseAssembly
	for _, fact := range facts {
		if fact.Command != "model_sample_progress_recorded" {
			continue
		}
		progress++
		assembly = fact.State.SampleLedger["turn-1-step-1"].Assembly
	}
	if progress >= progressCount/20 ||
		assembly == nil ||
		assembly.EventCount() != progressCount+2 {
		t.Fatalf(
			"progress=%d assembly=%+v facts=%d",
			progress,
			assembly,
			len(facts),
		)
	}
}

func TestR3EnginePersistsProviderRecoveryBoundaries(t *testing.T) {
	store := turnkernel.NewMemoryTerminalEnvelopeStore(nil, nil)
	coordinators, err := turnkernel.NewStoreCoordinatorRuntime(store)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &scriptedProvider{streams: []provider.Stream{
		textStream("durable"),
	}}
	engine := newEngine(t, runtime, nil)
	engine.options.TurnCoordinatorRuntime = coordinators
	result, err := engine.RunForTurn(
		t.Context(),
		"turn-r3-checkpoint",
		"review",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "durable" {
		t.Fatalf("result = %+v", result)
	}
	facts, err := store.LoadDomainFacts(
		t.Context(),
		"turn-r3-checkpoint",
	)
	if err != nil {
		t.Fatal(err)
	}
	var progress int
	var assembly *providerassembly.ResponseAssembly
	for _, fact := range facts {
		if fact.Command == "model_sample_progress_recorded" {
			progress++
			sample := fact.State.SampleLedger["turn-1-step-1"]
			assembly = sample.Assembly
		}
	}
	if progress < 2 || assembly == nil ||
		assembly.State != providerassembly.ResponseComplete ||
		assembly.EventCount() != 2 ||
		len(assembly.ConfirmedBlocks()) != 1 ||
		assembly.ConfirmedBlocks()[0].Text != "durable" {
		t.Fatalf(
			"progress=%d assembly=%+v facts=%d",
			progress,
			assembly,
			len(facts),
		)
	}
}

func TestR3CompleteAssemblyDoesNotRestartProviderTransport(t *testing.T) {
	assembly := providerassembly.NewResponseAssembly("sample-complete")
	if err := assembly.BeginTransport(provider.TransportMetadata{
		LogicalRequestID:   "sample-complete",
		TransportRequestID: "transport-1",
		Attempt:            1,
	}); err != nil {
		t.Fatal(err)
	}
	for _, event := range []provider.StreamEvent{
		{Type: provider.EventTextDelta, Text: "durable"},
		{Type: provider.EventMessageStop, StopReason: provider.StopReasonEndTurn},
	} {
		if _, err := assembly.Apply(event); err != nil {
			t.Fatal(err)
		}
	}
	engine := newEngine(t, &scriptedProvider{}, nil)
	scope := attachTestScope(t, engine)
	scope.spec.Request = TurnRequest{Prompt: "review"}
	catalog, err := engine.options.Tools.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	scope.spec.Catalog = catalog
	begun := 0
	history := []provider.Message{
		provider.TextMessage(provider.RoleUser, "review"),
	}
	blocks, _, _, _, err := engine.modelStep(
		t.Context(),
		&history,
		provider.Usage{},
		"sample-complete",
		"normal",
		modelRetryState{},
		false,
		false,
		nil,
		nil,
		nil,
		assembly,
		nil,
		func() error {
			begun++
			return nil
		},
		nil,
		func(State, Event) error { return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if begun != 0 || providerassembly.BlocksText(blocks) != "durable" {
		t.Fatalf("begin=%d blocks=%+v", begun, blocks)
	}
}

func TestR3CompleteToolAssemblyUsesFrozenCatalogBinding(t *testing.T) {
	registry := tool.NewRegistry(nil, nil)
	if err := registry.Register(&echoTool{}); err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, &scriptedProvider{}, registry)
	scope := attachTestScope(t, engine)
	scope.spec.Request = TurnRequest{Prompt: "echo evidence"}
	var err error
	scope.spec.Catalog, err = registry.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	binding, ok := scope.spec.Catalog.Binding("echo")
	if !ok {
		t.Fatal("echo binding is absent from frozen catalog")
	}
	assembly := providerassembly.NewResponseAssembly("sample-tool-complete")
	if err := assembly.BeginTransport(provider.TransportMetadata{
		LogicalRequestID:   "sample-tool-complete",
		TransportRequestID: "transport-1",
		Attempt:            1,
	}); err != nil {
		t.Fatal(err)
	}
	for _, event := range []provider.StreamEvent{
		{
			Type: provider.EventToolCallDelta,
			ToolCall: &provider.ToolCallFragment{
				Index: 0, ID: "call-1", Name: "echo",
				Arguments: `{"text":"evidence"}`,
			},
		},
		{
			Type:       provider.EventMessageStop,
			StopReason: provider.StopReasonToolUse,
		},
	} {
		if _, err := assembly.Apply(event); err != nil {
			t.Fatal(err)
		}
	}
	history := []provider.Message{
		provider.TextMessage(provider.RoleUser, "echo evidence"),
	}
	begun := 0
	_, calls, _, _, err := engine.modelStep(
		t.Context(),
		&history,
		provider.Usage{},
		"sample-tool-complete",
		"normal",
		modelRetryState{},
		false,
		false,
		nil,
		nil,
		nil,
		assembly,
		nil,
		func() error {
			begun++
			return nil
		},
		nil,
		func(State, Event) error { return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if begun != 0 || len(calls) != 1 ||
		calls[0].CatalogID != binding.CatalogID ||
		calls[0].CatalogGeneration != binding.Generation ||
		calls[0].CatalogRevision != binding.Revision ||
		calls[0].CatalogAuthority != binding.Authority {
		t.Fatalf(
			"begin=%d calls=%+v binding=%+v",
			begun,
			calls,
			binding,
		)
	}
}

func TestR3ChatDoneWithoutFinishReasonCompletesLogicalSample(t *testing.T) {
	input := strings.Join([]string{
		`data: {"choices":[{"delta":{"reasoning_content":"think "},"finish_reason":null}]}`,
		"",
		`data: {"choices":[{"delta":{"content":"hello"},"finish_reason":null}]}`,
		"",
		`data: {"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":1,"completion_tokens_details":{"reasoning_tokens":1}}}`,
		"",
		`data: [DONE]`,
		"",
		"",
	}, "\n")
	stream, err := provideropenai.NewStream(
		io.NopCloser(strings.NewReader(input)),
		model.ProtocolOpenAIChat,
	)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &scriptedProvider{streams: []provider.Stream{stream}}
	result, err := newEngine(t, runtime, nil).Run(
		t.Context(),
		"say hello",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "hello" || result.Reasoning != "think " ||
		len(runtime.requests) != 1 {
		t.Fatalf(
			"result=%+v requests=%d",
			result,
			len(runtime.requests),
		)
	}
}

type eventErrorStream struct {
	events []provider.StreamEvent
	err    error
}

func (s *eventErrorStream) Recv() (provider.StreamEvent, error) {
	if len(s.events) != 0 {
		event := s.events[0]
		s.events = s.events[1:]
		return event, nil
	}
	if s.err != nil {
		err := s.err
		s.err = nil
		return provider.StreamEvent{}, err
	}
	return provider.StreamEvent{}, io.EOF
}

func (*eventErrorStream) Close() error { return nil }
