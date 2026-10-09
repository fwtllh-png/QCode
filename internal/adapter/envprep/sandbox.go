package envprep

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/fwtllh-png/QCode/internal/common/environment"
	"github.com/fwtllh-png/QCode/internal/security/authority"
	"github.com/fwtllh-png/QCode/internal/security/envpolicy"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func materializeSandbox(prepared *PreparedEnvironment, bind BindContext, sourceEnv []string) error {
	options := &prepared.Sandbox
	// Preserve caller-owned diagnostics, Git roots and managed proxy options
	// without mutating their backing slices.
	options.HostReadRoots = append([]string(nil), options.HostReadRoots...)
	options.HostReadFiles = append([]string(nil), options.HostReadFiles...)
	options.HostWriteRoots = append([]string(nil), options.HostWriteRoots...)
	options.EnvironmentNetwork = append([]sandbox.EnvironmentNetworkTarget(nil), options.EnvironmentNetwork...)
	options.EnvironmentValues = append([]string(nil), options.EnvironmentValues...)
	home := envValue(sourceEnv, "HOME")
	env := map[string]string{}
	var baseline, declarations []string
	declarations = append(declarations, options.EnvironmentValues...)
	var writePaths []string
	for _, item := range prepared.Compiled {
		if !item.Bindable {
			if item.Request.Required {
				prepared.Facts = append(prepared.Facts, environment.Fact{
					Source:         environment.SourcePreparer,
					Category:       environment.CategoryEnvironmentResourceUnavailable,
					RequiredAction: actionFor(item.Request),
					Resource:       item.Request.Name,
					Detail:         item.Reason,
				})
			}
			continue
		}
		path := materializedPath(item, bind)
		if path != "" && item.Request.Access == environment.AccessWrite {
			writePaths = append(writePaths, path)
		}
		if item.Request.Namespace == environment.NamespaceNetwork &&
			item.Request.Source == environment.SourceUserDeclaration &&
			strings.TrimSpace(item.Request.Host) != "" {
			options.EnvironmentNetwork = append(options.EnvironmentNetwork, sandbox.EnvironmentNetworkTarget{
				Host: item.Request.Host, Protocol: item.Request.Protocol,
				Port: item.Request.Port, Methods: append([]string(nil), item.Request.Methods...),
			})
		}
		if name := strings.TrimSpace(item.Request.Env); name != "" &&
			!envpolicy.ManagedProxyEnvironmentName(name) {
			value := item.Request.Value
			if value == "" {
				value = path
			}
			entry := name + "=" + value
			if item.Request.Source == environment.SourceUserDeclaration {
				declarations = append(declarations, entry)
			} else {
				baseline = append(baseline, entry)
			}
		}
		if path == "" || !filepath.IsAbs(path) {
			continue
		}
		switch item.Request.Namespace {
		case environment.NamespaceHostConfig:
			if options.EnvironmentProfile != environment.ProfileNative &&
				home != "" && pathContains(home, path) {
				continue
			}
			appendHostRead(options, path)
		case environment.NamespaceHostToolchain:
			appendHostRead(options, path)
		case environment.NamespaceSharedUserTemp:
			options.HostWriteRoots = append(options.HostWriteRoots, path)
		}
	}
	if options.EnvironmentProfile == environment.ProfileIsolated && bind.SandboxHome != "" {
		env["HOME"] = bind.SandboxHome
	}
	temp := bind.SandboxHome
	if options.SharedUserTemp && bind.UserTemp != "" {
		temp = bind.UserTemp
	}
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		if temp != "" {
			env[name] = temp
		} else if env[name] == "" {
			if value := envValue(sourceEnv, name); value != "" {
				env[name] = value
			}
		}
	}
	var platformValues []string
	if options.Toolchains != nil {
		platformValues = options.Toolchains.Environment
	}
	values, err := envpolicy.Merge(
		envpolicy.SandboxDefaults(),
		envpolicy.WithoutManagedProxy(envpolicy.Baseline(sourceEnv)),
		platformValues, baseline, declarations, flattenEnv(env),
	)
	if err != nil {
		return err
	}
	if err := envpolicy.ValidatePreparedEnvironment(values); err != nil {
		return err
	}
	options.EnvironmentValues = envpolicy.WithoutManagedProxy(values)
	return ensureSandboxWriteTrees(writePaths, bind.SandboxHome)
}

func materializedPath(item CompiledResource, bind BindContext) string {
	var root string
	switch item.Resource.Namespace {
	case authority.NamespaceCache, authority.NamespaceSandboxHome:
		root = bind.SandboxHome
	case authority.NamespaceSharedUserTemp:
		root = bind.UserTemp
	case authority.NamespaceWorkspace:
		root = bind.WorkspaceRoot
	case authority.NamespaceHostConfig, authority.NamespaceHostToolchain:
		if filepath.IsAbs(item.Request.Path) {
			return item.Request.Path
		}
	}
	if root == "" {
		return ""
	}
	return filepath.Join(root, item.Resource.RelativePath)
}

func appendHostRead(options *sandbox.Options, path string) {
	info, err := os.Stat(path)
	if err == nil && info.IsDir() {
		options.HostReadRoots = append(options.HostReadRoots, path)
	} else {
		options.HostReadFiles = append(options.HostReadFiles, path)
	}
}

func actionFor(request environment.ResourceRequest) string {
	switch request.Namespace {
	case environment.NamespaceHostConfig:
		return environment.ActionApproveHostConfig
	case environment.NamespaceSharedUserTemp:
		return environment.ActionEnableSharedUserTemp
	case environment.NamespaceCredential:
		return environment.ActionBindCredential
	default:
		return ""
	}
}
