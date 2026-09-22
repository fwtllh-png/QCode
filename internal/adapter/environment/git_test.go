package environment

import (
	"os"
	"path/filepath"
	"testing"

	platformenv "github.com/fwtllh-png/QCode/internal/platform/environment"
)

func TestGitDiscoversExistingUserConfigFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GIT_CONFIG_GLOBAL", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	gitconfig := filepath.Join(home, ".gitconfig")
	xdg := filepath.Join(home, ".config", "git")
	if err := os.MkdirAll(xdg, 0o755); err != nil {
		t.Fatal(err)
	}
	xdgConfig := filepath.Join(xdg, "config")
	if err := os.WriteFile(gitconfig, []byte("[user]\n\tname = Fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(xdgConfig, []byte("[init]\n\tdefaultBranch = main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".git-credentials"), []byte("https://x:y@example.test\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	requests, facts, err := Git{}.Discover(t.Context(), platformenv.DiscoverInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 0 {
		t.Fatalf("facts = %+v", facts)
	}
	got := map[string]bool{}
	for _, request := range requests {
		if request.Namespace != "host_config" || request.Access != "read" {
			t.Fatalf("request = %+v", request)
		}
		got[request.Path] = true
	}
	if !got[gitconfig] || !got[xdgConfig] {
		t.Fatalf("discovered = %v", got)
	}
	if got[filepath.Join(home, ".git-credentials")] {
		t.Fatal("git-credentials was discovered")
	}
}

func TestGitSkipsMissingAndNullGlobalConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	requests, _, err := Git{}.Discover(t.Context(), platformenv.DiscoverInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 0 {
		t.Fatalf("requests = %+v", requests)
	}
}
