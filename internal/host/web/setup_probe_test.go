package web

import (
	"testing"

	webhost "github.com/fwtllh-png/QCode/internal/host/runtimeapi/web"
)

func TestSetupProbeConnectionMatchesApplyBoundary(t *testing.T) {
	for _, protocol := range []string{"openai_chat", "openai_responses"} {
		gotID, endpoint, got, err := resolveSetupProbeConnection(webhost.SetupProbeRequest{
			BaseURL: "https://models.example.com/v1/", Protocol: protocol,
		})
		if err != nil ||
			gotID != connectionID("https://models.example.com/v1") ||
			endpoint != "https://models.example.com/v1" || string(got) != protocol {
			t.Fatalf("id=%s endpoint=%s protocol=%s err=%v", gotID, endpoint, got, err)
		}
	}
	if _, endpoint, protocol, err := resolveSetupProbeConnection(webhost.SetupProbeRequest{
		BaseURL: "https://models.example.com/v1",
	}); err != nil ||
		endpoint != "https://models.example.com/v1" ||
		protocol != "openai_chat" {
		t.Fatalf("default protocol: endpoint=%s protocol=%s err=%v", endpoint, protocol, err)
	}
	for _, request := range []webhost.SetupProbeRequest{
		{},
		{BaseURL: "file:///tmp"},
		{BaseURL: "https://example.com", Protocol: "unknown"},
	} {
		if _, _, _, err := resolveSetupProbeConnection(request); err == nil {
			t.Fatalf("accepted invalid connection: %+v", request)
		}
	}
}
