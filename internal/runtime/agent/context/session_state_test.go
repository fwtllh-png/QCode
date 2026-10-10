package agentcontext

import (
	"strings"
	"testing"
)

func TestMandatorySessionStateDropsRefreshableFacts(t *testing.T) {
	capsule := BuildTruthCapsule(TruthProjection{
		Compatibility: Compatibility{SchemaVersion: TruthSchemaVersion},
		ModelID:       "model",
		ContextTokens: 4096,
		Plan: Plan{
			Objective: "keep the goal",
			Steps: []PlanStep{
				{Title: "open work", Status: StepInProgress},
				{Title: "finished work", Status: StepDone},
			},
		},
		Summary: Summary{
			Changes: []CompactionChange{{
				Path: "parser/lex.go", Turn: 1,
			}},
			Facts: []CompactionFact{{Line: "Lex at parser/lex.go:41"}},
		},
		Evidence: EvidenceDelta{
			Facts: []EvidenceFact{{
				Kind: KindDefinition, Path: "parser/lex.go",
				Line: 41, Symbol: "Lex", Tool: "search_definition", Turn: 1,
			}},
		},
	})
	state := MandatorySessionState(capsule)
	if len(state.Entities) == 0 {
		t.Fatal("mandatory session state was empty")
	}
	var sawGoal, sawOpen, sawDone, sawChange, sawFact bool
	for _, entity := range state.Entities {
		switch entity.Kind {
		case EntityGoal:
			sawGoal = strings.Contains(entity.Value, "keep the goal")
		case EntityTodo:
			if entity.Value == "open work" {
				sawOpen = true
			}
			if entity.Value == "finished work" {
				sawDone = true
			}
		case EntityChange:
			sawChange = entity.Key == "parser/lex.go" && !entity.Verified
		case EntityFact:
			sawFact = true
		}
	}
	if !sawGoal || !sawOpen || sawDone || sawChange || sawFact {
		t.Fatalf("session state = %+v", state.Entities)
	}
}

func TestRenderSessionStateIsNotAReplacementArtifact(t *testing.T) {
	capsule := MandatorySessionState(BuildTruthCapsule(TruthProjection{
		Compatibility: Compatibility{SchemaVersion: TruthSchemaVersion},
		ModelID:       "model",
		ContextTokens: 4096,
		Plan:          Plan{Objective: "ship the parser"},
	}))
	rendered, err := RenderSessionState(capsule, 0)
	if err != nil {
		t.Fatal(err)
	}
	if rendered.Text == "" ||
		strings.Contains(rendered.Text, "replaces") ||
		!strings.Contains(rendered.Text, "from ledgers") ||
		!strings.Contains(rendered.Text, TruthMarkerStart) ||
		!strings.Contains(rendered.Text, "ship the parser") {
		t.Fatalf("render = %q", rendered.Text)
	}
}

func TestRenderSessionStateRejectsUndersizedBudget(t *testing.T) {
	capsule := MandatorySessionState(BuildTruthCapsule(TruthProjection{
		Compatibility: Compatibility{SchemaVersion: TruthSchemaVersion},
		ModelID:       "model",
		ContextTokens: 4096,
		Plan:          Plan{Objective: "a mandatory goal that must fit"},
	}))
	_, err := RenderSessionState(capsule, 32)
	if err == nil || !strings.Contains(err.Error(), "session state requires") {
		t.Fatalf("error = %v", err)
	}
}
