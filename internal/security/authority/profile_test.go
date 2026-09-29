package authority

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
	"github.com/fwtllh-png/QCode/internal/security/policy"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func TestCompileProducesDeterministicEffectiveProfile(t *testing.T) {
	root := t.TempDir()
	runtime := policy.DefaultRuntime(policy.ModeAct, policy.PermissionSuggest)
	invocation := resolvePolicyFixture(policyInvocationFixture{
		CallID: "call-1", Tool: "run_command",
		Arguments:  json.RawMessage(`{"command":"go test ./..."}`),
		Capability: tool.CapabilityProcess,
		Access:     tool.AccessWrite,
		Sandbox:    tool.SandboxStrong,
		Validated:  true,
		Resources: []tool.Resource{
			{Kind: "file", Path: filepath.Join(root, "report.txt"), Access: tool.AccessWrite},
			{Kind: "repo", Path: root, Access: tool.AccessRead, Tree: true},
			{Kind: "process", ID: "workspace", Access: tool.AccessWrite, Tree: true},
		},
	})
	input := CompileInput{
		Runtime: runtime, Invocation: invocation,
		Decision:   policy.Decision{Action: policy.ActionAsk},
		Authorized: true, Revision: 1, Enforcement: "strong",
		Capability: sandbox.Capability{
			Backend: "seatbelt", Available: true,
			Effective: securitymodel.Controls{FilesystemRead: securitymodel.FilesystemReadDeclaredRoots,

				FilesystemWrite: securitymodel.FilesystemWriteExactPaths, Network: securitymodel.NetworkDenied, ProcessTree: securitymodel.ProcessTreeGroupKill,
				CrossProcess: securitymodel.CrossProcessUnrestricted,
				Syscall:      securitymodel.SyscallDenyDangerous, IPC: securitymodel.IPCUnrestricted, PathIdentity: securitymodel.PathIdentityDescriptorRelative,
				ArtifactOrigin: securitymodel.ArtifactOriginUnverifiedPath, DurableRecovery: securitymodel.DurableRecoveryMemoryOnly},
		},
		SandboxPolicy: sandbox.Policy{
			ID: "sandbox-policy", WorkspaceRoot: root,
			RuntimeReadRoots: []string{"/usr", "/bin"}, AllowNetwork: true,
		},
	}
	input.Prepared = preparedForPolicy(input.Invocation)
	first, err := compileProfileForTest(input)
	if err != nil {
		t.Fatal(err)
	}
	resolved := input.Prepared.Assessment.Input()
	resolved.Resources[0], resolved.Resources[1] = resolved.Resources[1], resolved.Resources[0]
	input.Prepared.Assessment = securitymodel.Assess(resolved)
	second, err := compileProfileForTest(input)
	if err != nil {
		t.Fatal(err)
	}
	if first.Digest != second.Digest || first.Digest == "" {
		t.Fatalf("digests first=%q second=%q", first.Digest, second.Digest)
	}
	if !first.Process.Allowed || first.Process.Enforcement != "strong" ||
		first.Controls.Network != securitymodel.NetworkDenied ||
		!slices.Contains(first.Filesystem.WritePaths, filepath.Join(root, "report.txt")) {
		t.Fatalf("profile = %+v", first)
	}
	for _, rootName := range []string{
		".qcode", ".qcode-worktree", ".git", ".agents",
	} {
		if !slices.Contains(
			first.Filesystem.DeniedWriteRoots,
			filepath.Join(root, rootName),
		) {
			t.Fatalf("denied roots = %v", first.Filesystem.DeniedWriteRoots)
		}
	}
}

func TestCompileDirectoryWriteDoesNotOpenWorkspace(t *testing.T) {
	input := fixtureCompileInput(t)
	generated := filepath.Join(input.SandboxPolicy.WorkspaceRoot, "generated")
	if err := os.Mkdir(generated, 0o700); err != nil {
		t.Fatal(err)
	}
	appendFixtureResources(&input.Prepared, tool.Resource{
		Kind: "directory", Path: generated, Access: tool.AccessWrite, Tree: true,
	})
	profile, err := compileProfileForTest(input)
	if err != nil {
		t.Fatal(err)
	}
	if profile.Filesystem.WorkspaceBaseWrite {
		t.Fatal("bounded write tree opened the workspace")
	}
	if !slices.Contains(profile.Filesystem.WritePaths, generated) {
		t.Fatalf("write paths = %v", profile.Filesystem.WritePaths)
	}
}

func TestCompileRejectsUnauthorizedAndDeniedInvocation(t *testing.T) {
	input := fixtureCompileInput(t)
	input.Authorized = false
	if _, err := compileProfileForTest(input); err == nil {
		t.Fatal("unauthorized invocation compiled")
	}
	input.Authorized = true
	input.Decision = policy.Decision{Action: policy.ActionDeny}
	if _, err := compileProfileForTest(input); err == nil {
		t.Fatal("denied invocation compiled")
	}
}

