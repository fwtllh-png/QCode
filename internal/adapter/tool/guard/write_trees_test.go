package guard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	toolresult "github.com/fwtllh-png/QCode/internal/adapter/tool/result"
	"github.com/fwtllh-png/QCode/internal/security/policy"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func TestWriteTreeEnumerationDeduplicatesOverlappingGrants(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tree := filepath.Join(root, "generated")
	if err := os.Mkdir(tree, 0o700); err != nil {
		t.Fatal(err)
	}
	count := sandbox.MaxExactWorkspaceWritePaths + 1
	for i := range count {
		if err := os.WriteFile(filepath.Join(tree, fmt.Sprintf("%04d.txt", i)), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	g := &Guard{workspace: root}
	invocation := Invocation{Resources: []tool.Resource{
		{Kind: "directory", Path: tree, Access: tool.AccessWrite, Tree: true},
		{Kind: "file", Path: filepath.Join(tree, "0000.txt"), Access: tool.AccessWrite},
	}}
	paths, err := g.settleWritePaths(t.Context(), invocation)
	if err != nil || len(paths) != count {
		t.Fatalf("paths=%d error=%v", len(paths), err)
	}
	// An unchanged tree must not count known files again against a shrinking
	// allowance when observing newly-created files after the command.
	if err := g.observeWriteTreeCreations(t.Context(), invocation, paths, &tool.Result{}, true); err != nil {
		t.Fatal(err)
	}
}

func TestWriteTreeDisappearsDuringApprovalReturnsRecoverableRejection(t *testing.T) {
	root := t.TempDir()
	tree := filepath.Join(root, "generated")
	if err := os.Mkdir(tree, 0o700); err != nil {
		t.Fatal(err)
	}
	descriptor := readDescriptor("tree_command")
	descriptor.Capability = tool.CapabilityProcess
	descriptor.ResourceResolver = tool.ResourceResolver{PathsField: "write_paths"}
	descriptor.InputSchema = map[string]any{
		"type": "object", "properties": map[string]any{
			"write_paths": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		}, "required": []string{"write_paths"},
	}
	executor := &testExecutor{descriptor: descriptor}
	registry := newTestRegistry(t, nil, executor)
	requests := make(chan ApprovalRequest, 1)
	runtime := policy.DefaultRuntime(policy.ModeAct, policy.PermissionSuggest)
	runtime.DisableAutoReview = true
	g, err := New(Options{Workspace: root, Registry: registry, Policy: runtime,
		Approvals: func(_ context.Context, request ApprovalRequest) error { requests <- request; return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	call := provider.ToolCall{ID: "tree-race", Name: descriptor.Name, Arguments: `{"write_paths":["generated"]}`}
	type outcome struct {
		result tool.Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		r, e := g.Execute(t.Context(), call.ID, call.Name, json.RawMessage(call.Arguments))
		done <- outcome{r, e}
	}()
	var request ApprovalRequest
	select {
	case request = <-requests:
	case out := <-done:
		t.Fatalf("ended before approval: %v", out.err)
	}
	if err := os.Remove(tree); err != nil {
		t.Fatal(err)
	}
	mustDecide(t, g, request, policy.ApprovalOnce, nil)
	out := <-done
	if !errors.Is(out.err, tool.ErrPrecondition) || executor.calls.Load() != 0 {
		t.Fatalf("error=%v calls=%d", out.err, executor.calls.Load())
	}
	recovered, ok := toolresult.RecoverResult(registry, call, out.result, out.err)
	if !ok || !recovered.IsError || recovered.Metadata["required_action"] != "check_write_paths" || recovered.Metadata["retry_original"] != false {
		t.Fatalf("recovery=%+v ok=%v", recovered, ok)
	}
	if out.result.Execution == nil || out.result.Execution.TerminalOwner != tool.TerminalOwnerGuard {
		t.Fatalf("receipt=%+v", out.result.Execution)
	}
}
