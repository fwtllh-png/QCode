package wire

import (
	"fmt"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/config"
)

type routeSetOptions struct {
	// Act resolves the act route, which every purpose without a slot of its own
	// falls back to.
	Act execRouteOptions
	// Additional holds extra models registered on the act connection; they join
	// the connections catalog so slots can route to them.
	Additional map[string]model.Model
	// Extras are the connections beyond the default one; they join the
	// connections catalog for slot resolution.
	Extras []ExtraConnectionSpec
	// Slots is the configured [route.*] table, keyed by purpose name.
	Slots map[string]config.RouteSlot
	Lock  bool
}

// resolveRouteSet resolves the session's whole routing table.
//
// A session without any [route.*] slot produces a set holding only the act
// route, which resolves every purpose to exactly what a single-route session
// used before per-purpose routing existed.
func resolveRouteSet(options routeSetOptions) (model.RouteSet, error) {
	act, err := resolveExecRoute(options.Act)
	if err != nil {
		return model.RouteSet{}, err
	}
	if len(options.Slots) == 0 {
		return model.NewRouteSet(act, nil, options.Lock)
	}
	catalog, err := connectionsCatalog(options.Act, options.Additional, options.Extras)
	if err != nil {
		return model.RouteSet{}, err
	}
	slots := make(map[model.Purpose]model.ReadyRoute, len(options.Slots))
	for name, slot := range options.Slots {
		purpose, err := model.ParsePurpose(name)
		if err != nil {
			return model.RouteSet{}, err
		}
		route, err := resolveSlotRoute(catalog, options.Act, slot, purpose)
		if err != nil {
			return model.RouteSet{}, fmt.Errorf("route.%s: %w", name, err)
		}
		slots[purpose] = route
	}
	return model.NewRouteSet(act, slots, options.Lock)
}

// resolveSlotRoute resolves one purpose's slot against the connections
// catalog (default connection plus every extra connection).
//
// A slot names a provider and a model and nothing else, so it can only reach
// models registered on a configured connection. A fixture session routes every
// purpose through the fixture provider on purpose: letting a slot dial a real
// provider would make a hermetic test's central claim false.
func resolveSlotRoute(
	catalog *model.Catalog, act execRouteOptions, slot config.RouteSlot, purpose model.Purpose,
) (model.ReadyRoute, error) {
	if act.Fixture {
		if slot.Provider != act.ProviderID {
			return model.ReadyRoute{}, fmt.Errorf(
				"a fixture session routes every purpose through the fixture provider %q, not %q",
				act.ProviderID, slot.Provider,
			)
		}
		fixture := act
		fixture.ModelID = slot.Model
		fixture.Model = fixtureModel(slot.Model)
		route, err := resolveExecRoute(fixture)
		if err != nil {
			return model.ReadyRoute{}, err
		}
		if err := model.RequireCapabilities(
			route.Model().ID, route.Model().Capabilities,
			model.PurposeRequiredCapabilities(purpose),
		); err != nil {
			return model.ReadyRoute{}, err
		}
		return route, nil
	}
	resolver, err := model.NewResolver(catalog)
	if err != nil {
		return model.ReadyRoute{}, err
	}
	route, err := resolver.Resolve(model.RouteRequest{
		ProviderID: slot.Provider, ModelID: slot.Model,
		Provenance: model.ProvenanceConfig,
		Require:    model.PurposeRequiredCapabilities(purpose),
	})
	if err != nil {
		return model.ReadyRoute{}, fmt.Errorf(
			"slot provider must be a configured connection: %w", err)
	}
	if slot.Provider == act.ProviderID &&
		(act.Credential.Kind != "" || act.Credential.Name != "") {
		route = route.WithCredential(act.Credential)
	}
	return route, nil
}
