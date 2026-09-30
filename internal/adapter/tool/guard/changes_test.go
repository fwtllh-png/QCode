package guard

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/observability/verify"
	"github.com/fwtllh-png/QCode/internal/persist/contentstore"
	"github.com/fwtllh-png/QCode/internal/persist/workspacejournal"
	"github.com/fwtllh-png/QCode/internal/security/policy"
)

// patchExecutor stands in for file_patch: its arguments carry no path, so the
// only way to learn what it wrote is to look at the workspace.
type patchExecutor struct {
	descriptor tool.Descriptor
	apply      func(workspace string) error
	calls      atomic.Int32
}

func (e *patchExecutor) Descriptor() tool.Descriptor { return e.descriptor }
func (e *patchExecutor) Execute(_ context.Context, _ json.RawMessage) (tool.Result, error) {
	e.calls.Add(1)
	if e.apply != nil {
		if err := e.apply(""); err != nil {
			return tool.Result{}, err
		}
	}
	return tool.Result{Content: "applied"}, nil
}

func patchToolDescriptor() tool.Descriptor {
	descriptor := readDescriptor("patch")
	descriptor.Capability = tool.CapabilityWrite
	descriptor.AccessMode = tool.AccessTree
	descriptor.ResourceResolver = tool.ResourceResolver{PatchField: "patch"}
	descriptor.InputSchema = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"patch": map[string]any{"type": "string", "minLength": float64(1)},
		},
		"required": []string{"patch"}, "additionalProperties": false,
	}
	return descriptor
}

func transactionToolDescriptor() tool.Descriptor {
	descriptor := readDescriptor("apply")
	descriptor.Capability = tool.CapabilityWrite
	descriptor.AccessMode = tool.AccessTree
	descriptor.ResourceResolver = tool.ResourceResolver{ChangesField: "changes"}
	descriptor.InputSchema = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"changes": map[string]any{"type": "array", "minItems": float64(1)},
		},
		"required": []string{"changes"}, "additionalProperties": false,
	}
	return descriptor
}

