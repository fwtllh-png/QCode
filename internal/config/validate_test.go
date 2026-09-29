package config

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

func TestLoadRejectsNarrativeOutputCeilingAboveSafetyLimit(t *testing.T) {
	tooLarge := (1 << 20) + 1
	_, err := Load(LoadOptions{Overrides: Overrides{
		CompactSemanticNarrativeMaxOutputTokens: &tooLarge,
	}})
	if err == nil || !strings.Contains(
		err.Error(),
		fieldCompactSemanticNarrativeMaxOutputTokens,
	) {
		t.Fatalf("output ceiling error = %v", err)
	}
}

func TestLoadRejectsUnknownSubagentDelegation(t *testing.T) {
	path := writeConfig(t, `
[execution.subagent]
delegation = "proactive"
`)
	_, err := Load(LoadOptions{Path: path})
	if err == nil || !strings.Contains(err.Error(), fieldSubagentDelegation) {
		t.Fatalf("delegation error = %v", err)
	}
}

func TestLoadAcceptsReferenceKindsAndRejectsUnknownKind(t *testing.T) {
	for _, kind := range []string{"env", "file", "keyring"} {
		snapshot, err := Load(LoadOptions{
			LookupEnv: envLookup(map[string]string{
				"QCODE_CREDENTIAL_KIND": kind,
				"QCODE_CREDENTIAL_NAME": "provider/default",
			}),
		})
		if err != nil {
			t.Fatalf("Load(%s): %v", kind, err)
		}
		if snapshot.Config.Credential.Kind != kind {
			t.Fatalf("credential = %+v", snapshot.Config.Credential)
		}
	}
	_, err := Load(LoadOptions{LookupEnv: envLookup(map[string]string{
		"QCODE_CREDENTIAL_KIND": "inline",
		"QCODE_CREDENTIAL_NAME": "provider/default",
	})})
	var fieldErr *FieldError
	if !errors.As(err, &fieldErr) || fieldErr.Field != fieldCredentialKind {
		t.Fatalf("Load(inline) error = %v, want credential kind FieldError", err)
	}
}

