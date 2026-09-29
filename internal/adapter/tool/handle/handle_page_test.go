package handle_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/handle"
	"github.com/fwtllh-png/QCode/testutil/tooltest"
)

type pageReader struct {
	registry *tool.Registry
	ref      handle.VarHandle
}

func newPageReader(t *testing.T, body string) pageReader {
	t.Helper()
	store := handle.NewStore()
	ref, err := store.PutText("sess", "payload", body)
	if err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry(nil, nil)
	if err := handle.Register(registry, store); err != nil {
		t.Fatal(err)
	}
	return pageReader{registry: registry, ref: ref}
}

func (r pageReader) read(t *testing.T, arguments map[string]any) tool.Result {
	t.Helper()
	arguments["handle"] = r.ref
	raw, err := json.Marshal(arguments)
	if err != nil {
		t.Fatal(err)
	}
	result, err := tooltest.Execute(t.Context(), r.registry, tool.Call{
		Name: "handle_read", Arguments: raw,
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func cursor(result tool.Result, key string) (int, bool) {
	value, ok := result.Metadata[key]
	if !ok {
		return 0, false
	}
	switch number := value.(type) {
	case int:
		return number, true
	case float64:
		return int(number), true
	}
	return 0, false
}

// A model-chosen byte offset inside a multibyte character must still yield a
// page and a cursor that advances; otherwise paging repeats forever.
func TestHandleReadBytesPageAdvancesFromInsideACharacter(t *testing.T) {
	reader := newPageReader(t, strings.Repeat("中文内容", 16))
	page := reader.read(t, map[string]any{"mode": "bytes", "offset": 1, "max_bytes": 8})
	next, ok := cursor(page, "next_offset")
	if page.Content == "" || !ok || next <= 1 {
		t.Fatalf("page = %q, next_offset = %d (%v)", page.Content, next, ok)
	}
}

// A single line longer than the page must not hand back the same start line
// as its continuation cursor.
func TestHandleReadLinesPageAdvancesPastAnOverlongLine(t *testing.T) {
	reader := newPageReader(t, strings.Repeat("x", 100)+"\nshort\n")
	page := reader.read(t, map[string]any{"mode": "lines", "start_line": 1, "max_bytes": 16})
	if !page.Truncated {
		t.Fatalf("page = %+v, want truncated", page)
	}
	if line, ok := cursor(page, "next_start_line"); ok && line <= 1 {
		t.Fatalf("next_start_line = %d repeats the overlong line", line)
	}
	if offset, ok := cursor(page, "next_offset"); !ok || offset != len(page.Content) {
		t.Fatalf("next_offset = %d (%v), want %d", offset, ok, len(page.Content))
	}
}

// Query pages obey the byte cap even when one matching line exceeds it.
func TestHandleReadQueryPageRespectsTheByteCap(t *testing.T) {
	reader := newPageReader(t, strings.Repeat("needle ", 50)+"\n")
	page := reader.read(t, map[string]any{"mode": "query", "query": "needle", "max_bytes": 16})
	if page.Content == "" || len(page.Content) > 16 {
		t.Fatalf("query page = %d bytes: %q", len(page.Content), page.Content)
	}
}

// handle_read and result_get page through the same projector, so the same
// body and request produce the same window and the same cursors.
func TestHandleReadAndResultGetPageIdentically(t *testing.T) {
	body := strings.Repeat("alpha 中文 needle\n", 6) +
		strings.Repeat("y", 90) + "\nomega needle 结束\n"
	reader := newPageReader(t, body)
	results := tool.NewResultStore(64)
	routed := results.Route(tool.Result{Content: body})
	if routed.Handle == "" {
		t.Fatal("result store did not retain the body")
	}
	resultRegistry := tool.NewRegistry(nil, results)
	requests := []map[string]any{
		{"mode": "summary", "max_bytes": 40},
		{"mode": "head", "max_bytes": 17},
		{"mode": "tail", "max_bytes": 17},
		{"mode": "bytes", "offset": 7, "max_bytes": 11},
		{"mode": "bytes", "offset": 9, "max_bytes": 5},
		{"mode": "lines", "start_line": 2, "max_lines": 3, "max_bytes": 60},
		{"mode": "lines", "start_line": 7, "max_bytes": 32},
		{"mode": "query", "query": "needle", "max_lines": 2, "max_bytes": 64},
		{"mode": "query", "query": "needle", "offset": 5, "max_bytes": 12},
	}
	keys := []string{"offset", "next_offset", "previous_offset", "start_line", "next_start_line"}
	for _, request := range requests {
		name := fmt.Sprint(request)
		fromHandle := reader.read(t, clone(request))
		arguments := clone(request)
		arguments["handle"] = routed.Handle
		raw, err := json.Marshal(arguments)
		if err != nil {
			t.Fatal(err)
		}
		fromResult, err := tooltest.Execute(t.Context(), resultRegistry, tool.Call{
			Name: "result_get", Arguments: raw,
		})
		if err != nil {
			t.Fatal(err)
		}
		if fromHandle.Content != fromResult.Content ||
			fromHandle.Truncated != fromResult.Truncated {
			t.Errorf("%s: handle_read %q (more=%v), result_get %q (more=%v)", name,
				fromHandle.Content, fromHandle.Truncated, fromResult.Content, fromResult.Truncated)
		}
		for _, key := range keys {
			left, leftOK := cursor(fromHandle, key)
			right, rightOK := cursor(fromResult, key)
			if left != right || leftOK != rightOK {
				t.Errorf("%s: %s handle_read=%d(%v) result_get=%d(%v)", name, key, left, leftOK, right, rightOK)
			}
		}
	}
}

func clone(values map[string]any) map[string]any {
	copied := make(map[string]any, len(values))
	for key, value := range values {
		copied[key] = value
	}
	return copied
}
