package contextview

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
)

// The manifest built from a measurement's reused estimates must be identical
// to the one built by re-estimating, including under a non-additive estimator
// where per-call rounding could diverge.
func TestBuildPrefixManifestFromMeasurementMatchesEstimation(t *testing.T) {
	estimate := agentcontext.EstimatorFunc(
		func(messages []provider.Message) (uint64, error) {
			var base uint64
			for _, message := range messages {
				for _, block := range message.Blocks {
					base += uint64(len(block.Text))
				}
			}
			return uint64(max(1, int64(math.Ceil(float64(base)*1.5)))), nil
		},
	)
	snapshot := agentcontext.NewMessageLedger(agentcontext.LedgerInput{
		Stable: []provider.Message{
			provider.TextMessage(provider.RoleSystem, "stable"),
		},
		History: []provider.Message{
			provider.TextMessage(provider.RoleUser, "question"),
			provider.TextMessage(provider.RoleAssistant, "answer"),
			provider.TextMessage(provider.RoleTool, "result"),
		},
		Dynamic: []provider.Message{
			provider.TextMessage(provider.RoleSystem, "dynamic"),
		},
		Definitions: []provider.ToolDefinition{{
			Name: "read", Description: "read a file",
		}},
	}).Snapshot()

	reestimated, err := reestimatedPrefixForTest(
		snapshot, estimate, "route-digest", "property-digest",
	)
	if err != nil {
		t.Fatal(err)
	}
	measurement, err := snapshot.MeasureDetailed("reason", "effort", estimate)
	if err != nil {
		t.Fatal(err)
	}
	reused, err := BuildPrefixManifestFromMeasurement(
		snapshot, measurement, "route-digest", "property-digest",
	)
	if err != nil {
		t.Fatal(err)
	}
	if reestimated.ContextDigest != reused.ContextDigest ||
		reestimated.ToolDefinitionDigest != reused.ToolDefinitionDigest ||
		len(reestimated.Items) != len(reused.Items) {
		t.Fatalf(
			"manifests diverged:\nreestimated %+v\nreused %+v",
			reestimated, reused,
		)
	}
	for index := range reestimated.Items {
		if reestimated.Items[index] != reused.Items[index] {
			t.Fatalf(
				"item %d diverged: %+v vs %+v",
				index, reestimated.Items[index], reused.Items[index],
			)
		}
	}

	misaligned := measurement
	misaligned.ItemTokens = misaligned.ItemTokens[:len(misaligned.ItemTokens)-1]
	if _, err := BuildPrefixManifestFromMeasurement(
		snapshot, misaligned, "route-digest", "property-digest",
	); err == nil {
		t.Fatal("misaligned measurement must be rejected")
	}
}

// Independent re-estimation is retained only as an equivalence-test oracle.
func reestimatedPrefixForTest(
	snapshot agentcontext.MessageSnapshot,
	estimate agentcontext.Estimator,
	routeDigest string,
	propertyDigest string,
) (PrefixManifest, error) {
	contextDigest, err := snapshot.Digest()
	if err != nil {
		return PrefixManifest{}, err
	}
	items := snapshot.Items()
	manifest := PrefixManifest{
		RouteDigest: routeDigest, PropertyDigest: propertyDigest,
		ContextDigest: contextDigest, Items: make([]PrefixItem, 0, len(items)),
	}
	for _, item := range items {
		tokens, estimateErr := estimate.Estimate([]provider.Message{item.Message})
		if estimateErr != nil {
			return PrefixManifest{}, estimateErr
		}
		manifest.Items = append(manifest.Items, PrefixItem{item.ID, item.Kind, tokens})
	}
	if definitions := snapshot.Definitions(); len(definitions) != 0 {
		encoded, encodeErr := json.Marshal(definitions)
		if encodeErr != nil {
			return PrefixManifest{}, encodeErr
		}
		manifest.ToolDefinitionDigest = prefixDigest(encoded)
	}
	return manifest, nil
}
