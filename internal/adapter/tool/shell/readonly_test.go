package shell

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	toolguard "github.com/fwtllh-png/QCode/internal/adapter/tool/guard"
	"github.com/fwtllh-png/QCode/internal/persist/contentstore"
	"github.com/fwtllh-png/QCode/internal/persist/workspacejournal"
	"github.com/fwtllh-png/QCode/internal/platform/process"
	"github.com/fwtllh-png/QCode/internal/security/policy"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
	"github.com/fwtllh-png/QCode/internal/testutil/tooltest"
)

func TestShellReadUsesEnforcedReadOnlyWorkspace(t *testing.T) {
	root := t.TempDir()
	inputPath := filepath.Join(root, "input.txt")
	if err := os.WriteFile(inputPath, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	backend, err := sandbox.NewPlatformBackend(sandbox.Options{WorkspaceRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sandbox.CloseBackend(backend) })
	if err := sandbox.RequireControls(backend, sandbox.DefaultProcessRequirements()); err != nil {
		t.Skipf("strong sandbox unavailable: %v", err)
	}
	manager := process.NewSessionManager(4096)
	t.Cleanup(manager.CloseAll)
	registry := tool.NewRegistry(nil, nil)
	if err := RegisterWithManagerAndBackend(registry, root, manager, backend); err != nil {
		t.Fatal(err)
	}

	read, err := tooltest.Execute(t.Context(), registry, tool.Call{
		Name: "shell_read",
		Arguments: json.RawMessage(
			`{"command":"cat input.txt; printf temp > \"$TMPDIR/probe\"; cat \"$TMPDIR/probe\"; python3 - <<'PY'\nprint('heredoc')\nPY"}`,
		),
	})
	if err != nil {
		t.Fatal(err)
	}
	if read.IsError || read.Content != "originaltempheredoc\n" {
		t.Fatalf("read result = %+v", read)
	}

	unsupported, err := tooltest.Execute(t.Context(), registry, tool.Call{
		Name: "shell_read",
		Arguments: json.RawMessage(
			`{"command":"diff <(printf left) <(printf right)"}`,
		),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !unsupported.IsError ||
		unsupported.Metadata["error_category"] != "unsupported_shell_syntax" ||
		unsupported.Metadata["required_action"] !=
			"rewrite_without_process_substitution" {
		t.Fatalf("unsupported syntax result = %+v", unsupported)
	}

	for _, name := range []string{"shell_read", "exec_command"} {
		ctx := tool.WithInvocationIdentity(
			t.Context(),
			tool.InvocationIdentity{ThreadID: processTestThread},
		)
		write, err := tooltest.Execute(ctx, registry, tool.Call{
			Name:      name,
			Arguments: json.RawMessage(`{"command":"printf changed > input.txt"}`),
		})
		if err != nil {
			t.Fatal(err)
		}
		if !write.IsError {
			t.Fatalf("%s workspace write unexpectedly succeeded: %+v", name, write)
		}
		content, err := os.ReadFile(inputPath)
		if err != nil {
			t.Fatal(err)
		}
		if string(content) != "original" {
			t.Fatalf("%s changed workspace content to %q", name, content)
		}
	}

	if runtime.GOOS == "darwin" {
		hostTemp, err := os.CreateTemp("/private/var/tmp", "qcode-shell-read-secret-")
		if err != nil {
			t.Fatal(err)
		}
		hostTempPath := hostTemp.Name()
		t.Cleanup(func() { _ = os.Remove(hostTempPath) })
		if _, err := hostTemp.WriteString("host-secret"); err != nil {
			t.Fatal(err)
		}
		if err := hostTemp.Close(); err != nil {
			t.Fatal(err)
		}
		arguments, err := json.Marshal(map[string]string{"command": "cat " + hostTempPath})
		if err != nil {
			t.Fatal(err)
		}
		hostRead, err := tooltest.Execute(t.Context(), registry, tool.Call{
			Name:      "shell_read",
			Arguments: arguments,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !hostRead.IsError || strings.Contains(hostRead.Content, "host-secret") {
			t.Fatalf("host temp read unexpectedly succeeded: %+v", hostRead)
		}
	}

	modeBefore, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	chmod, err := tooltest.Execute(t.Context(), registry, tool.Call{
		Name:      "shell_read",
		Arguments: json.RawMessage(`{"command":"chmod 700 ."}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !chmod.IsError {
		t.Fatalf("workspace mode change unexpectedly succeeded: %+v", chmod)
	}
	modeAfter, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if modeAfter.Mode().Perm() != modeBefore.Mode().Perm() {
		t.Fatalf(
			"workspace mode changed from %o to %o",
			modeBefore.Mode().Perm(),
			modeAfter.Mode().Perm(),
		)
	}
}

func TestShellReadDescriptorIsReadOnly(t *testing.T) {
	descriptor := (&Tool{readOnly: true}).Descriptor()
	if descriptor.Name != "shell_read" ||
		descriptor.Capability != tool.CapabilityRead ||
		descriptor.AccessMode != tool.AccessRead ||
		descriptor.SandboxRequirement != tool.SandboxStrong ||
		!strings.Contains(descriptor.Description, "single quotes") ||
		!strings.Contains(descriptor.Description, "POSIX sh") ||
		!strings.Contains(descriptor.Description, "process substitution") {
		t.Fatalf("descriptor = %+v", descriptor)
	}
	for _, resource := range descriptor.ResourceResolver.Templates {
		if resource.Access != tool.AccessRead {
			t.Fatalf("resource = %+v", resource)
		}
	}
}

func TestUnsupportedPOSIXShellSyntaxIgnoresLiteralText(t *testing.T) {
	for _, command := range []string{
		`printf '%s\n' '<(literal)'`,
		`printf '%s\n' \<\(escaped\)`,
		"printf ok # <(comment)\n",
	} {
		if got := unsupportedPOSIXShellSyntax(command); got != "" {
			t.Fatalf("unsupportedPOSIXShellSyntax(%q) = %q", command, got)
		}
	}
	for _, command := range []string{
		`diff <(printf left) file`,
		`cat >(consumer)`,
		`printf "%s" <(producer)`,
	} {
		if got := unsupportedPOSIXShellSyntax(command); got == "" {
			t.Fatalf("unsupportedPOSIXShellSyntax(%q) accepted process substitution", command)
		}
	}
}

func TestShellRunExactWriteScopeIsGuardedAndObserved(t *testing.T) {
	root := t.TempDir()
	declared := filepath.Join(root, "declared.txt")
	undeclared := filepath.Join(root, "undeclared.txt")
	for path, content := range map[string]string{
		declared: "before\n", undeclared: "protected\n",
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	backend, err := sandbox.NewPlatformBackend(sandbox.Options{WorkspaceRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sandbox.CloseBackend(backend) })
	if err := sandbox.RequireControls(backend, sandbox.DefaultProcessRequirements()); err != nil {
		t.Skipf("strong sandbox unavailable: %v", err)
	}
	manager := process.NewSessionManager(4096)
	t.Cleanup(manager.CloseAll)
	registry := tool.NewRegistry(nil, nil)
	if err := RegisterWithManagerAndBackend(registry, root, manager, backend); err != nil {
		t.Fatal(err)
	}
	journal, err := workspacejournal.New(
		root, contentstore.NewMemory(contentstore.Options{}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Begin("turn-shell-write"); err != nil {
		t.Fatal(err)
	}
	guarded, err := toolguard.New(toolguard.Options{
		Registry: registry,
		Policy: policy.DefaultRuntime(
			policy.ModeAct, policy.PermissionBypass,
		), Workspace: root, Journal: journal,
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := guarded.Execute(
		t.Context(), "call-declared", "exec_command",
		json.RawMessage(
			`{"command":"printf 'after\n' > declared.txt","write_paths":["declared.txt"]}`,
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("declared write failed: %+v", result)
	}
	changes := result.Outcome.Facts.WorkspaceChanges
	if len(changes) != 1 ||
		changes[0].Path != "declared.txt" ||
		changes[0].Kind != tool.WorkspaceModified {
		t.Fatalf("observed changes = %+v", changes)
	}
	data, err := os.ReadFile(declared)
	if err != nil || string(data) != "after\n" {
		t.Fatalf("declared content = %q, error = %v", data, err)
	}

	created := filepath.Join(root, "created.txt")
	result, err = guarded.Execute(
		t.Context(), "call-created", "exec_command",
		json.RawMessage(
			`{"command":"printf 'created\n' > created.txt","write_paths":["created.txt"]}`,
		),
	)
	if err != nil || result.IsError {
		t.Fatalf("created write failed: result=%+v error=%v", result, err)
	}
	changes = result.Outcome.Facts.WorkspaceChanges
	if len(changes) != 1 ||
		changes[0].Path != "created.txt" ||
		changes[0].Kind != tool.WorkspaceCreated {
		t.Fatalf("created changes = %+v", changes)
	}
	data, err = os.ReadFile(created)
	if err != nil || string(data) != "created\n" {
		t.Fatalf("created content = %q, error = %v", data, err)
	}

	escaped, err := guarded.Execute(
		t.Context(), "call-escaped", "exec_command",
		json.RawMessage(
			`{"command":"printf escaped > undeclared.txt","write_paths":["declared.txt"]}`,
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !escaped.IsError {
		t.Fatalf("undeclared write unexpectedly succeeded: %+v", escaped)
	}
	data, err = os.ReadFile(undeclared)
	if err != nil || string(data) != "protected\n" {
		t.Fatalf("undeclared content = %q, error = %v", data, err)
	}
}

func TestShellRunWriteTreeCreatesFilesWithoutListing(t *testing.T) {
	root := t.TempDir()
	generated := filepath.Join(root, "generated")
	if err := os.Mkdir(generated, 0o700); err != nil {
		t.Fatal(err)
	}
	backend, err := sandbox.NewPlatformBackend(sandbox.Options{WorkspaceRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sandbox.CloseBackend(backend) })
	if err := sandbox.RequireControls(backend, sandbox.DefaultProcessRequirements()); err != nil {
		t.Skipf("strong sandbox unavailable: %v", err)
	}
	manager := process.NewSessionManager(4096)
	t.Cleanup(manager.CloseAll)
	registry := tool.NewRegistry(nil, nil)
	if err := RegisterWithManagerAndBackend(registry, root, manager, backend); err != nil {
		t.Fatal(err)
	}
	journal, err := workspacejournal.New(
		root, contentstore.NewMemory(contentstore.Options{}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Begin("turn-shell-write-tree"); err != nil {
		t.Fatal(err)
	}
	guarded, err := toolguard.New(toolguard.Options{
		Registry: registry,
		Policy: policy.DefaultRuntime(
			policy.ModeAct, policy.PermissionBypass,
		), Workspace: root, Journal: journal,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := guarded.Execute(
		t.Context(), "call-tree", "exec_command",
		json.RawMessage(
			`{"command":"printf 'generated\n' > generated/out.txt","write_paths":["generated"]}`,
		),
	)
	if err != nil || result.IsError {
		t.Fatalf("write tree failed: result=%+v error=%v", result, err)
	}
	created := filepath.Join(generated, "out.txt")
	data, err := os.ReadFile(created)
	if err != nil || string(data) != "generated\n" {
		t.Fatalf("tree content = %q, error = %v", data, err)
	}
	found := false
	for _, change := range result.Outcome.Facts.WorkspaceChanges {
		if change.Path == "generated/out.txt" && change.Kind == tool.WorkspaceCreated {
			found = true
		}
	}
	if !found {
		t.Fatalf("created tree file was not observed: %+v", result.Outcome.Facts.WorkspaceChanges)
	}
	escaped, err := guarded.Execute(
		t.Context(), "call-tree-escape", "exec_command",
		json.RawMessage(
			`{"command":"printf escaped > outside.txt","write_paths":["generated"]}`,
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !escaped.IsError {
		t.Fatalf("write outside the tree succeeded: %+v", escaped)
	}
}

func TestShellRunWriteGlobsExpandToExistingExactFiles(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{
		"docs/en/a.md", "docs/zh-CN/a.md", "docs/en/ignored.txt",
	} {
		absolute := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(absolute, []byte("before\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	workspace, err := sandbox.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	shell := &Tool{workspace: workspace}
	expanded, err := shell.ExpandArguments(t.Context(), json.RawMessage(
		`{"command":"true","write_globs":["docs/**/*.md"]}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	var values struct {
		WritePaths []string `json:"write_paths"`
		WriteGlobs []string `json:"write_globs"`
	}
	if err := json.Unmarshal(expanded, &values); err != nil {
		t.Fatal(err)
	}
	want := []string{"docs/en/a.md", "docs/zh-CN/a.md"}
	if !reflect.DeepEqual(values.WritePaths, want) || len(values.WriteGlobs) != 0 {
		t.Fatalf("expanded arguments = %s", expanded)
	}
}

func TestShellRunWriteGlobsRejectMissingAndEscapingPatterns(t *testing.T) {
	root := t.TempDir()
	workspace, err := sandbox.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	shell := &Tool{workspace: workspace}
	for _, pattern := range []string{"../*.md", "missing/**/*.md"} {
		raw, err := json.Marshal(map[string]any{
			"command": "true", "write_globs": []string{pattern},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := shell.ExpandArguments(t.Context(), raw); err == nil {
			t.Fatalf("write glob %q was accepted", pattern)
		}
	}
}

func TestShellRunAcceptsBoundedLargeExactWriteSet(t *testing.T) {
	root := t.TempDir()
	paths := make([]string, 0, 129)
	for index := range 129 {
		path := filepath.Join("docs", "chapter-"+strconv.Itoa(index)+".md")
		absolute := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(absolute, []byte("before\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	workspace, err := sandbox.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	shell := &Tool{workspace: workspace}
	resolved, err := shell.resolveWritePaths(paths)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved) != len(paths) {
		t.Fatalf("resolved paths = %d, want %d", len(resolved), len(paths))
	}
}

func TestShellRunAcceptsMissingExactWritePathWithExistingParent(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "generated"), 0o700); err != nil {
		t.Fatal(err)
	}
	workspace, err := sandbox.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	shell := &Tool{workspace: workspace}
	path := filepath.Join("generated", "new.txt")
	resolved, err := shell.resolveWritePaths([]string{path})
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved) != 1 ||
		resolved[0] != filepath.Join(workspace.Root(), path) {
		t.Fatalf("resolved paths = %+v", resolved)
	}
}

func TestShellRunRejectsMissingParentWritePaths(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "directory"), 0o700); err != nil {
		t.Fatal(err)
	}
	workspace, err := sandbox.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	shell := &Tool{workspace: workspace}
	resolved, err := shell.resolveWritePaths([]string{"directory"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved) != 1 {
		t.Fatalf("directory write tree = %v", resolved)
	}
	relative, err := filepath.Rel(workspace.Root(), resolved[0])
	if err != nil || relative != "directory" {
		t.Fatalf("directory write tree = %v", resolved)
	}
	if _, err := shell.resolveWritePaths([]string{"."}); err == nil {
		t.Fatal("workspace root write tree was accepted")
	}
	if _, err := shell.resolveWritePaths([]string{filepath.Join("missing", "new.txt")}); err == nil {
		t.Fatal("missing parent was accepted")
	}
}

func TestShellRunWriteGlobsAreJournaledAsExactFiles(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{"docs/en/a.md", "docs/zh-CN/a.md"} {
		absolute := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(absolute, []byte("before\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	backend, err := sandbox.NewPlatformBackend(sandbox.Options{WorkspaceRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sandbox.CloseBackend(backend) })
	if err := sandbox.RequireControls(backend, sandbox.DefaultProcessRequirements()); err != nil {
		t.Skipf("strong sandbox unavailable: %v", err)
	}
	registry := tool.NewRegistry(nil, nil)
	manager := process.NewSessionManager(4096)
	t.Cleanup(manager.CloseAll)
	if err := RegisterWithManagerAndBackend(registry, root, manager, backend); err != nil {
		t.Fatal(err)
	}
	journal, err := workspacejournal.New(
		root, contentstore.NewMemory(contentstore.Options{}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Begin("turn-glob"); err != nil {
		t.Fatal(err)
	}
	guarded, err := toolguard.New(toolguard.Options{
		Registry: registry,
		Policy: policy.DefaultRuntime(
			policy.ModeAct, policy.PermissionBypass,
		), Workspace: root, Journal: journal,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := guarded.Execute(
		t.Context(), "call-glob", "exec_command", json.RawMessage(
			`{"command":"for f in docs/*/*.md; do printf 'after\n' > \"$f\"; done",`+
				`"write_globs":["docs/**/*.md"]}`,
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	changes := result.Outcome.Facts.WorkspaceChanges
	if result.IsError || len(changes) != 2 {
		t.Fatalf("glob write result = %+v changes = %+v", result, changes)
	}
	if count, _ := result.Metadata["observed_changes"].(int); count != 2 {
		t.Fatalf("observed changes metadata = %#v", result.Metadata)
	}
}

func TestOnlyExecCommandAdvertisesExactWritePaths(t *testing.T) {
	run := execCommandDescriptor()
	if run.ResourceResolver.PathsField != "write_paths" {
		t.Fatalf("exec_command paths field = %q", run.ResourceResolver.PathsField)
	}
	if run.ResourceResolver.LoopbackField != "allow_loopback" {
		t.Fatalf(
			"exec_command loopback field = %q",
			run.ResourceResolver.LoopbackField,
		)
	}
	properties, _ := run.InputSchema["properties"].(map[string]any)
	if _, exists := properties["write_paths"]; !exists {
		t.Fatal("exec_command does not advertise write_paths")
	}
	if !strings.Contains(run.Description, "bounded write tree") ||
		!strings.Contains(run.Description, "does not permit the workspace root") ||
		!strings.Contains(run.Description, "file_write or file_apply") ||
		!strings.Contains(run.Description, "parent directories") ||
		!strings.Contains(run.Description, "$TMPDIR") ||
		!strings.Contains(run.Description, "absolute /tmp remains denied") {
		t.Fatalf("exec_command does not explain directory writes: %q", run.Description)
	}
	read := (&Tool{}).Descriptor()
	if !strings.Contains(read.Description, "$TMPDIR") ||
		!strings.Contains(read.Description, "absolute /tmp remains denied") {
		t.Fatalf("shell_read does not explain private temporary files: %q", read.Description)
	}
	network, _ := properties["network_targets"].(map[string]any)
	loopback, _ := properties["allow_loopback"].(map[string]any)
	items, _ := network["items"].(map[string]any)
	targetProperties, _ := items["properties"].(map[string]any)
	required, _ := items["required"].([]string)
	if network["maxItems"] != 32 ||
		targetProperties["host"] == nil ||
		targetProperties["protocol"] == nil ||
		targetProperties["port"] == nil ||
		targetProperties["methods"] == nil ||
		targetProperties["allow_private"] == nil ||
		len(required) != 5 ||
		loopback["type"] != "boolean" ||
		!strings.Contains(run.Description, "HTTPS control is at the CONNECT tunnel endpoint") ||
		!strings.Contains(run.Description, "Undeclared egress is denied") ||
		!strings.Contains(run.Description, "do not put localhost or port 0") ||
		!strings.Contains(network["description"].(string), "allow_loopback") {
		t.Fatalf("exec_command network target schema = %#v", network)
	}
	methods, _ := targetProperties["methods"].(map[string]any)
	port, _ := targetProperties["port"].(map[string]any)
	if methods["minItems"] != 1 ||
		!strings.Contains(methods["description"].(string), "tunnel endpoint") ||
		port["minimum"] != 1 ||
		!strings.Contains(port["description"].(string), "Port 0") {
		t.Fatalf("exec_command method schema = %#v", methods)
	}
	for _, descriptor := range []tool.Descriptor{
		(&Tool{readOnly: true}).Descriptor(),
		writeStdinDescriptor(),
	} {
		if descriptor.ResourceResolver.PathsField != "" {
			t.Fatalf("%s advertises write paths", descriptor.Name)
		}
		properties, _ := descriptor.InputSchema["properties"].(map[string]any)
		if _, exists := properties["write_paths"]; exists {
			t.Fatalf("%s schema accepts write_paths", descriptor.Name)
		}
	}
}

func TestExecCommandAcceptsHTTPSMethodNamesAndValidatesShapeBeforeApproval(t *testing.T) {
	root := t.TempDir()
	manager := process.NewSessionManager(4096)
	t.Cleanup(manager.CloseAll)
	registry := tool.NewRegistry(nil, nil)
	if err := RegisterWithManagerAndBackend(
		registry,
		root,
		manager,
		passthroughBackend{},
	); err != nil {
		t.Fatal(err)
	}
	requested := false
	guarded, err := toolguard.New(toolguard.Options{
		Registry: registry,
		Policy: policy.DefaultRuntime(
			policy.ModeAct,
			policy.PermissionSuggest,
		),

		Approvals: func(context.Context, toolguard.ApprovalRequest) error {
			requested = true
			return errors.New("stop before execution")
		}, Workspace: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Method names no longer encode protocol details: HTTPS destinations
	// accept ordinary method names because the managed proxy only controls
	// the CONNECT tunnel endpoint.
	if err := tool.ValidateDeclaredNetworkTargets([]tool.DeclaredNetworkTarget{{
		Host: "api.example.com", Protocol: "https", Port: 443,
		Methods: []string{"GET", "POST"}, AllowPrivate: false,
	}}); err != nil {
		t.Fatalf("https GET/POST declaration rejected: %v", err)
	}
	if err := tool.ValidateDeclaredNetworkTargets([]tool.DeclaredNetworkTarget{{
		Host: "api.example.com", Protocol: "https", Port: 443,
		Methods: []string{"CONNECT"}, AllowPrivate: false,
	}}); err != nil {
		t.Fatalf("https CONNECT declaration rejected: %v", err)
	}
	_, err = guarded.Execute(
		t.Context(),
		"invalid-port",
		"exec_command",
		json.RawMessage(`{
			"command":"curl https://api.example.com/",
			"network_targets":[{
				"host":"api.example.com",
				"protocol":"https",
				"port":0,
				"methods":["GET"],
				"allow_private":false
			}]
		}`),
	)
	if err == nil || !strings.Contains(err.Error(), `tool "exec_command" arguments`) {
		t.Fatalf("invalid port error = %v", err)
	}
	if requested {
		t.Fatal("invalid network target requested approval before validation")
	}
}
