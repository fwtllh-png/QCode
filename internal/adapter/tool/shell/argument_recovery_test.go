package shell

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	toolguard "github.com/fwtllh-png/QCode/internal/adapter/tool/guard"
	toolresult "github.com/fwtllh-png/QCode/internal/adapter/tool/result"
	"github.com/fwtllh-png/QCode/internal/platform/process"
	"github.com/fwtllh-png/QCode/internal/security/policy"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func argumentRecoveryGuard(t *testing.T) (*tool.Registry, *toolguard.Guard, string) {
	t.Helper()
	root := t.TempDir()
	backend, err := sandbox.NewPlatformBackend(sandbox.Options{
		WorkspaceRoot: root, PrivateTemp: t.TempDir(), SkipPATHReadRoots: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sandbox.CloseBackend(backend) })
	manager := process.NewSessionManager(4096)
	t.Cleanup(manager.CloseAll)
	registry := tool.NewRegistry(nil, nil)
	if err := RegisterWithManagerAndBackend(registry, root, manager, backend); err != nil {
		t.Fatal(err)
	}
	guarded, err := toolguard.New(toolguard.Options{
		Registry: registry, Workspace: root,
		Policy: policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass),
	})
	if err != nil {
		t.Fatal(err)
	}
	return registry, guarded, root
}

func TestExecCommandArgumentErrorsRemainRecoverable(t *testing.T) {
	for _, target := range []string{"sandbox", "host"} {
		t.Run(target, func(t *testing.T) {
			registry, guarded, root := argumentRecoveryGuard(t)
			for _, test := range []struct {
				name   string
				fields map[string]any
			}{
				{"unknown_legacy_parameter", map[string]any{"verification": "check"}},
				{"timeout", map[string]any{"timeout_ms": -1}},
				{"output", map[string]any{"output_tokens": -1}},
				{"terminal_size", map[string]any{"rows": 24}},
				{"environment_name", map[string]any{"env": map[string]string{"INVALID=NAME": "value"}}},
			} {
				t.Run(test.name, func(t *testing.T) {
					test.fields["command"] = "printf unexpected >> should-not-run"
					test.fields["execution_target"] = target
					raw, err := json.Marshal(test.fields)
					if err != nil {
						t.Fatal(err)
					}
					call := provider.ToolCall{ID: target + test.name, Name: "exec_command", Arguments: string(raw)}
					ctx := tool.WithInvocationIdentity(t.Context(), tool.InvocationIdentity{
						ThreadID: "thread", TurnID: "turn", CallID: call.ID,
					})
					result, err := guarded.Execute(ctx, call.ID, call.Name, raw)
					if !errors.Is(err, tool.ErrInvalidArguments) {
						t.Fatalf("argument failure lost its classification: %v", err)
					}
					recovered, ok := toolresult.RecoverResult(registry, call, result, err)
					if !ok || !recovered.IsError || recovered.Metadata["error_category"] != "invalid_arguments" {
						t.Fatalf("argument failure cannot return to the model: %+v, recovered=%v", recovered, ok)
					}
					if _, err := os.Stat(filepath.Join(root, "should-not-run")); !os.IsNotExist(err) {
						t.Fatalf("invalid arguments started the command: %v", err)
					}
				})
			}
		})
	}
}

func TestHostArgumentRecoveryKeepsCompletedBatchWork(t *testing.T) {
	registry, guarded, root := argumentRecoveryGuard(t)
	if err := os.WriteFile(filepath.Join(root, "input.txt"), []byte("input"), 0o600); err != nil {
		t.Fatal(err)
	}
	completed := provider.ToolCall{ID: "completed", Name: "exec_command",
		Arguments: `{"command":"printf x >> completed","execution_target":"host"}`}
	rejected := provider.ToolCall{ID: "rejected", Name: "exec_command",
		Arguments: `{"command":"printf unexpected >> should-not-run","execution_target":"host","timeout_ms":-1}`}
	cache := &tool.ResultCache{}
	executed := make(map[string]tool.Result)
	runBatch := func(calls []provider.ToolCall) tool.BatchOutcome {
		t.Helper()
		outcome := tool.ExecuteBatch(tool.BatchExecution{
			Context: t.Context(), Calls: calls, Plan: cache.Plan(calls, executed, registry),
			Execute: func(ctx context.Context, call provider.ToolCall) (tool.Result, error) {
				ctx = tool.WithInvocationIdentity(ctx, tool.InvocationIdentity{
					ThreadID: "thread", TurnID: "same-turn", CallID: call.ID,
				})
				return guarded.Execute(ctx, call.ID, call.Name, json.RawMessage(call.Arguments))
			},
			Recover: func(call provider.ToolCall, result tool.Result, err error) (tool.Result, bool) {
				return toolresult.RecoverResult(registry, call, result, err)
			},
			FailureCategory: toolresult.FailureCategory,
		})
		for index, call := range calls {
			executed[call.ID] = outcome.Results[index]
		}
		return outcome
	}
	first := runBatch([]provider.ToolCall{completed, rejected})
	if first.Error != nil || first.Results[0].IsError || !first.Results[1].IsError ||
		first.Results[1].Metadata["error_category"] != "invalid_arguments" {
		t.Fatalf("repairable argument failure terminated the batch: %+v", first)
	}
	if _, err := os.Stat(filepath.Join(root, "should-not-run")); !os.IsNotExist(err) {
		t.Fatalf("rejected command started: %v", err)
	}
	corrected := provider.ToolCall{ID: "corrected", Name: "exec_command",
		Arguments: `{"command":"test -f input.txt","execution_target":"host"}`}
	second := runBatch([]provider.ToolCall{completed, corrected})
	if second.Error != nil || second.Results[0].IsError || second.Results[1].IsError {
		t.Fatalf("corrected command failed: %+v", second)
	}
	if second.Results[1].Metadata["exit_code"] != 0 {
		t.Fatalf("corrected command did not exit successfully: %+v", second.Results[1])
	}
	contents, err := os.ReadFile(filepath.Join(root, "completed"))
	if err != nil || string(contents) != "x" {
		t.Fatalf("completed sibling was lost or replayed: %q, %v", contents, err)
	}
}

func TestHostPreparationKeepsEnvironmentFailureClassification(t *testing.T) {
	registry := newGuardTestRegistry(t)
	_, _, executor, err := registry.Resolve("exec_command")
	if err != nil {
		t.Fatal(err)
	}
	_, err = executor.(toolguard.AuthorizedSessionExecutor).PrepareAuthorizedSession(t.Context(), tool.PreparedInvocation{
		Arguments: json.RawMessage(`{"command":"true","execution_target":"host"}`),
	})
	if err == nil || errors.Is(err, tool.ErrInvalidArguments) || errors.Is(err, tool.ErrPrecondition) {
		t.Fatalf("missing environment policy was classified as an argument error: %v", err)
	}
	if content, recoverable := toolresult.RecoverableFailure(err); recoverable {
		t.Fatalf("environment failure incorrectly asks the model to fix arguments: %s", content)
	}
}
