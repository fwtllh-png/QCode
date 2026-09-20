package shell

import (
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/platform/process"
)

func TestShellReadTimeoutHint(t *testing.T) {
	manager := process.NewSessionManager(4096)
	defer manager.CloseAll()
	registry := tool.NewRegistry(nil, nil)
	if err := RegisterWithManagerAndBackend(
		registry, t.TempDir(), manager, passthroughBackend{},
	); err != nil {
		t.Fatal(err)
	}
	result := executeProcessTool(
		t,
		registry,
		processTestThread,
		"shell_read",
		map[string]any{"command": "sleep 5", "timeout_ms": 50},
	)
	if !result.IsError {
		t.Fatalf("expected error result: %+v", result)
	}
	if !strings.Contains(result.Content, ForegroundTimeoutHint) {
		t.Fatalf("content missing hint: %q", result.Content)
	}
	if result.Metadata["timed_out"] != true {
		t.Fatalf("metadata=%+v", result.Metadata)
	}
	hint, _ := result.Metadata["timeout_hint"].(string)
	if !strings.Contains(hint, "exec_command") ||
		!strings.Contains(hint, "write_stdin") {
		t.Fatalf("timeout_hint=%q", hint)
	}
}

// The default foreground timeout is a documented contract: it bounds quick
// inspections while anything longer is steered to the session protocol.
func TestDefaultForegroundTimeoutContract(t *testing.T) {
	if DefaultForegroundTimeout != 60*time.Second {
		t.Fatalf("DefaultForegroundTimeout = %s", DefaultForegroundTimeout)
	}
}

func TestForegroundTimeoutResolution(t *testing.T) {
	cases := map[string]struct {
		input foregroundInput
		want  time.Duration
	}{
		"omitted uses the default": {foregroundInput{}, DefaultForegroundTimeout},
		"zero uses the default":    {foregroundInput{TimeoutMS: 0}, DefaultForegroundTimeout},
		"negative uses the default": {
			foregroundInput{TimeoutMS: -5}, DefaultForegroundTimeout,
		},
		"explicit value wins": {
			foregroundInput{TimeoutMS: 2500}, 2500 * time.Millisecond,
		},
	}
	for name, test := range cases {
		if got := foregroundTimeout(test.input); got != test.want {
			t.Errorf("%s: foregroundTimeout = %s, want %s", name, got, test.want)
		}
	}
}

// A command that finishes quickly must succeed without an explicit timeout:
// the default deadline applies but does not disturb fast reads.
func TestShellReadWithoutTimeoutCompletesQuickCommand(t *testing.T) {
	manager := process.NewSessionManager(4096)
	defer manager.CloseAll()
	registry := tool.NewRegistry(nil, nil)
	if err := RegisterWithManagerAndBackend(
		registry, t.TempDir(), manager, passthroughBackend{},
	); err != nil {
		t.Fatal(err)
	}
	result := executeProcessTool(
		t,
		registry,
		processTestThread,
		"shell_read",
		map[string]any{"command": "printf ok"},
	)
	if result.IsError || result.Content != "ok" {
		t.Fatalf("result = %+v", result)
	}
}
