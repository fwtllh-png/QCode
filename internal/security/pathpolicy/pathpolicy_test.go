package pathpolicy

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestControlPlaneNamesAreSortedAndCaseInsensitive(t *testing.T) {
	names := ControlPlaneNames()
	if !slices.IsSorted(names) || len(names) != 5 {
		t.Fatalf("names = %v", names)
	}
	names[0] = "mutated"
	if ControlPlaneNames()[0] == "mutated" {
		t.Fatal("ControlPlaneNames exposed its backing table")
	}
	for spelled, want := range map[string]string{
		".GIT": GitDir, ".Qcode": StateDir, ".codex": CodexDir,
		".qcode-worktree": WorktreeDir, ".Agents": AgentsDir,
	} {
		if got, ok := ControlPlaneName(spelled); !ok || got != want {
			t.Fatalf("ControlPlaneName(%q) = %q, %v", spelled, got, ok)
		}
	}
	for _, ordinary := range []string{".github", "git", ".qcodex", ""} {
		if _, ok := ControlPlaneName(ordinary); ok {
			t.Fatalf("%q classified as control plane", ordinary)
		}
	}
}

func TestInCredentialLocationMatchesSegmentsNotSubstrings(t *testing.T) {
	home := "/Users/dev"
	for _, path := range []string{
		"/Users/dev/.ssh",
		"/Users/dev/.ssh/config",
		"/srv/build/.SSH/known_hosts",
		"/Users/dev/.aws",
		"/Users/dev/.aws/sso/cache",
		"/Users/dev/Library/Keychains/login.keychain-db",
		"/Users/dev/.kube/config",
		"/Users/dev/.docker/config.json",
		"/Users/dev/.config/gh/hosts.yml",
		"/opt/app/secrets/token",
		"/Users/dev/.aws/credentials",
	} {
		if !InCredentialLocation(path, home) {
			t.Fatalf("%q not recognised as a credential location", path)
		}
	}
	for _, path := range []string{
		"/Users/dev/.config/ghostty/config",
		"/Users/dev/src/secrets-manager",
		"/Users/dev/.sshrc",
		"/Users/dev/ordinary.conf",
		"/srv/.aws",
	} {
		if InCredentialLocation(path, home) {
			t.Fatalf("%q misclassified as a credential location", path)
		}
	}
	if InCredentialLocation("/Users/dev/.aws", "") {
		t.Fatal("home-anchored entry matched without a home")
	}
}

func TestHomeCredentialRootsCoverEveryLocation(t *testing.T) {
	roots := HomeCredentialRoots("/Users/dev")
	if len(roots) != len(CredentialLocations()) {
		t.Fatalf("roots = %v", roots)
	}
	for _, want := range []string{
		"/Users/dev/.ssh", "/Users/dev/.aws", "/Users/dev/.gnupg",
		"/Users/dev/Library/Keychains", "/Users/dev/.config/gh",
	} {
		if !slices.Contains(roots, want) {
			t.Fatalf("roots = %v, missing %s", roots, want)
		}
	}
	for _, root := range roots {
		if !InCredentialLocation(root, "/Users/dev") {
			t.Fatalf("root %q is not itself a credential location", root)
		}
	}
	if HomeCredentialRoots("") != nil {
		t.Fatal("roots without a home")
	}
}

func TestIsCredentialFileName(t *testing.T) {
	for _, name := range []string{".netrc", ".NPMRC", "id_ed25519", ".git-credentials"} {
		if !IsCredentialFileName(name) {
			t.Fatalf("%q not recognised", name)
		}
	}
	if IsCredentialFileName("id_ed25519.pub") || IsCredentialFileName("config") {
		t.Fatal("public key or ordinary file recognised as a credential")
	}
}

func TestCanonicalAllowMissingResolvesExistingPrefix(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	resolvedReal, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	got, err := CanonicalAllowMissing(filepath.Join(root, "link", "a", "b.txt"))
	if err != nil || got != filepath.Join(resolvedReal, "a", "b.txt") {
		t.Fatalf("got %q, %v", got, err)
	}
	got, err = CanonicalAllowMissing(filepath.Join(root, "link"))
	if err != nil || got != resolvedReal {
		t.Fatalf("existing path = %q, %v", got, err)
	}
}
