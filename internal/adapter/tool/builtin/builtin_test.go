package builtin

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	toolguard "github.com/fwtllh-png/QCode/internal/adapter/tool/guard"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/toolsearch"
	"github.com/fwtllh-png/QCode/internal/persist/contentstore"
	"github.com/fwtllh-png/QCode/internal/platform/process"
	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
	"github.com/fwtllh-png/QCode/internal/security/policy"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
	"github.com/fwtllh-png/QCode/testutil/tooltest"
)

func TestCoreToolsRealWorkspace(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	root := t.TempDir()
	run(t, root, "git", "init", "-q")
	run(t, root, "git", "config", "user.email", "fixture@example.invalid")
	run(t, root, "git", "config", "user.name", "Fixture")
	if err := os.WriteFile(filepath.Join(root, "main.txt"), []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, root, "git", "add", "main.txt")
	run(t, root, "git", "commit", "-qm", "initial")
	registry, _, err := NewWithDependencies(
		root,
		builtinTestBackend{},
		contentstore.NewMemory(contentstore.Options{}),
		process.NewSessionManager(0),
	)
	if err != nil {
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
	executeGuarded := func(name, arguments string) tool.Result {
		t.Helper()
		ctx := tool.WithInvocationIdentity(
			t.Context(),
			tool.InvocationIdentity{ThreadID: "thread-builtin-test"},
		)
		ctx = tool.WithResultTokenBudget(ctx, registry.ResultTokenCapacity())
		result, executeErr := guarded.Execute(
			ctx, "call-"+name, name, json.RawMessage(arguments),
		)
		if executeErr != nil {
			t.Fatalf("%s: %v", name, executeErr)
		}
		return result
	}
	executeDirect := func(name, arguments string) tool.Result {
		t.Helper()
		ctx := tool.WithInvocationIdentity(
			t.Context(),
			tool.InvocationIdentity{ThreadID: "thread-builtin-test"},
		)
		ctx = tool.WithResultTokenBudget(ctx, registry.ResultTokenCapacity())
		result, executeErr := tooltest.Execute(ctx, registry, tool.Call{
			Name: name, Arguments: json.RawMessage(arguments),
		})
		if executeErr != nil {
			t.Fatalf("%s: %v", name, executeErr)
		}
		return result
	}

	executeGuarded("file_read", `{"path":"main.txt"}`)
	executeGuarded("file_edit", `{"path":"main.txt","old":"before","new":"after"}`)
	verified := executeGuarded("exec_command",
		`{"command":"test -f main.txt","verification":"check","covered_paths":["main.txt"]}`)
	if verified.IsError || verified.Execution == nil ||
		!verified.Execution.VerificationEvidenceAuthorized ||
		verified.Outcome == nil || verified.Outcome.Facts.Verification == nil ||
		verified.Outcome.Facts.Verification.Status != "passed" {
		t.Fatalf("guard did not authorize unified execution evidence: %+v", verified)
	}
	for _, name := range []string{
		"quality_test", "quality_diagnostics", "quality_review", "quality_verify", "quality_process_smoke",
	} {
		if _, _, _, err := registry.Resolve(name); err == nil {
			t.Fatalf("removed tool %s remains registered", name)
		}
	}
	search := executeDirect("search_text", `{"query":"after"}`)
	if !strings.Contains(search.Content, `"file":"main.txt"`) ||
		!strings.Contains(search.Content, `"line":1`) ||
		!strings.Contains(search.Content, `"text":"after"`) {
		t.Fatalf("search result = %q", search.Content)
	}
	if search.Outcome == nil || search.Outcome.Facts == nil ||
		len(search.Outcome.Facts.Evidence) == 0 {
		t.Fatalf("search typed facts = %+v", search.Outcome)
	}
	shell := executeDirect("exec_command", `{"command":"printf stdout; printf stderr >&2; exit 3"}`)
	if !shell.IsError || shell.Metadata["exit_code"] != 3 {
		t.Fatalf("shell result = %+v", shell)
	}
	diff := executeDirect("git_diff", `{}`)
	if !strings.Contains(diff.Content, "-before") || !strings.Contains(diff.Content, "+after") {
		t.Fatalf("git diff = %q", diff.Content)
	}
	info, err := os.Stat(filepath.Join(root, "main.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %o", info.Mode().Perm())
	}
}

func TestBuiltinRegistryUsesTypedOutcomeBoundary(t *testing.T) {
	registry, _, err := NewWithDependencies(
		t.TempDir(),
		builtinTestBackend{},
		contentstore.NewMemory(contentstore.Options{}),
		process.NewSessionManager(0),
	)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := registry.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := snapshot.Lookup(toolsearch.ToolName); !ok {
		t.Fatal("builtin registry has no bounded tool_search surface")
	}
	for _, descriptor := range registry.Descriptors(tool.VisibleModel) {
		if descriptor.Availability != tool.AvailabilityAvailable {
			continue
		}
		_, _, executor, resolveErr := registry.Resolve(descriptor.Name)
		if resolveErr != nil {
			t.Fatalf("resolve %s: %v", descriptor.Name, resolveErr)
		}
		if _, ok := executor.(tool.OutcomeExecutor); !ok {
			t.Errorf("%s executor %T has no typed Outcome boundary", descriptor.Name, executor)
		}
		if !tool.DispositionFor(executor).Valid() {
			t.Errorf("%s executor %T has no explicit disposition", descriptor.Name, executor)
		}
	}
}

type builtinTestBackend struct{}

func (builtinTestBackend) Capability() sandbox.Capability {
	return sandbox.Capability{
		Platform: "fixture", Backend: "passthrough",
		Available: true,
		Effective: securitymodel.Controls{FilesystemRead: securitymodel.FilesystemReadDeclaredRoots,

			FilesystemWrite: securitymodel.FilesystemWriteExactPaths, Network: securitymodel.NetworkDenied,
			ProcessTree: securitymodel.ProcessTreeGroupKill, CrossProcess: securitymodel.CrossProcessUnrestricted, Syscall: securitymodel.SyscallDenyDangerous,
			IPC: securitymodel.IPCUnrestricted, PathIdentity: securitymodel.PathIdentityDescriptorRelative, ArtifactOrigin: securitymodel.ArtifactOriginUnverifiedPath, DurableRecovery: securitymodel.DurableRecoveryMemoryOnly},
	}
}

func (builtinTestBackend) Prepare(
	_ context.Context, command sandbox.Command,
) (sandbox.Command, error) {
	command.PreparedReadOnly = command.WorkspaceReadOnly
	command.PreparedWritePaths = append(
		[]string(nil), command.WorkspaceWritePaths...,
	)
	command.PreparedNetworkDenied = command.DenyNetwork
	return command, nil
}

func run(t *testing.T, directory, name string, arguments ...string) {
	t.Helper()
	command := exec.Command(name, arguments...)
	command.Dir = directory
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("%s: %v: %s", name, err, output)
	}
}
