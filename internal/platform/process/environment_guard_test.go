package process

import (
	"os"
	"strings"
	"testing"
)

func TestSecretEnvironmentNameCoversKeySuffixAndLookalikes(t *testing.T) {
	for _, name := range []string{
		"OPENAI_KEY", "ANTHROPIC_KEY", "SIGNING_KEY", "GCP_KEY",
		"MY_API_TOKEN", "DB_PASSWORD",
	} {
		if !SecretEnvironmentName(name) {
			t.Fatalf("%q was not treated as secret", name)
		}
	}
	// Fullwidth lookalikes fold before matching.
	if !SecretEnvironmentName("ＡＰＩ＿ＫＥＹ") {
		t.Fatal("fullwidth API_KEY lookalike was not treated as secret")
	}
	for _, name := range []string{"KEYBOARD_LAYOUT", "MONKEY_PATCH", "HOME", "PATH"} {
		if SecretEnvironmentName(name) {
			t.Fatalf("%q was wrongly treated as secret", name)
		}
	}
}

func TestSanitizedEnvironmentRefusesPreloadAndPolicyOwnedNames(t *testing.T) {
	for _, name := range []string{
		"LD_PRELOAD", "LD_LIBRARY_PATH", "DYLD_INSERT_LIBRARIES",
		"BASH_ENV", "ENV", "NODE_OPTIONS", "PYTHONSTARTUP",
		"HOME", "TMPDIR", "TMP", "TEMP",
	} {
		if _, err := SanitizedEnvironment([]string{name + "=value"}); err == nil {
			t.Fatalf("%q was accepted as a model declaration", name)
		}
	}
	env, err := SanitizedEnvironment([]string{"CGO_ENABLED=0", "CUSTOM_FLAG=1"})
	if err != nil {
		t.Fatal(err)
	}
	if environmentValue(env, "CGO_ENABLED") != "0" ||
		environmentValue(env, "CUSTOM_FLAG") != "1" {
		t.Fatalf("ordinary declarations were dropped: %v", env)
	}
}

func TestToolchainSearchPathOrdersAndDedupes(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	host := t.TempDir()
	t.Setenv(
		"PATH",
		strings.Join([]string{"", host, host, first}, string(os.PathListSeparator)),
	)
	ordered := ToolchainSearchPath([]string{second, first})
	if len(ordered) == 0 || ordered[0] != second {
		t.Fatalf("toolchain bins must resolve first: %v", ordered)
	}
	seen := make(map[string]bool)
	for _, entry := range ordered {
		if entry == "" {
			t.Fatalf("empty PATH entry survived: %v", ordered)
		}
		if seen[entry] {
			t.Fatalf("duplicate entry %q survived: %v", entry, ordered)
		}
		seen[entry] = true
	}
	if !seen[host] || !seen[first] {
		t.Fatalf("host entries lost: %v", ordered)
	}
}
