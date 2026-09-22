package environment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	envcontract "github.com/fwtllh-png/QCode/internal/environment"
	platformenv "github.com/fwtllh-png/QCode/internal/platform/environment"
	"github.com/fwtllh-png/QCode/internal/security/goproxy"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

// Go discovers ResourceRequests from public `go env` interfaces only.
type Go struct{}

func (Go) Name() string { return "go" }

// HostProxyEnv returns the host toolchain's GOPROXY value using the
// platform PATH. It is for the host-side auth service, not the sandbox.
func HostProxyEnv(ctx context.Context, sourceEnv []string) (string, error) {
	if len(sourceEnv) == 0 {
		sourceEnv = []string{
			"PATH=" + strings.Join(sandbox.PlatformPATHDirectories(), string(os.PathListSeparator)),
		}
	}
	executable, err := lookPathFromEnv(sourceEnv, "go")
	if err != nil {
		return "", err
	}
	return probeGoEnv(ctx, executable, "GOPROXY")
}

func (Go) Discover(
	ctx context.Context,
	input platformenv.DiscoverInput,
) ([]envcontract.ResourceRequest, []envcontract.Fact, error) {
	executable, err := lookPathFromEnv(input.SourceEnv, "go")
	if err != nil {
		return nil, []envcontract.Fact{{
			Source:         envcontract.SourcePreparer,
			Category:       envcontract.CategoryEnvironmentResourceUnavailable,
			RequiredAction: envcontract.ActionApproveHostConfig,
			Resource:       "go",
			Detail:         "go executable is not on PATH",
		}}, nil
	}
	goroot, err := probeGoEnv(ctx, executable, "GOROOT")
	if err != nil {
		return nil, nil, err
	}
	values, err := probeGoEnvJSON(ctx, executable)
	if err != nil {
		return nil, nil, err
	}
	goenv, err := probeGoEnv(ctx, executable, "GOENV")
	if err != nil {
		return nil, nil, err
	}
	requests := []envcontract.ResourceRequest{
		{
			Name: "go-executable", Namespace: envcontract.NamespaceHostToolchain,
			Access: envcontract.AccessRead, Path: executable,
			Source: "path", Required: true, Lifecycle: "source_version",
		},
	}
	if strings.TrimSpace(goroot) != "" {
		requests = append(requests, envcontract.ResourceRequest{
			Name: "go-root", Namespace: envcontract.NamespaceHostToolchain,
			Access: envcontract.AccessRead, Path: strings.TrimSpace(goroot),
			Env: "GOROOT", Source: "go-env-GOROOT", Required: true,
			Lifecycle: "source_version",
		})
	}
	for _, key := range []string{
		"GOPROXY", "GONOSUMDB", "GOSUMDB", "GOPRIVATE", "GO111MODULE",
	} {
		value := values[key]
		if key == "GOPROXY" {
			value = redactProxyUserinfo(value)
		}
		requests = append(requests, envcontract.ResourceRequest{
			Name:      "go-env-" + strings.ToLower(key),
			Namespace: envcontract.NamespaceHostConfig, Access: envcontract.AccessRead,
			Path: "env:" + key, Env: key, Value: value,
			Source: "go-env-json", Required: true, Lifecycle: "source_version",
		})
	}
	if path := strings.TrimSpace(goenv); path != "" && path != "off" && filepath.IsAbs(path) {
		requests = append(requests, envcontract.ResourceRequest{
			Name: "go-env-file", Namespace: envcontract.NamespaceHostConfig,
			Access: envcontract.AccessRead, Path: path, Env: "GOENV",
			Value: path, Source: "go-env-GOENV", Lifecycle: "live_host_file",
		})
	}
	if input.SandboxHome != "" {
		requests = append(requests,
			envcontract.ResourceRequest{
				Name: "go-mod-cache", Namespace: envcontract.NamespaceCache,
				Access: envcontract.AccessWrite, Path: "sandbox-home/cache/go-mod",
				Env: "GOMODCACHE", Tree: true, Source: "workspace-state",
				Required: true, Lifecycle: "workspace",
			},
			envcontract.ResourceRequest{
				Name: "go-build-cache", Namespace: envcontract.NamespaceCache,
				Access: envcontract.AccessWrite, Path: "sandbox-home/cache/go-build",
				Env: "GOCACHE", Tree: true, Source: "workspace-state",
				Required: true, Lifecycle: "workspace",
			},
			envcontract.ResourceRequest{
				Name: "go-tmp", Namespace: envcontract.NamespaceCache,
				Access: envcontract.AccessWrite, Path: "sandbox-home/cache/go-tmp",
				Env: "GOTMPDIR", Tree: true, Source: "workspace-state",
				Required: true, Lifecycle: "workspace",
			},
		)
	}
	network, facts := goproxyNetwork(values["GOPROXY"])
	requests = append(requests, network...)
	facts = append(facts, toolchainFacts(input.Workspace, values)...)
	return requests, facts, nil
}

