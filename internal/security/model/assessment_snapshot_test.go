package model

import (
	"testing"

	"github.com/fwtllh-png/QCode/internal/security/netpolicy"
)

func TestAssessmentOwnsEveryMutableInput(t *testing.T) {
	fixed := Effect{Kind: NetworkRead, Risk: RiskMedium, Reversibility: Bounded}
	input := AssessmentInput{
		Binding: AssessmentBinding{Capability: CapabilityNetwork, Fixed: &fixed},
		Resources: []Resource{{Class: ClassNetwork, Access: Read,
			Network: &netpolicy.Target{Scheme: "http", Host: "example.com", Port: 80}, Methods: []string{"GET"}}},
	}
	snapshot := Assess(input)
	unchanged := Assess(input)
	fixed.Risk = RiskCritical
	input.Resources[0].Network.Host = "localhost"
	input.Resources[0].Methods[0] = "POST"
	for _, copy := range []AssessmentInput{snapshot.Input(), {Binding: snapshot.Binding(), Resources: snapshot.Resources()}} {
		copy.Binding.Fixed.Kind = ExternalMutation
		copy.Resources[0].Network.Host = "localhost"
		copy.Resources[0].Methods[0] = "POST"
	}
	if !snapshot.Same(unchanged) || snapshot.Effect() != unchanged.Effect() || snapshot.Facets() != unchanged.Facets() {
		t.Fatal("caller mutations changed an assessed invocation")
	}
	got := snapshot.Resources()[0]
	if got.Network.Host != "example.com" || got.Methods[0] != "GET" {
		t.Fatalf("snapshot resources were mutated: %+v", got)
	}
	if snapshot.Same(Assess(input)) || (Assessment{}).Valid() {
		t.Fatal("changed or absent input has the original snapshot identity")
	}
}
