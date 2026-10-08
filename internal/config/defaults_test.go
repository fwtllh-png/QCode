package config

import (
	"testing"
	"time"
)

func TestDefaultsUseExtendedTurnBudget(t *testing.T) {
	defaults := Defaults()
	if defaults.Execution.MaxSteps != 64 {
		t.Fatalf("default max steps = %d, want 64", defaults.Execution.MaxSteps)
	}
	if defaults.Execution.ImplementNoProgressSamples != 6 {
		t.Fatalf(
			"default implement no-progress samples = %d, want 6",
			defaults.Execution.ImplementNoProgressSamples,
		)
	}
	if defaults.Execution.Subagent.Delegation != SubagentDelegationAdaptive {
		t.Fatalf(
			"default subagent delegation = %q, want %q",
			defaults.Execution.Subagent.Delegation,
			SubagentDelegationAdaptive,
		)
	}
	if defaults.Context.Compact.TruthMaxBytes != 0 {
		t.Fatalf("default truth max bytes = %d, want automatic",
			defaults.Context.Compact.TruthMaxBytes)
	}
	if defaults.Context.View.NarrativeMode != "post_turn" ||
		defaults.Context.View.Digest != "ledger+narrative" ||
		defaults.Context.View.RecentTailTurns != 0 ||
		defaults.Context.View.KeepRecentToolResults != 0 ||
		defaults.Context.View.HistoryTokenCeiling != 0 ||
		defaults.Context.View.CheckpointMaxBytes != 0 {
		t.Fatalf("default view = %+v", defaults.Context.View)
	}
	if defaults.Context.Compact.SemanticNarrativeMaxInputTokens != 0 ||
		defaults.Context.Compact.SemanticNarrativeMaxItems != 0 ||
		defaults.Context.Compact.SemanticNarrativeItemMaxBytes != 0 ||
		defaults.Context.Compact.SemanticNarrativeMaxOutputTokens != 0 {
		t.Fatalf(
			"default semantic narrative max output tokens = %d, want automatic",
			defaults.Context.Compact.SemanticNarrativeMaxOutputTokens,
		)
	}
}

func TestDefaultExecutionTokenBudgetsAreDerivedAtRuntime(t *testing.T) {
	defaults := Defaults()
	if defaults.Execution.TurnBudgetTokens != 0 ||
		defaults.Execution.Subagent.MaxTokens != 0 {
		t.Fatalf("default execution budgets = %+v", defaults.Execution)
	}
	if defaults.Execution.BudgetTokens != 0 {
		t.Fatalf(
			"session budget=%d, want unlimited independently of turn budget",
			defaults.Execution.BudgetTokens,
		)
	}
}

func TestLSPDefaultsKeepResidentSessionsOff(t *testing.T) {
	defaults, err := Load(LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	lspConfig := defaults.Config.Context.LSP
	// A resident language server is a host process: nothing runs until a
	// configuration explicitly says so, and the bounds a session would use
	// are the documented defaults.
	if lspConfig.ResidentEnabled {
		t.Fatal("resident sessions must default to off")
	}
	if lspConfig.IdleTimeout != 10*time.Minute || lspConfig.MaxServers != 2 ||
		lspConfig.CacheCapacity != 256 {
		t.Fatalf("lsp defaults = %+v", lspConfig)
	}
}

func TestExecutionEnvironmentDefaultsAreV1Native(t *testing.T) {
	defaults := Defaults().Execution.Environment
	if defaults.Contract != EnvironmentContractV1 ||
		defaults.Profile != EnvironmentProfileNative ||
		defaults.SharedUserTemp ||
		defaults.Source != "" ||
		len(defaults.Resources) != 0 {
		t.Fatalf("default execution environment = %+v", defaults)
	}
}

func TestASessionWithoutRouteSlotsHasAnEmptyTable(t *testing.T) {
	snapshot, err := Load(LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if snapshot.Config.Route.Lock {
		t.Fatal("route lock defaults to on")
	}
	if len(snapshot.Config.Route.Slots) != 0 {
		t.Fatalf("slots = %+v, want none", snapshot.Config.Route.Slots)
	}
}
