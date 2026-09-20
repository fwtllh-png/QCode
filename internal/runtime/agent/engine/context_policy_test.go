package engine

import (
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
)

func TestContextWindowThresholdsDerivePrepareFromExplicitCompactLimit(t *testing.T) {
	prepare, compact, emergency := agentcontext.WindowThresholds(
		CompactWindowPolicy{AutoTokens: 512},
		3072,
	)
	if prepare != 512 || compact != 512 || emergency != 3072 {
		t.Fatalf(
			"thresholds = (%d, %d, %d), want (512, 512, 3072)",
			prepare,
			compact,
			emergency,
		)
	}
}

func TestDefaultCompactionThresholdsEqualHardInputWithoutOperatorCeiling(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, nil)
	engine.options.MaxOutputTokens = 0
	for _, contextTokens := range []uint64{4096, 64 << 10, 128 << 10, 1 << 20} {
		outputTokens := max(uint64(1), contextTokens/4)
		catalog, err := model.NewCatalog(model.Provider{
			ID: "dynamic-window", Adapter: model.AdapterOpenAICompatible,
			Endpoint:   "http://127.0.0.1:1",
			Protocol:   model.ProtocolOpenAIResponses,
			Provenance: model.ProvenanceFixture,
			Models: map[string]model.Model{"model": {
				ID: "model", CanonicalID: "model", WireID: "model",
				Limits: model.Limits{
					ContextTokens:   contextTokens,
					MaxOutputTokens: outputTokens,
				},
				Capabilities: model.Capabilities{
					Streaming: true, Reasoning: true, ToolCalls: true,
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
		route, err := resolver.Resolve(model.RouteRequest{
			ProviderID: "dynamic-window",
			ModelID:    "model",
		})
		if err != nil {
			t.Fatal(err)
		}
		engine.options.Route = route
		engine.options.Routes, _ = model.NewRouteSet(route, nil, false)

		prepare, compact, emergency := agentcontext.WindowThresholds(
			engine.options.Context.Window,
			engine.contextCapacity().HardInputTokens,
		)
		hardInput := contextTokens - outputTokens
		if prepare != hardInput || compact != hardInput ||
			emergency != hardInput {
			t.Fatalf(
				"context=%d thresholds=(%d,%d,%d), want (%d,%d,%d)",
				contextTokens,
				prepare,
				compact,
				emergency,
				hardInput,
				hardInput,
				hardInput,
			)
		}
	}
}

func TestCompactionThresholdsPreserveExplicitOverrides(t *testing.T) {
	prepare, compact, emergency := agentcontext.WindowThresholds(
		CompactWindowPolicy{
			PrepareTokens:   600,
			AutoTokens:      700,
			EmergencyTokens: 900,
		},
		1000,
	)
	if prepare != 600 || compact != 700 || emergency != 900 {
		t.Fatalf(
			"thresholds = (%d,%d,%d), want explicit (600,700,900)",
			prepare,
			compact,
			emergency,
		)
	}
}

func TestContextCapacityHonorsOperatorAndTokenBudgetCeilings(t *testing.T) {
	route := reasoningRoute(t)
	capacity := agentcontext.ResolveCapacity(route, 12_000, 0, 0)
	if capacity.OutputCeiling != 12_000 ||
		capacity.HardInputTokens != capacity.ContextTokens-12_000 ||
		capacity.OutputSource != "operator_config" {
		t.Fatalf("configured capacity = %+v", capacity)
	}
	capacity = agentcontext.ResolveCapacity(route, 0, 8_000, 0)
	if capacity.OutputCeiling != 8_000 ||
		capacity.HardInputTokens != capacity.ContextTokens-8_000 ||
		capacity.OutputSource != "operator_token_budget" {
		t.Fatalf("budget capacity = %+v", capacity)
	}
}

func TestContextBudgetSnapshotReportsViewContractNotDefaultTiers(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, nil)
	engine.options.Context.Digest = "ledger"
	engine.options.Context.SemanticNarrative = "post_turn"
	snapshot := engine.contextBudgetSnapshot(nil)
	if snapshot.PrepareTokens != 0 || snapshot.EmergencyTokens != 0 {
		t.Fatalf("default snapshot still reports compact tiers: %+v", snapshot)
	}
	if snapshot.RecentTailTurns != agentcontext.DefaultRecentTailTurns ||
		snapshot.Digest != "ledger" ||
		snapshot.NarrativeMode != "post_turn" {
		t.Fatalf("view snapshot = %+v", snapshot)
	}
	engine.options.Context.Window.PrepareTokens = 600
	engine.options.Context.Window.EmergencyTokens = 900
	snapshot = engine.contextBudgetSnapshot(nil)
	if snapshot.PrepareTokens == 0 || snapshot.EmergencyTokens == 0 {
		t.Fatalf("operator snapshot omitted ceilings: %+v", snapshot)
	}
}

func TestRecentTailDefaultUsesTurnBoundNotWindowPercent(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, nil)
	if got := engine.recentTailTurns(); got != agentcontext.DefaultRecentTailTurns {
		t.Fatalf("recent tail turns = %d, want %d", got, agentcontext.DefaultRecentTailTurns)
	}
	if got := engine.recentTailMaxTokens(); got != 0 {
		t.Fatalf("recent tail token ceiling = %d, want 0", got)
	}

	engine.options.Context.RecentTailMaxTokens = 12345
	if got := engine.recentTailMaxTokens(); got != 12345 {
		t.Fatalf("explicit recent tail limit = %d, want 12345", got)
	}
}

func TestRawTailTokenBudgetUsesLeftoverHardInputNotWindowPercent(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, nil)
	engine.options.MaxOutputTokens = 0
	for _, contextTokens := range []uint64{4096, 64 << 10, 128 << 10, 1 << 20} {
		outputTokens := max(uint64(1), contextTokens/4)
		catalog, err := model.NewCatalog(model.Provider{
			ID: "dynamic-window", Adapter: model.AdapterOpenAICompatible,
			Endpoint:   "http://127.0.0.1:1",
			Protocol:   model.ProtocolOpenAIResponses,
			Provenance: model.ProvenanceFixture,
			Models: map[string]model.Model{"model": {
				ID: "model", CanonicalID: "model", WireID: "model",
				Limits: model.Limits{
					ContextTokens:   contextTokens,
					MaxOutputTokens: outputTokens,
				},
				Capabilities: model.Capabilities{
					Streaming: true, Reasoning: true, ToolCalls: true,
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
		route, err := resolver.Resolve(model.RouteRequest{
			ProviderID: "dynamic-window", ModelID: "model",
		})
		if err != nil {
			t.Fatal(err)
		}
		engine.options.Route = route
		engine.options.Routes, _ = model.NewRouteSet(route, nil, false)
		hard := engine.contextCapacity().HardInputTokens
		budget, limited := engine.rawTailTokenBudget(nil)
		if !limited || budget != hard {
			t.Fatalf(
				"context=%d residual=%d limited=%t, want leftover hard input %d",
				contextTokens, budget, limited, hard,
			)
		}
		if budget*5 == hard*4 {
			t.Fatalf("residual reintroduced an 80 percent window gate: %d", budget)
		}
		engine.options.Context.RecentTailMaxTokens = hard / 2
		budget, limited = engine.rawTailTokenBudget(nil)
		if !limited || budget != hard/2 {
			t.Fatalf(
				"operator ceiling = %d limited=%t, want %d",
				budget, limited, hard/2,
			)
		}
		engine.options.Context.RecentTailMaxTokens = 0
	}
}

func TestSummaryBudgetUsesHardInputCapacityUnlessConfigured(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, nil)
	// The default summary byte budget derives from the hard input capacity at
	// dense-script density: three bytes per token.
	if got, want := engine.summaryBudget(),
		int(engine.contextCapacity().HardInputTokens*3); got != want {
		t.Fatalf("summary budget = %d, want %d", got, want)
	}
	engine.options.SummaryMaxBytes = 4096
	if got := engine.summaryBudget(); got != 4096 {
		t.Fatalf("configured summary budget = %d", got)
	}
}

func TestWriteReservationCountTracksDeclaredResources(t *testing.T) {
	if got := writeReservationCount(tool.Descriptor{}, `{}`); got != 1 {
		t.Fatalf("unscoped write reservation count = %d, want one call", got)
	}
	descriptor := tool.Descriptor{
		ResourceResolver: tool.ResourceResolver{
			Templates: []tool.ResourceTemplate{{}, {}, {}},
		},
	}
	if got := writeReservationCount(descriptor, `{}`); got != len(descriptor.ResourceResolver.Templates) {
		t.Fatalf(
			"write reservation count = %d, want declared resources %d",
			got,
			len(descriptor.ResourceResolver.Templates),
		)
	}
	descriptor.ResourceResolver = tool.ResourceResolver{ChangesField: "changes"}
	arguments := `{"changes":[{"path":"a.go"},{"path":"b.go","to":"c.go"}]}`
	if got := writeReservationCount(descriptor, arguments); got != 3 {
		t.Fatalf("transaction reservation count = %d, want 3 paths", got)
	}
	descriptor.ResourceResolver = tool.ResourceResolver{PatchField: "patch"}
	arguments = `{"patch":"--- a/a.go\n+++ b/a.go\n--- /dev/null\n+++ b/new.go\n"}`
	if got := writeReservationCount(descriptor, arguments); got != 2 {
		t.Fatalf("patch reservation count = %d, want 2 paths", got)
	}
}
