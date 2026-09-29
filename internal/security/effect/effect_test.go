package effect

import "testing"

func TestEffectValidateRejectsUnknownVocabulary(t *testing.T) {
	valid := Effect{Kind: NetworkRead, Risk: RiskMedium, Reversibility: Bounded}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []Effect{
		{Kind: "network.write", Risk: RiskMedium, Reversibility: Bounded},
		{Kind: NetworkRead, Risk: "severe", Reversibility: Bounded},
		{Kind: NetworkRead, Risk: RiskMedium, Reversibility: "undoable"},
		{},
	} {
		if err := invalid.Validate(); err == nil {
			t.Fatalf("%+v was accepted", invalid)
		}
	}
}