func TestCompileCarriesExplicitLoopbackAuthority(t *testing.T) {
	input := fixtureCompileInput(t)
	input.SandboxPolicy.ManagedProxyPort = 43128
	appendFixtureResources(&input.Prepared, tool.Resource{
		Kind: "host", ID: "localhost", Access: tool.AccessWrite,
		Protocol: securitymodel.LoopbackProtocol, Methods: []string{"BIND", "CONNECT"},
		AllowPrivate: true,
	})
	profile, err := compileProfileForTest(input)
	if err != nil {
		t.Fatal(err)
	}
	execution := profile.executionAuthority(RequiredControls{})
	if !profile.Network.Loopback || profile.Controls.Network != securitymodel.NetworkLoopbackAny ||
		profile.Network.ProxyPort != 0 || !execution.AllowLoopback ||
		execution.ManagedProxyPort != 0 || !execution.LoopbackOnly() ||
		len(profile.Network.Targets) != 0 {
		t.Fatalf("loopback profile = %+v execution = %+v", profile.Network, execution)
	}
}

func TestProfileDigestDetectsMutation(t *testing.T) {
	profile, err := compileProfileForTest(fixtureCompileInput(t))
	if err != nil {
		t.Fatal(err)
	}
	profile.Controls.Network = securitymodel.NetworkDirect
	if err := profile.Validate(); err == nil {
		t.Fatal("mutated profile was accepted")
	}
}

