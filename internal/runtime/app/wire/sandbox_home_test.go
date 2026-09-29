package wire

import (
	"slices"
	"strings"
	"testing"

	platformenv "github.com/fwtllh-png/QCode/internal/platform/environment"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func TestApplyPreparedSandboxOptionsInheritsUserNetworkOnly(t *testing.T) {
	options := sandbox.Options{
		WorkspaceRoot:      t.TempDir(),
		PrivateTemp:        t.TempDir(),
		EnvironmentProfile: platformenv.ProfileIsolated,
	}
	applyPreparedSandboxOptions(&options, platformenv.PreparedEnvironment{
		Env: []string{"HOME=/sandbox-home", "TMPDIR=/sandbox-home"},
		Compiled: []platformenv.CompiledResource{
			{
				Request: platformenv.ResourceRequest{
					Name: "user-proxy", Namespace: platformenv.NamespaceNetwork,
					Host: "declared.example", Port: 443, Protocol: "https",
					Methods: []string{"CONNECT"},
					Source:  platformenv.SourceUserDeclaration,
				},
				Bindable: true,
			},
			{
				Request: platformenv.ResourceRequest{
					Name: "goproxy-first", Namespace: platformenv.NamespaceNetwork,
					Host: "proxy.golang.org", Port: 443, Protocol: "https",
					Methods: []string{"CONNECT"},
					Source:  "goproxy-first-item",
				},
				Bindable: true,
			},
			{
				Request: platformenv.ResourceRequest{
					Name: "go-root", Namespace: platformenv.NamespaceHostToolchain,
					Env: "GOROOT", Value: "/opt/go", Path: "/opt/go",
					Source: "go-env-GOROOT",
				},
				Bindable: true,
			},
			{
				Request: platformenv.ResourceRequest{
					Name: "http-proxy", Namespace: platformenv.NamespaceHostConfig,
					Env: "HTTP_PROXY", Value: "http://proxy.example",
					Source: platformenv.SourceUserDeclaration,
				},
				Bindable: true,
			},
		},
	})
	if len(options.EnvironmentNetwork) != 1 ||
		options.EnvironmentNetwork[0].Host != "declared.example" {
		t.Fatalf("environment network = %+v", options.EnvironmentNetwork)
	}
	if !slices.Contains(options.EnvironmentValues, "GOROOT=/opt/go") {
		t.Fatalf("prepared env omitted GOROOT: %v", options.EnvironmentValues)
	}
	if environmentEntryValue(options.EnvironmentValues, "HOME") != options.PrivateTemp {
		t.Fatalf("isolated HOME = %v want %q", options.EnvironmentValues, options.PrivateTemp)
	}
	for _, entry := range options.EnvironmentValues {
		if strings.HasPrefix(entry, "HTTP_PROXY=") {
			t.Fatalf("managed proxy env leaked: %v", options.EnvironmentValues)
		}
	}
}

func TestApplyPreparedSandboxOptionsIsolatedHomeBeatsHostValue(t *testing.T) {
	sandboxHome := t.TempDir()
	options := sandbox.Options{
		WorkspaceRoot:      t.TempDir(),
		PrivateTemp:        sandboxHome,
		EnvironmentProfile: platformenv.ProfileIsolated,
	}
	applyPreparedSandboxOptions(&options, platformenv.PreparedEnvironment{
		Env: []string{"HOME=" + sandboxHome, "TMPDIR=" + sandboxHome},
		Compiled: []platformenv.CompiledResource{{
			Request: platformenv.ResourceRequest{
				Name: "home-value", Namespace: platformenv.NamespaceHostConfig,
				Env: "HOME", Value: "/host/home", Path: "env:HOME",
				Source: "startup-env",
			},
			Bindable: true,
		}},
	})
	if environmentEntryValue(options.EnvironmentValues, "HOME") != sandboxHome {
		t.Fatalf("isolated HOME lost to host-value: %v", options.EnvironmentValues)
	}
	if environmentEntryValue(options.EnvironmentValues, "TMPDIR") != sandboxHome {
		t.Fatalf("isolated TMPDIR lost: %v", options.EnvironmentValues)
	}
}
