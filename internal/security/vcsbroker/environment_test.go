package vcsbroker

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/security/authority"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

type environmentBackend struct {
	sandbox.Backend
	policy sandbox.Policy
}

func (b environmentBackend) Policy() sandbox.Policy { return b.policy }

func TestBrokerRejectsUnpreparedOrUnsafeEnvironment(t *testing.T) {
	for _, policy := range []sandbox.Policy{
		{},
		{ID: "unsafe", EnvironmentValues: []string{"API_TOKEN=fixture"}},
		{ID: "preload", EnvironmentValues: []string{"BASH_ENV=/fixture"}},
	} {
		if _, err := New(t.TempDir(), authority.NewLeaseAuthority(authority.LeaseAuthorityOptions{}), time.Minute,
			environmentBackend{policy: policy}); err == nil {
			t.Fatal("unprepared or unsafe broker environment accepted")
		}
	}
}

func TestBrokerCommitUsesPreparedGitConfiguration(t *testing.T) {
	configHome := t.TempDir()
	global := filepath.Join(configHome, ".gitconfig")
	if err := os.WriteFile(global, []byte("[include]\npath = identity\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configHome, "identity"), []byte(
		"[user]\nname = Global Fixture\nemail = global@example.invalid\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		profile string
		env     []string
		local   bool
		want    string
	}{
		{"native-home", "native", []string{"HOME=" + configHome}, false, "Global Fixture <global@example.invalid>"},
		{"native-explicit-config", "native", []string{"GIT_CONFIG_GLOBAL=" + global}, false, "Global Fixture <global@example.invalid>"},
		{"local-precedence", "native", []string{"HOME=" + configHome}, true, "Fixture <fixture@example.invalid>"},
		{"explicitly-disabled", "native", []string{"HOME=" + configHome, "GIT_CONFIG_GLOBAL=" + os.DevNull}, false, ""},
		{"isolated", "isolated", []string{"HOME=" + configHome, "GIT_CONFIG_GLOBAL=" + global}, false, ""},
		{"empty-source", "native", []string{}, false, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository := gitRepository(t)
			if !test.local {
				runRepositoryGit(t, repository, "config", "--unset", "user.name")
				runRepositoryGit(t, repository, "config", "--unset", "user.email")
			}
			runRepositoryGit(t, repository, "config", "user.useConfigOnly", "true")
			values := append([]string{"GIT_CONFIG_SYSTEM=" + os.DevNull}, test.env...)
			broker, err := New(repository, authority.NewLeaseAuthority(authority.LeaseAuthorityOptions{}), time.Minute,
				environmentBackend{policy: sandbox.Policy{
					ID: "test", EnvironmentProfile: test.profile,
					EnvironmentValues: values, PrivateTemp: t.TempDir(),
				}})
			if err != nil {
				t.Fatal(err)
			}
			// Neither later host changes nor mutation of the caller's policy
			// slice may replace the environment captured by the broker.
			t.Setenv("HOME", t.TempDir())
			t.Setenv("GIT_CONFIG_GLOBAL", global)
			for i := range values {
				values[i] = "IGNORED=changed"
			}
			before := strings.TrimSpace(runRepositoryGit(t, repository, "rev-parse", "HEAD"))
			if err := os.WriteFile(filepath.Join(repository, "README.md"), []byte("changed\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := broker.Mutate(t.Context(), Mutation{Kind: IndexAdd, Dir: repository, Args: []string{"add", "-A", "--", "README.md"}}); err != nil {
				t.Fatal(err)
			}
			result, err := broker.Mutate(t.Context(), Mutation{Kind: Commit, Dir: repository, Args: []string{"commit", "--no-gpg-sign", "-m", "global identity"}})
			if test.want == "" {
				var exit *exec.ExitError
				if !errors.As(err, &exit) || result.Process.ExitCode != 128 ||
					!strings.Contains(err.Error(), strings.TrimSpace(result.Process.Stderr)) ||
					!strings.Contains(err.Error(), "identity unknown") || result.Settlement.Status != "failed" {
					t.Fatalf("missing identity result=%+v err=%v", result, err)
				}
				if after := strings.TrimSpace(runRepositoryGit(t, repository, "rev-parse", "HEAD")); after != before {
					t.Fatal("failed commit changed HEAD")
				}
				return
			}
			if err != nil || result.Process.ExitCode != 0 || result.Settlement.Status != "succeeded" {
				t.Fatalf("commit result=%+v err=%v", result, err)
			}
			identity, err := broker.Read(t.Context(), repository, "log", "-1", "--format=%an <%ae>%n%cn <%ce>")
			if err != nil || strings.TrimSpace(identity) != test.want+"\n"+test.want {
				t.Fatalf("committed identity=%q err=%v", identity, err)
			}
			query, err := broker.ReadResult(t.Context(), repository, "var", "GIT_COMMITTER_IDENT")
			if err != nil || query.ExitCode != 0 || !strings.HasPrefix(query.Stdout, test.want+" ") {
				t.Fatalf("read environment differs from commit: %+v err=%v", query, err)
			}
			// An unchanged index reports on stdout. Preserve that diagnostic too.
			result, err = broker.Mutate(t.Context(), Mutation{Kind: Commit, Dir: repository, Args: []string{"commit", "--no-gpg-sign", "-m", "no changes"}})
			if err == nil || result.Process.ExitCode != 1 || strings.TrimSpace(result.Process.Stdout) == "" ||
				!strings.Contains(err.Error(), strings.TrimSpace(result.Process.Stdout)) {
				t.Fatalf("empty commit diagnostic lost: %+v err=%v", result, err)
			}
		})
	}
}
