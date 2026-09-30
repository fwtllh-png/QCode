package model

import (
	"strings"
	"testing"
)

func TestRequireCapabilitiesNamesWhatIsMissing(t *testing.T) {
	have := Capabilities{Streaming: true, ToolCalls: true}
	err := RequireCapabilities("local", have, []Capability{CapVision, CapToolCalls})
	if err == nil || !strings.Contains(err.Error(), "vision") || !strings.Contains(err.Error(), `"local"`) {
		t.Fatalf("RequireCapabilities() error = %v, want the model and the missing bit", err)
	}
	if err := RequireCapabilities("local", have, []Capability{CapToolCalls}); err != nil {
		t.Fatal(err)
	}
}

func TestPurposeRequiredCapabilitiesOnlyVisionAsksForVision(t *testing.T) {
	if got := PurposeRequiredCapabilities(PurposeVision); len(got) != 1 || got[0] != CapVision {
		t.Fatalf("vision requirements = %v", got)
	}
	for _, purpose := range []Purpose{PurposeAct, PurposeSummary} {
		if got := PurposeRequiredCapabilities(purpose); got != nil {
			t.Fatalf("%s requirements = %v, want none", purpose, got)
		}
	}
}
