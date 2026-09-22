package authority

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/security/controlmatrix"
	"github.com/fwtllh-png/QCode/internal/security/policy"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func TestCompileProducesDeterministicEffectiveProfile(t *testing.T) {
	root := t.TempDir()
	runtime := policy.DefaultRuntime(policy.ModeAct, policy.PermissionSuggest)
	invocation := policy.Invocation{
		CallID: "call-1", Tool: "exec_command",
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
	}
	input := CompileInput{
		Runtime: runtime, Invocation: invocation,
		Decision:   policy.Decision{Action: policy.ActionAsk},
		Authorized: true, Revision: 1, Enforcement: "strong",
		Capability: sandbox.Capability{
			Backend: "seatbelt", Available: true,
			Effective: controlmatrix.Matrix{FilesystemRead: controlmatrix.
				FilesystemReadDeclaredRoots,

				FilesystemWrite: controlmatrix.FilesystemWriteExactPaths, Network: controlmatrix.
							NetworkDenied, ProcessTree: controlmatrix.ProcessTreeGroupKill,
				CrossProcess: controlmatrix.CrossProcessUnrestricted,
				Syscall:      controlmatrix.SyscallDenyDangerous, IPC: controlmatrix.
						IPCUnrestricted, PathIdentity: controlmatrix.PathIdentityDescriptorRelative,
				ArtifactOrigin: controlmatrix.ArtifactOriginUnverifiedPath, DurableRecovery: controlmatrix.
						DurableRecoveryMemoryOnly},
		},
		SandboxPolicy: sandbox.Policy{
			ID: "sandbox-policy", WorkspaceRoot: root,
			RuntimeReadRoots: []string{"/usr", "/bin"}, AllowNetwork: true,
		},
	}
	first, err := Compile(input)
	if err != nil {
		t.Fatal(err)
	}
	input.Invocation.Resources[0], input.Invocation.Resources[1] =
		input.Invocation.Resources[1], input.Invocation.Resources[0]
	second, err := Compile(input)
	if err != nil {
		t.Fatal(err)
	}
	if first.Digest != second.Digest || first.Digest == "" {
		t.Fatalf("digests first=%q second=%q", first.Digest, second.Digest)
	}
	if !first.Process.Allowed || first.Process.Enforcement != "strong" ||
		first.Network.Mode != "denied" ||
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
	input.Invocation.Resources = append(input.Invocation.Resources, tool.Resource{
		Kind: "directory", Path: generated, Access: tool.AccessWrite, Tree: true,
	})
	profile, err := Compile(input)
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
	if _, err := Compile(input); err == nil {
		t.Fatal("unauthorized invocation compiled")
	}
	input.Authorized = true
	input.Decision = policy.Decision{Action: policy.ActionDeny}
	if _, err := Compile(input); err == nil {
		t.Fatal("denied invocation compiled")
	}
}

func TestCompileCarriesExplicitLoopbackAuthority(t *testing.T) {
	input := fixtureCompileInput(t)
	input.SandboxPolicy.ManagedProxyPort = 43128
	input.Invocation.Resources = append(input.Invocation.Resources, tool.Resource{
		Kind: "host", ID: "localhost", Access: tool.AccessWrite,
		Protocol: "loopback", Methods: []string{"BIND", "CONNECT"},
		AllowPrivate: true,
	})
	profile, err := Compile(input)
	if err != nil {
		t.Fatal(err)
	}
	execution := profile.executionAuthority(RequiredControls{})
	if !profile.Network.Loopback || profile.Network.Mode != "loopback" ||
		profile.Network.ProxyPort != 0 || !execution.AllowLoopback ||
		execution.ManagedProxyPort != 0 || !execution.LoopbackOnly() ||
		!slices.Contains(profile.Network.Targets, "loopback://localhost:0") {
		t.Fatalf("loopback profile = %+v execution = %+v", profile.Network, execution)
	}
}

func TestProfileDigestDetectsMutation(t *testing.T) {
	profile, err := Compile(fixtureCompileInput(t))
	if err != nil {
		t.Fatal(err)
	}
	profile.Network.Mode = "unrestricted"
	if err := profile.Validate(); err == nil {
		t.Fatal("mutated profile was accepted")
	}
}

func TestLeaseRejectsInsufficientControls(t *testing.T) {
	input := fixtureCompileInput(t)
	input.Capability.Effective.Network = controlmatrix.NetworkDirect
	input.SandboxPolicy.AllowNetwork = true
	profile, err := Compile(input)
	if err != nil {
		t.Fatal(err)
	}
	if profile.Controls.Network != controlmatrix.NetworkDirect {
		t.Fatalf("profile did not derive partial controls: %+v", profile)
	}
	operation, err := BuildExecutionOperation(OperationInput{
		WorkspaceRoot:       input.SandboxPolicy.WorkspaceRoot,
		WorkspaceGeneration: 1,
		Invocation:          fixturePreparedInvocation(input.SandboxPolicy.WorkspaceRoot),
		Effect: policy.Effect{
			Kind: policy.EffectProcessReadOnly, Risk: policy.RiskLow,
			Reversibility: "reversible",
		},
		Required: RequiredControls{
			Network: controlmatrix.NetworkDenied,
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
	first, err := Compile(input)
	if err != nil {
		t.Fatal(err)
	}
	input.Invocation.Arguments = json.RawMessage(`{"command":"go env"}`)
	input.Invocation.Resources = []tool.Resource{{
		Kind: "file", Path: filepath.Join(t.TempDir(), "other.txt"),
		Access: tool.AccessWrite,
	}}
	second, err := Compile(input)
	if err != nil {
		t.Fatal(err)
	}
	if first.Digest == second.Digest {
		t.Fatal("replacement invocation retained the previous profile digest")
	}
}

func TestProfileBindsPolicySourceRevision(t *testing.T) {
	input := fixtureCompileInput(t)
	first, err := Compile(input)
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
	second, err := Compile(input)
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

func fixtureCompileInput(t *testing.T) CompileInput {
	t.Helper()
	root := t.TempDir()
	return CompileInput{
		Runtime: policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass),
		Invocation: policy.Invocation{
			CallID: "fixture", Tool: "exec_command",
			Arguments: json.RawMessage(`{"command":"go test ./..."}`),
			Resources: []tool.Resource{{
				Kind: "repo", Path: root, Access: tool.AccessRead, Tree: true,
			}},
			Capability: tool.CapabilityProcess, Access: tool.AccessRead,
			Sandbox: tool.SandboxStrong, Validated: true,
		},
		Decision:   policy.Decision{Action: policy.ActionAllow},
		Authorized: true, Revision: 1, Enforcement: "strong",
		Capability: sandbox.Capability{
			Backend: "seatbelt", Available: true,
			Effective: controlmatrix.Matrix{FilesystemRead: controlmatrix.
				FilesystemReadDeclaredRoots,

				FilesystemWrite: controlmatrix.FilesystemWriteExactPaths, Network: controlmatrix.
							NetworkDenied, ProcessTree: controlmatrix.ProcessTreeGroupKill,
				CrossProcess: controlmatrix.CrossProcessUnrestricted,
				Syscall:      controlmatrix.SyscallDenyDangerous, IPC: controlmatrix.
						IPCUnrestricted, PathIdentity: controlmatrix.PathIdentityDescriptorRelative,
				ArtifactOrigin: controlmatrix.ArtifactOriginUnverifiedPath, DurableRecovery: controlmatrix.
						DurableRecoveryMemoryOnly},
		},
		SandboxPolicy: sandbox.Policy{ID: "sandbox", WorkspaceRoot: root},
	}
}
