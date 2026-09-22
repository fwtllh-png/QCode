package environment

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"

	envcontract "github.com/fwtllh-png/QCode/internal/environment"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

type Discoverer interface {
	Name() string
	Discover(context.Context, DiscoverInput) ([]envcontract.ResourceRequest, []envcontract.Fact, error)
}

type DiscoverInput struct {
	SourceEnv   []string
	Workspace   string
	SandboxHome string
	UserTemp    string
}

type Options struct {
	Contract       string
	Profile        string
	SharedUserTemp bool
	Source         string
	SourceEnv      []string
	WorkspaceRoot  string
	WorkspaceID    string
	SandboxHome    string
	Declarations   []envcontract.ResourceRequest
	Discoverers    []Discoverer
}

type PreparedEnvironment struct {
	Spec        envcontract.EnvironmentSpec
	Compiled    []envcontract.CompiledResource
	Env         []string
	ReadPaths   []string
	WritePaths  []string
	Facts       []envcontract.Fact
	UserTemp    string
	SandboxHome string
}

func UserTempDir() (string, error) {
	return platformUserTempDir()
}

func Prepare(ctx context.Context, options Options) (PreparedEnvironment, error) {
	if err := envcontract.ValidatePosture(options.Profile, options.SharedUserTemp); err != nil {
		return PreparedEnvironment{}, err
	}
	sourceEnv := append([]string(nil), options.SourceEnv...)
	if len(sourceEnv) == 0 {
		sourceEnv = os.Environ()
	}
	if pathValue, _ := effectivePATH(sourceEnv); pathValue != "" {
		sourceEnv = setSourceEnvValue(sourceEnv, "PATH", pathValue)
	}
	userTemp := ""
	if options.SharedUserTemp {
		resolved, err := UserTempDir()
		if err != nil {
			return PreparedEnvironment{}, err
		}
		userTemp = resolved
	}
	requests := coreRequests(options, sourceEnv, userTemp)
	for _, declaration := range options.Declarations {
		requests = append(requests, envcontract.StampDeclaration(declaration))
	}
	var facts []envcontract.Fact
	for _, discoverer := range options.Discoverers {
		if discoverer == nil {
			continue
		}
		discovered, extra, err := discoverer.Discover(ctx, DiscoverInput{
			SourceEnv:   sourceEnv,
			Workspace:   options.WorkspaceRoot,
			SandboxHome: options.SandboxHome,
			UserTemp:    userTemp,
		})
		if err != nil {
			return PreparedEnvironment{}, err
		}
		requests = append(requests, discovered...)
		facts = append(facts, extra...)
	}
	spec := envcontract.EnvironmentSpec{
		ID:       "workspace-environment",
		Version:  options.Source,
		Contract: options.Contract,
		Profile:  options.Profile,
		Source:   options.Source,
		Requests: dedupeRequests(requests),
	}
	if spec.Contract == "" {
		spec.Contract = envcontract.ContractV1
	}
	if spec.Version == "" {
		spec.Version = "startup"
	}
	if spec.Source == "" {
		spec.Source = "startup"
	}
	if spec.ID == "" {
		spec.ID = "workspace-environment"
	}
	compiled, err := envcontract.CompileSpec(spec, envcontract.BindContext{
		WorkspaceRoot:       options.WorkspaceRoot,
		WorkspaceID:         options.WorkspaceID,
		WorkspaceGeneration: 1,
		SandboxHome:         options.SandboxHome,
		UserTemp:            userTemp,
	})
	if err != nil {
		return PreparedEnvironment{}, err
	}
	prepared := PreparedEnvironment{
		Spec:        spec,
		Compiled:    compiled,
		Env:         append([]string(nil), sourceEnv...),
		SandboxHome: options.SandboxHome,
		UserTemp:    userTemp,
		Facts:       facts,
	}
	materialize(&prepared, options)
	if err := ensureSandboxWriteTrees(prepared.WritePaths, options.SandboxHome); err != nil {
		return PreparedEnvironment{}, err
	}
	return prepared, nil
}

