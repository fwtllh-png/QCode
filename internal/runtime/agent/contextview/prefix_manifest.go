package contextview

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type PrefixItem struct {
	ID     string
	Kind   agentcontext.MessageKind
	Tokens uint64
}

type PrefixManifest struct {
	RouteDigest, PropertyDigest, ContextDigest string
	ToolDefinitionDigest                       string
	Items                                      []PrefixItem
}

type PrefixComparison struct {
	Compared, Monotonic                     bool
	CommonItems                             int
	CommonTokens                            uint64
	FirstDivergenceIndex                    int
	FirstDivergenceKind, StablePrefixDigest string
}

func PrefixRequestIdentity(
	route model.ReadyRoute,
	maxOutputTokens uint64,
	reasoningEffort string,
	nativeSearch bool,
) (string, string) {
	descriptor, err := route.Identity()
	routeDigest := ""
	if err == nil {
		encoded, _ := json.Marshal(descriptor)
		routeDigest = prefixDigest(encoded)
	}
	properties, _ := json.Marshal(struct {
		MaxOutputTokens uint64 `json:"max_output_tokens"`
		ReasoningEffort string `json:"reasoning_effort,omitempty"`
		NativeSearch    bool   `json:"native_search,omitempty"`
	}{
		MaxOutputTokens: maxOutputTokens,
		ReasoningEffort: reasoningEffort,
		NativeSearch:    nativeSearch,
	})
	return routeDigest, prefixDigest(properties)
}

func BuildPrefixManifest(
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

// BuildPrefixManifestFromMeasurement builds the manifest from a measurement
// the same snapshot already produced, reusing its context digest and per-item
// token estimates instead of re-estimating the whole context.
func BuildPrefixManifestFromMeasurement(
	snapshot agentcontext.MessageSnapshot,
	measurement agentcontext.Measurement,
	routeDigest string,
	propertyDigest string,
) (PrefixManifest, error) {
	refs := snapshot.ItemRefs()
	if len(refs) != len(measurement.ItemTokens) {
		return PrefixManifest{}, fmt.Errorf(
			"prefix measurement covers %d of %d snapshot items",
			len(measurement.ItemTokens), len(refs),
		)
	}
	manifest := PrefixManifest{
		RouteDigest: routeDigest, PropertyDigest: propertyDigest,
		ContextDigest: measurement.Data.ContextDigest,
		Items:         make([]PrefixItem, 0, len(refs)),
	}
	for index, ref := range refs {
		manifest.Items = append(manifest.Items, PrefixItem{
			ref.ID, ref.Kind, measurement.ItemTokens[index],
		})
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

func ComparePrefix(previous, current PrefixManifest) PrefixComparison {
	if previous.ContextDigest == "" {
		return PrefixComparison{}
	}
	result := PrefixComparison{Compared: true}
	switch {
	case previous.RouteDigest != current.RouteDigest:
		result.FirstDivergenceKind = "route"
		return result
	case previous.PropertyDigest != current.PropertyDigest:
		result.FirstDivergenceKind = "request_properties"
		return result
	case previous.ToolDefinitionDigest != current.ToolDefinitionDigest:
		result.FirstDivergenceKind = "tool_definitions"
		return result
	}
	limit := min(len(previous.Items), len(current.Items))
	for result.CommonItems < limit &&
		previous.Items[result.CommonItems].ID == current.Items[result.CommonItems].ID {
		result.CommonTokens += current.Items[result.CommonItems].Tokens
		result.CommonItems++
	}
	result.FirstDivergenceIndex = result.CommonItems
	result.Monotonic = result.CommonItems == len(previous.Items)
	switch {
	case result.CommonItems < len(current.Items):
		result.FirstDivergenceKind = string(current.Items[result.CommonItems].Kind)
	case result.CommonItems < len(previous.Items):
		result.FirstDivergenceKind = "removed"
	}
	if result.CommonItems != 0 {
		ids := make([]string, result.CommonItems)
		for index := range ids {
			ids[index] = current.Items[index].ID
		}
		encoded, _ := json.Marshal(ids)
		result.StablePrefixDigest = prefixDigest(encoded)
	}
	return result
}

func ApplyPrefixAttribution(
	data *protocol.SampleContextData,
	previous, current PrefixManifest,
) {
	comparison := ComparePrefix(previous, current)
	data.PrefixCompared, data.PrefixMonotonic = comparison.Compared, comparison.Monotonic
	data.PrefixCommonItems, data.PrefixCommonTokens = comparison.CommonItems, comparison.CommonTokens
	data.PrefixFirstDivergence = comparison.FirstDivergenceIndex
	data.PrefixDivergenceKind = comparison.FirstDivergenceKind
	data.StablePrefixDigest = comparison.StablePrefixDigest
	data.PreviousContextDigest = previous.ContextDigest
	data.RouteDigest = current.RouteDigest
	data.RequestPropertyDigest = current.PropertyDigest
	data.ToolDefinitionDigest = current.ToolDefinitionDigest
}

func ApplyEconomicAttribution(
	data *protocol.SampleContextData,
	admission EconomicAdmission,
) {
	data.EconomicInputTokens, data.EconomicBudgeted = admission.AllowedInput, admission.Budgeted
	data.EconomicHardInputTokens, data.EconomicOperatorTokens = admission.HardInput, admission.OperatorInput
	data.EconomicBudgetMode = "unbounded"
	if admission.Budgeted {
		data.EconomicBudgetMode = "explicit"
	}
	data.EconomicBudgetScope = admission.Scope
	data.EconomicBudgetUsed, data.EconomicBudgetLimit = admission.Used, admission.Limit
	data.EconomicBudgetRemaining = admission.Remaining
	data.FinalizationOutputReserve = admission.FinalizationOutput
	data.EconomicGrantedTokens = admission.AllowedInput
	data.EconomicReason = admission.Reason
	data.EconomicSource = admission.Source
	data.EconomicProvenance = admission.Provenance
}

func prefixDigest(value []byte) string {
	sum := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(sum[:])
}
