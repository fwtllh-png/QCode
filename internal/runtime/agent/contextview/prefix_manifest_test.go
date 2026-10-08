package contextview

import (
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
)

func TestPrefixManifestReportsAppendOnlyAndEarlyDivergence(t *testing.T) {
	estimator := agentcontext.EstimatorFunc(func(messages []provider.Message) (uint64, error) {
		return agentcontext.EstimateMessageTokens(messages), nil
	})
	first := agentcontext.NewMessageLedger(agentcontext.LedgerInput{History: []provider.Message{
		provider.TextMessage(provider.RoleUser, "one"),
	}}).Snapshot()
	second := first.WithHistory([]provider.Message{
		provider.TextMessage(provider.RoleUser, "one"),
		provider.TextMessage(provider.RoleAssistant, "two"),
	})
	firstManifest, err := measuredPrefixForTest(first, estimator, "route", "properties")
	if err != nil {
		t.Fatal(err)
	}
	secondManifest, err := measuredPrefixForTest(second, estimator, "route", "properties")
	if err != nil {
		t.Fatal(err)
	}
	comparison := ComparePrefix(firstManifest, secondManifest)
	if !comparison.Compared || !comparison.Monotonic ||
		comparison.CommonItems != 1 || comparison.CommonTokens == 0 ||
		comparison.StablePrefixDigest == "" {
		t.Fatalf("append comparison = %+v", comparison)
	}
	rewritten, err := measuredPrefixForTest(
		first.WithHistory([]provider.Message{
			provider.TextMessage(provider.RoleUser, "changed"),
		}),
		estimator,
		"route",
		"properties",
	)
	if err != nil {
		t.Fatal(err)
	}
	comparison = ComparePrefix(firstManifest, rewritten)
	if comparison.Monotonic || comparison.CommonItems != 0 ||
		comparison.FirstDivergenceKind != string(agentcontext.KindHistory) {
		t.Fatalf("rewrite comparison = %+v", comparison)
	}
}

func TestPrefixManifestTreatsRequestIdentityAsPrefix(t *testing.T) {
	estimator := agentcontext.EstimatorFunc(func(messages []provider.Message) (uint64, error) {
		return agentcontext.EstimateMessageTokens(messages), nil
	})
	snapshot := agentcontext.NewMessageLedger(agentcontext.LedgerInput{
		History: []provider.Message{provider.TextMessage(provider.RoleUser, "one")},
	}).Snapshot()
	first, err := measuredPrefixForTest(snapshot, estimator, "route-a", "properties-a")
	if err != nil {
		t.Fatal(err)
	}
	for _, testCase := range []struct {
		name, route, properties, wantKind string
	}{
		{"route", "route-b", "properties-a", "route"},
		{"properties", "route-a", "properties-b", "request_properties"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			current, buildErr := measuredPrefixForTest(
				snapshot, estimator, testCase.route, testCase.properties,
			)
			if buildErr != nil {
				t.Fatal(buildErr)
			}
			comparison := ComparePrefix(first, current)
			if comparison.Monotonic || comparison.CommonItems != 0 ||
				comparison.CommonTokens != 0 ||
				comparison.FirstDivergenceKind != testCase.wantKind {
				t.Fatalf("comparison = %+v", comparison)
			}
		})
	}
}

func TestPrefixManifestTreatsToolDefinitionsAsPrefix(t *testing.T) {
	estimator := agentcontext.EstimatorFunc(func(messages []provider.Message) (uint64, error) {
		return agentcontext.EstimateMessageTokens(messages), nil
	})
	history := []provider.Message{provider.TextMessage(provider.RoleUser, "one")}
	first, err := measuredPrefixForTest(
		agentcontext.NewMessageLedger(agentcontext.LedgerInput{
			History:     history,
			Definitions: []provider.ToolDefinition{{Name: "first"}},
		}).Snapshot(),
		estimator, "route", "properties",
	)
	if err != nil {
		t.Fatal(err)
	}
	second, err := measuredPrefixForTest(
		agentcontext.NewMessageLedger(agentcontext.LedgerInput{
			History:     history,
			Definitions: []provider.ToolDefinition{{Name: "second"}},
		}).Snapshot(),
		estimator, "route", "properties",
	)
	if err != nil {
		t.Fatal(err)
	}
	comparison := ComparePrefix(first, second)
	if comparison.Monotonic || comparison.CommonItems != 0 ||
		comparison.CommonTokens != 0 ||
		comparison.FirstDivergenceKind != "tool_definitions" {
		t.Fatalf("comparison = %+v", comparison)
	}
}

func measuredPrefixForTest(snapshot agentcontext.MessageSnapshot, estimate agentcontext.Estimator, routeDigest, propertyDigest string) (PrefixManifest, error) {
	measurement, err := snapshot.MeasureDetailed("test", "", estimate)
	if err != nil {
		return PrefixManifest{}, err
	}
	return BuildPrefixManifestFromMeasurement(snapshot, measurement, routeDigest, propertyDigest)
}
