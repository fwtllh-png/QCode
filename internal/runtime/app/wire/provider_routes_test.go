package wire

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/config"
	"github.com/fwtllh-png/QCode/internal/persist/modelcapability"
	"github.com/fwtllh-png/QCode/internal/persist/state"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	"github.com/fwtllh-png/QCode/internal/security/egress"
)

func TestProviderRoutesSwitchBackToDefaultConnection(t *testing.T) {
	workspace, providerID, modelID := t.TempDir(), "connection-a", "model-a"
	wireProtocol := string(model.ProtocolOpenAIChat)
	var gate *egress.Gate
	modules := append(defaultBuildModules(), buildModuleFunc{
		name: "capture-provider", fn: func(_ context.Context, state *buildState) error {
			gate = state.provider.egress
			return nil
		},
	})
	session, err := newExec(t.Context(), withNonDurableTestJournal(t, ExecOptions{
		BaseURL:       "https://a.example.com/v1",
		ModelMetadata: ModelMetadataOptions{Descriptor: fixtureModel(modelID)},
		ExtraConnections: []ExtraConnectionSpec{{
			ProviderID: "connection-b", BaseURL: "https://b.example.com/v1",
			Protocol: model.ProtocolOpenAIResponses, Model: fixtureModel("model-b"),
			Credential: model.CredentialRef{Kind: "keyring", Name: "test/connection-b"},
		}},
		ConfigOverrides: config.Overrides{
			Workspace: &workspace, Provider: &providerID, Model: &modelID, Protocol: &wireProtocol,
		},
		Skills: SkillOptions{UserHome: t.TempDir(), DataDir: t.TempDir()},
	}), modules)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close(context.Background()) })
	if !gate.Allowed("a.example.com", "https") || !gate.Allowed("b.example.com", "https") ||
		gate.Allowed("unconfigured.example.com", "https") {
		t.Fatal("provider egress grants do not match the configured connections")
	}
	if _, err := session.threads.History("route-switch"); err != nil {
		t.Fatal(err)
	}
	engine, err := session.threads.ContextEngine("route-switch")
	if err != nil {
		t.Fatal(err)
	}
	baseline := engine.OptionsSeed().Routes.Act()
	selectable := engine.OptionsSeed().SelectableRoutes
	profiles, mutable := runtimeProfileModels(session.modelCatalog, providerID, session.modelCapabilities)
	if len(selectable) != 2 || len(profiles) != 2 || !slices.Contains(mutable, "provider") {
		t.Fatalf("selectable=%d profiles=%d mutable=%v", len(selectable), len(profiles), mutable)
	}
	for _, entry := range session.modelCatalog.Models {
		key := model.RouteKey(entry.Provider, entry.ID)
		if _, ok := selectable[key]; !ok || entry.Capabilities.SelectionMode != "hot" {
			t.Fatalf("catalog advertises a model unavailable to the engine: %+v", entry)
		}
	}
	for i, target := range []struct{ provider, model, endpoint string }{
		{"connection-b", "model-b", "https://b.example.com/v1"},
		{providerID, modelID, "https://a.example.com/v1"},
	} {
		profile := protocol.SessionProfile{
			Version: protocol.SessionProfileVersion, Revision: uint64(i + 2),
			Mode: "act", Provider: target.provider, Model: target.model,
			ApprovalPosture: "suggest", ExecutionTarget: "local",
			MaxSteps: 8, PromptCacheRevision: 2,
		}
		if err := engine.ApplySessionProfile(profile); err != nil {
			t.Fatalf("switch to %s/%s: %v", target.provider, target.model, err)
		}
		route := engine.OptionsSeed().Routes.Act()
		if route.Endpoint() != target.endpoint || route.Model().ID != target.model {
			t.Fatalf("selected route = %+v", route)
		}
		if !reflect.DeepEqual(route, selectable[model.RouteKey(target.provider, target.model)]) {
			t.Fatalf("switch changed the registered route: %+v", route)
		}
	}
	if got := engine.OptionsSeed().Routes.Act(); got.Credential() != baseline.Credential() {
		t.Fatalf("default credential changed: %+v", got.Credential())
	}
}

