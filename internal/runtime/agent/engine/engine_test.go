package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	providerfixture "github.com/fwtllh-png/QCode/internal/adapter/provider/fixture"
	"github.com/fwtllh-png/QCode/internal/adapter/provider/httpclient"
	"github.com/fwtllh-png/QCode/internal/adapter/provider/openai"
	providerrouter "github.com/fwtllh-png/QCode/internal/adapter/provider/router"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	completiontool "github.com/fwtllh-png/QCode/internal/adapter/tool/completion"
	filetool "github.com/fwtllh-png/QCode/internal/adapter/tool/file"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/interact"
	webtool "github.com/fwtllh-png/QCode/internal/adapter/tool/web"
	"github.com/fwtllh-png/QCode/internal/observability/diagnostics"
	"github.com/fwtllh-png/QCode/internal/persist/contentstore"
	"github.com/fwtllh-png/QCode/internal/persist/workspacejournal"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	promptcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/prompt"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	"github.com/fwtllh-png/QCode/internal/security/controlmatrix"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

type engineSandboxBackend struct{ root string }

func (b engineSandboxBackend) Capability() sandbox.Capability {
	return sandbox.Capability{
		Platform: "fixture", Backend: "fixture",
		Available: true,
		Effective: controlmatrix.Matrix{FilesystemRead: controlmatrix.FilesystemReadDeclaredRoots,

			FilesystemWrite: controlmatrix.
				FilesystemWriteExactPaths,

			Network: controlmatrix.
				NetworkDenied,

			ProcessTree: controlmatrix.
				ProcessTreeGroupKill,

			CrossProcess: controlmatrix.CrossProcessUnrestricted,
			Syscall:      controlmatrix.SyscallDenyDangerous, IPC: controlmatrix.
					IPCUnrestricted, PathIdentity: controlmatrix.PathIdentityDescriptorRelative,
			ArtifactOrigin: controlmatrix.ArtifactOriginUnverifiedPath, DurableRecovery: controlmatrix.DurableRecoveryMemoryOnly},
	}
}

func (b engineSandboxBackend) Prepare(_ context.Context, command sandbox.Command) (sandbox.Command, error) {
	command.PreparedPolicyID = "engine-fixture"
	return command, nil
}

func TestEngineExecutesToolAndFeedsResultOnce(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventMessageStart},
			{Type: provider.EventToolCallDelta, ToolCall: &provider.ToolCallFragment{
				Index: 0, ID: "call_1", Name: "echo", Arguments: `{"text":"hello"}`,
			}},
			{Type: provider.EventMessageStop},
		}},
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventMessageStart},
			{Type: provider.EventTextDelta, Text: "done"},
			{Type: provider.EventMessageStop},
		}},
	}}
	executor := &echoTool{}
	registry := tool.NewRegistry(nil, nil)
	if err := registry.Register(executor); err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, runtime, registry)
	var states []State
	result, err := engine.Run(t.Context(), "work", func(event Event) error {
		states = append(states, event.State)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "done" || result.State != Completed || executor.calls.Load() != 1 {
		t.Fatalf("result=%+v calls=%d", result, executor.calls.Load())
	}
	if len(runtime.requests) != 2 {
		t.Fatalf("requests = %+v", runtime.requests)
	}
	var toolMessage provider.Message
	for _, message := range runtime.requests[1].Messages {
		if message.Role == provider.RoleTool {
			toolMessage = message
			break
		}
	}
	if toolMessage.Role != provider.RoleTool || messageToolResultID(toolMessage) != "call_1" {
		t.Fatalf("tool message = %+v", toolMessage)
	}
	toolResult := toolMessage.Blocks[0].ToolResult
	if toolResult.Admission == nil ||
		toolResult.Admission.Reason != "inline" ||
		toolResult.Admission.Digest == "" ||
		strings.Contains(toolResult.Content, `"admission"`) {
		t.Fatalf("tool result admission = %+v", toolResult)
	}
	assertOneTerminal(t, states, Completed)
}

func TestEnginePreservesProcessSessionForNextProviderSample(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventToolCallDelta, ToolCall: &provider.ToolCallFragment{
				Index: 0, ID: "process-call", Name: "exec_command",
				Arguments: `{}`,
			}},
			{Type: provider.EventMessageStop},
		}},
		textStream("done"),
	}}
	registry := tool.NewRegistry(nil, nil)
	if err := registry.Register(processSessionTool{}); err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, runtime, registry)
	if _, err := engine.Run(t.Context(), "work", nil); err != nil {
		t.Fatal(err)
	}
	if len(runtime.requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(runtime.requests))
	}
	for _, message := range runtime.requests[1].Messages {
		if messageToolResultID(message) != "process-call" {
			continue
		}
		var projected tool.Result
		if err := json.Unmarshal(
			[]byte(message.Blocks[0].ToolResult.Content),
			&projected,
		); err != nil {
			t.Fatal(err)
		}
		if projected.Metadata["session_id"] != "session-1" ||
			projected.Metadata["cursor"] != float64(7) ||
			projected.Metadata["running"] != true ||
			projected.Outcome != nil {
			t.Fatalf("process projection = %#v", projected)
		}
		return
	}
	t.Fatal("second provider request has no process Tool Result")
}

func TestEngineReplaysCanonicalDuplicateToolCallsWithinTurn(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventToolCallDelta, ToolCall: &provider.ToolCallFragment{
				Index: 0, ID: "call_1", Name: "lookup",
				Arguments: `{"a":1,"b":2}`,
			}},
			{Type: provider.EventToolCallDelta, ToolCall: &provider.ToolCallFragment{
				Index: 1, ID: "call_2", Name: "lookup",
				Arguments: `{ "b": 2, "a": 1 }`,
			}},
			{Type: provider.EventMessageStop, StopReason: provider.StopReasonToolUse},
		}},
		textStream("done"),
	}}
	executor := &countingCatalogExecutor{descriptor: tool.Descriptor{
		Name: "lookup", Description: "deterministic lookup",
		Visibility: tool.VisibleModel, Capability: tool.CapabilityRead,
		AccessMode: tool.AccessRead, ParallelPolicy: tool.ParallelConcurrent,
		RepeatPolicy:       tool.RepeatReplaySameTurn,
		SandboxRequirement: tool.SandboxNone,
		Availability:       tool.AvailabilityAvailable,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"a": map[string]any{"type": "number"},
				"b": map[string]any{"type": "number"},
			},
			"required":             []string{"a", "b"},
			"additionalProperties": false,
		},
	}}
	registry := tool.NewRegistry(nil, nil)
	if err := registry.Register(executor); err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, runtime, registry)
	var results []Event
	if _, err := engine.Run(t.Context(), "look up twice", func(event Event) error {
		if event.ToolCall != nil && event.Result != nil {
			results = append(results, event)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if executor.calls.Load() != 1 {
		t.Fatalf("lookup executions = %d, want 1", executor.calls.Load())
	}
	if len(results) != 2 ||
		results[1].Result.Metadata["replayed_from_call_id"] != "call_1" {
		t.Fatalf("tool results = %+v", results)
	}
}

func TestEngineBlocksWhenCompletionRepairRemainsEmpty(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventMessageStop, StopReason: provider.StopReasonEndTurn},
		}},
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventMessageStop, StopReason: provider.StopReasonEndTurn},
		}},
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventMessageStop, StopReason: provider.StopReasonEndTurn},
		}},
		toolCallStream("incomplete-1", completiontool.Name, `{
			"status":"incomplete",
			"summary":"No user-facing answer was produced.",
			"pending_actions":["Produce the requested review."]
		}`),
	}}
	registry := tool.NewRegistry(nil, nil)
	if err := registry.Register(&completiontool.Tool{}); err != nil {
		t.Fatal(err)
	}
	var terminal Event
	result, err := newEngine(t, runtime, registry).Run(t.Context(), "review", func(event Event) error {
		if event.State == Failed {
			terminal = event
		}
		return nil
	})
	if err == nil || protocol.CodeOf(err) != protocol.CodeConflict {
		t.Fatalf("result=%+v err=%v, want blocked convergence", result, err)
	}
	if result.State != Failed ||
		terminal.Convergence == nil ||
		terminal.Convergence.Cause != string(turnkernel.ConvergenceRepairBudget) ||
		terminal.Completion == nil ||
		terminal.Completion.Status != "incomplete" {
		t.Fatalf("blocked result=%+v terminal=%+v", result, terminal)
	}
}

func TestEngineRepairsEmptyFinalResponse(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventMessageStop, StopReason: provider.StopReasonEndTurn},
		}},
		textStream("无法继续执行，但已明确说明原因。"),
	}}
	result, err := newEngine(t, runtime, nil).Run(t.Context(), "review", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "无法继续执行，但已明确说明原因。" ||
		len(runtime.requests) != 2 {
		t.Fatalf("result=%+v requests=%d", result, len(runtime.requests))
	}
	var foundFeedback bool
	for _, message := range runtime.requests[1].Messages {
		if message.Role == provider.RoleUser &&
			strings.Contains(message.Text(), "[completion_required]") {
			foundFeedback = true
			break
		}
	}
	if !foundFeedback {
		t.Fatalf("completion feedback missing from request: %+v", runtime.requests[1].Messages)
	}
}

func TestWorkspaceChangeIntentRejectsTextOnlyCompletion(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		textStream("I will make the change next."),
		textStream("I still did not change it."),
		toolCallStream("incomplete-1", completiontool.Name, `{
			"status":"incomplete",
			"summary":"No workspace mutation was produced.",
			"pending_actions":["Apply the requested workspace change."]
		}`),
	}}
	registry := tool.NewRegistry(nil, nil)
	if err := registry.Register(&completiontool.Tool{}); err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, runtime, registry)
	var states []State
	result, err := engine.RunForTurnWithIntentAndAttachments(
		t.Context(),
		"turn-1",
		"fix the bug",
		protocol.TurnIntentWorkspaceChange,
		nil,
		func(event Event) error {
			states = append(states, event.State)
			return nil
		},
	)
	if err == nil || protocol.CodeOf(err) != protocol.CodeConflict {
		t.Fatalf("Run() error = %v, want conflict", err)
	}
	if result.State != Failed {
		t.Fatalf("result state = %q, want failed", result.State)
	}
	assertOneTerminal(t, states, Failed)
	if len(engine.History()) == 0 {
		t.Fatal("blocked workspace change did not retain recovery context")
	}
}

func TestCompletionRepairHasIndependentStepBudget(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventToolCallDelta, ToolCall: &provider.ToolCallFragment{
				Index: 0, ID: "call_1", Name: "echo", Arguments: `{"text":"evidence"}`,
			}},
			{Type: provider.EventMessageStop, StopReason: provider.StopReasonToolUse},
		}},
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventMessageStop, StopReason: provider.StopReasonEndTurn},
		}},
		textStream("最终结论：验证完成。"),
	}}
	registry := tool.NewRegistry(nil, nil)
	if err := registry.Register(&echoTool{}); err != nil {
		t.Fatal(err)
	}
	engine, err := newTestEngine(Options{ProviderConfig: ProviderConfig{Provider: runtime, Route: testRoute(t),
		MaxOutputTokens: 128, MaxSteps: 2}, ToolConfig: ToolConfig{Tools: registry,

		Authorize: func(provider.ToolCall) bool { return true }},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := engine.Run(t.Context(), "review", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "最终结论：验证完成。" || len(runtime.requests) != 3 {
		t.Fatalf("result=%+v requests=%d", result, len(runtime.requests))
	}
}

func TestEngineContinuesIncompleteProviderStop(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventTextDelta, Text: "partial"},
			{Type: provider.EventMessageStop, StopReason: provider.StopReasonMaxTokens},
		}},
		textStream(" answer"),
	}}
	result, err := newEngine(t, runtime, nil).Run(t.Context(), "review", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "partial answer" || len(runtime.requests) != 2 {
		t.Fatalf("result=%+v requests=%d", result, len(runtime.requests))
	}
	var partialReplay, continuationFeedback bool
	for _, message := range runtime.requests[1].Messages {
		switch {
		case message.Role == provider.RoleAssistant && message.Text() == "partial":
			partialReplay = true
		case message.Role == provider.RoleUser &&
			strings.Contains(message.Text(), "[continue_after_incomplete"):
			continuationFeedback = true
		}
	}
	if !partialReplay || !continuationFeedback {
		t.Fatalf("continuation request = %+v", runtime.requests[1].Messages)
	}
}

func TestEngineDoesNotRepairCompletedPostToolContinuation(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		toolCallStream("call-1", "echo", `{"text":"evidence"}`),
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventTextDelta, Text: "完整结论的第一部分，"},
			{Type: provider.EventMessageStop, StopReason: provider.StopReasonMaxTokens},
		}},
		textStream("第二部分已完整结束。"),
	}}
	registry := tool.NewRegistry(nil, nil)
	if err := registry.Register(&echoTool{}); err != nil {
		t.Fatal(err)
	}

	result, err := newEngine(t, runtime, registry).Run(t.Context(), "review", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "完整结论的第一部分，第二部分已完整结束。" ||
		len(runtime.requests) != 3 {
		t.Fatalf("result=%+v requests=%d", result, len(runtime.requests))
	}
}

func TestEngineContinuesRepeatedReasoningLimitsAtSameEffort(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventReasoningDelta, Text: "completed analysis"},
			{Type: provider.EventMessageStop, StopReason: provider.StopReasonMaxTokens},
		}},
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventReasoningDelta, Text: " with more detail"},
			{Type: provider.EventMessageStop, StopReason: provider.StopReasonMaxTokens},
		}},
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventReasoningDelta, Text: " and final checks"},
			{Type: provider.EventMessageStop, StopReason: provider.StopReasonMaxTokens},
		}},
		toolCallStream("complete-1", completiontool.Name, `{
			"status":"complete",
			"summary":"final answer",
			"output_mode":"exact",
			"pending_actions":[]
		}`),
	}}
	registry := tool.NewRegistry(nil, nil)
	for _, executor := range []tool.Executor{
		&echoTool{}, &completiontool.Tool{},
	} {
		if err := registry.Register(executor); err != nil {
			t.Fatal(err)
		}
	}
	engine := newEngine(t, runtime, registry)
	engine.options.ReasoningEffort = "max"
	engine.options.Route = reasoningRoute(t)
	engine.options.Routes, _ = model.NewRouteSet(engine.options.Route, nil, false)

	var completedReasoning []ModelReasoning
	result, err := engine.Run(t.Context(), "review", func(event Event) error {
		if event.ReasoningCompleted != nil {
			completedReasoning = append(
				completedReasoning,
				*event.ReasoningCompleted,
			)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "final answer" ||
		result.Reasoning != "completed analysis with more detail and final checks" ||
		len(runtime.requests) != 4 {
		t.Fatalf("result=%+v requests=%d", result, len(runtime.requests))
	}
	if len(completedReasoning) != 1 ||
		completedReasoning[0].Text !=
			"completed analysis with more detail and final checks" ||
		completedReasoning[0].SampleID == "" {
		t.Fatalf("completed reasoning = %+v", completedReasoning)
	}
	if len(runtime.requests[0].Tools) == 0 {
		t.Fatal("normal reasoning sample did not receive tools")
	}
	continuation := runtime.requests[1]
	if continuation.ReasoningEffort != "max" || len(continuation.Tools) == 0 {
		t.Fatalf("same-effort continuation = %+v", continuation)
	}
	finish := runtime.requests[3]
	if finish.ReasoningEffort != "max" ||
		finish.MaxOutputTokens != runtime.requests[0].MaxOutputTokens {
		t.Fatalf("continued request = %+v", finish)
	}
	if len(finish.Tools) != len(runtime.requests[0].Tools) {
		t.Fatalf("continued tools = %+v", finish.Tools)
	}
	for _, message := range finish.Messages {
		if message.Role == provider.RoleUser &&
			strings.Contains(message.Text(), "[convergence_finalization]") {
			t.Fatalf("unexpected convergence feedback in %+v", finish.Messages)
		}
	}
}

func TestEngineDoesNotUseFinishRouteForPartialToolCall(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventToolCallDelta, ToolCall: &provider.ToolCallFragment{
				Index: 0, ID: "partial", Name: "echo", Arguments: `{"text":`,
			}},
			{Type: provider.EventMessageStop, StopReason: provider.StopReasonMaxTokens},
		}},
		textStream("recovered without replaying the partial call"),
	}}
	registry := tool.NewRegistry(nil, nil)
	if err := registry.Register(&echoTool{}); err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, runtime, registry)
	engine.options.ReasoningEffort = "max"
	engine.options.Route = reasoningRoute(t)
	engine.options.Routes, _ = model.NewRouteSet(engine.options.Route, nil, false)

	result, err := engine.Run(t.Context(), "review", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "recovered without replaying the partial call" ||
		len(runtime.requests) != 2 {
		t.Fatalf("result=%+v requests=%d", result, len(runtime.requests))
	}
	continuation := runtime.requests[1]
	if continuation.ReasoningEffort != "max" || len(continuation.Tools) == 0 {
		t.Fatalf("partial tool continuation entered finish route: %+v", continuation)
	}
	var foundSplitGuidance bool
	for _, message := range continuation.Messages {
		if message.Role == provider.RoleUser &&
			strings.Contains(message.Text(), "Split the operation into smaller") {
			foundSplitGuidance = true
		}
	}
	if !foundSplitGuidance {
		t.Fatalf("partial tool continuation lacks split guidance: %+v", continuation.Messages)
	}
}

