package prompt

import (
	"strings"
	"testing"
)

func TestActInstructionPackPreservesExecutionAndInteractionGuidance(t *testing.T) {
	pack := ActInstructionPack()
	if !strings.Contains(pack, "Act mode") ||
		!strings.Contains(pack, "call update_plan") ||
		!strings.Contains(pack, "shell_read") {
		t.Fatalf("act pack incomplete: %q", pack)
	}
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
		t.Fatalf("interaction contract incomplete: %q", pack)
	}
}

func TestActInstructionPackAdvertisesImageInput(t *testing.T) {
	if value := ActInstructionPack(true); !strings.Contains(
		value,
		"accepts image attachments",
	) {
		t.Fatalf("vision mode pack = %q", value)
	}
}
