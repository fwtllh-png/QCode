package dev

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/security/controlmatrix"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
	"github.com/fwtllh-png/QCode/internal/testutil/tooltest"
)

type passthroughBackend struct{}

func (passthroughBackend) Capability() sandbox.Capability {
	return sandbox.Capability{
		Platform: "test", Backend: "passthrough", Available: true,
		Effective: controlmatrix.Matrix{
			FilesystemRead:  controlmatrix.FilesystemReadDeclaredRoots,
			FilesystemWrite: controlmatrix.FilesystemWriteExactPaths,
			Network:         controlmatrix.NetworkDenied,
			ProcessTree:     controlmatrix.ProcessTreeGroupKill,
			CrossProcess:    controlmatrix.CrossProcessUnrestricted,
			Syscall:         controlmatrix.SyscallDenyDangerous,
			IPC:             controlmatrix.IPCUnrestricted,
			PathIdentity:    controlmatrix.PathIdentityDescriptorRelative,
			ArtifactOrigin:  controlmatrix.ArtifactOriginUnverifiedPath,
			DurableRecovery: controlmatrix.DurableRecoveryMemoryOnly,
		},
	}
}

func (passthroughBackend) Prepare(
	_ context.Context,
	command sandbox.Command,
) (sandbox.Command, error) {
	command.PreparedReadOnly = command.WorkspaceReadOnly
	command.PreparedWritePaths = append(
		[]string(nil), command.WorkspaceWritePaths...,
	)
	command.PreparedNetworkDenied = command.DenyNetwork
	return command, nil
}

