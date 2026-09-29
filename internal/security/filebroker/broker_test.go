package filebroker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/security/authority"
	"github.com/fwtllh-png/QCode/internal/security/effect"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

type recordingJournal struct {
	before   []string
	after    []string
	onBefore func(string) error
	onAfter  func(string) error
}

func (j *recordingJournal) Before(_ context.Context, path string) error {
	j.before = append(j.before, path)
	if j.onBefore != nil {
		return j.onBefore(path)
	}
	return nil
}

func (j *recordingJournal) After(path string) error {
	j.after = append(j.after, path)
	if j.onAfter != nil {
		return j.onAfter(path)
	}
	return nil
}

func TestBrokerCommitsExactPlanAndConsumesLeaseOnce(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "value.txt"), []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace, err := sandbox.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	plan := testPlan(t, workspace, "value.txt", []byte("after\n"))
	manager := authority.NewLeaseAuthority(authority.LeaseAuthorityOptions{})
	request := testRequest(t, root, manager, plan)
	broker, err := New(workspace, manager)
	if err != nil {
		t.Fatal(err)
	}
	journal := &recordingJournal{}
	request.Journal = journal

	result, err := broker.Commit(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "value.txt"))
	if err != nil || string(data) != "after\n" {
		t.Fatalf("committed data = %q, err = %v", data, err)
	}
	if result.Settlement.Status != "succeeded" ||
		len(journal.before) != 1 || len(journal.after) != 1 {
		t.Fatalf("result = %+v journal = %+v", result, journal)
	}
	if _, err := broker.Commit(t.Context(), request); err == nil ||
		!strings.Contains(err.Error(), "not issuable") {
		t.Fatalf("reused lease error = %v", err)
	}
}

