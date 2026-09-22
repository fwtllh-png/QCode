package processbroker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/platform/process"
	"github.com/fwtllh-png/QCode/internal/security/artifactbroker"
	"github.com/fwtllh-png/QCode/internal/security/authority"
	"github.com/fwtllh-png/QCode/internal/security/policy"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func TestBrokerConsumesLeaseRunsSnapshotAndReaps(t *testing.T) {
	fixture := newFixture(t)
	result, err := fixture.broker.RunSmoke(
		t.Context(),
		fixture.request(50*time.Millisecond),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Survived || !result.Handle.Terminal ||
		result.Settlement.Status != "succeeded" ||
		result.Settlement.CompletedAt.IsZero() {
		t.Fatalf("result = %+v", result)
	}
	state, err := fixture.authority.Snapshot(fixture.lease)
	if err != nil {
		t.Fatal(err)
	}
	if state.State != authority.LeaseSettled {
		t.Fatalf("lease state = %q", state.State)
	}
	if _, err := fixture.broker.RunSmoke(
		t.Context(),
		fixture.request(time.Millisecond),
	); err == nil || !strings.Contains(err.Error(), "not issuable") {
		t.Fatalf("reused lease error = %v", err)
	}
}

func TestBrokerRejectsMutatedSnapshotBeforeLeaseConsumption(t *testing.T) {
	fixture := newFixture(t)
	if err := os.Chmod(fixture.snapshot.ExecutablePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		fixture.snapshot.ExecutablePath,
		[]byte("#!/bin/sh\nexit 0\n"),
		0o500,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.broker.RunSmoke(
		t.Context(),
		fixture.request(time.Millisecond),
	); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("mutated snapshot error = %v", err)
	}
	state, err := fixture.authority.Snapshot(fixture.lease)
	if err != nil {
		t.Fatal(err)
	}
	if state.State != authority.LeaseIssued {
		t.Fatalf("rejected snapshot consumed lease: %q", state.State)
	}
}

func TestRunCommandSettlesNonZeroExitAsFailure(t *testing.T) {
	manager := authority.NewLeaseAuthority(authority.LeaseAuthorityOptions{})
	broker, err := New(manager)
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	sum := sha256.Sum256([]byte(filepath.Clean(workspace)))
	workspaceID := hex.EncodeToString(sum[:])
	subject, err := authority.NewManagedProcessSubject(
		authority.SubjectHost, "command-test", authority.TrustHost, 1,
		map[string]string{"command": "exit 7"},
	)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := authority.BuildManagedProcessOperation(
		authority.ManagedProcessInput{
			ID: "command-exit", Tool: "command-test",
			WorkspaceID: workspaceID, WorkspaceGeneration: 1,
			Subject: subject, Executable: "/bin/sh",
			Args: []string{"-c", "exit 7"}, WorkingDirectory: workspace,
			Effect: authority.ManagedProcessEffect(policy.RiskLow),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := authority.BuildManagedProcessProfile(
		authority.ManagedProfileInput{
			Operation: operation, Revision: 1, WorkspaceRoot: workspace,
			WorkspaceBaseWrite: true, AllowNetwork: true,
			Enforcement: "none", Backend: "none",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := manager.Issue(authority.LeaseIssueRequest{
		Operation: operation, Profile: profile,
		PolicyRevision: 1, Attempt: 1, ExpiresAt: time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := broker.RunCommand(t.Context(), CommandRequest{
		Lease: lease,
		Validation: authority.LeaseValidation{
			Operation: operation, PolicyRevision: 1,
			WorkspaceID: workspaceID, WorkspaceGeneration: 1,
			SubjectDigest: subject.Digest, SubjectGeneration: 1, Attempt: 1,
		},
		Options: process.Options{
			Path: "/bin/sh", Args: []string{"-c", "exit 7"}, Dir: workspace,
		},
		Identity: Identity{
			SessionID: "session", ThreadID: "thread", TurnID: "turn",
		},
	})
	if err == nil || !strings.Contains(err.Error(), "exit status 7") {
		t.Fatalf("RunCommand error = %v", err)
	}
	if result.Process.ExitCode != 7 ||
		result.Settlement.Status != "failed" ||
		result.Settlement.Reason != "command_failed" {
		t.Fatalf("result = %+v", result)
	}
}

type brokerFixture struct {
	broker     *Broker
	authority  *authority.LeaseAuthority
	operation  authority.ExecutionOperation
	lease      authority.ExecutionLease
	validation authority.LeaseValidation
	snapshot   artifactbroker.Snapshot
	workspace  string
}

func newFixture(t *testing.T) brokerFixture {
	t.Helper()
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	home := filepath.Join(base, "home")
	stage := filepath.Join(base, "artifacts")
	for _, path := range []string{workspace, home, stage} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	source := filepath.Join(workspace, "smoke")
	if err := os.WriteFile(
		source,
		[]byte("#!/bin/sh\nwhile :; do sleep 1; done\n"),
		0o700,
	); err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry(nil, nil)
	executor := fixtureExecutor{}
	if err := registry.Register(executor); err != nil {
		t.Fatal(err)
	}
	ref, descriptor, _, err := registry.ResolveBoundRef(
		executor.Descriptor().Name,
		tool.CatalogBinding{},
	)
	if err != nil {
		t.Fatal(err)
	}
	invocation := tool.PreparedInvocation{
		Identity: tool.InvocationIdentity{
			CallID: "call-smoke", SessionID: "session-smoke",
			ThreadID: "thread-smoke", TurnID: "turn-smoke",
		},
		CallID: "call-smoke", Tool: descriptor.Name, Ref: ref,
		Arguments: json.RawMessage(`{"path":"smoke"}`),
		Resources: []tool.Resource{
			{Kind: "file", Path: source, Access: tool.AccessRead},
			{Kind: "process", ID: "host", Access: tool.AccessWrite},
		},
		Descriptor: descriptor, Source: tool.InvocationSourceModel,
		Disposition: tool.DispositionWaitForTeardown,
	}
	invocation.Binding = tool.TrustedBindingFromDescriptor(descriptor)
	runtimePolicy := policy.DefaultRuntime(
		policy.ModeAct,
		policy.PermissionBypass,
	)
	profile, err := authority.Compile(authority.CompileInput{
		Runtime: runtimePolicy,
		Invocation: policy.Invocation{
			CallID: invocation.CallID, Tool: invocation.Tool,
			Arguments: invocation.Arguments, Resources: invocation.Resources,
			Capability: descriptor.Capability, Access: descriptor.AccessMode,
			Sandbox: descriptor.SandboxRequirement, Validated: true,
		},
		Decision:   policy.Decision{Action: policy.ActionAllow},
		Authorized: true, Revision: 1, Enforcement: "none",
		Capability:    sandbox.Capability{Backend: "host", Available: true},
		SandboxPolicy: sandbox.Policy{WorkspaceRoot: workspace},
	})
	if err != nil {
		t.Fatal(err)
	}
	preliminary, err := authority.BuildExecutionOperation(authority.OperationInput{
		WorkspaceRoot: workspace, WorkspaceGeneration: 1,
		Invocation: invocation,
		Effect: policy.Effect{
			Kind: policy.EffectProcessReadOnly, Risk: policy.RiskHigh,
			Reversibility: "bounded",
		},
		HostReadRoots: []string{workspace},
	})
	if err != nil {
		t.Fatal(err)
	}
	artifactBroker, err := artifactbroker.New(artifactbroker.Options{
		WorkspaceRoot: workspace, SandboxHomeRoot: home, StagingRoot: stage,
		WorkspaceID: preliminary.WorkspaceID, WorkspaceGeneration: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := artifactBroker.Prepare(artifactbroker.PrepareRequest{
		SourcePath: source, ProducerOperationDigest: preliminary.Digest,
	})
	if err != nil {
		t.Fatal(err)
	}
	operation, err := authority.BuildExecutionOperation(authority.OperationInput{
		WorkspaceRoot: workspace, WorkspaceID: preliminary.WorkspaceID,
		WorkspaceGeneration: 1, Invocation: invocation,
		Effect: policy.Effect{
			Kind: policy.EffectProcessReadOnly, Risk: policy.RiskHigh,
			Reversibility: "bounded",
		},
		Artifact: &authority.ArtifactIntent{
			ManifestDigest: snapshot.Manifest.Digest,
			Generation:     snapshot.Manifest.Generation,
		},
		HostReadRoots: []string{workspace},
	})
	if err != nil {
		t.Fatal(err)
	}
	manager := authority.NewLeaseAuthority(authority.LeaseAuthorityOptions{})
	lease, err := manager.Issue(authority.LeaseIssueRequest{
		Operation: operation, Profile: profile,
		PolicyRevision: runtimePolicy.Revision, Attempt: 1,
		ExpiresAt: time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	processBroker, err := New(manager)
	if err != nil {
		t.Fatal(err)
	}
	validation := authority.LeaseValidation{
		Operation: operation, PolicyRevision: runtimePolicy.Revision,
		WorkspaceID:         operation.WorkspaceID,
		WorkspaceGeneration: operation.WorkspaceGeneration,
		SubjectDigest:       operation.Subject.Digest,
		SubjectGeneration:   operation.Subject.Generation,
		ArtifactDigest:      snapshot.Manifest.Digest, Attempt: 1,
	}
	return brokerFixture{
		broker: processBroker, authority: manager, operation: operation,
		lease: lease, validation: validation, snapshot: snapshot,
		workspace: workspace,
	}
}

func (f brokerFixture) request(minimum time.Duration) Request {
	return Request{
		Lease: f.lease, Validation: f.validation, Artifact: f.snapshot,
		Dir: f.workspace, MinimumRuntime: minimum,
		Identity: Identity{
			SessionID: "session-smoke", ThreadID: "thread-smoke",
			TurnID: "turn-smoke",
		},
	}
}

type fixtureExecutor struct{}

func (fixtureExecutor) Descriptor() tool.Descriptor {
	return tool.Descriptor{
		Name: "fixture_process_smoke", Description: "fixture",
		Visibility: tool.VisibleInternal,
		Capability: tool.CapabilityProcess, AccessMode: tool.AccessRead,
		ParallelPolicy: tool.ParallelSerial, RepeatPolicy: tool.RepeatExecute,
		SandboxRequirement: tool.SandboxNone,
		Availability:       tool.AvailabilityAvailable,
		InputSchema:        map[string]any{"type": "object"},
	}
}

func (fixtureExecutor) Execute(context.Context, json.RawMessage) (tool.Result, error) {
	return tool.Result{}, nil
}
