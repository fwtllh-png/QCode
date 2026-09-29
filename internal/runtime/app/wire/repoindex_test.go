package wire

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/fwtllh-png/QCode/internal/config"
	"github.com/fwtllh-png/QCode/internal/persist/repoindex"
	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

type indexTestBackend struct{}

func (indexTestBackend) Capability() sandbox.Capability {
	return sandbox.Capability{
		Platform: "test", Backend: "passthrough",
		Available: true,
		Effective: securitymodel.Controls{FilesystemRead: securitymodel.FilesystemReadDeclaredRoots,

			FilesystemWrite: securitymodel.FilesystemWriteExactPaths,
			Network:         securitymodel.NetworkDenied, ProcessTree: securitymodel.ProcessTreeGroupKill, CrossProcess: securitymodel.CrossProcessUnrestricted,
			Syscall: securitymodel.SyscallDenyDangerous, IPC: securitymodel.IPCUnrestricted, PathIdentity: securitymodel.PathIdentityDescriptorRelative,
			ArtifactOrigin:  securitymodel.ArtifactOriginUnverifiedPath,
			DurableRecovery: securitymodel.DurableRecoveryMemoryOnly},
	}
}

func (indexTestBackend) Prepare(
	_ context.Context, command sandbox.Command,
) (sandbox.Command, error) {
	return command, nil
}

// A session without --data-dir still gets an index from the ephemeral database,
// which is what lets benchmarks exercise the symbol tools at all.
func TestRepositoryIndexOpensOverTheEphemeralDatabase(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(root, "api.go"), []byte("package api\n\nfunc Serve() {}\n"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	_, ephemeral, _, err := openRuntimeStateStore(t.Context(), nil, root, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ephemeral.Close() })

	index, status := openRepositoryIndex(
		root, indexTestBackend{}, ephemeral, config.Defaults().Context.Index,
	)
	if status != repoindex.StatusPending || index == nil {
		t.Fatalf("openRepositoryIndex() = %v, %q", index, status)
	}
	// The build is deferred, so the first query is what proves the handle works.
	found, snapshot, err := index.Symbols(t.Context(), repoindex.Query{Name: "Serve"})
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Ready() || len(found) != 1 || found[0].Name != "Serve" {
		t.Fatalf("symbols = %+v, snapshot = %+v", found, snapshot)
	}
}

func TestRepositoryIndexStaysClosedWhenDisabledOrWithoutADatabase(t *testing.T) {
	root := t.TempDir()
	off := config.Defaults().Context.Index
	off.Enabled = false
	if index, status := openRepositoryIndex(
		root, indexTestBackend{}, nil, off,
	); index != nil || status != repoindex.StatusDisabled {
		t.Fatalf("disabled index = %v, %q", index, status)
	}
	// No persistent store and no ephemeral one: there is nowhere to keep rows, and
	// a session must still start with text search.
	if index, status := openRepositoryIndex(
		root, indexTestBackend{}, nil, config.Defaults().Context.Index,
	); index != nil || status != repoindex.StatusDisabled {
		t.Fatalf("storeless index = %v, %q", index, status)
	}
}
