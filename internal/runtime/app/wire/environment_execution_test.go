package wire

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	toolguard "github.com/fwtllh-png/QCode/internal/adapter/tool/guard"
	"github.com/fwtllh-png/QCode/internal/config"
	"github.com/fwtllh-png/QCode/internal/environment"
	"github.com/fwtllh-png/QCode/internal/runtime/app"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

// Use the production thread factory and Guard; no provider call is needed to
// exercise Config -> wire -> Guard -> Shell -> process -> platform sandbox.
func environmentThreadGuard(t *testing.T, session *Session, thread string) *toolguard.Guard {
	t.Helper()
	if _, err := session.threads.History(protocol.ThreadID(thread)); err != nil {
		t.Fatal(err)
	}
	engine, err := session.threads.ContextEngine(thread)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := engine.OptionsSeed().GuardFactory(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return guard
}

func executeEnvironmentTool(t *testing.T, ctx context.Context, guard *toolguard.Guard, thread, id, name string, args map[string]any) tool.Result {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	ctx = tool.WithInvocationIdentity(ctx, tool.InvocationIdentity{SessionID: "environment-test", ThreadID: thread, TurnID: "environment-turn"})
	result, err := guard.Execute(ctx, id, name, raw)
	if err != nil {
		t.Fatalf("%s: %v; %+v", name, err, result)
	}
	return result
}

func TestConfiguredUnknownToolUsesGuardedEnvironmentAcrossExecutionForms(t *testing.T) {
	workspace, home, configDir := canonicalEnvironmentTestDir(t), canonicalEnvironmentTestDir(t), canonicalEnvironmentTestDir(t)
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", canonicalEnvironmentTestDir(t))
	t.Setenv("LANG", "POSIX")
	t.Setenv("LC_MESSAGES", "C")
	t.Setenv("GOPROXY", "https://fixture:fixture@unused.invalid")
	configFile, unbound := filepath.Join(configDir, "declared.conf"), filepath.Join(configDir, "unbound.conf")
	for _, path := range []string{configFile, unbound, filepath.Join(home, ".netrc")} {
		if err := os.WriteFile(path, []byte("configured"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	const program = `#!/bin/sh
set -eu
if [ "${1:-}" = wait ]; then read -r value; test "$value" = continue; fi
cat "$FIXTURE_CONFIG" > "$FIXTURE_CACHE/result"
if cat "$UNBOUND_FILE" >/dev/null 2>&1; then exit 21; fi
if cat "$HOST_PRIVATE_FILE" >/dev/null 2>&1; then exit 23; fi
if printf bad > forbidden-write 2>/dev/null; then exit 24; fi
if [ -n "${SIBLING_FILE:-}" ] && cat "$SIBLING_FILE" >/dev/null 2>&1; then exit 25; fi
test -z "${GOPROXY:-}"
cat "$FIXTURE_CACHE/result"
printf '|%s|%s|%s|%s|%s\n' "$FIXTURE_MODE" "$LANG" "$LC_MESSAGES" "$HOME" "$FIXTURE_CACHE"
if [ "${1:-}" = fail ]; then printf 'ordinary fixture failure\n' >&2; exit 37; fi
`
	writeProgram := func(root string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, "unrecognized-builder"), []byte(program), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeProgram(workspace)
	configPath := filepath.Join(canonicalEnvironmentTestDir(t), "config.toml")
	body := fmt.Sprintf(`
[execution.environment]
profile = "native"
[[execution.environment.resources]]
name = "configuration"
namespace = "host_config"
access = "read"
path = %q
env = "FIXTURE_CONFIG"
[[execution.environment.resources]]
name = "cache"
namespace = "cache"
access = "write"
path = "sandbox-home/cache/fixture"
env = "FIXTURE_CACHE"
tree = true
[[execution.environment.resources]]
name = "mode"
namespace = "host_config"
access = "read"
env = "FIXTURE_MODE"
value = "trusted"
[[execution.environment.resources]]
name = "declared-locale"
namespace = "host_config"
access = "read"
env = "LANG"
value = "C"
[[execution.environment.resources]]
name = "unavailable-auth"
namespace = "credential"
access = "use"
host = "unused.invalid"
required = true
`, configFile)
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	enabled := true
	session, err := NewExec(t.Context(), withNonDurableTestJournal(t, ExecOptions{
		ConfigPath: configPath, FixturePath: subagentFixture(t, "openai"), Permission: "auto",
		ConfigOverrides: config.Overrides{Workspace: &workspace, Tools: &enabled},
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := session.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	if err := sandbox.RequireControls(session.sandbox, sandbox.DefaultProcessRequirements()); err != nil {
		t.Fatal(err)
	}
	// Both main and subsequently constructed children must keep this snapshot.
	t.Setenv("LC_MESSAGES", "changed-after-preparation")
	t.Setenv("GOPROXY", "changed-after-preparation")
	facts := []environment.Fact{{Source: environment.SourcePreparer, Category: environment.CategoryEnvironmentResourceUnavailable,
		Resource: "unavailable-auth", RequiredAction: environment.ActionBindCredential, Detail: "credential_binder_unavailable"}}
	checkFacts := func(t *testing.T, result tool.Result) {
		t.Helper()
		got, _ := result.Metadata["environment_preparation_facts"].([]environment.Fact)
		if !slices.Equal(got, facts) {
			t.Fatalf("facts = %+v, want %+v", got, facts)
		}
	}
	env := map[string]string{"FIXTURE_MODE": "command", "UNBOUND_FILE": unbound, "HOST_PRIVATE_FILE": filepath.Join(home, ".netrc")}
	run := func(t *testing.T, guard *toolguard.Guard, thread, form, suffix string) tool.Result {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		args := map[string]any{"command": "./unrecognized-builder" + suffix, "env": env, "network_targets": []any{}}
		if form != "foreground" {
			args["command"] = "./unrecognized-builder wait"
			args["tty"], args["yield_time_ms"] = form == "pty", 1
		}
		result := executeEnvironmentTool(t, ctx, guard, thread, form+suffix, "exec_command", args)
		checkFacts(t, result)
		content := result.Content
		if form != "foreground" {
			id, _ := result.Metadata["session_id"].(string)
			if id == "" || result.Metadata["running"] != true {
				t.Fatalf("expected running %s: %+v", form, result)
			}
			result = executeEnvironmentTool(t, ctx, guard, thread, form+"-input", "write_stdin", map[string]any{"session_id": id, "chars": "continue\n", "yield_time_ms": 1000})
			content += result.Content
			checkFacts(t, result)
			for poll := 0; result.Metadata["running"] == true; poll++ {
				if err := ctx.Err(); err != nil {
					t.Fatal(err)
				}
				result = executeEnvironmentTool(t, ctx, guard, thread, fmt.Sprintf("%s-poll-%d", form, poll), "write_stdin", map[string]any{"session_id": id, "yield_time_ms": 1000})
				content += result.Content
				checkFacts(t, result)
			}
		}
		result.Content = content
		return result
	}
	parentPolicy, _ := sandbox.BackendPolicy(session.sandbox)
	parentCache := environmentEntryValue(parentPolicy.EnvironmentValues, "FIXTURE_CACHE")
	guard := environmentThreadGuard(t, session, "main")
	for _, form := range []string{"foreground", "background", "pty"} {
		t.Run(form, func(t *testing.T) {
			result := run(t, guard, "main", form, "")
			want := "configured|command|C|C|" + home + "|" + parentCache
			if result.IsError || result.Metadata["exit_code"] != 0 || !strings.Contains(result.Content, want) {
				t.Fatalf("%s: %+v", form, result)
			}
		})
	}
	failed := run(t, guard, "main", "foreground", " fail")
	if !failed.IsError || failed.Metadata["exit_code"] != 37 || failed.Metadata["error_category"] != environment.CategoryUnknown ||
		!strings.Contains(failed.Content, "ordinary fixture failure") || failed.Metadata["required_action"] != nil {
		t.Fatalf("ordinary failure was reclassified: %+v", failed)
	}
	siblingFile := filepath.Join(parentCache, "result")
	for _, thread := range []string{"child", "sibling"} {
		root := canonicalEnvironmentTestDir(t)
		writeProgram(root)
		if err := session.threads.RegisterChild(protocol.ThreadID(thread), app.ChildSpec{Workspace: root, HostWorkspace: workspace, ReadOnly: true}); err != nil {
			t.Fatal(err)
		}
		childGuard := environmentThreadGuard(t, session, thread)
		childPolicy, _ := sandbox.BackendPolicy(session.childTools.built[root].backend)
		cache := environmentEntryValue(childPolicy.EnvironmentValues, "FIXTURE_CACHE")
		env["SIBLING_FILE"] = siblingFile
		result := run(t, childGuard, thread, "foreground", "")
		want := "configured|command|C|C|" + childPolicy.PrivateTemp + "|" + cache
		if result.IsError || result.Metadata["exit_code"] != 0 || !strings.Contains(result.Content, want) || cache == parentCache || filepath.Join(cache, "result") == siblingFile {
			t.Fatalf("child environment or isolation: %+v", result)
		}
		siblingFile = filepath.Join(cache, "result")
	}
	// An escaping workspace symlink is rejected before the process starts.
	if err := os.Symlink(unbound, filepath.Join(workspace, "escape-link")); err != nil {
		t.Fatal(err)
	}
	raw := json.RawMessage(`{"command":"cat escape-link"}`)
	if result, err := guard.Execute(t.Context(), "escape", "exec_command", raw); err == nil || !strings.Contains(err.Error(), "escapes the sandbox") {
		t.Fatalf("escaping link was not rejected: %+v %v", result, err)
	}

	environmentRoot := session.environmentStateDir
	if err := session.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(environmentRoot); !os.IsNotExist(err) {
		t.Fatalf("temporary environment survived close: %v", err)
	}

}

func canonicalEnvironmentTestDir(t *testing.T) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTemporaryEnvironmentRemovedOnConstructionFailure(t *testing.T) {
	workspace := canonicalEnvironmentTestDir(t)
	t.Setenv("HOME", canonicalEnvironmentTestDir(t))
	t.Setenv("XDG_CONFIG_HOME", canonicalEnvironmentTestDir(t))
	injected := errors.New("fixture construction failure")
	var environmentRoot string
	modules := append(defaultBuildModules(), buildModuleFunc{name: "fail-after-environment", fn: func(_ context.Context, state *buildState) error {
		environmentRoot = state.session.environmentStateDir
		return injected
	}})
	enabled := true
	_, err := newExec(t.Context(), withNonDurableTestJournal(t, ExecOptions{
		FixturePath: subagentFixture(t, "openai"), Permission: "auto",
		ConfigOverrides: config.Overrides{Workspace: &workspace, Tools: &enabled},
	}), modules)
	if !errors.Is(err, injected) || environmentRoot == "" {
		t.Fatalf("construction failure = %v, root=%q", err, environmentRoot)
	}
	if _, err := os.Stat(environmentRoot); !os.IsNotExist(err) {
		t.Fatalf("partial construction leaked environment: %v", err)
	}
}
