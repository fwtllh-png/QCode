package config

import (
	"strings"

	platformenv "github.com/fwtllh-png/QCode/internal/platform/environment"
)

func (e ExecutionEnvironment) DeclaredRequests() []platformenv.ResourceRequest {
	if len(e.Resources) == 0 {
		return nil
	}
	requests := make([]platformenv.ResourceRequest, 0, len(e.Resources))
	for _, resource := range e.Resources {
		requests = append(requests, resource.Request())
	}
	return requests
}

func (r EnvironmentResource) Request() platformenv.ResourceRequest {
	required := true
	if r.Required != nil {
		required = *r.Required
	}
	access := platformenv.Access(strings.TrimSpace(r.Access))
	if access == "" {
		access = defaultDeclarationAccess(platformenv.Namespace(r.Namespace))
	}
	return platformenv.StampDeclaration(platformenv.ResourceRequest{
		Name:      r.Name,
		Namespace: platformenv.Namespace(r.Namespace),
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

func defaultDeclarationAccess(namespace platformenv.Namespace) platformenv.Access {
	switch namespace {
	case platformenv.NamespaceCredential:
		return platformenv.AccessUse
	case platformenv.NamespaceCache, platformenv.NamespaceSandboxHome,
		platformenv.NamespaceSharedUserTemp:
		return platformenv.AccessWrite
	default:
		return platformenv.AccessRead
	}
}

func cloneEnvironmentAuthServices(services []EnvironmentAuthService) []EnvironmentAuthService {
	if services == nil {
		return nil
	}
	cloned := make([]EnvironmentAuthService, len(services))
	for index, service := range services {
		service.Prefixes = append([]string(nil), service.Prefixes...)
		cloned[index] = service
	}
	return cloned
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
