package sandbox

import (
	"testing"

	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
)

func TestLoopbackOnlyRejectsProxyOrOutboundTargets(t *testing.T) {
	loopback := ExecutionAuthority{
		AllowLoopback:  true,
		NetworkTargets: nil,
	}
	if !loopback.LoopbackOnly() {
		t.Fatal("loopback-only authority was rejected")
	}
	withProxy := loopback
	withProxy.ManagedProxyPort = 43128
	if withProxy.LoopbackOnly() {
		t.Fatal("loopback authority with a managed proxy port is not loopback-only")
	}
	withOutbound := loopback
	withOutbound.NetworkTargets = []string{
		"https://example.com:443",
	}
	if withOutbound.LoopbackOnly() {
		t.Fatal("loopback authority with outbound targets is not loopback-only")
	}
}

func TestCommandControlsUseSharedNetworkDerivation(t *testing.T) {
	capability := Capability{
		Backend: "seatbelt", Available: true, ManagedProxy: true,
		Effective: platformControls("darwin"),
	}
	proxied := Policy{ManagedProxyPort: 43128}
	cases := []struct {
		name    string
		policy  Policy
		command Command
		reach   NetworkReach
	}{
		{"denied", proxied, Command{DenyNetwork: true}, ReachNone},
		{"loopback only", proxied, Command{AllowLoopback: true, LoopbackOnly: true}, ReachLoopback},
		{"loopback without proxy", Policy{}, Command{AllowLoopback: true}, ReachLoopback},
		{"proxied targets", proxied, Command{}, ReachTargets},
		{"loopback beside proxy", proxied, Command{AllowLoopback: true}, ReachTargets},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			controls, err := CommandControls(capability, tc.policy, tc.command)
			if err != nil {
				t.Fatal(err)
			}
			got := controls.Network
			want := NetworkControl(capability, CommandNetworkPolicy(tc.policy, tc.command), tc.reach)
			if got != want {
				t.Fatalf("command network = %s, shared derivation = %s", got, want)
			}
		})
	}
}

func TestVerifyPreparedRejectsNetworkBroaderThanAuthority(t *testing.T) {
	compiled := platformControls("darwin")
	compiled.Network = securitymodel.NetworkLoopbackAny
	execution := ExecutionAuthority{
		Enforcement:       EnforcementStrong,
		EffectiveControls: compiled,
	}
	prepared := compiled
	prepared.Network = securitymodel.NetworkDenied
	if _, err := execution.VerifyPrepared(prepared); err != nil {
		t.Fatalf("stricter prepared network was rejected: %v", err)
	}
	prepared.Network = securitymodel.NetworkProxyTargets
	if _, err := execution.VerifyPrepared(prepared); err == nil {
		t.Fatal("prepared proxy network exceeded a loopback-only authority")
	}
	execution.RequiredControls = securitymodel.RequiredControls{
		FilesystemWrite: securitymodel.FilesystemWriteExactPaths,
	}
	prepared = compiled
	prepared.FilesystemWrite = securitymodel.FilesystemWriteWorkspace
	if _, err := execution.VerifyPrepared(prepared); err == nil {
		t.Fatal("prepared controls below the required write control were accepted")
	}
}

func TestAuthorizedCommandMustRespectCompiledNetwork(t *testing.T) {
	capability := Capability{Available: true, ManagedProxy: true, Effective: platformControls("darwin")}
	for _, tc := range []struct {
		name      string
		command   Command
		wantError bool
	}{
		{"missing compiled control", Command{AuthorityDigest: "lease", DenyNetwork: true}, true},
		{"proxy exceeds denied authority", Command{AuthorityDigest: "lease", CompiledNetwork: securitymodel.NetworkDenied}, true},
		{"proxy agrees with authority", Command{AuthorityDigest: "lease", CompiledNetwork: securitymodel.NetworkProxyTargets}, false},
		{"command can narrow to denied", Command{AuthorityDigest: "lease", CompiledNetwork: securitymodel.NetworkProxyTargets, DenyNetwork: true}, false},
		{"loopback exceeds denied authority", Command{AuthorityDigest: "lease", CompiledNetwork: securitymodel.NetworkDenied, AllowLoopback: true, LoopbackOnly: true}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := CommandControls(capability, Policy{ManagedProxyPort: 43128}, tc.command)
			if (err != nil) != tc.wantError {
				t.Fatalf("CommandControls() error = %v", err)
			}
		})
	}
}