// Claims, approval and the journal all key off the declared resources, so a path
// a transaction can write and the resolver does not list is an uncovered write.
// This is a security boundary: assert every operation's paths are declared.
func TestGuardDeclaresEveryTransactionPathAsAWrite(t *testing.T) {
	workspace := t.TempDir()
	for _, name := range []string{"edit.txt", "move.txt", "delete.txt"} {
		if err := os.WriteFile(filepath.Join(workspace, name), []byte("v\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	registry := newTestRegistry(
		t, nil, &patchExecutor{descriptor: transactionToolDescriptor()},
	)
	guard, err := New(Options{
		Registry:  registry,
		Policy:    policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass),
		Workspace: workspace,
	})
	if err != nil {
		t.Fatal(err)
	}
	arguments, err := json.Marshal(map[string]any{"changes": []map[string]any{
		{"op": "edit", "path": "edit.txt", "old": "v", "new": "w"},
		{"op": "write", "path": "nested/created.txt", "content": "hello\n"},
		{"op": "move", "path": "move.txt", "to": "moved.txt"},
		{"op": "delete", "path": "delete.txt"},
	}})
	if err != nil {
		t.Fatal(err)
	}

	invocation, _, err := guard.prepare(
		t.Context(), "apply", "call-1", arguments, tool.CatalogBinding{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := guard.Execute(t.Context(), "call-1", "apply", arguments); err != nil {
		t.Fatal(err)
	}

	// The guard canonicalises paths, which on macOS resolves the temporary
	// directory's symlink; compare against the same canonical root.
	root, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	declared := make(map[string]tool.AccessMode)
	for _, resource := range invocation.Resources {
		if resource.Kind == "file" {
			relative, err := filepath.Rel(root, resource.Path)
			if err != nil {
				t.Fatal(err)
			}
			declared[filepath.ToSlash(relative)] = resource.Access
		}
	}
	for _, path := range []string{
		"edit.txt", "nested/created.txt", "move.txt", "moved.txt", "delete.txt",
	} {
		access, ok := declared[path]
		if !ok {
			t.Fatalf("resource for %q missing, declared = %+v", path, declared)
		}
		if access != tool.AccessWrite {
			t.Fatalf("access for %q = %q, want write", path, access)
		}
	}
}

// A changes array the resolver cannot read must stop the call: enumerating no
// paths would hand the tool an unmediated write.
func TestGuardRefusesTransactionArgumentsItCannotEnumerate(t *testing.T) {
	workspace := t.TempDir()
	executor := &patchExecutor{descriptor: transactionToolDescriptor()}
	registry := newTestRegistry(t, nil, executor)
	guard, err := New(Options{
		Registry:  registry,
		Policy:    policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass),
		Workspace: workspace,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, arguments := range []string{
		`{"changes":{"op":"write","path":"a.txt"}}`,
		`{"changes":["a.txt"]}`,
		`{"changes":[{"op":"write","path":42}]}`,
		`{"changes":[{"op":"write","path":"../escape.txt"}]}`,
	} {
		if _, err := guard.Execute(
			t.Context(), "call", "apply", json.RawMessage(arguments),
		); err == nil {
			t.Fatalf("Execute(%s) succeeded", arguments)
		}
	}
	if executor.calls.Load() != 0 {
		t.Fatal("the tool ran with arguments the guard could not enumerate")
	}
}

func TestGuardObservesWritesFromToolsWithoutPathArguments(t *testing.T) {
	workspace := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(workspace, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("edited.txt", "old\n")
	write("removed.txt", "bye\n")
	write("untouched.txt", "same\n")

	executor := &patchExecutor{descriptor: patchToolDescriptor(), apply: func(string) error {
		write("edited.txt", "new\n")
		write("created.txt", "hello\n")
		write("untouched.txt", "same\n") // rewritten byte-for-byte: not a change
		return os.Remove(filepath.Join(workspace, "removed.txt"))
	}}
	registry := newTestRegistry(t, nil, executor)
	guard, err := New(Options{
		Registry:  registry,
		Policy:    policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass),
		Workspace: workspace,
	})
	if err != nil {
		t.Fatal(err)
	}

	patch := "--- a/edited.txt\n+++ b/edited.txt\n" +
		"--- /dev/null\n+++ b/created.txt\n" +
		"--- a/removed.txt\n+++ /dev/null\n" +
		"--- a/untouched.txt\n+++ b/untouched.txt\n"
	arguments, err := json.Marshal(map[string]string{"patch": patch})
	if err != nil {
		t.Fatal(err)
	}
	result, err := guard.Execute(t.Context(), "call-1", "patch", arguments)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome == nil || result.Outcome.Facts == nil {
		t.Fatalf("outcome = %+v, want observed changes", result.Outcome)
	}
	changes := result.Outcome.Facts.WorkspaceChanges
	got := make(map[string]string, len(changes))
	for _, change := range changes {
		got[change.Path] = change.Kind
	}
	want := map[string]string{
		"edited.txt":  FileModified,
		"created.txt": FileCreated,
		"removed.txt": FileDeleted,
	}
	if len(got) != len(want) {
		t.Fatalf("changes = %+v, want %+v", got, want)
	}
	for path, kind := range want {
		if got[path] != kind {
			t.Fatalf("change for %q = %q, want %q (all: %+v)", path, got[path], kind, got)
		}
	}
}

func TestGuardReportsNoChangesWhenNothingWasWritten(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "keep.txt"), []byte("v\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	registry := newTestRegistry(
		t, nil, &patchExecutor{descriptor: patchToolDescriptor()},
	)
	guard, err := New(Options{
		Registry:  registry,
		Policy:    policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass),
		Workspace: workspace,
	})
	if err != nil {
		t.Fatal(err)
	}
	arguments, err := json.Marshal(map[string]string{
		"patch": "--- a/keep.txt\n+++ b/keep.txt\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := guard.Execute(t.Context(), "call-1", "patch", arguments)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != nil && result.Outcome.Facts != nil &&
		len(result.Outcome.Facts.WorkspaceChanges) != 0 {
		t.Fatalf("outcome = %+v, want no observed changes", result.Outcome)
	}
}

func TestGuardRetainsFingerprintForFilesCreatedByExactProcessWrite(t *testing.T) {
	workspace := t.TempDir()
	created := filepath.Join(workspace, "generated.tmp")
	existing := filepath.Join(workspace, "existing.txt")
	if err := os.WriteFile(existing, []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	createdBefore, _, _, err := workspacejournal.Snapshot(created)
	if err != nil {
		t.Fatal(err)
	}
	existingBefore, _, _, err := workspacejournal.Snapshot(existing)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(created, []byte{0, 1, 2}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(existing, []byte("after\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	guard := &Guard{
		workspace:   workspace,
		readTracker: workspacejournal.NewReadTracker(),
	}
	result := &tool.Result{}
	err = guard.finishFileWrites(
		t.Context(),
		[]string{created, existing},
		map[string]workspacejournal.Fingerprint{
			created: createdBefore, existing: existingBefore,
		},
		result,
		true,
		false,
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := guard.readTracker.ValidateWrite(created); err != nil {
		t.Fatalf("created file fingerprint was not retained: %v", err)
	}
	if _, err := guard.readTracker.ValidateWrite(existing); !errors.Is(
		err,
		workspacejournal.ErrUnread,
	) {
		t.Fatalf("modified existing file validation = %v, want ErrUnread", err)
	}
}

// Line counts are measured against the content the turn started from, not the
// previous call, so a file edited twice reports the turn's cumulative delta.
func TestGuardCountsLinesAgainstTheTurnsStartingContent(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "file.txt")
	if err := os.WriteFile(path, []byte("a\nb\nc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	journal, err := workspacejournal.New(workspace, contentstore.NewMemory(contentstore.Options{}))
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Begin("turn-1"); err != nil {
		t.Fatal(err)
	}
	content := "a\nB\nc\n"
	executor := &patchExecutor{descriptor: patchToolDescriptor(), apply: func(string) error {
		return os.WriteFile(path, []byte(content), 0o600)
	}}
	registry := newTestRegistry(t, nil, executor)
	guard, err := New(Options{
		Registry:  registry,
		Policy:    policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass),
		Workspace: workspace, Journal: journal,
	})
	if err != nil {
		t.Fatal(err)
	}
	arguments, err := json.Marshal(map[string]string{
		"patch": "--- a/file.txt\n+++ b/file.txt\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	observe := func(call string) tool.WorkspaceChange {
		t.Helper()
		result, err := guard.Execute(t.Context(), call, "patch", arguments)
		if err != nil {
			t.Fatal(err)
		}
		changes := result.Outcome.Facts.WorkspaceChanges
		if len(changes) != 1 {
			t.Fatalf("changes = %+v, want one", changes)
		}
		return changes[0]
	}

	if change := observe("call-1"); change.Added != 1 || change.Removed != 1 {
		t.Fatalf("first call = %+v, want +1 -1", change)
	}
	content = "a\nB\nc\nd\n"
	change := observe("call-2")
	if change.Added != 2 || change.Removed != 1 {
		t.Fatalf("second call = %+v, want the turn's cumulative +2 -1", change)
	}
	// Lines mean nothing in binary content: report the change, not a count.
	content = "a\x00b"
	if change := observe("call-3"); change.Kind != FileModified ||
		change.Added != 0 || change.Removed != 0 {
		t.Fatalf("binary write = %+v, want a modification with no counts", change)
	}
}

// Without a before-image the turn cannot be rolled back, so a store that cannot
// hold one must stop the write instead of quietly giving up the guarantee.
func TestGuardRefusesWritesWhenTheBeforeImageCannotBeStored(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "keep.txt")
	if err := os.WriteFile(path, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	// One byte of capacity cannot hold the ten-byte before-image.
	store := contentstore.NewMemory(contentstore.Options{MaxBytes: 1, MaxEntries: 1})
	journal, err := workspacejournal.New(workspace, store)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Begin("turn-1"); err != nil {
		t.Fatal(err)
	}
	executor := &patchExecutor{descriptor: patchToolDescriptor(), apply: func(string) error {
		return os.WriteFile(path, []byte("clobbered"), 0o600)
	}}
	registry := newTestRegistry(t, nil, executor)
	guard, err := New(Options{
		Registry:  registry,
		Policy:    policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass),
		Workspace: workspace, Journal: journal,
	})
	if err != nil {
		t.Fatal(err)
	}
	arguments, err := json.Marshal(map[string]string{
		"patch": "--- a/keep.txt\n+++ b/keep.txt\n",
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = guard.Execute(t.Context(), "call-1", "patch", arguments)

	if !errors.Is(err, contentstore.ErrCapacity) {
		t.Fatalf("Execute() error = %v, want ErrCapacity", err)
	}
	if executor.calls.Load() != 0 {
		t.Fatal("the tool ran even though its write could not be journalled")
	}
	if data, _ := os.ReadFile(path); string(data) != "0123456789" {
		t.Fatalf("file = %q, want it untouched", data)
	}
}

type failingDiagnosticRunner struct {
	calls atomic.Int32
}

func (r *failingDiagnosticRunner) Run(context.Context, string) (verify.DiagnosticReceipt, error) {
	r.calls.Add(1)
	return verify.DiagnosticReceipt{}, errors.New("diagnostic process failed")
}

func TestGuardKeepsSuccessfulWritesWhenPostEditDiagnosticsFail(t *testing.T) {
	workspace := t.TempDir()
	paths := []string{
		filepath.Join(workspace, "first.md"),
		filepath.Join(workspace, "second.md"),
	}
	journal, err := workspacejournal.New(
		workspace,
		contentstore.NewMemory(contentstore.Options{}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Begin("turn-1"); err != nil {
		t.Fatal(err)
	}
	expected := make(map[string]workspacejournal.Fingerprint, len(paths))
	for _, path := range paths {
		if err := os.WriteFile(path, []byte("before\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		fingerprint, _, _, err := workspacejournal.Snapshot(path)
		if err != nil {
			t.Fatal(err)
		}
		expected[path] = fingerprint
		if err := journal.Before(t.Context(), path); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("after\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runner := &failingDiagnosticRunner{}
	guard := &Guard{
		workspace:   workspace,
		journal:     journal,
		readTracker: workspacejournal.NewReadTracker(),
		diagnostics: runner,
	}

	result := &tool.Result{}
	err = guard.finishFileWrites(
		t.Context(),
		paths,
		expected,
		result,
		true,
		true,
		true,
	)

	if err != nil || runner.calls.Load() != int32(len(paths)) {
		t.Fatalf("finishFileWrites() error = %v, diagnostic calls = %d", err, runner.calls.Load())
	}
	receipts := result.Outcome.Facts.Diagnostics
	if len(receipts) != len(paths) {
		t.Fatalf("diagnostic receipts = %#v", receipts)
	}
	for _, receipt := range receipts {
		if receipt.Status != "unavailable" ||
			receipt.ErrorCategory != "runner_failure" ||
			receipt.Message != "diagnostic process failed" {
			t.Fatalf("diagnostic receipt = %+v", receipt)
		}
	}
	if err := journal.Commit("turn-1"); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != "after\n" {
			t.Fatalf("%s = %q, want successful write retained", path, data)
		}
	}
}
