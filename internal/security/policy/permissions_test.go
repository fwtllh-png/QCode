package policy

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
)

func TestAuthorityPathIsStableAndOutsideWorkspace(t *testing.T) {
	parent := t.TempDir()
	workspace := filepath.Join(parent, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(parent, "state")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	first, err := PermissionsPath(dataDir, workspace)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "workspace-link")
	if err := os.Symlink(workspace, link); err != nil {
		t.Fatal(err)
	}
	second, err := PermissionsPath(dataDir, link)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || strings.HasPrefix(first, workspace+string(filepath.Separator)) {
		t.Fatalf("authority paths first=%q second=%q workspace=%q", first, second, workspace)
	}
}

func TestAuthorityPathRejectsDataDirectoryInsideWorkspace(t *testing.T) {
	workspace := t.TempDir()
	dataDir := filepath.Join(workspace, ".qcode", "state")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := PermissionsPath(dataDir, workspace)
	if !errors.Is(err, ErrAuthorityInsideWorkspace) {
		t.Fatalf("Path() error = %v", err)
	}
}

func TestLoadMissingIsEmpty(t *testing.T) {
	bundle, err := LoadPermissions(filepath.Join(t.TempDir(), PermissionsFileName))
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Present || len(bundle.Rules) != 0 {
		t.Fatalf("bundle = %+v", bundle)
	}
}

func TestStoreExposesConfiguredPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), PermissionsFileName)
	store, err := OpenPermissionsStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if store.Path != path {
		t.Fatalf("store path = %q, want %q", store.Path, path)
	}
}

func TestLoadAndAppendAllowRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), PermissionsFileName)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	content := `
[[deny]]
tool = "run_command"
command_prefix = "rm"

[[ask]]
tool = "file_write"
resource = "secrets/"

[[allow]]
tool = "run_command"
grant_key = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, err := LoadPermissions(path)
	if err != nil {
		t.Fatal(err)
	}
	deny, ask, allow := bundle.Summary()
	if !bundle.Present || deny != 1 || ask != 1 || allow != 1 || len(bundle.Rules) != 3 {
		t.Fatalf("bundle = %+v summary=%d/%d/%d", bundle, deny, ask, allow)
	}
	if bundle.Rules[0].Action != ActionDeny || bundle.Rules[2].Action != ActionAllow {
		t.Fatalf("rules = %+v", bundle.Rules)
	}

	updated, err := AppendPermissionAllow(path, Rule{
		Tool: "file_write", GrantKey: strings.Repeat("b", 64), Action: ActionAllow,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, allow = updated.Summary()
	if allow != 2 {
		t.Fatalf("allow count = %d", allow)
	}
	reloaded, err := LoadPermissions(path)
	if err != nil {
		t.Fatal(err)
	}
	_, _, allow = reloaded.Summary()
	if allow != 2 {
		t.Fatalf("reloaded allow = %d", allow)
	}
	// idempotent
	again, err := AppendPermissionAllow(path, Rule{
		Tool: "file_write", GrantKey: strings.Repeat("b", 64), Action: ActionAllow,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, allow = again.Summary()
	if allow != 2 {
		t.Fatalf("dedupe allow = %d", allow)
	}
}

func TestLoadRejectsUnsafePersistentCommandPrefix(t *testing.T) {
	path := filepath.Join(t.TempDir(), PermissionsFileName)
	input := `
[[allow]]
tool = "run_command"
command_prefix = "git"
grant_key = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
`
	if err := os.WriteFile(path, []byte(input), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPermissions(path); err == nil {
		t.Fatal("unsafe persistent command prefix was accepted")
	}
}

func TestRuleFromInvocation(t *testing.T) {
	shell, err := RuleFromInvocation(resolveFixture(invocationFixture{
		Tool: "run_command", Arguments: json.RawMessage(`{"command":"go test ./..."}`),
		Capability: tool.CapabilityProcess, Access: tool.AccessRead,
		Sandbox: tool.SandboxStrong,
	}))
	if err != nil || len(shell.GrantKey) != 64 || shell.Action != ActionAllow {
		t.Fatalf("shell = %+v err=%v", shell, err)
	}
	file, err := RuleFromInvocation(resolveFixture(invocationFixture{
		Tool: "file_write", Arguments: json.RawMessage(`{"path":"a.txt"}`),
		Resources:  []tool.Resource{{Kind: "file", Path: "a.txt", Access: tool.AccessWrite}},
		Capability: tool.CapabilityWrite, Access: tool.AccessWrite,
		Sandbox: tool.SandboxNone, Journaled: true,
	}))
	if err != nil || len(file.GrantKey) != 64 {
		t.Fatalf("file = %+v err=%v", file, err)
	}
	host, err := RuleFromInvocation(resolveFixture(invocationFixture{
		Tool: "web_fetch", Arguments: json.RawMessage(`{"url":"https://example.com/a"}`),
		Resources:  []tool.Resource{{Kind: "host", ID: "example.com", Access: tool.AccessRead}},
		Capability: tool.CapabilityNetwork, Access: tool.AccessRead, Sandbox: tool.SandboxNone,
	}))
	if err != nil || len(host.GrantKey) != 64 {
		t.Fatalf("host = %+v err=%v", host, err)
	}
}

func TestStoreSerializesConcurrentAllows(t *testing.T) {
	store, err := OpenPermissionsStore(filepath.Join(t.TempDir(), PermissionsFileName))
	if err != nil {
		t.Fatal(err)
	}
	const count = 8
	var group sync.WaitGroup
	for index := range count {
		group.Add(1)
		go func() {
			defer group.Done()
			_, appendErr := store.AppendAllow(resolveFixture(invocationFixture{
				Tool:       "web_fetch",
				Capability: tool.CapabilityNetwork,
				Access:     tool.AccessRead, Sandbox: tool.SandboxNone,
				Resources: []tool.Resource{{
					Kind: "host", ID: fmt.Sprintf("host-%d.example", index),
					Access: tool.AccessRead,
				}},
			}))
			if appendErr != nil {
				t.Error(appendErr)
			}
		}()
	}
	group.Wait()
	if rules := store.Rules(); len(rules) != count {
		t.Fatalf("rules = %d, want %d", len(rules), count)
	}
}

func TestPersistedGrantMatchesOnlySameTypedInvocation(t *testing.T) {
	path := filepath.Join(t.TempDir(), PermissionsFileName)
	base := resolveFixture(invocationFixture{
		CallID: "one", Tool: "run_command", Capability: tool.CapabilityProcess,
		Access: tool.AccessRead, Sandbox: tool.SandboxStrong, Validated: true,
		Workspace: "/workspace",
		Arguments: json.RawMessage(`{"command":"go test ./...","cwd":"."}`),
		Resources: []tool.Resource{{
			Kind: "process", ID: "workspace", Access: tool.AccessRead, Tree: true,
		}, {
			Kind: "file", Path: "report.txt", Access: tool.AccessWrite,
		}},
	})
	rule, err := RuleFromInvocation(base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AppendPermissionAllow(path, rule); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadPermissions(path)
	if err != nil {
		t.Fatal(err)
	}
	runtime := DefaultRuntime(ModeAct, PermissionSuggest)
	runtime.User = reloaded.Rules
	if decision := runtime.Decide(base); decision.Action != ActionAllow {
		t.Fatalf("same invocation = %+v", decision)
	}
	changed := base
	changed.CallID = "two"
	changed.Arguments = json.RawMessage(`{"command":"go env","cwd":"."}`)
	if decision := runtime.Decide(changed); decision.Action != ActionAsk {
		t.Fatalf("changed invocation = %+v", decision)
	}
}
