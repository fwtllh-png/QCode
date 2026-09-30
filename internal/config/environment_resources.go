package config

import (
	"strings"

	"github.com/fwtllh-png/QCode/internal/common/environment"
)

func (e ExecutionEnvironment) DeclaredRequests() []environment.ResourceRequest {
	if len(e.Resources) == 0 {
		return nil
	}
	requests := make([]environment.ResourceRequest, 0, len(e.Resources))
	for _, resource := range e.Resources {
		requests = append(requests, resource.Request())
	}
	return requests
}

func (r EnvironmentResource) Request() environment.ResourceRequest {
	required := true
	if r.Required != nil {
		required = *r.Required
	}
	access := environment.Access(strings.TrimSpace(r.Access))
	if access == "" {
		access = defaultDeclarationAccess(environment.Namespace(r.Namespace))
	}
	return environment.StampDeclaration(environment.ResourceRequest{
		Name:      r.Name,
		Namespace: environment.Namespace(r.Namespace),
		Access:    access,
		Path:      r.Path,
		Host:      r.Host,
		Port:      r.Port,
		Protocol:  r.Protocol,
		Methods:   append([]string(nil), r.Methods...),
		Required:  required,
		Lifecycle: r.Lifecycle,
		Shared:    r.Shared,
		Purpose:   r.Purpose,
		Env:       r.Env,
		Value:     r.Value,
		Tree:      r.Tree,
	})
}

func defaultDeclarationAccess(namespace environment.Namespace) environment.Access {
	switch namespace {
	case environment.NamespaceCredential:
		return environment.AccessUse
	case environment.NamespaceCache, environment.NamespaceSandboxHome,
		environment.NamespaceSharedUserTemp:
		return environment.AccessWrite
	default:
		return environment.AccessRead
	}
}

func cloneEnvironmentResources(resources []EnvironmentResource) []EnvironmentResource {
	if resources == nil {
		return nil
	}
	cloned := make([]EnvironmentResource, len(resources))
	for index, resource := range resources {
		resource.Methods = append([]string(nil), resource.Methods...)
		if resource.Required != nil {
			required := *resource.Required
			resource.Required = &required
		}
		cloned[index] = resource
	}
	return cloned
}
