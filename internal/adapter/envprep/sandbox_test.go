package envprep

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/common/environment"
	"github.com/fwtllh-png/QCode/internal/security/authority"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func TestDeclaredCredentialCannotClaimBinding(t *testing.T) {
	for _, required := range []bool{true, false} {
		prepared, err := Prepare(t.Context(), Options{
			Sandbox: sandbox.Options{WorkspaceRoot: t.TempDir(), PrivateTemp: t.TempDir(),
				EnvironmentProfile: environment.ProfileIsolated},
			SourceEnv: []string{},
			Declarations: []environment.ResourceRequest{{
				Name: "artifact-auth", Namespace: environment.NamespaceCredential,
				Access: environment.AccessUse, Host: "artifacts.example", Required: required,
				Source: environment.SourceUserDeclaration,
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		var found bool
		for _, item := range prepared.Compiled {
			if item.Request.Name == "artifact-auth" {
				found = true
				if item.Bindable || item.Reason != "credential_binder_unavailable" {
					t.Fatalf("undelivered credential: %+v", item)
				}
			}
		}
		if !found || len(prepared.Sandbox.EnvironmentNetwork) != 0 {
			t.Fatal("credential identity was lost or granted network access")
		}
		var missing bool
		for _, fact := range prepared.Facts {
			if fact.Resource == "artifact-auth" {
				missing = true
				if fact.Source != environment.SourcePreparer || fact.HasSideEffects ||
					fact.RequiredAction != environment.ActionBindCredential ||
					fact.Detail != "credential_binder_unavailable" {
					t.Fatalf("preparation fact = %+v", fact)
				}
			}
		}
		if missing != required {
			t.Fatalf("required=%v missing fact=%v", required, missing)
		}
	}
}

func TestMaterializeSandboxInheritsUserNetworkOnly(t *testing.T) {
	prepared := PreparedEnvironment{Sandbox: sandbox.Options{
		WorkspaceRoot:      t.TempDir(),
		PrivateTemp:        t.TempDir(),
		EnvironmentProfile: environment.ProfileIsolated,
	},
		Compiled: []CompiledResource{
			{
				Request: environment.ResourceRequest{
					Name: "user-proxy", Namespace: environment.NamespaceNetwork,
					Host: "declared.example", Port: 443, Protocol: "https",
					Methods: []string{"CONNECT"},
					Source:  environment.SourceUserDeclaration,
				},
				Resource: authority.Resource{Namespace: authority.NamespaceNetwork},
				Bindable: true,
			},
			{
				Request: environment.ResourceRequest{
					Name: "goproxy-first", Namespace: environment.NamespaceNetwork,
					Host: "proxy.golang.org", Port: 443, Protocol: "https",
					Methods: []string{"CONNECT"},
					Source:  "goproxy-first-item",
				},
				Bindable: true,
			},
			{
				Request: environment.ResourceRequest{
					Name: "go-root", Namespace: environment.NamespaceHostToolchain,
					Env: "GOROOT", Value: "/opt/go", Path: "/opt/go",
					Source: "go-env-GOROOT",
				},
				Resource: authority.Resource{Namespace: authority.NamespaceHostToolchain},
				Bindable: true,
			},
			{
				Request: environment.ResourceRequest{
					Name: "http-proxy", Namespace: environment.NamespaceHostConfig,
					Env: "HTTP_PROXY", Value: "http://proxy.example",
					Source: environment.SourceUserDeclaration,
				},
				Bindable: true,
			},
		},
	}
	if err := materializeSandbox(&prepared, BindContext{SandboxHome: prepared.Sandbox.PrivateTemp}, nil); err != nil {
		t.Fatal(err)
	}
	options := prepared.Sandbox
	if len(options.EnvironmentNetwork) != 1 ||
		options.EnvironmentNetwork[0].Host != "declared.example" {
		t.Fatalf("environment network = %+v", options.EnvironmentNetwork)
	}
	if !slices.Contains(options.EnvironmentValues, "GOROOT=/opt/go") {
		t.Fatalf("prepared env omitted GOROOT: %v", options.EnvironmentValues)
	}
	if envLookup(options.EnvironmentValues, "HOME") != options.PrivateTemp {
		t.Fatalf("isolated HOME = %v want %q", options.EnvironmentValues, options.PrivateTemp)
	}
	for _, entry := range options.EnvironmentValues {
		if strings.HasPrefix(entry, "HTTP_PROXY=") {
			t.Fatalf("managed proxy env leaked: %v", options.EnvironmentValues)
		}
	}
}

func TestMaterializeSandboxIsolatedHomeBeatsHostValue(t *testing.T) {
	sandboxHome := t.TempDir()
	options := sandbox.Options{
		WorkspaceRoot:      t.TempDir(),
		PrivateTemp:        sandboxHome,
		EnvironmentProfile: environment.ProfileIsolated,
	}
	prepared := PreparedEnvironment{
		Sandbox: options,
		Compiled: []CompiledResource{{
			Request: environment.ResourceRequest{
				Name: "home-value", Namespace: environment.NamespaceHostConfig,
				Env: "HOME", Value: "/host/home", Path: "env:HOME",
				Source: "startup-env",
			},
			Bindable: true,
		}},
	}
	if err := materializeSandbox(&prepared, BindContext{SandboxHome: sandboxHome}, []string{"HOME=/host/home"}); err != nil {
		t.Fatal(err)
	}
	options = prepared.Sandbox
	if envLookup(options.EnvironmentValues, "HOME") != sandboxHome {
		t.Fatalf("isolated HOME lost to host-value: %v", options.EnvironmentValues)
	}
	if envLookup(options.EnvironmentValues, "TMPDIR") != sandboxHome {
		t.Fatalf("isolated TMPDIR lost: %v", options.EnvironmentValues)
	}
}

func TestSandboxProjectionPreservesCallerBindingsAndFiltersSource(t *testing.T) {
	sourceHome := t.TempDir()
	hostConfig := filepath.Join(sourceHome, "tool.conf")
	outsideConfig := filepath.Join(t.TempDir(), "outside.conf")
	for _, path := range []string{hostConfig, outsideConfig} {
		if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", t.TempDir())
	for _, profile := range []string{environment.ProfileNative, environment.ProfileIsolated} {
		t.Run(profile, func(t *testing.T) {
			options := sandbox.Options{
				WorkspaceRoot: t.TempDir(), PrivateTemp: t.TempDir(),
				EnvironmentContract: environment.ContractV1, EnvironmentProfile: profile,
				HostReadRoots: []string{t.TempDir()}, HostReadFiles: []string{outsideConfig},
				HostWriteRoots: []string{t.TempDir()}, AllowNetwork: true,
				ManagedProxyPort: 32123, ManagedProxyCredential: "fixture",
				EnvironmentValues: []string{"EXISTING=value"},
			}
			prepared, err := Prepare(t.Context(), Options{
				Sandbox: options,
				SourceEnv: []string{
					"HOME=" + sourceHome, "UNDECLARED=value",
					"API_TOKEN=fixture", "HTTP_PROXY=http://source.example",
				},
				Declarations: []environment.ResourceRequest{
					{Name: "config", Namespace: environment.NamespaceHostConfig,
						Access: environment.AccessRead, Path: hostConfig},
					{Name: "proxy", Namespace: environment.NamespaceHostConfig,
						Access: environment.AccessRead, Env: "https_proxy", Value: "http://declared.example"},
					{Name: "unresolved", Namespace: environment.NamespaceHostConfig,
						Access: environment.AccessRead, Path: "relative.conf", Required: true},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			got := prepared.Sandbox
			if !slices.Contains(got.HostReadRoots, options.HostReadRoots[0]) ||
				!slices.Contains(got.HostReadFiles, outsideConfig) ||
				!slices.Equal(got.HostWriteRoots, options.HostWriteRoots) ||
				!got.AllowNetwork || got.ManagedProxyPort != options.ManagedProxyPort ||
				got.ManagedProxyCredential != options.ManagedProxyCredential ||
				envLookup(got.EnvironmentValues, "EXISTING") != "value" {
				t.Fatal("caller bindings were lost")
			}
			if slices.Contains(got.HostReadFiles, hostConfig) != (profile == environment.ProfileNative) {
				t.Fatalf("source HOME config filtering failed: %v", got.HostReadFiles)
			}
			for _, key := range []string{"UNDECLARED", "API_TOKEN", "HTTP_PROXY", "https_proxy"} {
				if envLookup(got.EnvironmentValues, key) != "" {
					t.Fatalf("unapproved source or managed proxy variable copied: %s", key)
				}
			}
			if len(prepared.Facts) != 1 || prepared.Facts[0].Resource != "unresolved" ||
				prepared.Facts[0].RequiredAction != environment.ActionApproveHostConfig {
				t.Fatalf("missing required resource facts: %+v", prepared.Facts)
			}
			got.HostReadRoots[0] = "changed"
			got.EnvironmentValues[0] = "changed"
			if options.HostReadRoots[0] == "changed" || options.EnvironmentValues[0] == "changed" {
				t.Fatal("projection mutated caller-owned slices")
			}
		})
	}
}
