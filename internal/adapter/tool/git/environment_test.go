package git

import (
	"os"
	"path/filepath"
	"testing"
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

	source := os.Environ()
	// Discovery must keep using the supplied snapshot after the host changes.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	requests := EnvironmentRequests(source)
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
	requests := EnvironmentRequests(os.Environ())
	if len(requests) != 0 {
		t.Fatalf("requests = %+v", requests)
	}
}

func TestGitUsesExplicitConfigLocationsFromSnapshot(t *testing.T) {
	global := filepath.Join(t.TempDir(), "config")
	xdg := t.TempDir()
	xdgFile := filepath.Join(xdg, "git", "config")
	if err := os.MkdirAll(filepath.Dir(xdgFile), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{global, xdgFile} {
		if err := os.WriteFile(path, []byte("[user]\nname=Fixture\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	requests := EnvironmentRequests([]string{"GIT_CONFIG_GLOBAL=" + global, "XDG_CONFIG_HOME=" + xdg})
	if len(requests) != 2 || requests[0].Path != global || requests[1].Path != xdgFile || requests[0].Name == requests[1].Name {
		t.Fatalf("explicit snapshot requests=%v", requests)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	requests = EnvironmentRequests([]string{})
	if len(requests) != 0 {
		t.Fatalf("empty snapshot inherited host config: requests=%v", requests)
	}
}
