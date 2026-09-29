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
	"github.com/fwtllh-png/QCode/internal/security/controlmatrix"
	"github.com/fwtllh-png/QCode/internal/security/policy"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
	"github.com/fwtllh-png/QCode/testutil/tooltest"
	sourcediff "github.com/sourcegraph/go-diff/diff"
)

func TestFileToolsPreserveModeAndApplyEdits(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "sample.txt")
	if err := os.WriteFile(path, []byte("one two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tools, err := NewWithBackend(root, fileTestBackend{})
	if err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry(nil, nil)
	if err := tools.Register(registry); err != nil {
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
		t.Context(), "edit", "file_edit",
		json.RawMessage(`{"path":"sample.txt","old":"two","new":"three"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "edited" {
		t.Fatalf("result = %+v", result)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "one three\n" || info.Mode().Perm() != 0o600 {
		t.Fatalf("content=%q mode=%o", data, info.Mode().Perm())
	}
}

func TestReadBeforeEditContract(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "sample.txt")
	if err := os.WriteFile(path, []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tools, err := NewWithBackend(root, fileTestBackend{})
	if err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry(nil, nil)
	if err := tools.Register(registry); err != nil {
		t.Fatal(err)
	}
	guard, err := toolguard.New(toolguard.Options{
		Registry: registry, Policy: policy.DefaultRuntime(policy.ModeAct, policy.PermissionBypass), Workspace: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	execute := func(id, name, arguments string) (tool.Result, error) {
		return guard.Execute(t.Context(), id, name, json.RawMessage(arguments))
	}
	if _, err := execute("exact-edit", "file_edit", `{"path":"sample.txt","old":"one","new":"two"}`); err != nil {
		t.Fatalf("exact edit without separate read: %v", err)
	}
	read, err := execute("read-edit", "file_read", `{"path":"sample.txt"}`)
	if err != nil {
		t.Fatal(err)
	}
	canonicalPath, _ := filepath.EvalSymlinks(path)
	if read.Outcome == nil || read.Outcome.Facts == nil ||
		read.Outcome.Facts.WorkspaceRead == nil ||
		read.Outcome.Facts.WorkspaceRead.Path != canonicalPath ||
		read.Outcome.Facts.WorkspaceRead.Digest == "" {
		t.Fatalf("read fingerprint facts = %#v", read.Outcome)
	}
	if _, err := execute("edit", "file_edit", `{"path":"sample.txt","old":"two","new":"three"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := execute("sequential-edit", "file_edit", `{"path":"sample.txt","old":"three","new":"four"}`); err != nil {
		t.Fatalf("sequential edit error = %v", err)
	}
	if err := os.WriteFile(path, []byte("external\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := execute("stale", "file_write", `{"path":"sample.txt","content":"clobber\n"}`); !errors.Is(err, workspacejournal.ErrStale) {
		t.Fatalf("external change error = %v", err)
	}
	if _, err := execute("new", "file_write", `{"path":"new.txt","content":"created\n"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := execute("read-rename", "file_read", `{"path":"sample.txt"}`); err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(root, "replacement.txt")
	if err := os.WriteFile(replacement, []byte("external\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	if _, err := execute("rename-exact", "file_edit", `{"path":"sample.txt","old":"external","new":"changed"}`); err != nil {
		t.Fatalf("exact edit after identity replacement: %v", err)
	}
	if _, err := execute("read-patch", "file_read", `{"path":"sample.txt"}`); err != nil {
		t.Fatal(err)
	}
	patch := "--- a/sample.txt\n+++ b/sample.txt\n@@ -1 +1 @@\n-changed\n+patched\n"
	patchArguments, _ := json.Marshal(map[string]string{"patch": patch})
	if _, err := guard.Execute(t.Context(), "patch", "file_patch", patchArguments); err != nil {
		t.Fatal(err)
	}
	secondPatch := "--- a/sample.txt\n+++ b/sample.txt\n@@ -1 +1 @@\n-patched\n+twice\n"
	secondPatchArguments, _ := json.Marshal(map[string]string{"patch": secondPatch})
	if _, err := guard.Execute(
		t.Context(), "patch-sequential", "file_patch", secondPatchArguments,
	); err != nil {
		t.Fatalf("sequential patch error = %v", err)
	}
	if err := os.WriteFile(path, []byte("external-again\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stalePatch := "--- a/sample.txt\n+++ b/sample.txt\n@@ -1 +1 @@\n-external-again\n+unsafe\n"
	stalePatchArguments, _ := json.Marshal(map[string]string{"patch": stalePatch})
	if _, err := guard.Execute(
		t.Context(), "patch-stale", "file_patch", stalePatchArguments,
	); !errors.Is(err, workspacejournal.ErrStale) {
		t.Fatalf("stale patch fingerprint error = %v", err)
	}
}

func TestFileToolsRejectTraversalAndBinary(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "binary"), []byte{0, 1}, 0o600); err != nil {
		t.Fatal(err)
	}
	tools, err := NewWithBackend(root, fileTestBackend{})
	if err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry(nil, nil)
	if err := tools.Register(registry); err != nil {
		t.Fatal(err)
	}
	for _, arguments := range []string{`{"path":"../outside"}`, `{"path":"binary"}`} {
		if _, err := tooltest.Execute(t.Context(), registry, tool.Call{
			Name: "file_read", Arguments: json.RawMessage(arguments),
		}); err == nil {
			t.Fatalf("read %s succeeded", arguments)
		}
	}
}

func TestMissingFilePathsCarryStructuredRecoveryHints(t *testing.T) {
	root := t.TempDir()
	tools, err := NewWithBackend(root, fileTestBackend{})
	if err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry(nil, nil)
	if err := tools.Register(registry); err != nil {
		t.Fatal(err)
	}

	for _, testCase := range []struct {
		name string
		args string
	}{
		{name: "file_read", args: `{"path":"missing/chapter.md"}`},
		{name: "file_list", args: `{"path":"missing"}`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := tooltest.Execute(t.Context(), registry, tool.Call{
				Name: testCase.name, Arguments: json.RawMessage(testCase.args),
			})
			if err == nil || !errors.Is(err, tool.ErrPrecondition) {
				t.Fatalf("%s error = %v, want recoverable precondition",
					testCase.name, err)
			}
			hint, ok := tool.RecoveryHintFromError(err)
			if !ok ||
				hint.ErrorCategory != "file_not_found" ||
				hint.RequiredAction != "file_list" ||
				hint.RetryOriginal {
				t.Fatalf("%s recovery hint = %+v, found=%v",
					testCase.name, hint, ok)
			}
		})
	}
}

func TestMissingFileSuggestsBoundedExistingSiblingPaths(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "docs", "context")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"01-prompt-message-context.md",
		"02-workspace-index-editor.md",
		"notes.txt",
	} {
		if err := os.WriteFile(
			filepath.Join(directory, name),
			[]byte(name),
			0o600,
		); err != nil {
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

	_, err = tooltest.Execute(t.Context(), registry, tool.Call{
		Name: "file_read",
		Arguments: json.RawMessage(
			`{"path":"docs/context/01-prompt-context.md"}`,
		),
	})
	if err == nil {
		t.Fatal("missing read succeeded")
	}
	hint, ok := tool.RecoveryHintFromError(err)
	if !ok || hint.RequiredAction != "use_existing_path" {
		t.Fatalf("recovery hint = %+v, found=%v", hint, ok)
	}
	want := []string{
		"docs/context/01-prompt-message-context.md",
		"docs/context/02-workspace-index-editor.md",
		"docs/context/notes.txt",
	}
	if !reflect.DeepEqual(hint.CandidatePaths, want) {
		t.Fatalf("candidate paths = %#v, want %#v", hint.CandidatePaths, want)
	}
}

func TestFileReadRangesAndStructuredListAreBounded(t *testing.T) {
	root := t.TempDir()
	var content strings.Builder
	for line := 1; line <= 250; line++ {
		fmt.Fprintf(&content, "line-%03d\n", line)
	}
	if err := os.WriteFile(filepath.Join(root, "large.txt"), []byte(content.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"c.txt", "a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0o600); err != nil {
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

	ranged, err := tooltest.Execute(t.Context(), registry, tool.Call{
		Name: "file_read", Arguments: json.RawMessage(
			`{"path":"large.txt","start_line":2,"max_lines":2}`,
		),
	})
	if err != nil {
		t.Fatal(err)
	}
	if ranged.Content != "line-002\nline-003" || ranged.Metadata["next_start_line"] != 4 {
		t.Fatalf("ranged read = %+v", ranged)
	}
	bounded, err := tooltest.Execute(t.Context(), registry, tool.Call{
		Name: "file_read", Arguments: json.RawMessage(`{"path":"large.txt"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bounded.Truncated || bounded.Metadata["returned_lines"] != defaultReadLines ||
		bounded.Metadata["next_start_line"] != defaultReadLines+1 {
		t.Fatalf("default read = %+v", bounded)
	}

	listed, err := tooltest.Execute(t.Context(), registry, tool.Call{
		Name: "file_list", Arguments: json.RawMessage(
			`{"path":".","offset":0,"limit":2}`,
		),
	})
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Entries []map[string]any `json:"entries"`
		Total   int              `json:"total"`
		HasMore bool             `json:"has_more"`
	}
	if err := json.Unmarshal([]byte(listed.Content), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Entries) != 2 || payload.Entries[0]["name"] != "a.txt" ||
		payload.Entries[0]["type"] != "file" || !payload.HasMore || payload.Total != 4 {
		t.Fatalf("list payload = %#v", payload)
	}
}

func TestFileReadExtractsSelectedPDFPages(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "fixture.pdf"), twoPagePDF(), 0o600); err != nil {
		t.Fatal(err)
	}
	tools, err := NewWithBackend(root, fileTestBackend{})
	if err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry(nil, nil)
	if err := tools.Register(registry); err != nil {
		t.Fatal(err)
	}
	result, err := tooltest.Execute(t.Context(), registry, tool.Call{
		Name: "file_read", Arguments: json.RawMessage(
			`{"path":"fixture.pdf","pages":"2"}`,
		),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, "Second page") ||
		strings.Contains(result.Content, "First page") ||
		result.Metadata["total_pages"] != 2 {
		t.Fatalf("PDF result = %+v", result)
	}
}

func twoPagePDF() []byte {
	streams := []string{
		"BT /F1 12 Tf 72 720 Td (First page) Tj ET",
		"BT /F1 12 Tf 72 720 Td (Second page) Tj ET",
	}
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R 5 0 R] /Count 2 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 7 0 R >> >> /Contents 4 0 R >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(streams[0]), streams[0]),
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 7 0 R >> >> /Contents 6 0 R >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(streams[1]), streams[1]),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}
	var document strings.Builder
	document.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects)+1)
	for index, object := range objects {
		offsets[index+1] = document.Len()
		fmt.Fprintf(&document, "%d 0 obj\n%s\nendobj\n", index+1, object)
	}
	xref := document.Len()
	fmt.Fprintf(&document, "xref\n0 %d\n0000000000 65535 f \n", len(offsets))
	for _, offset := range offsets[1:] {
		fmt.Fprintf(&document, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&document, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets), xref)
	return []byte(document.String())
}

