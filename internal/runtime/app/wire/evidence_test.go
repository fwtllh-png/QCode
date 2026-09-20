package wire

import (
	"testing"

	"github.com/fwtllh-png/QCode/internal/config"
	promptcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/prompt"
)

func TestRepoContextCarriesEvidenceAndItsBudget(t *testing.T) {
	settings := config.Defaults().Context
	settings.RepoMap.Enabled, settings.WorkingSet.Enabled = false, false
	// Evidence alone is enough to need the tail: it describes the thread, not the
	// repository, so it works in a session with no index.
	if newRepoContext(nil, settings, nil, "") == nil {
		t.Fatal("evidence alone did not produce a tail provider")
	}

	settings.Evidence.Enabled = false
	if newRepoContext(nil, settings, nil, "") != nil {
		t.Fatal("all three sections off still produced a tail provider")
	}

	if budget := defaultPromptBudgets(128_000)[promptcontext.PartitionEvidence]; budget.MaxBytes == 0 {
		t.Fatal("the evidence partition has no default ceiling")
	}
	if budget := defaultPromptBudgets(128_000)[promptcontext.PartitionCodingPolicy]; budget.MaxBytes <
		len(promptcontext.NewCodingPolicySection().Render()) {
		t.Fatalf("the coding method does not fit its own ceiling: %+v", budget)
	}
}
