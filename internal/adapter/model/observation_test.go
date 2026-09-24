package model_test

import (
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
)

func TestApplyProbeTightensWithoutTrustAndWidensOnlyWithTrust(t *testing.T) {
	base := model.Capabilities{
		Streaming: true, Vision: true, Reasoning: true,
		ReasoningEfforts:       []string{"off", "high"},
		DefaultReasoningEffort: "high",
		ThinkingToggle:         true,
		PromptCache:            true, AutomaticPromptCache: true,
	}
	observations := []model.CapabilityObservation{
		{ConnectionID: "connection-1", Capability: model.CapVision, Supported: false, Source: "probe"},
		{ConnectionID: "connection-1", Capability: model.CapReasoning, Supported: false, Source: "probe"},
		{ConnectionID: "connection-1", Capability: model.CapPromptCache, Supported: false, Source: "probe"},
	}

	tightened := model.ApplyProbe(base, observations, false)
	if tightened.Vision {
		t.Fatal("probe unsupported must clear vision")
	}
	if tightened.Reasoning || len(tightened.ReasoningEfforts) != 0 ||
		tightened.DefaultReasoningEffort != "" || tightened.ThinkingToggle {
		t.Fatalf("reasoning dependencies survived probe tightening: %+v", tightened)
	}
	if tightened.PromptCache || tightened.AutomaticPromptCache {
		t.Fatalf("prompt cache dependencies survived probe tightening: %+v", tightened)
	}
	if !tightened.Streaming {
		t.Fatal("unrelated bits must stay")
	}
}

func TestApplyProbeWidensOnlyWithTrust(t *testing.T) {
	observation := []model.CapabilityObservation{{
		ConnectionID: "connection-1",
		Capability:   model.CapReasoning,
		Supported:    true,
		Source:       "probe",
	}}
	if model.ApplyProbe(model.Capabilities{}, observation, false).Reasoning {
		t.Fatal("supported probe widened capabilities without trust")
	}
	if !model.ApplyProbe(model.Capabilities{}, observation, true).Reasoning {
		t.Fatal("trusted supported probe did not widen capabilities")
	}
}

func TestReadyRouteWithCapabilitiesDoesNotMutateOriginal(t *testing.T) {
	catalog, err := model.NewCatalog(model.Provider{
		ID: "openai", Adapter: model.AdapterOpenAI,
		Endpoint: "https://api.openai.com/v1", Protocol: model.ProtocolOpenAIChat,
		Models: map[string]model.Model{
			"gpt-4.1": {
				ID: "gpt-4.1", CanonicalID: "gpt-4.1", WireID: "gpt-4.1",
				Limits: model.Limits{ContextTokens: 1_047_576, MaxOutputTokens: 32_768},
				Capabilities: model.Capabilities{
					Streaming: true, ToolCalls: true, Vision: true,
				},
				Provenance: model.ProvenanceOperatorConfig,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := model.NewResolver(catalog)
	if err != nil {
		t.Fatal(err)
	}
	route, err := resolver.Resolve(model.RouteRequest{
		ProviderID: "openai", ModelID: "gpt-4.1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !route.Model().Capabilities.Vision {
		t.Fatal("fixture: gpt-4.1 should advertise vision")
	}
	updated := route.WithCapabilities(model.ApplyProbe(
		route.Model().Capabilities,
		[]model.CapabilityObservation{{ConnectionID: "connection-1", Capability: model.CapVision, Supported: false}},
		false,
	))
	if updated.Model().Capabilities.Vision {
		t.Fatal("updated route should have vision cleared")
	}
	if !route.Model().Capabilities.Vision {
		t.Fatal("original route must keep catalog capabilities")
	}
}
