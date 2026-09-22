//go:build darwin

package process

import (
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/persist/joblog"
)

const archiveTestThread = "thread-archive-test"

// A poller that falls behind a job's bounded buffer used to be told its cursor
// expired, and the bytes were gone. With an archive the same cursor still reads.
func TestAPollerBehindTheBufferStillReadsWhatItMissed(t *testing.T) {
	archive, err := joblog.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = archive.Close() })
	// A buffer far smaller than the output guarantees the ring drops its beginning.
	manager := NewSessionManager(64)
	manager.SetArchive(archive)
	id, err := manager.Create(t.Context(), SessionOptions{
		Command: `for index in $(seq 1 200); do printf "line-$index\n"; done`,
		Dir:     t.TempDir(), PTY: true, ThreadID: archiveTestThread,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close(id, archiveTestThread) })

	// Read from the very beginning once the live buffer has moved past it.
	read := waitForArchivedRead(t, manager, id)
	// A pty ends lines with CRLF, so the marker is the line plus its return.
	if !strings.Contains(read.Data, "line-1\r") {
		t.Fatalf("archived read lost the beginning: %q", read.Data)
	}
	// Reading the whole job by following the cursor must reconstruct every line.
	var whole strings.Builder
	whole.WriteString(read.Data)
	cursor := read.Cursor
	for range 100 {
		next, err := manager.Read(id, archiveTestThread, cursor)
		if err != nil {
			t.Fatal(err)
		}
		if next.Data == "" {
			if next.Running {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			break
		}
		whole.WriteString(next.Data)
		cursor = next.Cursor
	}
	for _, index := range []string{"line-1\r", "line-100\r", "line-200\r"} {
		if !strings.Contains(whole.String(), index) {
			t.Fatalf("replayed output is missing %q", index)
		}
	}
}

// Without an archive the behaviour is unchanged: a cursor the buffer has passed
// is an error, because the bytes really are gone.
func TestAnExpiredCursorIsStillAnErrorWithoutAnArchive(t *testing.T) {
	manager := NewSessionManager(64)
	id, err := manager.Create(t.Context(), SessionOptions{
		Command: `for index in $(seq 1 200); do printf "line-$index\n"; done`,
		Dir:     t.TempDir(), ThreadID: archiveTestThread,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close(id, archiveTestThread) })
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := manager.Read(id, archiveTestThread, 0); err != nil {
			if !strings.Contains(err.Error(), "expired") {
				t.Fatalf("read error = %v, want an expired cursor", err)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the buffer never lapped, so the case under test never happened")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A caller that is up to date must not be told it is reading the archive, and a
// cursor beyond what the job produced is still a caller bug.
func TestAnUpToDateCursorReadsTheLiveBuffer(t *testing.T) {
	archive, err := joblog.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = archive.Close() })
	manager := NewSessionManager(4096)
	manager.SetArchive(archive)
	id, err := manager.Create(t.Context(), SessionOptions{
		Command: "printf done", Dir: t.TempDir(), ThreadID: archiveTestThread,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close(id, archiveTestThread) })
	read := waitForOutput(t, manager, id, 0, "done")
	if read.Archived {
		t.Fatalf("read = %+v, want the live buffer", read)
	}
	if _, err := manager.Read(id, archiveTestThread, read.Cursor+1); err == nil {
		t.Fatal("a cursor past the end should be rejected")
	}
}

// The log outlives the process that wrote it, which is the point of putting it on
// disk: a later reader can still see what a job printed.
func TestTheJobLogIsReadableAfterTheManagerIsGone(t *testing.T) {
	directory := t.TempDir()
	archive, err := joblog.New(directory)
	if err != nil {
		t.Fatal(err)
	}
	manager := NewSessionManager(4096)
	manager.SetArchive(archive)
	id, err := manager.Create(t.Context(), SessionOptions{
		Command: "printf survivor", Dir: t.TempDir(), ThreadID: archiveTestThread,
	})
	if err != nil {
		t.Fatal(err)
	}
	waitForOutput(t, manager, id, 0, "survivor")
	manager.CloseAll()
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := joblog.New(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	data, total, err := reopened.Range(id, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "survivor") || total == 0 {
		t.Fatalf("archived output = %q total=%d", data, total)
	}
}

// waitForArchivedRead waits until a read from the start of the stream has to come
// from the archive, which is the condition the feature exists for.
func waitForArchivedRead(t *testing.T, manager *SessionManager, id string) SessionRead {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		read, err := manager.Read(id, manager.OwnerThread(id), 0)
		if err != nil {
			t.Fatalf("read from the start of the stream: %v", err)
		}
		if read.Archived {
			return read
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the buffer never lapped, so the case under test never happened")
	return SessionRead{}
}
