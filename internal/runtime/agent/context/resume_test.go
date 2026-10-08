package agentcontext

import (
	"strings"
	"testing"
)

func TestFormatResumeHintEmptyWithoutCompletedWorkOrReads(t *testing.T) {
	if got := FormatResumeHintBudgeted(Plan{Steps: []PlanStep{{
		Title: "still exploring", Status: StepInProgress,
	}}}, nil, 0, nil); got != "" {
		t.Fatalf("hint = %q", got)
	}
	entity, ok := ResumeRetrievalEntityBudgeted(Plan{}, nil, 0, nil)
	if ok || entity.Value != "" {
		t.Fatalf("entity = %+v ok=%v", entity, ok)
	}
}

func TestResumeHintIsMandatoryAndDoesNotInventLists(t *testing.T) {
	plan := Plan{Steps: []PlanStep{
		{Title: "audit multi_paxos_node.h", Status: StepDone},
		{Title: "fix overflow in accept()", Status: StepPending},
	}}
	hint := FormatResumeHintBudgeted(plan, []string{"multi_paxos_node.h", "snapshot_store.h"}, 0, nil)
	if hint == "" ||
		!strings.Contains(hint, "Do not repeat completed plan steps") ||
		!strings.Contains(hint, "Next open work: fix overflow in accept().") ||
		!strings.Contains(hint, "Already-read paths: multi_paxos_node.h, snapshot_store.h.") ||
		!strings.Contains(hint, TurnHistoryToolName) ||
		!strings.Contains(hint, "Absence from the visible tail is not a reason to file_read") ||
		!strings.Contains(hint, "A dirty git status or git_diff is not a reason to file_read") ||
		!strings.Contains(hint, "After search_text returns line hits") ||
		!strings.Contains(hint, "if that output is truncated, call result_get") ||
		strings.Contains(hint, "file may have changed") ||
		strings.Contains(hint, "missing from the visible tail") ||
		strings.Contains(hint, "P2:") {
		t.Fatalf("hint = %q", hint)
	}
	entity, ok := ResumeRetrievalEntityBudgeted(plan, []string{"multi_paxos_node.h"}, 0, nil)
	if !ok || entity.Retention != RetentionMandatory ||
		entity.Source != ResumeSource ||
		entity.Kind != EntityFact ||
		strings.Contains(entity.Value, "P2:") {
		t.Fatalf("entity = %+v ok=%v", entity, ok)
	}
	capsule := MandatorySessionState(BuildTruthCapsule(TruthProjection{
		Compatibility: Compatibility{SchemaVersion: TruthSchemaVersion},
		ModelID:       "model",
		ContextTokens: 4096,
		ExtraEntities: []TruthEntity{entity},
	}))
	if SessionStateResumeHint(capsule) != entity.Value {
		t.Fatalf("capsule hint = %q", SessionStateResumeHint(capsule))
	}
	rendered, err := RenderSessionState(capsule, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rendered.Text, hint[:len("Do not repeat completed plan steps.")]) ||
		!strings.Contains(rendered.Text, "Next open work: fix overflow in accept().") {
		t.Fatalf("session state missing resume hint: %s", rendered.Text)
	}
}

func TestFormatResumeHintIncludesLocatedSites(t *testing.T) {
	plan := Plan{Steps: []PlanStep{
		{Title: "audit", Status: StepDone},
		{Title: "fix overflow in accept()", Status: StepPending},
	}}
	hint := FormatResumeHintBudgeted(
		plan,
		[]string{"paxos_core.cpp"},
		0,
		[]string{"paxos_core.cpp:412", "types.h:88"},
	)
	if !strings.Contains(hint, "Located sites: paxos_core.cpp:412, types.h:88.") ||
		!strings.Contains(hint, "Start file_read at a listed line") ||
		!strings.Contains(hint, "Next open work: fix overflow in accept().") {
		t.Fatalf("hint = %q", hint)
	}
	entity, ok := ResumeRetrievalEntityBudgeted(
		plan,
		nil,
		0,
		[]string{"paxos_core.cpp:412"},
	)
	if !ok || !strings.Contains(entity.Value, "Located sites: paxos_core.cpp:412.") {
		t.Fatalf("entity = %+v ok=%v", entity, ok)
	}
}

func TestFormatResumeHintBudgetOmitsOverflowingPaths(t *testing.T) {
	plan := Plan{Steps: []PlanStep{
		{Title: "audit", Status: StepDone},
		{Title: "fix overflow in accept()", Status: StepPending},
	}}
	paths := make([]string, 0, 20)
	for index := 0; index < 20; index++ {
		paths = append(paths, "internal/pkg/already_read_"+string(rune('a'+index))+".go")
	}
	full := FormatResumeHintBudgeted(plan, paths, 0, nil)
	budget := len(full) - 1
	if budget < 200 {
		t.Fatalf("full hint too small to exercise budget: %d", len(full))
	}
	hint := FormatResumeHintBudgeted(plan, paths, budget, nil)
	if hint == "" || len(hint) > budget {
		t.Fatalf("budgeted hint = %q len=%d budget=%d", hint, len(hint), budget)
	}
	if !strings.Contains(hint, "more already-read paths omitted") ||
		!strings.Contains(hint, "Already-read paths:") ||
		!strings.Contains(hint, "Do not file_read those paths again") {
		t.Fatalf("budgeted hint = %q", hint)
	}
	entity, ok := ResumeRetrievalEntityBudgeted(plan, paths, budget, nil)
	if !ok || entity.Value != hint || entity.Retention != RetentionMandatory {
		t.Fatalf("entity = %+v ok=%v", entity, ok)
	}
}