func goproxyNetwork(raw string) ([]envcontract.ResourceRequest, []envcontract.Fact) {
	items := goproxy.SplitProxyList(redactProxyUserinfo(raw))
	if len(items) == 0 {
		return nil, nil
	}
	first := items[0]
	remainder := strings.Join(items[1:], ",")
	if first == "" || first == "off" || first == "direct" {
		return nil, nil
	}
	parsed, err := url.Parse(first)
	if err != nil || parsed.Hostname() == "" {
		return nil, nil
	}
	port := uint16(443)
	if parsed.Scheme == "http" {
		port = 80
	}
	if parsed.Port() != "" {
		value, convErr := strconv.ParseUint(parsed.Port(), 10, 16)
		if convErr == nil && value > 0 {
			port = uint16(value)
		}
	}
	requests := []envcontract.ResourceRequest{{
		Name: "goproxy-first", Namespace: envcontract.NamespaceNetwork,
		Access: envcontract.AccessRead, Host: parsed.Hostname(), Port: port,
		Protocol: parsed.Scheme, Methods: []string{"CONNECT"},
		Source: "goproxy-first-item", Required: true, Lifecycle: "grant_version",
	}, {
		Name: "goproxy-credential", Namespace: envcontract.NamespaceCredential,
		Access: envcontract.AccessUse, Host: parsed.Hostname(),
		Source: "adapter-declared-auth", Required: true, Lifecycle: "provider_rotation",
	}}
	var facts []envcontract.Fact
	if strings.Contains(remainder, "direct") {
		facts = append(facts, envcontract.Fact{
			Source:         envcontract.SourcePreparer,
			Category:       envcontract.CategoryNetworkTargetUnapproved,
			RequiredAction: envcontract.ActionApproveNetworkTarget,
			Resource:       "goproxy-direct-fallback",
			Detail:         "GOPROXY |direct fallback is not auto-granted",
		})
	}
	return requests, facts
}

func redactProxyUserinfo(raw string) string {
	items := goproxy.SplitProxyList(raw)
	for index, item := range items {
		parsed, err := url.Parse(item)
		if err != nil || parsed.User == nil {
			continue
		}
		parsed.User = nil
		items[index] = parsed.String()
	}
	return strings.Join(items, ",")
}

func lookPathFromEnv(sourceEnv []string, name string) (string, error) {
	pathValue := ""
	for _, entry := range sourceEnv {
		key, value, ok := strings.Cut(entry, "=")
		if ok && key == "PATH" {
			pathValue = value
			break
		}
	}
	if pathValue == "" {
		return exec.LookPath(name)
	}
	for _, directory := range filepath.SplitList(pathValue) {
		if directory == "" {
			continue
		}
		candidate := filepath.Join(directory, name)
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() && info.Mode().Perm()&0o111 != 0 {
			return candidate, nil
		}
	}
	return exec.LookPath(name)
}

func probeGoEnv(ctx context.Context, executable, name string) (string, error) {
	output, err := runGoEnv(ctx, executable, name)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(output), nil
}

func probeGoEnvJSON(ctx context.Context, executable string) (map[string]string, error) {
	output, err := runGoEnv(ctx, executable, "-json")
	if err != nil {
		return nil, err
	}
	values := map[string]string{}
	if err := json.Unmarshal([]byte(output), &values); err != nil {
		return nil, fmt.Errorf("parse go env -json: %w", err)
	}
	return values, nil
}

func runGoEnv(ctx context.Context, executable string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, sandbox.ToolchainProbeTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, executable, append([]string{"env"}, args...)...)
	command.Env = os.Environ()
	command.WaitDelay = sandbox.ToolchainProbeTimeout
	var output limitedBuffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("go env timed out")
		}
		return "", fmt.Errorf("go env %s: %w", strings.Join(args, " "), err)
	}
	return output.String(), nil
}

type limitedBuffer struct{ buffer bytes.Buffer }

func (b *limitedBuffer) Write(data []byte) (int, error) {
	if len(data) > sandbox.ToolchainProbeMaxOutputBytes-b.buffer.Len() {
		return 0, errors.New("toolchain metadata exceeds output limit")
	}
	return b.buffer.Write(data)
}

func (b *limitedBuffer) String() string { return b.buffer.String() }

