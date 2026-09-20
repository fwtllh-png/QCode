// Package envprobe reports the environment fingerprint used by the base
// system prompt: host platform, login shell, and the toolchain versions the
// workspace's PATH exposes. Probes are bounded and cached per process, so a
// wedged PATH entry cannot delay every session construction.
package envprobe

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// probeTimeout bounds one version probe. Version calls return in
// milliseconds; the bound exists so a hung entry point on PATH fails fast
// instead of delaying session construction. Boundary tests lock the value.
const probeTimeout = 2 * time.Second

// Runner executes one command and returns its combined output. It exists so
// tests can substitute a fake; production uses exec.CommandContext.
type Runner func(ctx context.Context, name string, args ...string) (string, error)

// CommandRunner is the production runner.
func CommandRunner(ctx context.Context, name string, args ...string) (string, error) {
	output, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(output), err
}

// Tool is one version probe. Label names the tool in the fingerprint; the
// probe's first output line is its version.
type Tool struct {
	Name    string
	Version string // version flag, for example "--version" or "version"
	Label   string
}

// DefaultTools are the toolchains a coding agent most often needs to name
// when it writes commands: version control and the common language runtimes.
var DefaultTools = []Tool{
	{Name: "git", Version: "--version", Label: "git"},
	{Name: "go", Version: "version", Label: "go"},
	{Name: "node", Version: "--version", Label: "node"},
	{Name: "python3", Version: "--version", Label: "python"},
}

var (
	once   sync.Once
	cached []string
)

// Fingerprint returns the process-cached environment lines.
func Fingerprint() []string {
	once.Do(func() {
		cached = Probe(CommandRunner)
	})
	return cached
}

// Probe collects the fingerprint with the given runner: host platform, login
// shell, and one version line per tool that is both on PATH and answers
// within the probe timeout. Tools that are missing or silent are omitted
// rather than reported as absent versions.
func Probe(runner Runner) []string {
	lines := []string{
		"os: " + runtime.GOOS + " (" + runtime.GOARCH + ")",
	}
	if shell := shellPath(); shell != "" {
		lines = append(lines, "shell: "+shell)
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	for _, tool := range DefaultTools {
		output, err := runner(ctx, tool.Name, tool.Version)
		if err != nil {
			continue
		}
		version := firstLine(output)
		if version == "" {
			continue
		}
		lines = append(lines, tool.Label+" "+version)
	}
	return lines
}

func firstLine(output string) string {
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// shellPath reports the login shell. The SHELL variable is empty on platforms
// without one, and the line is omitted there instead of guessing.
func shellPath() string {
	return os.Getenv("SHELL")
}