func TestRuntimeRoutesPreserveCredentialPrecedenceAcrossConsumers(t *testing.T) {
	for _, explicit := range []model.CredentialRef{
		{}, {Kind: "env", Name: "EXPLICIT_KEY"}, {Kind: "keyring", Name: "test/connection"},
	} {
		t.Run(explicit.Kind, func(t *testing.T) {
			act := bundledAct()
			act.APIKeyEnv, act.Credential = "FALLBACK_KEY", explicit
			additional := testCustomModel("additional")
			resolved, err := resolveRuntimeRoutes(t.Context(), routeSetOptions{
				Act: act, Additional: map[string]model.Model{additional.ID: additional},
				Slots: map[string]config.RouteSlot{"summary": {Provider: act.ProviderID, Model: additional.ID}},
			}, nil, false)
			if err != nil {
				t.Fatal(err)
			}
			want := explicit
			if want == (model.CredentialRef{}) {
				want = model.CredentialRef{Kind: "env", Name: "FALLBACK_KEY"}
			}
			summary, err := resolved.routes.For(model.PurposeSummary)
			if err != nil {
				t.Fatal(err)
			}
			for _, route := range []model.ReadyRoute{
				resolved.routes.Act(), summary,
				resolved.selectable[model.RouteKey(act.ProviderID, additional.ID)],
			} {
				if route.Credential() != want || route.Endpoint() != act.BaseURL {
					t.Fatalf("connection identity changed: %+v", route)
				}
			}
		})
	}
}

