package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/orchestration/subagent"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type workspaceTestThreads struct {
	registered map[protocol.ThreadID]ChildSpec
	released   []protocol.ThreadID
	err        error
}

func (r *workspaceTestThreads) RegisterChild(id protocol.ThreadID, spec ChildSpec) error {
	if r.err != nil {
		return r.err
	}
	r.registered[id] = spec
	return nil
}

func (r *workspaceTestThreads) Release(id protocol.ThreadID) {
	r.released = append(r.released, id)
	delete(r.registered, id)
}

type workspaceTestTools struct{ released []string }

func (t *workspaceTestTools) Release(root string) { t.released = append(t.released, root) }

type workspaceTestMerger struct {
	snapshotErr error
	verifyErr   error
	snapshots   []string
	verified    []string
	applied     []string
}

func (m *workspaceTestMerger) Snapshot(_ context.Context, root string) error {
	m.snapshots = append(m.snapshots, root)
	return m.snapshotErr
}

func (m *workspaceTestMerger) Verify(_ context.Context, root string) error {
	m.verified = append(m.verified, root)
	return m.verifyErr
}

func (*workspaceTestMerger) Plan(_ context.Context, root string) (tool.EditPlan, error) {
	return tool.EditPlan{ID: "plan:" + root}, nil
}

func (m *workspaceTestMerger) Apply(_ context.Context, sessionID, root, planID string) (tool.EditPlan, error) {
	m.applied = []string{sessionID, root, planID}
	return tool.EditPlan{ID: planID}, nil
}

func workspaceTestOptions(t *testing.T) IsolatedSessionWorkspaceOptions {
	t.Helper()
	root := t.TempDir()
	return IsolatedSessionWorkspaceOptions{
		Root: root, Trees: subagent.NewScratchWorktrees(root),
		Threads: &workspaceTestThreads{registered: make(map[protocol.ThreadID]ChildSpec)},
		Tools:   &workspaceTestTools{}, Merger: &workspaceTestMerger{}, AllowApply: true,
	}
}

func TestIsolatedSessionWorkspacesProvisionRestoreAndDiscard(t *testing.T) {
	options := workspaceTestOptions(t)
	manager := NewIsolatedSessionWorkspaces(options)
	first, err := manager.Provision(t.Context(), "session-one", "thread-one")
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Provision(t.Context(), "session-two", "thread-two")
	if err != nil {
		t.Fatal(err)
	}
	if first.Mode != SessionIsolationWorktree || first.Root == second.Root {
		t.Fatalf("isolated workspaces: first=%+v second=%+v", first, second)
	}
	threads := options.Threads.(*workspaceTestThreads)
	spec := threads.registered["thread-one"]
	if spec.SessionID != "session-one" || spec.Workspace != first.Root || !spec.HostSeeded || spec.Role != "chat" {
		t.Fatalf("registered child authority = %+v", spec)
	}
	plan, err := manager.PlanMerge(t.Context(), "session-one", "thread-one")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ApplyMerge(t.Context(), "session-one", "wrong-thread", plan.ID); err == nil {
		t.Fatal("merge accepted a different thread")
	}
	if _, err := manager.ApplyMerge(t.Context(), "session-one", "thread-one", plan.ID); err != nil {
		t.Fatal(err)
	}
	merger := options.Merger.(*workspaceTestMerger)
	if !slices.Equal(merger.applied, []string{"session-one", first.Root, plan.ID}) {
		t.Fatalf("merge binding = %v", merger.applied)
	}
	// Rebuild only the in-memory service; the existing directory is recovered.
	restarted := NewIsolatedSessionWorkspaces(options)
	restored, err := restarted.Restore(t.Context(), "session-one", "thread-one")
	if err != nil {
		t.Fatal(err)
	}
	if restored != first || !slices.Equal(merger.verified, []string{first.Root}) {
		t.Fatalf("restored=%+v verified=%v", restored, merger.verified)
	}
	if _, err := restarted.Restore(t.Context(), "session-one", "wrong-thread"); err == nil {
		t.Fatal("restore rebound a session to a different thread")
	}
	if err := restarted.Discard(t.Context(), "session-one", "thread-one"); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Discard(t.Context(), "session-one", "thread-one"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(threads.released, []protocol.ThreadID{"thread-one"}) ||
		!slices.Equal(options.Tools.(*workspaceTestTools).released, []string{first.Root}) {
		t.Fatal("discard did not release exactly the owning thread and tools")
	}
	if _, err := os.Stat(first.Root); !os.IsNotExist(err) {
		t.Fatalf("discarded root: %v", err)
	}
	if _, err := os.Stat(second.Root); err != nil {
		t.Fatalf("sibling root: %v", err)
	}
}

func TestIsolatedSessionWorkspacesCleanUpFailedProvision(t *testing.T) {
	for _, stage := range []string{"snapshot", "register"} {
		t.Run(stage, func(t *testing.T) {
			options := workspaceTestOptions(t)
			failure := errors.New("injected " + stage + " failure")
			if stage == "snapshot" {
				options.Merger.(*workspaceTestMerger).snapshotErr = failure
			} else {
				options.Threads.(*workspaceTestThreads).err = failure
			}
			manager := NewIsolatedSessionWorkspaces(options)
			if _, err := manager.Provision(t.Context(), "session", "thread"); !errors.Is(err, failure) {
				t.Fatalf("provision error = %v", err)
			}
			path := filepath.Join(options.Root, "worktrees", chatWorktreeID("session"))
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("failed provision retained worktree: %v", err)
			}
			if _, err := manager.PlanMerge(t.Context(), "session", "thread"); err == nil {
				t.Fatal("failed provision published a session binding")
			}
			if len(options.Threads.(*workspaceTestThreads).registered) != 0 {
				t.Fatal("failed provision registered a thread")
			}
		})
	}
}

func TestIsolatedSessionWorkspacesRestoreRejectsUntrustedRootAndBaseline(t *testing.T) {
	for _, failure := range []string{"symlink", "baseline"} {
		t.Run(failure, func(t *testing.T) {
			options := workspaceTestOptions(t)
			path := filepath.Join(options.Root, "worktrees", chatWorktreeID("session"))
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if failure == "symlink" {
				if err := os.Symlink(t.TempDir(), path); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
				options.Merger.(*workspaceTestMerger).verifyErr = errors.New("missing baseline")
			}
			manager := NewIsolatedSessionWorkspaces(options)
			if _, err := manager.Restore(t.Context(), "session", "thread"); err == nil {
				t.Fatal("restore accepted an invalid workspace")
			}
			if len(options.Threads.(*workspaceTestThreads).registered) != 0 {
				t.Fatal("invalid restore registered a thread")
			}
		})
	}
}

func TestIsolatedSessionWorkspacesReadOnlyMergeDoesNotCallWriter(t *testing.T) {
	options := workspaceTestOptions(t)
	options.AllowApply = false
	manager := NewIsolatedSessionWorkspaces(options)
	if _, err := manager.Provision(t.Context(), "session", "thread"); err != nil {
		t.Fatal(err)
	}
	plan, err := manager.PlanMerge(t.Context(), "session", "thread")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ApplyMerge(t.Context(), "session", "thread", plan.ID); err == nil || !strings.Contains(err.Error(), "read-only workspace") {
		t.Fatalf("read-only apply: %v", err)
	}
	if len(options.Merger.(*workspaceTestMerger).applied) != 0 {
		t.Fatal("read-only merge called the writer")
	}
}