func TestVisionConfigFileAndValidation(t *testing.T) {
	path := writeConfig(t, `
[vision]
enabled = true
provider = "openai"
model = "gpt-4.1"
`)
	snapshot, err := Load(LoadOptions{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Config.Vision.Enabled ||
		snapshot.Config.Vision.Provider != "openai" ||
		snapshot.Config.Vision.Model != "gpt-4.1" {
		t.Fatalf("vision = %+v", snapshot.Config.Vision)
	}
	_, err = Load(LoadOptions{
		LookupEnv: envLookup(map[string]string{"QCODE_VISION_ENABLED": "true"}),
	})
	if err == nil {
		t.Fatal("expected validation error when vision enabled without provider/model")
	}
}

func TestViewRejectsInlineNarrativeMode(t *testing.T) {
	_, err := Load(LoadOptions{LookupEnv: envLookup(map[string]string{
		"QCODE_VIEW_NARRATIVE_MODE": "inline",
	})})
	var fieldErr *FieldError
	if !errors.As(err, &fieldErr) ||
		fieldErr.Field != fieldViewNarrativeMode {
		t.Fatalf(
			"Load(inline) error = %v, want %s field error",
			err,
			fieldViewNarrativeMode,
		)
	}
}

func TestViewRejectsDigestOff(t *testing.T) {
	_, err := Load(LoadOptions{LookupEnv: envLookup(map[string]string{
		"QCODE_VIEW_DIGEST": "off",
	})})
	var fieldErr *FieldError
	if !errors.As(err, &fieldErr) || fieldErr.Field != fieldViewDigest {
		t.Fatalf("Load(digest=off) error = %v, want %s", err, fieldViewDigest)
	}
}

func TestViewRejectsCheckpointMaxBytesBelowMinimum(t *testing.T) {
	_, err := Load(LoadOptions{LookupEnv: envLookup(map[string]string{
		"QCODE_VIEW_CHECKPOINT_MAX_BYTES": "128",
	})})
	var fieldErr *FieldError
	if !errors.As(err, &fieldErr) || fieldErr.Field != fieldViewCheckpointMaxBytes {
		t.Fatalf("Load(checkpoint_max_bytes=128) error = %v", err)
	}
}

// The affected scope now has a repo index behind it, and it accepts a command so
// an operator can point it at their own suite.
func TestVerifyGateConfigAcceptsTheAffectedScope(t *testing.T) {
	loaded, err := Load(LoadOptions{LookupEnv: envLookup(map[string]string{
		"QCODE_VERIFY_SCOPE":   "affected",
		"QCODE_VERIFY_COMMAND": "go test {packages}",
	})})
	if err != nil {
		t.Fatal(err)
	}
	verify := loaded.Config.Execution.Verify
	if verify.Scope != "affected" || verify.Command != "go test {packages}" {
		t.Fatalf("verify = %+v", verify)
	}
}

// The unimplemented values must not load: silently degrading them into
// something that runs would hide the gap.
func TestVerifyGateConfigRejectsUnimplementedValues(t *testing.T) {
	tests := map[string]struct {
		env       map[string]string
		wantField string
	}{
		"unknown scope": {
			env:       map[string]string{"QCODE_VERIFY_SCOPE": "packages"},
			wantField: fieldVerifyScope,
		},
		"ask on failure": {
			env:       map[string]string{"QCODE_VERIFY_ON_FAILURE": "ask"},
			wantField: fieldVerifyOnFailure,
		},
		"unknown mode": {
			env:       map[string]string{"QCODE_VERIFY_MODE": "always"},
			wantField: fieldVerifyMode,
		},
		"negative repair budget": {
			env:       map[string]string{"QCODE_VERIFY_MAX_REPAIR_STEPS": "-1"},
			wantField: fieldVerifyRepair,
		},
		"zero timeout": {
			env:       map[string]string{"QCODE_VERIFY_TIMEOUT": "0s"},
			wantField: fieldVerifyTimeout,
		},
		// A command under the diagnostics scope would silently never run.
		"command without a command scope": {
			env:       map[string]string{"QCODE_VERIFY_COMMAND": "make verify"},
			wantField: fieldVerifyCommand,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Load(LoadOptions{LookupEnv: envLookup(test.env)})
			var fieldErr *FieldError
			if !errors.As(err, &fieldErr) {
				t.Fatalf("Load() error = %v, want a field error", err)
			}
			if fieldErr.Field != test.wantField {
				t.Fatalf("field = %q, want %q", fieldErr.Field, test.wantField)
			}
			if fieldErr.Source != SourceEnv {
				t.Fatalf("source = %q, want env", fieldErr.Source)
			}
		})
	}
}

func TestExecutionEnvironmentRejectsInvalidCombinations(t *testing.T) {
	_, err := Load(LoadOptions{
		Path: writeConfig(t, `
[execution.environment]
contract = "next"
`),
		LookupEnv: envLookup(nil),
	})
	if err == nil || !strings.Contains(err.Error(), fieldEnvironmentContract) {
		t.Fatalf("invalid contract error = %v", err)
	}

	_, err = Load(LoadOptions{
		Path: writeConfig(t, `
[execution.environment]
contract = "legacy"
`),
		LookupEnv: envLookup(nil),
	})
	if err == nil || !strings.Contains(err.Error(), fieldEnvironmentContract) {
		t.Fatalf("legacy contract error = %v", err)
	}

	_, err = Load(LoadOptions{
		Path: writeConfig(t, `
[execution.environment]
profile = "shared"
`),
		LookupEnv: envLookup(nil),
	})
	if err == nil || !strings.Contains(err.Error(), fieldEnvironmentProfile) {
		t.Fatalf("invalid profile error = %v", err)
	}

	_, err = Load(LoadOptions{
		Path: writeConfig(t, `
[execution.environment]
profile = "isolated"
shared_user_temp = true
`),
		LookupEnv: envLookup(nil),
	})
	if err == nil || !strings.Contains(err.Error(), fieldEnvironmentSharedUserTemp) {
		t.Fatalf("isolated shared temp error = %v", err)
	}

	invalid := "v1"
	profile := EnvironmentProfileIsolated
	shared := true
	_, err = Load(LoadOptions{
		Overrides: Overrides{
			EnvironmentContract:       &invalid,
			EnvironmentProfile:        &profile,
			EnvironmentSharedUserTemp: &shared,
		},
	})
	if err == nil || !strings.Contains(err.Error(), fieldEnvironmentSharedUserTemp) {
		t.Fatalf("override shared temp error = %v", err)
	}
}

func TestExecutionEnvironmentRejectsInvalidDeclaredResources(t *testing.T) {
	_, err := Load(LoadOptions{
		Path: writeConfig(t, `
[execution.environment]
contract = "v1"

[[execution.environment.resources]]
name = "workspace-root"
namespace = "workspace"
access = "write"
path = "."
`),
		LookupEnv: envLookup(nil),
	})
	if err == nil || !strings.Contains(err.Error(), fieldEnvironmentResources) {
		t.Fatalf("workspace declaration error = %v", err)
	}

	_, err = Load(LoadOptions{
		Path: writeConfig(t, `
[execution.environment]
profile = "native"

[[execution.environment.resources]]
name = "user-temp"
namespace = "shared_user_temp"
access = "write"
shared = true
`),
		LookupEnv: envLookup(nil),
	})
	if err == nil || !strings.Contains(err.Error(), "shared_user_temp=true") {
		t.Fatalf("shared temp declaration error = %v", err)
	}

	_, err = Load(LoadOptions{
		Path: writeConfig(t, `
[execution.environment]
[[execution.environment.resources]]
name = "dup"
namespace = "host_config"
access = "read"
path = "env:LANG"
[[execution.environment.resources]]
name = "dup"
namespace = "host_config"
access = "read"
path = "env:LC_ALL"
`),
		LookupEnv: envLookup(nil),
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate declaration error = %v", err)
	}
}

func TestExecutionEnvironmentRejectsTooManyDeclaredResources(t *testing.T) {
	body := "[execution.environment]\n"
	for i := 0; i < MaxDeclaredEnvironmentResources+1; i++ {
		name := strconv.Itoa(i)
		body += "[[execution.environment.resources]]\n" +
			"name = \"res-" + name + "\"\n" +
			"namespace = \"host_config\"\naccess = \"read\"\npath = \"env:V" + name + "\"\n"
	}
	_, err := Load(LoadOptions{Path: writeConfig(t, body), LookupEnv: envLookup(nil)})
	if err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("resource ceiling error = %v", err)
	}
}

func TestExecutionModeOnlyAcceptsAct(t *testing.T) {
	for _, mode := range []string{"act", "plan", "operate"} {
		t.Run(mode, func(t *testing.T) {
			snapshot, err := Load(LoadOptions{
				LookupEnv: envLookup(map[string]string{"QCODE_MODE": mode}),
			})
			if mode == "act" {
				if err != nil || snapshot.Config.Execution.Mode != "act" {
					t.Fatalf("act configuration: %+v, %v", snapshot.Config.Execution, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "must be act") {
				t.Fatalf("mode %q error = %v, want a refusal", mode, err)
			}
		})
	}
}

func TestAHalfNamedSlotIsAnError(t *testing.T) {
	path := writeConfig(t, `
[route.summary]
provider = "openai"
`)

	_, err := Load(LoadOptions{Path: path})

	if err == nil || !strings.Contains(err.Error(), "route.summary.model") {
		t.Fatalf("Load() error = %v, want the missing model named", err)
	}
}
