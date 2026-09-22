package environment

import (
	"path/filepath"
	"strings"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/security/authority"
)

// BindContext supplies the roots needed to compile path-backed resources.
// Callers resolve these from Workspace state and platform interfaces; this
// package does not probe the OS or name ecosystems.
type BindContext struct {
	WorkspaceRoot       string
	WorkspaceID         string
	WorkspaceGeneration uint64
	SandboxHome         string
	UserTemp            string
}

// CompiledResource is the compile projection onto authority.Resource.
type CompiledResource struct {
	Request  ResourceRequest
	Resource authority.Resource
	Bindable bool
	Reason   string
}

func Compile(request ResourceRequest, bind BindContext) (CompiledResource, error) {
	if err := ValidateRequest(request); err != nil {
		return CompiledResource{}, err
	}
	compiled := CompiledResource{Request: request}
	switch request.Namespace {
	case NamespaceNetwork:
		return bindNetwork(request)
	case NamespaceCredential:
		return bindCredential(request)
	case NamespaceHostConfig:
		return bindHostConfig(request, bind)
	case NamespaceHostToolchain:
		return bindPathResource(request, bind, authority.NamespaceHostToolchain)
	case NamespaceWorkspace:
		return bindPathResource(request, bind, authority.NamespaceWorkspace)
	case NamespaceSandboxHome:
		return bindPathResource(request, bind, authority.NamespaceSandboxHome)
	case NamespaceCache:
		return bindPathResource(request, bind, authority.NamespaceCache)
	case NamespaceSharedUserTemp:
		return bindSharedUserTemp(request, bind)
	default:
		compiled.Reason = "namespace_not_bound"
		return compiled, nil
	}
}

func CompileSpec(spec EnvironmentSpec, bind BindContext) ([]CompiledResource, error) {
	if err := ValidateSpec(spec); err != nil {
		return nil, err
	}
	compiled := make([]CompiledResource, 0, len(spec.Requests))
	for _, request := range spec.Requests {
		item, err := Compile(request, bind)
		if err != nil {
			return nil, err
		}
		compiled = append(compiled, item)
	}
	return compiled, nil
}

func bindNetwork(request ResourceRequest) (CompiledResource, error) {
	access := tool.AccessRead
	if request.Access == AccessWrite {
		access = tool.AccessWrite
	}
	resource := authority.Resource{
		Namespace: authority.NamespaceNetwork,
		Kind:      "host",
		ID:        request.Host,
		Access:    access,
		Protocol:  request.Protocol,
		Port:      request.Port,
		Methods:   append([]string(nil), request.Methods...),
	}
	return finish(request, resource)
}

func bindCredential(request ResourceRequest) (CompiledResource, error) {
	id := strings.TrimSpace(request.Host)
	if id == "" {
		id = strings.TrimSpace(request.Name)
	}
	return finish(request, authority.Resource{
		Namespace: authority.NamespaceCredential,
		Kind:      "credential",
		ID:        id,
		Access:    tool.AccessUse,
	})
}

func bindHostConfig(request ResourceRequest, bind BindContext) (CompiledResource, error) {
	if envName := envIdentity(request); envName != "" {
		return finish(request, authority.Resource{
			Namespace: authority.NamespaceHostConfig,
			Kind:      "env",
			ID:        envName,
			Access:    tool.AccessRead,
		})
	}
	return bindPathResource(request, bind, authority.NamespaceHostConfig)
}

func bindSharedUserTemp(request ResourceRequest, bind BindContext) (CompiledResource, error) {
	root := strings.TrimSpace(bind.UserTemp)
	if root == "" {
		return unresolved(request, "path_root_unresolved"), nil
	}
	relative := strings.TrimSpace(request.Path)
	if relative == "" || relative == "." {
		relative = "."
	}
	return finish(request, authority.Resource{
		Namespace:      authority.NamespaceSharedUserTemp,
		Kind:           "directory",
		Access:         toolAccess(request.Access),
		Tree:           true,
		RootID:         authority.DigestString(root),
		RootGeneration: 1,
		RelativePath:   relative,
		ID:             root,
	})
}

func bindPathResource(
	request ResourceRequest,
	bind BindContext,
	namespace authority.ResourceNamespace,
) (CompiledResource, error) {
	root, relative, ok := resolvePath(request.Path, namespace, bind)
	if !ok {
		return unresolved(request, "path_root_unresolved"), nil
	}
	access := toolAccess(request.Access)
	tree := request.Tree ||
		namespace == authority.NamespaceCache ||
		namespace == authority.NamespaceSharedUserTemp ||
		namespace == authority.NamespaceSandboxHome
	kind := "file"
	if tree {
		kind = "directory"
	}
	return finish(request, authority.Resource{
		Namespace:      namespace,
		Kind:           kind,
		Access:         access,
		Tree:           tree,
		RootID:         authority.DigestString(root),
		RootGeneration: generationFor(namespace, bind),
		RelativePath:   relative,
		ID:             request.Name,
	})
}

func resolvePath(
	raw string,
	namespace authority.ResourceNamespace,
	bind BindContext,
) (root, relative string, ok bool) {
	path := strings.TrimSpace(raw)
	if path == "" {
		return "", "", false
	}
	switch namespace {
	case authority.NamespaceWorkspace:
		root = strings.TrimSpace(bind.WorkspaceRoot)
	case authority.NamespaceSandboxHome, authority.NamespaceCache:
		root = strings.TrimSpace(bind.SandboxHome)
		path = strings.TrimPrefix(path, "sandbox-home/")
		path = strings.TrimPrefix(path, "sandbox-home")
		if path == "" {
			path = "."
		}
	case authority.NamespaceSharedUserTemp:
		root = strings.TrimSpace(bind.UserTemp)
	case authority.NamespaceHostToolchain, authority.NamespaceHostConfig:
		if !filepath.IsAbs(path) {
			return "", "", false
		}
		root = filepath.Dir(path)
		return root, filepath.Base(path), root != ""
	default:
		root = strings.TrimSpace(bind.WorkspaceRoot)
	}
	if root == "" {
		return "", "", false
	}
	if filepath.IsAbs(path) {
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", "", false
		}
		return root, filepath.Clean(rel), true
	}
	return root, filepath.Clean(path), true
}

func envIdentity(request ResourceRequest) string {
	if name := strings.TrimSpace(request.Env); name != "" {
		return name
	}
	path := strings.TrimSpace(request.Path)
	if strings.HasPrefix(path, "env:") {
		return strings.TrimSpace(strings.TrimPrefix(path, "env:"))
	}
	return ""
}

func toolAccess(access Access) tool.AccessMode {
	switch access {
	case AccessWrite:
		return tool.AccessWrite
	case AccessUse:
		return tool.AccessUse
	default:
		return tool.AccessRead
	}
}

func generationFor(namespace authority.ResourceNamespace, bind BindContext) uint64 {
	if namespace == authority.NamespaceWorkspace && bind.WorkspaceGeneration != 0 {
		return bind.WorkspaceGeneration
	}
	return 1
}

func finish(request ResourceRequest, resource authority.Resource) (CompiledResource, error) {
	if err := resource.Validate(); err != nil {
		return CompiledResource{Request: request, Reason: err.Error()}, nil
	}
	return CompiledResource{Request: request, Resource: resource, Bindable: true}, nil
}

func unresolved(request ResourceRequest, reason string) CompiledResource {
	return CompiledResource{Request: request, Reason: reason}
}
