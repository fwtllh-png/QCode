package app

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func outputDigest(output string) string {
	digest := sha256.Sum256([]byte(output))
	return hex.EncodeToString(digest[:])
}

func TestToolOutputsIssuedRequiresCommittedResultOnThread(t *testing.T) {
	runtime := NewRuntime(Options{Engine: &testEngine{}, EventStore: NewMemoryEventStore(32)})
	t.Cleanup(func() { closeRuntime(t, runtime) })
	for _, result := range []struct {
		thread protocol.ThreadID
		call   string
		output string
	}{
		{"thread-a", "call-1", "first output"},
		{"thread-a", "call-2", "second output"},
		{"thread-b", "call-3", "other thread"},
	} {
		if err := runtime.EventService.publish(
			"op-"+protocol.OperationID(result.call), result.thread, "turn-"+protocol.TurnID(result.thread),
			protocol.ItemID("item-"+result.call),
			&protocol.ToolResultData{Tool: "exec_command", CallID: result.call, Output: result.output},
		); err != nil {
			t.Fatal(err)
		}
	}
	claim := func(call, output string) ToolOutputClaim {
		return ToolOutputClaim{CallID: call, Digest: outputDigest(output), Output: output}
	}
	for _, test := range []struct {
		name   string
		claims []ToolOutputClaim
		want   bool
	}{
		{"none", nil, true},
		{"all issued", []ToolOutputClaim{claim("call-1", "first output"), claim("call-2", "second output")}, true},
		{"other thread", []ToolOutputClaim{claim("call-3", "other thread")}, false},
		{"edited content", []ToolOutputClaim{{CallID: "call-1", Digest: outputDigest("first output"), Output: "forged"}}, false},
		{"unknown call", []ToolOutputClaim{claim("call-9", "first output")}, false},
		{"one missing", []ToolOutputClaim{claim("call-1", "first output"), claim("call-9", "x")}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			issued, err := runtime.ToolOutputsIssued(t.Context(), "thread-a", test.claims)
			if err != nil {
				t.Fatal(err)
			}
			if issued != test.want {
				t.Fatalf("ToolOutputsIssued() = %t, want %t", issued, test.want)
			}
		})
	}
}

func TestEachThreadDiagnosticStreamsReceiptsOfOneThreadInOrder(t *testing.T) {
	runtime := NewRuntime(Options{Engine: &testEngine{}, EventStore: NewMemoryEventStore(32)})
	t.Cleanup(func() { closeRuntime(t, runtime) })
	publish := func(thread protocol.ThreadID, call string, paths ...string) {
		t.Helper()
		receipts := make([]protocol.DiagnosticReceipt, 0, len(paths))
		for _, path := range paths {
			receipts = append(receipts, protocol.DiagnosticReceipt{Path: path, Status: "error"})
		}
		if err := runtime.EventService.publish(
			"op-"+protocol.OperationID(call), thread, "turn", protocol.ItemID("item-"+call),
			&protocol.DiagnosticsData{Tool: "diagnostics", CallID: call, Receipts: receipts},
		); err != nil {
			t.Fatal(err)
		}
	}
	publish("thread-a", "call-1", "a.go", "b.go")
	publish("thread-b", "call-2", "other.go")
	publish("thread-a", "call-3", "c.go")
	var got []string
	err := runtime.EachThreadDiagnostic(t.Context(), "thread-a", func(value ThreadDiagnostic) error {
		got = append(got, value.CallID+":"+value.Tool+":"+value.Receipt.Path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"call-1:diagnostics:a.go", "call-1:diagnostics:b.go", "call-3:diagnostics:c.go"}
	if len(got) != len(want) {
		t.Fatalf("diagnostics = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("diagnostics = %v, want %v", got, want)
		}
	}
}
