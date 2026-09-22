package shell

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/environment"
	"github.com/fwtllh-png/QCode/internal/platform/process"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
	"github.com/fwtllh-png/QCode/internal/testutil/tooltest"
)

func TestServiceFactsSinceCursorSurvivesRotation(t *testing.T) {
	first := environment.Fact{Resource: "a", Category: "c1"}
	second := environment.Fact{Resource: "b", Category: "c2"}
	if got := serviceFactsSinceCursor(nil, []environment.Fact{first}); len(got) != 1 {
		t.Fatalf("empty prior: %+v", got)
	}
	if got := serviceFactsSinceCursor(
		[]environment.Fact{first}, []environment.Fact{first, second},
	); len(got) != 1 || got[0] != second {
		t.Fatalf("append cursor: %+v", got)
	}
	// Rotation (service list reset): length heuristics would drop facts;
	// the cursor must re-report everything current.
	rotated := []environment.Fact{{Resource: "z", Category: "c3"}}
	if got := serviceFactsSinceCursor([]environment.Fact{first, second}, rotated); len(got) != 1 || got[0] != rotated[0] {
		t.Fatalf("rotation cursor: %+v", got)
	}
}

func TestSandboxedChildPATHMatchesPreflightSearchOrder(t *testing.T) {
	workspace := t.TempDir()
	backend, err := sandbox.NewPlatformBackend(sandbox.Options{
		WorkspaceRoot: workspace, PrivateTemp: t.TempDir(), SkipPATHReadRoots: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sandbox.CloseBackend(backend) })
	manager := process.NewSessionManager(4096)
	t.Cleanup(manager.CloseAll)
	registry := tool.NewRegistry(nil, nil)
	if err := RegisterWithManagerAndBackend(registry, workspace, manager, backend); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{"command": `printf %s "$PATH"`})
	if err != nil {
		t.Fatal(err)
	}
	result, err := tooltest.Execute(
		tool.WithInvocationIdentity(t.Context(), tool.InvocationIdentity{
			SessionID: "session-path-order",
			ThreadID:  processTestThread,
			TurnID:    "turn-path-order",
			CallID:    "call-path-order",
		}),
		registry,
		tool.Call{Name: "exec_command", Arguments: raw},
	)
	if err != nil || result.IsError {
		t.Fatalf("path probe: result=%+v err=%v", result, err)
	}
	policy, ok := sandbox.BackendPolicy(backend)
	if !ok {
		t.Fatal("backend has no policy")
	}
	expected := strings.Join(
		process.ToolchainSearchPath(policy.Toolchains.BinDirs),
		string(os.PathListSeparator),
	)
	if strings.TrimSpace(result.Content) != expected {
		t.Fatalf(
			"child PATH %q does not match preflight search order %q",
			strings.TrimSpace(result.Content), expected,
		)
	}
}

func TestForegroundTimeoutSharesExecCeiling(t *testing.T) {
	if timeout, err := foregroundTimeout(foregroundInput{TimeoutMS: 1000}); err != nil || timeout.Milliseconds() != 1000 {
		t.Fatalf("explicit timeout = %v err=%v", timeout, err)
	}
	if timeout, err := foregroundTimeout(foregroundInput{}); err != nil || timeout != DefaultForegroundTimeout {
		t.Fatalf("default timeout = %v err=%v", timeout, err)
	}
	over := maxProcessTimeout.Milliseconds() + 1
	if _, err := foregroundTimeout(foregroundInput{TimeoutMS: over}); err == nil {
		t.Fatal("timeout beyond the exec ceiling was accepted")
	}
}
