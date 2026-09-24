package wire

import (
	"fmt"
	"sort"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func runtimeModelCatalog(
	selectedRoute model.ReadyRoute,
	selectedCapabilities protocol.ModelCapabilities,
	selectable map[string]model.ReadyRoute,
) (protocol.ProviderCatalog, protocol.ModelCatalog) {
	selectedProvider := selectedRoute.ProviderID()
	selectedModel := selectedRoute.Model().ID
	selectedSeen := false
	providerIDs := make(map[string]bool)
	modelEntries := make([]protocol.ModelCatalogEntry, 0, len(selectable)+1)
	// 已配置连接（SelectableRoutes 有路由）的模型逐个列为可热切换条目。
	keys := make([]string, 0, len(selectable))
	for key := range selectable {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		route := selectable[key]
		providerIDs[route.ProviderID()] = true
		capabilities := catalogModelCapabilities(route.Model())
		capabilities.SelectionMode = "hot"
		selected := route.ProviderID() == selectedProvider &&
			route.Model().ID == selectedModel
		modelEntries = append(modelEntries, protocol.ModelCatalogEntry{
			Provider: route.ProviderID(), ID: route.Model().ID,
			Source: "registered", Selected: selected,
			Capabilities: capabilities,
		})
		if selected {
			selectedSeen = true
		}
	}
	if !selectedSeen {
		modelEntries = append(modelEntries, protocol.ModelCatalogEntry{
			Provider: selectedProvider, ID: selectedModel,
			Source:   "connection_baseline",
			Selected: true, Capabilities: selectedCapabilities,
		})
	}
	providerIDs[selectedProvider] = true
	providerEntries := make([]protocol.ProviderCatalogEntry, 0, len(providerIDs))
	for id := range providerIDs {
		providerEntries = append(providerEntries, protocol.ProviderCatalogEntry{
			ID: id, DisplayName: id,
			Selected: id == selectedProvider, Availability: "available",
		})
	}
	sort.Slice(providerEntries, func(left, right int) bool {
		return providerEntries[left].ID < providerEntries[right].ID
	})
	return protocol.ProviderCatalog{
			Version: protocol.ModelCatalogVersion, Providers: providerEntries,
		}, protocol.ModelCatalog{
			Version: protocol.ModelCatalogVersion, Models: modelEntries,
		}
}

func catalogModelCapabilities(descriptor model.Model) protocol.ModelCapabilities {
	capabilities := descriptor.Capabilities
	result := protocol.ModelCapabilities{
		DisplayName:          descriptor.ID,
		ContextWindow:        descriptor.Limits.ContextTokens,
		MaxOutputTokens:      descriptor.Limits.MaxOutputTokens,
		Streaming:            capabilities.Streaming,
		Reasoning:            capabilities.Reasoning,
		ToolCalls:            capabilities.ToolCalls,
		ParallelToolCalls:    "unknown",
		NativeSearch:         capabilities.NativeSearch,
		IncrementalResponses: capabilities.IncrementalResponses,
		Vision:               capabilities.Vision,
		ImageInput:           capabilities.ImageInput,
		PromptCache:          capabilities.PromptCache,
		AutomaticPromptCache: capabilities.AutomaticPromptCache,
		ThinkingToggle:       capabilities.ThinkingToggle,
		ReasoningEfforts:     capabilities.ReasoningEffortLevels(),
		MetadataProvenance: protocol.ModelMetadataProvenance{
			CanonicalID:  string(descriptor.MetadataProvenance.CanonicalID),
			WireID:       string(descriptor.MetadataProvenance.WireID),
			Limits:       string(descriptor.MetadataProvenance.Limits),
			Capabilities: string(descriptor.MetadataProvenance.Capabilities),
			Pricing:      string(descriptor.MetadataProvenance.Pricing),
		},
		CredentialStatus: "unknown",
		Availability:     "available",
		SelectionMode:    "restart_required",
	}
	if result.Reasoning {
		result.DefaultReasoningEffort = capabilities.DefaultReasoningEffort
	}
	return result
}

func runtimeSelectableRoutes(
	selected model.ReadyRoute,
	additional map[string]model.Model,
) (map[string]model.ReadyRoute, error) {
	result := make(map[string]model.ReadyRoute)
	if len(additional) != 0 {
		result[model.RouteKey(selected.ProviderID(), selected.Model().ID)] = selected
	}
	for id, descriptor := range additional {
		if err := validateResolvedModelMetadata(descriptor); err != nil {
			return nil, fmt.Errorf("additional model %q: %w", id, err)
		}
		if descriptor.ID != id {
			return nil, fmt.Errorf("additional model %q has mismatched id %q", id, descriptor.ID)
		}
		result[model.RouteKey(selected.ProviderID(), id)] =
			selected.WithModel(descriptor)
	}
	return result, nil
}

func runtimeProfileModels(
	catalog protocol.ModelCatalog,
	providerID string,
	selectedCapabilities protocol.ModelCapabilities,
) (map[string]protocol.ModelCapabilities, []string) {
	profiles := make(map[string]protocol.ModelCapabilities)
	reasoningMutable := selectedCapabilities.Reasoning
	providerMutable := false
	for _, entry := range catalog.Models {
		if entry.Capabilities.Availability != "available" ||
			entry.Capabilities.SelectionMode != "hot" {
			continue
		}
		profiles[model.RouteKey(entry.Provider, entry.ID)] =
			entry.Capabilities
		reasoningMutable = reasoningMutable || entry.Capabilities.Reasoning
		providerMutable = providerMutable || entry.Provider != providerID
	}
	mutable := make([]string, 0, 3)
	if selectedCapabilities.SelectionMode != "fixed" {
		if providerMutable {
			mutable = append(mutable, "provider")
		}
		mutable = append(mutable, "model")
	}
	if reasoningMutable {
		mutable = append(mutable, "reasoning_effort")
	}
	return profiles, mutable
}
