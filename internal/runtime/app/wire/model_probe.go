package wire

import (
	"context"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider/modelcatalog"
)

type DiscoveredModel = modelcatalog.DiscoveredModel

type ModelProbeResult struct {
	Models       []DiscoveredModel
	Capabilities model.Capabilities
	Warning      string
}

func ProbeModelConnection(
	ctx context.Context,
	providerID, baseURL, modelID, apiKey string,
	credential model.CredentialRef,
	protocol model.WireProtocol,
) (ModelProbeResult, error) {
	var (
		listed       map[string]any
		capabilities model.Capabilities
		listErr      error
		probeErr     error
	)
	if apiKey != "" {
		listed, listErr = modelcatalog.Discover(
			ctx,
			providerID,
			baseURL,
			apiKey,
		)
	} else {
		listed, listErr = modelcatalog.List(
			ctx,
			providerID,
			baseURL,
			credential,
		)
	}
	discovered, _ := listed["model_metadata"].([]modelcatalog.DiscoveredModel)
	var maxOutputTokens uint64
	for _, value := range discovered {
		if value.ID == modelID {
			maxOutputTokens = value.MaxOutputTokens
		}
	}
	if maxOutputTokens == 0 {
		if catalogProvider, found := model.DefaultCatalog().Provider(providerID); found {
			maxOutputTokens = catalogProvider.Models[modelID].Limits.MaxOutputTokens
		}
	}
	if apiKey != "" {
		capabilities, probeErr = modelcatalog.ProbeCapabilitiesForProtocol(
			ctx, baseURL, apiKey, modelID, protocol, maxOutputTokens,
		)
	} else {
		capabilities, probeErr = modelcatalog.ProbeCapabilitiesWithCredentialForProtocol(
			ctx,
			baseURL,
			credential,
			modelID,
			protocol,
			maxOutputTokens,
		)
	}
	if probeErr != nil {
		return ModelProbeResult{}, probeErr
	}
	capabilities = WithDefaultReasoningEfforts(modelID, capabilities)
	result := ModelProbeResult{Capabilities: capabilities}
	result.Models, _ = listed["model_metadata"].([]modelcatalog.DiscoveredModel)
	if listErr != nil {
		result.Warning = listErr.Error()
	}
	return result, nil
}

func WithDefaultReasoningEfforts(
	modelID string,
	capabilities model.Capabilities,
) model.Capabilities {
	if !capabilities.Reasoning || len(capabilities.ReasoningEfforts) != 0 {
		return capabilities
	}
	var matched *model.Model
	for _, provider := range model.DefaultCatalog().Providers() {
		candidate, exists := provider.Models[modelID]
		if !exists || !candidate.Capabilities.Reasoning ||
			len(candidate.Capabilities.ReasoningEfforts) == 0 {
			continue
		}
		if matched != nil {
			matched = nil
			break
		}
		value := candidate
		matched = &value
	}
	if matched != nil {
		capabilities.ReasoningEfforts = append(
			[]string(nil),
			matched.Capabilities.ReasoningEfforts...,
		)
		capabilities.DefaultReasoningEffort =
			matched.Capabilities.DefaultReasoningEffort
		capabilities.ThinkingToggle =
			matched.Capabilities.ThinkingToggle
		return capabilities
	}
	capabilities.ReasoningEfforts = []string{
		"low",
		"medium",
		"high",
		"xhigh",
		"max",
	}
	capabilities.DefaultReasoningEffort = "medium"
	return capabilities
}
