package engine

import (
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	providerfixture "github.com/fwtllh-png/QCode/internal/adapter/provider/fixture"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/observability/verify"
)

func TestWorkspaceReconciliationPreservesDurableChangesWhenProjectionIsLost(t *testing.T) {
	registry := tool.NewRegistry(nil, nil)
	if err := registry.Register(&declarationWriteTool{}); err != nil {
		t.Fatal(err)
	}
	runtime := &scriptedProvider{streams: []provider.Stream{
		toolCallStream("write", "write_fixture", `{}`),
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventTextDelta, Text: "done"}, {Type: provider.EventMessageStop},
		}},
	}}
	engine := newEngine(t, runtime, registry)
	verifier := &scriptedVerifier{receipts: []verify.Receipt{passedReceipt()}}
	engine.options.Verify = VerifyOptions{Mode: VerifyModeSoft, Scope: verify.ScopeAffected, Runner: verifier}
	result, err := engine.RunForTurn(t.Context(), "lost-projection", "edit", func(event Event) error {
		if event.Result != nil {
			// Restart loses this projection, while the closed tool result remains
			// in the kernel. Absence of a journal must not erase that observation.
			engine.currentScope().state.diff.Reset()
		}
		return nil
	})
	// Verification can complete, but the missing journal must still prevent
	// terminal settlement. Rebuilding the diff cannot bypass that safety check.
	if err == nil || result.State != AwaitingRecovery {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	if len(verifier.requests) != 1 || len(verifier.requests[0].Paths) != 1 || verifier.requests[0].Paths[0] != "a.go" {
		t.Fatalf("lost verification coverage: %+v", verifier.requests)
	}
	if diff := engine.TurnDiff(); len(diff) != 1 || diff[0].Path != "a.go" {
		t.Fatalf("lost turn diff: %+v", diff)
	}
}
