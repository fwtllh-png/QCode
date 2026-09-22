// Package environment is the sandbox execution-environment contract.
// It describes resources and compiles them toward authority.Resource.
// Core code has no ecosystem-name branches and does not import sandbox
// or process implementations.
package environment

import (
	"errors"
	"fmt"
	"strings"
)

const (
	ContractV1      = "v1"
	ProfileIsolated = "isolated"
	ProfileNative   = "native"
)

type Namespace string

const (
	NamespaceWorkspace      Namespace = "workspace"
	NamespaceSandboxHome    Namespace = "sandbox_home"
	NamespaceHostToolchain  Namespace = "host_toolchain"
	NamespaceHostConfig     Namespace = "host_config"
	NamespaceCache          Namespace = "cache"
	NamespaceSharedUserTemp Namespace = "shared_user_temp"
	NamespaceCredential     Namespace = "credential"
	NamespaceNetwork        Namespace = "network"
)

type Access string

const (
	AccessRead  Access = "read"
	AccessWrite Access = "write"
	AccessUse   Access = "use"
)

type ResourceRequest struct {
	Name      string    `json:"name"`
	Namespace Namespace `json:"namespace"`
	Access    Access    `json:"access"`
	Path      string    `json:"path,omitempty"`
	Host      string    `json:"host,omitempty"`
	Port      uint16    `json:"port,omitempty"`
	Protocol  string    `json:"protocol,omitempty"`
	Methods   []string  `json:"methods,omitempty"`
	Source    string    `json:"source"`
	Required  bool      `json:"required"`
	Lifecycle string    `json:"lifecycle"`
	Shared    bool      `json:"shared,omitempty"`
	Purpose   string    `json:"purpose,omitempty"`
	Env       string    `json:"env,omitempty"`
	Value     string    `json:"value,omitempty"`
	Tree      bool      `json:"tree,omitempty"`
}

type EnvironmentSpec struct {
	ID       string            `json:"id"`
	Version  string            `json:"version"`
	Contract string            `json:"contract"`
	Profile  string            `json:"profile"`
	Source   string            `json:"source"`
	Requests []ResourceRequest `json:"requests"`
}

func ChildProfile(parent string) string {
	_ = parent
	return ProfileIsolated
}

func ValidatePosture(profile string, sharedUserTemp bool) error {
	switch profile {
	case ProfileIsolated, ProfileNative:
	default:
		return fmt.Errorf("profile %q is invalid", profile)
	}
	if sharedUserTemp && profile != ProfileNative {
		return errors.New("shared_user_temp requires native profile")
	}
	return nil
}

func DefaultDeclarationLifecycle(namespace Namespace) string {
	switch namespace {
	case NamespaceCache, NamespaceSandboxHome:
		return "workspace"
	case NamespaceNetwork:
		return "grant_version"
	case NamespaceCredential:
		return "provider_rotation"
	case NamespaceSharedUserTemp:
		return "shared"
	default:
		return "source_version"
	}
}

func StampDeclaration(request ResourceRequest) ResourceRequest {
	request.Source = SourceUserDeclaration
	if strings.TrimSpace(request.Lifecycle) == "" {
		request.Lifecycle = DefaultDeclarationLifecycle(request.Namespace)
	}
	return request
}

func ValidateRequest(request ResourceRequest) error {
	if strings.TrimSpace(request.Name) == "" {
		return errors.New("resource request name is required")
	}
	switch request.Namespace {
	case NamespaceWorkspace, NamespaceSandboxHome, NamespaceHostToolchain,
		NamespaceHostConfig, NamespaceCache, NamespaceSharedUserTemp,
		NamespaceCredential, NamespaceNetwork:
	default:
		return fmt.Errorf("resource namespace %q is invalid", request.Namespace)
	}
	switch request.Access {
	case AccessRead, AccessWrite, AccessUse:
	default:
		return fmt.Errorf("resource access %q is invalid", request.Access)
	}
	if request.Namespace == NamespaceCredential && request.Access != AccessUse {
		return errors.New("credential resources must use access use")
	}
	if request.Access == AccessUse && request.Namespace != NamespaceCredential {
		return errors.New("access use is only valid for credential resources")
	}
	if request.Namespace == NamespaceSharedUserTemp && !request.Shared {
		return errors.New("shared_user_temp must declare shared=true")
	}
	if request.Namespace == NamespaceNetwork {
		if strings.TrimSpace(request.Host) == "" {
			return errors.New("network resource host is required")
		}
		if request.Port == 0 {
			return errors.New("network resource port is required")
		}
	}
	if strings.IndexByte(request.Path, 0) >= 0 ||
		strings.IndexByte(request.Host, 0) >= 0 ||
		strings.IndexByte(request.Source, 0) >= 0 ||
		strings.IndexByte(request.Env, 0) >= 0 ||
		strings.IndexByte(request.Value, 0) >= 0 {
		return errors.New("resource request must not contain NUL")
	}
	return nil
}

func ValidateSpec(spec EnvironmentSpec) error {
	if strings.TrimSpace(spec.ID) == "" {
		return errors.New("environment spec id is required")
	}
	switch spec.Contract {
	case ContractV1:
	default:
		return fmt.Errorf("environment contract %q is invalid", spec.Contract)
	}
	if err := ValidatePosture(spec.Profile, false); err != nil {
		return err
	}
	if len(spec.Requests) == 0 {
		return errors.New("environment spec requests are required")
	}
	seen := make(map[string]bool, len(spec.Requests))
	for _, request := range spec.Requests {
		if err := ValidateRequest(request); err != nil {
			return fmt.Errorf("%s: %w", request.Name, err)
		}
		if seen[request.Name] {
			return fmt.Errorf("duplicate resource request %q", request.Name)
		}
		seen[request.Name] = true
	}
	return nil
}
