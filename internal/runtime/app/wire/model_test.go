package wire

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
)

func TestResolveModelMetadataFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model.json")
	data := []byte(`{
		"canonical_id":"configured-model",
		"wire_id":"wire-model",
		"context_tokens":32768,
		"max_output_tokens":4096,
		"capabilities":{"streaming":true,"reasoning":true,"tool_calls":false,"native_search":false},
		"pricing":{"input_per_million":1.5,"output_per_million":4.5,"currency":"USD"}
	}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	descriptor, err := resolveModelMetadata(
		"custom",
		ModelMetadataOptions{Path: path},
	)
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.Limits.ContextTokens != 32768 ||
		descriptor.MetadataProvenance.Limits != model.ProvenanceOperatorConfig ||
		descriptor.MetadataProvenance.Capabilities != model.ProvenanceOperatorConfig ||
		descriptor.MetadataProvenance.Pricing != model.ProvenanceOperatorConfig ||
		!descriptor.Pricing.Known {
		t.Fatalf("descriptor = %+v", descriptor)
	}
}

func TestResolveStructuredModelMetadataPreservesCapabilitiesAndProvenance(t *testing.T) {
	descriptor, err := resolveModelMetadata("custom", ModelMetadataOptions{
		Descriptor: &model.Model{
			ID: "custom", CanonicalID: "vendor/custom", WireID: "custom",
			Limits: model.Limits{
				ContextTokens: 200_000, MaxOutputTokens: 24_000,
			},
			Capabilities: model.Capabilities{
				Streaming: true, Reasoning: true, ToolCalls: true,
				PromptCache:            true,
				ReasoningEfforts:       []string{"off", "high"},
				DefaultReasoningEffort: "high",
			},
			MetadataProvenance: model.MetadataProvenance{
				CanonicalID:  model.ProvenanceOperatorConfig,
				WireID:       model.ProvenanceOperatorConfig,
				Limits:       model.ProvenanceOperatorConfig,
				Capabilities: model.ProvenanceOperatorConfig,
				Pricing:      model.ProvenanceOperatorConfig,
			},
			Provenance: model.ProvenanceOperatorConfig,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.Limits.ContextTokens != 200_000 ||
		descriptor.Limits.MaxOutputTokens != 24_000 ||
		!descriptor.Capabilities.Reasoning ||
		descriptor.Capabilities.DefaultReasoningEffort != "high" ||
		descriptor.MetadataProvenance.Limits != model.ProvenanceOperatorConfig {
		t.Fatalf("descriptor = %+v", descriptor)
	}
}

func TestStructuredModelMetadataDrivesRouteCapacity(t *testing.T) {
	metadata := ModelMetadataOptions{Descriptor: &model.Model{
		ID: "same-model", CanonicalID: "vendor/same-model", WireID: "same-model",
		Limits: model.Limits{
			ContextTokens: 200_000, MaxOutputTokens: 24_000,
		},
		Capabilities: model.Capabilities{Streaming: true, ToolCalls: true},
		MetadataProvenance: model.MetadataProvenance{
			CanonicalID:  model.ProvenanceOperatorConfig,
			WireID:       model.ProvenanceOperatorConfig,
			Limits:       model.ProvenanceOperatorConfig,
			Capabilities: model.ProvenanceOperatorConfig,
			Pricing:      model.ProvenanceOperatorConfig,
		},
		Pricing:    model.Pricing{Provenance: model.ProvenanceOperatorConfig},
		Provenance: model.ProvenanceOperatorConfig,
	}}
	descriptor, err := resolveModelMetadata("same-model", metadata)
	if err != nil {
		t.Fatal(err)
	}
	route, err := resolveExecRoute(execRouteOptions{
		ProviderID: "openai-compatible", ModelID: "same-model",
		BaseURL:  "https://models.example.com/v1",
		Protocol: model.ProtocolOpenAIChat,
		Model:    descriptor,
	})
	if err != nil {
		t.Fatal(err)
	}
	capacity := agentcontext.ResolveCapacity(route, 0, 0, 0)
	if capacity.ContextTokens != 200_000 ||
		capacity.OutputCeiling != 24_000 ||
		capacity.HardInputTokens != 176_000 ||
		capacity.LimitSource != model.ProvenanceOperatorConfig {
		t.Fatalf("capacity = %+v", capacity)
	}
}

func TestResolveModelMetadataRejectsUnknownFacts(t *testing.T) {
	if _, err := resolveModelMetadata("custom", ModelMetadataOptions{}); err == nil {
		t.Fatal("resolveModelMetadata() accepted unknown facts")
	}
	if _, err := resolveExecRoute(execRouteOptions{
		ProviderID: "custom", ModelID: "custom", BaseURL: "http://127.0.0.1:1",
		Protocol: model.ProtocolOpenAIChat,
	}); err == nil {
		t.Fatal("resolveExecRoute() accepted missing metadata")
	}
}

func TestResolveModelMetadataAllowsExplicitUnknownPricing(t *testing.T) {
	descriptor, err := resolveModelMetadata("custom", ModelMetadataOptions{
		Descriptor: operatorModel("custom", 128_000, 8_192),
	})
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.Pricing.Known {
		t.Fatalf("custom pricing = %+v, want explicit unknown pricing", descriptor.Pricing)
	}
}

func TestEveryConnectionUsesTheCompatibleAdapterWithoutNameInference(t *testing.T) {
	deepSeek, err := resolveExecRoute(execRouteOptions{
		ProviderID: "deepseek", ModelID: "deepseek-chat",
		BaseURL: "http://127.0.0.1:1", Protocol: model.ProtocolOpenAIChat,
		Model: fixtureModel("deepseek-chat"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if deepSeek.Adapter() != model.AdapterOpenAICompatible {
		t.Fatalf(
			"known provider adapter = %q, want openai_compatible",
			deepSeek.Adapter(),
		)
	}
	lookalike, err := resolveExecRoute(execRouteOptions{
		ProviderID: "deepseek-shadow", ModelID: "shadow",
		BaseURL: "http://127.0.0.1:1", Protocol: model.ProtocolOpenAIChat,
		Model: fixtureModel("shadow"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if lookalike.Adapter() != model.AdapterOpenAICompatible {
		t.Fatalf("unknown provider adapter = %q, want openai_compatible", lookalike.Adapter())
	}
	futureModel, err := resolveModelMetadata("gpt-future", ModelMetadataOptions{
		Descriptor: operatorModel("gpt-future", 128_000, 8_192),
	})
	if err != nil {
		t.Fatal(err)
	}
	future, err := resolveExecRoute(execRouteOptions{
		ProviderID: "openai", ModelID: "gpt-future",
		BaseURL:  "https://api.openai.com/v1",
		Protocol: model.ProtocolOpenAIChat, Model: futureModel,
	})
	if err != nil {
		t.Fatal(err)
	}
	if future.Model().ID != "gpt-future" ||
		future.Adapter() != model.AdapterOpenAICompatible {
		t.Fatalf("future provider model route = %+v", future)
	}
}

func TestResolveModelMetadataRejectsConflictingSources(t *testing.T) {
	if _, err := resolveModelMetadata("custom", ModelMetadataOptions{
		Path: "model.json", Descriptor: operatorModel("custom", 4096, 1024),
	}); err == nil {
		t.Fatal("resolveModelMetadata() accepted conflicting sources")
	}
}

func TestResolveModelMetadataRejectsNonUSDPricing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model.json")
	data := []byte(`{
		"canonical_id":"custom",
		"wire_id":"custom",
		"context_tokens":4096,
		"max_output_tokens":1024,
		"capabilities":{"streaming":true},
		"pricing":{"input_per_million":1,"output_per_million":2,"currency":"CNY"}
	}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveModelMetadata(
		"custom",
		ModelMetadataOptions{Path: path},
	); err == nil {
		t.Fatal("resolveModelMetadata() accepted non-USD pricing")
	}
}

func operatorModel(id string, contextTokens, outputTokens uint64) *model.Model {
	return &model.Model{
		ID: id, CanonicalID: id, WireID: id,
		Limits: model.Limits{
			ContextTokens: contextTokens, MaxOutputTokens: outputTokens,
		},
		Capabilities: model.Capabilities{Streaming: true, ToolCalls: true},
		MetadataProvenance: model.MetadataProvenance{
			CanonicalID:  model.ProvenanceOperatorConfig,
			WireID:       model.ProvenanceOperatorConfig,
			Limits:       model.ProvenanceOperatorConfig,
			Capabilities: model.ProvenanceOperatorConfig,
			Pricing:      model.ProvenanceOperatorConfig,
		},
		Pricing:    model.Pricing{Provenance: model.ProvenanceOperatorConfig},
		Provenance: model.ProvenanceOperatorConfig,
	}
}
