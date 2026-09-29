package sandbox

import (
	"os"
	"strings"

	"github.com/fwtllh-png/QCode/internal/security/envpolicy"
)

// PreparePlatformEnvironment binds platform paths, SDK and public trust files
// using one explicit source. Only selected execution values leave this call.
// Discovery does not depend on language names or project manifests.
func PreparePlatformEnvironment(workspace string, sourceEnv []string) (ToolchainExposure, []string, error) {
	exposure := discoverToolchains(workspace, nil, nil, sourceEnv)
	if err := configuredCertificateFiles(&exposure, workspace, sourceEnv); err != nil {
		return ToolchainExposure{}, nil, err
	}
	discoverCertificateFiles(&exposure, workspace)
	directories := append([]string(nil), exposure.BinDirs...)
	if directory := PlatformDeveloperToolsDirectory(); directory != "" {
		// Bind the real platform binaries at preparation time, before launcher stubs.
		addToolchainDirectory(&exposure.BinDirs, directory, workspace, nil)
		directories = append([]string{directory}, directories...)
	}
	values, err := envpolicy.Merge(
		envpolicy.WithoutManagedProxy(envpolicy.Baseline(sourceEnv)),
		exposure.Environment,
		[]string{"PATH=" + strings.Join(directories, string(os.PathListSeparator))},
	)
	return exposure, values, err
}
