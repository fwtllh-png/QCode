package wire

import (
	"context"
	"fmt"
	"reflect"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/persist/modelcapability"
	"github.com/fwtllh-png/QCode/internal/persist/state"
)

// overlayProbeCapabilities applies SQLite probe observations onto every route
// in the set. Without a persistent store the set is returned unchanged: probe
// results only exist beside --data-dir.
func overlayProbeCapabilities(
	ctx context.Context,
	routes model.RouteSet,
	store *state.Store,
	trustProbe bool,
) (model.RouteSet, error) {
	if store == nil || !routes.Ready() {
		return routes, nil
	}
	repo := modelcapability.NewRepository(store.SQLite().DB())
	act, err := overlayRouteProbe(ctx, repo, routes.Act(), trustProbe)
	if err != nil {
		return model.RouteSet{}, err
	}
	slots := make(map[model.Purpose]model.ReadyRoute)
	for _, purpose := range routes.Slots() {
		route, err := routes.For(purpose)
		if err != nil {
			return model.RouteSet{}, err
		}
		overlaid, err := overlayRouteProbe(ctx, repo, route, trustProbe)
		if err != nil {
			return model.RouteSet{}, fmt.Errorf("%s route: %w", purpose, err)
		}
		slots[purpose] = overlaid
	}
	return model.NewRouteSet(act, slots, routes.Locked())
}

func overlayRouteProbe(
	ctx context.Context,
	repo *modelcapability.Repository,
	route model.ReadyRoute,
	trustProbe bool,
) (model.ReadyRoute, error) {
	observations, err := repo.List(
		ctx,
		route.ConnectionID(),
		route.Model().WireID,
	)
	if err != nil {
		return model.ReadyRoute{}, err
	}
	if len(observations) == 0 {
		return route, nil
	}
	current := route.Model().Capabilities
	updated := current
	changedSources := make(map[model.Provenance]struct{}, 2)
	for _, observation := range observations {
		next := model.ApplyProbe(
			updated,
			[]model.CapabilityObservation{observation},
			trustProbe,
		)
		if !reflect.DeepEqual(updated, next) {
			source := model.ProvenanceProviderDiscovery
			if observation.Source == "user" {
				source = model.ProvenanceOperatorConfig
			}
			changedSources[source] = struct{}{}
		}
		updated = next
	}
	if reflect.DeepEqual(current, updated) {
		return route, nil
	}
	provenance := model.ProvenanceMixed
	if len(changedSources) == 1 {
		for source := range changedSources {
			if route.Model().MetadataProvenance.Capabilities == source {
				provenance = source
			}
		}
	}
	return route.WithCapabilitiesFrom(updated, provenance), nil
}
