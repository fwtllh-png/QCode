package wire

import (
	"context"
	"fmt"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/config"
	"github.com/fwtllh-png/QCode/internal/persist/modelcapability"
	"github.com/fwtllh-png/QCode/internal/persist/state"
)

type routeSetOptions struct {
	// Act is the default route; Additional registers models on that connection.
	Act        execRouteOptions
	Additional map[string]model.Model
	Extras     []ExtraConnectionSpec
	// Slots names purpose overrides; Lock disables fallback to Act.
	Slots map[string]config.RouteSlot
	Lock  bool
}

type runtimeRoutes struct {
	routes     model.RouteSet
	selectable map[string]model.ReadyRoute
}

// resolveRuntimeRoutes derives all routes from one validated connection catalog.
// Each provider/model is resolved and overlaid once, before any consumer sees it.
func resolveRuntimeRoutes(
	ctx context.Context,
	options routeSetOptions,
	store *state.Store,
	trustProbe bool,
) (runtimeRoutes, error) {
	catalog, err := connectionsCatalog(options)
	if err != nil {
		return runtimeRoutes{}, err
	}
	resolver, err := model.NewResolver(catalog)
	if err != nil {
		return runtimeRoutes{}, err
	}
	var probes *modelcapability.Repository
	if store != nil {
		probes = modelcapability.NewRepository(store.SQLite().DB())
	}
	resolved := make(map[string]model.ReadyRoute)
	selectable := make(map[string]model.ReadyRoute)
	allowSelection := len(options.Additional) != 0 || len(options.Extras) != 0
	for _, connection := range catalog.Providers() {
		var base model.ReadyRoute
		for id, descriptor := range connection.Models {
			// Resolve the connection once; every descriptor has already passed
			// catalog validation. This also avoids cloning all sibling models
			// again for each call to Resolver.Resolve.
			if base.ProviderID() == "" {
				base, err = resolver.Resolve(model.RouteRequest{ProviderID: connection.ID, ModelID: id})
				if err != nil {
					return runtimeRoutes{}, err
				}
			}
			route := base.WithModel(descriptor)
			if probes != nil {
				route, err = overlayRouteProbe(ctx, probes, route, trustProbe)
				if err != nil {
					return runtimeRoutes{}, fmt.Errorf("capability probe overlay for %s/%s: %w", connection.ID, id, err)
				}
			}
			key := model.RouteKey(connection.ID, id)
			resolved[key] = route
			_, additional := options.Additional[id]
			if allowSelection && (connection.ID != options.Act.ProviderID || id == options.Act.ModelID || additional) {
				selectable[key] = route
			}
		}
	}
	act := resolved[model.RouteKey(options.Act.ProviderID, options.Act.ModelID)]
	slots := make(map[model.Purpose]model.ReadyRoute, len(options.Slots))
	for name, slot := range options.Slots {
		purpose, err := model.ParsePurpose(name)
		if err != nil {
			return runtimeRoutes{}, err
		}
		route, exists := resolved[model.RouteKey(slot.Provider, slot.Model)]
		if !exists {
			return runtimeRoutes{}, fmt.Errorf("route.%s: slot provider/model must belong to a configured connection: %s/%s", name, slot.Provider, slot.Model)
		}
		if !options.Act.Fixture {
			route = route.WithProvenance(model.ProvenanceConfig)
		}
		slots[purpose] = route
	}
	routes, err := model.NewRouteSet(act, slots, options.Lock)
	if err != nil {
		return runtimeRoutes{}, err
	}
	return runtimeRoutes{routes: routes, selectable: selectable}, nil
}
