package authority

import (
	"path/filepath"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
	"github.com/fwtllh-png/QCode/internal/security/policy"
)

func TestFullAccessScopesOnlyTheirOwnDimension(t *testing.T) {
	for _, write := range []bool{false, true} {
		for _, network := range []string{"direct", "loopback", "proxy", "proxy-loopback"} {
			t.Run(network+map[bool]string{false: "/host-write", true: "/scoped-write"}[write], func(t *testing.T) {
				input := fixtureCompileInput(t)
				input.Capability.ManagedProxy = true
				input.SandboxPolicy.ManagedProxyPort = 43128 // No socket is opened by compilation.
				if write {
					appendFixtureResources(&input.Prepared, tool.Resource{Kind: "file", Path: filepath.Join(input.SandboxPolicy.WorkspaceRoot, "output"), Access: tool.AccessWrite})
				}
				if network == "proxy" || network == "proxy-loopback" {
					appendFixtureResources(&input.Prepared, tool.Resource{Kind: "host", ID: "example.com", Protocol: "https", Port: 443, Access: tool.AccessRead, Methods: []string{"GET"}})
				}
				if network == "loopback" || network == "proxy-loopback" {
					appendFixtureResources(&input.Prepared, loopbackResource())
				}
				resolved := input.Prepared.Assessment.Input()
				resolved.Declared.FullAccess = true
				input.Prepared.Assessment = securitymodel.Assess(resolved)
				compiled, err := Compile(resolveCompileFixture(input))
				if err != nil {
					t.Fatal(err)
				}
				a := compiled.Profile.ExecutionAuthorityFor(compiled.Operation)
				wantNetwork := map[string]securitymodel.Network{"direct": securitymodel.NetworkDirect, "loopback": securitymodel.NetworkLoopbackAny, "proxy": securitymodel.NetworkProxyTargets, "proxy-loopback": securitymodel.NetworkProxyTargets}[network]
				wantWrite := securitymodel.FilesystemWriteUnrestricted
				if write {
					wantWrite = securitymodel.FilesystemWriteExactPaths
				}
				if !a.FullAccess || a.WorkspaceBaseWrite == write || a.EffectiveControls.FilesystemWrite != wantWrite || a.EffectiveControls.Network != wantNetwork || a.RequiredControls.Network != wantNetwork || a.EffectiveControls.FilesystemRead != securitymodel.FilesystemReadUnrestricted {
					t.Fatalf("dimension scope mismatch: %+v", a)
				}
			})
		}
	}
}

func TestFullAccessAuthorityBindsSessionAndControls(t *testing.T) {
	input := fixtureCompileInput(t)
	input.Runtime.Permission = policy.PermissionBypass
	resolved := input.Prepared.Assessment.Input()
	resolved.Declared.FullAccess = true
	input.Prepared.Assessment = securitymodel.Assess(resolved)
	input.Invocation.Assessment = input.Prepared.Assessment
	compiled, err := Compile(input)
	if err != nil {
		t.Fatal(err)
	}
	a := compiled.Profile.ExecutionAuthorityFor(compiled.Operation)
	if !a.FullAccess || !a.WorkspaceBaseWrite || !a.AllowNetwork || a.ManagedProxyPort != 0 ||
		a.EffectiveControls.Network != securitymodel.NetworkDirect || a.EffectiveControls.FilesystemWrite != securitymodel.FilesystemWriteUnrestricted {
		t.Fatalf("full access authority = %+v", a)
	}
	input.Runtime.Permission = policy.PermissionAuto
	if _, err := Compile(input); err == nil {
		t.Fatal("Auto accepted full access authority")
	}
}
