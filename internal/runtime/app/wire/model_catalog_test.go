package wire

import (
	"slices"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
)

func TestRuntimeModelCatalogMarksOnlySelectableProviderModelsHot(t *testing.T) {
	resolver, err := model.NewResolver(model.DefaultCatalog())
	if err != nil {
		t.Fatal(err)
	}
	selected, err := resolver.Resolve(model.RouteRequest{
		ProviderID: "deepseek",
		ModelID:    "deepseek-chat",
	})
	if err != nil {
		t.Fatal(err)
	}
	selectable, err := runtimeSelectableRoutes(selected, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	providers, models := runtimeModelCatalog(
		selected,
		selectedModelCapabilities(selected),
		selectable,
	)

	for _, provider := range providers.Providers {
		if provider.ID == "deepseek" {
			if provider.Availability != "available" {
				t.Fatalf("selected provider = %+v", provider)
			}
			continue
		}
		if provider.Availability != "unavailable" || provider.Reason == "" {
			t.Fatalf("non-selected provider = %+v", provider)
		}
	}
	hot := 0
	for _, entry := range models.Models {
		if entry.Provider == "deepseek" {
			if entry.Capabilities.SelectionMode != "hot" ||
				entry.Capabilities.Availability != "available" {
				t.Fatalf("deepseek model = %+v", entry)
			}
			hot++
		}
	}
	if hot != 2 {
		t.Fatalf("hot deepseek models = %d", hot)
	}
}

func TestRuntimeGLMModelsShareSelectableConnection(t *testing.T) {
	resolver, err := model.NewResolver(model.DefaultCatalog())
	if err != nil {
		t.Fatal(err)
	}
	modelIDs := []string{"glm-5.3", "glm-5.3-flash", "glm-5.3-flashx"}
	for _, selectedID := range modelIDs {
		t.Run(selectedID, func(t *testing.T) {
			selected, err := resolver.Resolve(model.RouteRequest{
				ProviderID: "glm", ModelID: selectedID,
			})
			if err != nil {
				t.Fatal(err)
			}
			selectable, err := runtimeSelectableRoutes(selected, true, nil)
			if err != nil {
				t.Fatal(err)
			}
			capabilities := selectedModelCapabilities(selected)
			_, catalog := runtimeModelCatalog(selected, capabilities, selectable)
			profiles, mutable := runtimeProfileModels(catalog, "glm", capabilities)
			if !slices.Contains(mutable, "model") {
				t.Fatalf("model selection is not mutable: %v", mutable)
			}
			for _, id := range modelIDs {
				key := model.RouteKey("glm", id)
				route, ok := selectable[key]
				if !ok {
					t.Fatalf("GLM model %q is not selectable from %q", id, selectedID)
				}
				if route.ConnectionID() != selected.ConnectionID() ||
					route.Credential() != selected.Credential() ||
					route.Model().WireID != id {
					t.Fatalf("GLM model %q changed connection or wire ID: %+v", id, route)
				}
				profile, ok := profiles[key]
				if !ok || profile.Availability != "available" ||
					profile.SelectionMode != "hot" {
					t.Fatalf("GLM model %q is missing from the selectable profile catalog: %+v", id, profile)
				}
			}
		})
	}
}

func TestRuntimeSelectableRoutesKeepsCustomRouteFixed(t *testing.T) {
	descriptor := &model.Model{
		ID: "future-model", CanonicalID: "vendor/future-model",
		WireID:       "future-model",
		Limits:       model.Limits{ContextTokens: 200_000, MaxOutputTokens: 24_000},
		Capabilities: model.Capabilities{Streaming: true, ToolCalls: true},
		MetadataProvenance: model.MetadataProvenance{
			CanonicalID:  model.ProvenanceOperatorConfig,
			WireID:       model.ProvenanceOperatorConfig,
			Limits:       model.ProvenanceOperatorConfig,
			Capabilities: model.ProvenanceOperatorConfig,
		},
		Provenance: model.ProvenanceOperatorConfig,
	}
	selected, err := resolveExecRoute(execRouteOptions{
		ProviderID: "openai-compatible", ModelID: "future-model",
		BaseURL:  "https://models.example.com/v1",
		Protocol: model.ProtocolOpenAIChat, Model: descriptor,
	})
	if err != nil {
		t.Fatal(err)
	}
	selectable, err := runtimeSelectableRoutes(selected, false, nil)
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
	baseline := testCustomModel("model-a")
	selected, err := resolveExecRoute(execRouteOptions{
		ProviderID: "openai-compatible", ModelID: baseline.ID,
		BaseURL:  "https://models.example.com/v1",
		Protocol: model.ProtocolOpenAIChat, Model: &baseline,
	})
	if err != nil {
		t.Fatal(err)
	}
	additional := testCustomModel("model-b")
	selectable, err := runtimeSelectableRoutes(
		selected,
		false,
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
