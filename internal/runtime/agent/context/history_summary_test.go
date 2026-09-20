package agentcontext

import (
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
)

func TestSummaryLineKeepsFailedToolResultShape(t *testing.T) {
	call := assistantToolCall("exec-1", "exec_command", `{"command":"go test ./parser"}`)
	result := toolResultMessage("exec-1", true, "FAIL: TestParse trailing comma")
	names := toolCallNames([]provider.Message{call, result})
	line := SummaryLine(result, 512, names)
	for _, want := range []string{
		"tool result", "exec-1", "exec_command", "error", "FAIL: TestParse",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("line = %q, missing %q", line, want)
		}
	}
}

func TestSummaryLineTruncatesToolResultShape(t *testing.T) {
	result := toolResultMessage("call-1", true, strings.Repeat("x", summaryIdentityBytes+40))
	line := SummaryLine(result, 512, map[string]string{"call-1": "shell"})
	shape := strings.TrimPrefix(line, "tool: tool result call-1 shell error ")
	if !strings.HasSuffix(shape, "...") ||
		len(strings.TrimSuffix(shape, "...")) != summaryIdentityBytes {
		t.Fatalf("shape was not bounded: %q", line)
	}
}

func TestBuildSummaryDigestIncludesFailedCommandShape(t *testing.T) {
	removed := []provider.Message{
		assistantToolCall("exec-1", "exec_command", `{"command":"go test ./parser"}`),
		toolResultMessage("exec-1", true, "exit 1: compiler could not locate stdlib"),
	}
	authority := NewAuthority()
	authority.ObserveToolFailure(
		provider.ToolCall{ID: "exec-1", Name: "exec_command"},
		tool.Result{IsError: true, Content: removed[1].Blocks[0].ToolResult.Content},
		1,
	)
	result := authority.BuildSummary(SummaryRequest{
		Removed: removed, Turn: 1, SummaryLineBytes: 512,
	})
	if len(result.Summary.Digest) != 2 {
		t.Fatalf("digest = %v", result.Summary.Digest)
	}
	line := result.Summary.Digest[0]
	for _, want := range []string{
		"tool result", "exec-1", "exec_command", "error", "compiler could not locate",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("digest line = %q, missing %q", line, want)
		}
	}
	failures := result.Summary.Failures
	if len(failures) != 1 || failures[0].CallID != "exec-1" ||
		failures[0].Name != "exec_command" ||
		!strings.Contains(failures[0].line(), "exec-1") {
		t.Fatalf("failures = %+v", failures)
	}
}

func TestBuildSummaryCountsOmittedDigest(t *testing.T) {
	removed := []provider.Message{
		provider.TextMessage(provider.RoleUser, "oldest"),
		provider.TextMessage(provider.RoleAssistant, "middle"),
		provider.TextMessage(provider.RoleUser, "newest"),
	}
	result := NewAuthority().BuildSummary(SummaryRequest{
		Removed: removed, MaxDigestEntries: 2, SummaryLineBytes: 512,
	})
	if len(result.Summary.Digest) != 2 || result.Summary.OmittedDigest != 1 {
		t.Fatalf(
			"digest=%v omitted=%d",
			result.Summary.Digest, result.Summary.OmittedDigest,
		)
	}
	if !strings.Contains(result.Summary.Digest[0], "newest") {
		t.Fatalf("expected newest first: %v", result.Summary.Digest)
	}
	if strings.Contains(strings.Join(result.Summary.Digest, "\n"), "oldest") {
		t.Fatalf("kept the omitted line: %v", result.Summary.Digest)
	}
}

func TestBuildSummaryMergesCarriedDigestOnRecompaction(t *testing.T) {
	first := NewAuthority().BuildSummary(SummaryRequest{
		Removed: []provider.Message{
			provider.TextMessage(provider.RoleUser, "oldest"),
			provider.TextMessage(provider.RoleAssistant, "middle"),
			provider.TextMessage(provider.RoleUser, "newest"),
		},
		MaxDigestEntries: 2, SummaryLineBytes: 512,
	})
	rendered, _, _ := first.Summary.Render(0)
	second := NewAuthority().BuildSummary(SummaryRequest{
		Removed: []provider.Message{
			provider.TextMessage(provider.RoleSystem, rendered),
			provider.TextMessage(provider.RoleAssistant, "after compact"),
		},
		MaxDigestEntries: 2, SummaryLineBytes: 512,
	})
	if len(second.Summary.Digest) != 2 {
		t.Fatalf("second digest=%v", second.Summary.Digest)
	}
	if !strings.Contains(second.Summary.Digest[0], "after compact") {
		t.Fatalf("lost the newest line: %v", second.Summary.Digest)
	}
	if !strings.Contains(second.Summary.Digest[1], "newest") {
		t.Fatalf("lost the prior digest prefix: %v", second.Summary.Digest)
	}
	if second.Summary.OmittedDigest != 2 {
		t.Fatalf("omitted=%d, want 2", second.Summary.OmittedDigest)
	}
	joined := strings.Join(second.Summary.Digest, "\n")
	if strings.Contains(joined, "Summary of earlier") ||
		strings.Contains(joined, MarkerStart) {
		t.Fatalf("flattened the whole prior summary: %v", second.Summary.Digest)
	}
}

func assistantToolCall(id, name, arguments string) provider.Message {
	return provider.Message{
		Role: provider.RoleAssistant,
		Blocks: []provider.ContentBlock{{
			Type: provider.ContentToolCall,
			ToolCall: &provider.ToolCall{
				ID: id, Name: name, Arguments: arguments,
			},
		}},
	}
}

func toolResultMessage(callID string, isError bool, content string) provider.Message {
	return provider.Message{
		Role: provider.RoleTool,
		Blocks: []provider.ContentBlock{{
			Type: provider.ContentToolResult,
			ToolResult: &provider.ToolResult{
				CallID: callID, IsError: isError, Content: content,
			},
		}},
	}
}

