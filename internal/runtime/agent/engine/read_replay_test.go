package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
)

func TestReadReplayPreservesWindowThroughKernel(t *testing.T) {
	engine := evidenceEngine(t)
	if err := os.WriteFile(filepath.Join(engine.options.Workspace, "parser.go"),
		[]byte(strings.Repeat("line\n", 100)), 0o644); err != nil {
		t.Fatal(err)
	}
	digest, err := engine.currentWorkspaceDigest("parser.go")
	if err != nil {
		t.Fatal(err)
	}
	kernel := readReplayKernel(t)
	engine.admissionKernel = kernel
	closeRead := func(call provider.ToolCall, result tool.Result) {
		t.Helper()
		if err := kernel.StartTools([]provider.ToolCall{call}); err != nil {
			t.Fatal(err)
		}
		if err := kernel.StartTool(call.ID); err != nil {
			t.Fatal(err)
		}
		if err := kernel.CloseTool(call, result, nil); err != nil {
			t.Fatal(err)
		}
		engine.recordReadResults([]provider.ToolCall{call}, []tool.Result{result})
	}
	first := provider.ToolCall{
		ID: "first", Name: "file_read",
		Arguments: `{"path":"parser.go","start_line":40,"max_lines":20}`,
	}
	result := tool.Result{
		Content: "lines 40 through 59", Truncated: true, OriginalBytes: 500,
		Handle: "read-result",
		Metadata: map[string]any{
			"start_line": 40, "returned_lines": 20, "max_lines": 20,
			"has_more": true, "next_start_line": 60,
		},
		Outcome: &tool.Outcome{Status: tool.OutcomeSucceeded, Facts: &tool.OutcomeFacts{
			WorkspaceRead: &tool.WorkspaceReadFact{Path: "parser.go", Digest: digest},
			ResultHandle:  "read-result",
		}},
	}
	closeRead(first, result)
	repeated := provider.ToolCall{
		ID: "repeated", Name: "file_read",
		Arguments: `{"max_lines":20, "start_line":40, "path":"parser.go"}`,
	}
	replay := engine.observationGate(repeated, false)
	if replay == nil || replay.Content != result.Content || !replay.Truncated ||
		replay.OriginalBytes != result.OriginalBytes || replay.Handle != result.Handle ||
		replay.Outcome.Facts.ResultHandle != result.Outcome.Facts.ResultHandle ||
		replay.Metadata["returned_lines"] != 20 || replay.Metadata["has_more"] != true ||
		replay.Metadata["next_start_line"] != 60 {
		t.Fatalf("read surface changed: %+v", replay)
	}
	closeRead(repeated, *replay)
	read, ok := kernel.WorkItem().KnownRead("parser.go")
	if !ok || read.StartLine != 40 || read.EndLine != 60 ||
		read.ResultHandle != "read-result" {
		t.Fatalf("replay widened the kernel read window: %+v", read)
	}
	next := provider.ToolCall{
		ID: "next", Name: "file_read",
		Arguments: `{"path":"parser.go","start_line":80,"max_lines":20}`,
	}
	if replay := engine.observationGate(next, false); replay != nil {
		t.Fatalf("later window received stale body: %+v", replay)
	}
	result.Content = "lines 80 through 99"
	result.Metadata["start_line"] = 80
	result.Metadata["next_start_line"] = 100
	closeRead(next, result)
	read, _ = kernel.WorkItem().KnownRead("parser.go")
	if read.StartLine != 80 || read.EndLine != 100 {
		t.Fatalf("fresh window was not recorded: %+v", read)
	}
}

func TestReadReplayRequiresSameRequestAndOwnsReturnedFacts(t *testing.T) {
	for _, original := range []string{
		`{"path":"parser.go","max_lines":20}`,
		`{"path":"parser.go","start_line":40,"max_lines":20}`,
		`{"path":"parser.go","pages":"1"}`,
	} {
		t.Run(original, func(t *testing.T) {
			engine := evidenceEngine(t)
			if err := os.WriteFile(filepath.Join(engine.options.Workspace, "parser.go"),
				[]byte("unchanged"), 0o644); err != nil {
				t.Fatal(err)
			}
			digest, err := engine.currentWorkspaceDigest("parser.go")
			if err != nil {
				t.Fatal(err)
			}
			call := provider.ToolCall{ID: "source", Name: "file_read", Arguments: original}
			result := tool.Result{
				Content:  "original body",
				Metadata: map[string]any{"returned_lines": 20, "has_more": true},
				Outcome: &tool.Outcome{Facts: &tool.OutcomeFacts{
					WorkspaceRead: &tool.WorkspaceReadFact{Path: "parser.go", Digest: digest},
				}},
			}
			kernel := readReplayKernel(t)
			engine.admissionKernel = kernel
			if err := kernel.StartTools([]provider.ToolCall{call}); err != nil {
				t.Fatal(err)
			}
			if err := kernel.StartTool(call.ID); err != nil {
				t.Fatal(err)
			}
			if err := kernel.CloseTool(call, result, nil); err != nil {
				t.Fatal(err)
			}
			engine.recordReadResults([]provider.ToolCall{call}, []tool.Result{result})
			result.Metadata["has_more"] = false
			replay := engine.observationGate(call, false)
			if replay == nil || replay.Metadata["has_more"] != true {
				t.Fatalf("cached metadata aliased input: %+v", replay)
			}
			replay.Metadata["has_more"] = false
			replay.Outcome.Facts.WorkspaceRead.Digest = "corrupted"
			replay = engine.observationGate(call, false)
			if replay == nil || replay.Metadata["has_more"] != true ||
				replay.Outcome.Facts.WorkspaceRead.Digest != digest {
				t.Fatalf("cached facts aliased replay: %+v", replay)
			}
			for _, arguments := range []string{
				`{"path":"parser.go","max_lines":40}`,
				`{"path":"parser.go","start_line":40,"max_lines":40}`,
				`{"path":"parser.go","start_line":50,"max_lines":20}`,
				`{"path":"parser.go","start_line":80}`,
				`{"path":"parser.go","pages":"2"}`,
			} {
				request := provider.ToolCall{ID: "changed-request", Name: "file_read", Arguments: arguments}
				if replay := engine.observationGate(request, false); replay != nil {
					t.Fatalf("different request %s reused %s: %+v", arguments, original, replay)
				}
			}
			if err := os.WriteFile(filepath.Join(engine.options.Workspace, "parser.go"),
				[]byte("changed"), 0o644); err != nil {
				t.Fatal(err)
			}
			if replay := engine.observationGate(call, false); replay != nil {
				t.Fatalf("changed file replayed: %+v", replay)
			}
		})
	}
}
