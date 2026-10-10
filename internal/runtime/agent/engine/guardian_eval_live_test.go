package engine

import (
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/provider/httpclient"
	"github.com/fwtllh-png/QCode/internal/adapter/provider/openai"
	providerrouter "github.com/fwtllh-png/QCode/internal/adapter/provider/router"
	"github.com/fwtllh-png/QCode/internal/security/egress"
)

// An explicit route file carries one operator-approved model.Provider with
// capabilities and a credential reference. No host config is changed, no
// endpoint is inferred from a model name, and no fixture price is reused.
func configureGuardianLiveEvaluation(t *testing.T, e *Engine) {
	t.Helper()
	path := os.Getenv("QCODE_GUARDIAN_EVAL_ROUTE")
	if path == "" {
		t.Fatal("QCODE_GUARDIAN_EVAL_ROUTE is required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("cannot read evaluation route")
	}
	var descriptor model.Provider
	if err := guardianEvalDecode(data, &descriptor); err != nil {
		t.Fatal("invalid evaluation route JSON")
	}
	if len(descriptor.Models) != 1 || descriptor.Provenance == model.ProvenanceFixture {
		t.Fatal("exactly one real model is required")
	}
	timeout, err := time.ParseDuration(os.Getenv("QCODE_GUARDIAN_EVAL_TIMEOUT"))
	if err != nil || timeout <= 0 {
		t.Fatal("explicit positive QCODE_GUARDIAN_EVAL_TIMEOUT is required")
	}
	var output uint64
	if value := os.Getenv("QCODE_GUARDIAN_EVAL_MAX_OUTPUT_TOKENS"); value != "" {
		output, err = strconv.ParseUint(value, 10, 64)
		if err != nil {
			t.Fatal("invalid evaluation output limit")
		}
	}
	catalog, err := model.NewCatalog(descriptor)
	if err != nil {
		t.Fatal("invalid evaluation model metadata")
	}
	resolver, err := model.NewResolver(catalog)
	if err != nil {
		t.Fatal("cannot construct evaluation resolver")
	}
	var modelID string
	for id := range descriptor.Models {
		modelID = id
	}
	route, err := resolver.Resolve(model.RouteRequest{ProviderID: descriptor.ID, ModelID: modelID, Provenance: descriptor.Provenance})
	if err != nil {
		t.Fatal("cannot resolve evaluation route")
	}
	// Resolve only through the production credential API, never log the value
	// or the resolver/provider error body. Missing credentials fail a live run.
	if _, err := httpclient.DefaultCredentials().Resolve(t.Context(), route.Credential()); err != nil {
		t.Fatal("evaluation credential unavailable")
	}
	routes, err := model.NewRouteSet(route, nil, false)
	if err != nil {
		t.Fatal("invalid evaluation route set")
	}
	gate := egress.NewStaticGate()
	if !gate.AllowURL(route.Endpoint()) {
		t.Fatal("evaluation endpoint is not admissible")
	}
	client := httpclient.New()
	client.Egress = gate
	client.IdleTimeout = timeout
	adapter, err := openai.NewAdapter(route.Adapter())
	if err != nil {
		t.Fatal("evaluation adapter unavailable")
	}
	registry, err := providerrouter.NewRegistry(adapter)
	if err != nil {
		t.Fatal("evaluation registry unavailable")
	}
	backend, err := providerrouter.New(registry, routes, client)
	if err != nil {
		t.Fatal("evaluation router unavailable")
	}
	e.options.Provider, e.options.Route, e.options.Routes = backend, route, routes
	e.options.Guardian = GuardianConfig{Enabled: true, Timeout: timeout, MaxOutputTokens: output}
	e.options.SharedRateLimit = NewSharedRateLimit(1)
	e.syncSessionTitleState(provider.Usage{})
}
