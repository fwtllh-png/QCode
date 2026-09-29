package shell

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/result"
	"github.com/fwtllh-png/QCode/internal/platform/process"
	"github.com/fwtllh-png/QCode/testutil/tooltest"
)

func newGuardTestRegistry(t *testing.T) *tool.Registry {
	t.Helper()
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
	return registry
}

// TestWriteStdinRejectsEmptySessionIdWithRecoveryGuidance keeps the dead-poll
// loop from the model losing the session id after a session ended: the empty
// id is rejected with an explanation and a recovery path instead of reaching
// the session manager.
func TestWriteStdinRejectsEmptySessionIdWithRecoveryGuidance(t *testing.T) {
	registry := newGuardTestRegistry(t)
	for _, input := range []map[string]any{
		{"session_id": ""},
		{"session_id": "   "},
	} {
		data, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		_, executeErr := tooltest.Execute(
			t.Context(),
			registry,
			tool.Call{Name: "write_stdin", Arguments: data},
		)
		if executeErr == nil {
			t.Fatalf("empty session id accepted: %+v", input)
		}
		if !strings.Contains(executeErr.Error(), "session_id is required") ||
			!strings.Contains(executeErr.Error(), "turn_history") {
			t.Fatalf("empty session id error = %v", executeErr)
		}
	}
}

// TestWriteStdinUnknownSessionCarriesRecoveryHint verifies that polling a
// session id that is no longer live produces the structured recovery hint so
// the model retrieves durable output instead of retrying the same call.
func TestWriteStdinUnknownSessionCarriesRecoveryHint(t *testing.T) {
	registry := newGuardTestRegistry(t)
	data, err := json.Marshal(map[string]any{"session_id": "term_missing"})
	if err != nil {
		t.Fatal(err)
	}
	_, executeErr := tooltest.Execute(
		t.Context(),
		registry,
		tool.Call{Name: "write_stdin", Arguments: data},
	)
	if executeErr == nil || !errors.Is(executeErr, process.ErrSessionNotFound) {
		t.Fatalf("unknown session error = %v", executeErr)
	}
	content, recoverable := result.RecoverableFailure(sessionLookupHint(executeErr))
	if !recoverable ||
		!strings.Contains(content, "required_action=use_turn_history") ||
		!strings.Contains(content, "retry_original=false") {
		t.Fatalf("recovery content = %q, recoverable = %v", content, recoverable)
	}
}

func TestSessionLookupHintLeavesOtherErrorsAlone(t *testing.T) {
	other := errors.New("rows and cols must be supplied together")
	if sessionLookupHint(other) != other {
		t.Fatal("unrelated error was rewritten")
	}
	if sessionLookupHint(nil) != nil {
		t.Fatal("nil error was rewritten")
	}
}

func TestExecCommandDescriptorWarnsAboutDaemons(t *testing.T) {
	registry := newGuardTestRegistry(t)
	_, descriptor, _, err := registry.Resolve("exec_command")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(descriptor.Description, "server or daemon") ||
		!strings.Contains(descriptor.Description, "close the session") {
		t.Fatalf("exec_command description = %q", descriptor.Description)
	}
}
