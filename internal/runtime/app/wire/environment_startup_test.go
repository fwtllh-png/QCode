package wire

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/config"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func TestNewExecDoesNotProbeLanguageTools(t *testing.T) {
	for _, scenario := range []struct {
		name     string
		manifest string
		body     string
		stub     string
	}{
		{name: "empty-workspace-without-declared-language-tools"},
		{
			name:     "python-workspace-with-failing-language-tools",
			manifest: "pyproject.toml", body: "[project]\nname = \"fixture\"\n",
			stub: "exit 23\n",
		},
		{
			name:     "go-workspace-with-hanging-language-tools",
			manifest: "go.mod", body: "module fixture.example/app\n\ngo 1.99\n",
			stub: "exec /bin/sleep 60\n",
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			workspace := canonicalEnvironmentTestDir(t)
			home := canonicalEnvironmentTestDir(t)
			bin := canonicalEnvironmentTestDir(t)
			marker := filepath.Join(workspace, "probes")
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", canonicalEnvironmentTestDir(t))
			t.Setenv("PATH", bin+string(os.PathListSeparator)+"/usr/bin:/bin")
			t.Setenv("GOPROXY", "https://fixture:fixture@goproxy.example")
			t.Setenv("NETRC", filepath.Join(home, ".netrc"))
			if err := os.WriteFile(filepath.Join(home, ".netrc"), []byte(
				"machine goproxy.example login fixture password fixture\n",
			), 0o600); err != nil {
				t.Fatal(err)
			}
			if scenario.manifest != "" {
				if err := os.WriteFile(filepath.Join(workspace, scenario.manifest), []byte(scenario.body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if scenario.stub != "" {
				for _, name := range []string{"go", "node", "python3"} {
					body := fmt.Sprintf("#!/bin/sh\nif [ \"${1:-}\" = --qcode-explicit ]; then printf 'explicit-%%s\\n' \"$0\"; exit 23; fi\nprintf '%%s\\n' \"$0\" >> %q\n", marker) + scenario.stub
					if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o700); err != nil {
						t.Fatal(err)
					}
				}
			}
			tools := true
			session, err := NewExec(t.Context(), withNonDurableTestJournal(t, ExecOptions{
				FixturePath:     subagentFixture(t, "openai"),
				Permission:      "auto",
				ConfigOverrides: config.Overrides{Workspace: &workspace, Tools: &tools},
			}))
			if err != nil {
				t.Fatalf("initialize workspace: %v", err)
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := session.Close(ctx); err != nil {
					t.Errorf("close workspace: %v", err)
				}
			})
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("startup executed a language tool: marker stat = %v", err)
			}
			policy, ok := sandbox.BackendPolicy(session.sandbox)
			if !ok {
				t.Fatal("workspace has no sandbox policy")
			}
			for _, entry := range policy.EnvironmentValues {
				if strings.HasPrefix(entry, "GO") {
					t.Fatalf("startup prepared undeclared Go environment: %q", entry)
				}
			}
			if scenario.stub != "" {
				guard := environmentThreadGuard(t, session, "explicit-tools")
				for _, name := range []string{"go", "node", "python3"} {
					result := executeEnvironmentTool(t, t.Context(), guard, "explicit-tools", name, "exec_command", map[string]any{
						"command": name + " --qcode-explicit", "env": map[string]string{"PATH": bin + ":/usr/bin:/bin"},
					})
					if !result.IsError || result.Metadata["exit_code"] != 23 || !strings.Contains(result.Content, "explicit-"+filepath.Join(bin, name)) {
						t.Fatalf("explicit %s did not execute PATH fixture: %+v", name, result)
					}
				}
			}
		})
	}
}
