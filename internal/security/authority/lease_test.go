package authority

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/security/controlmatrix"
	"github.com/fwtllh-png/QCode/internal/security/effect"
)

func TestExecutionLeaseLifecycleIsSingleUse(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	manager := NewLeaseAuthority(LeaseAuthorityOptions{
		Now:    func() time.Time { return now },
		Random: bytes.NewReader(bytes.Repeat([]byte{1}, 128)),
	})
	operation, profile := fixtureLeaseInputs(t)
	lease, err := manager.Issue(LeaseIssueRequest{
		Operation: operation, Profile: profile,
		PolicyRevision: 7, SandboxPolicyID: "sandbox-policy",
		Attempt: 1, ExpiresAt: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	current := fixtureLeaseValidation(operation)
	if err := manager.Consume(lease, current); err != nil {
		t.Fatal(err)
	}
	if err := manager.Consume(lease, current); err == nil {
		t.Fatal("consumed lease was reused")
	}
	settlement := Settlement{
		Status: "succeeded", CompletedAt: now.Add(time.Second),
	}
	if err := manager.Settle(lease, settlement); err != nil {
		t.Fatal(err)
	}
	if err := manager.Settle(lease, settlement); err != nil {
		t.Fatalf("idempotent settlement: %v", err)
	}
	snapshot, err := manager.Snapshot(lease)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.State != LeaseSettled ||
		snapshot.OperationDigest != operation.Digest ||
		snapshot.PermissionProfile.Digest != profile.Digest {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if err := manager.Release(lease); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Snapshot(lease); err == nil {
		t.Fatal("released lease remained registered")
	}
}

func TestExecutionLeaseRejectsForgeryExpiryAndRevocation(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	manager := NewLeaseAuthority(LeaseAuthorityOptions{
		Now:    func() time.Time { return now },
		Random: bytes.NewReader(sequenceBytes(256)),
	})
	operation, profile := fixtureLeaseInputs(t)
	issue := func() ExecutionLease {
		lease, err := manager.Issue(LeaseIssueRequest{
			Operation: operation, Profile: profile,
			PolicyRevision: 7, SandboxPolicyID: "sandbox-policy",
			Attempt: 1, ExpiresAt: now.Add(time.Minute),
		})
		if err != nil {
			t.Fatal(err)
		}
		return lease
	}
	forged := issue()
	forged.nonce = strings.Repeat("0", 64)
	if err := manager.Consume(forged, fixtureLeaseValidation(operation)); err == nil {
		t.Fatal("forged lease was consumed")
	}
	expired := issue()
	now = now.Add(2 * time.Minute)
	if err := manager.Consume(expired, fixtureLeaseValidation(operation)); err == nil {
		t.Fatal("expired lease was consumed")
	}
	now = now.Add(-2 * time.Minute)
	revoked := issue()
	if err := manager.Revoke(revoked); err != nil {
		t.Fatal(err)
	}
	if err := manager.Consume(revoked, fixtureLeaseValidation(operation)); err == nil {
		t.Fatal("revoked lease was consumed")
	}
}

func sequenceBytes(size int) []byte {
	result := make([]byte, size)
	for index := range result {
		result[index] = byte(index)
	}
	return result
}

func TestExecutionLeaseIdentityCollisionFailsClosed(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	manager := NewLeaseAuthority(LeaseAuthorityOptions{
		Now:    func() time.Time { return now },
		Random: bytes.NewReader(bytes.Repeat([]byte{7}, 128)),
	})
	operation, profile := fixtureLeaseInputs(t)
	request := LeaseIssueRequest{
		Operation: operation, Profile: profile,
		PolicyRevision: 7, SandboxPolicyID: "sandbox-policy",
		Attempt: 1, ExpiresAt: now.Add(time.Minute),
	}
	if _, err := manager.Issue(request); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Issue(request); err == nil {
		t.Fatal("duplicate lease identity replaced an existing lease")
	}
}

func TestExecutionLeaseRejectsAuthorityGenerationDrift(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	operation, profile := fixtureLeaseInputs(t)
	tests := map[string]func(*LeaseValidation){
		"policy revision": func(value *LeaseValidation) { value.PolicyRevision++ },
		"workspace generation": func(value *LeaseValidation) {
			value.WorkspaceGeneration++
		},
		"subject generation": func(value *LeaseValidation) {
			value.SubjectGeneration++
		},
		"artifact": func(value *LeaseValidation) {
			value.ArtifactDigest = strings.Repeat("f", 64)
		},
		"attempt": func(value *LeaseValidation) { value.Attempt++ },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			manager := NewLeaseAuthority(LeaseAuthorityOptions{
				Now:    func() time.Time { return now },
				Random: bytes.NewReader(bytes.Repeat([]byte(name), 128)),
			})
			lease, err := manager.Issue(LeaseIssueRequest{
				Operation: operation, Profile: profile,
				PolicyRevision: 7, SandboxPolicyID: "sandbox-policy",
				Attempt: 1, ExpiresAt: now.Add(time.Minute),
			})
			if err != nil {
				t.Fatal(err)
			}
			current := fixtureLeaseValidation(operation)
			mutate(&current)
			if err := manager.Consume(lease, current); err == nil {
				t.Fatal("drifted lease was consumed")
			}
		})
	}
}

func TestRequiredControlsFailClosed(t *testing.T) {
	operation, profile := fixtureLeaseInputs(t)
	operation.Required.CrossProcess = controlmatrix.CrossProcessIsolated
	digest, err := operationDigest(operation)
	if err != nil {
		t.Fatal(err)
	}
	operation.Digest = digest
	manager := NewLeaseAuthority(LeaseAuthorityOptions{})
	_, err = manager.Issue(LeaseIssueRequest{
		Operation: operation, Profile: profile,
		PolicyRevision: 7, SandboxPolicyID: "sandbox-policy",
		Attempt: 1, ExpiresAt: time.Now().Add(time.Minute),
	})
	if err == nil {
		t.Fatal("missing cross-process control was accepted")
	}
}

func fixtureLeaseInputs(
	t *testing.T,
) (ExecutionOperation, EffectivePermissionProfile) {
	t.Helper()
	root := t.TempDir()
	input := fixtureCompileInput(t)
	input.SandboxPolicy.WorkspaceRoot = root
	input.Runtime.Revision = 7
	input.Invocation.CallID = "call-lease"
	profile, err := Compile(input)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := BuildExecutionOperation(OperationInput{
		WorkspaceRoot: root, WorkspaceGeneration: 9,
		Invocation: fixturePreparedInvocation(root),
		Effect: effect.Effect{
			Kind: effect.ProcessReadOnly, Risk: effect.RiskLow,
			Reversibility: effect.Reversible,
		},
		Required: RequiredControls{
			FilesystemRead: controlmatrix.FilesystemReadDeclaredRoots,
			Network:        controlmatrix.NetworkDenied,
			ProcessTree:    controlmatrix.ProcessTreeGroupKill,
			PathIdentity:   controlmatrix.PathIdentityDescriptorRelative,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	operation.Artifact = &ArtifactIntent{
		ManifestDigest: strings.Repeat("a", 64), Generation: 4,
	}
	digest, err := operationDigest(operation)
	if err != nil {
		t.Fatal(err)
	}
	operation.Digest = digest
	return operation, profile
}

func fixtureLeaseValidation(operation ExecutionOperation) LeaseValidation {
	return LeaseValidation{
		Operation: operation, PolicyRevision: 7,
		WorkspaceID:         operation.WorkspaceID,
		WorkspaceGeneration: operation.WorkspaceGeneration,
		SubjectDigest:       operation.Subject.Digest,
		SubjectGeneration:   operation.Subject.Generation,
		SandboxPolicyID:     "sandbox-policy",
		ArtifactDigest:      operation.Artifact.ManifestDigest,
		Attempt:             1,
	}
}
