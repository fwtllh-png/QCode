package environment

import "testing"

func TestPreparationFactsSnapshotIsolation(t *testing.T) {
	facts := []Fact{{Source: SourcePreparer, Resource: "parent"}}
	parent := WithPreparationFacts(t.Context(), facts)
	facts[0].Resource = "mutated input"
	read := PreparationFactsFrom(parent)
	read[0].Resource = "mutated read"
	child := WithPreparationFacts(parent, []Fact{{Resource: "child"}})
	empty := WithPreparationFacts(parent, nil)
	if PreparationFactsFrom(parent)[0].Resource != "parent" ||
		PreparationFactsFrom(child)[0].Resource != "child" ||
		len(PreparationFactsFrom(empty)) != 0 || len(PreparationFactsFrom(t.Context())) != 0 {
		t.Fatal("preparation snapshots leaked across callers or environments")
	}
}
