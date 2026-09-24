package search

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/testutil/tooltest"
)

func decodePayload(t *testing.T, content string) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal([]byte(content), &payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

func executeErr(
	t *testing.T, registry *tool.Registry, name string, input map[string]any,
) (tool.Result, error) {
	t.Helper()
	data, _ := json.Marshal(input)
	ctx := tool.WithResultTokenBudget(t.Context(), 10_000)
	return tooltest.Execute(ctx, registry, tool.Call{
		Name: name, Arguments: data,
	})
}

func outputModeRegistry(t *testing.T, files map[string]string) *tool.Registry {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		write(t, filepath.Join(root, name), content)
	}
	registry := tool.NewRegistry(nil, nil)
	if err := RegisterWithBackend(registry, root, searchTestBackend{}); err != nil {
		t.Fatal(err)
	}
	return registry
}

func TestSearchOutputFilesRollsUpPerFileCounts(t *testing.T) {
	registry := outputModeRegistry(t, map[string]string{
		"beta.go":  "needle one\nneedle two\n",
		"alpha.go": "needle\nplain\n",
		"gamma.md": "nothing\n",
	})
	result := execute(t, registry, "search_text", map[string]any{
		"query": "needle", "output": "files", "max_results": 10,
	})
	payload := decodePayload(t, result.Content)
	files, ok := payload["files"].([]any)
	if !ok || len(files) != 2 {
		t.Fatalf("files rollup = %#v", payload)
	}
	first := files[0].(map[string]any)
	second := files[1].(map[string]any)
	if first["file"] != "alpha.go" || first["matches"] != float64(1) {
		t.Fatalf("first rollup = %#v", first)
	}
	if second["file"] != "beta.go" || second["matches"] != float64(2) {
		t.Fatalf("second rollup = %#v", second)
	}
	if payload["total_matches"] != float64(3) || payload["total_files"] != float64(2) {
		t.Fatalf("payload = %#v", payload)
	}
	if result.Truncated {
		t.Fatalf("unexpected truncation: %+v", result)
	}
	hits := result.Metadata["evidence"]
	if hits == nil {
		t.Fatalf("expected located-site evidence in metadata: %+v", result.Metadata)
	}
}

func TestSearchOutputCountReturnsTotalsOnly(t *testing.T) {
	registry := outputModeRegistry(t, map[string]string{
		"one.go": "hit\nhit\n", "two.go": "hit\n", "three.go": "miss\n",
	})
	result := execute(t, registry, "search_text", map[string]any{
		"query": "hit", "output": "count", "max_results": 10,
	})
	payload := decodePayload(t, result.Content)
	if payload["total_matches"] != float64(3) || payload["files_matched"] != float64(2) {
		t.Fatalf("count payload = %#v", payload)
	}
	if _, hasMatches := payload["matches"]; hasMatches {
		t.Fatalf("count output leaked a match list: %#v", payload)
	}
}

func TestSearchOutputFilesTruncatesFileEntries(t *testing.T) {
	registry := outputModeRegistry(t, map[string]string{
		"a.go": "hit\n", "b.go": "hit\n", "c.go": "hit\n",
	})
	result := execute(t, registry, "search_text", map[string]any{
		"query": "hit", "output": "files", "max_results": 2,
	})
	payload := decodePayload(t, result.Content)
	files := payload["files"].([]any)
	if len(files) != 2 || !result.Truncated {
		t.Fatalf("truncated rollup = %#v", payload)
	}
	if payload["total_matches"] != float64(3) {
		t.Fatalf("total_matches = %#v", payload)
	}
}

func TestSearchOutputRejectsUnknownMode(t *testing.T) {
	registry := outputModeRegistry(t, map[string]string{"a.go": "hit\n"})
	// The input schema rejects unknown output modes before run() executes.
	if _, err := executeErr(t, registry, "search_text", map[string]any{
		"query": "hit", "output": "summary", "max_results": 5,
	}); err == nil || !strings.Contains(err.Error(), "must be one of 'content', 'files', 'count'") {
		t.Fatalf("unknown output error = %v", err)
	}
}

func TestSearchParallelScanKeepsDeterministicOrder(t *testing.T) {
	files := make(map[string]string)
	names := []string{
		"m.go", "a.go", "z.go", "b.go", "y.go", "c.go", "x.go", "d.go",
		"w.go", "e.go", "v.go", "f.go", "u.go", "g.go", "t.go", "h.go",
	}
	for _, name := range names {
		files[name] = "needle first\nplain\nneedle second\n"
	}
	registry := outputModeRegistry(t, files)
	result := execute(t, registry, "search_text", map[string]any{
		"query": "needle", "max_results": 100,
	})
	matches := decodeMatches(t, result.Content)
	if len(matches) != len(names)*2 {
		t.Fatalf("matches = %d, want %d", len(matches), len(names)*2)
	}
	ordered := []string{"a.go", "b.go", "c.go", "d.go", "e.go", "f.go", "g.go",
		"h.go", "m.go", "t.go", "u.go", "v.go", "w.go", "x.go", "y.go", "z.go"}
	seen := 0
	for index, match := range matches {
		want := ordered[index/2]
		if match["file"] != want {
			t.Fatalf("match %d file = %v, want %v", index, match["file"], want)
		}
		wantLine := 1
		if index%2 == 1 {
			wantLine = 3
		}
		if match["line"] != float64(wantLine) {
			t.Fatalf("match %d line = %v, want %d", index, match["line"], wantLine)
		}
		seen++
	}
}

func TestScanTextMatchesMatchesSplitSemantics(t *testing.T) {
	data := "alpha\nbeta\nneedle one\nneedle two\ntail\n"
	matcher := func(line string) bool { return strings.Contains(line, "needle") }
	got := scanTextMatches(data, "f.go", false, matcher, 1, 1)

	// Reference implementation on strings.Split, mirroring the previous code.
	lines := strings.Split(data, "\n")
	type ref struct {
		line int
		text string
	}
	var want []ref
	for index, text := range lines {
		if matcher(text) {
			want = append(want, ref{line: index + 1, text: text})
		}
	}
	if len(got) != len(want) {
		t.Fatalf("matches = %d, want %d", len(got), len(want))
	}
	for index, expected := range want {
		if got[index].Line != expected.line || got[index].Text != expected.text {
			t.Fatalf("match %d = (%d, %q), want (%d, %q)",
				index, got[index].Line, got[index].Text, expected.line, expected.text)
		}
		before := got[index].Context.Before
		if len(before) != 1 || before[0].Line != expected.line-1 {
			t.Fatalf("context before = %#v", before)
		}
	}

	// Trailing-newline phantom line and lone-line files behave like Split.
	single := scanTextMatches("needle\n", "f.go", false, matcher, 0, 0)
	if len(single) != 1 || single[0].Line != 1 {
		t.Fatalf("single-file matches = %#v", single)
	}
	noTrailing := scanTextMatches("needle", "f.go", false, matcher, 0, 0)
	if len(noTrailing) != 1 || noTrailing[0].Line != 1 {
		t.Fatalf("no-trailing matches = %#v", noTrailing)
	}
}
