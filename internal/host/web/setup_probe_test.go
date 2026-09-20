package web

import (
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	webhost "github.com/fwtllh-png/QCode/internal/host/runtimeapi/web"
)

func TestSetupProbeConnectionMatchesApplyBoundary(t *testing.T) {
	for _, providerID := range []string{"openai", "deepseek", "glm"} {
		gotID, endpoint, protocol, err := resolveSetupProbeConnection(webhost.SetupProbeRequest{
			Provider: providerID, Model: "unknown-model",
			BaseURL: "https://ignored.invalid", Protocol: "ignored",
		})
		provider, _ := model.DefaultCatalog().Provider(providerID)
		if err != nil || gotID != providerID || endpoint != provider.Endpoint || protocol != provider.Protocol {
			t.Fatalf("%s: id=%s endpoint=%s protocol=%s err=%v", providerID, gotID, endpoint, protocol, err)
		}
	}
	for _, protocol := range []string{"openai_chat", "openai_responses"} {
		_, endpoint, got, err := resolveSetupProbeConnection(webhost.SetupProbeRequest{
			Provider: customProviderID, BaseURL: "https://models.example.com/v1/", Protocol: protocol,
		})
		if err != nil || endpoint != "https://models.example.com/v1" || string(got) != protocol {
			t.Fatalf("endpoint=%s protocol=%s err=%v", endpoint, got, err)
		}
	}
	for _, request := range []webhost.SetupProbeRequest{
		{Provider: "unknown"},
		{Provider: customProviderID, BaseURL: "file:///tmp"},
		{Provider: customProviderID, BaseURL: "https://example.com", Protocol: "unknown"},
	} {
		if _, _, _, err := resolveSetupProbeConnection(request); err == nil {
			t.Fatalf("accepted invalid connection: %+v", request)
		}
	}
}
