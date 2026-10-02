package app

import "testing"

func TestValidateStructuredPlanRejectsMarkdownAndRequiresRevision(t *testing.T) {
	if err := validateStructuredPlan("1. Update parser", false); err == nil {
		t.Fatal("Markdown Plan was accepted")
	}
	body := `{"version":1,"steps":[` +
		`{"id":"implement","title":"Update parser","status":"pending"}]}`
	if err := validateStructuredPlan(body, false); err != nil {
		t.Fatal(err)
	}
	if err := validateStructuredPlan(body, true); err == nil {
		t.Fatal("persisted Plan without Revision was accepted")
	}
}
