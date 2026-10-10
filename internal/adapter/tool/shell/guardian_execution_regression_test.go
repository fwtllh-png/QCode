package shell

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	toolguard "github.com/fwtllh-png/QCode/internal/adapter/tool/guard"
	"github.com/fwtllh-png/QCode/internal/security/guardian"
)

func TestGuardianCannotExecutePATHShadowShell(t *testing.T) {
	for _, when := range []string{"before_review", "during_review"} {
		t.Run(when, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			bin := filepath.Join(root, ".tools")
			if err := os.Mkdir(bin, 0700); err != nil {
				t.Fatal(err)
			}
			shadow := filepath.Join(bin, "sh")
			writeShadow := func() {
				if err := os.WriteFile(shadow, []byte("#!/bin/sh\nprintf unreviewed > generated/out.txt\n"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if when == "before_review" {
				writeShadow()
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			_, g, _, ctx, _ := guardianShellRegistryAt(t, root)
			r := newPipelineReviewer(root)
			if when == "during_review" {
				r.reviewHook = func(guardian.ReviewCandidate) { writeShadow() }
			}
			g.SetApprovalHandler(func(context.Context, toolguard.ApprovalRequest) error {
				t.Error("literal write required approval")
				return context.Canceled
			})
			raw := json.RawMessage(`{"command":"printf reviewed > generated/out.txt","write_paths":["generated"],"yield_time_ms":30000}`)
			result, err := g.Execute(pipelineContext(ctx, r), "call", "exec_command", raw)
			if err != nil || result.IsError {
				t.Fatalf("execution failed: %v %s", err, result.Content)
			}
			body, err := os.ReadFile(filepath.Join(root, "generated/out.txt"))
			if err != nil || string(body) != "reviewed" {
				t.Fatalf("unreviewed interpreter ran: %q %v", body, err)
			}
			if r.reviews.Load() != 1 {
				t.Fatalf("reviews=%d", r.reviews.Load())
			}
		})
	}
}

func TestGuardianExactFileScopeExecutesWithoutWidening(t *testing.T) {
	for _, settlement := range []string{"apply", "discard"} {
		t.Run(settlement, func(t *testing.T) {
			root, g, ctx, _ := guardianShellFixture(t)
			if err := os.WriteFile(filepath.Join(root, "generated/keep.txt"), []byte("keep"), 0600); err != nil {
				t.Fatal(err)
			}
			r := newPipelineReviewer(root)
			r.reviewHook = func(c guardian.ReviewCandidate) {
				if len(c.Execution.WritePaths) != 1 || c.Execution.WritePaths[0] != "generated/out.txt" {
					t.Error("exact scope widened")
				}
			}
			g.SetApprovalHandler(func(context.Context, toolguard.ApprovalRequest) error {
				t.Error("exact scope fell back to approval")
				return context.Canceled
			})
			raw, _ := json.Marshal(map[string]any{"command": "printf reviewed > generated/out.txt", "write_paths": []string{"generated/out.txt"}, "settle": settlement, "yield_time_ms": 30000})
			result, err := g.Execute(pipelineContext(ctx, r), "call", "exec_command", raw)
			if err != nil || result.IsError {
				t.Fatalf("exact scope failed: %v %s", err, result.Content)
			}
			body, err := os.ReadFile(filepath.Join(root, "generated/out.txt"))
			if settlement == "apply" && (err != nil || string(body) != "reviewed") {
				t.Fatalf("exact file not settled: %q %v", body, err)
			}
			if settlement == "discard" && !os.IsNotExist(err) {
				t.Fatal("discard applied exact-file output")
			}
			body, err = os.ReadFile(filepath.Join(root, "generated/keep.txt"))
			if err != nil || string(body) != "keep" {
				t.Fatal("sibling changed")
			}
		})
	}
}

func TestGuardianRunningCopySurvivesUntilFinalPoll(t *testing.T) {
	root, g, ctx, _ := guardianShellFixture(t)
	r := newPipelineReviewer(root)
	var copyRoot string
	r.reviewHook = func(c guardian.ReviewCandidate) { copyRoot = c.Execution.Root }
	// A finite literal printf forces a running response with a 1ms yield;
	// coverage stays closed without adding an unreviewable sleep/loop.
	command := "printf '%3000000s' x > generated/out.txt; printf done > generated/out.txt; printf done > generated/done.txt"
	raw, _ := json.Marshal(map[string]any{"command": command, "write_paths": []string{"generated"}, "yield_time_ms": 1})
	result, err := g.Execute(pipelineContext(ctx, r), "call", "exec_command", raw)
	if err != nil || result.IsError {
		t.Fatalf("initial execution: %v %s", err, result.Content)
	}
	id, _ := result.Metadata["session_id"].(string)
	if id == "" {
		t.Fatal("fixture did not yield a running session")
	}
	if _, err := os.Stat(copyRoot); err != nil {
		t.Fatalf("running copy was reclaimed: %v", err)
	}
	poll, _ := json.Marshal(map[string]any{"session_id": id, "yield_time_ms": 30000})
	result, err = g.Execute(tool.WithInvocationIdentity(ctx, tool.InvocationIdentity{SessionID: "session", ThreadID: "thread", TurnID: "turn", CallID: "poll"}), "poll", "write_stdin", poll)
	if err != nil || result.IsError {
		t.Fatalf("final poll: %v %s", err, result.Content)
	}
	if strings.Contains(result.Content, "Process running") {
		t.Fatal("finite command did not finish")
	}
	if body, err := os.ReadFile(filepath.Join(root, "generated/done.txt")); err != nil || string(body) != "done" {
		t.Fatalf("background output not settled: %q %v", body, err)
	}
	if _, err := os.Stat(copyRoot); !os.IsNotExist(err) {
		t.Fatalf("finished copy not reclaimed: %v", err)
	}
}

func TestGuardianRunningCopyClosesWithoutApplyingAbandonedWrites(t *testing.T) {
	root, g, ctx, _ := guardianShellFixture(t)
	r := newPipelineReviewer(root)
	var copyRoot string
	r.reviewHook = func(c guardian.ReviewCandidate) { copyRoot = c.Execution.Root }
	raw := json.RawMessage(`{"command":"printf '%3000000s' x > generated/out.txt","write_paths":["generated"],"yield_time_ms":1}`)
	result, err := g.Execute(pipelineContext(ctx, r), "call", "exec_command", raw)
	if err != nil || result.IsError {
		t.Fatalf("start: %v %s", err, result.Content)
	}
	id, _ := result.Metadata["session_id"].(string)
	if id == "" {
		t.Fatal("fixture did not yield a running session")
	}
	if _, err := os.Stat(copyRoot); err != nil {
		t.Fatalf("running copy missing: %v", err)
	}
	closeArgs, _ := json.Marshal(map[string]any{"session_id": id, "close": true})
	result, err = g.Execute(tool.WithInvocationIdentity(ctx, tool.InvocationIdentity{SessionID: "session", ThreadID: "thread", TurnID: "turn", CallID: "close"}), "close", "write_stdin", closeArgs)
	if err != nil || result.IsError {
		t.Fatalf("close: %v %s", err, result.Content)
	}
	if _, err := os.Stat(copyRoot); !os.IsNotExist(err) {
		t.Fatalf("abandoned copy not reclaimed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "generated/out.txt")); !os.IsNotExist(err) {
		t.Fatalf("abandoned writes applied: %v", err)
	}
}