// goManifestSearchDepth and goManifestSearchLimit bound the go.mod walk that
// collects toolchain requirements: vendored and build trees must not turn
// requirement collection into a full index. Public contract constants;
// boundary tests pin them.
const (
	goManifestSearchDepth = 4
	goManifestSearchLimit = 16
)

// goManifestSkips lists directories that never hold a module worth
// comparing toolchains against.
var goManifestSkips = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "target": true,
	"testdata": true,
}

// toolchainFacts compares the workspace's go.mod requirements with the host
// toolchain. A module needing a newer toolchain than the host installs does
// not fail discovery: GOTOOLCHAIN=auto fetches the required toolchain
// through GOPROXY at build time. The fact tells the model that path exists,
// so it does not hand-download toolchains into $TMPDIR.
func toolchainFacts(workspace string, values map[string]string) []envcontract.Fact {
	required, toolchain := moduleGoRequirement(workspace)
	if required == "" {
		return nil
	}
	host := strings.TrimSpace(values["GOVERSION"])
	if host == "" {
		return nil
	}
	need := required
	if toolchain != "" && compareGoVersion(toolchain, need) > 0 {
		need = toolchain
	}
	if compareGoVersion(need, host) <= 0 {
		return nil
	}
	display := strings.TrimPrefix(need, "go")
	detail := fmt.Sprintf(
		"workspace go.mod requires go >= %s but the host toolchain is %s. "+
			"GOTOOLCHAIN=auto switches to the required toolchain and fetches "+
			"it through GOPROXY (the session proxy serves toolchain "+
			"downloads) when network targets are declared.",
		display, host)
	action := ""
	if strings.EqualFold(strings.TrimSpace(values["GOTOOLCHAIN"]), "local") {
		detail += " GOTOOLCHAIN=local on this host disables auto-switching."
		action = envcontract.ActionApproveHostConfig
	}
	return []envcontract.Fact{{
		Source:         envcontract.SourcePreparer,
		Category:       envcontract.CategoryEnvironmentResourceUnavailable,
		Resource:       "go-toolchain",
		Detail:         detail,
		RequiredAction: action,
		HasSideEffects: false,
	}}
}

// moduleGoRequirement returns the highest go directive and the highest
// toolchain directive across workspace modules. Multi-module workspaces
// take the strictest requirement.
func moduleGoRequirement(workspace string) (goVersion, toolchain string) {
	if strings.TrimSpace(workspace) == "" {
		return "", ""
	}
	dirs := []string{}
	seen := make(map[string]bool)
	_ = filepath.WalkDir(workspace, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		relative, relErr := filepath.Rel(workspace, path)
		if relErr != nil {
			return nil
		}
		depth := 0
		if relative != "." {
			depth = strings.Count(filepath.ToSlash(relative), "/") + 1
		}
		if entry.IsDir() {
			if depth >= goManifestSearchDepth ||
				(depth > 0 && goManifestSkips[entry.Name()]) {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Name() != "go.mod" || len(seen) >= goManifestSearchLimit {
			return nil
		}
		seen[filepath.Dir(path)] = true
		dirs = append(dirs, path)
		return nil
	})
	for _, manifest := range dirs {
		body, err := os.ReadFile(manifest)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(body), "\n") {
			fields := strings.Fields(strings.TrimSpace(line))
			if len(fields) != 2 {
				continue
			}
			switch fields[0] {
			case "go":
				if compareGoVersion(fields[1], goVersion) > 0 {
					goVersion = fields[1]
				}
			case "toolchain":
				if compareGoVersion(fields[1], toolchain) > 0 {
					toolchain = fields[1]
				}
			}
		}
	}
	return goVersion, toolchain
}

// compareGoVersion orders Go version strings ("go1.26.3", "1.25", "1.21rc4").
// Non-numeric suffixes are ignored; rc ordering is not needed for directive
// comparison because directives use released versions.
func compareGoVersion(left, right string) int {
	parse := func(value string) [3]int {
		value = strings.TrimPrefix(strings.TrimSpace(value), "go")
		var parsed [3]int
		for index, part := range strings.SplitN(value, ".", 3) {
			if index >= 3 {
				break
			}
			digits := ""
			for _, char := range part {
				if char < '0' || char > '9' {
					break
				}
				digits += string(char)
			}
			parsed[index], _ = strconv.Atoi(digits)
		}
		return parsed
	}
	leftParts, rightParts := parse(left), parse(right)
	for index := 0; index < 3; index++ {
		if leftParts[index] != rightParts[index] {
			if leftParts[index] < rightParts[index] {
				return -1
			}
			return 1
		}
	}
	return 0
}
