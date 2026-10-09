package authority

import (
	"strings"
	"testing"

	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
	"github.com/fwtllh-png/QCode/internal/security/policy"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func TestHostAuthorityRequiresApprovalAndBindsConcreteCommand(t *testing.T) {
	input := fixtureCompileInput(t)
	input.Runtime.Permission = policy.PermissionAuto
	input.Enforcement = sandbox.EnforcementNone
	resolved := input.Prepared.Assessment.Input()
	resolved.Declared.HostExecution = true
	input.Prepared.Assessment = securitymodel.Assess(resolved)
	input.Invocation.Assessment = input.Prepared.Assessment
	if _, err := Compile(input); err == nil {
		t.Fatal("Auto host authority without approval")
	}
	input.Decision = policy.Decision{Action: policy.ActionAsk, Approval: policy.ApprovalFreshOnce}
	compiled, err := Compile(input)
	if err != nil {
		t.Fatal(err)
	}
	a := compiled.Profile.ExecutionAuthorityFor(compiled.Operation)
	if !a.HostExecution || a.FullAccess || a.Enforcement != sandbox.EnforcementNone || a.EffectiveControls.CrossProcess != securitymodel.CrossProcessUnrestricted || a.EffectiveControls.ProcessTree != securitymodel.ProcessTreeGroupKill || len(compiled.Profile.Filesystem.DeniedWriteRoots) != 0 {
		t.Fatalf("false host controls: %+v", a)
	}
	digest := strings.Repeat("c", 64)
	bound, err := compiled.Bind(Evidence{ProcessCommandDigest: digest})
	if err != nil {
		t.Fatal(err)
	}
	if compiled.Operation.Process.PreparedCommandDigest != "" || bound.Operation.Process.PreparedCommandDigest != digest || compiled.Operation.Digest == bound.Operation.Digest {
		t.Fatal("concrete command binding mutated source or failed to change operation identity")
	}
	input.Authorized = false
	if _, err := Compile(input); err == nil {
		t.Fatal("pending approval compiled host authority")
	}
	input.Authorized = true
	input.Runtime.Permission = policy.PermissionBypass
	input.Decision = policy.Decision{Action: policy.ActionAllow}
	if _, err := Compile(input); err != nil {
		t.Fatalf("Full Access did not authorize host: %v", err)
	}
	input.Runtime.DisableHostExecution = true
	if _, err := Compile(input); err == nil {
		t.Fatal("delegated runtime compiled host authority")
	}
	input.Runtime.DisableHostExecution = false
	input.Enforcement = sandbox.EnforcementStrong
	if _, err := Compile(input); err == nil {
		t.Fatal("host authority claiming strong sandbox")
	}
}
