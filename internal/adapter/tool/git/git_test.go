package git

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/security/authority"
	"github.com/fwtllh-png/QCode/internal/security/controlmatrix"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
	"github.com/fwtllh-png/QCode/internal/security/workspacebroker"
	"github.com/fwtllh-png/QCode/internal/testutil/tooltest"
)

func TestLocalGitReadOperations(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	root := t.TempDir()
	runGit(t, root, "init")
	runGit(t, root, "config", "user.email", "fixture@example.test")
	runGit(t, root, "config", "user.name", "Fixture")
	if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("first\nsecond\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", "note.txt")
	runGit(t, root, "commit", "-m", "fixture")
	runGit(t, root, "remote", "add", "origin", "https://example.test/acme/repo.git")

	registry := tool.NewRegistry(nil, nil)
	if err := RegisterWithBackend(registry, root, gitTestBackend{}); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		args string
		want string
	}{
		{"git_remote", `{}`, "origin"},
		{"git_branch", `{}`, "* "},
		{"git_show", `{"revision":"HEAD","path":"note.txt"}`, "first"},
		{"git_blame", `{"path":"note.txt"}`, "fixture@example.test"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			result, err := tooltest.Execute(t.Context(), registry, tool.Call{
				Name: test.name, Arguments: json.RawMessage(test.args),
			})
			if err != nil {
				t.Fatal(err)
			}
			if result.IsError || !strings.Contains(result.Content, test.want) {
				t.Fatalf("result = %+v", result)
			}
		})
	}
}

func TestGitConfigEnvFollowsEnvironmentProfile(t *testing.T) {
	isolated := gitConfigEnv(profilePolicyBackend{profile: "isolated"})
	if len(isolated) != 2 {
		t.Fatalf("isolated git config env = %v", isolated)
	}
	native := gitConfigEnv(profilePolicyBackend{profile: "native"})
	if native != nil {
		t.Fatalf("native still nulled git config: %v", native)
	}
}

func TestGitReadOperationsRequestReadOnlyOfflineSandbox(t *testing.T) {
	root := t.TempDir()
	runGit(t, root, "init", "-q")
	backend := &recordingGitBackend{}
	registry := tool.NewRegistry(nil, nil)
	if err := RegisterWithBackend(registry, root, backend); err != nil {
		t.Fatal(err)
	}
	result, err := tooltest.Execute(t.Context(), registry, tool.Call{
		Name: "git_status", Arguments: json.RawMessage(`{}`),
	})
	if err != nil || result.IsError {
		t.Fatalf("git_status result = %+v, err = %v", result, err)
	}
	if !backend.command.WorkspaceReadOnly || !backend.command.DenyNetwork {
		t.Fatalf("sandbox command = %+v", backend.command)
	}
}

