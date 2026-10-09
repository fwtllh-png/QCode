package processbroker

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/platform/process"
	"github.com/fwtllh-png/QCode/internal/security/authority"
	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
	"github.com/fwtllh-png/QCode/internal/security/policy"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func TestHostSessionBindsLaunchAndConsumesLeaseOnce(t *testing.T) {
	root := t.TempDir()
	backend, err := sandbox.NewPlatformBackend(sandbox.Options{WorkspaceRoot: root, PrivateTemp: t.TempDir(), SkipPATHReadRoots: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sandbox.CloseBackend(backend) })
	base, ok := sandbox.BackendPolicy(backend)
	if !ok {
		t.Fatal("no environment policy")
	}
	environment, err := process.EnvironmentFromPolicy(base)
	if err != nil {
		t.Fatal(err)
	}
	directory, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	options := process.SessionOptions{Command: "printf bound", Dir: root, DirFile: directory, Environment: environment, ExecutionTarget: "host", ThreadID: "owner", TurnID: "turn", CallID: "call", Env: []string{"LANG=C"}}
	digest, err := SessionCommandDigest(options)
	if err != nil {
		t.Fatal(err)
	}
	assessment := securitymodel.Assess(securitymodel.AssessmentInput{
		Binding:  securitymodel.AssessmentBinding{Capability: securitymodel.CapabilityProcess, Access: securitymodel.Read, SandboxDeclared: true, StrongSandbox: true},
		Declared: securitymodel.Declared{HostExecution: true},
	})
	prepared := securitymodel.PreparedInvocation{CallID: "call", Tool: "exec_command", Arguments: json.RawMessage(`{"command":"printf bound","execution_target":"host"}`), Assessment: assessment,
		Subject: securitymodel.Subject{Kind: securitymodel.SubjectBuiltin, Trust: securitymodel.TrustBuiltin, ID: "exec_command", Digest: strings.Repeat("b", 64), Generation: 1}}
	runtime := policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass)
	compiled, err := authority.Compile(authority.CompileInput{Runtime: runtime, Prepared: prepared,
		Invocation: policy.Invocation{CallID: prepared.CallID, Tool: prepared.Tool, Arguments: prepared.Arguments, Assessment: assessment, Source: securitymodel.SourceBuiltin, Validated: true},
		Decision:   policy.Decision{Action: policy.ActionAllow}, Authorized: true, Revision: 1, Enforcement: sandbox.EnforcementNone, SandboxPolicy: base})
	if err != nil {
		t.Fatal(err)
	}
	compiled, err = compiled.Bind(authority.Evidence{ProcessCommandDigest: digest})
	if err != nil {
		t.Fatal(err)
	}
	operation := compiled.Operation
	leases := authority.NewLeaseAuthority(authority.LeaseAuthorityOptions{})
	lease, err := leases.Issue(authority.LeaseIssueRequest{Operation: operation, Profile: compiled.Profile, PolicyRevision: runtime.Revision, Attempt: 1, ExpiresAt: time.Now().Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	broker, err := New(leases)
	if err != nil {
		t.Fatal(err)
	}
	manager := process.NewSessionManager(4096)
	t.Cleanup(manager.CloseAll)
	request := SessionRequest{Lease: lease, Options: options, Validation: authority.LeaseValidation{Operation: operation,
		PolicyRevision: runtime.Revision, Attempt: 1, WorkspaceID: operation.WorkspaceID, WorkspaceGeneration: operation.WorkspaceGeneration,
		SubjectDigest: operation.Subject.Digest, SubjectGeneration: operation.Subject.Generation}}
	for _, mutate := range []func(*process.SessionOptions){
		func(o *process.SessionOptions) { o.DirFile = nil },
		func(o *process.SessionOptions) { o.DisplayCommand = "fake receipt" },
		func(o *process.SessionOptions) { o.DetachFromCaller = true },
		func(o *process.SessionOptions) { o.Command = "printf replaced" },
		func(o *process.SessionOptions) { o.Env = []string{"LANG=replaced"} },
		func(o *process.SessionOptions) { o.ThreadID = "intruder" },
		func(o *process.SessionOptions) { o.Timeout = time.Second },
		func(o *process.SessionOptions) { o.RequireSandbox = true },
	} {
		changed := request
		mutate(&changed.Options)
		if _, err := broker.StartSession(t.Context(), manager, changed); err == nil {
			t.Fatal("changed launch accepted")
		}
		snapshot, err := leases.Snapshot(lease)
		if err != nil || snapshot.State != authority.LeaseIssued {
			t.Fatalf("mismatch consumed lease: %+v %v", snapshot, err)
		}
	}
	id, err := broker.StartSession(t.Context(), manager, request)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(id, "owner"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := leases.Snapshot(lease)
	if err != nil || snapshot.State != authority.LeaseSettled {
		t.Fatalf("launch not settled: %+v %v", snapshot, err)
	}
	if _, err := broker.StartSession(t.Context(), manager, request); err == nil {
		t.Fatal("launch lease reused")
	}
	if err := leases.Release(lease); err != nil {
		t.Fatal(err)
	}
}
