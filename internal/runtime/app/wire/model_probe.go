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
	if apiKey != "" {
		capabilities, probeErr = modelcatalog.ProbeCapabilitiesForProtocol(
			ctx, baseURL, apiKey, modelID, protocol,
		)
	} else {
		capabilities, probeErr = modelcatalog.ProbeCapabilitiesWithCredentialForProtocol(
			ctx,
			baseURL,
			credential,
			modelID,
			protocol,
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
