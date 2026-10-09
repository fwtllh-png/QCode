package sandbox

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
)

func TestRelocateWorkspacePreservesAuthorityCeiling(t *testing.T) {
	source, target, external := t.TempDir(), t.TempDir(), t.TempDir()
	original := ExecutionAuthority{
		Digest: strings.Repeat("a", 64), Enforcement: EnforcementStrong,
		WorkspaceRoot: source, AllowProcess: true,
		ReadPaths: []string{source, external}, WorkspaceWritePaths: []string{filepath.Join(source, "deps")},
		NetworkTargets: []string{"https://example.com:443"}, ManagedProxyPort: 12345,
		RequiredControls: securitymodel.RequiredControls{FilesystemWrite: securitymodel.FilesystemWriteExactPaths},
	}
	projected, err := original.RelocateWorkspace(source, target)
	if err != nil {
		t.Fatal(err)
	}
	if projected.Digest == original.Digest || projected.WorkspaceRoot != target ||
		!reflect.DeepEqual(projected.ReadPaths, []string{target, external}) ||
		!reflect.DeepEqual(projected.WorkspaceWritePaths, []string{filepath.Join(target, "deps")}) {
		t.Fatalf("projection=%+v", projected)
	}
	if projected.Enforcement != original.Enforcement || projected.RequiredControls != original.RequiredControls ||
		projected.AllowNetwork != original.AllowNetwork || projected.ManagedProxyPort != original.ManagedProxyPort ||
		projected.WorkspaceBaseWrite != original.WorkspaceBaseWrite || !reflect.DeepEqual(projected.NetworkTargets, original.NetworkTargets) {
		t.Fatal("projection changed authority ceiling")
	}
	if _, denied := projected.DeniedWritePath([]string{filepath.Join(target, "outside.txt")}); !denied {
		t.Fatal("projection authorized an undeclared path")
	}
	if _, err := original.RelocateWorkspace(external, target); err == nil {
		t.Fatal("wrong source identity accepted")
	}
	invalid := original
	invalid.WorkspaceWritePaths = []string{external}
	if _, err := invalid.RelocateWorkspace(source, target); err == nil {
		t.Fatal("external write grant accepted")
	}
	projected.WorkspaceWritePaths[0] = "changed"
	if original.WorkspaceWritePaths[0] != filepath.Join(source, "deps") {
		t.Fatal("projection mutated the original authority")
	}
}

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

func TestFullAccessPreparedGrantsMustMatchEveryDimension(t *testing.T) {
	compiled := platformControls("darwin")
	compiled.FilesystemRead = securitymodel.FilesystemReadUnrestricted
	compiled.FilesystemWrite = securitymodel.FilesystemWriteUnrestricted
	compiled.Network = securitymodel.NetworkDirect
	compiled.Syscall = securitymodel.SyscallUnrestricted
	authority := ExecutionAuthority{FullAccess: true, Enforcement: EnforcementStrong, EffectiveControls: compiled}
	if _, err := authority.VerifyPrepared(compiled); err != nil {
		t.Fatal(err)
	}
	for _, change := range []struct {
		name  string
		apply func(*securitymodel.Controls)
	}{
		{"read narrowed", func(c *securitymodel.Controls) { c.FilesystemRead = securitymodel.FilesystemReadDeclaredRoots }},
		{"write narrowed", func(c *securitymodel.Controls) { c.FilesystemWrite = securitymodel.FilesystemWriteExactPaths }},
		{"network narrowed", func(c *securitymodel.Controls) { c.Network = securitymodel.NetworkDenied }},
		{"system calls narrowed", func(c *securitymodel.Controls) { c.Syscall = securitymodel.SyscallPlatformFiltered }},
		{"credential IPC opened", func(c *securitymodel.Controls) { c.IPC = securitymodel.IPCUnrestricted }},
	} {
		t.Run(change.name, func(t *testing.T) {
			prepared := compiled
			change.apply(&prepared)
			if _, err := authority.VerifyPrepared(prepared); err == nil {
				t.Fatal("mismatched Full Access grants accepted")
			}
		})
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