func TestRuntimeRoutesKeepBaselineMetadataConsistent(t *testing.T) {
	act := bundledAct()
	duplicate := testCustomModel(act.ModelID)
	resolved, err := resolveRuntimeRoutes(t.Context(), routeSetOptions{
		Act: act, Additional: map[string]model.Model{duplicate.ID: duplicate},
		Slots: map[string]config.RouteSlot{"summary": {Provider: act.ProviderID, Model: act.ModelID}},
	}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	summary, err := resolved.routes.For(model.PurposeSummary)
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range []model.ReadyRoute{
		resolved.routes.Act(), summary, resolved.selectable[model.RouteKey(act.ProviderID, act.ModelID)],
	} {
		if !reflect.DeepEqual(route.Model(), *act.Model) {
			t.Fatalf("baseline metadata was replaced: %+v", route.Model())
		}
	}
	if resolved.routes.Act().Provenance() != model.ProvenanceStartup || summary.Provenance() != model.ProvenanceConfig {
		t.Fatal("route selection provenance was lost")
	}
}

func TestRuntimeRoutesShareProbeCapabilitiesAcrossConsumers(t *testing.T) {
	store, err := state.Open(t.Context(), state.Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.CloseAll(context.Background()) })
	act := bundledAct()
	additional := *fixtureModel("additional")
	additional.WireID = "additional-wire"
	extra := *fixtureModel(act.ModelID)
	extra.WireID = "extra-wire"
	options := routeSetOptions{
		Act: act, Additional: map[string]model.Model{additional.ID: additional},
		Extras: []ExtraConnectionSpec{{
			ProviderID: "extra", BaseURL: "https://extra.example.com/v1",
			Protocol: model.ProtocolOpenAIChat, Model: &extra,
		}},
		Slots: map[string]config.RouteSlot{"summary": {Provider: act.ProviderID, Model: additional.ID}},
		Lock:  true,
	}
	initial, err := resolveRuntimeRoutes(t.Context(), options, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	repo := modelcapability.NewRepository(store.SQLite().DB())
	for _, route := range initial.selectable {
		if err := repo.Upsert(t.Context(), model.CapabilityObservation{
			ConnectionID: route.ConnectionID(), ModelID: route.Model().WireID,
			Capability: model.CapReasoning, Supported: false, Source: "probe",
		}); err != nil {
			t.Fatal(err)
		}
	}
	resolved, err := resolveRuntimeRoutes(t.Context(), options, store, false)
	if err != nil {
		t.Fatal(err)
	}
	capabilities := selectedModelCapabilities(resolved.routes.Act())
	capabilities.SelectionMode = "hot"
	_, catalog := runtimeModelCatalog(resolved.routes.Act(), capabilities, resolved.selectable)
	profiles, _ := runtimeProfileModels(catalog, act.ProviderID, capabilities)
	if len(catalog.Models) != 3 || len(profiles) != 3 {
		t.Fatalf("catalog=%+v profiles=%+v", catalog, profiles)
	}
	for _, entry := range catalog.Models {
		key := model.RouteKey(entry.Provider, entry.ID)
		route := resolved.selectable[key]
		if route.Model().Capabilities.Reasoning || entry.Capabilities.Reasoning || profiles[key].Reasoning ||
			len(entry.Capabilities.ReasoningEfforts) != 0 || entry.Capabilities.DefaultReasoningEffort != "" ||
			entry.Capabilities.MetadataProvenance.Capabilities != string(model.ProvenanceMixed) {
			t.Fatalf("probe result lost for %q: route=%+v catalog=%+v", key, route, entry)
		}
	}
	if !reflect.DeepEqual(resolved.routes.Act(), resolved.selectable[model.RouteKey(act.ProviderID, act.ModelID)]) {
		t.Fatal("act and selectable baseline disagree")
	}
	summary, err := resolved.routes.For(model.PurposeSummary)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(summary.Model(), resolved.selectable[model.RouteKey(act.ProviderID, additional.ID)].Model()) ||
		summary.Provenance() != model.ProvenanceConfig || !resolved.routes.Locked() {
		t.Fatalf("summary lost metadata, provenance or lock: %+v", summary)
	}
	if !additional.Capabilities.Reasoning || !act.Model.Capabilities.Reasoning || !extra.Capabilities.Reasoning {
		t.Fatal("probe overlay mutated the input descriptors")
	}
}

func TestRuntimeRoutesValidateVisionAfterProbeOverlay(t *testing.T) {
	store, err := state.Open(t.Context(), state.Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.CloseAll(context.Background()) })
	act := bundledAct()
	base, err := resolveExecRoute(act)
	if err != nil {
		t.Fatal(err)
	}
	repo := modelcapability.NewRepository(store.SQLite().DB())
	for _, supported := range []bool{false, true} {
		if err := repo.Upsert(t.Context(), model.CapabilityObservation{
			ConnectionID: base.ConnectionID(), ModelID: base.Model().WireID,
			Capability: model.CapVision, Supported: supported, Source: "probe",
		}); err != nil {
			t.Fatal(err)
		}
		if supported {
			act.Model.Capabilities.Vision = false
		}
		for _, trust := range []bool{false, true} {
			_, err := resolveRuntimeRoutes(t.Context(), routeSetOptions{
				Act: act, Slots: map[string]config.RouteSlot{"vision": {Provider: act.ProviderID, Model: act.ModelID}},
			}, store, trust)
			if supported && trust {
				if err != nil {
					t.Fatalf("trusted positive observation did not enable vision: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "vision") {
				t.Fatalf("supported=%v trust=%v: error=%v", supported, trust, err)
			}
		}
	}
}

func TestRuntimeRoutesKeepFixtureOnlySlotsOutOfSelection(t *testing.T) {
	for _, hot := range []bool{false, true} {
		options := routeSetOptions{
			Act: execRouteOptions{
				ProviderID: "fixture", ModelID: "fixture-model", BaseURL: "http://127.0.0.1:1",
				Protocol: model.ProtocolOpenAIChat, Fixture: true, Model: fixtureModel("fixture-model"),
			},
			Slots: map[string]config.RouteSlot{"summary": {Provider: "fixture", Model: "summary-only"}},
		}
		if hot {
			options.Additional = map[string]model.Model{"additional": *fixtureModel("additional")}
		}
		resolved, err := resolveRuntimeRoutes(t.Context(), options, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		summary, err := resolved.routes.For(model.PurposeSummary)
		if err != nil {
			t.Fatal(err)
		}
		if summary.Model().ID != "summary-only" || summary.Provenance() != model.ProvenanceFixture {
			t.Fatalf("fixture summary route=%+v", summary)
		}
		if _, exists := resolved.selectable[model.RouteKey("fixture", "summary-only")]; exists {
			t.Fatal("fixture-only slot implicitly became selectable")
		}
		if (len(resolved.selectable) != 0) != hot {
			t.Fatalf("hot=%v selectable=%+v", hot, resolved.selectable)
		}
	}
}

func TestRuntimeRoutesValidateCatalogWithoutSlots(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*routeSetOptions)
	}{
		{"duplicate connection", func(o *routeSetOptions) {
			o.Extras = []ExtraConnectionSpec{{
				ProviderID: o.Act.ProviderID, BaseURL: "https://other.example.com/v1",
				Protocol: model.ProtocolOpenAIChat, Model: fixtureModel("other"),
			}}
		}},
		{"invalid additional protocol capability", func(o *routeSetOptions) {
			additional := *fixtureModel("additional")
			additional.Capabilities.IncrementalResponses = true
			o.Additional = map[string]model.Model{additional.ID: additional}
		}},
		{"invalid additional identity", func(o *routeSetOptions) {
			o.Additional = map[string]model.Model{"wrong-key": *fixtureModel("additional")}
		}},
		{"invalid explicit credential", func(o *routeSetOptions) {
			o.Act.Credential = model.CredentialRef{Kind: "unsupported", Name: "test"}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			options := routeSetOptions{Act: bundledAct()}
			test.mutate(&options)
			if _, err := resolveRuntimeRoutes(t.Context(), options, nil, false); err == nil {
				t.Fatal("invalid connection catalog was accepted without slots")
			}
		})
	}
}
