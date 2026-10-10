package shell

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/platform/process"
)

func commandResultRegistry(t *testing.T) (*tool.Registry, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "input.txt"), []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := process.NewSessionManager(4096)
	t.Cleanup(manager.CloseAll)
	registry := tool.NewRegistry(nil, nil)
	if err := RegisterWithManagerAndBackend(registry, root, manager, passthroughBackend{}); err != nil {
		t.Fatal(err)
	}
	return registry, root
}

func TestCommandsRecordExitWithoutCoverageDeclarations(t *testing.T) {
	registry, _ := commandResultRegistry(t)
	for _, tc := range []struct {
		command string
		exit    int
	}{
		{"test -f input.txt", 0},
		{"printf 'tests passed'; exit 7", 7},
		{"false && printf masked", 1},
	} {
		result := executeProcessTool(t, registry, processTestThread, "exec_command", map[string]any{"command": tc.command})
		if result.Metadata["exit_code"] != tc.exit || result.IsError != (tc.exit != 0) {
			t.Fatalf("lost actual exit: %+v", result)
		}
		if result.Metadata["verification_evidence"] != nil || result.Outcome != nil && result.Outcome.Facts != nil && result.Outcome.Facts.Verification != nil {
			t.Fatal("command manufactured coverage")
		}
	}
}