func TestEngineConvergesRepeatedPartialToolCallsWithoutResourceExhaustion(
	t *testing.T,
) {
	partial := func(id string) provider.Stream {
		return &providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventToolCallDelta, ToolCall: &provider.ToolCallFragment{
				Index: 0, ID: id, Name: "echo", Arguments: `{"text":`,
			}},
			{Type: provider.EventMessageStop, StopReason: provider.StopReasonMaxTokens},
		}}
	}
	runtime := &scriptedProvider{streams: []provider.Stream{
		partial("partial-1"),
		partial("partial-2"),
		partial("partial-3"),
		toolCallStream("complete-1", completiontool.Name, `{
			"status":"complete",
			"summary":"Recovered through structured convergence.",
			"output_mode":"exact",
			"pending_actions":[]
		}`),
	}}
	registry := tool.NewRegistry(nil, nil)
	for _, executor := range []tool.Executor{
		&echoTool{},
		&completiontool.Tool{},
	} {
		if err := registry.Register(executor); err != nil {
			t.Fatal(err)
		}
	}
	result, err := newEngine(t, runtime, registry).Run(
		t.Context(),
		"review",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "Recovered through structured convergence." ||
		len(runtime.requests) != 4 {
		t.Fatalf("result=%+v requests=%d", result, len(runtime.requests))
	}
}

func TestEngineContinuesIncompleteOutputUntilProviderCompletes(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventTextDelta, Text: "one"},
			{Type: provider.EventMessageStop, StopReason: provider.StopReasonIncomplete},
		}},
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventTextDelta, Text: " two"},
			{Type: provider.EventMessageStop, StopReason: provider.StopReasonMaxTokens},
		}},
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventTextDelta, Text: " three"},
			{Type: provider.EventMessageStop, StopReason: provider.StopReasonIncomplete},
		}},
		textStream(" done"),
	}}
	var states []State
	result, err := newEngine(t, runtime, nil).Run(t.Context(), "review", func(event Event) error {
		states = append(states, event.State)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "one two three done" {
		t.Fatalf("result=%+v", result)
	}
	if len(runtime.requests) != 4 {
		t.Fatalf("requests=%d", len(runtime.requests))
	}
	assertOneTerminal(t, states, Completed)
}

func TestEngineDoesNotConvergeOnRepeatedIncompleteOutput(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventTextDelta, Text: "one"},
			{Type: provider.EventMessageStop, StopReason: provider.StopReasonMaxTokens},
		}},
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventTextDelta, Text: " two"},
			{Type: provider.EventMessageStop, StopReason: provider.StopReasonMaxTokens},
		}},
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventTextDelta, Text: " three"},
			{Type: provider.EventMessageStop, StopReason: provider.StopReasonMaxTokens},
		}},
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventTextDelta, Text: " closing"},
			{Type: provider.EventMessageStop, StopReason: provider.StopReasonMaxTokens},
		}},
		textStream(" done"),
	}}
	result, err := newEngine(t, runtime, nil).Run(t.Context(), "review", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "one two three closing done" {
		t.Fatalf("result=%+v", result)
	}
	if len(runtime.requests) != 5 {
		t.Fatalf("provider requests = %d", len(runtime.requests))
	}
}

func TestEngineDoesNotContinueContentFilterStop(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventTextDelta, Text: "blocked"},
			{Type: provider.EventMessageStop, StopReason: provider.StopReasonContentFilter},
		}},
	}}
	result, err := newEngine(t, runtime, nil).Run(t.Context(), "review", nil)
	if err == nil || protocol.CodeOf(err) != protocol.CodeInvalidArgument {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if len(runtime.requests) != 1 {
		t.Fatalf("requests=%d", len(runtime.requests))
	}
}

func TestEngineIncompleteContinuationCanResumeWithToolCall(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventTextDelta, Text: "checking"},
			{Type: provider.EventMessageStop, StopReason: provider.StopReasonMaxTokens},
		}},
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventToolCallDelta, ToolCall: &provider.ToolCallFragment{
				Index: 0, ID: "call_1", Name: "echo", Arguments: `{"text":"evidence"}`,
			}},
			{Type: provider.EventMessageStop, StopReason: provider.StopReasonToolUse},
		}},
		textStream("done"),
	}}
	executor := &echoTool{}
	registry := tool.NewRegistry(nil, nil)
	if err := registry.Register(executor); err != nil {
		t.Fatal(err)
	}
	result, err := newEngine(t, runtime, registry).Run(t.Context(), "review", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "done" || len(result.Tools) != 1 ||
		executor.calls.Load() != 1 || len(runtime.requests) != 3 {
		t.Fatalf(
			"result=%+v calls=%d requests=%d",
			result,
			executor.calls.Load(),
			len(runtime.requests),
		)
	}
}

func TestEngineRepairsInterruptedPostToolNarrationBeforeCompletion(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventToolCallDelta, ToolCall: &provider.ToolCallFragment{
				Index: 0, ID: "call_1", Name: "echo", Arguments: `{"text":"evidence"}`,
			}},
			{Type: provider.EventMessageStop, StopReason: provider.StopReasonToolUse},
		}},
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventTextDelta, Text: "所有事实齐备，现在提交修改："},
			{Type: provider.EventMessageStop, StopReason: provider.StopReasonMaxTokens},
		}},
		textStream("继续提交 file_apply 事务："),
		textStream("最终结论：修改未执行，工作区保持不变。"),
	}}
	registry := tool.NewRegistry(nil, nil)
	if err := registry.Register(&echoTool{}); err != nil {
		t.Fatal(err)
	}
	var completedText string
	result, err := newEngine(t, runtime, registry).Run(t.Context(), "review", func(event Event) error {
		if event.State == Completed {
			completedText = event.Text
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "最终结论：修改未执行，工作区保持不变。" ||
		completedText != result.Text || len(runtime.requests) != 4 {
		t.Fatalf(
			"result=%+v completed=%q requests=%d",
			result,
			completedText,
			len(runtime.requests),
		)
	}
	var foundFeedback bool
	for _, message := range runtime.requests[3].Messages {
		if message.Role == provider.RoleUser &&
			strings.Contains(message.Text(), "[completion_required]") {
			foundFeedback = true
			break
		}
	}
	if !foundFeedback {
		t.Fatalf("completion feedback missing from request: %+v", runtime.requests[3].Messages)
	}
}

func TestEnginePublishesAnswerAfterRecoveredToolFailure(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventToolCallDelta, ToolCall: &provider.ToolCallFragment{
				Index: 0, ID: "call_1", Name: "result_error", Arguments: `{}`,
			}},
			{Type: provider.EventMessageStop, StopReason: provider.StopReasonToolUse},
		}},
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventToolCallDelta, ToolCall: &provider.ToolCallFragment{
				Index: 0, ID: "call_2", Name: "echo", Arguments: `{"text":"fixed"}`,
			}},
			{Type: provider.EventMessageStop, StopReason: provider.StopReasonToolUse},
		}},
		textStream("最终结论：修正后的检查已通过。"),
	}}
	registry := tool.NewRegistry(nil, nil)
	if err := registry.Register(resultErrorTool{}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(&echoTool{}); err != nil {
		t.Fatal(err)
	}
	result, err := newEngine(t, runtime, registry).Run(t.Context(), "review", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "最终结论：修正后的检查已通过。" ||
		len(result.Tools) != 2 || len(runtime.requests) != 3 {
		t.Fatalf("result=%+v requests=%d", result, len(runtime.requests))
	}
	for index, request := range runtime.requests {
		if requestContains(request, "[tool_failure_resolution_required]") {
			t.Fatalf("recovered failure still repaired at request %d", index)
		}
	}
}

func TestEngineDoesNotClearToolFailureWithTextOnlyPromises(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventToolCallDelta, ToolCall: &provider.ToolCallFragment{
				Index: 0, ID: "call_1", Name: "result_error", Arguments: `{}`,
			}},
			{Type: provider.EventMessageStop, StopReason: provider.StopReasonToolUse},
		}},
		textStream("I will retry the failed edit next."),
		textStream("The remaining fix still needs to be applied."),
		textStream("Continuing with the repair."),
		toolCallStream("incomplete-1", completiontool.Name, `{
			"status":"incomplete",
			"summary":"The structured tool failure remains unresolved.",
			"pending_actions":["Retry the failed operation with corrected input."]
		}`),
	}}
	registry := tool.NewRegistry(nil, nil)
	for _, executor := range []tool.Executor{
		resultErrorTool{}, &completiontool.Tool{},
	} {
		if err := registry.Register(executor); err != nil {
			t.Fatal(err)
		}
	}

	result, err := newEngine(t, runtime, registry).RunForTurnWithIntentAndAttachments(
		t.Context(), "turn-failure", "fix it",
		protocol.TurnIntentOperation, nil, nil,
	)

	if err == nil || protocol.CodeOf(err) != protocol.CodeConflict {
		t.Fatalf("result=%+v error=%v, want conflict", result, err)
	}
	if result.State == Completed {
		t.Fatalf("text-only promises cleared a structured tool failure: %+v", result)
	}
}

func TestEngineCompletesRecoveredFailureFromCapturedText(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventToolCallDelta, ToolCall: &provider.ToolCallFragment{
				Index: 0, ID: "call_1", Name: "result_error", Arguments: `{}`,
			}},
			{Type: provider.EventMessageStop, StopReason: provider.StopReasonToolUse},
		}},
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventToolCallDelta, ToolCall: &provider.ToolCallFragment{
				Index: 0, ID: "call_2", Name: "echo", Arguments: `{"text":"recovered"}`,
			}},
			{Type: provider.EventMessageStop, StopReason: provider.StopReasonToolUse},
		}},
		textStream("最终结论：恢复和核实均已完成。"),
	}}
	registry := tool.NewRegistry(nil, nil)
	if err := registry.Register(resultErrorTool{}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(&echoTool{}); err != nil {
		t.Fatal(err)
	}

	result, err := newEngine(t, runtime, registry).Run(t.Context(), "review", nil)

	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "最终结论：恢复和核实均已完成。" ||
		len(result.Tools) != 2 || len(runtime.requests) != 3 {
		t.Fatalf("result=%+v requests=%d", result, len(runtime.requests))
	}
	for index, request := range runtime.requests {
		if requestContains(request, "[tool_failure_resolution_required]") {
			t.Fatalf("recovered failure still repaired at request %d", index)
		}
	}
}

func TestEngineResetsCompletionRepairBudgetAfterToolProgress(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventToolCallDelta, ToolCall: &provider.ToolCallFragment{
				Index: 0, ID: "call_1", Name: "result_error", Arguments: `{}`,
			}},
			{Type: provider.EventMessageStop, StopReason: provider.StopReasonToolUse},
		}},
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventToolCallDelta, ToolCall: &provider.ToolCallFragment{
				Index: 0, ID: "call_2", Name: "echo", Arguments: `{"text":"first recovered"}`,
			}},
			{Type: provider.EventMessageStop, StopReason: provider.StopReasonToolUse},
		}},
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventToolCallDelta, ToolCall: &provider.ToolCallFragment{
				Index: 0, ID: "call_3", Name: "result_error", Arguments: `{}`,
			}},
			{Type: provider.EventMessageStop, StopReason: provider.StopReasonToolUse},
		}},
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventToolCallDelta, ToolCall: &provider.ToolCallFragment{
				Index: 0, ID: "call_4", Name: "echo", Arguments: `{"text":"second recovered"}`,
			}},
			{Type: provider.EventMessageStop, StopReason: provider.StopReasonToolUse},
		}},
		textStream("Final result: both independent failures were recovered."),
	}}
	registry := tool.NewRegistry(nil, nil)
	if err := registry.Register(resultErrorTool{}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(&echoTool{}); err != nil {
		t.Fatal(err)
	}

	result, err := newEngine(t, runtime, registry).RunForTurnWithIntentAndAttachments(
		t.Context(), "turn-two-failures", "inspect it",
		protocol.TurnIntentOperation, nil, nil,
	)

	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "Final result: both independent failures were recovered." ||
		len(result.Tools) != 4 || len(runtime.requests) != 5 {
		t.Fatalf("result=%+v requests=%d", result, len(runtime.requests))
	}
}

func TestRunToolsReturnsReadFailureToModelAndClosesEveryStartedCall(t *testing.T) {
	registry := tool.NewRegistry(nil, nil)
	if err := registry.Register(&echoTool{}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(failingTool{}); err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, &scriptedProvider{}, registry)
	kernel := newEngineTurnKernel(
		protocol.TurnIntentAnswer,
		"act",
		nil,
		0,
		nil,
		nil,
	)
	var emitted []tool.Result

	_, err := engine.runToolsWithCache(
		t.Context(),
		"turn-test",
		[]provider.ToolCall{
			{ID: "call_ok", Name: "echo", Arguments: `{"text":"hello"}`},
			{ID: "call_fail", Name: "fail", Arguments: `{}`},
		},
		make(map[string]tool.Result),
		&toolResultCache{},
		kernel,
		func(_ State, event Event) error {
			if event.Result != nil {
				emitted = append(emitted, *event.Result)
			}
			return nil
		},
	)

	if err != nil {
		t.Fatalf("runTools() error = %v", err)
	}
	if len(emitted) != 2 {
		t.Fatalf("runTools() emitted %d results, want 2", len(emitted))
	}
	if emitted[0].IsError || !emitted[1].IsError {
		t.Fatalf("emitted results = %+v", emitted)
	}
	if category, _ := emitted[1].Metadata["error_category"].(string); category != "tool_execution_failed" {
		t.Fatalf("fatal error_category = %q", category)
	}
	if len(kernel.Snapshot().OpenCalls) != 0 ||
		len(kernel.Snapshot().ClosedCalls) != 2 {
		t.Fatalf("kernel tool ledger = %+v", kernel.Snapshot())
	}
}

