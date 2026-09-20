package agentcontext

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestWorkspaceReconciliationRewritesStaleTruthInHistory(t *testing.T) {
	truth := TruthCapsule{
		SchemaVersion: TruthSchemaVersion,
		Generation:    1, CompatibilityHash: "sha256:compat",
		ModelID: "model", ContextTokens: 4096,
		DownshiftPolicy: DownshiftRuntimeTruthOnly,
	}
	entity := NewTruthEntity(
		EntityChange,
		"changed.go",
		"changed.go verified",
		"runtime.evidence",
	)
	entity.Verified = true
	entity.VerificationSource = "runtime.evidence"
	entity.WorkspacePath = "changed.go"
	entity.WorkspaceDigest = "sha256:old"
	entity.WorkspaceClaimStatus = WorkspaceClaimCurrent
	truth.Entities = []TruthEntity{entity}
	truth.Seal()
	rendered, err := RenderStructured(
		Summary{Window: 2},
		truth,
		Narrative{Lines: []string{"old workspace conclusion"}},
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := manifestSnapshot(t, 3, []provider.Message{
		turnMessage(provider.RoleSystem, rendered.Text, 2),
	})
	snapshot.Workspace.BoundPaths = []BoundPath{{
		Path: "changed.go", ContentDigest: "sha256:old",
	}}
	snapshot.Workspace.Seal()
	snapshot.Evidence.Changes = []EvidenceChange{{
		Path: "changed.go", Turn: 2, Verified: true,
	}}
	snapshot.Compaction = Compaction{Count: 1, State: &CompactionState{
		ID: "compact-1", ThreadID: protocol.ThreadID("thread-1"),
		TurnID: protocol.TurnID("turn-2"), Phase: "fallback",
		PlanDigest: "sha256:plan", Truth: truth,
		SourceWindowID: "window-1", TargetWindowID: "window-2",
		SourceContextDigest: "sha256:context",
		FallbackReason:      "test fixture",
	}}
	if err := snapshot.Seal(); err != nil {
		t.Fatal(err)
	}
	current := snapshot.Workspace
	current.BoundPaths = append([]BoundPath(nil), current.BoundPaths...)
	current.BoundPaths[0].ContentDigest = "sha256:new"
	current.Seal()

	reconciled, _, err := ReconcileWorkspace(snapshot, current)
	if err != nil {
		t.Fatal(err)
	}
	restoredTruth, found, err := ParseTruthCapsule(
		reconciled.History[0].Text(),
	)
	if err != nil || !found || len(restoredTruth.Entities) != 1 {
		t.Fatalf("restored truth=%+v found=%t err=%v", restoredTruth, found, err)
	}
	got := restoredTruth.Entities[0]
	if got.Verified || got.WorkspaceClaimStatus != WorkspaceClaimStale ||
		strings.Contains(reconciled.History[0].Text(), "old workspace conclusion") {
		t.Fatalf("stale structured history survived reconciliation: %q", reconciled.History[0].Text())
	}
}

func TestWorkspaceReconciliationPreservesCarriedDigest(t *testing.T) {
	truth := TruthCapsule{
		SchemaVersion: TruthSchemaVersion,
		Generation:    1, CompatibilityHash: "sha256:compat",
		ModelID: "model", ContextTokens: 4096,
		DownshiftPolicy: DownshiftRuntimeTruthOnly,
	}
	entity := NewTruthEntity(
		EntityChange,
		"changed.go",
		"changed.go verified",
		"runtime.evidence",
	)
	entity.Verified = true
	entity.VerificationSource = "runtime.evidence"
	entity.WorkspacePath = "changed.go"
	entity.WorkspaceDigest = "sha256:old"
	entity.WorkspaceClaimStatus = WorkspaceClaimCurrent
	truth.Entities = []TruthEntity{entity}
	truth.Seal()
	rendered, err := RenderStructured(
		Summary{
			Window:        2,
			Digest:        []string{"user: keep this digest"},
			OmittedDigest: 1,
		},
		truth,
		Narrative{},
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := manifestSnapshot(t, 3, []provider.Message{
		turnMessage(provider.RoleSystem, rendered.Text, 2),
	})
	snapshot.Workspace.BoundPaths = []BoundPath{{
		Path: "changed.go", ContentDigest: "sha256:old",
	}}
	snapshot.Workspace.Seal()
	snapshot.Evidence.Changes = []EvidenceChange{{
		Path: "changed.go", Turn: 2, Verified: true,
	}}
	snapshot.Compaction = Compaction{Count: 1, State: &CompactionState{
		ID: "compact-1", ThreadID: protocol.ThreadID("thread-1"),
		TurnID: protocol.TurnID("turn-2"), Phase: "fallback",
		PlanDigest: "sha256:plan", Truth: truth,
		SourceWindowID: "window-1", TargetWindowID: "window-2",
		SourceContextDigest: "sha256:context",
		FallbackReason:      "test fixture",
	}}
	if err := snapshot.Seal(); err != nil {
		t.Fatal(err)
	}
	current := snapshot.Workspace
	current.BoundPaths = append([]BoundPath(nil), current.BoundPaths...)
	current.BoundPaths[0].ContentDigest = "sha256:new"
	current.Seal()

	reconciled, _, err := ReconcileWorkspace(snapshot, current)
	if err != nil {
		t.Fatal(err)
	}
	lines, omitted, ok := CarriedDigest(reconciled.History[0].Text())
	if !ok || omitted != 1 || len(lines) != 1 || lines[0] != "user: keep this digest" {
		t.Fatalf(
			"reconcile dropped digest: lines=%v omitted=%d ok=%v text=%s",
			lines, omitted, ok, reconciled.History[0].Text(),
		)
	}
}

func TestCaptureWorkspaceBindingRejectsSymlinkEscape(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(outside, "secret.txt"),
		[]byte("outside"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace, "linked")); err != nil {
		t.Skipf("create symlink: %v", err)
	}
	_, err := CaptureWorkspaceBinding(
		workspace,
		"workspace:test",
		1,
		[]string{"linked/secret.txt"},
	)
	if err == nil || !strings.Contains(err.Error(), "escapes root") {
		t.Fatalf("CaptureWorkspaceBinding() error = %v, want escape rejection", err)
	}
}

func TestWorkspaceBindingRejectsNonCanonicalPath(t *testing.T) {
	binding := WorkspaceBinding{
		WorkspaceIdentity: "workspace:test",
		BoundPaths: []BoundPath{{
			Path: "dir/../main.go", ContentDigest: "sha256:content",
		}},
	}
	binding.Seal()
	if err := binding.Validate(); err == nil {
		t.Fatal("WorkspaceBinding.Validate() accepted a non-canonical path")
	}
}

func TestRepositoryHeadResolvesPackedAndLinkedWorktreeRefs(t *testing.T) {
	repository := t.TempDir()
	git := filepath.Join(repository, ".git")
	if err := os.MkdirAll(
		filepath.Join(git, "worktrees", "feature"),
		0o700,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(git, "packed-refs"),
		[]byte("0123456789abcdef refs/heads/main\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	worktree := t.TempDir()
	worktreeGit := filepath.Join(git, "worktrees", "feature")
	for path, content := range map[string]string{
		filepath.Join(worktree, ".git"):         "gitdir: " + worktreeGit + "\n",
		filepath.Join(worktreeGit, "HEAD"):      "ref: refs/heads/main\n",
		filepath.Join(worktreeGit, "commondir"): "../..\n",
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got := repositoryHead(worktree); got != "0123456789abcdef" {
		t.Fatalf("repositoryHead() = %q", got)
	}
}
