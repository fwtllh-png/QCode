package authority

import (
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
)

func TestCompileManagedNetworkRequiresProbedProxyAndDeclaredTarget(t *testing.T) {
	for _, test := range []struct {
		name      string
		proxy     bool
		target    bool
		port      uint16
		want      securitymodel.Network
		wantProxy uint16
	}{
		{"approved proxy target", true, true, 43128, securitymodel.NetworkProxyTargets, 43128},
		{"no declared target", true, false, 43128, securitymodel.NetworkDenied, 0},
		{"no proxy listener", true, true, 0, securitymodel.NetworkDenied, 0},
		{"unprobed backend", false, true, 43128, securitymodel.NetworkDenied, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := fixtureCompileInput(t)
			input.Capability.ManagedProxy = test.proxy
			input.SandboxPolicy.ManagedProxyPort = test.port
			if test.target {
				appendFixtureResources(&input.Prepared, tool.Resource{
					Kind: "host", ID: "registry.npmjs.org", Protocol: "https",
					Port: 443, Methods: []string{"CONNECT"}, Access: tool.AccessWrite,
				})
			}
			profile, err := compileProfileForTest(input)
			if err != nil {
				t.Fatal(err)
			}
			execution := profile.executionAuthority(RequiredControls{})
			if profile.Controls.Network != test.want ||
				profile.Network.ProxyPort != test.wantProxy ||
				execution.ManagedProxyPort != test.wantProxy ||
				execution.AllowNetwork != (test.want != securitymodel.NetworkDenied) {
				t.Fatalf("network = %+v controls = %s, execution = %+v",
					profile.Network, profile.Controls.Network, execution)
			}
		})
	}
}
