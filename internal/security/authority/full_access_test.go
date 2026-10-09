package authority

import (
	"testing"

	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
	"github.com/fwtllh-png/QCode/internal/security/policy"
)

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
