package host

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOwnerLeaseRejectsConcurrentOwnerAndAllowsTakeoverAfterClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owner.lock")
	first, err := acquireOwnerLease(path, ownerLeaseMetadata{
		OwnerKind: "web", PublicURL: "http://127.0.0.1:1/",
		CapabilityToken: "private-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("owner lease permissions = %o", info.Mode().Perm())
	}

	_, err = acquireOwnerLease(path, ownerLeaseMetadata{OwnerKind: "web"})
	var held *ownerLeaseHeldError
	if !errors.As(err, &held) {
		t.Fatalf("second acquireOwnerLease error = %v, want ownerLeaseHeldError", err)
	}
	if held.Metadata.PublicURL != "http://127.0.0.1:1/" {
		t.Fatalf("held metadata = %#v", held.Metadata)
	}
	if held.Metadata.CapabilityToken != "private-token" {
		t.Fatalf("held capability token was not recovered")
	}
	if strings.Contains(held.Error(), "private-token") {
		t.Fatal("held owner error exposed the capability token")
	}

	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := acquireOwnerLease(path, ownerLeaseMetadata{OwnerKind: "web"})
	if err != nil {
		t.Fatalf("takeover after close: %v", err)
	}
	if err := next.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOwnerLeaseKeepsLockByteOutsideMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owner.lock")
	lease, err := acquireOwnerLease(path, ownerLeaseMetadata{OwnerKind: "web"})
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) <= ownerLeaseMetadataOffset || data[0] != 0 {
		t.Fatalf("lease metadata does not preserve the lock byte: %q", data)
	}
	var metadata ownerLeaseMetadata
	if err := json.Unmarshal(data[ownerLeaseMetadataOffset:], &metadata); err != nil {
		t.Fatalf("decode lease metadata: %v", err)
	}
	if metadata.OwnerKind != "web" {
		t.Fatalf("metadata = %#v", metadata)
	}
}

func TestOwnerLeaseRejectsSymbolicLink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "owner.lock")
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
	if _, err := acquireOwnerLease(path, ownerLeaseMetadata{OwnerKind: "web"}); err == nil {
		t.Fatal("expected symbolic link rejection")
	}
}

func TestOwnerLeasePathCanonicalizesDataDirectoryAliases(t *testing.T) {
	root := t.TempDir()
	physical := filepath.Join(root, "state")
	if err := os.Mkdir(physical, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "state-alias")
	if err := os.Symlink(physical, alias); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
	if got, want := ownerLeasePath(alias, "workspace"), ownerLeasePath(physical, "workspace"); got != want {
		t.Fatalf("lease path through alias = %q, want %q", got, want)
	}
}

func TestOwnerLeasePathCanonicalizesAliasBeforeMissingSuffix(t *testing.T) {
	root := t.TempDir()
	physical := filepath.Join(root, "state")
	if err := os.Mkdir(physical, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "state-alias")
	if err := os.Symlink(physical, alias); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
	if got, want := ownerLeasePath(
		filepath.Join(alias, "future"),
		"workspace",
	), ownerLeasePath(
		filepath.Join(physical, "future"),
		"workspace",
	); got != want {
		t.Fatalf("lease path through missing alias suffix = %q, want %q", got, want)
	}
}
