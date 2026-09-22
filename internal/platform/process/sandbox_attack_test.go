//go:build capability && darwin

package process

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func TestRealSandboxAttackCorpus(t *testing.T) {
	if os.Getenv("QCODE_SANDBOX_STAGE") != "1" {
		t.Skip("real sandbox attack corpus runs in the required sandbox stage")
	}
	root := t.TempDir()
	external := t.TempDir()
	secretValue := "fixture-secret-never-host-data"
	secret := filepath.Join(external, "secret")
	if err := os.WriteFile(filepath.Join(root, "workspace"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secret, []byte(secretValue), 0o600); err != nil {
		t.Fatal(err)
	}
	backend, err := sandbox.NewPlatformBackend(sandbox.Options{
		WorkspaceRoot: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sandbox.CloseBackend(backend)
	if err := sandbox.RequireControls(backend, sandbox.DefaultProcessRequirements()); err != nil {
		t.Fatal(err)
	}
	directory, err := sandbox.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	root = directory.Root()
	pinned, err := directory.OpenDirectory(".")
	if err != nil {
		t.Fatal(err)
	}
	defer pinned.Close()

	run := func(tb testing.TB, command string) Result {
		tb.Helper()
		result, runErr := Run(tb.Context(), Options{
			Command: command, Dir: root, DirFile: pinned,
			Sandbox: backend, RequireSandbox: true,
		})
		if runErr != nil {
			tb.Fatal(runErr)
		}
		if strings.Contains(result.Stdout+result.Stderr, secretValue) {
			tb.Fatalf("fixture secret leaked for %q", command)
		}
		return result
	}
	if result := run(t, `cat workspace; test "$(cat <<'EOF'
heredoc
EOF
)" = heredoc; printf written > created; sh -c 'cat workspace'`); result.ExitCode != 0 {
		t.Fatalf("workspace/child command failed: %+v", result)
	}
	attacks := []struct {
		name    string
		command string
	}{
		{"external-read", "cat " + shellQuote(secret)},
		{"external-write", "printf escaped > " + shellQuote(filepath.Join(external, "escaped"))},
		{"host-temp-write", `printf escaped > "/private/tmp/qcode-sandbox-attack-$$"`},
		{"host-var-temp-lexical-write", `printf escaped > "/var/tmp/qcode-sandbox-attack-$$"`},
		{"host-var-temp-write", `printf escaped > "/private/var/tmp/qcode-sandbox-attack-$$"`},
		{"symlink-read", "ln -s " + shellQuote(external) + " link && cat link/secret"},
		{"network", "/usr/bin/nc -w 1 127.0.0.1 9"},
		{"environment", `test -z "$QCODE_ATTACK_SECRET"`},
	}
	for _, attack := range attacks {
		t.Run(attack.name, func(t *testing.T) {
			t.Setenv("QCODE_ATTACK_SECRET", secretValue)
			result := run(t, attack.command)
			if attack.name == "symlink-read" {
				_ = os.Remove(filepath.Join(root, "link"))
			}
			if attack.name == "environment" {
				if result.ExitCode != 0 {
					t.Fatalf("secret environment was inherited: %+v", result)
				}
				return
			}
			if result.ExitCode == 0 {
				t.Fatalf("attack unexpectedly succeeded: %+v", result)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(external, "escaped")); !os.IsNotExist(err) {
		t.Fatalf("outside write created a file: %v", err)
	}
	hardlink := filepath.Join(root, "hardlink-secret")
	if err := os.Link(secret, hardlink); err == nil {
		if _, err := NewCommand(t.Context(), Options{
			Command: "cat hardlink-secret", Dir: root, DirFile: pinned,
			Sandbox: backend, RequireSandbox: true,
		}); err == nil {
			t.Fatal("hard-linked external fixture was accepted")
		}
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