func TestRunToolsEnforcesRecordedEconomicSurfaceBudget(t *testing.T) {
	registry := tool.NewRegistry(nil, nil)
	if err := registry.Register(largeResultTool{}); err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, &scriptedProvider{}, registry)
	scope := engine.executionScope()
	scope.mu.Lock()
	scope.state.toolSurfaceMaxBytes = 40
	scope.state.toolSurfaceItemBytes = 40
	scope.mu.Unlock()
	kernel := newEngineTurnKernel(
		protocol.TurnIntentAnswer,
		"act",
		nil,
		0,
		nil,
		nil,
	)
	results, err := engine.runToolsWithCache(
		t.Context(),
		"turn-budget",
		[]provider.ToolCall{{
			ID: "call-large", Name: "large_result", Arguments: `{}`,
		}},
		make(map[string]tool.Result),
		&toolResultCache{},
		kernel,
		func(State, Event) error { return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	// The 40-byte surface budget is a ceil(40/3) = 14-token ceiling at
	// dense-script density.
	if len(results) != 1 || results[0].Admission == nil ||
		results[0].Admission.TokenLimit != 14 ||
		!results[0].Admission.Truncated {
		t.Fatalf("economic result admission = %+v", results)
	}
}

func TestRunToolsPoolAdmitsMixedBatchByDemand(t *testing.T) {
	registry := tool.NewRegistry(nil, nil)
	if err := registry.Register(largeResultTool{}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(smallResultTool{}); err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, &scriptedProvider{}, registry)
	scope := engine.executionScope()
	// Pool of ceil(800/3) = 267 units: the equal split would cap both results
	// at 133 and truncate the large one; the pool admits the 19-unit result
	// whole and grants the large one the reclaimed 248.
	scope.mu.Lock()
	scope.state.toolSurfaceMaxBytes = 800
	scope.state.toolSurfaceItemBytes = 800
	scope.mu.Unlock()
	kernel := newEngineTurnKernel(
		protocol.TurnIntentAnswer,
		"act",
		nil,
		0,
		nil,
		nil,
	)
	results, err := engine.runToolsWithCache(
		t.Context(),
		"turn-pool",
		[]provider.ToolCall{
			{ID: "call-small", Name: "small_result", Arguments: `{}`},
			{ID: "call-large", Name: "large_result", Arguments: `{}`},
		},
		make(map[string]tool.Result),
		&toolResultCache{},
		kernel,
		func(State, Event) error { return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
	if results[0].Truncated {
		t.Fatalf("small result truncated: %+v", results[0].Admission)
	}
	large := results[1]
	if !large.Truncated || large.Admission == nil ||
		large.Admission.TokenLimit != 248 {
		t.Fatalf("pooled large admission = %+v", large)
	}
}

func TestFinishOnlyClosesExplorationCallWithoutExecutingIt(t *testing.T) {
	registry := tool.NewRegistry(nil, nil)
	executor := &echoTool{}
	if err := registry.Register(executor); err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, &scriptedProvider{}, registry)
	kernel := newEngineTurnKernel(
		protocol.TurnIntentWorkspaceChange,
		"act",
		nil,
		0,
		nil,
		nil,
	)

	results, err := engine.runToolsWithCache(
		tool.WithFinishOnly(t.Context()),
		"turn-finish-only",
		[]provider.ToolCall{{
			ID: "call_read", Name: "echo",
			Arguments: `{"text":"keep exploring"}`,
		}},
		make(map[string]tool.Result),
		&toolResultCache{},
		kernel,
		func(State, Event) error { return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if executor.calls.Load() != 0 ||
		len(results) != 1 ||
		!results[0].IsError ||
		results[0].Metadata["error_category"] != "no_progress_finish_only" {
		t.Fatalf(
			"calls=%d results=%+v",
			executor.calls.Load(),
			results,
		)
	}
	if len(kernel.Snapshot().OpenCalls) != 0 ||
		len(kernel.Snapshot().ClosedCalls) != 1 {
		t.Fatalf("tool lifecycle not closed: %+v", kernel.Snapshot())
	}
}

func TestRunToolsRejectsDuplicateCallIdentityBeforeExecution(t *testing.T) {
	registry := tool.NewRegistry(nil, nil)
	executor := &echoTool{}
	if err := registry.Register(executor); err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, &scriptedProvider{}, registry)
	kernel := newEngineTurnKernel(
		protocol.TurnIntentAnswer,
		"act",
		nil,
		0,
		nil,
		nil,
	)
	var starts, results int
	_, err := engine.runToolsWithCache(
		t.Context(),
		"turn-duplicate",
		[]provider.ToolCall{
			{ID: "duplicate", Name: "echo", Arguments: `{"text":"first"}`},
			{ID: "duplicate", Name: "echo", Arguments: `{"text":"second"}`},
		},
		make(map[string]tool.Result),
		&toolResultCache{},
		kernel,
		func(_ State, event Event) error {
			if event.ToolCall != nil && event.Result == nil {
				starts++
			}
			if event.Result != nil {
				results++
			}
			return nil
		},
	)
	if err == nil || protocol.CodeOf(err) != protocol.CodeConflict {
		t.Fatalf("runToolsWithCache() error = %v", err)
	}
	if executor.calls.Load() != 0 || starts != 0 || results != 0 {
		t.Fatalf(
			"execution leaked: calls=%d starts=%d results=%d",
			executor.calls.Load(),
			starts,
			results,
		)
	}
}

func TestRunToolsDoesNotRepublishAlreadyClosedCall(t *testing.T) {
	registry := tool.NewRegistry(nil, nil)
	executor := &echoTool{}
	if err := registry.Register(executor); err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, &scriptedProvider{}, registry)
	kernel := newEngineTurnKernel(
		protocol.TurnIntentAnswer,
		"act",
		nil,
		0,
		nil,
		nil,
	)
	executed := make(map[string]tool.Result)
	cache := &toolResultCache{}
	call := provider.ToolCall{
		ID: "stable-id", Name: "echo", Arguments: `{"text":"same"}`,
	}
	var starts, results int
	send := func(_ State, event Event) error {
		if event.ToolCall != nil && event.Result == nil {
			starts++
		}
		if event.Result != nil {
			results++
		}
		return nil
	}
	for range 2 {
		got, err := engine.runToolsWithCache(
			t.Context(),
			"turn-cache",
			[]provider.ToolCall{call},
			executed,
			cache,
			kernel,
			send,
		)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].IsError {
			t.Fatalf("results = %+v", got)
		}
	}
	if executor.calls.Load() != 1 || starts != 1 || results != 1 {
		t.Fatalf(
			"replayed lifecycle: calls=%d starts=%d results=%d",
			executor.calls.Load(),
			starts,
			results,
		)
	}
	if len(kernel.Snapshot().OpenCalls) != 0 ||
		len(kernel.Snapshot().ClosedCalls) != 1 {
		t.Fatalf("kernel tool ledger = %+v", kernel.Snapshot())
	}
}

func TestRunToolsClosesPublishedCallsWhenStartPublicationFails(t *testing.T) {
	registry := tool.NewRegistry(nil, nil)
	executor := &echoTool{}
	if err := registry.Register(executor); err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, &scriptedProvider{}, registry)
	kernel := newEngineTurnKernel(
		protocol.TurnIntentAnswer,
		"act",
		nil,
		0,
		nil,
		nil,
	)
	var starts, results int
	_, err := engine.runToolsWithCache(
		t.Context(),
		"turn-publish-failure",
		[]provider.ToolCall{
			{ID: "first", Name: "echo", Arguments: `{"text":"first"}`},
			{ID: "second", Name: "echo", Arguments: `{"text":"second"}`},
		},
		make(map[string]tool.Result),
		&toolResultCache{},
		kernel,
		func(_ State, event Event) error {
			if event.ToolCall != nil && event.Result == nil {
				starts++
				if starts == 2 {
					return errors.New("start sink failed")
				}
			}
			if event.Result != nil {
				results++
				if !event.Result.IsError {
					t.Fatalf("aborted result = %+v", event.Result)
				}
			}
			return nil
		},
	)
	if err == nil || !strings.Contains(err.Error(), "start sink failed") {
		t.Fatalf("runToolsWithCache() error = %v", err)
	}
	if executor.calls.Load() != 0 || starts != 2 || results != 1 {
		t.Fatalf(
			"publication lifecycle: calls=%d starts=%d results=%d",
			executor.calls.Load(),
			starts,
			results,
		)
	}
	if len(kernel.Snapshot().OpenCalls) != 0 ||
		len(kernel.Snapshot().ClosedCalls) != 2 {
		t.Fatalf("kernel tool ledger = %+v", kernel.Snapshot())
	}
	for _, callID := range []string{"first", "second"} {
		if _, closed := kernel.Snapshot().ClosedCalls[callID]; !closed {
			t.Fatalf("durable start %q was not closed: %+v", callID, kernel.Snapshot())
		}
	}
}

func TestRunToolsContinuesResultPublicationAfterSinkFailure(t *testing.T) {
	registry := tool.NewRegistry(nil, nil)
	executor := &echoTool{}
	if err := registry.Register(executor); err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, &scriptedProvider{}, registry)
	kernel := newEngineTurnKernel(
		protocol.TurnIntentAnswer,
		"act",
		nil,
		0,
		nil,
		nil,
	)
	var results int
	executed := make(map[string]tool.Result)
	_, err := engine.runToolsWithCache(
		t.Context(),
		"turn-result-publication-failure",
		[]provider.ToolCall{
			{ID: "first", Name: "echo", Arguments: `{"text":"first"}`},
			{ID: "second", Name: "echo", Arguments: `{"text":"second"}`},
		},
		executed,
		&toolResultCache{},
		kernel,
		func(_ State, event Event) error {
			if event.Result == nil {
				return nil
			}
			results++
			if results == 1 {
				return errors.New("result sink failed")
			}
			return nil
		},
	)
	if err == nil || !strings.Contains(err.Error(), "result sink failed") {
		t.Fatalf("runToolsWithCache() error = %v", err)
	}
	if executor.calls.Load() != 2 || results != 2 {
		t.Fatalf(
			"result publication: calls=%d results=%d",
			executor.calls.Load(),
			results,
		)
	}
	if len(kernel.Snapshot().OpenCalls) != 0 ||
		len(kernel.Snapshot().ClosedCalls) != 2 {
		t.Fatalf("kernel tool ledger = %+v", kernel.Snapshot())
	}
	for _, callID := range []string{"first", "second"} {
		if _, closed := kernel.Snapshot().ClosedCalls[callID]; !closed {
			t.Fatalf("durable result did not close %q: %+v", callID, kernel.Snapshot())
		}
		if _, cached := executed[callID]; !cached {
			t.Fatalf("durable result missing from execution cache: %+v", executed)
		}
	}
}

func TestRunToolsCancellationClosesKernelLifecycle(t *testing.T) {
	registry := tool.NewRegistry(nil, nil)
	executor := &echoTool{}
	if err := registry.Register(executor); err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, &scriptedProvider{}, registry)
	kernel := newEngineTurnKernel(
		protocol.TurnIntentAnswer,
		"act",
		nil,
		0,
		nil,
		nil,
	)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var starts, results int
	got, err := engine.runToolsWithCache(
		ctx,
		"turn-canceled",
		[]provider.ToolCall{{
			ID: "canceled", Name: "echo", Arguments: `{"text":"never"}`,
		}},
		make(map[string]tool.Result),
		&toolResultCache{},
		kernel,
		func(_ State, event Event) error {
			if event.ToolCall != nil && event.Result == nil {
				starts++
			}
			if event.Result != nil {
				results++
			}
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].IsError ||
		executor.calls.Load() != 0 ||
		starts != 1 ||
		results != 1 {
		t.Fatalf(
			"canceled lifecycle: got=%+v calls=%d starts=%d results=%d",
			got,
			executor.calls.Load(),
			starts,
			results,
		)
	}
	if len(kernel.Snapshot().OpenCalls) != 0 ||
		len(kernel.Snapshot().ClosedCalls) != 1 {
		t.Fatalf("kernel tool ledger = %+v", kernel.Snapshot())
	}
}

func TestEngineReturnsReadToolPanicToModelAndContinues(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventToolCallDelta, ToolCall: &provider.ToolCallFragment{
				Index: 0, ID: "call_1", Name: "panic_tool", Arguments: `{}`,
			}},
			{Type: provider.EventMessageStop, StopReason: provider.StopReasonToolUse},
		}},
		textStream("recovered"),
		textStream("done"),
	}}
	registry := tool.NewRegistry(nil, nil)
	if err := registry.Register(panickingTool{}); err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, runtime, registry)
	var kernelRecords []turnkernel.TransitionRecord
	engine.options.TurnKernelObserver = func(record turnkernel.TransitionRecord) {
		kernelRecords = append(kernelRecords, record)
	}
	var states []State
	result, err := engine.Run(t.Context(), "run it", func(event Event) error {
		states = append(states, event.State)
		return nil
	})
	if err != nil || result.Text != "done" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	assertOneTerminal(t, states, Completed)
	var started, closed int
	for _, record := range kernelRecords {
		if record.Drift != "" {
			t.Fatalf("kernel drift = %+v", record)
		}
		switch record.Command {
		case "model_sample_result_received":
			started++
		case "tool_result_received":
			closed++
		}
	}
	if started != 3 || closed != 1 {
		t.Fatalf(
			"kernel lifecycle starts=%d results=%d records=%+v",
			started,
			closed,
			kernelRecords,
		)
	}
}

func TestToolDefinitionsAreEmptyWhenToolsAreDisabled(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))

	if definitions := testToolDefinitions(t, engine); len(definitions) != 0 {
		t.Fatalf("toolDefinitions() = %+v, want empty tools-off surface", definitions)
	}
}

func TestEngineToolRoundTripAcrossProviderProtocols(t *testing.T) {
	tests := map[string]struct {
		protocol model.WireProtocol
		path     string
		first    string
		second   string
		wantBody []string
	}{
		"chat": {
			protocol: model.ProtocolOpenAIChat, path: "/chat/completions",
			first:    "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"echo\",\"arguments\":\"{\\\"text\\\":\\\"hello\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n",
			second:   "data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n",
			wantBody: []string{`"role":"tool"`, `"tool_call_id":"call_1"`},
		},
		"responses": {
			protocol: model.ProtocolOpenAIResponses, path: "/responses",
			first:    "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"echo\"}}\n\ndata: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":0,\"item_id\":\"call_1\",\"delta\":\"{\\\"text\\\":\\\"hello\\\"}\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{}}\n\n",
			second:   "data: {\"type\":\"response.output_text.delta\",\"delta\":\"done\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{}}\n\n",
			wantBody: []string{`"type":"function_call"`, `"type":"function_call_output"`, `"call_id":"call_1"`},
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			var requests atomic.Int32
			var secondBody string
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path != test.path {
					t.Errorf("path = %q", request.URL.Path)
				}
				data, _ := io.ReadAll(request.Body)
				attempt := requests.Add(1)
				writer.Header().Set("Content-Type", "text/event-stream")
				if attempt == 1 {
					_, _ = io.WriteString(writer, test.first)
					return
				}
				secondBody = string(data)
				_, _ = io.WriteString(writer, test.second)
			}))
			defer server.Close()

			executor := &echoTool{}
			registry := tool.NewRegistry(nil, nil)
			if err := registry.Register(executor); err != nil {
				t.Fatal(err)
			}
			route := testRouteProtocol(t, server.URL, test.protocol)
			runtime, err := newTestEngine(Options{ProviderConfig: ProviderConfig{Provider: testHTTPProvider(t, route), Route: route,
				MaxOutputTokens: 128}, ToolConfig: ToolConfig{Tools: registry,
				Authorize: func(provider.ToolCall) bool { return true }},
			})
			if err != nil {
				t.Fatal(err)
			}
			result, err := runtime.Run(t.Context(), "work", nil)
			if err != nil {
				t.Fatal(err)
			}
			if result.Text != "done" || executor.calls.Load() != 1 || requests.Load() != 2 {
				t.Fatalf("result=%+v tool_calls=%d requests=%d", result, executor.calls.Load(), requests.Load())
			}
			for _, fragment := range test.wantBody {
				if !strings.Contains(secondBody, fragment) {
					t.Fatalf("second request missing %s: %s", fragment, secondBody)
				}
			}
		})
	}
}

