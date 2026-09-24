package model

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestReadyRouteGolden(t *testing.T) {
	resolver, err := NewResolver(testCatalog(t))
	if err != nil {
		t.Fatal(err)
	}
	route, err := resolver.Resolve(RouteRequest{
		ProviderID: "openai",
		ModelID:    "gpt-4.1",
		Provenance: ProvenanceStartup,
	})
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := route.Describe()
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.MarshalIndent(descriptor, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	want, err := os.ReadFile(filepath.Join("testdata", "route.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("route golden mismatch\nGOT:\n%s\nWANT:\n%s", got, want)
	}
}
