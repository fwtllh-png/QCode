package wire

import (
	"path/filepath"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/persist/modelcapability"
	sqlitestate "github.com/fwtllh-png/QCode/internal/persist/state/sqlite"
)

func TestProbeOverlayUsesWireIDAndReportsMixedProvenance(t *testing.T) {
	store, err := sqlitestate.Open(
		t.Context(),
		filepath.Join(t.TempDir(), "state.db"),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	route := probeTestRoute(t, "logical-model", "wire-current")
	repository := modelcapability.NewRepository(store.DB())
	if err := repository.Upsert(t.Context(), model.CapabilityObservation{
		ConnectionID: route.ConnectionID(),
		ModelID:      "wire-previous",
		Capability:   model.CapReasoning,
		Supported:    false,
		Source:       "probe",
	}); err != nil {
		t.Fatal(err)
	}
	unchanged, err := overlayRouteProbe(
		t.Context(),
		repository,
		route,
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !unchanged.Model().Capabilities.Reasoning {
		t.Fatal("observation for another wire model leaked into the route")
	}
	if err := repository.Upsert(t.Context(), model.CapabilityObservation{
		ConnectionID: route.ConnectionID(),
		ModelID:      route.Model().WireID,
		Capability:   model.CapReasoning,
		Supported:    false,
		Source:       "probe",
	}); err != nil {
		t.Fatal(err)
	}
	overlaid, err := overlayRouteProbe(
		t.Context(),
		repository,
		route,
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	capabilities := overlaid.Model().Capabilities
	if capabilities.Reasoning ||
		len(capabilities.ReasoningEfforts) != 0 ||
		capabilities.DefaultReasoningEffort != "" {
		t.Fatalf("overlaid capabilities = %+v", capabilities)
	}
	if overlaid.Model().MetadataProvenance.Capabilities !=
		model.ProvenanceMixed {
		t.Fatalf(
			"capability provenance = %q",
			overlaid.Model().MetadataProvenance.Capabilities,
		)
	}
}

func probeTestRoute(
	t *testing.T,
	modelID string,
	wireID string,
) model.ReadyRoute {
	t.Helper()
	catalog, err := model.NewCatalog(model.Provider{
		ID: "custom", Adapter: model.AdapterOpenAICompatible,
		Endpoint:   "https://models.example.com/v1",
		Protocol:   model.ProtocolOpenAIChat,
		Provenance: model.ProvenanceOperatorConfig,
		Models: map[string]model.Model{modelID: {
			ID: modelID, CanonicalID: "vendor/" + modelID, WireID: wireID,
			Limits: model.Limits{ContextTokens: 4096, MaxOutputTokens: 1024},
			Capabilities: model.Capabilities{
				Streaming: true, Reasoning: true,
				ReasoningEfforts:       []string{"off", "high"},
				DefaultReasoningEffort: "high",
			},
			Pricing: model.Pricing{
				Provenance: model.ProvenanceOperatorConfig,
			},
			MetadataProvenance: model.MetadataProvenance{
				CanonicalID:  model.ProvenanceOperatorConfig,
				WireID:       model.ProvenanceOperatorConfig,
				Limits:       model.ProvenanceOperatorConfig,
				Capabilities: model.ProvenanceOperatorConfig,
				Pricing:      model.ProvenanceOperatorConfig,
			},
			Provenance: model.ProvenanceOperatorConfig,
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
		ProviderID: "custom",
		ModelID:    modelID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return route
}
