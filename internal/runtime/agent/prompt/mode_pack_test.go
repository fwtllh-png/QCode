package prompt

import (
	"strings"
	"testing"
)

func TestModeInstructionPackDiffersByMode(t *testing.T) {
	plan := ModeInstructionPack("plan")
	act := ModeInstructionPack("act")
	operate := ModeInstructionPack("operate")
	if plan == act || plan == operate || act == operate {
		t.Fatalf("packs must differ")
	}
	if !strings.Contains(plan, "Plan mode") || !strings.Contains(plan, "submit_plan") {
		t.Fatalf("plan pack incomplete: %q", plan)
	}
	if !strings.Contains(plan, "independently verifiable steps") ||
		!strings.Contains(act, "call update_plan") {
		t.Fatalf("plan progress guidance missing: plan=%q act=%q", plan, act)
	}
	if !strings.Contains(operate, "Operate mode") ||
		!strings.Contains(plan, "shell_read") ||
		!strings.Contains(act, "shell_read") ||
		!strings.Contains(operate, "shell_read") {
		t.Fatalf("operate pack incomplete: %q", operate)
	}
	for mode, pack := range map[string]string{
		"plan": plan, "act": act, "operate": operate,
	} {
		if !strings.Contains(pack, "request_user_input") ||
			!strings.Contains(pack, "ordinary assistant text") ||
			!strings.Contains(pack, "stop calling tools") ||
			!strings.Contains(pack, "turn_complete is optional") ||
			!strings.Contains(pack, "Resolve facts available through tools") ||
			!strings.Contains(pack, "already loaded facts") ||
			!strings.Contains(pack, "git_status or git_diff on Continue") ||
			!strings.Contains(pack, "authoritative facts about closed turns") ||
			!strings.Contains(pack, "rely on it instead of re-verifying") ||
			!strings.Contains(pack, "After search_text returns line hits") ||
			!strings.Contains(pack, "confirmed continuity or Located sites") ||
			!strings.Contains(pack, "error_category and required_action") ||
			!strings.Contains(pack, "environment_resource_unavailable") ||
			!strings.Contains(pack, "credential_unavailable") ||
			!strings.Contains(pack, "filesystem_access_denied") ||
			!strings.Contains(pack, "network_target_unapproved") ||
			!strings.Contains(pack, "401 or permission") ||
			!strings.Contains(pack, "host lacks credentials") ||
			!strings.Contains(pack, "network is unreachable") ||
			!strings.Contains(pack, "upstream") ||
			!strings.Contains(pack, "bound auth service") ||
			!strings.Contains(pack, "session-local origin") {
			t.Fatalf("%s interaction contract incomplete: %q", mode, pack)
		}
	}
}

func TestModeInstructionPackAdvertisesImageInput(t *testing.T) {
	if value := ModeInstructionPack("act", true); !strings.Contains(
		value,
		"accepts image attachments",
	) {
		t.Fatalf("vision mode pack = %q", value)
	}
}
