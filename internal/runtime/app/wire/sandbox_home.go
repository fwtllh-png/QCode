package wire

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	adapterenv "github.com/fwtllh-png/QCode/internal/adapter/environment"
	"github.com/fwtllh-png/QCode/internal/config"
	envcontract "github.com/fwtllh-png/QCode/internal/environment"
	platformenv "github.com/fwtllh-png/QCode/internal/platform/environment"
	"github.com/fwtllh-png/QCode/internal/security/authority"
	"github.com/fwtllh-png/QCode/internal/security/egress"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func newWorkspaceSandbox(
	state *buildState,
	helperPath string,
) (sandbox.Backend, []envcontract.Fact, error) {
	privateHome := ""
	if state.config.workspaceStateRoot != "" {
		privateHome = filepath.Join(
			state.config.workspaceStateRoot,
			"sandbox-home",
		)
	}
	environment := state.config.execution.Environment
	options, prepareFacts, err := bindEnvironmentSandbox(
		sandbox.Options{
			WorkspaceRoot:       state.config.execution.Workspace,
			HelperPath:          helperPath,
			PrivateTemp:         privateHome,
			HostReadRoots:       append([]string(nil), state.config.diagnosticReadRoots...),
			HostReadFiles:       append([]string(nil), state.config.diagnosticReadFiles...),
			EnvironmentContract: environment.Contract,
			EnvironmentProfile:  environment.Profile,
			SharedUserTemp:      environment.SharedUserTemp,
		},
		environment,
		state.config.workspaceStateID,
		privateHome,
	)
	if err != nil {
		return nil, nil, err
	}
	backend, err := egress.NewManagedBackend(
		state.platform.processEgress, options, newPlatformBackend,
	)
	if err != nil {
		return nil, nil, err
	}
	return backend, prepareFacts, nil
}

func bindEnvironmentSandbox(
	options sandbox.Options,
	environment config.ExecutionEnvironment,
	workspaceID, sandboxHome string,
) (sandbox.Options, []envcontract.Fact, error) {
	prepared, err := platformenv.Prepare(context.Background(), platformenv.Options{
		Contract:       environment.Contract,
		Profile:        options.EnvironmentProfile,
		SharedUserTemp: options.SharedUserTemp,
		Source:         environment.Source,
		WorkspaceRoot:  options.WorkspaceRoot,
		WorkspaceID:    workspaceID,
		SandboxHome:    sandboxHome,
		Declarations:   environment.DeclaredRequests(),
		Discoverers:    []platformenv.Discoverer{adapterenv.Go{}, adapterenv.Git{}},
	})
	if err != nil {
		return sandbox.Options{}, nil, err
	}
	applyPreparedSandboxOptions(&options, prepared)
	return options, prepared.Facts, nil
}

func applyPreparedSandboxOptions(
	options *sandbox.Options,
	prepared platformenv.PreparedEnvironment,
) {
	home, _ := os.UserHomeDir()
	env := map[string]string{}
	for _, item := range prepared.Compiled {
		if !item.Bindable {
			continue
		}
		if item.Request.Namespace == envcontract.NamespaceNetwork &&
			item.Request.Source == envcontract.SourceUserDeclaration &&
			strings.TrimSpace(item.Request.Host) != "" {
			options.EnvironmentNetwork = append(
				options.EnvironmentNetwork,
				sandbox.EnvironmentNetworkTarget{
					Host:     item.Request.Host,
					Protocol: item.Request.Protocol,
					Port:     item.Request.Port,
					Methods:  append([]string(nil), item.Request.Methods...),
				},
			)
		}
		if name := strings.TrimSpace(item.Request.Env); name != "" &&
			!managedProxyEnvironmentName(name) {
			value := item.Request.Value
			if value == "" {
				value = preparedPath(item, *options)
			}
			if value != "" {
				env[name] = value
			}
		}
		path := preparedPath(item, *options)
		if path == "" || !filepath.IsAbs(path) {
			continue
		}
		switch item.Request.Namespace {
		case envcontract.NamespaceHostConfig:
			if options.EnvironmentProfile != envcontract.ProfileNative &&
				pathUnder(home, path) {
				continue
			}
			if isDirectory(path) {
				options.HostReadRoots = append(options.HostReadRoots, path)
			} else {
				options.HostReadFiles = append(options.HostReadFiles, path)
			}
		case envcontract.NamespaceHostToolchain:
			if isDirectory(path) {
				options.HostReadRoots = append(options.HostReadRoots, path)
			} else {
				options.HostReadFiles = append(options.HostReadFiles, path)
			}
		case envcontract.NamespaceSharedUserTemp:
			options.HostWriteRoots = append(options.HostWriteRoots, path)
		}
	}
	if options.EnvironmentProfile == envcontract.ProfileIsolated &&
		options.PrivateTemp != "" {
		env["HOME"] = options.PrivateTemp
	} else if value := environmentEntryValue(prepared.Env, "HOME"); value != "" {
		env["HOME"] = value
	}
	if !options.SharedUserTemp && options.PrivateTemp != "" {
		env["TMPDIR"] = options.PrivateTemp
		env["TMP"] = options.PrivateTemp
		env["TEMP"] = options.PrivateTemp
	} else {
		for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
			if value := environmentEntryValue(prepared.Env, name); value != "" {
				env[name] = value
			}
		}
	}
	for name, value := range env {
		options.EnvironmentValues = append(options.EnvironmentValues, name+"="+value)
	}
}

func preparedPath(item envcontract.CompiledResource, options sandbox.Options) string {
	if item.Request.Path != "" && filepath.IsAbs(item.Request.Path) {
		return item.Request.Path
	}
	if item.Resource.ID != "" && filepath.IsAbs(item.Resource.ID) {
		return item.Resource.ID
	}
	switch item.Resource.Namespace {
	case authority.NamespaceCache, authority.NamespaceSandboxHome:
		if options.PrivateTemp == "" {
			return ""
		}
		if item.Resource.RelativePath == "." {
			return options.PrivateTemp
		}
		return filepath.Join(options.PrivateTemp, item.Resource.RelativePath)
	}
	return ""
}

func pathUnder(parent, child string) bool {
	if strings.TrimSpace(parent) == "" {
		return false
	}
	relative, err := filepath.Rel(parent, child)
	return err == nil && relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func isDirectory(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func environmentEntryValue(environment []string, name string) string {
	prefix := name + "="
	for _, entry := range environment {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix)
		}
	}
	return ""
}

func managedProxyEnvironmentName(name string) bool {
	switch strings.ToUpper(name) {
	case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY":
		return true
	default:
		return false
	}
}
