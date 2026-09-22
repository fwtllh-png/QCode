package shell

import (
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/platform/process"
)

func TestCommandExecutionTracksOriginThroughContinuation(t *testing.T) {
	for _, test := range []struct {
		name, command, want string
		close               bool
		timeoutMS           int
	}{
		{name: "completed", command: "IFS= read line; exit 0", want: "completed"},
		{name: "failed", command: "IFS= read line; exit 7", want: "failed"},
		{name: "closed", command: "IFS= read line", close: true, want: "canceled"},
		{name: "deadline", command: "IFS= read line", timeoutMS: 200, want: "timed_out"},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager := process.NewSessionManager(4096)
			t.Cleanup(manager.CloseAll)
			registry := tool.NewRegistry(nil, nil)
			if err := RegisterWithManagerAndBackend(registry, t.TempDir(), manager, passthroughBackend{}); err != nil {
				t.Fatal(err)
			}
			started := executeProcessTool(t, registry, processTestThread, "exec_command", map[string]any{
				"command": test.command, "yield_time_ms": 1, "timeout_ms": test.timeoutMS,
			})
			meta := started.Metadata["command_execution"].(map[string]any)
			id, _ := started.Metadata["session_id"].(string)
			origin := meta["call_id"]
			if id == "" || origin != "call-exec_command" || meta["status"] != "started" || meta["command"] != test.command {
				t.Fatalf("initial command event=%+v", meta)
			}
			if _, exists := meta["exit_code"]; exists {
				t.Fatalf("running command has an exit code: %+v", meta)
			}
			args := map[string]any{"session_id": id, "yield_time_ms": 1000}
			if test.close {
				args["close"] = true
			} else if test.timeoutMS == 0 {
				args["chars"] = "\n"
			}
			finished := executeProcessTool(t, registry, processTestThread, "write_stdin", args)
			for finished.Metadata["running"] == true {
				delete(args, "chars")
				finished = executeProcessTool(t, registry, processTestThread, "write_stdin", args)
			}
			meta = finished.Metadata["command_execution"].(map[string]any)
			if meta["status"] != test.want || meta["call_id"] != origin ||
				meta["session_id"] != id || meta["command"] != test.command {
				t.Fatalf("settled command event=%+v want=%s", meta, test.want)
			}
			if _, exists := meta["exit_code"]; !exists || manager.Count() != 0 {
				t.Fatalf("terminal exit or cleanup missing: %+v", meta)
			}
		})
	}
}
