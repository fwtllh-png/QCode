package wire

import (
	"context"
	"reflect"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/persist/modelcapability"
)

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
