package file

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	toolguard "github.com/fwtllh-png/QCode/internal/adapter/tool/guard"
	"github.com/fwtllh-png/QCode/internal/persist/workspacejournal"
	"github.com/fwtllh-png/QCode/internal/security/policy"
)

// applyTools builds a registry over a workspace seeded with the given files.
func applyTools(t *testing.T, files map[string]string) (string, *tool.Registry) {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tools, err := NewWithBackend(root, fileTestBackend{})
	if err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry(nil, nil)
	if err := tools.Register(registry); err != nil {
		t.Fatal(err)
	}
	return root, registry
}

func applyChanges(
	t *testing.T,
	root string,
	registry *tool.Registry,
	changes []map[string]any,
	dryRun bool,
) (tool.Result, error) {
	t.Helper()
	guarded, err := toolguard.New(toolguard.Options{
		Registry: registry,
		Policy: policy.DefaultRuntime(
			policy.ModeAct, policy.PermissionBypass,
		),
		Workspace: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	read := make(map[string]bool)
	for _, change := range changes {
		for _, field := range []string{"path", "to"} {
			path, _ := change[field].(string)
			if path == "" || read[path] {
				continue
			}
			if info, statErr := os.Stat(filepath.Join(root, path)); statErr == nil &&
				info.Mode().IsRegular() {
				arguments, _ := json.Marshal(map[string]string{"path": path})
				if _, readErr := guarded.Execute(
					t.Context(), "read-"+field+"-"+path, "file_read", arguments,
				); readErr != nil {
					t.Fatal(readErr)
				}
				read[path] = true
			}
		}
	}
	arguments, err := json.Marshal(map[string]any{"changes": changes, "dry_run": dryRun})
	if err != nil {
		t.Fatal(err)
	}
	return guarded.Execute(t.Context(), "apply", "file_apply", arguments)
}

func read(t *testing.T, root, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestFileApplyCommitsEveryOperationKind(t *testing.T) {
	root, registry := applyTools(t, map[string]string{
		"edit.txt":   "one\ntwo\n",
		"move.txt":   "moved\n",
		"delete.txt": "gone\n",
	})
	if err := os.Chmod(filepath.Join(root, "move.txt"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := applyChanges(t, root, registry, []map[string]any{
		{"op": "edit", "path": "edit.txt", "old": "two", "new": "three"},
		{"op": "write", "path": "nested/created.txt", "content": "new\n"},
		{"op": "move", "path": "move.txt", "to": "moved/target.txt"},
		{"op": "delete", "path": "delete.txt"},
	}, false)
	if err != nil {
		t.Fatal(err)
	}

	if got := read(t, root, "edit.txt"); got != "one\nthree\n" {
		t.Fatalf("edit.txt = %q", got)
	}
	if got := read(t, root, "nested/created.txt"); got != "new\n" {
		t.Fatalf("created.txt = %q", got)
	}
	if got := read(t, root, "moved/target.txt"); got != "moved\n" {
		t.Fatalf("moved target = %q", got)
	}
	info, err := os.Stat(filepath.Join(root, "moved/target.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("move target mode = %o, want the source's 600", info.Mode().Perm())
	}
	for _, name := range []string{"move.txt", "delete.txt"} {
		if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Fatalf("%s still exists: %v", name, err)
		}
	}

	files, _ := result.Metadata["files"].([]map[string]any)
	// Five entries for four operations: a move reports both of its paths.
	if len(files) != 5 {
		t.Fatalf("reported files = %#v", result.Metadata["files"])
	}
	kinds := make(map[string]string, len(files))
	for _, file := range files {
		path, _ := file["path"].(string)
		kind, _ := file["kind"].(string)
		kinds[path] = kind
	}
	want := map[string]string{
		"edit.txt": "modified", "nested/created.txt": "created",
		"moved/target.txt": "created", "move.txt": "deleted",
		"delete.txt": "deleted",
	}
	for path, kind := range want {
		if kinds[path] != kind {
			t.Fatalf("kind of %s = %q, want %q (all: %#v)", path, kinds[path], kind, kinds)
		}
	}
	if result.Metadata["added"] != 3 || result.Metadata["removed"] != 3 {
		t.Fatalf("line stats = %#v", result.Metadata)
	}
}

func TestFileApplyEditsTheSameFileTwiceInOneCall(t *testing.T) {
	root, registry := applyTools(t, map[string]string{"sample.txt": "alpha beta\n"})
	if _, err := applyChanges(t, root, registry, []map[string]any{
		{"op": "edit", "path": "sample.txt", "old": "alpha", "new": "gamma"},
		{"op": "edit", "path": "sample.txt", "old": "beta", "new": "delta"},
	}, false); err != nil {
		t.Fatal(err)
	}
	if got := read(t, root, "sample.txt"); got != "gamma delta\n" {
		t.Fatalf("sample.txt = %q", got)
	}
}

func TestFileApplyNoopSucceedsWithoutAWorkspaceMutation(t *testing.T) {
	root, registry := applyTools(t, map[string]string{
		"sample.txt": "already current\n",
	})
	result, err := applyChanges(t, root, registry, []map[string]any{
		{"op": "write", "path": "sample.txt", "content": "already current\n"},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "no changes" || result.Metadata["observed_changes"] != 0 {
		t.Fatalf("noop result = %+v", result)
	}
	if got := read(t, root, "sample.txt"); got != "already current\n" {
		t.Fatalf("sample.txt = %q", got)
	}
}

func TestFileApplyNoopDoesNotRequestApproval(t *testing.T) {
	root, registry := applyTools(t, map[string]string{
		"sample.txt": "already current\n",
	})
	requestedApproval := false
	guarded, err := toolguard.New(toolguard.Options{
		Registry:  registry,
		Policy:    policy.DefaultRuntime(policy.ModeAct, policy.PermissionSuggest),
		Workspace: root,
		Approvals: func(context.Context, toolguard.ApprovalRequest) error {
			requestedApproval = true
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := guarded.Execute(
		t.Context(), "read", "file_read", json.RawMessage(`{"path":"sample.txt"}`),
	); err != nil {
		t.Fatal(err)
	}
	result, err := guarded.Execute(
		t.Context(), "noop", "file_apply", json.RawMessage(`{
		"changes":[{"op":"write","path":"sample.txt","content":"already current\n"}]
	}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	if requestedApproval || result.Content != "no changes" {
		t.Fatalf("requested approval=%v result=%+v", requestedApproval, result)
	}
}

// Composition happens in memory, so a precondition that fails on the last
// operation must leave the files named by the earlier ones untouched.
func TestFileApplyValidationFailureWritesNothing(t *testing.T) {
	root, registry := applyTools(t, map[string]string{
		"first.txt":  "first\n",
		"repeat.txt": "same\nsame\n",
	})
	_, err := applyChanges(t, root, registry, []map[string]any{
		{"op": "write", "path": "first.txt", "content": "rewritten\n"},
		{"op": "write", "path": "created.txt", "content": "created\n"},
		{"op": "edit", "path": "repeat.txt", "old": "same", "new": "changed"},
	}, false)
	if err == nil || !strings.Contains(err.Error(), "want exactly once") {
		t.Fatalf("error = %v, want the ambiguous edit to be refused", err)
	}
	// Repeated code has no unique excerpt anchor, so the refusal must name
	// every matching line instead of leaving the model to re-read the file.
	if !strings.Contains(err.Error(), "lines [1 2]") {
		t.Fatalf("error lacks match lines: %v", err)
	}
	if got := read(t, root, "first.txt"); got != "first\n" {
		t.Fatalf("first.txt = %q, want it untouched", got)
	}
	if _, err := os.Stat(filepath.Join(root, "created.txt")); !os.IsNotExist(err) {
		t.Fatalf("created.txt exists after a rejected transaction: %v", err)
	}
}

func TestFileApplyEditMismatchCarriesStructuredRecoveryHint(t *testing.T) {
	_, registry := applyTools(t, map[string]string{
		"chapter.md": "# Current title\n",
	})
	_, _, executor, err := registry.Resolve("file_apply")
	if err != nil {
		t.Fatal(err)
	}
	planner, ok := executor.(tool.EditPlanner)
	if !ok {
		t.Fatal("file_apply does not implement EditPlanner")
	}
	arguments, err := json.Marshal(map[string]any{
		"changes": []map[string]any{{
			"op":   "edit",
			"path": "chapter.md",
			"old":  "# Stale title",
			"new":  "# New title",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = planner.PlanEdit(t.Context(), arguments)

	if !errors.Is(err, tool.ErrPrecondition) {
		t.Fatalf("PlanEdit() error = %v, want ErrPrecondition", err)
	}
	hint, ok := tool.RecoveryHintFromError(err)
	if !ok || hint.ErrorCategory != "edit_precondition_miss" ||
		hint.RequiredAction != "file_read" ||
		hint.Path != "chapter.md" ||
		hint.RetryOriginal {
		t.Fatalf("hint = %+v, found = %v", hint, ok)
	}
}

func TestFileApplyRejectsReconstructedNonContiguousOldText(t *testing.T) {
	content := "Policy 合并 Repository Rule、Tool Grant、Mode。\n" +
		"Deny/Hold 立即失败；Ask 可命中 Cache，或异步\n" +
		"请求 Host。Replacement Argument 必须重新 Prepare/Evaluate。\n"
	old := "或异步请求 Host。Replacement Argument 必须重新 Prepare/Evaluate。\n\n" +
		"Policy 合并 Repository Rule"

	_, _, err := replaceExact([]byte(content), old, "replacement", 1)
	if err == nil || !strings.Contains(err.Error(), "matched 0 times") {
		t.Fatalf("replaceExact error = %v", err)
	}
}

func TestFileApplyMismatchReturnsBoundedCurrentExcerpt(t *testing.T) {
	content := "## Trust Transition\n\n" +
		"Policy 合并 Repository Rule、Tool Grant、Mode。\n" +
		"Deny/Hold 立即失败；Ask 可命中 Cache，或异步\n" +
		"请求 Host。Replacement Argument 必须重新 Prepare/Evaluate。\n" +
		"Edit Plan 是 One-shot。\n"
	_, registry := applyTools(t, map[string]string{"guard.md": content})
	_, _, executor, err := registry.Resolve("file_apply")
	if err != nil {
		t.Fatal(err)
	}
	planner := executor.(tool.EditPlanner)
	arguments, err := json.Marshal(map[string]any{"changes": []map[string]any{{
		"op": "edit", "path": "guard.md",
		"old": "或异步请求 Host。Replacement Argument 必须重新 Prepare/Evaluate。\n\n" +
			"Policy 合并 Repository Rule",
		"new": "replacement",
	}}})
	if err != nil {
		t.Fatal(err)
	}

	_, err = planner.PlanEdit(t.Context(), arguments)
	hint, ok := tool.RecoveryHintFromError(err)
	if !ok || hint.RequiredAction != "file_read" ||
		hint.FailedChange != 1 || hint.MatchCount != 0 ||
		hint.StartLine != 2 || hint.EndLine != 7 ||
		!strings.Contains(hint.CurrentExcerpt, "Policy 合并") ||
		!strings.Contains(hint.CurrentExcerpt, "请求 Host") {
		t.Fatalf("hint = %+v, found = %v", hint, ok)
	}
}

// The apply phase can still fail on I/O. What is already written must be put
// back, so the turn never observes half a transaction.
func TestFileApplyRollsBackWritesWhenALaterWriteFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	root, registry := applyTools(t, map[string]string{
		"first.txt":         "first\n",
		"locked/second.txt": "second\n",
	})
	locked := filepath.Join(root, "locked")
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })

	_, err := applyChanges(t, root, registry, []map[string]any{
		{"op": "write", "path": "first.txt", "content": "rewritten\n"},
		{"op": "write", "path": "locked/second.txt", "content": "rewritten\n"},
	}, false)
	if err == nil || !strings.Contains(err.Error(), "locked/second.txt") {
		t.Fatalf("error = %v, want the failing path named", err)
	}
	if strings.Contains(err.Error(), "rollback failed") {
		t.Fatalf("error = %v, want a clean rollback", err)
	}
	if got := read(t, root, "first.txt"); got != "first\n" {
		t.Fatalf("first.txt = %q, want the rollback to restore it", got)
	}
	if got := read(t, root, "locked/second.txt"); got != "second\n" {
		t.Fatalf("locked/second.txt = %q", got)
	}
}

// A file created before the failure has no before-image to restore, so rollback
// has to remove it rather than leave a stray new file behind.
func TestFileApplyRollbackRemovesFilesItCreated(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	root, registry := applyTools(t, map[string]string{"locked/second.txt": "second\n"})
	locked := filepath.Join(root, "locked")
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })

	if _, err := applyChanges(t, root, registry, []map[string]any{
		{"op": "write", "path": "created.txt", "content": "created\n"},
		{"op": "write", "path": "locked/second.txt", "content": "rewritten\n"},
	}, false); err == nil {
		t.Fatal("expected the locked write to fail")
	}
	if _, err := os.Stat(filepath.Join(root, "created.txt")); !os.IsNotExist(err) {
		t.Fatalf("created.txt survived the rollback: %v", err)
	}
}

func TestFileApplyRefusesImpossibleOperations(t *testing.T) {
	root, registry := applyTools(t, map[string]string{
		"exists.txt": "exists\n", "other.txt": "other\n",
	})
	cases := []struct {
		name    string
		changes []map[string]any
		want    string
	}{{
		name:    "move onto an existing path",
		changes: []map[string]any{{"op": "move", "path": "exists.txt", "to": "other.txt"}},
		want:    "already exists",
	}, {
		name:    "edit a missing file",
		changes: []map[string]any{{"op": "edit", "path": "missing.txt", "old": "a", "new": "b"}},
		want:    "does not exist",
	}, {
		name:    "delete a missing file",
		changes: []map[string]any{{"op": "delete", "path": "missing.txt"}},
		want:    "does not exist",
	}, {
		name: "edit a file the transaction just deleted",
		changes: []map[string]any{
			{"op": "delete", "path": "exists.txt"},
			{"op": "edit", "path": "exists.txt", "old": "exists", "new": "back"},
		},
		want: "does not exist",
	}, {
		name:    "escape the workspace",
		changes: []map[string]any{{"op": "write", "path": "../outside.txt", "content": "x"}},
		want:    "escapes workspace",
	}, {
		// The schema rejects this before the core sees it; both layers refuse.
		name:    "unknown op",
		changes: []map[string]any{{"op": "touch", "path": "exists.txt"}},
		want:    "must be one of",
	}, {
		name:    "fields belonging to another op",
		changes: []map[string]any{{"op": "write", "path": "exists.txt", "content": "x", "old": "y"}},
		want:    "does not take old",
	}, {
		name:    "move without a destination",
		changes: []map[string]any{{"op": "move", "path": "exists.txt"}},
		want:    `requires "to"`,
	}, {
		name:    "move onto itself",
		changes: []map[string]any{{"op": "move", "path": "exists.txt", "to": "./exists.txt"}},
		want:    "same file",
	}}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := applyChanges(t, root, registry, testCase.changes, false)
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("error = %v, want it to mention %q", err, testCase.want)
			}
		})
	}
}

// The core validates ops on its own rather than trusting the schema to have
// done it: file_write and file_edit build changes in Go, bypassing the schema.
func TestChangeRequestValidation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		request changeRequest
		want    string
	}{
		{changeRequest{Op: opWrite}, "path is required"},
		{changeRequest{Path: "a.txt"}, "op is required"},
		{changeRequest{Op: "touch", Path: "a.txt"}, "unknown op"},
		{changeRequest{Op: opEdit, Path: "a.txt", New: "b"}, `requires a non-empty "old"`},
		{changeRequest{Op: opDelete, Path: "a.txt", Content: "x"}, "does not take content"},
	}
	for _, testCase := range cases {
		err := testCase.request.validate()
		if err == nil || !strings.Contains(err.Error(), testCase.want) {
			t.Fatalf("validate(%+v) = %v, want it to mention %q", testCase.request, err, testCase.want)
		}
	}
	valid := []changeRequest{
		{Op: opWrite, Path: "a.txt", Content: "x"},
		{Op: opWrite, Path: "a.txt"}, // truncating a file is a legitimate write
		{Op: opEdit, Path: "a.txt", Old: "x", New: ""},
		{Op: opMove, Path: "a.txt", To: "b.txt"},
		{Op: opDelete, Path: "a.txt"},
	}
	for _, request := range valid {
		if err := request.validate(); err != nil {
			t.Fatalf("validate(%+v) = %v", request, err)
		}
	}
}

func TestFileApplyRejectsAnEmptyOrOversizedTransaction(t *testing.T) {
	root, registry := applyTools(t, nil)
	if _, err := applyChanges(t, root, registry, nil, false); err == nil {
		t.Fatal("expected an empty transaction to be refused")
	}
	changes := make([]map[string]any, maxTransactionChanges+1)
	for index := range changes {
		changes[index] = map[string]any{
			"op": "write", "path": "file" + string(rune('a'+index%26)) + ".txt", "content": "x",
		}
	}
	if _, err := applyChanges(t, root, registry, changes, false); err == nil {
		t.Fatal("expected an oversized transaction to be refused")
	}
}

// snapshotTree records the name, bytes and mode of everything under root, so a
// test can assert the workspace is untouched down to leftover temporary files.
func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	tree := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			tree[relative] = "dir " + info.Mode().Perm().String()
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		tree[relative] = info.Mode().Perm().String() + " " + string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

func TestFileApplyDryRunPreviewsWithoutWriting(t *testing.T) {
	root, registry := applyTools(t, map[string]string{
		"edit.txt": "one\ntwo\nthree\n", "gone.txt": "gone\n",
	})
	before := snapshotTree(t, root)

	result, err := applyChanges(t, root, registry, []map[string]any{
		{"op": "edit", "path": "edit.txt", "old": "two", "new": "TWO"},
		{"op": "write", "path": "created.txt", "content": "created\n"},
		{"op": "delete", "path": "gone.txt"},
	}, true)
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		"--- a/edit.txt", "+++ b/edit.txt", "-two", "+TWO",
		"--- /dev/null", "+++ b/created.txt", "+created",
		"--- a/gone.txt", "+++ /dev/null", "-gone",
	} {
		if !strings.Contains(result.Content, want) {
			t.Fatalf("preview = %q, want it to contain %q", result.Content, want)
		}
	}
	if result.Metadata["dry_run"] != true {
		t.Fatalf("metadata = %#v, want dry_run", result.Metadata)
	}
	if result.Metadata["added"] != 2 || result.Metadata["removed"] != 2 {
		t.Fatalf("line stats = %#v", result.Metadata)
	}
	if after := snapshotTree(t, root); !reflect.DeepEqual(before, after) {
		t.Fatalf("workspace changed during a dry run:\nbefore %v\nafter  %v", before, after)
	}
}

// A dry run reports the same failures as the real thing, and still writes
// nothing when it succeeds only partly.
func TestFileApplyDryRunReportsValidationFailures(t *testing.T) {
	root, registry := applyTools(t, map[string]string{"repeat.txt": "same\nsame\n"})
	before := snapshotTree(t, root)
	if _, err := applyChanges(t, root, registry, []map[string]any{
		{"op": "edit", "path": "repeat.txt", "old": "same", "new": "changed"},
	}, true); err == nil {
		t.Fatal("expected the ambiguous edit to be refused in a dry run too")
	}
	if after := snapshotTree(t, root); !reflect.DeepEqual(before, after) {
		t.Fatalf("workspace changed during a failed dry run: %v vs %v", before, after)
	}
}

func TestFileApplyDryRunIsLowRiskAndDoesNotMutate(t *testing.T) {
	root, registry := applyTools(t, map[string]string{"edit.txt": "one\n"})
	var approvals int
	guard, err := toolguard.New(toolguard.Options{
		Registry: registry,
		Policy:   policy.DefaultRuntime(policy.ModeAct, policy.PermissionSuggest),
		Approvals: func(context.Context, toolguard.ApprovalRequest) error {
			approvals++
			return nil
		}, Workspace: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := guard.Execute(
		t.Context(), "read-1", "file_read",
		json.RawMessage(`{"path":"edit.txt"}`),
	); err != nil {
		t.Fatal(err)
	}
	arguments, err := json.Marshal(map[string]any{
		"dry_run": true,
		"changes": []map[string]any{
			{"op": "edit", "path": "edit.txt", "old": "one", "new": "two"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := guard.Execute(t.Context(), "call-1", "file_apply", arguments); err != nil {
		t.Fatal(err)
	}
	if approvals != 0 {
		t.Fatalf("dry run requested %d approvals", approvals)
	}
	content, err := os.ReadFile(filepath.Join(root, "edit.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "one\n" {
		t.Fatalf("dry run mutated workspace: %q", content)
	}
}

func TestPlannedWriteAppliesOnlyTheDisplayedContent(t *testing.T) {
	root, registry := applyTools(t, nil)
	var guarded *toolguard.Guard
	guarded, err := toolguard.New(toolguard.Options{
		Registry: registry,
		Policy:   policy.DefaultRuntime(policy.ModeAct, policy.PermissionSuggest),
		Approvals: func(_ context.Context, request toolguard.ApprovalRequest) error {
			if request.EditPlan == nil || len(request.EditPlan.Files) != 1 {
				return errors.New("missing edit plan")
			}
			if request.EditPlan.Files[0].After != "planned\n" ||
				request.EditPlan.Files[0].BeforeExists {
				return fmt.Errorf("unexpected edit plan: %+v", request.EditPlan)
			}
			return guarded.Decide(toolguard.ApprovalDecision{
				RequestID: request.RequestID, Approved: true,
				Scope: policy.ApprovalOnce, ExpiresAt: request.ExpiresAt,
				PlanID: request.EditPlan.ID,
			})
		},
		ForceEditPlanApproval: true, Workspace: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := guarded.Execute(
		t.Context(), "call-plan", "file_write",
		json.RawMessage(`{"path":"planned.txt","content":"planned\n"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, root, "planned.txt"); got != "planned\n" {
		t.Fatalf("planned.txt = %q", got)
	}
	if result.Content != "written" {
		t.Fatalf("result = %+v", result)
	}
}

func TestSuggestFileWriteUsesGuardedWriteWithoutApproval(t *testing.T) {
	root, registry := applyTools(t, nil)
	guarded, err := toolguard.New(toolguard.Options{
		Registry: registry,
		Policy:   policy.DefaultRuntime(policy.ModeAct, policy.PermissionSuggest),
		Approvals: func(_ context.Context, _ toolguard.ApprovalRequest) error {
			return errors.New("file_write unexpectedly requested approval")
		}, Workspace: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := guarded.Execute(
		t.Context(), "call-auto-write", "file_write",
		json.RawMessage(`{"path":"automatic.txt","content":"written\n"}`),
	); err != nil {
		t.Fatal(err)
	}
	if got := read(t, root, "automatic.txt"); got != "written\n" {
		t.Fatalf("automatic.txt = %q", got)
	}
}

func TestForcedEditPlanOverridesBroaderWriteGrant(t *testing.T) {
	root, registry := applyTools(t, nil)
	asked := false
	var guarded *toolguard.Guard
	guarded, err := toolguard.New(toolguard.Options{
		Registry: registry,
		Policy:   policy.DefaultRuntime(policy.ModeAct, policy.PermissionAuto),
		Approvals: func(_ context.Context, request toolguard.ApprovalRequest) error {
			asked = true
			if request.EditPlan == nil {
				return errors.New("missing forced edit plan")
			}
			return guarded.Decide(toolguard.ApprovalDecision{
				RequestID: request.RequestID, Approved: true,
				Scope: policy.ApprovalOnce, ExpiresAt: request.ExpiresAt,
				PlanID: request.EditPlan.ID,
			})
		},
		ForceEditPlanApproval: true, Workspace: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := guarded.Execute(
		t.Context(), "call-forced", "file_write",
		json.RawMessage(`{"path":"forced.txt","content":"forced\n"}`),
	); err != nil {
		t.Fatal(err)
	}
	if !asked || read(t, root, "forced.txt") != "forced\n" {
		t.Fatal("broader grant bypassed forced edit plan approval")
	}
}

func TestPlannedWriteRejectsWorkspaceDriftWithZeroWrites(t *testing.T) {
	root, registry := applyTools(t, nil)
	var guarded *toolguard.Guard
	guarded, err := toolguard.New(toolguard.Options{
		Registry: registry,
		Policy:   policy.DefaultRuntime(policy.ModeAct, policy.PermissionSuggest),
		Approvals: func(_ context.Context, request toolguard.ApprovalRequest) error {
			if request.EditPlan == nil {
				return errors.New("missing edit plan")
			}
			if err := os.WriteFile(
				filepath.Join(root, "planned.txt"), []byte("external\n"), 0o600,
			); err != nil {
				return err
			}
			return guarded.Decide(toolguard.ApprovalDecision{
				RequestID: request.RequestID, Approved: true,
				Scope: policy.ApprovalOnce, ExpiresAt: request.ExpiresAt,
				PlanID: request.EditPlan.ID,
			})
		},
		ForceEditPlanApproval: true, Workspace: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = guarded.Execute(
		t.Context(), "call-stale", "file_write",
		json.RawMessage(`{"path":"planned.txt","content":"planned\n"}`),
	)
	var decision *policy.DecisionError
	if !errors.As(err, &decision) || decision.Code != "edit_plan_stale" {
		t.Fatalf("Execute() error = %v, want edit_plan_stale", err)
	}
	if got := read(t, root, "planned.txt"); got != "external\n" {
		t.Fatalf("drifted file was overwritten: %q", got)
	}
}

func TestPlannedWriteRejectsWrongPlanIdentity(t *testing.T) {
	root, registry := applyTools(t, nil)
	var guarded *toolguard.Guard
	guarded, err := toolguard.New(toolguard.Options{
		Registry: registry,
		Policy:   policy.DefaultRuntime(policy.ModeAct, policy.PermissionSuggest),
		Approvals: func(_ context.Context, request toolguard.ApprovalRequest) error {
			return guarded.Decide(toolguard.ApprovalDecision{
				RequestID: request.RequestID, Approved: true,
				Scope: policy.ApprovalOnce, ExpiresAt: request.ExpiresAt,
				PlanID: strings.Repeat("0", 64),
			})
		},
		ForceEditPlanApproval: true, Workspace: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = guarded.Execute(
		t.Context(), "call-wrong-plan", "file_write",
		json.RawMessage(`{"path":"planned.txt","content":"planned\n"}`),
	)
	var decision *policy.DecisionError
	if !errors.As(err, &decision) || decision.Code != "edit_plan_mismatch" {
		t.Fatalf("Execute() error = %v, want edit_plan_mismatch", err)
	}
	if _, err := os.Stat(filepath.Join(root, "planned.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("wrong plan identity wrote a file: %v", err)
	}
}

// Exact edits carry their own content precondition. Destructive operations
// without one still require an explicit read of every existing path.
func TestFileApplyAcceptsExactEditsAndRequiresReadsForOtherWrites(t *testing.T) {
	root, registry := applyTools(t, map[string]string{
		"first.txt": "first\n", "second.txt": "second\n",
		"overwrite.txt": "before\n", "source.txt": "source\n",
	})
	guard, err := toolguard.New(toolguard.Options{
		Registry: registry, Policy: policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass), Workspace: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	apply := func(id string, changes ...map[string]any) error {
		arguments, err := json.Marshal(map[string]any{"changes": changes})
		if err != nil {
			t.Fatal(err)
		}
		_, err = guard.Execute(t.Context(), id, "file_apply", arguments)
		return err
	}
	readTool := func(id, path string) {
		t.Helper()
		arguments, _ := json.Marshal(map[string]string{"path": path})
		if _, err := guard.Execute(t.Context(), id, "file_read", arguments); err != nil {
			t.Fatal(err)
		}
	}
	editFirst := map[string]any{"op": "edit", "path": "first.txt", "old": "first", "new": "1"}
	editSecond := map[string]any{"op": "edit", "path": "second.txt", "old": "second", "new": "2"}

	// Creating a file needs no prior read: there is nothing to have read.
	if err := apply("create", map[string]any{
		"op": "write", "path": "created.txt", "content": "created\n",
	}); err != nil {
		t.Fatal(err)
	}
	if err := apply("exact-edits", editFirst, editSecond); err != nil {
		t.Fatalf("exact edits without separate reads: %v", err)
	}
	if got := read(t, root, "first.txt"); got != "1\n" {
		t.Fatalf("first.txt = %q", got)
	}
	if err := apply("overwrite-unread", map[string]any{
		"op": "write", "path": "overwrite.txt", "content": "after\n",
	}); !errors.Is(err, workspacejournal.ErrUnread) {
		t.Fatalf("error = %v, want ErrUnread for full overwrite", err)
	}
	readTool("read-overwrite", "overwrite.txt")
	if err := apply("overwrite", map[string]any{
		"op": "write", "path": "overwrite.txt", "content": "after\n",
	}); err != nil {
		t.Fatal(err)
	}
	// A move rewrites its source too, so the source needs a read of its own.
	if err := apply("move-unread", map[string]any{
		"op": "move", "path": "source.txt", "to": "target.txt",
	}); !errors.Is(err, workspacejournal.ErrUnread) {
		t.Fatalf("error = %v, want ErrUnread for the unread move source", err)
	}
	readTool("read-source", "source.txt")
	if err := apply("move", map[string]any{
		"op": "move", "path": "source.txt", "to": "target.txt",
	}); err != nil {
		t.Fatal(err)
	}
	if got := read(t, root, "target.txt"); got != "source\n" {
		t.Fatalf("target.txt = %q", got)
	}
}

// file_write and file_edit run through the transaction core now; their existing
// contract must not have moved.
func TestSingleFileToolsKeepTheirContract(t *testing.T) {
	root, registry := applyTools(t, map[string]string{"sample.txt": "one two\n"})
	if err := os.Chmod(filepath.Join(root, "sample.txt"), 0o600); err != nil {
		t.Fatal(err)
	}
	guarded, err := toolguard.New(toolguard.Options{
		Registry: registry,
		Policy: policy.DefaultRuntime(
			policy.ModeAct, policy.PermissionBypass,
		),
		Workspace: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := guarded.Execute(
		t.Context(), "read", "file_read",
		json.RawMessage(`{"path":"sample.txt"}`),
	); err != nil {
		t.Fatal(err)
	}
	result, err := guarded.Execute(
		t.Context(), "write", "file_write",
		json.RawMessage(`{"path":"sample.txt","content":"three\n"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "written" || result.Metadata["bytes"] != 6 {
		t.Fatalf("write result = %+v", result)
	}
	info, err := os.Stat(filepath.Join(root, "sample.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode after write = %o, want the existing 600 kept", info.Mode().Perm())
	}
	result, err = guarded.Execute(
		t.Context(), "edit", "file_edit",
		json.RawMessage(`{"path":"sample.txt","old":"three","new":"four"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "edited" || result.Metadata["replacements"] != 1 {
		t.Fatalf("edit result = %+v", result)
	}
	if got := read(t, root, "sample.txt"); got != "four\n" {
		t.Fatalf("sample.txt = %q", got)
	}
	if _, err := guarded.Execute(
		t.Context(), "fresh", "file_write",
		json.RawMessage(`{"path":"fresh.txt","content":"fresh\n"}`),
	); err != nil {
		t.Fatal(err)
	}
	if got := read(t, root, "fresh.txt"); got != "fresh\n" {
		t.Fatalf("fresh.txt = %q", got)
	}
}

// occurrences is a compare-and-swap rename: declaring the observed count
// replaces every match in one call, and a drifted count refuses without
// touching the file.
func TestFileEditOccurrencesReplaceEveryMatch(t *testing.T) {
	root, registry := applyTools(t, map[string]string{
		"repeat.txt": "value\nvalue\nvalue\n",
	})
	_, err := applyChanges(t, root, registry, []map[string]any{
		{"op": "edit", "path": "repeat.txt", "old": "value", "new": "renamed", "occurrences": 3},
	}, false)
	if err != nil {
		t.Fatalf("occurrences edit = %v", err)
	}
	if got := read(t, root, "repeat.txt"); got != "renamed\nrenamed\nrenamed\n" {
		t.Fatalf("repeat.txt = %q", got)
	}
}

func TestFileEditOccurrencesDriftRefusesAndReportsLines(t *testing.T) {
	root, registry := applyTools(t, map[string]string{
		"drift.txt": "token\ntoken\n",
	})
	_, err := applyChanges(t, root, registry, []map[string]any{
		{"op": "edit", "path": "drift.txt", "old": "token", "new": "x", "occurrences": 5},
	}, false)
	if err == nil || !strings.Contains(err.Error(), "want exactly 5") ||
		!strings.Contains(err.Error(), "lines [1 2]") {
		t.Fatalf("drifted occurrences error = %v", err)
	}
	if got := read(t, root, "drift.txt"); got != "token\ntoken\n" {
		t.Fatalf("drift.txt = %q, want untouched", got)
	}
}
