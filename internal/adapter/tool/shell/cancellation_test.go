//go:build darwin

package shell

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/platform/process"
	"github.com/fwtllh-png/QCode/internal/testutil/tooltest"
)

func TestCanceledExecCommandHasBoundedTeardownAndNoOrphan(t *testing.T) {
	const samples = 12
	root := t.TempDir()
	manager := process.NewSessionManager(4096)
	t.Cleanup(manager.CloseAll)
	registry := tool.NewRegistry(nil, nil)
	if err := RegisterWithManagerAndBackend(
		registry,
		root,
		manager,
		passthroughBackend{},
	); err != nil {
		t.Fatal(err)
	}
	latencies := make([]time.Duration, 0, samples)
	for sample := range samples {
		pidName := "cancel-" + strconv.Itoa(sample) + ".pid"
		pidPath := filepath.Join(root, pidName)
		if err := os.WriteFile(pidPath, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(map[string]any{
			"command":       "printf '%d' $$ > " + pidName + "; exec sleep 30",
			"yield_time_ms": 30_000,
			"write_paths":   []string{pidName},
			"output_tokens": 64,
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(
			tool.WithInvocationIdentity(
				t.Context(),
				tool.InvocationIdentity{
					ThreadID: "thread-cancel-test",
					CallID:   "call-cancel-" + strconv.Itoa(sample),
				},
			),
		)
		done := make(chan error, 1)
		go func() {
			_, executeErr := tooltest.Execute(ctx, registry, tool.Call{
				Name: "exec_command", Arguments: raw,
			})
			done <- executeErr
		}()
		pid := waitForProcessPID(t, pidPath)
		started := time.Now()
		cancel()
		select {
		case executeErr := <-done:
			if !errors.Is(executeErr, context.Canceled) {
				t.Fatalf("sample %d error = %v", sample, executeErr)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("sample %d cancellation exceeded two seconds", sample)
		}
		latencies = append(latencies, time.Since(started))
		if manager.Count() != 0 {
			t.Fatalf("sample %d retained %d sessions", sample, manager.Count())
		}
		waitForProcessExit(t, pid)
	}
	slices.Sort(latencies)
	p95 := latencies[(samples*95+99)/100-1]
	t.Logf("cancellation p95=%s samples=%d", p95, samples)
	if p95 >= 2*time.Second {
		t.Fatalf("cancellation p95 = %s", p95)
	}
}

func waitForProcessPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil && strings.TrimSpace(string(data)) != "" {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if parseErr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("process pid was not published to %s", path)
	return 0
}

func waitForProcessExit(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("process %d remained after cancellation", pid)
}
