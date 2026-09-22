//go:build darwin

package shell

import (
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/platform/process"
)

func TestExecCommandPublishesBoundedOutput(t *testing.T) {
	manager := process.NewSessionManager(128 << 10)
	t.Cleanup(manager.CloseAll)
	registry := tool.NewRegistry(nil, nil)
	if err := RegisterWithManagerAndBackend(
		registry,
		t.TempDir(),
		manager,
		passthroughBackend{},
	); err != nil {
		t.Fatal(err)
	}
	_, _, executor, err := registry.Resolve("exec_command")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := executor.(tool.OutcomeExecutor); !ok {
		t.Fatalf("exec_command executor %T has no typed outcome boundary", executor)
	}
	if tool.DispositionFor(executor) != tool.DispositionDetached {
		t.Fatalf("exec_command disposition = %q", tool.DispositionFor(executor))
	}
	result := executeProcessTool(
		t,
		registry,
		processTestThread,
		"exec_command",
		map[string]any{
			"command":       `dd if=/dev/zero bs=65536 count=1 2>/dev/null | tr '\000' x`,
			"output_tokens": 1024,
		},
	)
	if !strings.Contains(result.Content, "[output truncated:") ||
		len(result.Content) > 4200 {
		t.Fatalf("bounded model content bytes = %d", len(result.Content))
	}
	if omitted, _ := result.Metadata["omitted_bytes"].(int); omitted == 0 {
		t.Fatalf("output metadata = %#v", result.Metadata)
	}
}

func TestShellReadStillPublishesCollectorReceipt(t *testing.T) {
	manager := process.NewSessionManager(4096)
	t.Cleanup(manager.CloseAll)
	registry := tool.NewRegistry(nil, nil)
	if err := RegisterWithManagerAndBackend(
		registry,
		t.TempDir(),
		manager,
		passthroughBackend{},
	); err != nil {
		t.Fatal(err)
	}
	result := executeProcessTool(
		t,
		registry,
		processTestThread,
		"shell_read",
		map[string]any{
			"command": `dd if=/dev/zero bs=2097152 count=1 2>/dev/null | tr '\000' x`,
		},
	)
	receipt, ok := result.Metadata["output_receipt"].(process.OutputReceipt)
	if !ok ||
		receipt.Stdout.TotalBytes != 2<<20 ||
		receipt.Stdout.RetainedBytes != process.ModelOutputLimitBytes {
		t.Fatalf("output receipt = %+v, found=%t", receipt, ok)
	}
}
