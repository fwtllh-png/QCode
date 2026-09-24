package file

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/testutil/tooltest"
)

func readTruncationRegistry(t *testing.T, files map[string]string) *tool.Registry {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tools, err := NewWithBackend(root, fileTestBackend{})
	if err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry(nil, nil)
	if err := tools.Register(registry); err != nil {
		t.Fatal(err)
	}
	return registry
}

func TestFileReadTruncatesLongLinesAndReportsThem(t *testing.T) {
	long := strings.Repeat("a", maxReadLineRunes+1500)
	content := "short one\n" + long + "\nshort two\n" + strings.Repeat("好", maxReadLineRunes+10) + "\n"
	registry := readTruncationRegistry(t, map[string]string{"blob.txt": content})

	result, err := tooltest.Execute(t.Context(), registry, tool.Call{
		Name: "file_read", Arguments: json.RawMessage(`{"path":"blob.txt"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(result.Content, "\n")
	if len(lines) != 4 {
		t.Fatalf("lines = %d, want 4: %q", len(lines), result.Content)
	}
	if got := len([]rune(strings.TrimSuffix(lines[1], readLineTruncationNote))); got != maxReadLineRunes {
		t.Fatalf("ascii long line kept %d runes, want %d", got, maxReadLineRunes)
	}
	if !strings.HasSuffix(lines[1], readLineTruncationNote) {
		t.Fatalf("ascii line missing truncation note: %q", lines[1])
	}
	cjk := strings.TrimSuffix(lines[3], readLineTruncationNote)
	if got := len([]rune(cjk)); got != maxReadLineRunes {
		t.Fatalf("cjk long line kept %d runes, want %d", got, maxReadLineRunes)
	}
	if !utf8.ValidString(result.Content) {
		t.Fatalf("truncated content is not valid UTF-8")
	}
	truncated, ok := result.Metadata["truncated_lines"].([]int)
	if !ok || len(truncated) != 2 || truncated[0] != 2 || truncated[1] != 4 {
		t.Fatalf("truncated_lines = %#v", result.Metadata["truncated_lines"])
	}
	if result.Metadata["max_line_runes"] != maxReadLineRunes {
		t.Fatalf("max_line_runes = %v", result.Metadata["max_line_runes"])
	}
	// Untouched short lines survive verbatim.
	if lines[0] != "short one" || lines[2] != "short two" {
		t.Fatalf("short lines altered: %q", result.Content)
	}
}

func TestFileReadHandlesLineBeyondOldScannerCeiling(t *testing.T) {
	// A line past the previous 1 MiB scanner limit used to fail the whole
	// read with "token too long"; it now streams through and truncates.
	huge := strings.Repeat("z", (1<<20)+200)
	registry := readTruncationRegistry(t, map[string]string{"minified.js": huge + "\ntail\n"})

	result, err := tooltest.Execute(t.Context(), registry, tool.Call{
		Name: "file_read", Arguments: json.RawMessage(`{"path":"minified.js"}`),
	})
	if err != nil {
		t.Fatalf("read error = %v", err)
	}
	lines := strings.Split(result.Content, "\n")
	if len(lines) != 2 || lines[1] != "tail" {
		t.Fatalf("lines = %q", result.Content)
	}
	if got := len([]rune(strings.TrimSuffix(lines[0], readLineTruncationNote))); got != maxReadLineRunes {
		t.Fatalf("huge line kept %d runes, want %d", got, maxReadLineRunes)
	}
	if truncated, ok := result.Metadata["truncated_lines"].([]int); !ok || len(truncated) != 1 || truncated[0] != 1 {
		t.Fatalf("truncated_lines = %#v", result.Metadata["truncated_lines"])
	}
}

func TestFileReadStillRejectsBinaryInLongLines(t *testing.T) {
	registry := readTruncationRegistry(t, map[string]string{
		"binary.dat": strings.Repeat("a", maxReadLineRunes+100) + "\x00" + strings.Repeat("b", 50) + "\n",
	})
	if _, err := tooltest.Execute(t.Context(), registry, tool.Call{
		Name: "file_read", Arguments: json.RawMessage(`{"path":"binary.dat"}`),
	}); err == nil || !strings.Contains(err.Error(), "binary or non-UTF-8") {
		t.Fatalf("binary read error = %v", err)
	}
}

func TestFileReadChunkedUTF8AcrossBufferBoundary(t *testing.T) {
	// Build a file whose multi-byte runes straddle the reader's 64 KiB chunk
	// boundary: validity checking and the rune budget must survive the split.
	var content strings.Builder
	content.WriteString("head\n")
	for len(content.String()) < 64<<10 {
		content.WriteString("填充数据用于跨越缓冲区边界填充数据用于跨越缓冲区边界")
	}
	content.WriteString("\ntail\n")
	registry := readTruncationRegistry(t, map[string]string{"cjk.txt": content.String()})

	result, err := tooltest.Execute(t.Context(), registry, tool.Call{
		Name: "file_read", Arguments: json.RawMessage(`{"path":"cjk.txt","max_lines":3}`),
	})
	if err != nil {
		t.Fatalf("read error = %v", err)
	}
	if !utf8.ValidString(result.Content) {
		t.Fatalf("content is not valid UTF-8")
	}
	lines := strings.Split(result.Content, "\n")
	if lines[0] != "head" || lines[2] != "tail" {
		t.Fatalf("lines = %q", result.Content)
	}
}

func TestFileReadCRLFPaginationUnchanged(t *testing.T) {
	registry := readTruncationRegistry(t, map[string]string{
		"win.txt": "alpha\r\nbeta\r\n",
	})
	result, err := tooltest.Execute(t.Context(), registry, tool.Call{
		Name: "file_read", Arguments: json.RawMessage(`{"path":"win.txt","start_line":2,"max_lines":1}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "beta" {
		t.Fatalf("content = %q, want beta", result.Content)
	}
}
