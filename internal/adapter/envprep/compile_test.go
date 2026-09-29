package envprep

import (
	"reflect"
	"testing"

	"github.com/fwtllh-png/QCode/internal/environment"
	"github.com/fwtllh-png/QCode/internal/security/authority"
	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
)

func TestCompileSpecPreservesGenericDeclarations(t *testing.T) {
	for _, test := range []struct {
		name          string
		request       environment.ResourceRequest
		want          authority.Resource
		unboundReason string
	}{
		{
			name: "required network target",
			request: environment.ResourceRequest{Name: "artifact-source", Namespace: environment.NamespaceNetwork, Access: environment.AccessRead,
				Host: "artifacts.example", Port: 443, Protocol: "https", Methods: []string{"CONNECT"}, Required: true},
			want: authority.Resource{Namespace: authority.NamespaceNetwork, Kind: "host", ID: "artifacts.example", Access: securitymodel.Read,
				Port: 443, Protocol: "https", Methods: []string{"CONNECT"}},
		},
		{
			name: "optional network target",
			request: environment.ResourceRequest{Name: "optional-source", Namespace: environment.NamespaceNetwork, Access: environment.AccessRead,
				Host: "mirror.example", Port: 443, Protocol: "https", Methods: []string{"CONNECT"}, Required: false},
			want: authority.Resource{Namespace: authority.NamespaceNetwork, Kind: "host", ID: "mirror.example", Access: securitymodel.Read,
				Port: 443, Protocol: "https", Methods: []string{"CONNECT"}},
		},
		{
			name: "credential identity without binder",
			request: environment.ResourceRequest{Name: "artifact-auth", Namespace: environment.NamespaceCredential, Access: environment.AccessUse,
				Host: "artifacts.example", Required: true},
			want:          authority.Resource{Namespace: authority.NamespaceCredential, Kind: "credential", ID: "artifacts.example", Access: securitymodel.Use},
			unboundReason: "credential_binder_unavailable",
		},
		{
			name: "environment path identity",
			request: environment.ResourceRequest{Name: "tool-endpoint", Namespace: environment.NamespaceHostConfig, Access: environment.AccessRead,
				Path: "env:TOOL_ENDPOINT", Required: true},
			want: authority.Resource{Namespace: authority.NamespaceHostConfig, Kind: "env", ID: "TOOL_ENDPOINT", Access: securitymodel.Read},
		},
		{
			name: "explicit environment value",
			request: environment.ResourceRequest{Name: "tool-mode", Namespace: environment.NamespaceHostConfig, Access: environment.AccessRead,
				Env: "TOOL_MODE", Value: "offline", Required: true},
			want: authority.Resource{Namespace: authority.NamespaceHostConfig, Kind: "env", ID: "TOOL_MODE", Access: securitymodel.Read},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := environment.StampDeclaration(test.request)
			compiled, err := CompileSpec(environment.EnvironmentSpec{
				ID: "generic-resources", Version: "1", Contract: environment.ContractV1, Profile: environment.ProfileNative,
				Requests: []environment.ResourceRequest{request},
			}, BindContext{})
			if err != nil {
				t.Fatal(err)
			}
			if len(compiled) != 1 {
				t.Fatalf("compiled resources = %d, want 1", len(compiled))
			}
			got := compiled[0]
			if !reflect.DeepEqual(got.Request, request) {
				t.Fatalf("declaration metadata changed: %+v, want %+v", got.Request, request)
			}
			if got.Bindable != (test.unboundReason == "") || got.Reason != test.unboundReason || !reflect.DeepEqual(got.Resource, test.want) {
				t.Fatalf("compiled resource = %+v, want resource=%+v unboundReason=%q", got, test.want, test.unboundReason)
			}
		})
	}
}

func TestCompileSpecRequiresResolvedPathRoots(t *testing.T) {
	home, userTemp := t.TempDir(), t.TempDir()
	for _, test := range []struct {
		name string
		bind BindContext
	}{
		{name: "roots unavailable"},
		{name: "roots resolved", bind: BindContext{SandboxHome: home, UserTemp: userTemp}},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, path := range []struct {
				request        environment.ResourceRequest
				namespace      authority.ResourceNamespace
				root, relative string
			}{
				{
					request: environment.ResourceRequest{Name: "tool-cache", Namespace: environment.NamespaceCache, Access: environment.AccessWrite,
						Path: "sandbox-home/cache/tool", Env: "TOOL_CACHE", Tree: true, Required: true},
					namespace: authority.NamespaceCache, root: test.bind.SandboxHome, relative: "cache/tool",
				},
				{
					request: environment.ResourceRequest{Name: "shared-temp", Namespace: environment.NamespaceSharedUserTemp, Access: environment.AccessWrite,
						Shared: true, Required: false},
					namespace: authority.NamespaceSharedUserTemp, root: test.bind.UserTemp, relative: ".",
				},
			} {
				t.Run(path.request.Name, func(t *testing.T) {
					request := environment.StampDeclaration(path.request)
					compiled, err := CompileSpec(environment.EnvironmentSpec{
						ID: "generic-paths", Version: "1", Contract: environment.ContractV1, Profile: environment.ProfileNative,
						Requests: []environment.ResourceRequest{request},
					}, test.bind)
					if err != nil {
						t.Fatal(err)
					}
					if len(compiled) != 1 {
						t.Fatalf("compiled resources = %d, want 1", len(compiled))
					}
					got := compiled[0]
					if !reflect.DeepEqual(got.Request, request) {
						t.Fatalf("path declaration changed: %+v, want %+v", got.Request, request)
					}
					if path.root == "" {
						if got.Bindable || got.Reason != "path_root_unresolved" {
							t.Fatalf("unresolved path bound: %+v", got)
						}
						return
					}
					if !got.Bindable || got.Reason != "" || got.Resource.Namespace != path.namespace ||
						got.Resource.Kind != "directory" || got.Resource.Access != securitymodel.Write || !got.Resource.Tree ||
						got.Resource.RootID != authority.DigestString(path.root) || got.Resource.RootGeneration != 1 || got.Resource.RelativePath != path.relative {
						t.Fatalf("path bound incorrectly: %+v", got)
					}
				})
			}
		})
	}
}
