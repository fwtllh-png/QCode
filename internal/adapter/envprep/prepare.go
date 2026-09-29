// Package envprep adapts host environments into sandbox configuration.
// Platform binding, resource compilation and materialization share one source.
package envprep

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/fwtllh-png/QCode/internal/environment"
	"github.com/fwtllh-png/QCode/internal/security/envpolicy"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

type Options struct {
	Sandbox sandbox.Options
	Source  string
	// A nil SourceEnv captures the host environment once; an empty slice
	// explicitly supplies no host variables.
	SourceEnv    []string
	WorkspaceID  string
	Declarations []environment.ResourceRequest
}

type PreparedEnvironment struct {
	Spec     environment.EnvironmentSpec
	Compiled []CompiledResource
	// Sandbox is the sole execution projection. It never contains the raw
	// source environment; shared declaration safety checks apply at each boundary.
	Sandbox  sandbox.Options
	Facts    []environment.Fact
	UserTemp string
}

func UserTempDir() (string, error) {
	return platformUserTempDir()
}

func Prepare(ctx context.Context, options Options) (PreparedEnvironment, error) {
	if err := environment.ValidatePosture(options.Sandbox.EnvironmentProfile, options.Sandbox.SharedUserTemp); err != nil {
		return PreparedEnvironment{}, err
	}
	sourceEnv := append([]string{}, options.SourceEnv...)
	if options.SourceEnv == nil {
		sourceEnv = os.Environ()
	}
	if err := ctx.Err(); err != nil {
		return PreparedEnvironment{}, err
	}
	if err := envpolicy.ValidateDeclaredEnvironment(options.Sandbox.EnvironmentValues); err != nil {
		return PreparedEnvironment{}, err
	}
	for _, declaration := range options.Declarations {
		if name := strings.TrimSpace(declaration.Env); name != "" {
			if err := envpolicy.ValidateDeclaredEnvironment([]string{name + "=" + declaration.Value}); err != nil {
				return PreparedEnvironment{}, fmt.Errorf("%s: %w", declaration.Name, err)
			}
		}
	}
	var exposure sandbox.ToolchainExposure
	var platformValues []string
	if options.Sandbox.SkipPATHReadRoots && options.Sandbox.Toolchains != nil {
		exposure = *options.Sandbox.Toolchains
		platformValues = sourceEnv
	} else {
		var err error
		exposure, platformValues, err = sandbox.PreparePlatformEnvironment(options.Sandbox.WorkspaceRoot, sourceEnv)
		if err != nil {
			return PreparedEnvironment{}, err
		}
	}
	options.Sandbox.Toolchains = &exposure
	options.Sandbox.SkipPATHReadRoots = true
	sourceEnv = setSourceEnvValue(sourceEnv, "PATH", envpolicy.Value(platformValues, "PATH"))
	userTemp := ""
	if options.Sandbox.SharedUserTemp {
		resolved, err := UserTempDir()
		if err != nil {
			return PreparedEnvironment{}, err
		}
		userTemp = resolved
	}
	requests := coreRequests(options.Sandbox, sourceEnv, exposure.BinDirs, userTemp)
	for _, declaration := range options.Declarations {
		requests = append(requests, environment.StampDeclaration(declaration))
	}
	spec := environment.EnvironmentSpec{
		ID:       "workspace-environment",
		Version:  options.Source,
		Contract: options.Sandbox.EnvironmentContract,
		Profile:  options.Sandbox.EnvironmentProfile,
		Source:   options.Source,
		Requests: requests,
	}
	if spec.Contract == "" {
		spec.Contract = environment.ContractV1
	}
	if spec.Version == "" {
		spec.Version = "startup"
	}
	if spec.Source == "" {
		spec.Source = "startup"
	}
	bind := BindContext{
		WorkspaceRoot:       options.Sandbox.WorkspaceRoot,
		WorkspaceID:         options.WorkspaceID,
		WorkspaceGeneration: 1,
		SandboxHome:         options.Sandbox.PrivateTemp,
		UserTemp:            userTemp,
	}
	compiled, err := CompileSpec(spec, bind)
	if err != nil {
		return PreparedEnvironment{}, err
	}
	prepared := PreparedEnvironment{
		Spec:     spec,
		Compiled: compiled,
		Sandbox:  options.Sandbox,
		UserTemp: userTemp,
	}
	prepared.Sandbox.EnvironmentContract = spec.Contract
	if err := materializeSandbox(&prepared, bind, sourceEnv); err != nil {
		return PreparedEnvironment{}, err
	}
	return prepared, nil
}

