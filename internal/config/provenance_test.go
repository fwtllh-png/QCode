package config

import "testing"

func TestDefaultProvenanceSources(t *testing.T) {
	provenance := defaultProvenance()
	if len(provenance) == 0 {
		t.Fatal("default provenance is empty")
	}
	for field, source := range provenance {
		if source != SourceDefault {
			t.Fatalf("default provenance[%q] = %q", field, source)
		}
	}
}
