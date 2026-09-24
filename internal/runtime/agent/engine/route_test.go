package engine

import (
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	providerfixture "github.com/fwtllh-png/QCode/internal/adapter/provider/fixture"
	"github.com/fwtllh-png/QCode/internal/security/policy"
)

// namedRoute is a second route distinguishable from testRoute's by model id,
// price and window, so an assertion can tell which one a turn used and which one
// it was budgeted against.
func namedRoute(t *testing.T, modelID string) model.ReadyRoute {
	t.Helper()
	catalog, err := model.NewCatalog(model.Provider{
		ID: "second", Adapter: model.AdapterOpenAICompatible, Endpoint: "http://127.0.0.1:2",
		Protocol: model.ProtocolOpenAIChat, Provenance: model.ProvenanceFixture,
		Models: map[string]model.Model{modelID: {
			ID: modelID, CanonicalID: modelID, WireID: modelID,
			Limits:       model.Limits{ContextTokens: 8192, MaxOutputTokens: 256},
			Capabilities: model.Capabilities{Streaming: true, ToolCalls: true},
			Pricing: model.Pricing{
				InputPerMillion: 10, OutputPerMillion: 10,
				Currency: "USD", Known: true, Provenance: model.ProvenanceFixture,
			},
			Provenance: model.ProvenanceFixture,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := model.NewResolver(catalog)
	if err != nil {
		t.Fatal(err)
	}
	route, err := resolver.Resolve(model.RouteRequest{ProviderID: "second", ModelID: modelID})
	if err != nil {
		t.Fatal(err)
	}
	return route
}

func textStream(text string) provider.Stream {
	return &providerfixture.SliceStream{Events: []provider.StreamEvent{
		{Type: provider.EventTextDelta, Text: text},
		{Type: provider.EventMessageStop},
	}}
}

func TestATurnWithoutARouteTableSamplesOnTheOnlyRouteItHas(t *testing.T) {
	scripted := &scriptedProvider{streams: []provider.Stream{textStream("done")}}
	engine, err := newTestEngine(Options{ProviderConfig: ProviderConfig{Provider: scripted, Route: testRoute(t),
		MaxOutputTokens: 128, MaxSteps: 2}, SecurityConfig: SecurityConfig{Workspace: t.TempDir()},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := engine.Run(t.Context(), "hello", nil); err != nil {
		t.Fatal(err)
	}

	if len(scripted.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(scripted.requests))
	}
	if got := scripted.requests[0].Route.Model().ID; got != "model" {
		t.Fatalf("sampled model = %q, want the act model", got)
	}
	if got := scripted.requests[0].MaxOutputTokens; got != 128 {
		t.Fatalf("max output = %d, want the configured 128", got)
	}
}

func TestTurnSamplesOnActAndUsesItsOutputLimit(t *testing.T) {
	act := namedRoute(t, "coder")
	routes, err := model.NewRouteSet(act, map[model.Purpose]model.ReadyRoute{
		model.PurposeSummary: testRoute(t),
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	scripted := &scriptedProvider{streams: []provider.Stream{textStream("done")}}
	engine, err := newTestEngine(Options{ProviderConfig: ProviderConfig{Provider: scripted, Routes: routes,

		// Above the act model's own ceiling, so the clamp is observable.
		MaxOutputTokens: 512, MaxSteps: 2}, SecurityConfig: SecurityConfig{Workspace: t.TempDir(),
		Security: policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass)},
	})
	if err != nil {
		t.Fatal(err)
	}

	var prepared Event
	if _, err := engine.Run(t.Context(), "how would you do it", func(event Event) error {
		if event.State == Preparing {
			prepared = event
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if got := scripted.requests[0].Route.Model().ID; got != "coder" {
		t.Fatalf("sampled model = %q, want coder", got)
	}
	if prepared.Purpose != string(model.PurposeAct) || prepared.Model != "coder" {
		t.Fatalf("prepared = purpose %q model %q", prepared.Purpose, prepared.Model)
	}
	// The act model's ceiling is lower than the session's, and asking for more
	// than a model allows is a provider error rather than a routing story.
	if got := scripted.requests[0].MaxOutputTokens; got != 256 {
		t.Fatalf("max output = %d, want the act model's 256", got)
	}
}

func TestActIgnoresTheSummarySlot(t *testing.T) {
	routes, err := model.NewRouteSet(testRoute(t), map[model.Purpose]model.ReadyRoute{
		model.PurposeSummary: namedRoute(t, "summarizer"),
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	scripted := &scriptedProvider{streams: []provider.Stream{textStream("done")}}
	engine, err := newTestEngine(Options{ProviderConfig: ProviderConfig{Provider: scripted, Routes: routes,

		MaxOutputTokens: 128, MaxSteps: 2}, SecurityConfig: SecurityConfig{Workspace: t.TempDir(),
		Security: policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass)},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := engine.Run(t.Context(), "do it", nil); err != nil {
		t.Fatal(err)
	}

	if got := scripted.requests[0].Route.Model().ID; got != "model" {
		t.Fatalf("sampled model = %q, want the act model", got)
	}
}

func TestRemovedModeFailsBeforeReachingTheProvider(t *testing.T) {
	routes, err := model.NewRouteSet(testRoute(t), nil, true)
	if err != nil {
		t.Fatal(err)
	}
	scripted := &scriptedProvider{streams: []provider.Stream{textStream("done")}}
	engine, err := newTestEngine(Options{ProviderConfig: ProviderConfig{Provider: scripted, Routes: routes,

		MaxOutputTokens: 128, MaxSteps: 2}, SecurityConfig: SecurityConfig{Workspace: t.TempDir(),
		Security: policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass)},
	})
	if err != nil {
		t.Fatal(err)
	}

	engine.options.Security.Mode = "plan"
	var states []State
	_, err = engine.Run(t.Context(), "how would you do it", func(event Event) error {
		states = append(states, event.State)
		return nil
	})

	if err == nil || !strings.Contains(err.Error(), "only act") {
		t.Fatalf("Run() error = %v, want a mode refusal", err)
	}
	if len(scripted.requests) != 0 {
		t.Fatalf("provider was called %d times; the refusal must precede sampling", len(scripted.requests))
	}
	// The turn never announced itself, so there is nothing for a host to render
	// as a turn that started and then went nowhere.
	for _, state := range states {
		if state == Preparing {
			t.Fatal("a refused turn reported Preparing")
		}
	}
}

func TestCostFollowsTheRouteTheTurnActuallyUsed(t *testing.T) {
	routes, err := model.NewRouteSet(namedRoute(t, "coder"), map[model.Purpose]model.ReadyRoute{
		model.PurposeSummary: testRoute(t),
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	scripted := &scriptedProvider{streams: []provider.Stream{
		&providerfixture.SliceStream{Events: []provider.StreamEvent{
			{Type: provider.EventTextDelta, Text: "a plan"},
			{Type: provider.EventUsage, Usage: &provider.Usage{
				InputTokens: 1_000_000, OutputTokens: 0,
			}},
			{Type: provider.EventMessageStop},
		}},
	}}
	engine, err := newTestEngine(Options{ProviderConfig: ProviderConfig{Provider: scripted, Routes: routes,

		MaxOutputTokens: 128, MaxSteps: 2}, SecurityConfig: SecurityConfig{Workspace: t.TempDir(),
		Security: policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass)},
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := engine.Run(t.Context(), "how would you do it", nil)
	if err != nil {
		t.Fatal(err)
	}

	// A million input tokens at the act model's ten dollars per million.
	if result.CostUSD != 10 {
		t.Fatalf("cost = %v, want the act model's price", result.CostUSD)
	}
}
