package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
)

func TestCurrentTurnSpecReturnsFrozenActiveSpec(t *testing.T) {
	engine := &Engine{}
	engine.publishScope(&Scope{
		engine: engine,
		spec: TurnSpec{
			Identity: TurnIdentity{TurnID: "turn-active"},
			Request: TurnRequest{
				Prompt: "inspect auth",
				Attachments: []provider.Attachment{{
					Name: "context.txt", Data: []byte("original"),
				}},
			},
		},
	})

	first := engine.CurrentTurnSpec()
	if first.Identity.TurnID != "turn-active" ||
		first.Request.Prompt != "inspect auth" ||
		len(first.Request.Attachments) != 1 {
		t.Fatalf("snapshot = %+v", first)
	}
	first.Request.Attachments[0].Name = "mutated.txt"

	second := engine.CurrentTurnSpec()
	if second.Request.Attachments[0].Name != "context.txt" {
		t.Fatalf("snapshot aliases engine state: %+v", second)
	}
}

func TestWorkspaceExcerptReadsBoundedPrefix(t *testing.T) {
	engine := newEngine(
		t, &scriptedProvider{streams: []provider.Stream{textStream("ok")}},
		tool.NewRegistry(nil, nil),
	)
	root := t.TempDir()
	engine.options.Workspace = root
	if err := os.MkdirAll(filepath.Join(root, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	content := strings.Repeat("line of code\n", 512)
	if err := os.WriteFile(
		filepath.Join(root, "pkg", "node.go"), []byte(content), 0o644,
	); err != nil {
		t.Fatal(err)
	}
	excerpt, ok := engine.WorkspaceExcerpt("pkg/node.go", 256)
	if !ok {
		t.Fatal("readable text file produced no excerpt")
	}
	if len(excerpt) > 256 || !strings.HasPrefix(excerpt, "line of code") {
		t.Fatalf("excerpt = %d bytes starting %q", len(excerpt), excerpt[:40])
	}
}

// spawn_agent forks parent context from inside a running tool call, while
// Execute still owns the parent turn. The excerpt read must not wait on it.
func TestWorkspaceExcerptDuringParentToolCall(t *testing.T) {
	registry := tool.NewRegistry(nil, nil)
	probe := &excerptProbeTool{}
	if err := registry.Register(probe); err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, &scriptedProvider{streams: []provider.Stream{
		toolCallStream("call-excerpt", probe.Descriptor().Name, `{}`),
		textStream("delegated"),
	}}, registry)
	root := t.TempDir()
	engine.options.Workspace = root
	if err := os.WriteFile(
		filepath.Join(root, "node.go"), []byte("package node\n"), 0o644,
	); err != nil {
		t.Fatal(err)
	}
	probe.engine = engine

	if _, err := engine.Run(t.Context(), "delegate the parser", nil); err != nil {
		t.Fatal(err)
	}
	if probe.timedOut.Load() {
		t.Fatal("WorkspaceExcerpt blocked while the parent turn was running")
	}
	if got := probe.excerpt.Load(); got == nil || *got != "package node" {
		t.Fatalf("excerpt = %v", got)
	}
}

type excerptProbeTool struct {
	engine   *Engine
	excerpt  atomic.Pointer[string]
	timedOut atomic.Bool
}

func (*excerptProbeTool) Descriptor() tool.Descriptor {
	return tool.Descriptor{
		Name: "excerpt_probe", Description: "reads a parent workspace excerpt",
		Visibility: tool.VisibleModel, Capability: tool.CapabilityRead,
		AccessMode: tool.AccessRead, ParallelPolicy: tool.ParallelSerial,
		SandboxRequirement: tool.SandboxNone,
		Availability:       tool.AvailabilityAvailable,
		InputSchema: map[string]any{
			"type":                 "object",
			"properties":           map[string]any{},
			"additionalProperties": false,
		},
	}
}

func (p *excerptProbeTool) Execute(context.Context, json.RawMessage) (tool.Result, error) {
	done := make(chan string, 1)
	go func() {
		excerpt, _ := p.engine.WorkspaceExcerpt("node.go", 256)
		done <- excerpt
	}()
	select {
	case excerpt := <-done:
		p.excerpt.Store(&excerpt)
	case <-time.After(5 * time.Second):
		p.timedOut.Store(true)
	}
	return tool.Result{Content: "probed"}, nil
}

func TestWorkspaceExcerptRejectsEscapesAndUnreadableInput(t *testing.T) {
	engine := newEngine(
		t, &scriptedProvider{streams: []provider.Stream{textStream("ok")}},
		tool.NewRegistry(nil, nil),
	)
	root := t.TempDir()
	engine.options.Workspace = root
	if err := os.WriteFile(
		filepath.Join(root, "empty.go"), nil, 0o644,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, "blob.bin"), []byte{0x00, 0xff, 0xfe, 0xfd},
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"../outside.go", "/etc/passwd", "..", ".", "missing.go", "empty.go",
		"blob.bin",
	} {
		if excerpt, ok := engine.WorkspaceExcerpt(path, 256); ok {
			t.Fatalf("excerpt(%q) = %q, want none", path, excerpt)
		}
	}
	if _, ok := engine.WorkspaceExcerpt("node.go", 0); ok {
		t.Fatal("zero bound produced an excerpt")
	}
}