func TestLeaseRejectsInsufficientControls(t *testing.T) {
	input := fixtureCompileInput(t)
	input.Capability.Effective.Network = securitymodel.NetworkDirect
	input.SandboxPolicy.AllowNetwork = true
	profile, err := compileProfileForTest(input)
	if err != nil {
		t.Fatal(err)
	}
	if profile.Controls.Network != securitymodel.NetworkDirect {
		t.Fatalf("profile did not derive partial controls: %+v", profile)
	}
	operation, err := buildFixtureOperation(operationInput{
		WorkspaceRoot:       input.SandboxPolicy.WorkspaceRoot,
		WorkspaceGeneration: 1,
		Invocation:          resolvePreparedFixture(fixturePreparedInvocation(input.SandboxPolicy.WorkspaceRoot)),
		Effect: securitymodel.Effect{
			Kind: securitymodel.ProcessReadOnly, Risk: securitymodel.RiskLow,
			Reversibility: securitymodel.Reversible,
		},
		Required: RequiredControls{
			Network: securitymodel.NetworkDenied,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	manager := NewLeaseAuthority(LeaseAuthorityOptions{})
	if _, err := manager.Issue(LeaseIssueRequest{
		Operation: operation, Profile: profile,
		PolicyRevision:  input.Runtime.Revision,
		SandboxPolicyID: input.SandboxPolicy.ID,
		Attempt:         1, ExpiresAt: time.Now().Add(time.Minute),
	}); err == nil {
		t.Fatal("profile with insufficient network control received a lease")
	}
}

func TestReplacementArgumentsProduceDifferentProfileDigest(t *testing.T) {
	input := fixtureCompileInput(t)
	input.Prepared = preparedForPolicy(input.Invocation)
	first, err := compileProfileForTest(input)
	if err != nil {
		t.Fatal(err)
	}
	input.Invocation.Arguments = json.RawMessage(`{"command":"go env"}`)
	resolved := input.Prepared.Assessment.Input()
	resolved.Resources = nil
	input.Prepared.Assessment = securitymodel.Assess(resolved)
	appendFixtureResources(&input.Prepared, tool.Resource{
		Kind: "file", Path: filepath.Join(t.TempDir(), "other.txt"),
		Access: tool.AccessWrite,
	})
	second, err := compileProfileForTest(input)
	if err != nil {
		t.Fatal(err)
	}
	if first.Digest == second.Digest {
		t.Fatal("replacement invocation retained the previous profile digest")
	}
}

func TestProfileBindsPolicySourceRevision(t *testing.T) {
	input := fixtureCompileInput(t)
	input.Prepared = preparedForPolicy(input.Invocation)
	first, err := compileProfileForTest(input)
	if err != nil {
		t.Fatal(err)
	}
	revision, err := input.Runtime.ReloadSources(
		[]policy.Rule{{Tool: input.Invocation.Tool, Action: policy.ActionAllow}},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	input.Runtime = input.Runtime.CloneSampling()
	second, err := compileProfileForTest(input)
	if err != nil {
		t.Fatal(err)
	}
	if first.Digest == second.Digest || revision != input.Runtime.Revision {
		t.Fatalf(
			"policy revision did not change profile: first=%s second=%s revision=%d",
			first.Digest, second.Digest, revision,
		)
	}
	found := false
	for _, source := range second.Provenance {
		if source.Kind == "policy" && source.Revision == revision {
			found = true
		}
	}
	if !found {
		t.Fatalf("policy revision missing from provenance: %+v", second.Provenance)
	}
}

func TestProfileProvenanceBindsConstitutionAndDecisionLayer(t *testing.T) {
	input := fixtureCompileInput(t)
	input.Decision.Layer = policy.LayerPosture
	input.Prepared = preparedForPolicy(input.Invocation)
	first, err := compileProfileForTest(input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := input.Runtime.SetConstitution([]policy.Rule{{
		Tool: "*", Resource: "secrets/", Action: policy.ActionDeny,
	}}); err != nil {
		t.Fatal(err)
	}
	input.Runtime = input.Runtime.CloneSampling()
	second, err := compileProfileForTest(input)
	if err != nil {
		t.Fatal(err)
	}
	sources := map[string]AuthoritySource{}
	for _, source := range second.Provenance {
		sources[source.Kind] = source
	}
	if first.Digest == second.Digest || sources["constitution"].Digest == "" ||
		sources["decision_layer"].Value != string(policy.LayerPosture) {
		t.Fatalf("provenance = %+v", second.Provenance)
	}
}

func fixtureCompileInput(t *testing.T) CompileInput {
	t.Helper()
	root := t.TempDir()
	input := CompileInput{
		Runtime: policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass),
		Invocation: resolvePolicyFixture(policyInvocationFixture{
			CallID: "fixture", Tool: "run_command",
			Arguments: json.RawMessage(`{"command":"go test ./..."}`),
			Resources: []tool.Resource{{
				Kind: "repo", Path: root, Access: tool.AccessRead, Tree: true,
			}},
			Capability: tool.CapabilityProcess, Access: tool.AccessRead,
			Sandbox: tool.SandboxStrong, Validated: true,
		}),
		Decision:   policy.Decision{Action: policy.ActionAllow},
		Authorized: true, Revision: 1, Enforcement: "strong",
		Capability: sandbox.Capability{
			Backend: "seatbelt", Available: true,
			Effective: securitymodel.Controls{FilesystemRead: securitymodel.FilesystemReadDeclaredRoots,

				FilesystemWrite: securitymodel.FilesystemWriteExactPaths, Network: securitymodel.NetworkDenied, ProcessTree: securitymodel.ProcessTreeGroupKill,
				CrossProcess: securitymodel.CrossProcessUnrestricted,
				Syscall:      securitymodel.SyscallDenyDangerous, IPC: securitymodel.IPCUnrestricted, PathIdentity: securitymodel.PathIdentityDescriptorRelative,
				ArtifactOrigin: securitymodel.ArtifactOriginUnverifiedPath, DurableRecovery: securitymodel.DurableRecoveryMemoryOnly},
		},
		SandboxPolicy: sandbox.Policy{ID: "sandbox", WorkspaceRoot: root},
	}
	input.Prepared = preparedForPolicy(input.Invocation)
	return input
}

func compileProfileForTest(input CompileInput) (EffectivePermissionProfile, error) {
	compiled, err := Compile(resolveCompileFixture(input))
	return compiled.Profile, err
}

func preparedForPolicy(invocation policy.Invocation) securitymodel.PreparedInvocation {
	prepared := tool.PreparedInvocation{
		CallID: invocation.CallID, Tool: invocation.Tool,
		Ref: tool.ToolRef{
			Name: invocation.Tool, Source: "builtin:" + invocation.Tool,
			CatalogID: "catalog-1", Generation: 1, Revision: 1, Authority: 1,
		},
		Arguments:  invocation.Arguments,
		Assessment: invocation.Assessment,
		Descriptor: tool.Descriptor{
			Name: invocation.Tool, Capability: invocation.Capability(),
			AccessMode: invocation.Access(), SandboxRequirement: fixtureSandbox(invocation),
		},
		Source: tool.InvocationSourceModel,
	}
	prepared.Binding = tool.TrustedBindingFromDescriptor(prepared.Descriptor)
	return resolvePreparedFixture(prepared)
}

func fixtureSandbox(invocation policy.Invocation) tool.SandboxRequirement {
	if invocation.StrongSandbox() {
		return tool.SandboxStrong
	}
	return tool.SandboxNone
}

// resolveCompileFixture models Guard preparation after a test changes its
// declared resources. Compile itself must reject unassessed inputs.
func resolveCompileFixture(input CompileInput) CompileInput {
	if input.Prepared.Tool == "" {
		input.Prepared = preparedForPolicy(input.Invocation)
	}
	input.Prepared.CallID, input.Prepared.Tool, input.Prepared.Arguments = input.Invocation.CallID, input.Invocation.Tool, input.Invocation.Arguments
	input.Invocation.Assessment = input.Prepared.Assessment
	return input
}
