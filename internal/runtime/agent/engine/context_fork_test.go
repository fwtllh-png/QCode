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
	"github.com/fwtllh-png/QCode/internal/common/contextsnapshot"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
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

func TestParentContextSnapshotRequiresExactTurn(t *testing.T) {
	for _, test := range []struct {
		name        string
		activeTurn  string
		requestTurn string
		wantError   string
	}{
		{"missing scope", "", "turn-parent", "has no context snapshot"},
		{"missing turn", "turn-parent", "", "parent turn id is required"},
		{"changed turn", "turn-next", "turn-parent", "parent turn changed from turn-parent to turn-next"},
	} {
		t.Run(test.name, func(t *testing.T) {
			engine := &Engine{}
			if test.activeTurn != "" {
				engine.publishScope(&Scope{
					engine: engine,
					spec: TurnSpec{Identity: TurnIdentity{
						ThreadID: "thread-parent", TurnID: test.activeTurn,
					}},
				})
			}
			_, err := engine.ParentContextSnapshot(contextsnapshot.SourceRef{
				ThreadID: "thread-parent", TurnID: test.requestTurn,
			})
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("snapshot error = %v, want %q", err, test.wantError)
			}
		})
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

func TestProjectMessagesExcludesOpaqueParentContent(t *testing.T) {
	messages := []provider.Message{
		provider.TextMessage(provider.RoleUser, "parent goal"),
		{
			Role: provider.RoleAssistant, Turn: 1,
			Provenance: &provider.AssistantProvenance{
				Adapter: "openai", Provider: "openai", Model: "model",
				Replay: &provider.ReplayState{
					Version:       provider.ReplayVersion,
					ContentDigest: "opaque-digest",
					Data:          []byte(`{"private":"replay"}`),
				},
			},
			Blocks: []provider.ContentBlock{
				{Type: provider.ContentText, Text: "visible"},
				{Type: provider.ContentReasoning, Text: "private reasoning"},
				{Type: provider.ContentToolCall, ToolCall: &provider.ToolCall{
					ID: "call-1", Name: "file_read", Arguments: `{"path":"a.go"}`,
				}},
			},
		},
		{
			Role: provider.RoleTool, Turn: 1,
			Blocks: []provider.ContentBlock{{
				Type: provider.ContentToolResult,
				ToolResult: &provider.ToolResult{
					CallID: "call-1", Content: "file body",
				},
			}},
		},
	}
	projected := projectMessages(messages)
	if parentGoal(projected, "") != "parent goal" {
		t.Fatalf("projected = %+v", projected)
	}
	rendered := strings.Builder{}
	for _, message := range projected {
		for _, block := range message.Blocks {
			rendered.WriteString(block.Text)
			rendered.WriteString(block.Arguments)
		}
	}
	if strings.Contains(rendered.String(), "private reasoning") ||
		strings.Contains(rendered.String(), "opaque") ||
		strings.Contains(rendered.String(), "replay") ||
		!strings.Contains(rendered.String(), "visible") ||
		!strings.Contains(rendered.String(), "file body") {
		t.Fatalf("projected messages = %+v", projected)
	}
}

func TestLatestWorldTextUsesTypedMarkerAndTombstone(t *testing.T) {
	message := provider.TextMessage(provider.RoleSystem, "coding rules")
	full, err := agentcontext.ProjectWorld(
		[]agentcontext.WorldSection{{
			ID: "coding_policy", Digest: "digest-1",
			Present: true, Message: &message,
		}},
		agentcontext.WorldBaseline{},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := latestWorldText(full.Messages, "coding_policy"); got != "coding rules" {
		t.Fatalf("world text=%q", got)
	}
	history := append([]provider.Message(nil), full.Messages...)
	removed, err := agentcontext.ProjectWorld(
		nil,
		full.Baseline,
		history,
	)
	if err != nil {
		t.Fatal(err)
	}
	history = append(history, removed.Messages...)
	if got := latestWorldText(history, "coding_policy"); got != "" {
		t.Fatalf("removed world text=%q", got)
	}
}