func coreRequests(options Options, sourceEnv []string, userTemp string) []envcontract.ResourceRequest {
	requests := []envcontract.ResourceRequest{
		{
			Name: "locale", Namespace: envcontract.NamespaceHostConfig,
			Access: envcontract.AccessRead, Path: "env:LANG", Env: "LANG",
			Value: envValue(sourceEnv, "LANG"), Source: "startup-env",
			Lifecycle: "source_version",
		},
		{
			Name: "home-value", Namespace: envcontract.NamespaceHostConfig,
			Access: envcontract.AccessRead, Path: "env:HOME", Env: "HOME",
			Value: envValue(sourceEnv, "HOME"), Source: "startup-env",
			Lifecycle: "source_version",
			Purpose:   "record home variable; does not authorize the directory",
		},
	}
	if options.SandboxHome != "" {
		requests = append(requests, envcontract.ResourceRequest{
			Name: "sandbox-home", Namespace: envcontract.NamespaceSandboxHome,
			Access: envcontract.AccessWrite, Path: ".", Tree: true,
			Source: "workspace-state", Required: true, Lifecycle: "workspace",
		})
	}
	if options.SharedUserTemp && userTemp != "" {
		requests = append(requests, envcontract.ResourceRequest{
			Name: "shared-user-temp", Namespace: envcontract.NamespaceSharedUserTemp,
			Access: envcontract.AccessWrite, Shared: true, Tree: true,
			Source: "platform-user-temp", Lifecycle: "shared",
			Env: "TMPDIR",
		})
	}
	if pathValue, pathDirs := effectivePATH(sourceEnv); pathValue != "" {
		requests = append(requests, envcontract.ResourceRequest{
			Name: "path-value", Namespace: envcontract.NamespaceHostConfig,
			Access: envcontract.AccessRead, Path: "env:PATH", Env: "PATH",
			Value: pathValue, Source: "platform-path", Lifecycle: "source_version",
		})
		for _, directory := range pathDirs {
			requests = append(requests, envcontract.ResourceRequest{
				Name: "path-dir:" + directory,
				Namespace: envcontract.NamespaceHostToolchain,
				Access:    envcontract.AccessRead, Path: directory,
				Source: "platform-path", Lifecycle: "source_version",
			})
		}
	}
	for _, file := range discoveredCertificateFiles(options.WorkspaceRoot) {
		requests = append(requests, envcontract.ResourceRequest{
			Name:      "certificate:" + filepath.Base(file),
			Namespace: envcontract.NamespaceHostConfig, Access: envcontract.AccessRead,
			Path: file, Source: "openssl-version-d", Lifecycle: "source_version",
		})
	}
	return requests
}

func discoveredCertificateFiles(workspace string) []string {
	exposure := sandbox.ToolchainExposure{}
	for _, directory := range sandbox.PlatformPATHDirectories() {
		if directory != "" && filepath.IsAbs(directory) {
			exposure.BinDirs = append(exposure.BinDirs, directory)
		}
	}
	sandbox.DiscoverCertificateFiles(&exposure, workspace)
	return append([]string(nil), exposure.ReadFiles...)
}

