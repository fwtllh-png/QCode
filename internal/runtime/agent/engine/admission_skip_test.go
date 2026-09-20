package engine

import (
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
)

// Results an earlier step already admitted (inline receipts matching their
// encoded content) skip the re-encoding round trip: the second pass returns
// byte-identical blocks. Stale receipts still re-admit honestly.
func TestAdmittedInlineResultsSkipReencoding(t *testing.T) {
	engine := newEngine(
		t,
		&scriptedProvider{},
		tool.NewRegistry(nil, tool.NewResultStore(32<<10)),
	)
	history := []provider.Message{
		toolCallMessage(1, "call-1", "exec_command", `{}`),
		toolResultMessage(1, "call-1", "compact result payload"),
		toolCallMessage(1, "call-2", "exec_command", `{}`),
		toolResultMessage(1, "call-2", "second compact payload"),
	}
	admitted, err := engine.admitToolResultHistory(history)
	if err != nil {
		t.Fatal(err)
	}
	for _, index := range []int{1, 3} {
		receipt := admitted[index].Blocks[0].ToolResult.Admission
		if receipt == nil || receipt.Truncated {
			t.Fatalf(
				"fixture must produce inline receipts, got %+v at %d",
				receipt, index,
			)
		}
	}

	again, err := engine.admitToolResultHistory(admitted)
	if err != nil {
		t.Fatal(err)
	}
	for _, index := range []int{1, 3} {
		before := admitted[index].Blocks[0].ToolResult
		after := again[index].Blocks[0].ToolResult
		if before.Content != after.Content ||
			before.Admission.Digest != after.Admission.Digest ||
			before.Admission.TokenLimit != after.Admission.TokenLimit ||
			after.Admission.Handle != before.Admission.Handle {
			t.Fatalf(
				"re-admission changed an already admitted block at %d:\nbefore %+v\nafter  %+v",
				index, before, after,
			)
		}
	}

	// A receipt whose content drifted no longer matches and must re-admit.
	stale := cloneMessages(admitted)
	stale[1].Blocks[0].ToolResult.Content += " drifted"
	fixed, err := engine.admitToolResultHistory(stale)
	if err != nil {
		t.Fatal(err)
	}
	block := fixed[1].Blocks[0].ToolResult
	if block.Admission == nil ||
		block.Admission.Digest == admitted[1].Blocks[0].ToolResult.Admission.Digest {
		t.Fatalf("stale receipt was not re-admitted: %+v", block.Admission)
	}
}
