package wire

import (
	"context"
	"slices"
	"testing"

	"github.com/fwtllh-png/QCode/internal/config"
	"github.com/fwtllh-png/QCode/internal/security/policy"
)

func TestMutableSessionProfileFieldsExcludeDerivedPlanningPolicy(t *testing.T) {
	fields := mutableSessionProfileFields(
		[]string{"model", "reasoning_effort"},
		true,
		true,
	)
	if slices.Contains(fields, "planning_policy") || slices.Contains(fields, "mode") {
		t.Fatalf("fixed policy field is mutable: %v", fields)
	}
	if !slices.Contains(fields, "enabled_tool_ids") ||
		!slices.Contains(fields, "approval_posture") {
		t.Fatalf("expected mutable fields are missing: %v", fields)
	}
}

func TestSessionPermissionCeilingRequiresExplicitHostAuthority(t *testing.T) {
	for _, test := range []struct {
		configured string
		initial    policy.Permission
		want       policy.Permission
	}{
		{"", policy.PermissionAuto, policy.PermissionAuto},
		{"bypass", policy.PermissionAuto, policy.PermissionBypass},
		{"", policy.PermissionNever, policy.PermissionNever},
		{"never", policy.PermissionAuto, policy.PermissionNever},
	} {
		got, err := sessionPermissionCeiling(test.configured, test.initial)
		if err != nil || got != test.want {
			t.Fatalf("ceiling(%q, %q) = %q, %v; want %q", test.configured, test.initial, got, err, test.want)
		}
	}
	if _, err := sessionPermissionCeiling("unknown", policy.PermissionAuto); err == nil {
		t.Fatal("unknown host authority accepted")
	}
}

func TestSessionCanSelectFullAccessWithoutChangingTheDefault(t *testing.T) {
	workspace, tools := t.TempDir(), true
	session, err := NewExec(t.Context(), withNonDurableTestJournal(t, ExecOptions{
		FixturePath: subagentFixture(t, "openai"),
		Permission:  "auto", ProfilePermissionCeiling: "bypass",
		ConfigOverrides: config.Overrides{Workspace: &workspace, Tools: &tools},
		Skills:          SkillOptions{UserHome: t.TempDir(), DataDir: t.TempDir()},
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close(context.Background()) })
	if _, err := session.threads.History("permission-switch"); err != nil {
		t.Fatal(err)
	}
	engine, err := session.threads.ContextEngine("permission-switch")
	if err != nil {
		t.Fatal(err)
	}
	profile := session.DefaultProfile()
	if profile.ApprovalPosture != "auto" {
		t.Fatalf("default = %q", profile.ApprovalPosture)
	}
	for _, posture := range []policy.Permission{policy.PermissionAuto, policy.PermissionBypass, policy.PermissionNever, policy.PermissionAuto} {
		profile.Revision++
		profile.ApprovalPosture = string(posture)
		if err := engine.ApplySessionProfile(profile); err != nil {
			t.Fatal(err)
		}
		if got := engine.OptionsSeed().Security.PermissionValue(); got != posture {
			t.Fatalf("selected %q, effective %q", posture, got)
		}
	}
}