func TestFilePatchAppliesMultiFileCreateDeleteRenameAndMode(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "old.txt"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "delete.txt"), []byte("delete\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tools, err := NewWithBackend(root, fileTestBackend{})
	if err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry(nil, nil)
	if err := tools.Register(registry); err != nil {
		t.Fatal(err)
	}
	patch := `diff --git a/old.txt b/renamed.txt
similarity index 100%
rename from old.txt
rename to renamed.txt
old mode 100644
new mode 100755
diff --git a/delete.txt b/delete.txt
deleted file mode 100644
--- a/delete.txt
+++ /dev/null
@@ -1 +0,0 @@
-delete
diff --git a/created.txt b/created.txt
new file mode 100644
--- /dev/null
+++ b/created.txt
@@ -0,0 +1 @@
+created
`
	executePatch(t, root, registry, patch)
	if _, err := os.Stat(filepath.Join(root, "old.txt")); !os.IsNotExist(err) {
		t.Fatalf("old.txt still exists: %v", err)
	}
	info, err := os.Stat(filepath.Join(root, "renamed.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("renamed mode = %o", info.Mode().Perm())
	}
	if data, err := os.ReadFile(filepath.Join(root, "created.txt")); err != nil || string(data) != "created\n" {
		t.Fatalf("created.txt = %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(root, "delete.txt")); !os.IsNotExist(err) {
		t.Fatalf("delete.txt still exists: %v", err)
	}
}

func TestFilePatchConflictIsAllOrNothing(t *testing.T) {
	root := t.TempDir()
	for name, content := range map[string]string{"one.txt": "one\n", "two.txt": "two\n"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
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
	patch := `--- a/one.txt
+++ b/one.txt
@@ -1 +1 @@
-one
+changed
--- a/two.txt
+++ b/two.txt
@@ -1 +1 @@
-missing
+changed
`
	data, _ := json.Marshal(map[string]string{"patch": patch})
	guarded := patchGuard(t, root, registry, patch)
	if _, err := guarded.Execute(
		t.Context(), "patch", "file_patch", data,
	); err == nil || !strings.Contains(err.Error(), "patch conflict") {
		t.Fatalf("file_patch error = %v", err)
	}
	for name, want := range map[string]string{"one.txt": "one\n", "two.txt": "two\n"} {
		got, err := os.ReadFile(filepath.Join(root, name))
		if err != nil || string(got) != want {
			t.Fatalf("%s = %q, %v", name, got, err)
		}
	}
}

func TestFilePatchRequiresStrongSandbox(t *testing.T) {
	tools, err := NewWithBackend(t.TempDir(), fileTestBackend{})
	if err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry(nil, nil)
	if err := tools.Register(registry); err != nil {
		t.Fatal(err)
	}
	_, descriptor, _, err := registry.Resolve("file_patch")
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.SandboxRequirement != tool.SandboxStrong {
		t.Fatalf("file_patch sandbox = %q, want strong", descriptor.SandboxRequirement)
	}
	for _, name := range []string{"file_read", "file_write", "file_edit", "file_list"} {
		_, descriptor, _, err := registry.Resolve(name)
		if err != nil {
			t.Fatal(err)
		}
		if descriptor.SandboxRequirement != tool.SandboxNone {
			t.Fatalf("%s sandbox = %q, want none", name, descriptor.SandboxRequirement)
		}
	}
}

func TestFileWritersAdvertiseParentDirectoryCreation(t *testing.T) {
	tools, err := NewWithBackend(t.TempDir(), fileTestBackend{})
	if err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry(nil, nil)
	if err := tools.Register(registry); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"file_write", "file_apply"} {
		_, descriptor, _, err := registry.Resolve(name)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(descriptor.Description, "parent directories") ||
			!strings.Contains(descriptor.Description, "placeholder") {
			t.Fatalf("%s description does not explain directory creation: %q", name, descriptor.Description)
		}
	}
}

func TestFileMutationToolsAreSerial(t *testing.T) {
	tools, err := NewWithBackend(t.TempDir(), fileTestBackend{})
	if err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry(nil, nil)
	if err := tools.Register(registry); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"file_write", "file_edit", "file_apply", "file_patch"} {
		_, descriptor, _, err := registry.Resolve(name)
		if err != nil {
			t.Fatal(err)
		}
		if descriptor.ParallelPolicy != tool.ParallelSerial {
			t.Fatalf("%s parallel policy = %q, want serial", name, descriptor.ParallelPolicy)
		}
	}
	for _, name := range []string{"file_read", "file_list"} {
		_, descriptor, _, err := registry.Resolve(name)
		if err != nil {
			t.Fatal(err)
		}
		if descriptor.ParallelPolicy != tool.ParallelConcurrent {
			t.Fatalf("%s parallel policy = %q, want concurrent", name, descriptor.ParallelPolicy)
		}
	}
}

func TestFilePatchCannotBypassGuardWithSandboxAttempt(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "sample.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tools, err := NewWithBackend(root, fileTestBackend{})
	if err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry(nil, nil)
	if err := tools.Register(registry); err != nil {
		t.Fatal(err)
	}
	patch := "--- a/sample.txt\n+++ b/sample.txt\n@@ -1 +1 @@\n-one\n+two\n"
	data, _ := json.Marshal(map[string]string{"patch": patch})
	ctx := toolguard.WithSandboxAttempt(t.Context(), toolguard.SandboxAttempt{Mode: toolguard.SandboxModeNone})
	if _, err := tooltest.Execute(ctx, registry, tool.Call{
		Name: "file_patch", Arguments: data,
	}); err == nil || !strings.Contains(err.Error(), "File Broker lease") {
		t.Fatalf("direct file_patch error = %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, "sample.txt"))
	if err != nil || string(got) != "one\n" {
		t.Fatalf("patched content = %q, %v", got, err)
	}
}

func executePatch(t *testing.T, root string, registry *tool.Registry, patch string) {
	t.Helper()
	data, _ := json.Marshal(map[string]string{"patch": patch})
	if _, err := patchGuard(t, root, registry, patch).Execute(
		t.Context(), "patch", "file_patch", data,
	); err != nil {
		t.Fatal(err)
	}
}

func patchGuard(
	t *testing.T,
	root string,
	registry *tool.Registry,
	patch string,
) *toolguard.Guard {
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
	files, err := sourcediff.ParseMultiFileDiff([]byte(patch))
	if err != nil {
		t.Fatal(err)
	}
	read := make(map[string]bool)
	var extraPaths []string
	for line := range strings.SplitSeq(patch, "\n") {
		if strings.HasPrefix(line, "rename from ") {
			extraPaths = append(
				extraPaths,
				strings.TrimSpace(strings.TrimPrefix(line, "rename from ")),
			)
		}
	}
	for _, file := range files {
		for _, value := range []string{file.OrigName, file.NewName} {
			path, exists, pathErr := patchPath(value)
			if pathErr != nil {
				t.Fatal(pathErr)
			}
			if !exists || read[path] {
				continue
			}
			if info, statErr := os.Stat(filepath.Join(root, path)); statErr == nil &&
				info.Mode().IsRegular() {
				arguments, _ := json.Marshal(map[string]string{"path": path})
				if _, readErr := guarded.Execute(
					t.Context(), "read-"+path, "file_read", arguments,
				); readErr != nil {
					t.Fatal(readErr)
				}
				read[path] = true
			}
		}
	}
	for _, path := range extraPaths {
		if read[path] {
			continue
		}
		if info, statErr := os.Stat(filepath.Join(root, path)); statErr == nil &&
			info.Mode().IsRegular() {
			arguments, _ := json.Marshal(map[string]string{"path": path})
			if _, readErr := guarded.Execute(
				t.Context(), "read-"+path, "file_read", arguments,
			); readErr != nil {
				t.Fatal(readErr)
			}
		}
	}
	return guarded
}

type fileTestBackend struct{}

func (fileTestBackend) Capability() sandbox.Capability {
	return sandbox.Capability{
		Platform: "test", Backend: "passthrough",
		Available: true,
		Effective: controlmatrix.
			Matrix{FilesystemRead: controlmatrix.
			FilesystemReadDeclaredRoots,

			FilesystemWrite: controlmatrix.
				FilesystemWriteExactPaths,

			Network: controlmatrix.NetworkDenied, ProcessTree: controlmatrix.ProcessTreeGroupKill, CrossProcess: controlmatrix.CrossProcessUnrestricted,
			Syscall: controlmatrix.SyscallDenyDangerous, IPC: controlmatrix.IPCUnrestricted, PathIdentity: controlmatrix.
					PathIdentityDescriptorRelative, ArtifactOrigin: controlmatrix.
					ArtifactOriginUnverifiedPath, DurableRecovery: controlmatrix.
					DurableRecoveryMemoryOnly},
	}
}

func (fileTestBackend) Prepare(_ context.Context, command sandbox.Command) (sandbox.Command, error) {
	return command, nil
}
