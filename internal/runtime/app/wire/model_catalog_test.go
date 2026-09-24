package wire

import (
	"slices"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func testConnectionRoute(t *testing.T, baseline string) model.ReadyRoute {
	t.Helper()
	descriptor := testCustomModel(baseline)
	route, err := resolveExecRoute(execRouteOptions{
		ProviderID: "openai-compatible:abc123", ModelID: baseline,
		BaseURL:  "https://models.example.com/v1",
		Protocol: model.ProtocolOpenAIChat, Model: &descriptor,
		Credential: model.CredentialRef{Kind: "keyring", Name: "web/test/custom"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return route
}

func TestRuntimeModelCatalogListsOnlyConfiguredConnections(t *testing.T) {
	selected := testConnectionRoute(t, "model-a")
	additional := testCustomModel("model-b")
	selectable, err := runtimeSelectableRoutes(
		selected,
		map[string]model.Model{additional.ID: additional},
	)
	if err != nil {
		t.Fatal(err)
	}
	capabilities := selectedModelCapabilities(selected)
	providers, models := runtimeModelCatalog(selected, capabilities, selectable)

	for _, provider := range providers.Providers {
		if provider.ID != selected.ProviderID() {
			t.Fatalf("unexpected provider = %+v", provider)
		}
		if provider.Availability != "available" || !provider.Selected {
			t.Fatalf("selected provider = %+v", provider)
		}
	}
	if len(models.Models) != 2 {
		t.Fatalf("model entries = %+v", models.Models)
	}
	for _, entry := range models.Models {
		if entry.Capabilities.SelectionMode != "hot" ||
			entry.Capabilities.Availability != "available" {
			t.Fatalf("registered model = %+v", entry)
		}
	}
}

func TestRuntimeModelsShareSelectableConnection(t *testing.T) {
	selected := testConnectionRoute(t, "model-a")
	additional := testCustomModel("model-b")
	selectable, err := runtimeSelectableRoutes(
		selected,
		map[string]model.Model{additional.ID: additional},
	)
	if err != nil {
		t.Fatal(err)
	}
	capabilities := selectedModelCapabilities(selected)
	_, catalog := runtimeModelCatalog(selected, capabilities, selectable)
	profiles, mutable := runtimeProfileModels(catalog, selected.ProviderID(), capabilities)
	if !slices.Contains(mutable, "model") {
		t.Fatalf("model selection is not mutable: %v", mutable)
	}
	for _, id := range []string{"model-a", "model-b"} {
		key := model.RouteKey(selected.ProviderID(), id)
		route, ok := selectable[key]
		if !ok {
			t.Fatalf("model %q is not selectable", id)
		}
		if route.ConnectionID() != selected.ConnectionID() ||
			route.Credential() != selected.Credential() ||
			route.Model().WireID != id {
			t.Fatalf("model %q changed connection or wire ID: %+v", id, route)
		}
		profile, ok := profiles[key]
		if !ok || profile.Availability != "available" ||
			profile.SelectionMode != "hot" {
			t.Fatalf("model %q is missing from the selectable profile catalog: %+v", id, profile)
		}
	}
}

func TestRuntimeProfileProviderMutableOnlyWithSelectableConnections(t *testing.T) {
	for _, test := range []struct {
		name         string
		provider     string
		availability string
		selection    string
		baseline     string
		wantMutable  bool
	}{
		{"another connection", "connection-b", "available", "hot", "hot", true},
		{"same connection", "connection-a", "available", "hot", "hot", false},
		{"unavailable connection", "connection-b", "unavailable", "hot", "hot", false},
		{"restart required", "connection-b", "available", "restart_required", "hot", false},
		{"fixed target", "connection-b", "available", "fixed", "hot", false},
		{"fixed baseline", "connection-b", "available", "hot", "fixed", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			catalog := protocol.ModelCatalog{
				Models: []protocol.ModelCatalogEntry{{
					Provider: test.provider,
					ID:       "model-b",
					Capabilities: protocol.ModelCapabilities{
						Availability:  test.availability,
						SelectionMode: test.selection,
					},
				}},
			}
			_, mutable := runtimeProfileModels(
				catalog, "connection-a",
				protocol.ModelCapabilities{SelectionMode: test.baseline},
			)
			if got := slices.Contains(mutable, "provider"); got != test.wantMutable {
				t.Fatalf("mutable provider = %v, want %v; fields = %v", got, test.wantMutable, mutable)
			}
		})
	}
}

func TestRuntimeSelectableRoutesKeepsCustomRouteFixed(t *testing.T) {
	selected := testConnectionRoute(t, "future-model")
	selectable, err := runtimeSelectableRoutes(selected, nil)
	if err != nil {
		t.Fatal(err)
	}
	capabilities := selectedModelCapabilities(selected)
	capabilities.SelectionMode = "fixed"
	_, models := runtimeModelCatalog(
		selected, capabilities,
		selectable,
	)
	for _, entry := range models.Models {
		if !entry.Selected {
			continue
		}
		if entry.Capabilities.SelectionMode != "fixed" ||
			entry.Source != "connection_baseline" ||
			entry.Capabilities.MetadataProvenance.Limits !=
				string(model.ProvenanceOperatorConfig) {
			t.Fatalf("selected fixed model = %+v", entry)
		}
	}
	profiles, mutable := runtimeProfileModels(models, selected.ProviderID(), capabilities)
	if len(profiles) != 0 || len(mutable) != 0 {
		t.Fatalf("fixed route profiles=%+v mutable=%v", profiles, mutable)
	}
}

func TestRuntimeSelectableRoutesAddsCustomModelsWithoutReplacingBaseline(t *testing.T) {
	selected := testConnectionRoute(t, "model-a")
	additional := testCustomModel("model-b")
	selectable, err := runtimeSelectableRoutes(
		selected,
		map[string]model.Model{additional.ID: additional},
	)
	if err != nil {
		t.Fatal(err)
	}
	capabilities := selectedModelCapabilities(selected)
	capabilities.SelectionMode = "hot"
	_, models := runtimeModelCatalog(selected, capabilities, selectable)
	profiles, mutable := runtimeProfileModels(
		models,
		selected.ProviderID(),
		capabilities,
	)
	for _, id := range []string{"model-a", "model-b"} {
		key := model.RouteKey(selected.ProviderID(), id)
		if _, ok := selectable[key]; !ok {
			t.Fatalf("selectable route %q is missing", id)
		}
		if _, ok := profiles[key]; !ok {
			t.Fatalf(
				"profile model %q is missing: selectable=%+v models=%+v profiles=%+v",
				id,
				selectable,
				models.Models,
				profiles,
			)
		}
	}
	if !slices.Contains(mutable, "model") {
		t.Fatalf("mutable fields = %v", mutable)
	}
}

func testCustomModel(id string) model.Model {
	return model.Model{
		ID: id, CanonicalID: id, WireID: id,
		Limits: model.Limits{ContextTokens: 200_000, MaxOutputTokens: 24_000},
		Capabilities: model.Capabilities{
			Streaming: true, ToolCalls: true,
		},
		Pricing: model.Pricing{Provenance: model.ProvenanceOperatorConfig},
		MetadataProvenance: model.MetadataProvenance{
			CanonicalID:  model.ProvenanceOperatorConfig,
			WireID:       model.ProvenanceOperatorConfig,
			Limits:       model.ProvenanceOperatorConfig,
			Capabilities: model.ProvenanceOperatorConfig,
			Pricing:      model.ProvenanceOperatorConfig,
		},
		Provenance: model.ProvenanceOperatorConfig,
	}
}
