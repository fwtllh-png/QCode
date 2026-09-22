package sandbox

import (
	"testing"

	"github.com/fwtllh-png/QCode/internal/security/controlmatrix"
)

func TestManagedNetworkControlsRequireProbedSupport(t *testing.T) {
	for _, test := range []struct {
		name      string
		platform  string
		available bool
		proxy     bool
		want      controlmatrix.Network
	}{
		{"probed seatbelt", "darwin", true, true, controlmatrix.NetworkProxyTargets},
		{"unprobed seatbelt", "darwin", true, false, controlmatrix.NetworkDenied},
		{"unavailable", "darwin", false, true, controlmatrix.NetworkDenied},
		{"unsupported backend", "unsupported", true, false, controlmatrix.NetworkDirect},
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
			denied := CommandControls(capability, policy, Command{DenyNetwork: true})
			if capability.Effective.Network == controlmatrix.NetworkDenied &&
				denied.Network != controlmatrix.NetworkDenied {
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
		want      controlmatrix.Network
		wantPort  uint16
	}{
		{
			name: "managed", proxyPort: 43128,
			want: controlmatrix.NetworkProxyTargets, wantPort: 43128,
		},
		{
			name: "managed and loopback", proxyPort: 43128,
			command: Command{AllowLoopback: true},
			want:    controlmatrix.NetworkProxyTargets, wantPort: 43128,
		},
		{
			name: "loopback only on managed backend", proxyPort: 43128,
			command: Command{AllowLoopback: true, LoopbackOnly: true},
			want:    controlmatrix.NetworkLoopbackExact,
		},
		{
			name:    "loopback without proxy",
			command: Command{AllowLoopback: true, LoopbackOnly: true},
			want:    controlmatrix.NetworkLoopbackExact,
		},
		{
			name: "denial overrides loopback and proxy", proxyPort: 43128,
			command: Command{DenyNetwork: true, AllowLoopback: true, LoopbackOnly: true},
			want:    controlmatrix.NetworkDenied,
		},
		{
			name: "proxy reduction does not grant loopback", proxyPort: 43128,
			command: Command{LoopbackOnly: true},
			want:    controlmatrix.NetworkDenied,
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
			if controls := CommandControls(capability, base, test.command); controls.Network != test.want {
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
	controls := CommandControls(capability, Policy{AllowNetwork: true}, command)
	if controls.Network != controlmatrix.NetworkDirect {
		t.Fatalf("unsupported loopback isolation was advertised: %+v", controls)
	}
	if err := (controlmatrix.Requirements{
		Network: controlmatrix.NetworkLoopbackExact,
	}).SatisfiedBy(controls); err == nil {
		t.Fatal("loopback requirement accepted a backend without isolation")
	}
}
