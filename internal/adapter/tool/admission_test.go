package tool

import "testing"

func TestGuardianAdmissionBindsCatalogGenerationThroughStart(t *testing.T) {
	registry := NewRegistry(nil, nil)
	if err := registry.Register(&countingTool{}); err != nil {
		t.Fatal(err)
	}
	ref, _, _, err := registry.ResolveBoundRef("count", CatalogBinding{})
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.WithCurrentBinding(ref, true, func() error {
		if registry.mu.TryLock() {
			registry.mu.Unlock()
			t.Fatal("catalog replacement can race start")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(&closableTool{}); err != nil {
		t.Fatal(err)
	}
	if err := registry.WithCurrentBinding(ref, true, func() error { t.Fatal("stale Guardian catalog started"); return nil }); err == nil {
		t.Fatal("catalog generation ignored")
	}
	if err := registry.WithCurrentBinding(ref, false, func() error { return nil }); err != nil {
		t.Fatalf("unrelated registration invalidated ordinary human authorization: %v", err)
	}
	ref.Revision++
	if err := registry.WithCurrentBinding(ref, false, func() error { t.Fatal("changed binding started"); return nil }); err == nil {
		t.Fatal("binding revision ignored")
	}
}
