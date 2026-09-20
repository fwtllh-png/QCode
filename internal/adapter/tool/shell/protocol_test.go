package shell

import (
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/platform/process"
)

// The declared process deadline is a documented contract with one working-day
// ceiling: values beyond it are rejected rather than occupying a session
// indefinitely.
func TestProcessTimeoutCeiling(t *testing.T) {
	day := int64(24 * time.Hour / time.Millisecond)
	if _, err := processTimeout(day); err != nil {
		t.Fatalf("24h timeout rejected: %v", err)
	}
	if _, err := processTimeout(day + 1); err == nil ||
		!strings.Contains(err.Error(), "timeout exceeds") {
		t.Fatalf("over-ceiling timeout = %v, want ceiling error", err)
	}
	if _, err := processTimeout(-1); err == nil {
		t.Fatal("negative timeout accepted")
	}
}

// env entries reach the process and stay inside the child-process
// allow-list: secret-named and non-allow-listed variables are refused before
// anything starts.
func TestExecCommandEnvPassesAllowListedVariables(t *testing.T) {
	entries, err := environmentEntries(map[string]string{
		"GOPROXY": "off", "LANG": "C",
	})
	if err != nil || len(entries) != 2 ||
		entries[0] != "GOPROXY=off" || entries[1] != "LANG=C" {
		t.Fatalf("entries = %v err = %v", entries, err)
	}
	if entries, err = environmentEntries(nil); err != nil || entries != nil {
		t.Fatalf("empty entries = %v err = %v", entries, err)
	}
	for name := range map[string]bool{"MY_TOKEN": true, "SECRET_PATH": true, "CI_API_KEY": true} {
		if _, err = environmentEntries(map[string]string{name: "x"}); err != nil {
			t.Fatalf("secret name %s was rejected before sanitization: %v", name, err)
		}
	}
	if _, err = environmentEntries(map[string]string{"FOO=BAR": "x"}); err == nil ||
		!strings.Contains(err.Error(), "invalid") {
		t.Fatalf("malformed name accepted: %v", err)
	}
}

func TestExecCommandRunsCommandWithEnv(t *testing.T) {
	manager := process.NewSessionManager(4096)
	t.Cleanup(manager.CloseAll)
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
		"exec_command",
		map[string]any{
			"command": `printf '%s' "$GOPROXY"`,
			"env":     map[string]any{"GOPROXY": "off"},
		},
	)
	if result.IsError || result.Content != "off" {
		t.Fatalf("result = %+v", result)
	}
}
