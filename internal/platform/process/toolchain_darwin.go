//go:build darwin

package process

import "github.com/fwtllh-png/QCode/internal/security/sandbox"

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
	return sandbox.PlatformDeveloperToolsDirectory()
}
