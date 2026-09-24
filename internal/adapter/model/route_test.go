package model

import (
	"strings"
	"testing"
)

func TestResolverCreatesReadyRouteWithMetadata(t *testing.T) {
	resolver, err := NewResolver(testCatalog(t))
	if err != nil {
		t.Fatal(err)
	}

	route, err := resolver.Resolve(RouteRequest{
		ProviderID: "openai",
		ModelID:    "gpt-4.1",
		Provenance: ProvenanceStartup,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := route.Validate(); err != nil {
		t.Fatal(err)
	}
	if route.Adapter() != AdapterOpenAI || route.Protocol() != ProtocolOpenAIChat {
		t.Fatalf("unexpected route: adapter=%q protocol=%q", route.Adapter(), route.Protocol())
	}
	model := route.Model()
	if model.WireID != "gpt-4.1" || !model.Capabilities.NativeSearch {
		t.Fatalf("unexpected model metadata: %+v", model)
	}
	if model.Pricing.Provenance != ProvenanceBundled || route.Provenance() != ProvenanceStartup {
		t.Fatalf("unexpected provenance: model=%q route=%q", model.Pricing.Provenance, route.Provenance())
	}
}

func TestConnectionIdentityIncludesEndpointAndNormalizesTrailingSlash(t *testing.T) {
	resolver, err := NewResolver(testCatalog(t))
	if err != nil {
		t.Fatal(err)
	}
	route, err := resolver.Resolve(RouteRequest{
		ProviderID: "openai",
		ModelID:    "gpt-4.1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if route.ConnectionID() == route.WithEndpoint(
		"https://models.example.com/v1",
	).ConnectionID() {
		t.Fatal("different endpoints share a connection identity")
	}
	if route.ConnectionID() != route.WithEndpoint(
		route.Endpoint()+"/",
	).ConnectionID() {
		t.Fatal("equivalent endpoint spelling changed the connection identity")
	}
}

func TestResolverDoesNotInferProviderFromModelName(t *testing.T) {
	resolver, err := NewResolver(testCatalog(t))
	if err != nil {
		t.Fatal(err)
	}

	_, err = resolver.Resolve(RouteRequest{ModelID: "nonexistent-model"})

	if err == nil || !strings.Contains(err.Error(), "provider id is required") {
		t.Fatalf("Resolve() error = %v, want explicit provider error", err)
	}
}

func TestResolverRejectsForeignModel(t *testing.T) {
	resolver, err := NewResolver(testCatalog(t))
	if err != nil {
		t.Fatal(err)
	}

	_, err = resolver.Resolve(RouteRequest{ProviderID: "openai", ModelID: "nonexistent-model"})

	if err == nil || !strings.Contains(err.Error(), "does not offer model") {
		t.Fatalf("Resolve() error = %v, want foreign model error", err)
	}
}

func TestResolverAutoRouteRequiresExplicitUniqueMatch(t *testing.T) {
	resolver, err := NewResolver(testCatalog(t))
	if err != nil {
		t.Fatal(err)
	}
	route, err := resolver.Resolve(RouteRequest{ModelID: "glm-4-flash", Auto: true})
	if err != nil {
		t.Fatal(err)
	}
	if route.ProviderID() != "zai" || route.Provenance() != ProvenanceBundled {
		t.Fatalf("auto route = %+v", route)
	}
	if route.Model().MetadataProvenance.Limits != ProvenanceBundled {
		t.Fatalf("metadata provenance = %+v", route.Model().MetadataProvenance)
	}
}

// TestAutoRouteForAGPTModelIsAmbiguousBetweenChatAndResponses pins D6: the
// Responses provider shares the gpt-4.1 model id with the chat provider, so
// Auto must refuse rather than guess which protocol the operator wanted.
func TestAutoRouteForAGPTModelIsAmbiguousBetweenChatAndResponses(t *testing.T) {
	resolver, err := NewResolver(testCatalog(t))
	if err != nil {
		t.Fatal(err)
	}
	_, err = resolver.Resolve(RouteRequest{ModelID: "gpt-4.1", Auto: true})
	if err == nil || !strings.Contains(err.Error(), "exactly one provider") {
		t.Fatalf("Resolve() error = %v, want an ambiguous-provider refusal", err)
	}
}

func TestBundledResponsesRouteIsReachableWithoutACustomEndpoint(t *testing.T) {
	resolver, err := NewResolver(testCatalog(t))
	if err != nil {
		t.Fatal(err)
	}
	route, err := resolver.Resolve(RouteRequest{
		ProviderID: "openai-responses", ModelID: "gpt-4.1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if route.Protocol() != ProtocolOpenAIResponses {
		t.Fatalf("protocol = %q, want openai_responses", route.Protocol())
	}
	if route.Endpoint() != "https://api.openai.com/v1" {
		t.Fatalf("endpoint = %q", route.Endpoint())
	}
	if !route.Model().Capabilities.PromptCache {
		t.Fatal("Responses gpt-4.1 must advertise prompt_cache: that is where the sticky key is sent")
	}
	if route.Credential().Name != "OPENAI_API_KEY" {
		t.Fatalf("credential = %+v, want the shared OpenAI key", route.Credential())
	}
}

func TestDeepSeekV4FlashDefaultsToChatCompletions(t *testing.T) {
	resolver, err := NewResolver(testCatalog(t))
	if err != nil {
		t.Fatal(err)
	}
	route, err := resolver.Resolve(RouteRequest{
		ProviderID: "deepseek-v4-flash", ModelID: "deepseek-v4-flash",
	})
	if err != nil {
		t.Fatal(err)
	}
	if route.Protocol() != ProtocolOpenAIChat {
		t.Fatalf("protocol = %q, want openai_chat", route.Protocol())
	}
	if route.Adapter() != AdapterOpenAICompatible {
		t.Fatalf("adapter = %q, want openai_compatible", route.Adapter())
	}
	if route.Model().Capabilities.IncrementalResponses {
		t.Fatal("DeepSeek Chat must keep complete HTTP/SSE transport")
	}
	if route.Model().Limits.ContextTokens != 1_048_576 ||
		route.Model().Limits.MaxOutputTokens != 384_000 ||
		!route.Model().Capabilities.PromptCache {
		t.Fatalf("DeepSeek V4 Flash metadata = %+v", route.Model())
	}
	if route.Model().Pricing.Known {
		t.Fatal("time-varying DeepSeek pricing must fail closed without an effective window")
	}
}

func TestRouteIdentityExcludesVolatilePricing(t *testing.T) {
	resolver, err := NewResolver(testCatalog(t))
	if err != nil {
		t.Fatal(err)
	}
	route, err := resolver.Resolve(RouteRequest{
		ProviderID: "openai", ModelID: "gpt-4.1",
	})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := route.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if identity.ProviderID != route.ProviderID() ||
		identity.ModelID != route.Model().ID ||
		identity.WireID != route.Model().WireID {
		t.Fatalf("route identity = %+v", identity)
	}
}

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

func TestZeroReadyRouteIsInvalid(t *testing.T) {
	var route ReadyRoute
	if err := route.Validate(); err == nil {
		t.Fatal("zero ReadyRoute is valid")
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
