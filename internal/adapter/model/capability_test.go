package model

import (
	"strings"
	"testing"
)

func TestRequireCapabilitiesNamesWhatIsMissing(t *testing.T) {
	have := Capabilities{Streaming: true, ToolCalls: true}
	err := RequireCapabilities("local", have, []Capability{CapVision, CapToolCalls})
	if err == nil || !strings.Contains(err.Error(), "vision") || !strings.Contains(err.Error(), `"local"`) {
		t.Fatalf("RequireCapabilities() error = %v, want the model and the missing bit", err)
	}
	if err := RequireCapabilities("local", have, []Capability{CapToolCalls}); err != nil {
		t.Fatal(err)
	}
}

func TestAVisionSlotWithoutVisionIsRefusedAtConstruction(t *testing.T) {
	act := testRoute(t, "deepseek", "deepseek-chat")
	// deepseek-chat is an ordinary chat model: no vision bit in the catalog.
	blind := testRoute(t, "deepseek", "deepseek-chat")

	_, err := NewRouteSet(act, map[Purpose]ReadyRoute{PurposeVision: blind}, false)
	if err == nil || !strings.Contains(err.Error(), "vision") {
		t.Fatalf("NewRouteSet() error = %v, want a vision capability refusal", err)
	}
}

func TestFallingBackToABlindActForVisionIsRefused(t *testing.T) {
	act := testRoute(t, "deepseek", "deepseek-chat")

	routes, err := NewRouteSet(act, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	// Plan is ordinary chat, so it still falls back.
	if _, routeErr := routes.For(PurposePlan); routeErr != nil {
		t.Fatalf("For(%q) error = %v", PurposePlan, routeErr)
	}
	_, err = routes.For(PurposeVision)
	if err == nil || !strings.Contains(err.Error(), "vision") {
		t.Fatalf("For(vision) error = %v, want a capability refusal on the act fallback", err)
	}
}

func TestResolverRequireRefusesAModelMissingTheBit(t *testing.T) {
	resolver, err := NewResolver(DefaultCatalog())
	if err != nil {
		t.Fatal(err)
	}
	_, err = resolver.Resolve(RouteRequest{
		ProviderID: "deepseek", ModelID: "deepseek-chat",
		Require: []Capability{CapVision},
	})
	if err == nil || !strings.Contains(err.Error(), "vision") {
		t.Fatalf("Resolve() error = %v, want a vision refusal", err)
	}
	route, err := resolver.Resolve(RouteRequest{
		ProviderID: "openai", ModelID: "gpt-4.1",
		Require: []Capability{CapVision},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !route.Model().Capabilities.Vision {
		t.Fatal("gpt-4.1 should advertise vision in the catalog")
	}
}

func TestPurposeRequiredCapabilitiesOnlyVisionAsksForVision(t *testing.T) {
	if got := PurposeRequiredCapabilities(PurposeVision); len(got) != 1 || got[0] != CapVision {
		t.Fatalf("vision requirements = %v", got)
	}
	for _, purpose := range []Purpose{PurposeAct, PurposePlan} {
		if got := PurposeRequiredCapabilities(purpose); got != nil {
			t.Fatalf("%s requirements = %v, want none", purpose, got)
		}
	}
}

func TestIncrementalResponsesIsAdvertisedOnlyByBundledResponsesRoute(t *testing.T) {
	resolver, err := NewResolver(DefaultCatalog())
	if err != nil {
		t.Fatal(err)
	}
	responses, err := resolver.Resolve(RouteRequest{
		ProviderID: "openai-responses", ModelID: "gpt-4.1",
	})
	if err != nil {
		t.Fatal(err)
	}
	chat, err := resolver.Resolve(RouteRequest{
		ProviderID: "openai", ModelID: "gpt-4.1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !responses.Model().Capabilities.IncrementalResponses {
		t.Fatal("bundled Responses route must advertise incremental transport")
	}
	if chat.Model().Capabilities.IncrementalResponses {
		t.Fatal("Chat route must not advertise Responses transport")
	}
}

func TestAutomaticPromptCacheRequiresPromptCacheSupport(t *testing.T) {
	provider, ok := DefaultCatalog().Provider("deepseek")
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
	catalog := DefaultCatalog()
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

func TestReadyRouteWithModelIDPreservesConnection(t *testing.T) {
	resolver, err := NewResolver(DefaultCatalog())
	if err != nil {
		t.Fatal(err)
	}
	route, err := resolver.Resolve(RouteRequest{
		ProviderID: "deepseek",
		ModelID:    "deepseek-chat",
	})
	if err != nil {
		t.Fatal(err)
	}
	updated := route.WithModelID("deepseek-next")
	if updated.ProviderID() != route.ProviderID() ||
		updated.Endpoint() != route.Endpoint() ||
		updated.Credential() != route.Credential() {
		t.Fatalf("connection changed: %+v", updated)
	}
	if updated.Model().ID != "deepseek-next" ||
		updated.Model().WireID != "deepseek-next" {
		t.Fatalf("model identity = %+v", updated.Model())
	}
	if updated.Model().Provenance != ProvenanceStartup ||
		updated.Model().MetadataProvenance.Pricing != ProvenanceStartup ||
		updated.Model().Pricing.Known {
		t.Fatalf("derived model provenance = %+v", updated.Model())
	}
}
