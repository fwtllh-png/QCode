package workspacebroker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/security/authority"
	"github.com/fwtllh-png/QCode/internal/security/filebroker"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func TestContentBaselineUsesPrivateBrokerUnderCommandAuthority(t *testing.T) {
	parent, isolated := t.TempDir(), t.TempDir()
	broker, err := New(parent, authority.NewLeaseAuthority(authority.LeaseAuthorityOptions{}), time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{".gitignore": "ignored.txt\n", "ignored.txt": "dependency\n"} {
		if err := os.WriteFile(filepath.Join(isolated, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, err := sandbox.WithExecutionAuthority(t.Context(), sandbox.ExecutionAuthority{
		Digest: strings.Repeat("a", 64), Enforcement: sandbox.EnforcementStrong,
		WorkspaceRoot: parent, AllowProcess: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.InitContentBaseline(ctx, isolated); err != nil {
		t.Fatal(err)
	}
	result, err := broker.ReadVCSResult(ctx, isolated, "show", "HEAD:ignored.txt")
	if err != nil || result.ExitCode != 0 || result.Stdout != "dependency\n" {
		t.Fatalf("baseline=%+v error=%v", result, err)
	}
	if _, err := os.Lstat(filepath.Join(parent, ".git")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("baseline touched parent Git: %v", err)
	}
	if err := broker.InitContentBaseline(ctx, isolated); err == nil {
		t.Fatal("existing Git repository was reinitialized")
	}
	marker := t.TempDir()
	if err := os.WriteFile(filepath.Join(marker, ".git"), []byte("gitdir: "+filepath.Join(isolated, ".git")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := broker.InitContentBaseline(ctx, marker); err == nil {
		t.Fatal("shared worktree metadata was accepted as a private baseline")
	}
}

type recordingJournal struct {
	before []string
	after  []string
	err    error
}

func (j *recordingJournal) Before(_ context.Context, path string) error {
	j.before = append(j.before, path)
	return j.err
}

func (j *recordingJournal) After(path string) error {
	j.after = append(j.after, path)
	return nil
}

func TestCommitFilesUsesJournalPort(t *testing.T) {
	for _, reject := range []bool{false, true} {
		name := "commit"
		if reject {
			name = "journal_failure"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			workspace, err := sandbox.NewWorkspace(root)
			if err != nil {
				t.Fatal(err)
			}
			broker, err := New(root, authority.NewLeaseAuthority(authority.LeaseAuthorityOptions{}), time.Minute, nil)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := filebroker.PlanWrite(workspace, "note.txt", []byte("committed"), 0o600)
			if err != nil {
				t.Fatal(err)
			}
			journal := &recordingJournal{}
			if reject {
				journal.err = errors.New("journal unavailable")
			}
			_, err = broker.CommitFiles(t.Context(), "workspace-test", plan, journal)
			if !errors.Is(err, journal.err) {
				t.Fatalf("CommitFiles error = %v", err)
			}
			if len(journal.before) != 1 {
				t.Fatalf("journal before = %v", journal.before)
			}
			data, readErr := os.ReadFile(filepath.Join(root, "note.txt"))
			if reject {
				if !os.IsNotExist(readErr) || len(journal.after) != 0 {
					t.Fatalf("rejected journal still wrote file: %q, %v, %v", data, readErr, journal.after)
				}
				return
			}
			if readErr != nil || string(data) != "committed" || !slices.Equal(journal.before, journal.after) {
				t.Fatalf("commit data=%q err=%v journal=%+v", data, readErr, journal)
			}
		})
	}
}
