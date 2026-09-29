//go:build darwin

package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/fwtllh-png/QCode/internal/security/envpolicy"
)

func discoverPlatformToolchains(exposure *ToolchainExposure, workspace string, seen map[string]bool, sourceEnv []string) {
	sdk := strings.TrimSpace(envpolicy.Value(sourceEnv, "SDKROOT"))
	if sdk == "" {
		command := exec.Command("/usr/bin/xcrun", "--sdk", "macosx", "--show-sdk-path")
		command.Env = []string{"PATH=/usr/bin:/bin"}
		if developer := envpolicy.Value(sourceEnv, "DEVELOPER_DIR"); filepath.IsAbs(developer) {
			command.Env = append(command.Env, "DEVELOPER_DIR="+developer)
		}
		if output, err := command.Output(); err == nil {
			sdk = strings.TrimSpace(string(output))
		}
	}
	if !filepath.IsAbs(sdk) {
		return
	}
	sdk, err := filepath.EvalSymlinks(sdk)
	if err != nil {
		return
	}
	if _, err := os.Stat(filepath.Join(sdk, "SDKSettings.json")); err != nil {
		return
	}
	if canonical, ok := canonicalToolchainDirectory(sdk, workspace); ok {
		addToolchainReadDirectory(&exposure.ReadRoots, canonical, workspace, seen)
		exposure.Environment = append(exposure.Environment, "SDKROOT="+canonical)
	}
}

// PlatformDeveloperToolsDirectory avoids macOS launcher stubs that prompt or
// try to update host caches. These are documented platform installations.
func PlatformDeveloperToolsDirectory() string {
	for _, executable := range []string{
		"/Library/Developer/CommandLineTools/usr/bin/git",
		"/Applications/Xcode.app/Contents/Developer/usr/bin/git",
	} {
		if info, err := os.Stat(executable); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return filepath.Dir(executable)
		}
	}
	return ""
}
