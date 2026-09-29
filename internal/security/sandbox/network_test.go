package sandbox

import (
	"testing"

	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
)

func TestManagedNetworkControlsRequireProbedSupport(t *testing.T) {
	for _, test := range []struct {
		name      string
		platform  string
		available bool
		proxy     bool
		want      securitymodel.Network
	}{
		{"probed seatbelt", "darwin", true, true, securitymodel.NetworkProxyTargets},
		{"unprobed seatbelt", "darwin", true, false, securitymodel.NetworkDenied},
		{"unavailable", "darwin", false, true, securitymodel.NetworkDenied},
		{"unsupported backend", "unsupported", true, false, securitymodel.NetworkDirect},
	} {
		t.Run(test.name, func(t *testing.T) {
			capability := Capability{
				Platform: test.platform, Available: test.available,
				Effective: platformControls(test.platform), ManagedProxy: test.proxy,
			}
			policy := Policy{ManagedProxyPort: 43128}
			if got := EffectiveControls(capability, policy).Network; got != test.want {
				t.Fatalf("network = %q, want %q", got, test.want)
			}
			denied, err := CommandControls(capability, policy, Command{DenyNetwork: true})
			if err != nil {
				t.Fatal(err)
			}
			if capability.Effective.Network == securitymodel.NetworkDenied &&
				denied.Network != securitymodel.NetworkDenied {
				t.Fatalf("local command was granted network: %+v", denied)
			}
		})
	}
}

func TestCommandNetworkPolicySeparatesLoopbackFromManagedEgress(t *testing.T) {
	for _, test := range []struct {
		name      string
		proxyPort uint16
		command   Command
		want      securitymodel.Network
		wantPort  uint16
	}{
		{
			name: "managed", proxyPort: 43128,
			want: securitymodel.NetworkProxyTargets, wantPort: 43128,
		},
		{
			name: "managed and loopback", proxyPort: 43128,
			command: Command{AllowLoopback: true},
			want:    securitymodel.NetworkProxyTargets, wantPort: 43128,
		},
		{
			name: "loopback only on managed backend", proxyPort: 43128,
			command: Command{AllowLoopback: true, LoopbackOnly: true},
			want:    securitymodel.NetworkLoopbackAny,
		},
		{
			name:    "loopback without proxy",
			command: Command{AllowLoopback: true, LoopbackOnly: true},
			want:    securitymodel.NetworkLoopbackAny,
		},
		{
			name: "denial overrides loopback and proxy", proxyPort: 43128,
			command: Command{DenyNetwork: true, AllowLoopback: true, LoopbackOnly: true},
			want:    securitymodel.NetworkDenied,
		},
		{
			name: "proxy reduction does not grant loopback", proxyPort: 43128,
			command: Command{LoopbackOnly: true},
			want:    securitymodel.NetworkDenied,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			capability := Capability{
				Available: true, ManagedProxy: true,
				Effective: platformControls("darwin"),
			}
			base := Policy{ID: "workspace-policy", ManagedProxyPort: test.proxyPort}
			narrowed := CommandNetworkPolicy(base, test.command)
			if narrowed.ManagedProxyPort != test.wantPort || narrowed.ID != base.ID {
				t.Fatalf("command policy = %+v", narrowed)
			}
			if base.ManagedProxyPort != test.proxyPort {
				t.Fatal("command reduction changed workspace policy")
			}
			if controls, err := CommandControls(capability, base, test.command); err != nil || controls.Network != test.want {
				t.Fatalf("command controls = %+v, want network %s", controls, test.want)
			}
		})
	}
}

func TestLoopbackCommandCannotInventNetworkIsolation(t *testing.T) {
	capability := Capability{
		Available: true, Effective: platformControls("unsupported"),
	}
	command := Command{AllowLoopback: true, LoopbackOnly: true}
	controls, err := CommandControls(capability, Policy{AllowNetwork: true}, command)
	if err != nil {
		t.Fatal(err)
	}
	if controls.Network != securitymodel.NetworkDirect {
		t.Fatalf("unsupported loopback isolation was advertised: %+v", controls)
	}
	if err := (securitymodel.RequiredControls{
		Network: securitymodel.NetworkLoopbackAny,
	}).SatisfiedBy(controls); err == nil {
		t.Fatal("loopback requirement accepted a backend without isolation")
	}
}
