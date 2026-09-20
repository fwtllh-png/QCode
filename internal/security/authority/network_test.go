package authority

import (
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/security/controlmatrix"
)

func TestCompileManagedNetworkRequiresProbedProxyAndDeclaredTarget(t *testing.T) {
	for _, test := range []struct {
		name      string
		proxy     bool
		target    bool
		port      uint16
		wantMode  string
		wantProxy uint16
	}{
		{"approved proxy target", true, true, 43128, "managed", 43128},
		{"no declared target", true, false, 43128, "denied", 0},
		{"no proxy listener", true, true, 0, "denied", 0},
		{"unprobed backend", false, true, 43128, "denied", 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := fixtureCompileInput(t)
			input.Capability.ManagedProxy = test.proxy
			input.SandboxPolicy.ManagedProxyPort = test.port
			if test.target {
				input.Invocation.Resources = append(input.Invocation.Resources, tool.Resource{
					Kind: "host", ID: "registry.npmjs.org", Protocol: "https",
					Port: 443, Methods: []string{"CONNECT"}, Access: tool.AccessWrite,
				})
			}
			profile, err := Compile(input)
			if err != nil {
				t.Fatal(err)
			}
			execution := profile.executionAuthority(RequiredControls{})
			if profile.Network.Mode != test.wantMode ||
				profile.Network.ProxyPort != test.wantProxy ||
				execution.ManagedProxyPort != test.wantProxy ||
				execution.AllowNetwork != (test.wantMode == "managed") {
				t.Fatalf("network = %+v, execution = %+v", profile.Network, execution)
			}
			wantControl := controlmatrix.NetworkDenied
			if test.wantMode == "managed" {
				wantControl = controlmatrix.NetworkProxyTargets
			}
			if profile.Controls.Network != wantControl {
				t.Fatalf("network controls = %q, want %q", profile.Controls.Network, wantControl)
			}
		})
	}
}