func materialize(prepared *PreparedEnvironment, options Options) {
	env := map[string]string{}
	for _, entry := range prepared.Env {
		name, value, ok := strings.Cut(entry, "=")
		if ok {
			env[name] = value
		}
	}
	for _, item := range prepared.Compiled {
		if !item.Bindable {
			if item.Request.Required {
				prepared.Facts = append(prepared.Facts, envcontract.Fact{
					Source:         envcontract.SourcePreparer,
					Category:       envcontract.CategoryEnvironmentResourceUnavailable,
					RequiredAction: actionFor(item.Request),
					Resource:       item.Request.Name,
				})
			}
			continue
		}
		path := materializedPath(item, options)
		if path != "" {
			switch item.Request.Access {
			case envcontract.AccessWrite:
				prepared.WritePaths = append(prepared.WritePaths, path)
				prepared.ReadPaths = append(prepared.ReadPaths, path)
			default:
				prepared.ReadPaths = append(prepared.ReadPaths, path)
			}
		}
		if name := strings.TrimSpace(item.Request.Env); name != "" {
			if item.Request.Value != "" {
				env[name] = item.Request.Value
			} else if path != "" {
				env[name] = path
			}
		} else if envName := envIdentity(item.Request); envName != "" && item.Request.Value != "" {
			env[envName] = item.Request.Value
		}
	}
	if options.Profile == envcontract.ProfileIsolated && options.SandboxHome != "" {
		env["HOME"] = options.SandboxHome
	}
	if options.SharedUserTemp && prepared.UserTemp != "" {
		env["TMPDIR"] = prepared.UserTemp
		env["TMP"] = prepared.UserTemp
		env["TEMP"] = prepared.UserTemp
	} else if options.SandboxHome != "" {
		env["TMPDIR"] = options.SandboxHome
		env["TMP"] = options.SandboxHome
		env["TEMP"] = options.SandboxHome
	}
	prepared.Env = flattenEnv(env)
}

func materializedPath(item envcontract.CompiledResource, options Options) string {
	switch item.Resource.Namespace {
	case "cache", "sandbox_home":
		if options.SandboxHome == "" {
			return ""
		}
		if item.Resource.RelativePath == "." {
			return options.SandboxHome
		}
		return filepath.Join(options.SandboxHome, item.Resource.RelativePath)
	case "shared_user_temp":
		return optionsUserTemp(options, item)
	case "host_config", "host_toolchain":
		if filepath.IsAbs(item.Request.Path) {
			return item.Request.Path
		}
	case "workspace":
		if options.WorkspaceRoot == "" {
			return ""
		}
		if item.Resource.RelativePath == "." {
			return options.WorkspaceRoot
		}
		return filepath.Join(options.WorkspaceRoot, item.Resource.RelativePath)
	}
	return ""
}

func optionsUserTemp(_ Options, item envcontract.CompiledResource) string {
	if item.Resource.ID != "" && filepath.IsAbs(item.Resource.ID) {
		return item.Resource.ID
	}
	return ""
}

func actionFor(request envcontract.ResourceRequest) string {
	switch request.Namespace {
	case envcontract.NamespaceHostConfig:
		return envcontract.ActionApproveHostConfig
	case envcontract.NamespaceSharedUserTemp:
		return envcontract.ActionEnableSharedUserTemp
	case envcontract.NamespaceCredential:
		return envcontract.ActionBindCredential
	default:
		return ""
	}
}

func envIdentity(request envcontract.ResourceRequest) string {
	if name := strings.TrimSpace(request.Env); name != "" {
		return name
	}
	if strings.HasPrefix(request.Path, "env:") {
		return strings.TrimPrefix(request.Path, "env:")
	}
	return ""
}

func effectivePATH(sourceEnv []string) (string, []string) {
	seen := make(map[string]bool)
	var dirs []string
	add := func(directory string) {
		if directory == "" || !filepath.IsAbs(directory) || seen[directory] {
			return
		}
		info, err := os.Stat(directory)
		if err != nil || !info.IsDir() {
			return
		}
		canonical, err := filepath.EvalSymlinks(directory)
		if err != nil {
			return
		}
		canonical = filepath.Clean(canonical)
		if seen[canonical] {
			return
		}
		seen[canonical] = true
		dirs = append(dirs, canonical)
	}
	for _, directory := range filepath.SplitList(envValue(sourceEnv, "PATH")) {
		add(directory)
	}
	for _, directory := range sandbox.PlatformPATHDirectories() {
		add(directory)
	}
	if len(dirs) == 0 {
		return "", nil
	}
	return strings.Join(dirs, string(os.PathListSeparator)), dirs
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

func dedupeRequests(requests []envcontract.ResourceRequest) []envcontract.ResourceRequest {
	seen := make(map[string]bool, len(requests))
	out := make([]envcontract.ResourceRequest, 0, len(requests))
	for _, request := range requests {
		if seen[request.Name] {
			continue
		}
		seen[request.Name] = true
		out = append(out, request)
	}
	return out
}
