//go:build darwin

package process

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"testing"
	"time"
)

func TestSessionCompletionReapsBackgroundGroup(t *testing.T) {
	testSessionCompletionReapsBackgroundGroup(t, t.Context(), SessionOptions{Dir: t.TempDir()})
}

func testSessionCompletionReapsBackgroundGroup(t *testing.T, ctx context.Context, options SessionOptions) {
	t.Helper()
	for _, tty := range []bool{false, true} {
		for _, outputOpen := range []bool{false, true} {
			t.Run(fmt.Sprintf("tty=%t/output_open=%t", tty, outputOpen), func(t *testing.T) {
				manager := NewSessionManager(4096)
				t.Cleanup(manager.CloseAll)
				redirect := " >/dev/null 2>&1"
				if outputOpen {
					redirect = ""
				}
				current := options
				current.Command = "sleep 30" + redirect + ` & printf "child:%s\n" "$!"; read line; exit 7`
				current.ThreadID, current.TurnID = testSessionThread, "completed-turn"
				current.PTY, current.DetachFromCaller = tty, true
				id, err := manager.Create(ctx, current)
				if err != nil {
					t.Fatal(err)
				}
				read := waitForOutput(t, manager, id, 0, "child:")
				child := parseChildPID(t, read.Data)
				t.Cleanup(func() { _ = syscall.Kill(child, syscall.SIGKILL) })
				if err := manager.Write(id, testSessionThread, []byte("finish\n")); err != nil {
					t.Fatal(err)
				}
				deadline := time.Now().Add(2 * time.Second)
				for time.Now().Before(deadline) {
					read, err = manager.Read(id, testSessionThread, 0)
					if err != nil {
						t.Fatal(err)
					}
					if !read.Running {
						break
					}
					time.Sleep(time.Millisecond)
				}
				if read.Running || read.ExitCode != 7 {
					t.Fatalf("parent completion=%+v", read)
				}
				deadline = time.Now().Add(2 * time.Second)
				for time.Now().Before(deadline) && syscall.Kill(child, 0) == nil {
					time.Sleep(time.Millisecond)
				}
				if err := syscall.Kill(child, 0); !errors.Is(err, syscall.ESRCH) {
					t.Fatalf("background child survived completed session: pid=%d err=%v", child, err)
				}
				if err := manager.Close(id, testSessionThread); err != nil {
					t.Fatal(err)
				}
				if count, err := manager.CloseByTurn("completed-turn"); err != nil || count != 0 {
					t.Fatalf("completed turn cleanup=%d err=%v", count, err)
				}
			})
		}
	}
}