func TestBrokerRejectsDriftBeforeAnyWrite(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "value.txt")
	if err := os.WriteFile(path, []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace, err := sandbox.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	plan := testPlan(t, workspace, "value.txt", []byte("after\n"))
	manager := authority.NewLeaseAuthority(authority.LeaseAuthorityOptions{})
	request := testRequest(t, root, manager, plan)
	broker, err := New(workspace, manager)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("external\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := broker.Commit(t.Context(), request); err == nil ||
		!strings.Contains(err.Error(), "stale") {
		t.Fatalf("drift error = %v", err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "external\n" {
		t.Fatalf("drifted file was overwritten: %q", data)
	}
}

func TestWorkspaceSnapshotRejectsHardlink(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, filepath.Join(root, "linked")); err != nil {
		t.Skipf("hardlinks unavailable: %v", err)
	}
	workspace, err := sandbox.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.SnapshotFile("linked"); err == nil ||
		!strings.Contains(err.Error(), "multiply linked") {
		t.Fatalf("hardlink snapshot error = %v", err)
	}
}

func TestPlanRejectsGitMetadata(t *testing.T) {
	sum := sha256.Sum256([]byte("index"))
	if _, err := NewPlan([]Entry{{
		Path: ".git/index",
		After: State{
			Exists: true, Digest: hex.EncodeToString(sum[:]), Mode: 0o600,
		},
		Data: []byte("index"),
	}}); err == nil || !strings.Contains(err.Error(), "control metadata") {
		t.Fatalf("Git metadata plan error = %v", err)
	}
}

func TestBrokerRejectsParentSwapAfterJournalBeforeImage(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "dir")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "value.txt"), []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace, err := sandbox.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	plan := testPlan(t, workspace, "dir/value.txt", []byte("after\n"))
	manager := authority.NewLeaseAuthority(authority.LeaseAuthorityOptions{})
	request := testRequest(t, root, manager, plan)
	broker, err := New(workspace, manager)
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	request.Journal = &recordingJournal{onBefore: func(string) error {
		if err := os.Rename(directory, filepath.Join(root, "original")); err != nil {
			return err
		}
		return os.Symlink(outside, directory)
	}}
	if _, err := broker.Commit(t.Context(), request); err == nil ||
		(!strings.Contains(err.Error(), "symbolic link") &&
			!strings.Contains(err.Error(), "not a directory")) {
		t.Fatalf("parent swap error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "value.txt")); !os.IsNotExist(err) {
		t.Fatalf("parent swap wrote outside workspace: %v", err)
	}
}

func TestBrokerRollsBackWhenJournalSettlementFails(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "value.txt")
	if err := os.WriteFile(path, []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace, err := sandbox.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	plan := testPlan(t, workspace, "value.txt", []byte("after\n"))
	manager := authority.NewLeaseAuthority(authority.LeaseAuthorityOptions{})
	request := testRequest(t, root, manager, plan)
	request.Journal = &recordingJournal{
		onAfter: func(string) error { return errors.New("ledger unavailable") },
	}
	broker, err := New(workspace, manager)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := broker.Commit(t.Context(), request); err == nil ||
		!strings.Contains(err.Error(), "ledger unavailable") {
		t.Fatalf("journal failure error = %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "before\n" {
		t.Fatalf("rollback data = %q, err = %v", data, err)
	}
}

func TestBrokerRollsBackEarlierWriteWhenLaterPathDrifts(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"first.txt", "second.txt"} {
		if err := os.WriteFile(
			filepath.Join(root, name), []byte("before\n"), 0o600,
		); err != nil {
			t.Fatal(err)
		}
	}
	workspace, err := sandbox.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	first := testPlan(t, workspace, "first.txt", []byte("after\n"))
	second := testPlan(t, workspace, "second.txt", []byte("after\n"))
	plan, err := NewPlan(append(first.Entries, second.Entries...))
	if err != nil {
		t.Fatal(err)
	}
	manager := authority.NewLeaseAuthority(authority.LeaseAuthorityOptions{})
	request := testRequest(t, root, manager, plan)
	broker, err := New(workspace, manager)
	if err != nil {
		t.Fatal(err)
	}
	broker.beforeApply = func(path string) error {
		if path != "second.txt" {
			return nil
		}
		broker.beforeApply = nil
		return os.WriteFile(
			filepath.Join(root, path), []byte("external\n"), 0o600,
		)
	}
	if _, err := broker.Commit(t.Context(), request); err == nil ||
		!strings.Contains(err.Error(), "changed before apply") {
		t.Fatalf("concurrent drift error = %v", err)
	}
	firstData, _ := os.ReadFile(filepath.Join(root, "first.txt"))
	secondData, _ := os.ReadFile(filepath.Join(root, "second.txt"))
	if string(firstData) != "before\n" || string(secondData) != "external\n" {
		t.Fatalf("rollback first=%q second=%q", firstData, secondData)
	}
}

func testPlan(
	t *testing.T,
	workspace *sandbox.Workspace,
	path string,
	after []byte,
) Plan {
	t.Helper()
	before, err := workspace.SnapshotFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(after)
	plan, err := NewPlan([]Entry{{
		Path: path,
		Before: State{
			Exists: before.Exists, Digest: before.Digest,
			Identity: before.Identity, Mode: uint32(before.Mode.Perm()),
		},
		BeforeData: before.Data,
		After: State{
			Exists: true, Digest: hex.EncodeToString(sum[:]),
			Mode: uint32(before.Mode.Perm()),
		},
		Data: after,
	}})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func testRequest(
	t *testing.T,
	root string,
	manager *authority.LeaseAuthority,
	plan Plan,
) Request {
	t.Helper()
	resources := make([]tool.Resource, 0, len(plan.Entries))
	for _, entry := range plan.Entries {
		resources = append(resources, tool.Resource{
			Kind:   "file",
			Path:   filepath.Join(root, filepath.FromSlash(entry.Path)),
			Access: tool.AccessWrite,
		})
	}
	rootSum := sha256.Sum256([]byte(filepath.Clean(root)))
	workspaceID := hex.EncodeToString(rootSum[:])
	invocation := tool.PreparedInvocation{
		CallID: "call-file", Tool: "file_write",
		Ref: tool.ToolRef{
			Name: "file_write", Source: "builtin:file_write",
			CatalogID: "catalog", Generation: 1, Revision: 1, Authority: 1,
		},
		Arguments: json.RawMessage(`{"path":"value.txt","content":"after\n"}`),
		Resources: resources,
		Descriptor: tool.Descriptor{
			Name: "file_write", Capability: tool.CapabilityWrite,
			AccessMode: tool.AccessWrite,
		},
	}
	invocation.Binding = tool.TrustedBindingFromDescriptor(
		invocation.Descriptor,
	)
	operation, err := authority.BuildExecutionOperation(authority.OperationInput{
		WorkspaceRoot: root, WorkspaceID: workspaceID,
		WorkspaceGeneration: 1, Invocation: invocation,
		Effect: effect.Effect{
			Kind: effect.WorkspaceEdit, Risk: effect.RiskMedium,
			Reversibility: effect.Reversible,
		},
		Journaled: true, RequireReadBeforeWrite: true,
		FileMutationDigest: plan.Digest,
	})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := authority.BuildManagedProcessProfile(
		authority.ManagedProfileInput{
			Operation: operation, Revision: 1, WorkspaceRoot: root,
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
	return Request{
		Lease: lease,
		Validation: authority.LeaseValidation{
			Operation: operation, PolicyRevision: 1,
			WorkspaceID: workspaceID, WorkspaceGeneration: 1,
			SubjectDigest:     operation.Subject.Digest,
			SubjectGeneration: operation.Subject.Generation,
			Attempt:           1,
		},
		Plan: plan,
	}
}