func TestGitStatusUsesPlatformReadOnlySandbox(t *testing.T) {
	root := t.TempDir()
	runGit(t, root, "init", "-q")
	backend, err := sandbox.NewPlatformBackend(sandbox.Options{
		WorkspaceRoot: root,
		PrivateTemp:   t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sandbox.CloseBackend(backend) })
	if err := sandbox.RequireControls(backend, sandbox.DefaultProcessRequirements()); err != nil {
		t.Skipf("strong sandbox unavailable: %v", err)
	}
	registry := tool.NewRegistry(nil, nil)
	if err := RegisterWithBackend(registry, root, backend); err != nil {
		t.Fatal(err)
	}
	result, err := tooltest.Execute(t.Context(), registry, tool.Call{
		Name: "git_status", Arguments: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("git_status result = %+v", result)
	}
}

func TestManagedGitMutationWorkflow(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	root := t.TempDir()
	runGit(t, root, "init", "-q", "-b", "main")
	runGit(t, root, "config", "user.email", "fixture@example.test")
	runGit(t, root, "config", "user.name", "Fixture")
	if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", "note.txt")
	runGit(t, root, "commit", "-qm", "seed")

	broker, err := workspacebroker.New(
		root,
		authority.NewLeaseAuthority(authority.LeaseAuthorityOptions{}),
		time.Minute,
	)
	if err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry(nil, nil)
	if err := RegisterWithBackendAndRuntime(
		registry, root, gitTestBackend{}, broker,
	); err != nil {
		t.Fatal(err)
	}
	executeContext := func(ctx context.Context, name, arguments string) tool.Result {
		t.Helper()
		result, executeErr := tooltest.Execute(ctx, registry, tool.Call{
			Name: name, Arguments: json.RawMessage(arguments),
		})
		if executeErr != nil {
			t.Fatalf("%s: %v", name, executeErr)
		}
		if result.IsError {
			t.Fatalf("%s result = %+v", name, result)
		}
		return result
	}
	execute := func(name, arguments string) tool.Result {
		return executeContext(t.Context(), name, arguments)
	}
	restricted, err := sandbox.WithExecutionAuthority(
		t.Context(),
		sandbox.ExecutionAuthority{
			Digest: strings.Repeat("a", 64), Enforcement: "none",
			WorkspaceRoot: root,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	mutate := func(name, arguments string) tool.Result {
		return executeContext(restricted, name, arguments)
	}

	if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mutate("git_add", `{"paths":["note.txt"]}`)
	committed := mutate("git_commit", `{"message":"update note"}`)
	if !strings.Contains(committed.Content, `"revision"`) {
		t.Fatalf("commit result = %q", committed.Content)
	}
	log := execute("git_log", `{"limit":1}`)
	if !strings.Contains(log.Content, "update note") {
		t.Fatalf("log after commit = %q", log.Content)
	}
	mutate("git_switch", `{"branch":"feature","create":true}`)
	branch := execute("git_branch", `{}`)
	if !strings.Contains(branch.Content, "* feature") {
		t.Fatalf("branches after switch = %q", branch.Content)
	}

	remote := filepath.Join(t.TempDir(), "remote.git")
	runGit(t, filepath.Dir(remote), "init", "--bare", "-q", remote)
	runGit(t, root, "remote", "add", "origin", remote)
	mutate("git_push", `{"remote":"origin","branch":"feature"}`)
	command := exec.Command("git", "rev-parse", "--verify", "refs/heads/feature")
	command.Dir = remote
	if output, verifyErr := command.CombinedOutput(); verifyErr != nil {
		t.Fatalf("verify pushed branch: %v\n%s", verifyErr, output)
	}
	mutate("git_fetch", `{"remote":"origin"}`)

	publisher := t.TempDir()
	runGit(t, publisher, "clone", "-q", "--branch", "feature", remote, ".")
	runGit(t, publisher, "config", "user.email", "publisher@example.test")
	runGit(t, publisher, "config", "user.name", "Publisher")
	if err := os.WriteFile(filepath.Join(publisher, "remote.txt"), []byte("remote\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, publisher, "add", "remote.txt")
	runGit(t, publisher, "commit", "-qm", "remote update")
	runGit(t, publisher, "push", "-q", "origin", "feature")
	pulled := mutate("git_pull", `{"remote":"origin","branch":"feature"}`)
	if pulled.Outcome == nil || pulled.Outcome.Facts == nil ||
		len(pulled.Outcome.Facts.WorkspaceChanges) != 1 {
		t.Fatalf("pull outcome = %+v", pulled.Outcome)
	}
	if _, err := os.Stat(filepath.Join(root, "remote.txt")); err != nil {
		t.Fatalf("pulled file: %v", err)
	}
}

func TestGitMutationToolsAreUnavailableWithoutBroker(t *testing.T) {
	registry := tool.NewRegistry(nil, nil)
	if err := RegisterWithBackend(registry, t.TempDir(), gitTestBackend{}); err != nil {
		t.Fatal(err)
	}
	descriptors := make(map[string]tool.Descriptor)
	for _, descriptor := range registry.Descriptors(tool.VisibleModel) {
		descriptors[descriptor.Name] = descriptor
	}
	for _, name := range []string{
		"git_add", "git_commit", "git_switch", "git_fetch", "git_pull", "git_push",
		"git_amend", "git_merge", "git_rebase", "git_cherry_pick",
		"git_restore", "git_stash", "git_tag", "git_conflict",
	} {
		descriptor, ok := descriptors[name]
		if !ok {
			t.Fatalf("%s is missing from the model catalog", name)
		}
		if descriptor.Availability != tool.AvailabilityUnavailable {
			t.Fatalf("%s availability = %q", name, descriptor.Availability)
		}
	}
}

func TestGitMutationBindingsDeclareConsequentialEffects(t *testing.T) {
	tests := map[string]struct {
		kind          tool.EffectKind
		risk          tool.RiskLevel
		reversibility tool.Reversibility
		approval      tool.ApprovalMode
	}{
		"git_add":    {tool.EffectProcessMutating, tool.RiskMedium, tool.Bounded, tool.ApprovalPolicyDefault},
		"git_commit": {tool.EffectProcessMutating, tool.RiskMedium, tool.Bounded, tool.ApprovalPolicyDefault},
		"git_switch": {tool.EffectProcessMutating, tool.RiskMedium, tool.Bounded, tool.ApprovalPolicyDefault},
		"git_fetch":  {tool.EffectNetworkRead, tool.RiskMedium, tool.Bounded, tool.ApprovalPolicyDefault},
		"git_pull":   {tool.EffectNetworkMutating, tool.RiskHigh, tool.Bounded, tool.ApprovalPolicyOnce},
		"git_push":   {tool.EffectExternalMutation, tool.RiskHigh, tool.Irreversible, tool.ApprovalPolicyOnce},
		"git_amend":  {tool.EffectProcessMutating, tool.RiskHigh, tool.Bounded, tool.ApprovalPolicyOnce},
		"git_merge":  {tool.EffectProcessMutating, tool.RiskHigh, tool.Bounded, tool.ApprovalPolicyOnce},
		"git_rebase": {tool.EffectProcessMutating, tool.RiskHigh, tool.Bounded, tool.ApprovalPolicyOnce},
		"git_cherry_pick": {
			tool.EffectProcessMutating, tool.RiskHigh, tool.Bounded, tool.ApprovalPolicyOnce,
		},
		"git_restore":  {tool.EffectProcessMutating, tool.RiskHigh, tool.Bounded, tool.ApprovalPolicyOnce},
		"git_stash":    {tool.EffectProcessMutating, tool.RiskHigh, tool.Bounded, tool.ApprovalPolicyOnce},
		"git_tag":      {tool.EffectProcessMutating, tool.RiskMedium, tool.Bounded, tool.ApprovalPolicyDefault},
		"git_conflict": {tool.EffectProcessMutating, tool.RiskHigh, tool.Bounded, tool.ApprovalPolicyOnce},
	}
	for name, want := range tests {
		instance := &mutationTool{kind: name}
		binding := instance.TrustedBinding()
		if binding.Effect.Kind != want.kind ||
			binding.Effect.Risk != want.risk ||
			binding.Effect.Reversibility != want.reversibility ||
			binding.Effect.Approval != want.approval {
			t.Fatalf("%s binding = %+v", name, binding)
		}
		if err := binding.Validate(); err != nil {
			t.Fatalf("%s binding: %v", name, err)
		}
	}
}

type profilePolicyBackend struct {
	gitTestBackend
	profile string
}

func (b profilePolicyBackend) Policy() sandbox.Policy {
	return sandbox.Policy{ID: "git-profile", EnvironmentProfile: b.profile}
}

type gitTestBackend struct{}

func (gitTestBackend) Capability() sandbox.Capability {
	return sandbox.Capability{
		Platform: "test", Backend: "passthrough", Available: true,
		Effective: controlmatrix.
			Matrix{
			FilesystemRead: controlmatrix.
				FilesystemReadDeclaredRoots,

			FilesystemWrite: controlmatrix.
				FilesystemWriteExactPaths,

			Network: controlmatrix.
				NetworkDenied, ProcessTree: controlmatrix.ProcessTreeGroupKill,
			CrossProcess: controlmatrix.CrossProcessUnrestricted, Syscall: controlmatrix.SyscallDenyDangerous, IPC: controlmatrix.
					IPCUnrestricted, PathIdentity: controlmatrix.
					PathIdentityDescriptorRelative,
			ArtifactOrigin: controlmatrix.
				ArtifactOriginUnverifiedPath,
			DurableRecovery: controlmatrix.DurableRecoveryMemoryOnly,
		},
	}
}

func (gitTestBackend) Prepare(_ context.Context, command sandbox.Command) (sandbox.Command, error) {
	command.PreparedReadOnly = command.WorkspaceReadOnly
	command.PreparedNetworkDenied = command.DenyNetwork
	return command, nil
}

type recordingGitBackend struct {
	gitTestBackend
	command sandbox.Command
}

func (b *recordingGitBackend) Prepare(
	ctx context.Context,
	command sandbox.Command,
) (sandbox.Command, error) {
	b.command = command
	return b.gitTestBackend.Prepare(ctx, command)
}

func runGit(t *testing.T, root string, arguments ...string) {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = root
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", arguments, err, output)
	}
}

func TestGitReadsLocateNestedRepositories(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	root := t.TempDir()
	repo := filepath.Join(root, "eds_metaserver")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "init")
	runGit(t, repo, "config", "user.email", "fixture@example.test")
	runGit(t, repo, "config", "user.name", "Fixture")
	if err := os.MkdirAll(filepath.Join(repo, "meta"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(repo, "meta", "fence_test.go"),
		[]byte("package meta\n"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "meta/fence_test.go")
	runGit(t, repo, "commit", "-m", "fixture")

	registry := tool.NewRegistry(nil, nil)
	if err := RegisterWithBackend(registry, root, gitTestBackend{}); err != nil {
		t.Fatal(err)
	}

	// A path inside the nested repository runs the command there.
	result, err := tooltest.Execute(t.Context(), registry, tool.Call{
		Name: "git_log",
		Arguments: json.RawMessage(
			`{"path":"eds_metaserver/meta/fence_test.go","limit":5}`,
		),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError || !strings.Contains(result.Content, "fixture") {
		t.Fatalf("nested git_log result = %+v", result)
	}

	// git_show rewrites the pathspec relative to the discovered root.
	result, err = tooltest.Execute(t.Context(), registry, tool.Call{
		Name: "git_show",
		Arguments: json.RawMessage(
			`{"revision":"HEAD","path":"eds_metaserver/meta/fence_test.go"}`,
		),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError || !strings.Contains(result.Content, "fence_test.go") {
		t.Fatalf("nested git_show result = %+v", result)
	}

	// Without a path the workspace root is not a repository; the failure
	// lists the nested repositories so the model can retry with a path.
	result, err = tooltest.Execute(t.Context(), registry, tool.Call{
		Name:      "git_status",
		Arguments: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError ||
		!strings.Contains(strings.ToLower(result.Content), "not a git repository") ||
		!strings.Contains(result.Content, "eds_metaserver") {
		t.Fatalf("root git_status result = %+v", result)
	}
}
