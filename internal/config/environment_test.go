package config

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestLoadSubagentConfigurationFromEnvironment(t *testing.T) {
	snapshot, err := Load(LoadOptions{LookupEnv: envLookup(map[string]string{
		"QCODE_SUBAGENT_DELEGATION":   "disabled",
		"QCODE_SUBAGENT_MAX_DEPTH":    "7",
		"QCODE_SUBAGENT_MAX_PARALLEL": "3",
		"QCODE_SUBAGENT_MAX_RESIDENT": "5",
		"QCODE_SUBAGENT_MAX_TOTAL":    "9",
		"QCODE_SUBAGENT_MAX_STEPS":    "31",
		"QCODE_SUBAGENT_MAX_TOKENS":   "12000",
		"QCODE_SUBAGENT_MAX_COST_USD": "2.5",
		"QCODE_SUBAGENT_WALL_TIME":    "90s",
		"QCODE_SUBAGENT_WORKSPACE":    "read_only",
	})})
	if err != nil {
		t.Fatal(err)
	}
	got := snapshot.Config.Execution.Subagent
	if got.Delegation != SubagentDelegationDisabled ||
		got.MaxDepth != 7 || got.MaxParallel != 3 ||
		got.MaxResident != 5 || got.MaxTotal != 9 ||
		got.MaxSteps != 31 || got.MaxTokens != 12000 ||
		got.MaxCostUSD != 2.5 || got.WallTime != 90*time.Second ||
		got.Workspace != SubagentWorkspaceReadOnly {
		t.Fatalf("subagent environment config = %+v", got)
	}
	for _, field := range []string{
		fieldSubagentDelegation, fieldSubagentMaxDepth,
		fieldSubagentMaxParallel, fieldSubagentMaxResident,
		fieldSubagentMaxTotal, fieldSubagentMaxSteps,
		fieldSubagentMaxTokens, fieldSubagentMaxCostUSD,
		fieldSubagentWallTime, fieldSubagentWorkspace,
	} {
		if source := snapshot.Provenance[field]; source != SourceEnv {
			t.Fatalf("provenance[%s] = %q", field, source)
		}
	}
}

func TestSecretReferenceDoesNotResolveOrLeakValue(t *testing.T) {
	const secret = "credential-value-that-must-not-leak"
	snapshot, err := Load(LoadOptions{
		LookupEnv: envLookup(map[string]string{
			"QCODE_CREDENTIAL_KIND": "env",
			"QCODE_CREDENTIAL_NAME": "PROVIDER_API_KEY",
			"PROVIDER_API_KEY":      secret,
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), secret) {
		t.Fatalf("snapshot contains secret value: %s", data)
	}
	if !strings.Contains(string(data), "PROVIDER_API_KEY") {
		t.Fatalf("snapshot does not contain reference name: %s", data)
	}
}

func envLookup(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, exists := values[name]
		return value, exists
	}
}
