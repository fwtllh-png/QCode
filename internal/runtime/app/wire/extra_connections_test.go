package wire

import (
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
)

func customConnectionModel(id string) *model.Model {
	return &model.Model{
		ID: id, CanonicalID: id, WireID: id,
		Limits:      model.Limits{ContextTokens: 65_536, MaxOutputTokens: 8_192},
		Capabilities: model.Capabilities{Streaming: true, ToolCalls: true},
	}
}

func TestExtraConnectionRoutesResolveBaselineAndAdditionalModels(t *testing.T) {
	baseline := customConnectionModel("vendor/model-x")
	additional := customConnectionModel("vendor/model-y")
	routes, err := extraConnectionRoutes([]ExtraConnectionSpec{
		{
			ProviderID: "openai-compatible:abc123",
			BaseURL:    "https://models.example.com/v1",
			Protocol:   model.ProtocolOpenAIChat,
			Credential: model.CredentialRef{Kind: "keyring", Name: "web/test/custom"},
			Model:      baseline,
			Models: map[string]model.Model{
				additional.ID: *additional,
			},
		},
		{
			ProviderID: "openai-compatible:def456",
			BaseURL:    "https://api.deepseek.example/v1",
			Protocol:   model.ProtocolOpenAIChat,
			Credential: model.CredentialRef{Kind: "keyring", Name: "web/test/second"},
			Model:      customConnectionModel("second-model"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	custom, ok := routes[model.RouteKey("openai-compatible:abc123", "vendor/model-x")]
	if !ok {
		t.Fatal("custom connection baseline route missing")
	}
	if custom.Endpoint() != "https://models.example.com/v1" ||
		custom.Credential().Name != "web/test/custom" ||
		custom.Model().Limits.ContextTokens != 65_536 {
		t.Fatalf("custom route = %+v", custom)
	}
	extra, ok := routes[model.RouteKey("openai-compatible:abc123", "vendor/model-y")]
	if !ok {
		t.Fatal("custom connection additional model route missing")
	}
	if extra.Credential().Name != "web/test/custom" {
		t.Fatalf("additional route credential = %+v", extra.Credential())
	}
	second, ok := routes[model.RouteKey("openai-compatible:def456", "second-model")]
	if !ok {
		t.Fatal("second connection route missing")
	}
	if second.Endpoint() != "https://api.deepseek.example/v1" ||
		second.Credential().Name != "web/test/second" {
		t.Fatalf("second route = %+v", second)
	}
}

func TestExtraConnectionRoutesRejectsMissingBaseURLOrMetadata(t *testing.T) {
	if _, err := extraConnectionRoutes([]ExtraConnectionSpec{{
		ProviderID: "openai-compatible:none",
		BaseURL:    "https://models.example.com/v1",
		Protocol:   model.ProtocolOpenAIChat,
	}}); err == nil {
		t.Fatal("custom extra connection without metadata was accepted")
	}
	if _, err := extraConnectionRoutes([]ExtraConnectionSpec{{
		ProviderID: "openai-compatible:none",
		Protocol:   model.ProtocolOpenAIChat,
		Model:      customConnectionModel("vendor/model-x"),
	}}); err == nil {
		t.Fatal("extra connection without a base URL was accepted")
	}
}
