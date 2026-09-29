package host

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/fwtllh-png/QCode/internal/security/credential"
)

func TestWorkspaceRegistryPersistsCanonicalRootsWithPrivatePermissions(
	t *testing.T,
) {
	dataDir := t.TempDir()
	initial := t.TempDir()
	other := t.TempDir()
	alias := filepath.Join(t.TempDir(), "workspace-alias")
	if err := os.Symlink(initial, alias); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
	manager, err := newWorkspaceRuntimeManager(dataDir, alias)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := manager.Add(t.Context(), other)
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.Ready {
		t.Fatal("Workspace without a bound Runtime was reported ready")
	}
	roots, err := loadWorkspaceRoots(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	canonicalInitial, _, err := normalizeWorkspaceRoot(initial)
	if err != nil {
		t.Fatal(err)
	}
	canonicalOther, _, err := normalizeWorkspaceRoot(other)
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 2 ||
		roots[0] != canonicalInitial ||
		roots[1] != canonicalOther {
		t.Fatalf("Workspace roots = %#v", roots)
	}
	info, err := os.Stat(workspaceRegistryPath(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("Workspace registry permissions = %o", info.Mode().Perm())
	}

	reopened, err := newWorkspaceRuntimeManager(dataDir, initial)
	if err != nil {
		t.Fatal(err)
	}
	if len(reopened.roots) != 2 ||
		reopened.roots[0] != canonicalInitial ||
		reopened.roots[1] != canonicalOther {
		t.Fatalf("reopened Workspace roots = %#v", reopened.roots)
	}
}

func TestWorkspaceRegistryRejectsFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(path, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newWorkspaceRuntimeManager(t.TempDir(), path); err == nil {
		t.Fatal("file path was accepted as a Workspace")
	}
}

func TestWorkspaceRegistryHasNoImplicitInitialRoot(t *testing.T) {
	dataDir := t.TempDir()
	manager, err := newWorkspaceRuntimeManager(dataDir, "")
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := manager.List(t.Context())
	if err != nil || len(catalog.Workspaces) != 0 {
		t.Fatalf("empty registry=%+v err=%v", catalog, err)
	}
	added, err := manager.Add(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Remove(t.Context(), added.ID); err != nil {
		t.Fatal(err)
	}
	reopened, err := newWorkspaceRuntimeManager(dataDir, "")
	if err != nil || len(reopened.roots) != 0 {
		t.Fatalf("removed Workspace restored: err=%v", err)
	}
	if _, err := reopened.Add(t.Context(), dataDir); err == nil {
		t.Fatal("Supervisor state was accepted as a Workspace before configuration")
	}
}

func TestWorkspaceRegistryRemovePersistsAndIsIdempotent(t *testing.T) {
	dataDir := t.TempDir()
	initial := t.TempDir()
	other := t.TempDir()
	manager, err := newWorkspaceRuntimeManager(dataDir, initial)
	if err != nil {
		t.Fatal(err)
	}
	otherDescriptor, err := manager.Add(t.Context(), other)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := manager.Remove(t.Context(), otherDescriptor.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, initialIdentity, err := normalizeWorkspaceRoot(initial)
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Workspaces) != 1 ||
		catalog.Workspaces[0].ID != initialIdentity.RootID {
		t.Fatalf("Workspace catalog after removal = %+v", catalog)
	}
	if _, removeErr := manager.Remove(t.Context(), otherDescriptor.ID); removeErr != nil {
		t.Fatalf("idempotent Workspace removal: %v", removeErr)
	}
	reopened, err := newWorkspaceRuntimeManager(dataDir, initial)
	if err != nil {
		t.Fatal(err)
	}
	if len(reopened.roots) != 1 {
		t.Fatalf("reopened Workspace roots = %#v", reopened.roots)
	}
}

func TestWorkspaceRegistryAllowsRemovingLastWorkspace(t *testing.T) {
	dataDir := t.TempDir()
	initial := t.TempDir()
	manager, err := newWorkspaceRuntimeManager(dataDir, initial)
	if err != nil {
		t.Fatal(err)
	}
	_, identity, err := normalizeWorkspaceRoot(initial)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := manager.Remove(t.Context(), identity.RootID)
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Workspaces) != 0 {
		t.Fatalf("Workspace catalog after last removal = %+v", catalog)
	}
	roots, err := loadWorkspaceRoots(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 0 {
		t.Fatalf("persisted Workspace roots = %#v", roots)
	}
}

func TestWorkspaceRegistryRemovesMissingDirectory(t *testing.T) {
	dataDir := t.TempDir()
	initial := t.TempDir()
	other := t.TempDir()
	manager, err := newWorkspaceRuntimeManager(dataDir, initial)
	if err != nil {
		t.Fatal(err)
	}
	otherDescriptor, err := manager.Add(t.Context(), other)
	if err != nil {
		t.Fatal(err)
	}
	if removeErr := os.RemoveAll(other); removeErr != nil {
		t.Fatal(removeErr)
	}
	catalog, err := manager.Remove(t.Context(), otherDescriptor.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Workspaces) != 1 {
		t.Fatalf("Workspace catalog after stale removal = %+v", catalog)
	}
}

func TestRuntimeCredentialStagingStaysInTheDefaultNamespace(t *testing.T) {
	selection := webSetupSelection{
		Connections: []webSetupConnection{
			{ID: "openai", Provider: "openai"},
			{ID: "openai-compatible:2222", Provider: "openai-compatible:2222"},
		},
		DefaultConnection: "openai",
	}
	staged := &credentialRotation{
		control:   &credential.Control{},
		reference: credential.Reference{Kind: "keyring", Name: "staged-key"},
		phase:     credentialRotationStaged,
	}
	tests := []struct {
		name        string
		stagedOwner string
		passes      bool
	}{
		{
			name: "reconfigure staging has no owner", stagedOwner: "",
			passes: true,
		},
		{
			name:        "editing the default connection through connection/add",
			stagedOwner: "openai", passes: true,
		},
		{
			name:        "second connection must not reach the default runtime",
			stagedOwner: "openai-compatible:2222", passes: false,
		},
		{
			name:        "unknown connection owner is not the default",
			stagedOwner: "missing", passes: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := runtimeCredentialStaging(selection, test.stagedOwner, staged)
			if test.passes {
				if got != staged {
					t.Fatalf("default staging was filtered: %+v", got)
				}
				return
			}
			if got != nil {
				t.Fatalf("foreign staging reached the default runtime: %+v", got)
			}
		})
	}

	// A nil rotation stays nil for every owner; nothing is invented.
	if got := runtimeCredentialStaging(selection, "openai-compatible:2222", nil); got != nil {
		t.Fatalf("nil staging changed: %+v", got)
	}
}
