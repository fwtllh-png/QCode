package shell

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/environment"
	securitypolicy "github.com/fwtllh-png/QCode/internal/security/policy"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

// preflightExecutables fails fast when a static command segment resolves to
// a binary the sandbox profile cannot read. The verdict comes from the
// policy's read-set model — a backend fact, never command output — and
// replaces the bare EPERM errno the child would surface with a structured
// denial that names the executable and the required action.
func (p *commandProtocol) preflightExecutables(
	backend sandbox.Backend,
	command string,
	directory string,
	coveredPaths []string,
) (tool.Result, bool) {
	policyValue, ok := sandbox.BackendPolicy(backend)
	if !ok {
		return tool.Result{}, false
	}
	analysis, err := securitypolicy.AnalyzeCommand(command)
	if err != nil {
		return tool.Result{}, false
	}
	additional := make([]string, 0, len(coveredPaths))
	for _, relative := range coveredPaths {
		resolved, resolveErr := p.workspace.Resolve(relative, sandbox.AllowMissing)
		if resolveErr != nil {
			continue
		}
		additional = append(additional, resolved)
	}
	search := append(
		append([]string(nil), policyValue.Toolchains.BinDirs...),
		filepath.SplitList(os.Getenv("PATH"))...,
	)
	for _, segment := range analysis.Segments {
		if segment.Dynamic || len(segment.Argv) == 0 {
			continue
		}
		candidate, resolvable := resolveSegmentExecutable(
			segment.Argv[0], directory, search,
		)
		if !resolvable {
			// Shell builtins and genuinely missing programs are reported
			// by the shell itself; prediction stops here.
			continue
		}
		if policyValue.ExecutableReadable(candidate, additional) {
			continue
		}
		return tool.Result{
			Content: fmt.Sprintf(
				"sandbox cannot read executable %s: it is outside the sandbox "+
					"read roots. Declare the toolchain resource "+
					"(host_config or host_toolchain) that grants it, or use a "+
					"tool from a directory the environment contract already "+
					"covers.",
				candidate,
			),
			IsError: true,
			Metadata: map[string]any{
				"error_category":  environment.CategoryFilesystemAccessDenied,
				"required_action": environment.ActionApproveHostConfig,
				"executable":      candidate,
				"retry_original":  false,
			},
		}, true
	}
	return tool.Result{}, false
}

// resolveSegmentExecutable resolves one static argv[0] the way the child
// shell would: path-shaped names against the child directory, bare names
// against the child's effective PATH (discovered toolchain directories
// first).
func resolveSegmentExecutable(
	name string, directory string, search []string,
) (string, bool) {
	qualified := name
	if !filepath.IsAbs(name) && strings.Contains(name, "/") {
		qualified = filepath.Join(directory, name)
	}
	if qualified != name || filepath.IsAbs(name) {
		cleaned := filepath.Clean(qualified)
		if isExecutableFile(cleaned) {
			return cleaned, true
		}
		return "", false
	}
	for _, pathDirectory := range search {
		if pathDirectory == "" {
			continue
		}
		candidate := filepath.Join(pathDirectory, name)
		if isExecutableFile(candidate) {
			return candidate, true
		}
	}
	return "", false
}

func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode().Perm()&0o111 != 0
}
