package process

import (
	"slices"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/security/sandbox"
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

func TestPreparedEnvironmentPreservesPolicyWithoutHostRecapture(t *testing.T) {
	policy := sandbox.Policy{
		EnvironmentProfile: "native", PrivateTemp: t.TempDir(),
		EnvironmentValues: []string{"HOME=/prepared/home", "LANG=prepared"},
	}
	prepared, err := EnvironmentFromPolicy(policy)
	if err != nil {
		t.Fatal(err)
	}
	policy.EnvironmentValues[0] = "HOME=/changed/home"
	t.Setenv("HOME", "/host/home")
	t.Setenv("API_TOKEN", "host-secret")
	options := Options{Path: "/usr/bin/env", Dir: t.TempDir(), Environment: prepared, Env: []string{"LANG=declared"}}
	result, err := Run(t.Context(), options)
	if err != nil || result.ExitCode != 0 || !strings.Contains(result.Stdout, "HOME=/prepared/home\n") ||
		!strings.Contains(result.Stdout, "LANG=declared\n") || !strings.Contains(result.Stdout, "TMPDIR="+policy.PrivateTemp+"\n") ||
		strings.Contains(result.Stdout, "host-secret") || strings.Contains(result.Stdout, "API_TOKEN=") {
		t.Fatalf("prepared environment result=%+v err=%v", result, err)
	}
	options.Env = []string{"HOME=/override"}
	if _, err := NewCommand(t.Context(), options); err == nil {
		t.Fatal("declaration overrode policy-owned HOME")
	}
	options.Env = nil
	options.TrustedRuntimeHelper = true
	if _, err := NewCommand(t.Context(), options); err == nil {
		t.Fatal("prepared environment mixed with host capture")
	}
}

func TestPreparedEnvironmentRejectsUnsafePolicyValues(t *testing.T) {
	for _, values := range [][]string{
		{"API_TOKEN=fixture"}, {"BASH_ENV=/fixture"}, {"malformed"}, {"LANG=a", "LANG=b"},
	} {
		if _, err := EnvironmentFromPolicy(sandbox.Policy{EnvironmentValues: values}); err == nil {
			t.Fatalf("unsafe prepared environment accepted: %v", values)
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

func TestSanitizedEnvironmentRejectsSecretsAndMalformedNames(t *testing.T) {
	for _, extra := range [][]string{
		{"API_TOKEN=value"},
		{"malformed"},
		{"=leading-equals"},
		{"1STARTSWITHDIGIT=value"},
	} {
		if _, err := SanitizedEnvironment(extra); err == nil {
			t.Fatalf("SanitizedEnvironment(%q) succeeded", extra)
		}
	}
	// Well-formed non-secret names are explicit reviewed input: language
	// and build variables (CGO_ENABLED, CARGO_HOME, ...) declare freely.
	environment, err := SanitizedEnvironment([]string{
		"LANG=C", "CGO_ENABLED=0", "CARGO_HOME=/cargo", "GOTMPDIR=/tmp/go-tmp",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"LANG=C", "CGO_ENABLED=0", "CARGO_HOME=/cargo", "GOTMPDIR=/tmp/go-tmp",
	} {
		if !slices.Contains(environment, want) {
			t.Fatalf("environment omitted %q: %q", want, environment)
		}
	}
}

func TestSanitizedEnvironmentDropsHostLanguageVariables(t *testing.T) {
	values := map[string]string{
		"GOPROXY":   "https://proxy.internal.example|direct",
		"GOPRIVATE": "code.internal.example",
		"GOSUMDB":   "sum.golang.org",
		"GOVCS":     "public:git|hg,private:all",
	}
	for name, value := range values {
		t.Setenv(name, value)
	}
	environment, err := SanitizedEnvironment(nil)
	if err != nil {
		t.Fatal(err)
	}
	for name := range values {
		for _, entry := range environment {
			if strings.HasPrefix(entry, name+"=") {
				t.Fatalf("host language variable %s leaked: %q", name, entry)
			}
		}
	}
	// Explicit declarations are the delivery path for the same names.
	environment, err = SanitizedEnvironment([]string{
		"GOPROXY=https://proxy.internal.example|direct",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(environment, "GOPROXY=https://proxy.internal.example|direct") {
		t.Fatalf("declared GOPROXY omitted: %q", environment)
	}
}
