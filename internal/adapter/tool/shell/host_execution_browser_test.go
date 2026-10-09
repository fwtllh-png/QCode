//go:build capability

package shell

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	toolguard "github.com/fwtllh-png/QCode/internal/adapter/tool/guard"
	"github.com/fwtllh-png/QCode/internal/platform/process"
	"github.com/fwtllh-png/QCode/internal/security/policy"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

// Opt-in acceptance runs the actual browser fixtures through the host branch,
// including their independent Runtime and sandbox. Build the Web binary first.
func TestHostExecutionBrowserFixtures(t *testing.T) {
	binary := os.Getenv("QCODE_HOST_E2E_BINARY")
	if binary == "" {
		t.Skip("set QCODE_HOST_E2E_BINARY to a built Web binary")
	}
	root, err := filepath.Abs("../../../..")
	if err != nil {
		t.Fatal(err)
	}
	backend, err := sandbox.NewPlatformBackend(sandbox.Options{WorkspaceRoot: root, PrivateTemp: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sandbox.CloseBackend(backend) })
	manager := process.NewSessionManager(65536)
	t.Cleanup(manager.CloseAll)
	registry := tool.NewRegistry(nil, nil)
	if err := RegisterWithManagerAndBackend(registry, root, manager, backend); err != nil {
		t.Fatal(err)
	}
	runtime := policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass)
	guarded, err := toolguard.New(toolguard.Options{Registry: registry, Workspace: root, Policy: runtime})
	if err != nil {
		t.Fatal(err)
	}
	ctx := tool.WithInvocationIdentity(t.Context(), tool.InvocationIdentity{ThreadID: "host-browser", TurnID: "host-browser"})
	args := map[string]any{"command": "npm --prefix web run test:e2e -- tests/e2e/visual.spec.ts --output ../.tmp/playwright-host-execution", "execution_target": "host", "env": map[string]string{"QCODE_E2E_BINARY": binary}, "yield_time_ms": 1000}
	if deadline, ok := t.Deadline(); ok {
		args["timeout_ms"] = time.Until(deadline).Milliseconds()
	}
	name := "exec_command"
	for call := 0; ; call++ {
		raw, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		result, err := guarded.Execute(ctx, "browser-"+strconv.Itoa(call), name, raw)
		if result.Content != "" {
			t.Log(result.Content)
		}
		if err != nil || result.IsError {
			t.Fatalf("browser fixtures: %v %+v", err, result)
		}
		if result.Metadata["execution_target"] != "host" {
			t.Fatal("lost host execution identity")
		}
		if result.Metadata["running"] != true {
			break
		}
		args = map[string]any{"session_id": result.Metadata["session_id"], "yield_time_ms": 1000}
		name = "write_stdin"
	}
}
