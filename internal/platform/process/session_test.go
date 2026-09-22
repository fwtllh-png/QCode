//go:build darwin

package process

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

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