func TestFormatCodeRunsInstalledFormatterForExactPath(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "main.go")
	if err := os.WriteFile(source, []byte("package main\nfunc main( ){}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry(nil, nil)
	backend, err := sandbox.BindPolicy(
		passthroughBackend{}, sandbox.Options{WorkspaceRoot: root},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := registerFormat(registry, root, backend); err != nil {
		t.Fatal(err)
	}
	result, err := tooltest.Execute(t.Context(), registry, tool.Call{
		Name: "format_code", Arguments: json.RawMessage(`{"paths":["main.go"]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError || strings.Contains(string(data), "main( )") {
		t.Fatalf("format result=%+v source=%q", result, data)
	}
}

func TestBreakpointCommandRejectsCommandInjection(t *testing.T) {
	for _, value := range []string{"main; shell touch owned", "../main.cpp:4", "main\nquit"} {
		if _, err := breakpointCommand(value); err == nil {
			t.Fatalf("unsafe breakpoint %q accepted", value)
		}
	}
	if command, err := breakpointCommand("src/main.cpp:42"); err != nil ||
		!strings.Contains(command, "--line 42") {
		t.Fatalf("file breakpoint = %q, %v", command, err)
	}
	if command, err := breakpointCommand("Namespace::Run"); err != nil ||
		command != "breakpoint set --name Namespace::Run" {
		t.Fatalf("symbol breakpoint = %q, %v", command, err)
	}
}

func TestDetectDependencyCommandsUsesManifestAndLockedInvocation(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"go.mod", "package.json", "pnpm-lock.yaml"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	bin := t.TempDir()
	for _, name := range []string{"go", "pnpm"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	commands, err := detectDependencyCommands(root, "auto")
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) != 2 ||
		commands[0].Ecosystem != "go" ||
		strings.Join(commands[0].Args, " ") != "mod download" ||
		commands[1].Ecosystem != "node" ||
		!strings.Contains(strings.Join(commands[1].Args, " "), "frozen-lockfile") {
		t.Fatalf("dependency commands = %+v", commands)
	}
}

func TestDependencyResolveRunsInNestedModuleDirectory(t *testing.T) {
	root := t.TempDir()
	module := filepath.Join(root, "eds_metaserver")
	if err := os.MkdirAll(module, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(module, "go.mod"),
		[]byte("module example.com/eds\n\ngo 1.26\n"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, ".bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	goStub := filepath.Join(bin, "go")
	if err := os.WriteFile(
		goStub, []byte("#!/bin/sh\npwd\n"), 0o755,
	); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	commands, err := detectDependencyCommands(root, "go")
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) != 1 || commands[0].Dir != module ||
		commands[0].RelativeDir != "eds_metaserver" {
		t.Fatalf("commands = %+v", commands)
	}
	registry := tool.NewRegistry(nil, nil)
	backend, err := sandbox.BindPolicy(
		passthroughBackend{}, sandbox.Options{WorkspaceRoot: root},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := registerDependency(registry, root, backend); err != nil {
		t.Fatal(err)
	}
	result, err := tooltest.Execute(t.Context(), registry, tool.Call{
		Name:      "dependency_resolve",
		Arguments: json.RawMessage(`{"ecosystem":"go"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("nested-module resolve failed: %+v", result)
	}
	var payload struct {
		Checks []struct {
			Dir      string `json:"dir"`
			Stdout   string `json:"stdout"`
			ExitCode int    `json:"exit_code"`
		} `json:"checks"`
	}
	if err := json.Unmarshal([]byte(result.Content), &payload); err != nil {
		t.Fatal(err)
	}
	canonicalModule := module
	if resolved, err := filepath.EvalSymlinks(module); err == nil {
		canonicalModule = resolved
	}
	if len(payload.Checks) != 1 || payload.Checks[0].Dir != "eds_metaserver" ||
		strings.TrimSpace(payload.Checks[0].Stdout) != canonicalModule {
		t.Fatalf("checks = %+v want cwd %s", payload.Checks, canonicalModule)
	}
}

func TestDependencyResolveFailureCarriesStructuredMetadata(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(root, "go.mod"), []byte("module example.com/x\n"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, ".bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(bin, "go"),
		[]byte("#!/bin/sh\necho 'boom' >&2; exit 1\n"), 0o755,
	); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	registry := tool.NewRegistry(nil, nil)
	backend, err := sandbox.BindPolicy(
		passthroughBackend{}, sandbox.Options{WorkspaceRoot: root},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := registerDependency(registry, root, backend); err != nil {
		t.Fatal(err)
	}
	result, err := tooltest.Execute(t.Context(), registry, tool.Call{
		Name:      "dependency_resolve",
		Arguments: json.RawMessage(`{"ecosystem":"go"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatalf("manager failure was not an error: %+v", result)
	}
	if result.Metadata["error_category"] != "dependency_resolve_failed" {
		t.Fatalf("error_category = %v metadata=%v", result.Metadata["error_category"], result.Metadata)
	}
	if result.Metadata["ecosystem"] != "go" {
		t.Fatalf("ecosystem metadata = %v", result.Metadata["ecosystem"])
	}
}

func TestFindManifestDirsBoundedAndSkipped(t *testing.T) {
	root := t.TempDir()
	module := filepath.Join(root, "svc")
	if err := os.MkdirAll(module, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(module, "go.mod"), []byte("module m\n"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	// Vendored and build trees are skipped even when they hold manifests.
	for _, skipped := range []string{"node_modules", "target", "vendor", ".git"} {
		dir := filepath.Join(root, skipped)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(
			filepath.Join(dir, "go.mod"), []byte("module m\n"), 0o600,
		); err != nil {
			t.Fatal(err)
		}
	}
	dirs := findManifestDirs(root, []string{"go.mod"})
	if len(dirs) != 1 || dirs[0] != module {
		t.Fatalf("dirs = %v, want only %s", dirs, module)
	}
	// Deeper than the search depth is not discovered.
	deep := root
	for i := 0; i < dependencyManifestSearchDepth+2; i++ {
		deep = filepath.Join(deep, "level")
	}
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(deep, "go.mod"), []byte("module deep\n"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	if dirs := findManifestDirs(root, []string{"go.mod"}); len(dirs) != 1 {
		t.Fatalf("deep manifest was discovered: %v", dirs)
	}
	// The result limit truncates discovery.
	for i := 0; i < dependencyManifestSearchLimit+4; i++ {
		dir := filepath.Join(root, "mod"+string(rune('a'+i%26))+fmt.Sprint(i))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(
			filepath.Join(dir, "go.mod"), []byte("module m\n"), 0o600,
		); err != nil {
			t.Fatal(err)
		}
	}
	if dirs := findManifestDirs(root, []string{"go.mod"}); len(dirs) > dependencyManifestSearchLimit {
		t.Fatalf("limit exceeded: %d dirs", len(dirs))
	}
}

func TestDetectDependencyCommandsRejectsMissingManifestForExplicitEcosystem(t *testing.T) {
	root := t.TempDir()
	if _, err := detectDependencyCommands(root, "go"); err == nil ||
		!strings.Contains(err.Error(), "no go dependency manifest") {
		t.Fatalf("missing manifest error = %v", err)
	}
}
