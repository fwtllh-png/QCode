package engine

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	toolresult "github.com/fwtllh-png/QCode/internal/adapter/tool/result"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/toolsearch"
	"github.com/fwtllh-png/QCode/internal/observability/diagnostics"
	"github.com/fwtllh-png/QCode/internal/platform/tokenestimate"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type toolResultCache = tool.ResultCache

func (e *Engine) runToolsWithCache(
	ctx context.Context,
	turnID string,
	calls []provider.ToolCall,
	executed map[string]tool.Result,
	cache *toolResultCache,
	kernel *turnkernel.RuntimeKernel,
	send func(State, Event) error,
) ([]tool.Result, error) {
	if kernel == nil {
		return nil, protocol.NewProblem(
			protocol.CodeInternal,
			"turn kernel is required for tool execution",
			false,
			nil,
		)
	}
	if err := send(RunningTools, Event{}); err != nil {
		return nil, err
	}
	identity := tool.InvocationIdentityFrom(ctx)
	if identity.SessionID == "" {
		identity.SessionID = e.options.SessionID
	}
	if identity.ThreadID == "" {
		identity.ThreadID = e.options.SessionID
	}
	if identity.TurnID == "" {
		identity.TurnID = turnID
	}
	toolCtx, cancel := context.WithCancelCause(tool.WithInvocationIdentity(ctx, identity))
	toolCtx = tool.WithInvocationSource(toolCtx, tool.InvocationSourceModel)
	toolCtx = toolsearch.WithEnabled(toolCtx, e.toolEnabled)
	// The per-result ceiling guides producer pre-clamping and bounds any
	// single projection; the batch total is the aggregate pool the batch
	// admission redistributes by real result size. Both derive from the same
	// authorities as the previous equal split, whose aggregate ceiling was
	// exactly this total.
	resultBudget := max(uint64(1), e.autoCompactLimit())
	batchBudget := resultBudget
	scope := e.executionScope()
	if scope == nil {
		cancel(nil)
		return nil, errors.New("turn scope is not active")
	}
	scope.mu.Lock()
	surfaceMaxBytes := scope.state.toolSurfaceMaxBytes
	surfaceItemBytes := scope.state.toolSurfaceItemBytes
	scope.mu.Unlock()
	if surfaceItemBytes > 0 && surfaceMaxBytes > 0 {
		itemTokens := max(uint64(1), tokenestimate.MaxTokensForBytes(uint64(surfaceItemBytes)))
		totalTokens := max(uint64(1), tokenestimate.MaxTokensForBytes(uint64(surfaceMaxBytes)))
		resultBudget = min(resultBudget, itemTokens)
		batchBudget = min(batchBudget, totalTokens)
	}
	toolCtx = tool.WithResultTokenBudget(toolCtx, min(e.options.Tools.ResultTokenCapacity(), resultBudget))
	toolCtx = tool.WithResultBatchBudget(toolCtx, batchBudget)

	toolCtx = withToolAccount(toolCtx, &toolAccount{
		engine: e,
		emit:   func(event Event) error { return send(RunningTools, event) },
	})
	stream := tool.NewOutputStream(
		e.options.MaxToolStreamBytes,
		func(output tool.OutputProjection) {
			_ = send(RunningTools, Event{ToolOutput: &ToolOutput{
				Tool: output.Tool, CallID: output.CallID,
				Stream: output.Stream, Chunk: output.Chunk,
				Cursor: output.Cursor, Truncated: output.Truncated,
			}})
		},
	)
	defer stream.Close()

	e.setActiveCancel(cancel)
	defer e.clearActiveCancel()
	defer cancel(nil)

	sched := scope.state.scheduler
	diagnosticReceipts := make(map[string][]diagnostics.Receipt, len(calls))
	return kernel.ExecuteToolEffect(turnkernel.ToolEffect{
		Context: toolCtx, Calls: calls, Executed: executed,
		Cache: cache, Registry: e.options.Tools,
		Admit:       e.admitToolBatch,
		BeforeStart: e.contextAuthority().NoteToolCall,
		PublishStart: func(call provider.ToolCall) error {
			copy := call
			return send(RunningTools, Event{ToolCall: &copy})
		},
		PublishAborted: func(
			call provider.ToolCall,
			result tool.Result,
		) error {
			return send(RunningTools, Event{
				ToolCall: &call,
				Result:   &result,
			})
		},
		Execute: func(callCtx context.Context, call provider.ToolCall) (tool.Result, error) {
			binding := tool.BindingForCall(call)
			if e.options.VerificationOnly && call.Name == "exec_command" {
				var declaration struct {
					Verification string `json:"verification"`
				}
				if json.Unmarshal([]byte(call.Arguments), &declaration) != nil ||
					declaration.Verification == "" {
					return tool.Result{
						Content:  "verifier role requires exec_command with verification and covered_paths",
						IsError:  true,
						Metadata: map[string]any{"error_category": "child_authority_denied"},
					}, nil
				}
			}
			finishOnly := tool.FinishOnlyEnabled(toolCtx)
			if finishOnly {
				canonical, descriptor, _, resolveErr :=
					e.options.Tools.ResolveBound(call.Name, binding)
				if resolveErr == nil &&
					!tool.FinishOnlyAllowed(canonical, descriptor) {
					return tool.Result{
						Content: "read-only exploration is disabled after repeated " +
							"model samples without structured progress; apply a " +
							"workspace change, run a verification command, update the " +
							"plan, or call turn_complete",
						IsError: true,
						Metadata: map[string]any{
							"error_category":  "no_progress_finish_only",
							"required_action": "finish_current_batch",
							"retry_original":  false,
						},
					}, nil
				}
			}
			if blocked := e.observationGate(call, finishOnly); blocked != nil {
				return *blocked, nil
			}
			if !e.toolCallEnabled(call.Name, binding) {
				return tool.Result{
					Content: "tool disabled by Session Profile: " + call.Name,
					IsError: true,
					Metadata: map[string]any{
						"error_category": "tool_disabled",
					},
				}, nil
			}
			span := e.beginToolSpan(call)
			callCtx = tool.WithOutputObserver(callCtx, stream.Observe(call))
			callCtx = tool.WithExecutionAdmission(callCtx, sched.Admit)
			callCtx = e.tracer().Context(callCtx, span.ID())
			result, err := e.guard.ExecuteBound(
				callCtx,
				call.ID,
				call.Name,
				json.RawMessage(call.Arguments),
				binding,
			)
			e.endToolSpan(call, span, result, err)
			return result, err
		},
		Recover: func(
			call provider.ToolCall,
			result tool.Result,
			err error,
		) (tool.Result, bool) {
			return toolresult.RecoverResult(
				e.options.Tools,
				call,
				result,
				err,
			)
		},
		FailureCategory: toolFailureCategory,
		BeforeClose: func(
			call provider.ToolCall,
			result *tool.Result,
			batchMutated bool,
			mutationRevision uint64,
		) {
			if call.Name == "file_read" && result != nil && !result.IsError {
				if reason := e.takeReadInvalidation(call.ID); reason != "" {
					if result.Metadata == nil {
						result.Metadata = make(map[string]any)
					}
					result.Metadata["invalidated_read"] = reason
				}
			}
			e.bindVerificationEvidence(
				call,
				result,
				batchMutated,
				mutationRevision,
			)
			for _, change := range turnkernel.ObservedFileChanges(*result) {
				relative, ok := agentcontext.WorkspaceRelative(e.options.Workspace, change.Path)
				if !ok {
					// The change sits outside the workspace (for example an
					// absolute path a tool reported for a host location).
					// It still enters the turn-local diff, but durable
					// workspace evidence must stay workspace-relative:
					// bound-path validation rejects anything else and one
					// malformed entry would fail every later snapshot.
					scope.state.diff.Record(turnkernel.TurnDiffEntry{
						Path: change.Path, Tool: call.Name, Kind: change.Kind,
						Added: change.Added, Removed: change.Removed,
					})
					continue
				}
				change.Path = relative
				scope.state.diff.Record(turnkernel.TurnDiffEntry{
					Path: change.Path, Tool: call.Name, Kind: change.Kind,
					Added: change.Added, Removed: change.Removed,
				})
				e.contextAuthority().ObservePath(
					e.options.Workspace, agentcontext.SourceEdited, e.turn, change.Path,
				)
				e.contextAuthority().ObserveChange(e.options.Workspace, change, e.turn)
			}
			if !result.IsError {
				if read := turnkernel.ObservedFileRead(*result); read != "" {
					if relative, ok := agentcontext.WorkspaceRelative(e.options.Workspace, read); ok {
						e.contextAuthority().ObservePath(
							e.options.Workspace,
							agentcontext.SourceRead,
							e.turn,
							relative,
						)
					}
				}
				e.contextAuthority().ObserveToolResult(
					e.options.Workspace,
					call,
					*result,
					e.turn,
				)
			} else {
				e.contextAuthority().ObserveToolFailure(
					call,
					*result,
					e.turn,
				)
			}
			if result.Outcome != nil && result.Outcome.Facts != nil {
				diagnosticReceipts[call.ID] = result.Outcome.Facts.Diagnostics
			}
			e.recordTurnDiagnostics(diagnosticReceipts[call.ID])
			e.contextAuthority().ObserveDiagnostics(
				e.options.Workspace,
				diagnosticReceipts[call.ID],
			)
		},
		CompletionCandidate: e.completionCandidate,
		AfterClose: func(call provider.ToolCall, result tool.Result) error {
			if call.Name == toolsearch.ToolName && !result.IsError {
				return e.refreshScopeCatalog()
			}
			return nil
		},
		PublishResult: func(
			call provider.ToolCall,
			result tool.Result,
		) error {
			return send(RunningTools, Event{
				ToolCall:    &call,
				Result:      &result,
				Diagnostics: diagnosticReceipts[call.ID],
				FileChanges: turnkernel.ObservedFileChanges(result),
			})
		},
	})
}
