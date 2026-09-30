package shell

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/common/environment"
	"github.com/fwtllh-png/QCode/internal/platform/process"
)

func TestPreparationFactsDoNotClassifyUnrelatedProcessResults(t *testing.T) {
	manager := process.NewSessionManager(4096)
	t.Cleanup(manager.CloseAll)
	registry := tool.NewRegistry(nil, nil)
	t.Cleanup(func() { _ = registry.Close() })
	if err := RegisterWithManagerAndBackend(registry, t.TempDir(), manager, passthroughBackend{}); err != nil {
		t.Fatal(err)
	}
	facts := []environment.Fact{{Source: environment.SourcePreparer,
		Category:       environment.CategoryEnvironmentResourceUnavailable,
		RequiredAction: environment.ActionBindCredential, Resource: "unused-artifact-auth",
		Detail: "credential_binder_unavailable",
	}}
	ctx := environment.WithPreparationFacts(t.Context(), facts)
	for _, command := range []string{"true", "printf '401 Unauthorized\\n'; exit 1"} {
		result := executeProcessToolContext(t, ctx, registry, processTestThread, "exec_command", map[string]any{
			"command": command,
		})
		got, _ := result.Metadata["environment_preparation_facts"].([]environment.Fact)
		if !slices.Equal(got, facts) {
			t.Fatalf("preparation facts missing: %+v", result)
		}
		if result.Metadata["required_action"] != nil || result.Metadata["environment_detail"] != nil {
			t.Fatalf("unrelated preparation fact classified the command: %+v", result)
		}
		if command == "true" {
			if result.IsError {
				t.Fatalf("successful command: %+v", result)
			}
		} else if !result.IsError || result.Metadata["error_category"] != environment.CategoryUnknown {
			t.Fatalf("unrelated failure must stay unknown: %+v", result)
		}
	}
	started := executeProcessToolContext(t, ctx, registry, processTestThread, "exec_command", map[string]any{
		"command": "read value; exit 1", "tty": true, "yield_time_ms": 1,
	})
	id, _ := started.Metadata["session_id"].(string)
	if id == "" {
		t.Fatalf("expected live PTY: %+v", started)
	}
	finished := executeProcessToolContext(t, ctx, registry, processTestThread, "write_stdin", map[string]any{
		"session_id": id, "chars": "done\n", "yield_time_ms": 1000,
	})
	pollCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for finished.Metadata["running"] == true {
		finished = executeProcessToolContext(t, pollCtx, registry, processTestThread, "write_stdin", map[string]any{
			"session_id": id, "yield_time_ms": 1000,
		})
	}
	got, _ := finished.Metadata["environment_preparation_facts"].([]environment.Fact)
	if !finished.IsError || finished.Metadata["error_category"] != environment.CategoryUnknown || !slices.Equal(got, facts) {
		t.Fatalf("PTY completion mixed preparation and execution facts: %+v", finished)
	}
}