func coreRequests(options sandbox.Options, sourceEnv, pathDirs []string, userTemp string) []environment.ResourceRequest {
	requests := []environment.ResourceRequest{
		{
			Name: "locale", Namespace: environment.NamespaceHostConfig,
			Access: environment.AccessRead, Path: "env:LANG", Env: "LANG",
			Value: envValue(sourceEnv, "LANG"), Source: "startup-env",
			Lifecycle: "source_version",
		},
		{
			Name: "home-value", Namespace: environment.NamespaceHostConfig,
			Access: environment.AccessRead, Path: "env:HOME", Env: "HOME",
			Value: envValue(sourceEnv, "HOME"), Source: "startup-env",
			Lifecycle: "source_version",
			Purpose:   "record home variable; does not authorize the directory",
		},
	}
	if options.PrivateTemp != "" {
		requests = append(requests, environment.ResourceRequest{
			Name: "sandbox-home", Namespace: environment.NamespaceSandboxHome,
			Access: environment.AccessWrite, Path: ".", Tree: true,
			Source: "workspace-state", Required: true, Lifecycle: "workspace",
		})
	}
	if options.SharedUserTemp && userTemp != "" {
		requests = append(requests, environment.ResourceRequest{
			Name: "shared-user-temp", Namespace: environment.NamespaceSharedUserTemp,
			Access: environment.AccessWrite, Shared: true, Tree: true,
			Source: "platform-user-temp", Lifecycle: "shared",
			Env: "TMPDIR",
		})
	}
	if pathValue := envValue(sourceEnv, "PATH"); pathValue != "" {
		requests = append(requests, environment.ResourceRequest{
			Name: "path-value", Namespace: environment.NamespaceHostConfig,
			Access: environment.AccessRead, Path: "env:PATH", Env: "PATH",
			Value: pathValue, Source: "platform-path", Lifecycle: "source_version",
		})
		for _, directory := range pathDirs {
			requests = append(requests, environment.ResourceRequest{
				Name:      "path-dir:" + directory,
				Namespace: environment.NamespaceHostToolchain,
				Access:    environment.AccessRead, Path: directory,
				Source: "platform-path", Lifecycle: "source_version",
			})
		}
	}
	for _, file := range options.Toolchains.ReadFiles {
		requests = append(requests, environment.ResourceRequest{
			Name:      "platform-file:" + file,
			Namespace: environment.NamespaceHostConfig, Access: environment.AccessRead,
			Path: file, Source: "platform-trust", Lifecycle: "source_version",
		})
	}
	return requests
}

func setSourceEnvValue(env []string, name, value string) []string {
	prefix := name + "="
	out := make([]string, 0, len(env)+1)
	replaced := false
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			if !replaced {
				out = append(out, prefix+value)
				replaced = true
			}
			continue
		}
		out = append(out, entry)
	}
	if !replaced {
		out = append(out, prefix+value)
	}
	return out
}

func envValue(env []string, name string) string {
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if ok && key == name {
			return value
		}
	}
	return ""
}

func flattenEnv(values map[string]string) []string {
	out := make([]string, 0, len(values))
	for name, value := range values {
		out = append(out, name+"="+value)
	}
	sort.Strings(out)
	return out
}

func ensureSandboxWriteTrees(paths []string, sandboxHome string) error {
	root := strings.TrimSpace(sandboxHome)
	if root == "" {
		return nil
	}
	for _, path := range paths {
		if path != root && !pathContains(root, path) {
			continue
		}
		if err := os.MkdirAll(path, 0o700); err != nil {
			return err
		}
	}
	return nil
}

func pathContains(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	return err == nil && relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
