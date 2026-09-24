package model

import "testing"

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
					Limits:       Limits{ContextTokens: 1_047_576, MaxOutputTokens: 32_768},
					Provenance:   ProvenanceBundled,
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
					Limits:       Limits{ContextTokens: 1_047_576, MaxOutputTokens: 32_768},
					Provenance:   ProvenanceBundled,
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
					WireID: "deepseek-chat",
					Limits:     Limits{ContextTokens: 131_072, MaxOutputTokens: 8_192},
					Provenance: ProvenanceBundled,
					Capabilities: Capabilities{
						Streaming: true, ToolCalls: true, PromptCache: true,
					},
					Pricing: Pricing{Provenance: ProvenanceBundled},
				},
				"deepseek-reasoner": {
					ID: "deepseek-reasoner", CanonicalID: "deepseek-reasoner",
					WireID: "deepseek-reasoner",
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
					WireID: "deepseek-v4-flash",
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
					ID: "deepseek-v4-flash-vision-exp",
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
			Endpoint: "https://open.bigmodel.cn/api/paas/v4",
			Protocol: ProtocolOpenAIChat,
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
