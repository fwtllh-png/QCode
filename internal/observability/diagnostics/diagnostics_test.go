package diagnostics

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/fwtllh-png/QCode/internal/security/controlmatrix"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func TestSpaceDelimitedOutputParsesAsStructuredDiagnostics(t *testing.T) {
	values := parse(
		"docs/readme.md:4:7 error MD022/blanks-around-headings "+
			"Headings should be surrounded by blank lines\n",
		"fixture-lint",
		"docs/readme.md",
	)
	if len(values) != 1 {
		t.Fatalf("diagnostics = %+v, want one", values)
	}
	diagnostic := values[0]
	if diagnostic.Path != "docs/readme.md" ||
		diagnostic.Range.Start.Line != 3 ||
		diagnostic.Range.Start.Character != 6 ||
		diagnostic.Severity != "error" ||
		diagnostic.Source != "fixture-lint" {
		t.Fatalf("diagnostic = %+v", diagnostic)
	}
}

func TestColonDelimitedDiagnosticOutputStillParses(t *testing.T) {
	values := parse(
		"main.go:8:3:12: warning: stale value\n",
		"gopls",
		"main.go",
	)
	if len(values) != 1 ||
		values[0].Severity != "warning" ||
		values[0].Range.End.Character != 12 {
		t.Fatalf("diagnostics = %+v", values)
	}
}

func TestCommandRunnerReportsUnconfiguredMarkdown(t *testing.T) {
	runner := NewCommandRunner(t.TempDir(), nil, nil)
	receipt, err := runner.Run(context.Background(), "README.md")
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Status != "unavailable" ||
		receipt.Message != "no post-edit diagnostics command is configured for .md" {
		t.Fatalf("receipt = %+v", receipt)
	}
}

type passthroughBackend struct{}

func (passthroughBackend) Capability() sandbox.Capability {
	return sandbox.Capability{
		Platform: runtime.GOOS,
		Backend:  "test",

		Available: true,
		Effective: controlmatrix.Matrix{
			FilesystemRead: controlmatrix.
				FilesystemReadDeclaredRoots,

			FilesystemWrite: controlmatrix.FilesystemWriteExactPaths, Network: controlmatrix.
						NetworkDenied, ProcessTree:        controlmatrix.ProcessTreeGroupKill, CrossProcess: controlmatrix.
						CrossProcessUnrestricted, Syscall: controlmatrix.SyscallDenyDangerous,
			IPC: controlmatrix.IPCUnrestricted,
			PathIdentity: controlmatrix.
				PathIdentityDescriptorRelative,

			ArtifactOrigin: controlmatrix.ArtifactOriginUnverifiedPath,

			DurableRecovery: controlmatrix.
				DurableRecoveryMemoryOnly},
	}
}

func (passthroughBackend) Prepare(
	_ context.Context,
	command sandbox.Command,
) (sandbox.Command, error) {
	return command, nil
}

func TestCommandRunnerReportsSignaledProcessAsUnavailableReceipt(t *testing.T) {
	root := t.TempDir()
	command := filepath.Join(root, "abort-check")
	if err := os.WriteFile(command, []byte("#!/bin/sh\nkill -ABRT $$\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "README.md")
	if err := os.WriteFile(path, []byte("# title\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := NewCommandRunner(root, passthroughBackend{}, map[string]Command{
		".md": {Name: command, Args: []string{"{path}"}},
	})

	receipt, err := runner.Run(t.Context(), path)

	if err != nil {
		t.Fatalf("Run() error = %v, want a structured failed receipt", err)
	}
	if receipt.Status != "unavailable" ||
		receipt.ErrorCategory != "runner_failure" ||
		receipt.ExitCode == 0 ||
		receipt.Message == "" {
		t.Fatalf("receipt = %+v", receipt)
	}
}
