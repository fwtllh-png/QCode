package wire

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/config"
	"github.com/fwtllh-png/QCode/internal/runtime/app"
)

func TestRuntimeGitCommitUsesPreparedGlobalIdentity(t *testing.T) {
	workspace := newGitWorkspace(t)
	runChatGit(t, workspace, "config", "--unset", "user.name")
	runChatGit(t, workspace, "config", "--unset", "user.email")
	runChatGit(t, workspace, "config", "user.useConfigOnly", "true")
	configHome := canonicalEnvironmentTestDir(t)
	global := filepath.Join(configHome, ".gitconfig")
	if err := os.WriteFile(global, []byte("[user]\nname=Runtime Fixture\nemail=runtime@example.invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", configHome)
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	tools := true
	session, err := NewExec(t.Context(), withNonDurableTestJournal(t, ExecOptions{
		FixturePath: subagentFixture(t, "openai"), Permission: "auto",
		ConfigOverrides: config.Overrides{Workspace: &workspace, Tools: &tools},
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := session.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	// Runtime must keep the prepared source even if the launch environment changes.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("HOME", t.TempDir())
	if err := os.WriteFile(filepath.Join(workspace, "README.md"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runChatGit(t, workspace, "add", "README.md")
	state, err := session.workspaceQuery.GitOverview(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	result, err := session.Runtime.ExecuteGit(t.Context(), app.GitRequest{
		Action: "commit", Branch: state.Branch, Revision: state.Revision,
		Message: "prepared identity", Paths: []string{"README.md"},
	})
	if err != nil || result.Problem != nil || result.CommitHash == "" {
		t.Fatalf("runtime commit=%+v err=%v", result, err)
	}
	identity := strings.TrimSpace(runChatGit(t, workspace, "log", "-1", "--format=%an <%ae>%n%cn <%ce>"))
	if identity != "Runtime Fixture <runtime@example.invalid>\nRuntime Fixture <runtime@example.invalid>" {
		t.Fatalf("runtime used wrong identity: %q", identity)
	}
}
