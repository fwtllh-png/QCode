package engine

import (
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
)

// testCatalog 内联 engine 测试所需的连接目录：deepseek 上的 chat 与
// reasoner 模型，以及独立的 deepseek-v4-flash 端点。内置目录已删除，
// 连接元数据由此 fixture 提供。
func testCatalog(t *testing.T) *model.Catalog {
	t.Helper()
	catalog, err := model.NewCatalog(
		model.Provider{
			ID: "deepseek", Adapter: model.AdapterOpenAICompatible,
			Endpoint: "https://api.deepseek.com/v1", Protocol: model.ProtocolOpenAIChat,
			Credential: model.CredentialRef{Kind: "env", Name: "DEEPSEEK_API_KEY"},
			Provenance: model.ProvenanceBundled,
			Models: map[string]model.Model{
				"deepseek-chat": {
					ID: "deepseek-chat", CanonicalID: "deepseek-chat",
					WireID: "deepseek-chat",
					Limits: model.Limits{ContextTokens: 131_072, MaxOutputTokens: 8_192},
					Capabilities: model.Capabilities{
						Streaming: true, ToolCalls: true, PromptCache: true,
					},
					Pricing:    model.Pricing{Provenance: model.ProvenanceBundled},
					Provenance: model.ProvenanceBundled,
				},
				"deepseek-reasoner": {
					ID: "deepseek-reasoner", CanonicalID: "deepseek-reasoner",
					WireID: "deepseek-reasoner",
					Limits: model.Limits{ContextTokens: 131_072, MaxOutputTokens: 8_192},
					Capabilities: model.Capabilities{
						Streaming: true, ToolCalls: true, PromptCache: true,
						Reasoning:              true,
						ReasoningEfforts:       []string{"off", "low", "high", "max"},
						DefaultReasoningEffort: "high",
					},
					Pricing:    model.Pricing{Provenance: model.ProvenanceBundled},
					Provenance: model.ProvenanceBundled,
				},
			},
		},
		deepSeekV4FlashProvider(),
	)
	if err != nil {
		t.Fatalf("build engine test catalog: %v", err)
	}
	return catalog
}

// deepSeekV4FlashProvider 内联 DeepSeek V4 Flash 连接声明，供 live 运行
// 手册（docs/DEEPSEEK-LIVE.zh-CN.md）路径使用。
func deepSeekV4FlashProvider() model.Provider {
	return model.Provider{
		ID: "deepseek-v4-flash", Adapter: model.AdapterOpenAICompatible,
		Endpoint: "https://api.deepseek.com", Protocol: model.ProtocolOpenAIChat,
		Credential: model.CredentialRef{Kind: "keyring", Name: "deepseek/default"},
		Provenance: model.ProvenanceBundled,
		Models: map[string]model.Model{
			"deepseek-v4-flash": {
				ID: "deepseek-v4-flash", CanonicalID: "deepseek-v4-flash",
				WireID: "deepseek-v4-flash",
				Limits: model.Limits{
					ContextTokens: 1_048_576, MaxOutputTokens: 384_000,
				},
				Capabilities: model.Capabilities{
					Streaming: true, Reasoning: true, ToolCalls: true,
					PromptCache: true, AutomaticPromptCache: true, ThinkingToggle: true,
					ReasoningEfforts:       []string{"off", "low", "high", "max"},
					DefaultReasoningEffort: "high",
				},
				Pricing:    model.Pricing{Provenance: model.ProvenanceBundled},
				Provenance: model.ProvenanceBundled,
			},
		},
	}
}
