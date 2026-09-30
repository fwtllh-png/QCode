package model

import (
	"strings"
	"testing"
)

func TestCatalogRejectsAdapterProtocolMismatch(t *testing.T) {
	_, err := NewCatalog(Provider{
		ID: "invalid", Adapter: AdapterID("legacy"),
		Endpoint: "https://example.com", Protocol: ProtocolOpenAIChat,
		Models: map[string]Model{"model": {
			ID: "model", CanonicalID: "model", WireID: "model",
			Limits: Limits{ContextTokens: 1024, MaxOutputTokens: 128},
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "does not support protocol") {
		t.Fatalf("NewCatalog() error = %v, want adapter/protocol refusal", err)
	}
}

func TestCatalogDefensivelyCopiesProvider(t *testing.T) {
	catalog := testCatalog(t)
	provider, ok := catalog.Provider("openai")
	if !ok {
		t.Fatal("openai provider missing")
	}
	delete(provider.Models, "gpt-4.1")
	if second, _ := catalog.Provider("openai"); len(second.Models) != 1 {
		t.Fatal("catalog was mutated through returned provider")
	}
}

func TestAutomaticPromptCacheRequiresPromptCacheSupport(t *testing.T) {
	provider, ok := testCatalog(t).Provider("deepseek")
	if !ok {
		t.Fatal("DeepSeek provider is missing")
	}
	descriptor := provider.Models["deepseek-chat"]
	descriptor.Capabilities.PromptCache = false
	descriptor.Capabilities.AutomaticPromptCache = true
	provider.Models["deepseek-chat"] = descriptor
	if _, err := NewCatalog(provider); err == nil ||
		!strings.Contains(err.Error(), "automatic prompt cache") {
		t.Fatalf("NewCatalog() error = %v", err)
	}
}

func TestBundledReasoningEffortsAreExplicitAndIsolated(t *testing.T) {
	catalog := testCatalog(t)
	for _, entry := range []struct {
		provider string
		model    string
	}{
		{provider: "deepseek", model: "deepseek-reasoner"},
		{provider: "deepseek-v4-flash", model: "deepseek-v4-flash"},
		{provider: "deepseek-v4-flash", model: "deepseek-v4-flash-vision-exp"},
	} {
		deepseek, ok := catalog.Provider(entry.provider)
		if !ok {
			t.Fatalf("%s provider is missing", entry.provider)
		}
		capabilities := deepseek.Models[entry.model].Capabilities
		levels := capabilities.ReasoningEffortLevels()
		if len(levels) != 4 || levels[0] != "off" ||
			levels[1] != "low" || levels[2] != "high" || levels[3] != "max" {
			t.Fatalf("%s reasoning efforts = %v", entry.model, levels)
		}
		if capabilities.DefaultReasoningEffort != "high" {
			t.Fatalf(
				"%s default reasoning effort = %q",
				entry.model,
				capabilities.DefaultReasoningEffort,
			)
		}
	}
	deepseek, _ := catalog.Provider("deepseek-v4-flash")
	vision := deepseek.Models["deepseek-v4-flash-vision-exp"].Capabilities
	if !vision.Vision || !vision.ImageInput {
		t.Fatalf("vision model capabilities = %+v", vision)
	}
	levels := deepseek.Models["deepseek-v4-flash"].
		Capabilities.ReasoningEffortLevels()
	levels[3] = "mutated"
	again, _ := catalog.Provider("deepseek-v4-flash")
	if got := again.Models["deepseek-v4-flash"].
		Capabilities.ReasoningEffortLevels()[3]; got != "max" {
		t.Fatalf("catalog reasoning efforts mutated to %q", got)
	}
}

func TestReasoningCapabilityDoesNotInventEffortLevels(t *testing.T) {
	capabilities := Capabilities{Reasoning: true}
	if levels := capabilities.ReasoningEffortLevels(); len(levels) != 0 {
		t.Fatalf("undeclared reasoning efforts = %v", levels)
	}
	if capabilities.SupportsReasoningEffort("low") {
		t.Fatal("undeclared reasoning effort was accepted")
	}
}

func TestCatalogRejectsProtocolAndReasoningCapabilityContradictions(t *testing.T) {
	base := Provider{
		ID: "custom", Adapter: AdapterOpenAICompatible,
		Endpoint:   "https://models.example.com/v1",
		Protocol:   ProtocolOpenAIChat,
		Provenance: ProvenanceOperatorConfig,
		Models: map[string]Model{"model": {
			ID: "model", CanonicalID: "vendor/model", WireID: "model",
			Limits: Limits{ContextTokens: 4096, MaxOutputTokens: 1024},
			Capabilities: Capabilities{
				Streaming: true, IncrementalResponses: true,
			},
			MetadataProvenance: MetadataProvenance{
				CanonicalID:  ProvenanceOperatorConfig,
				WireID:       ProvenanceOperatorConfig,
				Limits:       ProvenanceOperatorConfig,
				Capabilities: ProvenanceOperatorConfig,
				Pricing:      ProvenanceOperatorConfig,
			},
			Pricing:    Pricing{Provenance: ProvenanceOperatorConfig},
			Provenance: ProvenanceOperatorConfig,
		}},
	}
	if _, err := NewCatalog(base); err == nil ||
		!strings.Contains(err.Error(), "responses protocol") {
		t.Fatalf("incremental chat catalog error = %v", err)
	}
	entry := base.Models["model"]
	entry.Capabilities = Capabilities{
		Streaming: true, ThinkingToggle: true,
	}
	base.Models["model"] = entry
	if _, err := NewCatalog(base); err == nil ||
		!strings.Contains(err.Error(), "without reasoning") {
		t.Fatalf("thinking toggle catalog error = %v", err)
	}
}

// testCatalog 构造内联目录 fixture，覆盖路由/能力/路由集测试所需的
// 场景：共享模型 id 的 chat 与 responses provider、普通 chat 模型、
// 推理模型与视觉模型。内置 provider 目录已删除，测试目录由此提供。
func testCatalog(t *testing.T) *Catalog {
	t.Helper()
	catalog, err := NewCatalog(
		Provider{
			ID: "openai", Adapter: AdapterOpenAI,
			Endpoint: "https://api.openai.com/v1", Protocol: ProtocolOpenAIChat,
			Credential: CredentialRef{Kind: "env", Name: "OPENAI_API_KEY"},
			Provenance: ProvenanceBundled,
			Models: map[string]Model{
				"gpt-4.1": {
					ID: "gpt-4.1", CanonicalID: "gpt-4.1", WireID: "gpt-4.1",
					Limits:     Limits{ContextTokens: 1_047_576, MaxOutputTokens: 32_768},
					Provenance: ProvenanceBundled,
					Capabilities: Capabilities{
						Streaming: true, ToolCalls: true, NativeSearch: true,
						Vision: true, ImageInput: true, PromptCache: true,
					},
					Pricing: Pricing{
						InputPerMillion: 2, OutputPerMillion: 8,
						Currency: "USD", Known: true, Provenance: ProvenanceBundled,
					},
				},
			},
		},
		Provider{
			ID: "openai-responses", Adapter: AdapterOpenAI,
			Endpoint: "https://api.openai.com/v1", Protocol: ProtocolOpenAIResponses,
			Credential: CredentialRef{Kind: "env", Name: "OPENAI_API_KEY"},
			Provenance: ProvenanceBundled,
			Models: map[string]Model{
				"gpt-4.1": {
					ID: "gpt-4.1", CanonicalID: "gpt-4.1", WireID: "gpt-4.1",
					Limits:     Limits{ContextTokens: 1_047_576, MaxOutputTokens: 32_768},
					Provenance: ProvenanceBundled,
					Capabilities: Capabilities{
						Streaming: true, ToolCalls: true, NativeSearch: true,
						Vision: true, ImageInput: true, PromptCache: true,
						IncrementalResponses: true,
					},
					Pricing: Pricing{Provenance: ProvenanceBundled},
				},
			},
		},
		Provider{
			ID: "deepseek", Adapter: AdapterOpenAICompatible,
			Endpoint: "https://api.deepseek.com/v1", Protocol: ProtocolOpenAIChat,
			Credential: CredentialRef{Kind: "env", Name: "DEEPSEEK_API_KEY"},
			Provenance: ProvenanceBundled,
			Models: map[string]Model{
				"deepseek-chat": {
					ID: "deepseek-chat", CanonicalID: "deepseek-chat",
					WireID:     "deepseek-chat",
					Limits:     Limits{ContextTokens: 131_072, MaxOutputTokens: 8_192},
					Provenance: ProvenanceBundled,
					Capabilities: Capabilities{
						Streaming: true, ToolCalls: true, PromptCache: true,
					},
					Pricing: Pricing{Provenance: ProvenanceBundled},
				},
				"deepseek-reasoner": {
					ID: "deepseek-reasoner", CanonicalID: "deepseek-reasoner",
					WireID:     "deepseek-reasoner",
					Limits:     Limits{ContextTokens: 131_072, MaxOutputTokens: 8_192},
					Provenance: ProvenanceBundled,
					Capabilities: Capabilities{
						Streaming: true, ToolCalls: true, PromptCache: true,
						Reasoning:              true,
						ReasoningEfforts:       []string{"off", "low", "high", "max"},
						DefaultReasoningEffort: "high",
					},
					Pricing: Pricing{Provenance: ProvenanceBundled},
				},
			},
		},
		Provider{
			ID: "deepseek-v4-flash", Adapter: AdapterOpenAICompatible,
			Endpoint: "https://api.deepseek.com", Protocol: ProtocolOpenAIChat,
			Credential: CredentialRef{Kind: "keyring", Name: "deepseek/default"},
			Provenance: ProvenanceBundled,
			Models: map[string]Model{
				"deepseek-v4-flash": {
					ID: "deepseek-v4-flash", CanonicalID: "deepseek-v4-flash",
					WireID:     "deepseek-v4-flash",
					Limits:     Limits{ContextTokens: 1_048_576, MaxOutputTokens: 384_000},
					Provenance: ProvenanceBundled,
					Capabilities: Capabilities{
						Streaming: true, ToolCalls: true, PromptCache: true,
						Reasoning:              true,
						ReasoningEfforts:       []string{"off", "low", "high", "max"},
						DefaultReasoningEffort: "high",
					},
					Pricing: Pricing{Provenance: ProvenanceBundled},
				},
				"deepseek-v4-flash-vision-exp": {
					ID:          "deepseek-v4-flash-vision-exp",
					CanonicalID: "deepseek-v4-flash-vision-exp",
					WireID:      "deepseek-v4-flash-vision-exp",
					Limits:      Limits{ContextTokens: 1_048_576, MaxOutputTokens: 384_000},
					Provenance:  ProvenanceBundled,
					Capabilities: Capabilities{
						Streaming: true, ToolCalls: true, PromptCache: true,
						Vision: true, ImageInput: true,
						Reasoning:              true,
						ReasoningEfforts:       []string{"off", "low", "high", "max"},
						DefaultReasoningEffort: "high",
					},
					Pricing: Pricing{Provenance: ProvenanceBundled},
				},
			},
		},
		Provider{
			ID: "zai", Adapter: AdapterOpenAICompatible,
			Endpoint:   "https://open.bigmodel.cn/api/paas/v4",
			Protocol:   ProtocolOpenAIChat,
			Credential: CredentialRef{Kind: "env", Name: "ZAI_API_KEY"},
			Provenance: ProvenanceBundled,
			Models: map[string]Model{
				"glm-4-flash": {
					ID: "glm-4-flash", CanonicalID: "glm-4-flash",
					WireID: "glm-4-flash",
					Limits: Limits{ContextTokens: 131_072, MaxOutputTokens: 8_192},
					Capabilities: Capabilities{
						Streaming: true, ToolCalls: true, PromptCache: true,
					},
					Provenance: ProvenanceBundled,
					Pricing:    Pricing{Provenance: ProvenanceBundled},
				},
			},
		},
	)
	if err != nil {
		t.Fatalf("build test catalog: %v", err)
	}
	return catalog
}
