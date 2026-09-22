package extension

import (
	"encoding/json"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestCommandExecutionContinuationPreservesOriginAndReplays(t *testing.T) {
	for _, status := range []string{"started", "completed", "failed", "canceled", "timed_out"} {
		t.Run(status, func(t *testing.T) {
			meta := map[string]any{
				"call_id": "call-start", "session_id": "term-session",
				"command": "test command", "status": status,
			}
			if status != "started" {
				meta["exit_code"] = 0
			}
			data, ok := commandExecutionFromResult("call-poll", &tool.Result{
				Metadata: map[string]any{"command_execution": meta},
			})
			if !ok || data.CallID != "call-start" || data.SessionID != "term-session" ||
				(status == "started") != (data.ExitCode == nil) {
				t.Fatalf("command projection=%+v", data)
			}
			event, err := protocol.NewEvent(protocol.EventMeta{
				Sequence: 1, OperationID: "op-test", ThreadID: "thread-test", TurnID: "turn-test", ItemID: "item-poll",
			}, data)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(event)
			if err != nil {
				t.Fatal(err)
			}
			var replay protocol.Event
			if err := json.Unmarshal(encoded, &replay); err != nil {
				t.Fatal(err)
			}
			if err := replay.Validate(); err != nil {
				t.Fatal(err)
			}
			command := replay.Data.(*protocol.CommandExecutionData)
			if command.CallID != data.CallID || command.Status != status {
				t.Fatalf("replay changed command identity: %+v", command)
			}
		})
	}
}
