package config

import (
	"testing"
)

func TestSubagentResidentAndTotalOverridesWin(t *testing.T) {
	resident, total := 11, 13
	snapshot, err := Load(LoadOptions{Overrides: Overrides{
		SubagentMaxResident: &resident,
		SubagentMaxTotal:    &total,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshot.Config.Execution.Subagent; got.MaxResident != resident ||
		got.MaxTotal != total {
		t.Fatalf("subagent overrides = %+v", got)
	}
	if snapshot.Provenance[fieldSubagentMaxResident] != SourceStartup ||
		snapshot.Provenance[fieldSubagentMaxTotal] != SourceStartup {
		t.Fatalf("subagent override provenance = %+v", snapshot.Provenance)
	}
}

func TestLoadPreservesAbsentAndExplicitFalseZeroOverrides(t *testing.T) {
	path := writeConfig(t, `
[execution]
tools = true
native_search = true
budget_tokens = 700
budget_usd = 3.5
`)
	fromFile, err := Load(LoadOptions{
		Path: path, LookupEnv: envLookup(nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !fromFile.Config.Execution.Tools ||
		!fromFile.Config.Execution.NativeSearch ||
		fromFile.Config.Execution.BudgetTokens != 700 ||
		fromFile.Config.Execution.BudgetUSD != 3.5 {
		t.Fatalf("absent startup flags did not preserve file values: %+v", fromFile.Config.Execution)
	}

	disabled := false
	zeroTokens := uint64(0)
	zeroUSD := float64(0)
	overridden, err := Load(LoadOptions{
		Path: path,
		LookupEnv: envLookup(map[string]string{
			"QCODE_TOOLS":         "true",
			"QCODE_NATIVE_SEARCH": "true",
			"QCODE_BUDGET_TOKENS": "900",
			"QCODE_BUDGET_USD":    "4.5",
		}),
		Overrides: Overrides{
			Tools: &disabled, NativeSearch: &disabled,
			BudgetTokens: &zeroTokens, BudgetUSD: &zeroUSD,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if overridden.Config.Execution.Tools ||
		overridden.Config.Execution.NativeSearch ||
		overridden.Config.Execution.BudgetTokens != 0 ||
		overridden.Config.Execution.BudgetUSD != 0 {
		t.Fatalf("explicit false/zero overrides were lost: %+v", overridden.Config.Execution)
	}
	for _, field := range []string{fieldTools, fieldNativeSearch, fieldBudgetTokens, fieldBudgetUSD} {
		if overridden.Provenance[field] != SourceStartup {
			t.Fatalf("provenance[%q] = %q, want startup", field, overridden.Provenance[field])
		}
	}
}

func TestLockRouteOverrideBeatsTheFile(t *testing.T) {
	path := writeConfig(t, "[route]\nlock = false\n")
	locked := true

	snapshot, err := Load(LoadOptions{Path: path, Overrides: Overrides{RouteLock: &locked}})
	if err != nil {
		t.Fatal(err)
	}

	if !snapshot.Config.Route.Lock {
		t.Fatal("--lock-route did not win over the file")
	}
	if snapshot.Provenance[fieldRouteLock] != SourceStartup {
		t.Fatalf("provenance = %q", snapshot.Provenance[fieldRouteLock])
	}
}
