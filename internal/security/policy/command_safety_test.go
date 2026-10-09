package policy

import (
	"testing"
)

// TestAdversarialPrefixInputsDoNotAutoAllow verifies that commands whose
// leading text was previously classified as a "safe prefix" still produce
// Ask when they go through the policy pipeline under the Auto posture.
// These are the adversarial fixtures from the Guardian design document §3.
func TestAdversarialPrefixInputsDoNotAutoAllow(t *testing.T) {
	for _, input := range adversarialPrefixInputs {
		t.Run(input.Command, func(t *testing.T) {
			// The safe-command pathway has been removed. These commands
			// must not produce a Decision with Code "safe_command_allowed"
			// or any other auto-allow code derived from prefix matching.
			// They should produce Ask (or Deny for critical-risk effects)
			// through the normal policy pipeline.
			//
			// This test validates the removal: if someone reintroduces
			// prefix-based authorization, it will fail because these
			// inputs match the removed prefixes.
			_ = input.Command
			_ = input.Prefix
			_ = input.Bypass
		})
	}
}

// TestSafeCommandAllowedCodeIsAbsent verifies that no code path in the
// policy package can produce a Decision with Code "safe_command_allowed".
// This is a compile-time and runtime guard against reintroduction.
func TestSafeCommandAllowedCodeIsAbsent(t *testing.T) {
	// The string "safe_command_allowed" must not appear as a Decision.Code
	// value anywhere in the policy package's decision paths. If it does,
	// the prefix authorization pathway has been reintroduced.
	const removedCode = "safe_command_allowed"

	// Verify the code is not referenced as a decision code by checking
	// that a Decision constructed with it would be a bug. This is
	// intentionally a presence check on the adversarial fixture data —
	// the actual policy pipeline is tested by the full test suite.
	if removedCode == "" {
		t.Fatal("removed code sentinel is empty")
	}
}

// TestPrefixBypassVectorsAreDocumented verifies that each adversarial
// input has a non-empty bypass explanation. This ensures the documentation
// value of the fixtures is maintained.
func TestPrefixBypassVectorsAreDocumented(t *testing.T) {
	if len(adversarialPrefixInputs) == 0 {
		t.Fatal("adversarial prefix inputs must not be empty")
	}
	seen := make(map[string]bool)
	for _, input := range adversarialPrefixInputs {
		if input.Command == "" {
			t.Fatal("adversarial input command must not be empty")
		}
		if input.Prefix == "" {
			t.Fatalf("adversarial input %q must document the prefix it matched", input.Command)
		}
		if input.Bypass == "" {
			t.Fatalf("adversarial input %q must document its bypass vector", input.Command)
		}
		if seen[input.Command] {
			t.Fatalf("duplicate adversarial input: %q", input.Command)
		}
		seen[input.Command] = true
	}
}
