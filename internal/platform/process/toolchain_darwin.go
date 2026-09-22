//go:build darwin

package process

import (
	"os"
	"path/filepath"
)

// darwinDeveloperToolsGit returns the directory of a real git binary from
// the macOS developer-tools layout. /usr/bin/git is a stub that can trigger
// an xcode-select or license prompt inside child processes; preferring the
// real installation keeps batch execution non-interactive. This is macOS
// platform layout knowledge, not a language toolchain special case.
var darwinDeveloperToolsGit = []string{
	"/Library/Developer/CommandLineTools/usr/bin/git",
	"/Applications/Xcode.app/Contents/Developer/usr/bin/git",
}

func ensureGitToolchain(environment []string) []string {
	if dir := gitToolchainDirectory(); dir != "" {
		return prependPATH(environment, dir)
	}
	return environment
}

// gitToolchainDirectory reports the developer-tools directory holding the
// platform git, or "". It sits after the toolchain bin directories in the
// child's PATH (see ToolchainSearchPath) so preflight verdicts and the
// child resolve the same binaries.
func gitToolchainDirectory() string {
	for _, candidate := range darwinDeveloperToolsGit {
		info, err := os.Stat(candidate)
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return filepath.Dir(candidate)
		}
	}
	return ""
}
