package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	completiontool "github.com/fwtllh-png/QCode/internal/adapter/tool/completion"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type failedWriteWithChange struct {
	declarationWriteTool
	path string
}

func (f failedWriteWithChange) Execute(ctx context.Context, raw json.RawMessage) (tool.Result, error) {
	result, err := f.declarationWriteTool.Execute(ctx, raw)
	result.IsError = true
	result.Content = "command failed after a partial write"
	result.Outcome.Facts.WorkspaceChanges[0].Path = f.path
	return result, err
}

func TestFailedToolKeepsObservedChangeWithCanonicalPath(t *testing.T) {
	root := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry(nil, nil)
	if err := registry.Register(failedWriteWithChange{path: filepath.Join(root, "a.go")}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(&completiontool.Tool{}); err != nil {
		t.Fatal(err)
	}
	runtime := &scriptedProvider{streams: []provider.Stream{
		toolCallStream("failed-write", "write_fixture", `{}`),
		toolCallStream("complete", completiontool.Name,
			`{"status":"incomplete","summary":"partial write retained","pending_actions":["repair failed command"]}`),
	}}
	engine := declarationEngine(t, runtime, registry)
	engine.options.Workspace = alias
	result, err := engine.Run(t.Context(), "record a partial write", nil)
	var problem *protocol.Problem
	if !errors.As(err, &problem) || problem.Code != protocol.CodeConflict ||
		result.State != Failed || len(result.Tools) != 2 {
		t.Fatalf("partial write did not reach incomplete finalization: result=%+v err=%v", result, err)
	}
	changes := engine.TurnDiff()
	if len(changes) != 1 || changes[0].Path != "a.go" || changes[0].Kind != tool.WorkspaceModified {
		t.Fatalf("failed tool change was hidden or used an alias: %+v", changes)
	}
}
