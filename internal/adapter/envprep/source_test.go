package envprep

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/environment"
	"github.com/fwtllh-png/QCode/internal/platform/process"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func TestSourceSnapshotSurvivesPolicyAndProcessConstruction(t *testing.T) {
	root, home, bin := canonicalTestDir(t), canonicalTestDir(t), canonicalTestDir(t)
	certificate := filepath.Join(canonicalTestDir(t), "public.pem")
	if err := os.WriteFile(certificate, []byte("public certificate fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	sdk := canonicalTestDir(t)
	if err := os.WriteFile(filepath.Join(sdk, "SDKSettings.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := []string{"HOME=" + home, "PATH=" + bin, "LANG=C", "LC_MESSAGES=C", "SDKROOT=" + sdk, "SSL_CERT_FILE=" + certificate, "API_TOKEN=excluded-fixture", "UNDECLARED=excluded-fixture"}
	prepared, err := Prepare(t.Context(), Options{
		Sandbox:   sandbox.Options{WorkspaceRoot: root, PrivateTemp: canonicalTestDir(t), EnvironmentProfile: environment.ProfileNative},
		SourceEnv: source,
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := range source {
		source[i] = "MUTATED=changed"
	}
	for _, name := range []string{"HOME", "PATH", "LANG", "LC_MESSAGES", "SDKROOT", "SSL_CERT_FILE"} {
		t.Setenv(name, "/not-the-captured-source")
	}
	backend, err := sandbox.NewPlatformBackend(prepared.Sandbox)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sandbox.CloseBackend(backend) })
	policy, ok := sandbox.BackendPolicy(backend)
	if !ok {
		t.Fatal("backend has no policy")
	}
	if envLookup(policy.EnvironmentValues, "HOME") != home || envLookup(policy.EnvironmentValues, "LANG") != "C" ||
		envLookup(policy.EnvironmentValues, "SSL_CERT_FILE") != certificate || !slices.Contains(policy.HostReadFiles, certificate) {
		t.Fatal("policy lost the prepared environment or certificate binding")
	}
	canonicalSDK, _ := filepath.EvalSymlinks(sdk)
	if envLookup(policy.EnvironmentValues, "SDKROOT") != canonicalSDK {
		t.Fatal("policy re-read SDKROOT")
	}
	encoded, err := json.Marshal(prepared)
	if err != nil || strings.Contains(string(encoded), "excluded-fixture") || strings.Contains(string(encoded), "MUTATED") {
		t.Fatal("raw source escaped preparation")
	}
	if err := sandbox.RequireControls(backend, sandbox.DefaultProcessRequirements()); err != nil {
		t.Skipf("strong sandbox unavailable: %v", err)
	}
	directory, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	command, err := process.NewCommand(t.Context(), process.Options{Command: "true", Dir: root, DirFile: directory, Sandbox: backend, RequireSandbox: true})
	if err != nil {
		t.Fatal(err)
	}
	if envLookup(command.Env, "HOME") != home || envLookup(command.Env, "LC_MESSAGES") != "C" ||
		envLookup(command.Env, "PATH") != envLookup(policy.EnvironmentValues, "PATH") {
		t.Fatal("command did not use the policy snapshot")
	}
}

func TestExplicitEmptySourceDoesNotRecoverHostSettings(t *testing.T) {
	bin := canonicalTestDir(t)
	t.Setenv("PATH", bin)
	t.Setenv("HOME", canonicalTestDir(t))
	t.Setenv("LANG", "host-locale")
	t.Setenv("SSL_CERT_FILE", "/missing-host-certificate")
	t.Setenv("SDKROOT", "/missing-host-sdk")
	prepared, err := Prepare(t.Context(), Options{
		Sandbox:   sandbox.Options{WorkspaceRoot: canonicalTestDir(t), PrivateTemp: canonicalTestDir(t), EnvironmentProfile: environment.ProfileNative},
		SourceEnv: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := sandbox.BuildPolicy(prepared.Sandbox)
	if err != nil {
		t.Fatal(err)
	}
	if envLookup(policy.EnvironmentValues, "HOME") != "" || envLookup(policy.EnvironmentValues, "LANG") != "" ||
		strings.Contains(envLookup(policy.EnvironmentValues, "PATH"), filepath.Base(bin)) || envLookup(policy.EnvironmentValues, "SSL_CERT_FILE") != "" {
		t.Fatal("empty source recovered host settings")
	}
}

func TestDeclarationsOverrideBaselineAndRejectConflicts(t *testing.T) {
	declaration := func(name, variable, value string) environment.ResourceRequest {
		return environment.ResourceRequest{Name: name, Namespace: environment.NamespaceHostConfig, Access: environment.AccessRead, Env: variable, Value: value}
	}
	options := Options{
		Sandbox:      sandbox.Options{WorkspaceRoot: canonicalTestDir(t), PrivateTemp: canonicalTestDir(t), EnvironmentProfile: environment.ProfileNative},
		SourceEnv:    []string{"LANG=source"},
		Declarations: []environment.ResourceRequest{declaration("locale-override", "LANG", "trusted"), declaration("unknown", "NEW_TOOL_FLAG", "")},
	}
	prepared, err := Prepare(t.Context(), options)
	if err != nil || envLookup(prepared.Sandbox.EnvironmentValues, "LANG") != "trusted" || !slices.Contains(prepared.Sandbox.EnvironmentValues, "NEW_TOOL_FLAG=") {
		t.Fatalf("trusted override failed: %v", err)
	}
	options.Declarations = append(options.Declarations, declaration("locale-conflict", "LANG", "conflicting"))
	if _, err := Prepare(t.Context(), options); err == nil {
		t.Fatal("same-layer environment conflict accepted")
	}
	options.Declarations = []environment.ResourceRequest{declaration("duplicate", "A", "one"), declaration("duplicate", "B", "two")}
	if _, err := Prepare(t.Context(), options); err == nil {
		t.Fatal("duplicate resource name was silently discarded")
	}
	for _, name := range []string{"HOME", "TMPDIR", "API_TOKEN", "BASH_ENV", "NODE_OPTIONS"} {
		options.Declarations = []environment.ResourceRequest{declaration("invalid", name, "fixture")}
		if _, err := Prepare(t.Context(), options); err == nil {
			t.Fatalf("accepted reserved or unsafe declaration %s", name)
		}
	}
}

func TestUnknownToolRunsWithDeclaredResourcesAndCommandOverrides(t *testing.T) {
	root, privateHome := canonicalTestDir(t), canonicalTestDir(t)
	config := filepath.Join(canonicalTestDir(t), "fixture.conf")
	outside := filepath.Join(canonicalTestDir(t), "unbound.conf")
	for _, path := range []string{config, outside} {
		if err := os.WriteFile(path, []byte("configured"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	program := filepath.Join(root, "unrecognized-builder")
	if err := os.WriteFile(program, []byte(`#!/bin/sh
set -eu
cat "$FIXTURE_CONFIG" > "$FIXTURE_CACHE/result"
cat "$FIXTURE_CACHE/result"
printf '|%s|%s|%s' "$FIXTURE_MODE" "$LANG" "$PATH"
if cat "$UNBOUND_FILE" >/dev/null 2>&1; then exit 21; fi
if touch forbidden-workspace-write 2>/dev/null; then exit 22; fi
`), 0o700); err != nil {
		t.Fatal(err)
	}
	prepared, err := Prepare(t.Context(), Options{
		Sandbox:   sandbox.Options{WorkspaceRoot: root, PrivateTemp: privateHome, EnvironmentProfile: environment.ProfileIsolated},
		SourceEnv: []string{"LANG=source"},
		Declarations: []environment.ResourceRequest{
			{Name: "config", Namespace: environment.NamespaceHostConfig, Access: environment.AccessRead, Path: config, Env: "FIXTURE_CONFIG"},
			{Name: "cache", Namespace: environment.NamespaceCache, Access: environment.AccessWrite, Path: "sandbox-home/cache/fixture", Env: "FIXTURE_CACHE"},
			{Name: "mode", Namespace: environment.NamespaceHostConfig, Access: environment.AccessRead, Env: "FIXTURE_MODE", Value: "trusted"},
			{Name: "trusted-locale", Namespace: environment.NamespaceHostConfig, Access: environment.AccessRead, Env: "LANG", Value: "C"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	backend, err := sandbox.NewPlatformBackend(prepared.Sandbox)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sandbox.CloseBackend(backend) })
	if err := sandbox.RequireControls(backend, sandbox.DefaultProcessRequirements()); err != nil {
		t.Skipf("strong sandbox unavailable: %v", err)
	}
	directory, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	result, err := process.Run(t.Context(), process.Options{
		Path: program, Dir: root, DirFile: directory, Sandbox: backend, RequireSandbox: true,
		WorkspaceReadOnly: true, DenyNetwork: true,
		Env: []string{"FIXTURE_MODE=command", "PATH=/bin:/usr/bin", "UNBOUND_FILE=" + outside},
	})
	if err != nil || result.ExitCode != 0 || result.Stdout != "configured|command|C|/bin:/usr/bin" {
		t.Fatalf("unknown tool result=%+v err=%v", result, err)
	}
	if content, err := os.ReadFile(filepath.Join(privateHome, "cache/fixture/result")); err != nil || string(content) != "configured" {
		t.Fatalf("declared cache result=%q err=%v", content, err)
	}
}

func canonicalTestDir(t *testing.T) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return path
}
