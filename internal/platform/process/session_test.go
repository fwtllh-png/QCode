//go:build darwin

package process

import (
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"syscall"
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

func TestCreateAppliesSessionProxyPortAndClosesNetwork(t *testing.T) {
	manager := NewSessionManager(4096)
	t.Cleanup(manager.CloseAll)
	root := t.TempDir()
	backend := &recordingBackend{root: root, proxyPort: 43128}
	closer := &closeRecorder{}
	id, err := manager.Create(t.Context(), SessionOptions{
		Command: "true", Dir: root, ThreadID: testSessionThread,
		Sandbox: backend, SessionProxyPort: 43129, Network: closer,
	})
	if err != nil {
		t.Fatal(err)
	}
	if backend.command.SessionProxyPort != 43129 {
		t.Fatalf("session port binding = %+v", backend.command)
	}
	if environmentValue(backend.command.Env, "HTTPS_PROXY") != "http://127.0.0.1:43129" {
		t.Fatalf("session proxy environment = %v", backend.command.Env)
	}
	if closer.closed {
		t.Fatal("network closed before session teardown")
	}
	if err := manager.Close(id, testSessionThread); err != nil {
		t.Fatal(err)
	}
	if !closer.closed {
		t.Fatal("session close did not recycle the network channel")
	}
}

func TestCreateRollbackClosesUnregisteredNetwork(t *testing.T) {
	manager := NewSessionManager(4096)
	t.Cleanup(manager.CloseAll)
	closer := &closeRecorder{}
	_, err := manager.Create(t.Context(), SessionOptions{
		Command: "", Dir: "", ThreadID: testSessionThread,
		SessionProxyPort: 43129, Network: closer,
	})
	if err == nil {
		t.Fatal("Create() succeeded with an empty directory")
	}
	if !closer.closed {
		t.Fatal("failed Create left the network channel open")
	}
}

type closeRecorder struct{ closed bool }

func (c *closeRecorder) Close() error {
	c.closed = true
	return nil
}

var _ io.Closer = (*closeRecorder)(nil)

const testSessionThread = "thread-process-test"

func TestSessionRequiresOwnerThread(t *testing.T) {
	manager := NewSessionManager(4096)
	_, err := manager.Create(t.Context(), SessionOptions{
		Command: "printf denied",
		Dir:     t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), "owner thread is required") {
		t.Fatalf("Create() error = %v", err)
	}
}

func TestSessionLifecycleIncrementalReadResizeSignalAndClose(t *testing.T) {
	manager := NewSessionManager(4096)
	defer manager.CloseAll()
	id, err := manager.Create(t.Context(), SessionOptions{
		Command: `trap 'printf "resized\n"' WINCH; while IFS= read line; do printf "got:%s\n" "$line"; done`,
		Dir:     t.TempDir(), Rows: 24, Cols: 80, PTY: true,
		ThreadID: testSessionThread,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Write(id, testSessionThread, []byte("first\n")); err != nil {
		t.Fatal(err)
	}
	first := waitForOutput(t, manager, id, 0, "got:first")
	if err := manager.Resize(id, testSessionThread, 40, 120); err != nil {
		t.Fatal(err)
	}
	if err := manager.Signal(id, testSessionThread, syscall.SIGWINCH); err != nil {
		t.Fatal(err)
	}
	second := waitForOutput(t, manager, id, first.Cursor, "resized")
	if strings.Contains(second.Data, "got:first") || second.Cursor <= first.Cursor {
		t.Fatalf("incremental read = %+v after %+v", second, first)
	}
	if err := manager.Close(id, testSessionThread); err != nil {
		t.Fatal(err)
	}
	if manager.Count() != 0 {
		t.Fatalf("session count = %d", manager.Count())
	}
}

func TestSessionCancellationKillsProcessGroup(t *testing.T) {
	manager := NewSessionManager(4096)
	ctx, cancel := context.WithCancel(t.Context())
	id, err := manager.Create(ctx, SessionOptions{
		Command: `sleep 30 & printf "child:%s\n" "$!"; wait`, Dir: t.TempDir(),
		ThreadID: testSessionThread,
	})
	if err != nil {
		t.Fatal(err)
	}
	read := waitForOutput(t, manager, id, 0, "child:")
	childPID := parseChildPID(t, read.Data)
	cancel()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if manager.Count() == 0 && errors.Is(syscall.Kill(childPID, 0), syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("session/process remained after cancellation: count=%d pid=%d", manager.Count(), childPID)
}

func TestSessionDetachSurvivesCallerCancelAndCloseByThread(t *testing.T) {
	manager := NewSessionManager(4096)
	defer manager.CloseAll()
	ctx, cancel := context.WithCancel(t.Context())
	id, err := manager.Create(ctx, SessionOptions{
		Command: `while true; do sleep 1; done`, Dir: t.TempDir(),
		ThreadID: "thread-a", TurnID: "turn-1", CallID: "call-1",
		DetachFromCaller: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	time.Sleep(50 * time.Millisecond)
	if manager.Count() != 1 {
		t.Fatalf("detached session killed by caller cancel: count=%d", manager.Count())
	}
	if got := manager.OwnerThread(id); got != "thread-a" {
		t.Fatalf("OwnerThread = %q", got)
	}
	other, err := manager.Create(t.Context(), SessionOptions{
		Command: `while true; do sleep 1; done`, Dir: t.TempDir(),
		ThreadID: "thread-b", DetachFromCaller: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := manager.CloseByThread("thread-a"); err != nil || n != 1 {
		t.Fatalf("CloseByThread = %d, want 1", n)
	}
	if manager.OwnerThread(id) != "" {
		t.Fatal("thread-a session should be gone")
	}
	if manager.Count() != 1 || manager.OwnerThread(other) != "thread-b" {
		t.Fatalf("thread-b lease should remain: count=%d owner=%q", manager.Count(), manager.OwnerThread(other))
	}
}

func TestCloseByTurnPreservesConcurrentTurnInSameThread(t *testing.T) {
	manager := NewSessionManager(4096)
	defer manager.CloseAll()
	first, err := manager.Create(t.Context(), SessionOptions{
		Command: `while true; do sleep 1; done`, Dir: t.TempDir(),
		ThreadID: "thread-a", TurnID: "turn-1", CallID: "call-1",
		DetachFromCaller: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Create(t.Context(), SessionOptions{
		Command: `while true; do sleep 1; done`, Dir: t.TempDir(),
		ThreadID: "thread-a", TurnID: "turn-2", CallID: "call-2",
		DetachFromCaller: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := manager.CloseByTurn("turn-1"); err != nil || n != 1 {
		t.Fatalf("CloseByTurn = %d, want 1", n)
	}
	if manager.OwnerThread(first) != "" {
		t.Fatal("turn-1 session should be gone")
	}
	if manager.Count() != 1 || manager.OwnerThread(second) != "thread-a" {
		t.Fatalf(
			"turn-2 session should remain: count=%d owner=%q",
			manager.Count(),
			manager.OwnerThread(second),
		)
	}
}

func TestSessionOperationsRejectNonOwner(t *testing.T) {
	manager := NewSessionManager(4096)
	id, err := manager.Create(t.Context(), SessionOptions{
		Command: `while IFS= read line; do printf "%s\n" "$line"; done`,
		Dir:     t.TempDir(), ThreadID: "thread-owner", PTY: true,
		DetachFromCaller: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertDenied := func(name string, err error) {
		t.Helper()
		if !errors.Is(err, ErrSessionOwnership) {
			t.Fatalf("%s error = %v, want ownership denial", name, err)
		}
	}
	assertDenied("write", manager.Write(id, "thread-other", []byte("no\n")))
	_, err = manager.Read(id, "thread-other", 0)
	assertDenied("read", err)
	_, err = manager.Wait(t.Context(), id, "thread-other", 0, 0)
	assertDenied("wait", err)
	assertDenied("resize", manager.Resize(id, "thread-other", 24, 80))
	assertDenied("signal", manager.Signal(id, "thread-other", syscall.SIGINT))
	assertDenied("close", manager.Close(id, "thread-other"))
	if manager.Count() != 1 {
		t.Fatalf("denied close removed session: count=%d", manager.Count())
	}
	if err := manager.Close(id, "thread-owner"); err != nil {
		t.Fatal(err)
	}
}

func TestSessionWaitWakesOnOutputNotification(t *testing.T) {
	manager := NewSessionManager(4096)
	id, err := manager.Create(t.Context(), SessionOptions{
		Command: `sleep 0.05; printf notified`,
		Dir:     t.TempDir(), ThreadID: "thread-owner",
		DetachFromCaller: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	wait, err := manager.Wait(
		t.Context(),
		id,
		"thread-owner",
		0,
		2*time.Second,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(wait.Data, "notified") {
		t.Fatalf("wait = %+v", wait)
	}
	if err := manager.Close(id, "thread-owner"); err != nil {
		t.Fatal(err)
	}
}

func TestSessionRepeatedCreateDestroy(t *testing.T) {
	manager := NewSessionManager(1024)
	for range 50 {
		id, err := manager.Create(t.Context(), SessionOptions{
			Command: "printf done", Dir: t.TempDir(), ThreadID: testSessionThread,
		})
		if err != nil {
			t.Fatal(err)
		}
		waitForOutput(t, manager, id, 0, "done")
		if err := manager.Close(id, testSessionThread); err != nil {
			t.Fatal(err)
		}
	}
	if manager.Count() != 0 {
		t.Fatalf("session count = %d", manager.Count())
	}
}

func waitForOutput(
	t *testing.T,
	manager *SessionManager,
	id string,
	cursor uint64,
	contains string,
) SessionRead {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		read, err := manager.Read(id, manager.OwnerThread(id), cursor)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(read.Data, contains) {
			return read
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("terminal output did not contain %q", contains)
	return SessionRead{}
}

func parseChildPID(t *testing.T, output string) int {
	t.Helper()
	index := strings.Index(output, "child:")
	if index < 0 {
		t.Fatalf("child PID missing: %q", output)
	}
	value := output[index+len("child:"):]
	value = strings.TrimSpace(strings.SplitN(value, "\n", 2)[0])
	value = strings.TrimSuffix(value, "\r")
	pid, err := strconv.Atoi(value)
	if err != nil {
		t.Fatalf("child PID %q: %v", value, err)
	}
	return pid
}

type recordingCloser struct {
	closed chan struct{}
	once   sync.Once
}

func (r *recordingCloser) Close() error {
	r.once.Do(func() { close(r.closed) })
	return nil
}

func TestSessionNaturalExitClosesNetworkChannel(t *testing.T) {
	network := &recordingCloser{closed: make(chan struct{})}
	manager := NewSessionManager(4096)
	defer manager.CloseAll()
	id, err := manager.Create(t.Context(), SessionOptions{
		Command: "printf done", Dir: t.TempDir(),
		ThreadID: testSessionThread, Network: network,
	})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		read, err := manager.Read(id, testSessionThread, 0)
		if err == nil && !read.Running {
			select {
			case <-network.closed:
				return
			case <-time.After(time.Second):
				t.Fatal("network channel outlived the exited process")
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("session never exited")
}

func TestSessionInterruptIsNotTerminationButKillIs(t *testing.T) {
	manager := NewSessionManager(4096)
	defer manager.CloseAll()

	waitExited := func(id string, wantTerminated bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			read, err := manager.Read(id, testSessionThread, 0)
			if err != nil {
				t.Fatal(err)
			}
			if !read.Running {
				if read.Terminated != wantTerminated {
					t.Fatalf("terminated = %t, want %t", read.Terminated, wantTerminated)
				}
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("session never exited")
	}

	// An interrupt is ordinary signal delivery: the child may die from it,
	// but the session is not flagged as terminated-by-us.
	interrupted, err := manager.Create(t.Context(), SessionOptions{
		Command: "sleep 30", Dir: t.TempDir(), ThreadID: testSessionThread,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Signal(interrupted, testSessionThread, syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	waitExited(interrupted, false)

	// An uncatchable kill is a termination and is reported as one.
	killed, err := manager.Create(t.Context(), SessionOptions{
		Command: "sleep 30", Dir: t.TempDir(), ThreadID: testSessionThread,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Signal(killed, testSessionThread, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitExited(killed, true)
}

func TestPTYSessionFinalLineSurvivesExit(t *testing.T) {
	manager := NewSessionManager(4096)
	defer manager.CloseAll()
	id, err := manager.Create(t.Context(), SessionOptions{
		Command: "sleep 0.3; printf 'first-line\\n'; sleep 0.1; printf 'final-line\\n'",
		Dir:     t.TempDir(), PTY: true,
		ThreadID: testSessionThread,
	})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		read, err := manager.Read(id, testSessionThread, 0)
		if err != nil {
			t.Fatal(err)
		}
		if !read.Running {
			if !strings.Contains(read.Data, "final-line") {
				t.Fatalf("pty tail lost: %q", read.Data)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("session never exited")
}
