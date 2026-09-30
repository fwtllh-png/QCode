package modelcapability_test

import (
	"path/filepath"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/persist/modelcapability"
	sqlitestate "github.com/fwtllh-png/QCode/internal/persist/state/sqlite"
)

func TestCapabilityRepositoryIsolatesConnectionsWithTheSameModelID(t *testing.T) {
	store, err := sqlitestate.Open(
		t.Context(),
		filepath.Join(t.TempDir(), "state.db"),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	repository := modelcapability.NewRepository(store.DB())
	first := model.CapabilityObservation{
		ConnectionID: "connection-first",
		ModelID:      "shared-model",
		Capability:   model.CapReasoning,
		Supported:    true,
	}
	second := first
	second.ConnectionID = "connection-second"
	second.Supported = false
	if err := repository.Upsert(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	if err := repository.Upsert(t.Context(), second); err != nil {
		t.Fatal(err)
	}

	firstValues, listErr := repository.List(
		t.Context(),
		first.ConnectionID,
		first.ModelID,
	)
	if listErr != nil {
		t.Fatal(listErr)
	}
	secondValues, listErr := repository.List(
		t.Context(),
		second.ConnectionID,
		second.ModelID,
	)
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(firstValues) != 1 || !firstValues[0].Supported ||
		len(secondValues) != 1 || secondValues[0].Supported {
		t.Fatalf("observations leaked: first=%+v second=%+v", firstValues, secondValues)
	}
}
