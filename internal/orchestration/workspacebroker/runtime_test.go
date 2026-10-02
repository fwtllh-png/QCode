package workspacebroker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/security/authority"
	"github.com/fwtllh-png/QCode/internal/security/filebroker"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

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
			broker, err := New(root, authority.NewLeaseAuthority(authority.LeaseAuthorityOptions{}), time.Minute)
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
