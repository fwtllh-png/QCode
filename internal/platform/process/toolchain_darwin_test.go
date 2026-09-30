//go:build darwin

package process

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestShellRestoresSelectedGitToolchainAfterLoginProfile(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS login shell behavior")
	}
	const git = "/Library/Developer/CommandLineTools/usr/bin/git"
	if _, err := os.Stat(git); err != nil {
		t.Skip("Command Line Tools Git is unavailable")
	}
	result, err := Run(t.Context(), Options{
		Command: `printf '%s|%s\n' "$1" "$2"; command -v git`,
		Dir:     t.TempDir(),
		Env:     []string{"PATH=/usr/bin:/bin", "LANG=C"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 ||
		!strings.HasPrefix(result.Stdout, "|\n") ||
		!strings.Contains(result.Stdout, filepath.Dir(git)+"/git") {
		t.Fatalf("result = %+v", result)
	}
}
