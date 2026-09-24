package wire

import (
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
)

func reasoningConnectionRoute(t *testing.T, reasoning bool) model.ReadyRoute {
	t.Helper()
	id := "plain-model"
	if reasoning {
		id = "reasoning-model"
	}
	descriptor := testCustomModel(id)
	if reasoning {
		descriptor.Capabilities.Reasoning = true
		descriptor.Capabilities.ReasoningEfforts = []string{"off", "low", "high", "max"}
		descriptor.Capabilities.DefaultReasoningEffort = "high"
	}
	route, err := resolveExecRoute(execRouteOptions{
		ProviderID: "openai-compatible:reason", ModelID: id,
		BaseURL:  "https://models.example.com/v1",
		Protocol: model.ProtocolOpenAIChat, Model: &descriptor,
	})
	if err != nil {
		t.Fatal(err)
	}
	return route
}

func TestSelectedReasoningCapabilitiesAdvertiseNativeLevels(t *testing.T) {
	route := reasoningConnectionRoute(t, true)
	capabilities := selectedModelCapabilities(route)
	if capabilities.DefaultReasoningEffort != "high" ||
		len(capabilities.ReasoningEfforts) != 4 ||
		capabilities.ReasoningEfforts[0] != "off" ||
		capabilities.ReasoningEfforts[2] != "high" ||
		capabilities.ReasoningEfforts[3] != "max" {
		t.Fatalf("capabilities = %+v", capabilities)
	}
}

func TestReasoningDefaultEffortIsAppliedWithoutChangingExplicitValues(t *testing.T) {
	route := reasoningConnectionRoute(t, true)
	if effort := effectiveReasoningEffort(route, ""); effort != "high" {
		t.Fatalf("default reasoning effort = %q, want high", effort)
	}
	if effort := effectiveReasoningEffort(route, "max"); effort != "max" {
		t.Fatalf("explicit reasoning effort = %q, want max", effort)
	}

	plain := reasoningConnectionRoute(t, false)
	if effort := effectiveReasoningEffort(plain, ""); effort != "" {
		t.Fatalf("non-reasoning model effort = %q, want empty", effort)
	}
}
