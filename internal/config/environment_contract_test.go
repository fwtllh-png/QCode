package config

import (
	"strconv"
	"strings"
	"testing"
)

func TestExecutionEnvironmentDefaultsAreV1Native(t *testing.T) {
	defaults := Defaults().Execution.Environment
	if defaults.Contract != EnvironmentContractV1 ||
		defaults.Profile != EnvironmentProfileNative ||
		defaults.SharedUserTemp ||
		defaults.Source != "" ||
		len(defaults.Resources) != 0 ||
		len(defaults.AuthServices) != 0 {
		t.Fatalf("default execution environment = %+v", defaults)
	}
}

func TestExecutionEnvironmentHasProvenanceAndValidation(t *testing.T) {
	path := writeConfig(t, `
[execution.environment]
contract = "v1"
profile = "native"
shared_user_temp = true
source = "login-shell"
`)
	fromFile, err := Load(LoadOptions{Path: path, LookupEnv: envLookup(nil)})
	if err != nil {
		t.Fatal(err)
	}
	got := fromFile.Config.Execution.Environment
	if got.Contract != EnvironmentContractV1 ||
		got.Profile != EnvironmentProfileNative ||
		!got.SharedUserTemp ||
		got.Source != "login-shell" {
		t.Fatalf("file environment = %+v", got)
	}
	for _, field := range []string{
		fieldEnvironmentContract,
		fieldEnvironmentProfile,
		fieldEnvironmentSharedUserTemp,
		fieldEnvironmentSource,
	} {
		if fromFile.Provenance[field] != SourceFile {
			t.Fatalf("file provenance[%s] = %q", field, fromFile.Provenance[field])
		}
	}

	fromEnv, err := Load(LoadOptions{
		Path: path,
		LookupEnv: envLookup(map[string]string{
			"QCODE_ENVIRONMENT_CONTRACT":         "v1",
			"QCODE_ENVIRONMENT_PROFILE":          "isolated",
			"QCODE_ENVIRONMENT_SHARED_USER_TEMP": "false",
			"QCODE_ENVIRONMENT_SOURCE":           "",
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	got = fromEnv.Config.Execution.Environment
	if got.Contract != EnvironmentContractV1 ||
		got.Profile != EnvironmentProfileIsolated ||
		got.SharedUserTemp ||
		got.Source != "" {
		t.Fatalf("env environment = %+v", got)
	}
	if fromEnv.Provenance[fieldEnvironmentContract] != SourceEnv ||
		fromEnv.Provenance[fieldEnvironmentProfile] != SourceEnv ||
		fromEnv.Provenance[fieldEnvironmentSharedUserTemp] != SourceEnv ||
		fromEnv.Provenance[fieldEnvironmentSource] != SourceEnv {
		t.Fatalf("env provenance = %+v", fromEnv.Provenance)
	}

	startupContract := EnvironmentContractV1
	startupProfile := EnvironmentProfileIsolated
	_, err = Load(LoadOptions{
		Overrides: Overrides{
			EnvironmentContract: &startupContract,
			EnvironmentProfile:  &startupProfile,
		},
	})
	if err != nil {
		t.Fatal(err)
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

func TestExecutionEnvironmentLoadsDeclaredResources(t *testing.T) {
	path := writeConfig(t, `
[execution.environment]
contract = "v1"
profile = "native"

[[execution.environment.resources]]
name = "tool-config"
namespace = "host_config"
access = "read"
path = "/opt/tool/config.toml"

[[execution.environment.resources]]
name = "tool-cache"
namespace = "cache"
access = "write"
path = "sandbox-home/cache/tool"
env = "TOOL_CACHE"
tree = true
`)
	snapshot, err := Load(LoadOptions{Path: path, LookupEnv: envLookup(nil)})
	if err != nil {
		t.Fatal(err)
	}
	got := snapshot.Config.Execution.Environment
	if snapshot.Provenance[fieldEnvironmentResources] != SourceFile ||
		len(got.Resources) != 2 ||
		got.Resources[0].Path != "/opt/tool/config.toml" ||
		got.Resources[1].Env != "TOOL_CACHE" {
		t.Fatalf("declared resources = %+v provenance=%q", got.Resources, snapshot.Provenance[fieldEnvironmentResources])
	}
	requests := got.DeclaredRequests()
	if len(requests) != 2 ||
		requests[0].Source != "user-declaration" ||
		requests[1].Env != "TOOL_CACHE" {
		t.Fatalf("declared requests = %+v", requests)
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

func TestExecutionEnvironmentLoadsGoproxyAuthService(t *testing.T) {
	path := writeConfig(t, `
[execution.environment]
contract = "v1"

[[execution.environment.auth_services]]
protocol = "goproxy"
upstream = "https://goproxy.example"
prefixes = ["example.com/qcode/"]
credential = { kind = "env", name = "GOPROXY_TOKEN" }
`)
	snapshot, err := Load(LoadOptions{Path: path, LookupEnv: envLookup(nil)})
	if err != nil {
		t.Fatal(err)
	}
	got := snapshot.Config.Execution.Environment.AuthServices
	if snapshot.Provenance[fieldEnvironmentAuthServices] != SourceFile ||
		len(got) != 1 || got[0].Protocol != "goproxy" ||
		got[0].Credential.Name != "GOPROXY_TOKEN" {
		t.Fatalf("auth services = %+v provenance=%q", got, snapshot.Provenance[fieldEnvironmentAuthServices])
	}
}

func TestExecutionEnvironmentRejectsInvalidAuthServices(t *testing.T) {
	_, err := Load(LoadOptions{
		Path: writeConfig(t, `
[execution.environment]
[[execution.environment.auth_services]]
protocol = "npm"
upstream = "https://registry.example"
prefixes = ["example.com/"]
credential = { kind = "env", name = "TOKEN" }
`),
		LookupEnv: envLookup(nil),
	})
	if err == nil || !strings.Contains(err.Error(), "goproxy") {
		t.Fatalf("unknown protocol error = %v", err)
	}

	_, err = Load(LoadOptions{
		Path: writeConfig(t, `
[execution.environment]
[[execution.environment.auth_services]]
protocol = "goproxy"
upstream = "https://user:secret@goproxy.example"
prefixes = ["example.com/"]
credential = { kind = "env", name = "TOKEN" }
`),
		LookupEnv: envLookup(nil),
	})
	if err == nil || !strings.Contains(err.Error(), "embed") {
		t.Fatalf("embedded credential error = %v", err)
	}

	_, err = Load(LoadOptions{
		Path: writeConfig(t, `
[execution.environment]
[[execution.environment.auth_services]]
protocol = "goproxy"
upstream = "https://goproxy.example"
prefixes = ["example.com/"]
upstream_timeout_ms = -1
credential = { kind = "env", name = "TOKEN" }
`),
		LookupEnv: envLookup(nil),
	})
	if err == nil || !strings.Contains(err.Error(), "upstream_timeout_ms") {
		t.Fatalf("negative timeout error = %v", err)
	}

	snapshot, err := Load(LoadOptions{
		Path: writeConfig(t, `
[execution.environment]
[[execution.environment.auth_services]]
protocol = "goproxy"
upstream = "https://goproxy.example"
prefixes = ["example.com/"]
upstream_timeout_ms = 120000
credential = { kind = "env", name = "TOKEN" }
`),
		LookupEnv: envLookup(nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshot.Config.Execution.Environment.AuthServices[0].UpstreamTimeoutMS; got != 120000 {
		t.Fatalf("upstream_timeout_ms = %d", got)
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