func TestEngineUsesWebSearchCitationToCompleteTurn(t *testing.T) {
	searchServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"results":[{"title":"Fixture Source","url":"https://example.test/source","snippet":"verified answer"}]}`)
	}))
	defer searchServer.Close()
	t.Setenv("QCODE_WEB_SEARCH_URL", searchServer.URL)

	runtime := &scriptedProvider{streams: []provider.Stream{
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventToolCallDelta, ToolCall: &provider.ToolCallFragment{
				ID: "web_1", Name: "web_search", Arguments: `{"query":"verified answer"}`,
			}},
			{Type: provider.EventMessageStop},
		}},
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventTextDelta, Text: "completed with citation"},
			{Type: provider.EventMessageStop},
		}},
	}}
	registry := tool.NewRegistry(nil, nil)
	if err := webtool.Register(registry, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	result, err := newEngine(t, runtime, registry).Run(t.Context(), "research", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "completed with citation" || len(runtime.requests) != 2 {
		t.Fatalf("result=%+v requests=%d", result, len(runtime.requests))
	}
	var toolResult provider.Message
	for _, message := range runtime.requests[1].Messages {
		if message.Role == provider.RoleTool {
			toolResult = message
			break
		}
	}
	if toolResult.Role != provider.RoleTool ||
		!strings.Contains(toolResult.Blocks[0].ToolResult.Content, `"url":"https://example.test/source"`) ||
		!strings.Contains(toolResult.Blocks[0].ToolResult.Content, `"citations"`) {
		t.Fatalf("tool result = %+v", toolResult)
	}
}

func TestEngineRetriesOnlyBeforeMeaningfulStreamData(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		&errorStream{err: protocol.NewProblem(
			protocol.CodeUnavailable,
			"temporary",
			true,
			&provider.Failure{
				Code: provider.FailureTransport, Message: "temporary",
			},
		)},
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventMessageStart},
			{Type: provider.EventTextDelta, Text: "ok"},
			{Type: provider.EventMessageStop},
		}},
	}}
	registry := tool.NewRegistry(nil, nil)
	engine := newEngine(t, runtime, registry)
	engine.options.MaxRetries = 1

	result, err := engine.Run(t.Context(), "retry", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "ok" || len(runtime.requests) != 2 {
		t.Fatalf("result=%+v requests=%d", result, len(runtime.requests))
	}
	if runtime.requests[0].Projection.Retry ||
		!runtime.requests[1].Projection.Retry ||
		runtime.requests[0].Projection.WindowID == "" ||
		runtime.requests[0].Projection.WindowID !=
			runtime.requests[1].Projection.WindowID {
		t.Fatalf(
			"retry projection: first=%+v second=%+v",
			runtime.requests[0].Projection,
			runtime.requests[1].Projection,
		)
	}
}

func TestEngineGuaranteesOneRetryForStructuredTransportFailure(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		&errorStream{err: protocol.NewProblem(
			protocol.CodeUnavailable,
			"provider stream transport failed",
			true,
			syscall.ECONNRESET,
		)},
		textStream("ok"),
	}}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	var retries []*ProviderRetry

	result, err := engine.Run(t.Context(), "retry transport", func(event Event) error {
		if event.ProviderRetry != nil {
			retries = append(retries, event.ProviderRetry)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "ok" || len(runtime.requests) != 2 {
		t.Fatalf("result=%+v requests=%d", result, len(runtime.requests))
	}
	if len(retries) != 1 || retries[0].Attempt != 1 ||
		retries[0].Code != protocol.CodeUnavailable ||
		retries[0].Category != "connection_reset" {
		t.Fatalf("provider retries = %+v", retries)
	}
}

func TestEnginePublishesProviderAttemptLifecycle(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		&errorStream{err: protocol.NewProblem(
			protocol.CodeUnavailable,
			"provider stream transport failed",
			true,
			syscall.ECONNRESET,
		)},
		textStream("ok"),
	}}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	var attempts []ModelExecution
	if _, err := engine.Run(t.Context(), "retry transport", func(event Event) error {
		if event.ModelExecution != nil && event.ModelExecution.Kind == "provider_attempt" {
			attempts = append(attempts, *event.ModelExecution)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 5 {
		t.Fatalf("provider attempts = %+v, want started/failed/retry_wait/started/completed", attempts)
	}
	if attempts[0].Status != protocol.ProviderAttemptStarted ||
		attempts[0].Attempt != 1 || attempts[0].StartedAt.IsZero() {
		t.Fatalf("first started attempt = %+v", attempts[0])
	}
	if attempts[1].Status != protocol.ProviderAttemptFailed || attempts[1].Attempt != 1 {
		t.Fatalf("failed attempt = %+v", attempts[1])
	}
	if attempts[2].Status != protocol.ProviderAttemptRetryWait ||
		attempts[2].Attempt != 1 {
		t.Fatalf("retry wait = %+v", attempts[2])
	}
	if attempts[3].Status != protocol.ProviderAttemptStarted || attempts[3].Attempt != 2 {
		t.Fatalf("second started attempt = %+v", attempts[3])
	}
	if attempts[4].Status != protocol.ProviderAttemptCompleted ||
		attempts[4].StopReason != provider.StopReasonEndTurn ||
		attempts[4].FinishedAt.IsZero() {
		t.Fatalf("completed attempt = %+v", attempts[4])
	}
}

func TestTurnCatalogSnapshotRejectsReplacementAndRemainsFrozen(t *testing.T) {
	registry := tool.NewRegistry(nil, nil)
	oldExecutor := &countingCatalogExecutor{descriptor: echoDescriptor()}
	oldExecutor.descriptor.Description = "catalog v1"
	if _, err := registry.Reconcile(
		"dynamic:sampling", 0,
		[]tool.Registration{trustedExternalRegistration(oldExecutor)},
	); err != nil {
		t.Fatal(err)
	}
	newExecutor := &countingCatalogExecutor{descriptor: echoDescriptor()}
	newExecutor.descriptor.Description = "catalog v2"
	runtime := &catalogMutationProvider{
		registry: registry,
		streams: []provider.Stream{
			&providerfixture.SliceStream{Events: []provider.StreamEvent{
				{Type: provider.EventToolCallDelta, ToolCall: &provider.ToolCallFragment{
					Index: 0, ID: "call_stale", Name: "echo", Arguments: `{"text":"old"}`,
				}},
				{Type: provider.EventMessageStop},
			}},
			&providerfixture.SliceStream{Events: []provider.StreamEvent{
				{Type: provider.EventTextDelta, Text: "recovered"},
				{Type: provider.EventMessageStop},
			}},
			textStream("recovered"),
		},
		mutate: func() error {
			_, err := registry.Replace(
				"dynamic:sampling", registry.Generation(),
				trustedExternalRegistration(newExecutor),
			)
			return err
		},
	}
	result, err := newEngine(t, runtime, registry).Run(t.Context(), "replace race", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "recovered" || oldExecutor.calls.Load() != 0 || newExecutor.calls.Load() != 0 {
		t.Fatalf(
			"result=%+v old_calls=%d new_calls=%d",
			result, oldExecutor.calls.Load(), newExecutor.calls.Load(),
		)
	}
	if got := definitionDescription(runtime.requests[0], "echo"); got != "catalog v1" {
		t.Fatalf("first sample description = %q", got)
	}
	if got := definitionDescription(runtime.requests[1], "echo"); got != "catalog v1" {
		t.Fatalf("next sample escaped frozen catalog: %q", got)
	}
}

func TestProviderRetryReusesCatalogSnapshot(t *testing.T) {
	registry := tool.NewRegistry(nil, nil)
	oldExecutor := &countingCatalogExecutor{descriptor: echoDescriptor()}
	oldExecutor.descriptor.Description = "retry v1"
	if _, err := registry.Reconcile(
		"dynamic:retry", 0,
		[]tool.Registration{trustedExternalRegistration(oldExecutor)},
	); err != nil {
		t.Fatal(err)
	}
	newExecutor := &countingCatalogExecutor{descriptor: echoDescriptor()}
	newExecutor.descriptor.Description = "retry v2"
	runtime := &catalogMutationProvider{
		registry: registry,
		streams: []provider.Stream{
			&errorStream{err: protocol.NewProblem(
				protocol.CodeUnavailable,
				"temporary",
				true,
				&provider.Failure{
					Code:    provider.FailureTransport,
					Message: "temporary",
				},
			)},
			&providerfixture.SliceStream{Events: []provider.StreamEvent{
				{Type: provider.EventTextDelta, Text: "ok"},
				{Type: provider.EventMessageStop},
			}},
		},
		mutate: func() error {
			_, err := registry.Replace(
				"dynamic:retry", registry.Generation(),
				trustedExternalRegistration(newExecutor),
			)
			return err
		},
	}
	engine := newEngine(t, runtime, registry)
	engine.options.MaxRetries = 1
	if _, err := engine.Run(t.Context(), "retry snapshot", nil); err != nil {
		t.Fatal(err)
	}
	if len(runtime.requests) != 2 {
		t.Fatalf("requests = %d", len(runtime.requests))
	}
	for index, request := range runtime.requests {
		if got := definitionDescription(request, "echo"); got != "retry v1" {
			t.Fatalf("retry request %d description = %q, want frozen v1", index, got)
		}
	}
}

func TestMCPHealthSnapshotProjectsOncePerTurn(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	snapshots := []MCPHealthSnapshot{{
		Server: "remote", State: "healthy", ChangedAt: now,
	}}
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	engine.options.TurnSnapshots.MCP = func() []MCPHealthSnapshot {
		return append([]MCPHealthSnapshot(nil), snapshots...)
	}
	engine.options.Observability.Clock = func() time.Time { return now }
	var changes []MCPHealthChanged
	send := func(_ State, event Event) error {
		if event.MCPHealthChanged != nil {
			changes = append(changes, *event.MCPHealthChanged)
		}
		return nil
	}
	if err := engine.emitMCPHealthChanges(append([]MCPHealthSnapshot(nil), snapshots...), send); err != nil {
		t.Fatal(err)
	}
	if err := engine.emitMCPHealthChanges(append([]MCPHealthSnapshot(nil), snapshots...), send); err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Current.State != "healthy" {
		t.Fatalf("changes = %+v", changes)
	}
}

func TestSamplingFailsClosedUntilToolCatalogSyncRecovers(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventMessageStart},
			{Type: provider.EventTextDelta, Text: "done"},
			{Type: provider.EventMessageStop},
		}},
	}}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	syncFailure := errors.New("fixture MCP catalog conflict")
	failing := true
	var syncCalls int
	engine.options.ToolCatalogSync = func() error {
		syncCalls++
		if failing {
			return syncFailure
		}
		return nil
	}

	_, err := engine.Run(t.Context(), "blocked", nil)
	if !protocol.IsCode(err, protocol.CodeUnavailable) ||
		!errors.Is(err, syncFailure) {
		t.Fatalf("blocked sampling error = %v", err)
	}
	if len(runtime.requests) != 0 {
		t.Fatalf("provider requests after failed sync = %d, want 0", len(runtime.requests))
	}

	failing = false
	result, err := engine.Run(t.Context(), "retry", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "done" || len(runtime.requests) != 1 || syncCalls != 2 {
		t.Fatalf(
			"result=%+v requests=%d sync calls=%d",
			result,
			len(runtime.requests),
			syncCalls,
		)
	}
}

func definitionDescription(request provider.ModelRequest, name string) string {
	for _, definition := range request.Tools {
		if definition.Name == name {
			return definition.Description
		}
	}
	return ""
}

func TestEngineToolsOffAndOnUseSameImplementation(t *testing.T) {
	for name, registry := range map[string]*tool.Registry{
		"off": nil,
		"on":  tool.NewRegistry(nil, nil),
	} {
		t.Run(name, func(t *testing.T) {
			runtime := &scriptedProvider{streams: []provider.Stream{
				&providerfixture.SliceStream{Events: []provider.StreamEvent{
					{Type: provider.EventTextDelta, Text: "ok"},
					{Type: provider.EventMessageStop},
				}},
			}}
			engine, err := newTestEngine(Options{ProviderConfig: ProviderConfig{Provider: runtime, Route: testRoute(t),
				MaxOutputTokens: 128, MaxSteps: 2}, ToolConfig: ToolConfig{Tools: registry},
			})
			if err != nil {
				t.Fatal(err)
			}
			result, err := engine.Run(t.Context(), "work", nil)
			if err != nil || result.Text != "ok" || len(runtime.requests) != 1 {
				t.Fatalf("result=%+v err=%v requests=%d", result, err, len(runtime.requests))
			}
		})
	}
}

func TestEngineBindsVersionedReplayToAssistantProvenance(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventReasoningDelta, Index: 0, Text: "think"},
			{Type: provider.EventTextDelta, Index: 1, Text: "first"},
			{Type: provider.EventReplayState, Replay: &provider.ReplayState{
				Version: provider.ReplayVersion,
				Data:    json.RawMessage(`{"items":[{"type":"reasoning"}]}`),
			}},
			{Type: provider.EventMessageStop},
		}},
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventTextDelta, Text: "second"},
			{Type: provider.EventMessageStop},
		}},
	}}
	engine, err := newTestEngine(Options{ProviderConfig: ProviderConfig{Provider: runtime, Route: testRoute(t), NativeSearch: true,
		MaxOutputTokens: 128},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Run(t.Context(), "one", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Run(t.Context(), "two", nil); err != nil {
		t.Fatal(err)
	}
	var assistant provider.Message
	for _, message := range runtime.requests[1].Messages {
		if message.Role == provider.RoleAssistant && message.Provenance != nil {
			assistant = message
			break
		}
	}
	replayed := assistant.Blocks
	if len(replayed) != 2 ||
		replayed[0].Type != provider.ContentReasoning ||
		replayed[0].Text != "think" ||
		replayed[1].Text != "first" ||
		assistant.Provenance == nil ||
		assistant.Provenance.Adapter != model.AdapterOpenAICompatible ||
		assistant.Provenance.Replay == nil ||
		assistant.Provenance.Replay.ContentDigest !=
			provider.MessageContentDigest(assistant) ||
		!runtime.requests[1].NativeSearch {
		t.Fatalf("second request = %+v", runtime.requests[1])
	}
}

func TestEngineBudgetAndFailedHistoryRollback(t *testing.T) {
	runtime := &scriptedProvider{}
	engine, err := newTestEngine(Options{ProviderConfig: ProviderConfig{Provider: runtime, Route: testRoute(t), MaxOutputTokens: 128}, ContextConfig: ContextConfig{Budget: Budget{MaxTokens: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	var terminalFault *protocol.FaultMetadata
	result, err := engine.Run(t.Context(), "too large", func(event Event) error {
		if event.State == Failed {
			terminalFault = protocol.CloneFaultMetadata(event.Fault)
		}
		return nil
	})
	if !protocol.IsCode(err, protocol.CodeResourceExhausted) {
		t.Fatalf("Run() error = %v", err)
	}
	if result.State != Failed ||
		terminalFault == nil ||
		terminalFault.Disposition != protocol.FaultResumeTurn {
		t.Fatalf(
			"budget terminal = state:%s fault:%+v",
			result.State,
			terminalFault,
		)
	}
	history := engine.History()
	if len(history) < 2 ||
		history[0].Text() != "too large" ||
		!failedTurnCapsuleAt(t, history[len(history)-1], "too large", "resource_exhausted") {
		t.Fatalf("failed turn capsule = %+v", history)
	}

	costEngine, err := newTestEngine(Options{ProviderConfig: ProviderConfig{Provider: &scriptedProvider{}, Route: testRoute(t), MaxOutputTokens: 128}, ContextConfig: ContextConfig{Budget: Budget{MaxCostUSD: 0.000001}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := costEngine.Run(t.Context(), "cost", nil); !protocol.IsCode(err, protocol.CodeResourceExhausted) {
		t.Fatalf("cost budget error = %v", err)
	}

	runtime.streams = []provider.Stream{&errorStream{err: errors.New("failed")}}
	engine.options.Budget = Budget{}
	if _, err := engine.Run(t.Context(), "rollback", nil); err == nil {
		t.Fatal("Run() error = nil")
	}
	if history := engine.History(); !failedTurnCapsuleAt(t, history[len(history)-1], "rollback", "") ||
		!slices.ContainsFunc(history, func(message provider.Message) bool {
			return message.Text() == "rollback"
		}) ||
		!slices.ContainsFunc(history, func(message provider.Message) bool {
			return strings.Contains(message.Text(), `"goal":"too large"`)
		}) {
		t.Fatalf("provider failure capsules = %+v", history)
	}
	runtime.streams = []provider.Stream{&providerfixture.SliceStream{Events: []provider.StreamEvent{
		{Type: provider.EventTextDelta, Text: "recovered"},
		{Type: provider.EventMessageStop},
	}}}
	if _, err := engine.Run(t.Context(), "summarize the failure", nil); err != nil {
		t.Fatal(err)
	}
	request := runtime.requests[len(runtime.requests)-1]
	var sawFailure bool
	for _, message := range request.Messages {
		if message.Role == provider.RoleSystem &&
			strings.Contains(message.Text(), "[turn_terminal]") &&
			strings.Contains(message.Text(), `"goal":"rollback"`) {
			sawFailure = true
		}
	}
	if !sawFailure {
		t.Fatalf("next turn request omitted failed turn capsule: %+v", request.Messages)
	}

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	cancelEngine, err := newTestEngine(Options{ProviderConfig: ProviderConfig{Provider: cancelProvider{}, Route: testRoute(t), MaxOutputTokens: 128}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cancelEngine.Run(canceled, "cancel", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Run() error = %v", err)
	}
	if history := cancelEngine.History(); len(history) != 0 {
		t.Fatalf("canceled turn committed history: %+v", history)
	}
}

func TestEngineUnauthorizedToolHasSingleFailedTerminal(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventToolCallDelta, ToolCall: &provider.ToolCallFragment{
				ID: "call_1", Name: "echo", Arguments: `{"text":"hello"}`,
			}},
			{Type: provider.EventMessageStop},
		}},
	}}
	registry := tool.NewRegistry(nil, nil)
	if err := registry.Register(&echoTool{}); err != nil {
		t.Fatal(err)
	}
	engine, err := newTestEngine(Options{ProviderConfig: ProviderConfig{Provider: runtime, Route: testRoute(t), MaxOutputTokens: 128}, ToolConfig: ToolConfig{Tools: registry}})
	if err != nil {
		t.Fatal(err)
	}
	var states []State
	if _, err := engine.Run(t.Context(), "work", func(event Event) error {
		states = append(states, event.State)
		return nil
	}); err == nil {
		t.Fatal("Run() error = nil")
	}
	assertOneTerminal(t, states, Failed)
}

func TestRequestCancelHasSingleCanceledTerminalAndNoCommittedHistory(t *testing.T) {
	started := make(chan struct{})
	engine, err := newTestEngine(Options{ProviderConfig: ProviderConfig{Provider: &steerProvider{started: started},
		Route: testRoute(t), MaxOutputTokens: 128},
	})
	if err != nil {
		t.Fatal(err)
	}
	var (
		statesMu sync.Mutex
		states   []State
	)
	done := make(chan error, 1)
	go func() {
		_, runErr := engine.Run(t.Context(), "cancel active turn", func(event Event) error {
			statesMu.Lock()
			states = append(states, event.State)
			statesMu.Unlock()
			return nil
		})
		done <- runErr
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("model stream did not start")
	}
	_ = mustControl(t, engine).Cancel("")
	select {
	case runErr := <-done:
		if !errors.Is(runErr, context.Canceled) {
			t.Fatalf("Run() error = %v, want context.Canceled", runErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RequestCancel did not stop the active turn")
	}
	statesMu.Lock()
	defer statesMu.Unlock()
	assertOneTerminal(t, states, Canceled)
	if history := engine.History(); len(history) != 0 {
		t.Fatalf("canceled turn committed history: %+v", history)
	}
}

func TestCanceledTurnContinuationRetainsTaskContext(t *testing.T) {
	started := make(chan struct{})
	runtime := &steerProvider{started: started}
	engine, err := newTestEngine(Options{ProviderConfig: ProviderConfig{Provider: runtime, Route: testRoute(t), MaxOutputTokens: 128}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, runErr := engine.Run(t.Context(), "inspect web", nil)
		done <- runErr
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("model stream did not start")
	}
	_ = mustControl(t, engine).Cancel(protocol.CancelReasonUserInterrupted)
	if runErr := <-done; !errors.Is(runErr, context.Canceled) {
		t.Fatalf("canceled Run() error = %v, want context.Canceled", runErr)
	}
	if _, err := engine.Run(t.Context(), "继续", nil); err != nil {
		t.Fatal(err)
	}

	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if len(runtime.requests) != 2 {
		t.Fatalf("provider requests=%d want 2", len(runtime.requests))
	}
	var userPrompts []string
	for _, message := range runtime.requests[1].Messages {
		if message.Role == provider.RoleUser {
			userPrompts = append(userPrompts, message.Text())
		}
	}
	if !slices.Contains(userPrompts, "inspect web") ||
		!slices.Contains(userPrompts, "继续") {
		t.Fatalf("continuation user prompts=%q", userPrompts)
	}
}

func TestRetainCanceledHistoryDropsOrphanToolTraffic(t *testing.T) {
	messages := []provider.Message{
		provider.TextMessage(provider.RoleUser, "inspect web"),
		{
			Role: provider.RoleAssistant,
			Blocks: []provider.ContentBlock{{
				Type: provider.ContentToolCall,
				ToolCall: &provider.ToolCall{
					ID: "paired", Name: "read", Arguments: `{}`,
				},
			}},
		},
		{
			Role: provider.RoleTool,
			Blocks: []provider.ContentBlock{{
				Type: provider.ContentToolResult,
				ToolResult: &provider.ToolResult{
					CallID: "paired", Content: "result",
				},
			}},
		},
		{
			Role: provider.RoleAssistant,
			Blocks: []provider.ContentBlock{{
				Type: provider.ContentToolCall,
				ToolCall: &provider.ToolCall{
					ID: "orphan", Name: "read", Arguments: `{}`,
				},
			}},
		},
	}
	retained := retainCanceledHistory(messages)
	if len(retained) != 3 ||
		retained[0].Text() != "inspect web" ||
		retained[1].Blocks[0].ToolCall.ID != "paired" ||
		retained[2].Blocks[0].ToolResult.CallID != "paired" {
		t.Fatalf("retained history=%+v", retained)
	}
}

func TestEngineCompactionPreservesTurnGroupsAndSummary(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	engine.options.Context.Window.AutoTokens = 600
	// A budget wide enough for the whole summary: what this test is about is the
	// shape of the summary and the atomicity of the cut, not the byte ceiling.
	engine.options.SummaryMaxBytes = 4 << 10
	// The summary reports the live critical paths, so they have to be observed the
	// way a session observes them rather than set on the frozen options.
	engine.options.Workspace = "/workspace"
	engine.options.WorkingSet = []string{"/workspace/a.go"}
	engine.seedWorkingSet()
	engine.options.StaticContextReceipts = []promptcontext.Receipt{{
		Kind: promptcontext.PartitionWorkingSet, SourcePath: "/workspace/a.go",
		OriginalBytes: 20, RetainedBytes: 10, OriginalTokens: 5, RetainedTokens: 3,
		Digest: "sha256:test", Truncated: true, TruncationReason: "byte_budget",
	}}
	engine.history = []provider.Message{
		messageWithText(provider.RoleUser, "old request", 1),
		{Role: provider.RoleAssistant, Blocks: []provider.ContentBlock{{
			Type: provider.ContentToolCall, ToolCall: &provider.ToolCall{
				ID: "call_old", Name: "echo", Arguments: `{"text":"old"}`,
			},
		}}, Turn: 1},
		{Role: provider.RoleTool, Blocks: []provider.ContentBlock{{
			Type:       provider.ContentToolResult,
			ToolResult: &provider.ToolResult{CallID: "call_old", Content: strings.Repeat("old result ", 100)},
		}}, Turn: 1},
		messageWithText(provider.RoleAssistant, strings.Repeat("old answer ", 100), 1),
		messageWithText(provider.RoleUser, "current request", 2),
	}

	receipt := engine.compact()

	if len(engine.history) != 2 {
		t.Fatalf("compacted history = %+v", engine.history)
	}
	summary := engine.history[0].Text()
	if engine.history[0].Role != provider.RoleSystem ||
		!strings.Contains(summary, agentcontext.MarkerStart) ||
		!strings.Contains(summary, "Critical paths: a.go") ||
		strings.Contains(summary, "old request") ||
		strings.Contains(summary, "call_old") ||
		engine.history[1].Turn != 2 {
		t.Fatalf("compacted history = %+v", engine.history)
	}
	// Partition byte and digest detail belongs to the audit trail, not to every
	// sample after the compaction.
	if strings.Contains(summary, "ContextReceipts") {
		t.Fatalf("summary inlined receipt detail: %s", summary)
	}
	if receipt == nil ||
		receipt.RemovedMessages != 4 ||
		len(receipt.RemovedTurns) != 1 ||
		receipt.RemovedTurns[0] != 1 ||
		receipt.SummaryTruncated ||
		len(receipt.ContextReceipts) != 1 ||
		len(receipt.CriticalPaths) != 1 || receipt.CriticalPaths[0] != "a.go" ||
		len(receipt.WorkingSet) != 1 ||
		receipt.AuthorityDigest == "" ||
		!receipt.AuthorityEquivalent {
		t.Fatalf("compaction receipt = %+v", receipt)
	}
	if !slices.Contains(receipt.Sections, agentcontext.SectionTruth) ||
		!slices.Contains(receipt.Sections, agentcontext.SectionCritical) ||
		slices.Contains(receipt.Sections, agentcontext.SectionNarrative) {
		t.Fatalf("compaction sections = %v", receipt.Sections)
	}
	assertToolPairs(t, engine.history)
}

// Structural-only compaction does not disguise flattened transcript lines as a
// semantic narrative.
func TestEngineCompactionOmitsDeterministicTranscriptNarrative(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	engine.options.Context.Window.AutoTokens = 400
	engine.options.SummaryMaxBytes = 1024
	engine.history = []provider.Message{
		messageWithText(provider.RoleUser, "first ancient request "+strings.Repeat("filler ", 200), 1),
		messageWithText(provider.RoleAssistant, "an answer in the middle", 1),
		messageWithText(provider.RoleUser, "one more question", 1),
		messageWithText(provider.RoleAssistant, "last thing before the cut", 1),
		messageWithText(provider.RoleUser, "current request", 2),
	}
	receipt := engine.compact()
	if receipt == nil || receipt.NarrativeIncluded {
		t.Fatalf("compaction receipt = %+v", receipt)
	}
	summary := engine.history[0].Text()
	if strings.Contains(summary, "last thing before the cut") ||
		strings.Contains(summary, "first ancient request") {
		t.Fatalf("summary retained transcript as narrative:\n%s", summary)
	}
}

// A second compaction has to pass the first summary through whole. Flattening it
// like an ordinary message would cut everything the first compaction preserved
// down to one line, which is how a long session loses its early history.
func TestEngineCompactionMergesTruthCapsulesAcrossGenerations(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	engine.options.Context.Window.AutoTokens = 650
	engine.options.SummaryMaxBytes = 4 << 10
	engine.ApplyPlan(interact.Plan{
		Objective: "teach the parser about trailing commas",
		Steps:     []interact.PlanStep{{Title: "update the lexer", Status: interact.StepInProgress}},
	})
	engine.evidenceSet().Observe(agentcontext.EvidenceFact{
		Kind: agentcontext.KindDefinition, Path: "parser/lex.go",
		Line: 41, Symbol: "Lex", Tool: "search_definition", Turn: 1,
	})
	engine.history = []provider.Message{
		messageWithText(provider.RoleUser, strings.Repeat("early ", 500), 1),
		messageWithText(provider.RoleAssistant, "the first answer", 1),
		messageWithText(provider.RoleUser, "second request", 2),
	}
	if receipt := engine.compact(); receipt == nil {
		t.Fatal("expected a first compaction")
	}
	first := engine.history[0].Text()
	if _, ok := agentcontext.Carry(first); !ok {
		t.Fatalf("first summary is not marked as one:\n%s", first)
	}
	firstCapsule, found, err := agentcontext.ParseTruthCapsule(first)
	if err != nil || !found || firstCapsule.Generation != 1 {
		t.Fatalf("first capsule=%+v found=%t err=%v", firstCapsule, found, err)
	}

	// Drop the live evidence to prove the second generation is rebuilt from the
	// current owner snapshot instead of permanently unioning prior facts.
	engine.context = agentcontext.NewAuthority()
	engine.ApplyPlan(interact.Plan{
		Objective: "also accept trailing commas in calls",
		Steps:     []interact.PlanStep{{Title: "update the parser", Status: interact.StepPending}},
	})
	engine.history = append(engine.history,
		messageWithText(provider.RoleAssistant, strings.Repeat("later ", 400), 2),
		messageWithText(provider.RoleUser, "third request", 3),
	)
	if receipt := engine.CompactForced(); receipt == nil {
		t.Fatal("expected a second compaction")
	}
	second := engine.history[0].Text()
	if !strings.Contains(second, "also accept trailing commas in calls") {
		t.Fatalf("second summary lost the current goal:\n%s", second)
	}
	secondCapsule, found, err := agentcontext.ParseTruthCapsule(second)
	if err != nil || !found || secondCapsule.Generation != 2 {
		t.Fatalf("second capsule=%+v found=%t err=%v", secondCapsule, found, err)
	}
	var retainedFact bool
	for _, entity := range secondCapsule.Entities {
		retainedFact = retainedFact ||
			entity.Kind == agentcontext.EntityFact &&
				strings.Contains(entity.Value, "parser/lex.go:41")
	}
	if retainedFact {
		t.Fatalf("second capsule retained a fact its owner no longer declares: %+v", secondCapsule)
	}
	if strings.Contains(second, "Earlier summary:") {
		t.Fatalf("second summary carried prior narrative verbatim:\n%s", second)
	}
	if strings.Count(second, agentcontext.MarkerStart) != 1 {
		t.Fatalf("carried summary nested its markers:\n%s", second)
	}
}

func TestEngineCompactStripsConstitutionAndStaticContextReinjects(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	engine.options.Context.Window.AutoTokens = 228
	engine.options.SummaryMaxBytes = 2 << 10
	constitution := promptcontext.WrapFragment(promptcontext.FragmentConstitution, "constitution body")
	engine.options.StaticContext = []provider.Message{
		provider.TextMessage(provider.RoleSystem, constitution),
	}
	engine.history = []provider.Message{
		provider.TextMessage(provider.RoleSystem, constitution),
		messageWithText(provider.RoleUser, strings.Repeat("old ", 80), 1),
		messageWithText(provider.RoleAssistant, strings.Repeat("ans ", 80), 1),
		messageWithText(provider.RoleUser, "keep", 2),
	}
	receipt := engine.CompactForced()
	if receipt == nil {
		t.Fatal("expected forced compact")
	}
	for _, message := range engine.History() {
		if promptcontext.IsContextualFragment(message.Text()) {
			t.Fatalf("history retained fragment after compact: %q", message.Text())
		}
	}
	prompt := engine.promptMessages()
	var sawConstitution bool
	for _, message := range prompt {
		kind, ok := promptcontext.MatchFragment(message.Text())
		if !ok {
			continue
		}
		if kind == promptcontext.FragmentConstitution {
			sawConstitution = true
		}
	}
	if !sawConstitution {
		t.Fatalf("prompt reinjection missing constitution: prompt=%+v", prompt)
	}
}

func TestEngineCompactForcedRejectsAReplacementThatWouldGrowHistory(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	engine.options.Context.Window.AutoTokens = 65664
	engine.history = []provider.Message{
		messageWithText(provider.RoleUser, "old request", 1),
		messageWithText(provider.RoleAssistant, "old answer", 1),
		messageWithText(provider.RoleUser, "current request", 2),
		messageWithText(provider.RoleAssistant, "current answer", 2),
	}
	if receipt := engine.Compact(); receipt != nil {
		t.Fatalf("auto Compact under budget = %+v", receipt)
	}
	before := engine.History()
	if receipt := engine.CompactForced(); receipt != nil {
		t.Fatalf("CompactForced receipt = %+v, want no-growth rejection", receipt)
	}
	if !reflect.DeepEqual(engine.History(), before) {
		t.Fatalf("rejected compaction changed history: %+v", engine.History())
	}
}

func TestStructuredCompactionFailureLeavesOriginalHistoryIntact(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	engine.options.SummaryMaxBytes = 2 << 10
	engine.history = []provider.Message{
		provider.TextMessage(
			provider.RoleSystem,
			agentcontext.MarkerStart+"\n"+
				agentcontext.TruthMarkerStart+"\n{invalid}\n"+
				agentcontext.TruthMarkerEnd+"\n"+
				agentcontext.MarkerEnd,
		),
		messageWithText(provider.RoleUser, strings.Repeat("old ", 400), 1),
		messageWithText(provider.RoleAssistant, strings.Repeat("answer ", 400), 1),
		messageWithText(provider.RoleUser, "current", 2),
	}
	before := engine.History()
	if receipt := engine.CompactForced(); receipt != nil {
		t.Fatalf("malformed truth produced receipt: %+v", receipt)
	}
	if !reflect.DeepEqual(engine.History(), before) {
		t.Fatalf("failed structured compaction changed history: %+v", engine.History())
	}
}

func TestEngineCompactionRetainsToolPairingAtomically(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	engine.options.Context.Window.AutoTokens = 500
	engine.history = []provider.Message{
		messageWithText(provider.RoleUser, strings.Repeat("discard ", 100), 1),
		messageWithText(provider.RoleAssistant, strings.Repeat("discarded ", 100), 1),
		messageWithText(provider.RoleUser, "use a tool", 2),
		{Role: provider.RoleAssistant, Blocks: []provider.ContentBlock{{
			Type: provider.ContentToolCall,
			ToolCall: &provider.ToolCall{
				ID: "call_keep", Name: "echo", Arguments: `{"text":"keep"}`,
			},
		}}, Turn: 2},
		{Role: provider.RoleTool, Blocks: []provider.ContentBlock{{
			Type: provider.ContentToolResult,
			ToolResult: &provider.ToolResult{
				CallID: "call_keep", Content: "kept",
			},
		}}, Turn: 2},
		messageWithText(provider.RoleAssistant, "tool completed", 2),
		messageWithText(provider.RoleUser, "current", 3),
	}

	receipt := engine.compact()
	if receipt == nil || len(receipt.RemovedTurns) != 1 || receipt.RemovedTurns[0] != 1 {
		t.Fatalf("compaction receipt = %+v", receipt)
	}
	assertToolPairs(t, engine.history)
	for _, message := range engine.history {
		if message.Turn == 1 {
			t.Fatalf("compaction retained a partial old turn: %+v", engine.history)
		}
	}
}

func TestMidTurnCompactionCutsClosedToolPairsWithinActiveTurn(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	route := mustTestRouteWithContext(t, 1024)
	engine.options.Route = route
	engine.options.Routes, _ = model.NewRouteSet(route, nil, false)
	engine.options.MaxOutputTokens = 128
	engine.options.Context.Window.AutoTokens = 500
	engine.options.SummaryMaxBytes = 2 << 10
	user := "fix the parser " + strings.Repeat("context ", 80)
	history := []provider.Message{
		messageWithText(provider.RoleUser, user, 1),
		toolCallMessage(1, "call_1", "read", `{}`),
		toolResultMessage(1, "call_1", strings.Repeat("first ", 200)),
		toolCallMessage(1, "call_2", "read", `{}`),
		toolResultMessage(1, "call_2", strings.Repeat("second ", 200)),
		toolCallMessage(1, "call_3", "read", `{}`),
		toolResultMessage(1, "call_3", "latest"),
	}
	var receipt *CompactionReceipt
	snapshot := agentcontext.NewMessageLedger(agentcontext.LedgerInput{}).Snapshot()
	window, err := engine.runCompactGate(
		t.Context(), &history, snapshot, 128, CompactionPhaseMidTurn, true,
		func(_ State, event Event) error {
			receipt = event.Compaction
			return nil
		}, 0, engine.contextViewProject(nil),
	)
	if err != nil {
		t.Fatalf("progressing turn failed: %v", err)
	}
	if window.hardLimit != 0 && window.total > window.hardLimit {
		t.Fatalf("admitted an overflowing window: %+v", window)
	}
	assertToolPairs(t, history)
	if !currentTurnKeepsUser(history, user) {
		t.Fatalf("pinned user missing: %+v", history)
	}
	if receipt == nil || receipt.TruncationReason != "current_turn_working_set" {
		t.Fatalf("working-set receipt = %+v", receipt)
	}
	joined := historyTexts(history)
	if strings.Contains(joined, "first ") || strings.Contains(joined, "second ") {
		t.Fatalf("closed groups stayed in history: %s", joined)
	}
	if !strings.Contains(joined, "latest") {
		t.Fatalf("latest pair was dropped: %s", joined)
	}
}

func TestMidTurnCompactionFailsClosedWhenNoSafeCandidateFits(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	engine.options.Context.Window.AutoTokens = 300
	engine.options.SummaryMaxBytes = 100
	history := []provider.Message{
		messageWithText(provider.RoleUser, strings.Repeat("goal ", 4000), 1),
	}
	before := cloneMessages(history)
	snapshot := agentcontext.NewMessageLedger(agentcontext.LedgerInput{}).Snapshot()
	window, err := engine.runCompactGate(
		t.Context(), &history, snapshot, 128, CompactionPhaseMidTurn, true,
		func(State, Event) error { return nil }, 0, engine.contextViewProject(nil),
	)
	if protocol.CodeOf(err) != protocol.CodeResourceExhausted ||
		window.hardLimit == 0 || window.total <= window.hardLimit {
		t.Fatalf("window=%+v error=%v, want resource_exhausted", window, err)
	}
	if !reflect.DeepEqual(history, before) {
		t.Fatalf("failed compaction changed history: %+v", history)
	}
}

func TestTerminalCompletionFailsClosedWhenFinalMessageExceedsContextBudget(t *testing.T) {
	runtime := &scriptedProvider{}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	engine.options.Context.Window.AutoTokens = 160
	engine.options.SummaryMaxBytes = 64
	var completed bool

	result, err := engine.Run(t.Context(), strings.Repeat("request ", 4000), func(event Event) error {
		completed = completed || event.State == Completed
		return nil
	})

	if err == nil || protocol.CodeOf(err) != protocol.CodeResourceExhausted {
		t.Fatalf("result=%+v error=%v, want resource_exhausted", result, err)
	}
	if completed || result.State == Completed {
		t.Fatalf("over-budget terminal was completed: result=%+v emitted=%v", result, completed)
	}
	if len(runtime.requests) != 0 {
		t.Fatalf("provider requests = %d, want fail before sampling", len(runtime.requests))
	}
}

func TestWorkspaceChangeGetsOneRepairBeforeConvergenceFinalization(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		textStream("done without changing"),
		textStream("still done without changing"),
		toolCallStream("incomplete-1", completiontool.Name, `{
			"status":"incomplete",
			"summary":"No workspace mutation was observed.",
			"pending_actions":["Apply the requested workspace change."]
		}`),
	}}
	registry := tool.NewRegistry(nil, nil)
	if err := registry.Register(&completiontool.Tool{}); err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, runtime, registry)
	engine.options.MaxSteps = 1

	_, err := engine.RunForTurnWithIntentAndAttachments(
		t.Context(), "turn-no-change", "change it",
		protocol.TurnIntentWorkspaceChange, nil, func(Event) error { return nil },
	)

	if err == nil ||
		protocol.CodeOf(err) != protocol.CodeConflict ||
		!strings.Contains(err.Error(), "repair_budget") {
		t.Fatalf("Run() error = %v", err)
	}
	if len(runtime.requests) != 3 {
		t.Fatalf("provider requests = %d, want repair plus finalization", len(runtime.requests))
	}
	var feedback strings.Builder
	for _, message := range runtime.requests[1].Messages {
		feedback.WriteString(message.Text())
		feedback.WriteByte('\n')
	}
	if !strings.Contains(feedback.String(), "required_action=perform_workspace_mutation") ||
		!strings.Contains(feedback.String(), "observed_changes=0") {
		t.Fatalf("repair feedback = %q", feedback.String())
	}
}

func TestTerminalSeparatesPrimaryAndContextFinalizationFailure(t *testing.T) {
	var terminal Event
	handler := newTurnEmitter(2, func(event Event) error {
		terminal = event
		return nil
	})
	primary := protocol.NewProblem(
		protocol.CodeConflict, "primary verification conflict", false, nil,
	)
	secondary := protocol.NewProblem(
		protocol.CodeResourceExhausted, "history compaction failed", false, nil,
	)
	handler.setPrimary(primary)
	handler.addSecondary("terminal_context", secondary)
	resultErr := errors.Join(primary, secondary)
	result := Result{}
	handler.finish(t.Context(), &result, &resultErr)

	if terminal.ErrorCode != protocol.CodeConflict ||
		terminal.Error != "primary verification conflict" {
		t.Fatalf("primary terminal error = %+v", terminal)
	}
	if len(terminal.SecondaryIssues) != 1 ||
		terminal.SecondaryIssues[0].Phase != "terminal_context" ||
		terminal.SecondaryIssues[0].Code != protocol.CodeResourceExhausted {
		t.Fatalf("secondary issues = %+v", terminal.SecondaryIssues)
	}
}

func TestFailedTurnFinalizesDurableHistoryBeforeTerminalEvent(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		&errorStream{err: errors.New("provider failed")},
	}}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	engine.options.Context.Window.AutoTokens = 700
	engine.options.Context.RecentTailMaxTokens = 128
	engine.options.SummaryMaxBytes = 2 << 10
	engine.history = []provider.Message{
		messageWithText(provider.RoleUser, strings.Repeat("old request ", 60), 1),
		messageWithText(provider.RoleAssistant, strings.Repeat("old answer ", 60), 1),
		messageWithText(provider.RoleUser, strings.Repeat("recent request ", 60), 2),
		messageWithText(provider.RoleAssistant, strings.Repeat("recent answer ", 60), 2),
	}
	engine.turn = 2
	var terminalBudget *ContextBudgetSnapshot
	var postTurnCompaction bool

	_, err := engine.Run(t.Context(), "new request", func(event Event) error {
		if event.Compaction != nil &&
			event.Compaction.Phase == CompactionPhasePostTurn {
			postTurnCompaction = true
		}
		if event.State == Failed {
			terminalBudget = event.ContextBudget
		}
		return nil
	})

	if err == nil || !strings.Contains(err.Error(), "provider failed") {
		t.Fatalf("Run() error = %v", err)
	}
	if !postTurnCompaction {
		t.Fatal("failed turn did not emit post-turn compaction")
	}
	if terminalBudget == nil ||
		terminalBudget.ActiveTokens > terminalBudget.AutoCompactTokens {
		t.Fatalf("terminal context budget = %+v", terminalBudget)
	}
	if engine.options.Context.Window.AutoTokens != terminalBudget.AutoCompactTokens {
		t.Fatalf(
			"auto compact tokens = %d, terminal snapshot = %+v",
			engine.options.Context.Window.AutoTokens, terminalBudget,
		)
	}
}

func TestFailedTurnDiscardsCompactionPreparedFromFailedTransaction(t *testing.T) {
	runtime := &stagedCompactionFailureProvider{}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	runtime.engine = engine
	engine.history = []provider.Message{
		messageWithText(provider.RoleUser, "durable request", 1),
		messageWithText(provider.RoleAssistant, "durable answer", 1),
	}
	engine.context.SetCompaction(agentcontext.Compaction{
		Count: 1,
		State: &agentcontext.CompactionState{
			ID: "detached-durable-compaction", Phase: "fallback",
		},
	})
	engine.turn = 1
	var terminal Event

	_, err := engine.Run(t.Context(), "new request", func(event Event) error {
		if event.State == Failed {
			terminal = event
		}
		return nil
	})

	if err == nil || !strings.Contains(err.Error(), "provider failed") {
		t.Fatalf("Run() error = %v", err)
	}
	if strings.Contains(err.Error(), "truth capsule") {
		t.Fatalf("failed transaction leaked compaction state: %v", err)
	}
	if len(terminal.SecondaryIssues) != 0 {
		t.Fatalf("terminal secondary issues = %+v", terminal.SecondaryIssues)
	}
	if state := engine.context.Compaction().State; state != nil {
		t.Fatalf("detached compaction state survived finalization: %+v", state)
	}
}

func TestFailedTurnCompactsWithinOversizedDurableLastTurn(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		&errorStream{err: errors.New("provider failed")},
	}}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	engine.options.Context.Window.AutoTokens = 400
	engine.options.SummaryMaxBytes = 2 << 10
	engine.history = []provider.Message{
		messageWithText(provider.RoleUser, strings.Repeat("durable request ", 20), 1),
		toolCallMessage(1, "call_1", "read", `{}`),
		toolResultMessage(1, "call_1", strings.Repeat("first result ", 40)),
		toolCallMessage(1, "call_2", "read", `{}`),
		toolResultMessage(1, "call_2", strings.Repeat("second result ", 40)),
		toolCallMessage(1, "call_3", "read", `{}`),
		toolResultMessage(1, "call_3", "latest result"),
	}
	engine.turn = 1
	var terminalBudget *ContextBudgetSnapshot
	var postTurn *CompactionReceipt

	_, err := engine.Run(t.Context(), "new request", func(event Event) error {
		if event.Compaction != nil &&
			event.Compaction.Phase == CompactionPhasePostTurn {
			postTurn = event.Compaction
		}
		if event.State == Failed {
			terminalBudget = event.ContextBudget
		}
		return nil
	})

	if err == nil || !strings.Contains(err.Error(), "provider failed") {
		t.Fatalf("Run() error = %v", err)
	}
	if postTurn == nil || postTurn.OriginalBytes <= postTurn.RetainedBytes {
		t.Fatalf("post-turn compaction = %+v", postTurn)
	}
	if terminalBudget == nil ||
		terminalBudget.ActiveTokens > terminalBudget.AutoCompactTokens {
		t.Fatalf("terminal context budget = %+v", terminalBudget)
	}
	assertToolPairs(t, engine.history)
	// The failed Turn's closed exchanges (here only the goal plus per-turn
	// context projections) legitimately persist; partial assistant or tool
	// traffic from the failing sample must not.
	for _, message := range engine.history {
		if message.Turn == 2 &&
			(message.Role == provider.RoleAssistant ||
				message.Role == provider.RoleTool) {
			t.Fatalf("failed transaction leaked unclosed traffic: %+v", engine.history)
		}
	}
}

func TestEngineEmitsStructuredCompactionReceipt(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventTextDelta, Text: "done"},
			{Type: provider.EventMessageStop},
		}},
	}}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	engine.options.Context.Window.AutoTokens = 500
	engine.options.SummaryMaxBytes = 2 << 10
	engine.options.StaticContextReceipts = []promptcontext.Receipt{{
		Kind: promptcontext.PartitionBase, SourcePath: "builtin://base-system",
		OriginalBytes: 4, RetainedBytes: 4, OriginalTokens: 1, RetainedTokens: 1,
		Digest: "sha256:base",
	}}
	engine.history = []provider.Message{
		messageWithText(provider.RoleUser, strings.Repeat("old ", 200), 1),
		messageWithText(provider.RoleAssistant, strings.Repeat("answer ", 200), 1),
	}
	engine.turn = 1
	var fold *CompactionReceipt
	var states []State
	if _, err := engine.Run(t.Context(), "new request", func(event Event) error {
		states = append(states, event.State)
		if event.State == Compacting &&
			event.Compaction != nil &&
			event.Compaction.Phase == CompactionPhasePreSampling &&
			fold == nil {
			fold = event.Compaction
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if fold == nil ||
		fold.TruncationReason != "visible_tail_fold" ||
		fold.RemovedMessages < 1 ||
		fold.RetainedBytes >= fold.OriginalBytes {
		t.Fatalf("pre-sampling fold = %+v", fold)
	}
	assertStateOrder(t, states, Preparing, Compacting, CallingModel)
}

func assertStateOrder(t *testing.T, got []State, want ...State) {
	t.Helper()
	index := 0
	for _, state := range got {
		if index < len(want) && state == want[index] {
			index++
		}
	}
	if index != len(want) {
		t.Fatalf("state order %v missing prefix %v", got, want)
	}
}

func TestEnginePreSamplingGateBeforeModelCall(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventTextDelta, Text: "ok"},
			{Type: provider.EventMessageStop},
		}},
	}}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	engine.options.Context.Window.AutoTokens = 500
	engine.options.SummaryMaxBytes = 2 << 10
	engine.history = []provider.Message{
		messageWithText(provider.RoleUser, strings.Repeat("old ", 200), 1),
		messageWithText(provider.RoleAssistant, strings.Repeat("ans ", 200), 1),
	}
	engine.turn = 1
	var sawPreSampling bool
	var states []State
	if _, err := engine.Run(t.Context(), "fresh", func(event Event) error {
		states = append(states, event.State)
		if event.State == Compacting &&
			event.Compaction != nil &&
			event.Compaction.Phase == CompactionPhasePreSampling {
			sawPreSampling = true
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !sawPreSampling {
		t.Fatal("expected pre-sampling compact gate")
	}
	assertStateOrder(t, states, Preparing, Compacting, CallingModel)
	if len(runtime.requests) != 1 {
		t.Fatalf("model requests = %d, want 1 after gate", len(runtime.requests))
	}
}

func TestDeduplicateCompactionReceipts(t *testing.T) {
	var events []Event
	send := deduplicateCompactionReceipts(func(_ State, event Event) error {
		events = append(events, event)
		return nil
	})
	first := &CompactionReceipt{
		Phase: CompactionPhasePreSampling, OriginalBytes: 100,
		RetainedBytes: 80, PrunedToolResults: 1,
	}
	if err := send(Compacting, Event{Compaction: first}); err != nil {
		t.Fatal(err)
	}
	same := *first
	same.WorkingSet = []string{"internal-only-difference"}
	if err := send(Compacting, Event{Compaction: &same}); err != nil {
		t.Fatal(err)
	}
	changed := same
	changed.RetainedBytes = 70
	if err := send(Compacting, Event{Compaction: &changed}); err != nil {
		t.Fatal(err)
	}
	if err := send(Streaming, Event{Text: "done"}); err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 ||
		events[0].Compaction.RetainedBytes != 80 ||
		events[1].Compaction.RetainedBytes != 70 ||
		events[2].Text != "done" {
		t.Fatalf("events = %+v", events)
	}
}

func TestEngineSteerContinuesCurrentTurnWithoutStaleInput(t *testing.T) {
	runtime := &steerProvider{started: make(chan struct{})}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	done := make(chan error, 1)
	go func() {
		_, err := engine.Run(t.Context(), "initial", nil)
		done <- err
	}()
	<-runtime.started
	control := mustControl(t, engine)
	if err := control.Steer("change direction"); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(runtime.requests) != 2 {
		t.Fatalf("requests = %+v", runtime.requests)
	}
	second := agentcontext.StripWorldState(runtime.requests[1].Messages)
	if len(second) != 3 ||
		second[0].Text() != "initial" ||
		second[1].Text() != "partial" ||
		second[2].Text() != "change direction" {
		t.Fatalf("steered messages = %+v", second)
	}
	if err := control.Steer("stale"); err == nil {
		t.Fatal("Steer() after completion succeeded")
	}
}

func TestEnginePendingInputFIFOSteerAndMailbox(t *testing.T) {
	runtime := &steerProvider{started: make(chan struct{})}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	done := make(chan error, 1)
	go func() {
		_, err := engine.Run(t.Context(), "initial", nil)
		done <- err
	}()
	<-runtime.started
	if err := engine.EnqueueMailbox("mail-first", true); err != nil {
		t.Fatal(err)
	}
	if err := mustControl(t, engine).Steer("steer-second"); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(runtime.requests) < 2 {
		t.Fatalf("requests = %d, want >= 2", len(runtime.requests))
	}
	second := agentcontext.StripWorldState(runtime.requests[1].Messages)
	if len(second) < 4 {
		t.Fatalf("steered messages = %+v", second)
	}
	// After initial + partial assistant, FIFO: mailbox then steer.
	if second[2].Text() != "[mailbox] mail-first" {
		t.Fatalf("mailbox text = %q", second[2].Text())
	}
	if second[3].Text() != "steer-second" {
		t.Fatalf("steer text = %q", second[3].Text())
	}
}

func TestEngineMailboxNonTriggerHeldUntilNextTurn(t *testing.T) {
	runtime := &steerProvider{started: make(chan struct{})}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	done := make(chan error, 1)
	go func() {
		_, err := engine.Run(t.Context(), "turn-1", nil)
		done <- err
	}()
	<-runtime.started
	if err := engine.EnqueueMailbox("late-mail", false); err != nil {
		t.Fatal(err)
	}
	if err := mustControl(t, engine).Steer("nudge"); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	second := runtime.requests[1].Messages
	for _, msg := range second {
		if strings.Contains(msg.Text(), "late-mail") {
			t.Fatalf("non-trigger mailbox leaked into current turn: %+v", second)
		}
	}
	// Next turn should promote held mailbox before/with user prompt inject path.
	runtime2 := &scriptedProvider{streams: []provider.Stream{
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventTextDelta, Text: "ok"},
			{Type: provider.EventMessageStop},
		}},
	}}
	engine.options.Provider = runtime2
	if _, err := engine.Run(t.Context(), "turn-2", nil); err != nil {
		t.Fatal(err)
	}
	if len(runtime2.requests) != 1 {
		t.Fatalf("next-turn requests = %d", len(runtime2.requests))
	}
	msgs := runtime2.requests[0].Messages
	found := false
	for _, msg := range msgs {
		if msg.Text() == "[mailbox] late-mail" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("held mailbox not promoted on next turn: %+v", msgs)
	}
}

func TestEngineMailboxBacklogIsBoundedAndObservable(t *testing.T) {
	engine := newEngine(
		t,
		&scriptedProvider{},
		tool.NewRegistry(nil, nil),
	)
	for index := range maxMailboxBacklog {
		if err := engine.EnqueueMailbox(
			fmt.Sprintf("mail-%d", index),
			false,
		); err != nil {
			t.Fatalf("enqueue %d: %v", index, err)
		}
	}
	err := engine.EnqueueMailbox("overflow", false)
	if !protocol.IsCode(err, protocol.CodeResourceExhausted) {
		t.Fatalf("overflow error = %v", err)
	}
	if len(engine.mailboxHold) != maxMailboxBacklog {
		t.Fatalf(
			"mailbox backlog = %d, want %d",
			len(engine.mailboxHold),
			maxMailboxBacklog,
		)
	}
}

func TestEngineUndoRemovesCompleteToolTurnAndForkIsIndependent(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventToolCallDelta, ToolCall: &provider.ToolCallFragment{
				ID: "call_1", Name: "echo", Arguments: `{"text":"hello"}`,
			}},
			{Type: provider.EventMessageStop},
		}},
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventTextDelta, Text: "done"},
			{Type: provider.EventMessageStop},
		}},
	}}
	registry := tool.NewRegistry(nil, nil)
	if err := registry.Register(&echoTool{}); err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, runtime, registry)
	if _, err := engine.Run(t.Context(), "work", nil); err != nil {
		t.Fatal(err)
	}
	fork, err := engine.Fork()
	if err != nil {
		t.Fatal(err)
	}
	if fork.guard == nil || fork.guard == engine.guard {
		t.Fatal("Fork() did not construct an independent Guard")
	}
	var forkCall, engineCall *provider.ToolCall
	for messageIndex := range fork.history {
		for blockIndex := range fork.history[messageIndex].Blocks {
			if fork.history[messageIndex].Blocks[blockIndex].ToolCall != nil {
				forkCall = fork.history[messageIndex].Blocks[blockIndex].ToolCall
				break
			}
		}
	}
	for messageIndex := range engine.history {
		for blockIndex := range engine.history[messageIndex].Blocks {
			if engine.history[messageIndex].Blocks[blockIndex].ToolCall != nil {
				engineCall = engine.history[messageIndex].Blocks[blockIndex].ToolCall
				break
			}
		}
	}
	if forkCall == nil || engineCall == nil {
		t.Fatalf("tool call missing: fork=%+v engine=%+v", fork.history, engine.history)
	}
	forkCall.Name = "changed"
	if engineCall.Name != "echo" {
		t.Fatal("Fork() shares tool-call backing storage")
	}
}

func TestEngineCloneEmptyIsolatesHistoryAndGuard(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventTextDelta, Text: "hello"},
			{Type: provider.EventMessageStop},
		}},
	}}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	if _, err := engine.Run(t.Context(), "seed", nil); err != nil {
		t.Fatal(err)
	}
	clone, err := engine.CloneEmpty()
	if err != nil {
		t.Fatal(err)
	}
	if len(clone.History()) != 0 {
		t.Fatalf("clone history = %+v", clone.History())
	}
	if len(engine.History()) == 0 {
		t.Fatal("source history cleared")
	}
	if clone.guard == nil || clone.guard == engine.guard {
		t.Fatal("CloneEmpty must allocate a distinct Guard")
	}
}

func TestPostEditDiagnosticsTwoStepFixture(t *testing.T) {
	_, runtime, path := runWorkspaceEditTurn(t, "turn-diagnostics")
	if len(runtime.requests) != 3 {
		t.Fatalf("model requests = %d, want 3", len(runtime.requests))
	}
	var secondStepSawDiagnostics bool
	for _, message := range runtime.requests[2].Messages {
		for _, block := range message.Blocks {
			if block.ToolResult != nil && strings.Contains(block.ToolResult.Content, "fake diagnostic") {
				secondStepSawDiagnostics = true
			}
		}
	}
	if !secondStepSawDiagnostics {
		t.Fatal("model step after edit did not receive structured diagnostics")
	}
	if data, _ := os.ReadFile(path); string(data) != "after\n" {
		t.Fatalf("edited workspace content = %q", data)
	}
}

func TestWorkspaceRevertContract(t *testing.T) {
	engine, _, path := runWorkspaceEditTurn(t, "turn-revert")
	receipt, err := engine.RevertWorkspace(t.Context(), "turn-revert")
	if err != nil {
		t.Fatalf("revert error = %v receipt=%+v", err, receipt)
	}
	if len(receipt.Restored) != 1 || len(receipt.Conflicts) != 0 ||
		receipt.NonFileSideEffectsReverted {
		t.Fatalf("revert receipt = %+v", receipt)
	}
	if data, _ := os.ReadFile(path); string(data) != "before\n" {
		t.Fatalf("workspace after revert = %q", data)
	}
	if len(engine.History()) != 0 {
		t.Fatalf("history after workspace revert = %+v", engine.History())
	}
}

func runWorkspaceEditTurn(
	t *testing.T, turnID string,
) (*Engine, *scriptedProvider, string) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "value.txt")
	if err := os.WriteFile(path, []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := contentstore.NewMemory(contentstore.Options{})
	registry := tool.NewRegistry(nil, tool.NewResultStoreWithStore(32<<10, store))
	files, err := filetool.NewWithBackend(root, engineSandboxBackend{root: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := files.Register(registry); err != nil {
		t.Fatal(err)
	}
	journal, err := workspacejournal.New(root, store)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &scriptedProvider{streams: []provider.Stream{
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventToolCallDelta, ToolCall: &provider.ToolCallFragment{
				ID: "read", Name: "file_read", Arguments: `{"path":"value.txt"}`,
			}},
			{Type: provider.EventMessageStop},
		}},
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventToolCallDelta, ToolCall: &provider.ToolCallFragment{
				ID: "edit", Name: "file_edit",
				Arguments: `{"path":"value.txt","old":"before","new":"after"}`,
			}},
			{Type: provider.EventMessageStop},
		}},
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventTextDelta, Text: "done"},
			{Type: provider.EventMessageStop},
		}},
	}}
	engine, err := newTestEngine(Options{ProviderConfig: ProviderConfig{Provider: runtime, Route: testRoute(t),
		MaxOutputTokens: 128}, ToolConfig: ToolConfig{Tools: registry,
		Diagnostics: fakeDiagnosticRunner{}}, SecurityConfig: SecurityConfig{Workspace: root,
		Journal: journal},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.RunForTurn(t.Context(), turnID, "edit", nil); err != nil {
		t.Fatal(err)
	}
	return engine, runtime, path
}

type fakeDiagnosticRunner struct{}

func (fakeDiagnosticRunner) Run(_ context.Context, path string) (diagnostics.Receipt, error) {
	return diagnostics.Receipt{
		Path: path, Status: "completed", Runner: "fake",
		Diagnostics: []diagnostics.Diagnostic{{
			Path: path, Severity: "warning", Code: "fixture",
			Message: "fake diagnostic", Source: "fake",
		}},
	}, nil
}

func newEngine(t *testing.T, runtime provider.Provider, registry *tool.Registry) *Engine {
	t.Helper()
	engine, err := newTestEngine(Options{ProviderConfig: ProviderConfig{Provider: runtime, Route: testRoute(t), MaxOutputTokens: 128}, ToolConfig: ToolConfig{Tools: registry,
		Authorize: func(provider.ToolCall) bool { return true }},
	})
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func attachTestScope(t *testing.T, engine *Engine) *Scope {
	t.Helper()
	scope := &Scope{
		engine: engine,
		state:  newScopeState(engine),
	}
	engine.publishScope(scope)
	t.Cleanup(func() { engine.finishScope(scope) })
	return scope
}

func mustControl(t *testing.T, engine *Engine) ControlPort {
	t.Helper()
	control, err := engine.Control()
	if err != nil {
		t.Fatal(err)
	}
	return control
}

func TestControlBeforeScopeUsesCoordinatorNotActiveSentinel(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	if _, err := engine.Control(); !errors.Is(err, ErrTurnCoordinatorNotActive) {
		t.Fatalf("Control() error = %v, want %v", err, ErrTurnCoordinatorNotActive)
	}
}

func testToolDefinitions(t *testing.T, engine *Engine) []provider.ToolDefinition {
	t.Helper()
	snapshot, err := engine.options.Tools.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	definitions, _, err := engine.toolDefinitionsFromSnapshot(snapshot, TurnRequest{})
	if err != nil {
		t.Fatal(err)
	}
	return definitions
}

func messageWithText(role provider.Role, text string, turn uint64) provider.Message {
	message := provider.TextMessage(role, text)
	message.Turn = turn
	return message
}

func toolCallMessage(
	turn uint64,
	id string,
	name string,
	arguments string,
) provider.Message {
	return provider.Message{
		Role: provider.RoleAssistant, Turn: turn,
		Blocks: []provider.ContentBlock{{
			Type: provider.ContentToolCall,
			ToolCall: &provider.ToolCall{
				ID: id, Name: name, Arguments: arguments,
			},
		}},
	}
}

func toolResultMessage(turn uint64, id string, content string) provider.Message {
	return provider.Message{
		Role: provider.RoleTool, Turn: turn,
		Blocks: []provider.ContentBlock{{
			Type: provider.ContentToolResult,
			ToolResult: &provider.ToolResult{
				CallID: id, Content: content,
			},
		}},
	}
}

func testRoute(t testing.TB) model.ReadyRoute {
	return testRouteProtocol(t, "http://127.0.0.1:1", model.ProtocolOpenAIChat)
}

func mustTestRouteWithContext(t testing.TB, contextTokens uint64) model.ReadyRoute {
	t.Helper()
	catalog, err := model.NewCatalog(model.Provider{
		ID: "test", Adapter: model.AdapterOpenAICompatible,
		Endpoint: "http://127.0.0.1:1", Protocol: model.ProtocolOpenAIChat,
		Provenance: model.ProvenanceFixture,
		Models: map[string]model.Model{"model": {
			ID: "model", CanonicalID: "model", WireID: "model",
			Limits: model.Limits{
				ContextTokens: contextTokens, MaxOutputTokens: 1024,
			},
			Capabilities: model.Capabilities{Streaming: true, ToolCalls: true},
			Pricing: model.Pricing{
				InputPerMillion: 1, OutputPerMillion: 1,
				Currency: "USD", Known: true, Provenance: model.ProvenanceFixture,
			},
			Provenance: model.ProvenanceFixture,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := model.NewResolver(catalog)
	if err != nil {
		t.Fatal(err)
	}
	route, err := resolver.Resolve(model.RouteRequest{ProviderID: "test", ModelID: "model"})
	if err != nil {
		t.Fatal(err)
	}
	return route
}

func testRouteProtocol(t testing.TB, endpoint string, protocol model.WireProtocol) model.ReadyRoute {
	t.Helper()
	catalog, err := model.NewCatalog(model.Provider{
		ID: "test", Adapter: model.AdapterOpenAICompatible, Endpoint: endpoint,
		Protocol: protocol, Provenance: model.ProvenanceFixture,
		Models: map[string]model.Model{"model": {
			ID: "model", CanonicalID: "model", WireID: "model",
			Limits:       model.Limits{ContextTokens: 4096, MaxOutputTokens: 1024},
			Capabilities: model.Capabilities{Streaming: true, ToolCalls: true},
			Pricing: model.Pricing{
				InputPerMillion: 1, OutputPerMillion: 1,
				Currency: "USD", Known: true, Provenance: model.ProvenanceFixture,
			},
			Provenance: model.ProvenanceFixture,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := model.NewResolver(catalog)
	if err != nil {
		t.Fatal(err)
	}
	route, err := resolver.Resolve(model.RouteRequest{ProviderID: "test", ModelID: "model"})
	if err != nil {
		t.Fatal(err)
	}
	return route
}

func testHTTPProvider(
	t *testing.T,
	route model.ReadyRoute,
) provider.Provider {
	t.Helper()
	adapter, err := openai.NewAdapter(route.Adapter())
	if err != nil {
		t.Fatal(err)
	}
	registry, err := providerrouter.NewRegistry(adapter)
	if err != nil {
		t.Fatal(err)
	}
	routes, err := model.NewRouteSet(route, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := providerrouter.New(registry, routes, httpclient.New())
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}

type scriptedProvider struct {
	streams  []provider.Stream
	requests []provider.ModelRequest
}

type stagedCompactionFailureProvider struct {
	engine *Engine
}

func (p *stagedCompactionFailureProvider) Stream(
	context.Context,
	provider.ModelRequest,
) (provider.Stream, error) {
	p.engine.stageContextCompaction(&agentcontext.CompactionState{
		ID: "failed-turn-compaction", Phase: "fallback",
	})
	return nil, protocol.NewProblem(
		protocol.CodeInvalidArgument,
		"provider failed",
		false,
		nil,
	)
}

type cancelProvider struct{}

func (cancelProvider) Stream(ctx context.Context, _ provider.ModelRequest) (provider.Stream, error) {
	return nil, ctx.Err()
}

func (p *scriptedProvider) Stream(_ context.Context, request provider.ModelRequest) (provider.Stream, error) {
	p.requests = append(p.requests, request)
	if len(p.streams) == 0 {
		return nil, errors.New("no stream")
	}
	stream := p.streams[0]
	p.streams = p.streams[1:]
	return stream, nil
}

type catalogMutationProvider struct {
	registry *tool.Registry
	streams  []provider.Stream
	requests []provider.ModelRequest
	mutate   func() error
	calls    int
}

func (p *catalogMutationProvider) Stream(
	_ context.Context,
	request provider.ModelRequest,
) (provider.Stream, error) {
	p.requests = append(p.requests, request)
	if p.calls == 0 && p.mutate != nil {
		if err := p.mutate(); err != nil {
			return nil, err
		}
	}
	p.calls++
	if len(p.streams) == 0 {
		return nil, errors.New("no stream")
	}
	stream := p.streams[0]
	p.streams = p.streams[1:]
	return stream, nil
}

type errorStream struct {
	err error
}

func (s *errorStream) Recv() (provider.StreamEvent, error) {
	if s.err == nil {
		return provider.StreamEvent{}, io.EOF
	}
	err := s.err
	s.err = nil
	return provider.StreamEvent{}, err
}

func (*errorStream) Close() error { return nil }

type steerProvider struct {
	mu       sync.Mutex
	started  chan struct{}
	requests []provider.ModelRequest
	calls    atomic.Int32
}

func (p *steerProvider) Stream(ctx context.Context, request provider.ModelRequest) (provider.Stream, error) {
	p.mu.Lock()
	p.requests = append(p.requests, request)
	p.mu.Unlock()
	if p.calls.Add(1) == 1 {
		return &steerStream{ctx: ctx, started: p.started}, nil
	}
	return &providerfixture.SliceStream{Events: []provider.StreamEvent{
		{Type: provider.EventTextDelta, Text: "final"},
		{Type: provider.EventMessageStop},
	}}, nil
}

type steerStream struct {
	ctx     context.Context
	started chan struct{}
	step    int
}

func (s *steerStream) Recv() (provider.StreamEvent, error) {
	if s.step == 0 {
		s.step++
		close(s.started)
		return provider.StreamEvent{Type: provider.EventTextDelta, Text: "partial"}, nil
	}
	<-s.ctx.Done()
	return provider.StreamEvent{}, s.ctx.Err()
}

func (*steerStream) Close() error { return nil }

type echoTool struct {
	calls atomic.Int32
}

type largeResultTool struct{}

func (largeResultTool) Descriptor() tool.Descriptor {
	return tool.Descriptor{
		Name: "large_result", Description: "large result fixture",
		Visibility: tool.VisibleModel, Capability: tool.CapabilityRead,
		AccessMode: tool.AccessRead, ParallelPolicy: tool.ParallelSerial,
		SandboxRequirement: tool.SandboxNone,
		Availability:       tool.AvailabilityAvailable,
		InputSchema: map[string]any{
			"type":                 "object",
			"properties":           map[string]any{},
			"additionalProperties": false,
		},
	}
}

func (largeResultTool) Execute(context.Context, json.RawMessage) (tool.Result, error) {
	return tool.Result{Content: strings.Repeat("large result ", 100)}, nil
}

type smallResultTool struct{}

func (smallResultTool) Descriptor() tool.Descriptor {
	return tool.Descriptor{
		Name: "small_result", Description: "small result fixture",
		Visibility: tool.VisibleModel, Capability: tool.CapabilityRead,
		AccessMode: tool.AccessRead, ParallelPolicy: tool.ParallelConcurrent,
		SandboxRequirement: tool.SandboxNone,
		Availability:       tool.AvailabilityAvailable,
		InputSchema: map[string]any{
			"type":                 "object",
			"properties":           map[string]any{},
			"additionalProperties": false,
		},
	}
}

func (smallResultTool) Execute(context.Context, json.RawMessage) (tool.Result, error) {
	return tool.Result{Content: strings.Repeat("ok ", 25)}, nil
}

type processSessionTool struct{}

func (processSessionTool) Descriptor() tool.Descriptor {
	return tool.Descriptor{
		Name: "exec_command", Description: "process fixture",
		Visibility: tool.VisibleModel, Capability: tool.CapabilityProcess,
		AccessMode: tool.AccessRead, ParallelPolicy: tool.ParallelConcurrent,
		SandboxRequirement: tool.SandboxNone,
		Availability:       tool.AvailabilityAvailable,
		InputSchema: map[string]any{
			"type":                 "object",
			"properties":           map[string]any{},
			"additionalProperties": false,
		},
	}
}

func (processSessionTool) Execute(
	context.Context,
	json.RawMessage,
) (tool.Result, error) {
	return tool.Result{}, errors.New("Execute should not be called")
}

func (processSessionTool) ExecuteOutcome(
	context.Context,
	json.RawMessage,
) (tool.Result, tool.Outcome, error) {
	outcome := tool.Outcome{
		Status: tool.OutcomeSucceeded,
		Facts: &tool.OutcomeFacts{ProcessSession: &tool.ProcessSessionFact{
			SessionID: "session-1",
			Cursor:    7,
			Running:   true,
		}},
	}
	return tool.Result{
		Content: "partial output",
		Metadata: map[string]any{
			"session_id": "session-1",
			"cursor":     uint64(7),
			"running":    true,
		},
		Outcome: tool.CloneOutcome(&outcome),
	}, outcome, nil
}

type countingCatalogExecutor struct {
	descriptor tool.Descriptor
	calls      atomic.Int32
}

func trustedExternalRegistration(executor tool.Executor) tool.Registration {
	descriptor := executor.Descriptor()
	return tool.NewExternalRegistration(
		tool.ExternalFromDescriptor(descriptor),
		tool.TrustedBindingFromDescriptor(descriptor),
		executor,
	)
}

func echoDescriptor() tool.Descriptor {
	return (&echoTool{}).Descriptor()
}

func (e *countingCatalogExecutor) Descriptor() tool.Descriptor {
	return e.descriptor
}

func (e *countingCatalogExecutor) Execute(
	_ context.Context,
	raw json.RawMessage,
) (tool.Result, error) {
	e.calls.Add(1)
	return tool.Result{Content: string(raw)}, nil
}

type failingTool struct{}

func (failingTool) Descriptor() tool.Descriptor {
	return tool.Descriptor{
		Name: "fail", Description: "always fail", Visibility: tool.VisibleModel,
		Capability: tool.CapabilityRead, AccessMode: tool.AccessRead,
		ParallelPolicy: tool.ParallelConcurrent, SandboxRequirement: tool.SandboxNone,
		Availability: tool.AvailabilityAvailable,
		InputSchema: map[string]any{
			"type":                 "object",
			"properties":           map[string]any{},
			"additionalProperties": false,
		},
	}
}

func (failingTool) Execute(context.Context, json.RawMessage) (tool.Result, error) {
	return tool.Result{}, errors.New("intentional failure")
}

type resultErrorTool struct{}

func (resultErrorTool) Descriptor() tool.Descriptor {
	descriptor := failingTool{}.Descriptor()
	descriptor.Name = "result_error"
	descriptor.Description = "return a structured tool failure"
	return descriptor
}

func (resultErrorTool) Execute(context.Context, json.RawMessage) (tool.Result, error) {
	return tool.Result{Content: "structured failure", IsError: true}, nil
}

type panickingTool struct{}

func (panickingTool) Descriptor() tool.Descriptor {
	descriptor := failingTool{}.Descriptor()
	descriptor.Name = "panic_tool"
	descriptor.Description = "panic fixture"
	return descriptor
}

func (panickingTool) Execute(context.Context, json.RawMessage) (tool.Result, error) {
	panic("intentional panic")
}

func (*echoTool) Descriptor() tool.Descriptor {
	return tool.Descriptor{
		Name: "echo", Description: "echo text", Visibility: tool.VisibleModel,
		Capability: tool.CapabilityRead, AccessMode: tool.AccessRead,
		ParallelPolicy: tool.ParallelConcurrent, SandboxRequirement: tool.SandboxNone,
		Availability: tool.AvailabilityAvailable,
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"text": map[string]any{"type": "string"}},
			"required":   []string{"text"}, "additionalProperties": false,
		},
	}
}

func (t *echoTool) Execute(_ context.Context, raw json.RawMessage) (tool.Result, error) {
	t.calls.Add(1)
	return tool.Result{Content: string(raw)}, nil
}

func failedTurnCapsuleAt(
	t *testing.T,
	message provider.Message,
	goal string,
	code string,
) bool {
	t.Helper()
	if message.Role != provider.RoleSystem ||
		!strings.Contains(message.Text(), "[turn_terminal]") ||
		!strings.Contains(message.Text(), `"goal":"`+goal+`"`) {
		return false
	}
	return code == "" || strings.Contains(message.Text(), `"code":"`+code+`"`)
}

func assertOneTerminal(t *testing.T, states []State, want State) {
	t.Helper()
	count := 0
	var got State
	for _, state := range states {
		if state == Completed || state == Failed || state == Canceled {
			count++
			got = state
		}
	}
	if count != 1 || got != want {
		t.Fatalf("terminal count=%d state=%q all=%v", count, got, states)
	}
}

func truthEntityContains(
	capsule agentcontext.TruthCapsule,
	kind string,
	fragment string,
) bool {
	for _, entity := range capsule.Entities {
		if entity.Kind == kind && strings.Contains(entity.Value, fragment) {
			return true
		}
	}
	return false
}

func assertToolPairs(t *testing.T, messages []provider.Message) {
	t.Helper()
	calls := make(map[string]struct{})
	results := make(map[string]struct{})
	for _, message := range messages {
		for _, call := range messageToolCalls(message) {
			calls[call.ID] = struct{}{}
		}
		if id := messageToolResultID(message); id != "" {
			results[id] = struct{}{}
		}
	}
	if len(calls) != len(results) {
		t.Fatalf("tool pairing mismatch: calls=%v results=%v", calls, results)
	}
	for id := range calls {
		if _, exists := results[id]; !exists {
			t.Fatalf("tool call %q has no retained result", id)
		}
	}
}

func TestMidTurnSurfacePruneSavesOverpressureTurn(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	route := mustTestRouteWithContext(t, 1024)
	engine.options.Route = route
	engine.options.Routes, _ = model.NewRouteSet(route, nil, false)
	engine.options.MaxOutputTokens = 128
	engine.options.Context.Window.AutoTokens = 500
	bigResult := func(callID, body string) provider.Message {
		encoded, err := json.Marshal(
			tool.ModelResult("read", tool.Result{Content: body}),
		)
		if err != nil {
			t.Fatal(err)
		}
		return toolResultMessage(1, callID, string(encoded))
	}
	history := []provider.Message{
		messageWithText(provider.RoleUser, "fix the parser", 1),
		toolCallMessage(1, "call_1", "read", `{}`),
		bigResult("call_1", strings.Repeat("first ", 600)),
		toolCallMessage(1, "call_2", "read", `{}`),
		bigResult("call_2", strings.Repeat("second ", 600)),
		toolCallMessage(1, "call_3", "read", `{}`),
		bigResult("call_3", "latest"),
	}
	snapshot := agentcontext.NewMessageLedger(agentcontext.LedgerInput{}).Snapshot()
	var receipts []*CompactionReceipt
	window, err := engine.runCompactGate(
		t.Context(), &history, snapshot, 128, CompactionPhaseMidTurn, true,
		func(_ State, event Event) error {
			if event.Compaction != nil {
				receipts = append(receipts, event.Compaction)
			}
			return nil
		}, 0, engine.contextViewProject(nil),
	)
	if err != nil {
		t.Fatalf("over-pressure turn failed instead of degrading: %v", err)
	}
	if window.hardLimit != 0 && window.total > window.hardLimit {
		t.Fatalf("degraded window still overflows: %+v", window)
	}
	assertToolPairs(t, history)
	if !currentTurnKeepsUser(history, "fix the parser") {
		t.Fatalf("pinned user missing: %+v", history)
	}
	joined := historyTexts(history)
	if strings.Contains(joined, strings.Repeat("first ", 20)) ||
		strings.Contains(joined, strings.Repeat("second ", 20)) {
		t.Fatalf("closed results stayed in the working set: %s", joined)
	}
	if len(receipts) == 0 {
		t.Fatal("expected a degradation receipt")
	}
}
