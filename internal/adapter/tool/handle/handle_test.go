package handle_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/handle"
	"github.com/fwtllh-png/QCode/testutil/tooltest"
)

func TestHandleReadBoundedProjection(t *testing.T) {
	store := handle.NewStore()
	payload := strings.Repeat("alpha-line\n", 4000)
	ref, err := store.PutText("sess-1", "transcript", payload)
	if err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry(nil, nil)
	if err := handle.Register(registry, store); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{
		"handle":    ref,
		"mode":      "head",
		"max_bytes": 128,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := tooltest.Execute(context.Background(), registry, tool.Call{
		Name: "handle_read", Arguments: raw,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Truncated || len(result.Content) > 128 {
		t.Fatalf("expected truncated head, got %+v", result)
	}
	if result.Metadata["session_id"] != "sess-1" || result.Metadata["name"] != "transcript" {
		t.Fatalf("metadata = %#v", result.Metadata)
	}

	compact, err := json.Marshal(map[string]any{
		"handle": "sess-1/transcript",
		"mode":   "count",
	})
	if err != nil {
		t.Fatal(err)
	}
	count, err := tooltest.Execute(context.Background(), registry, tool.Call{
		Name: "handle_read", Arguments: compact,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(count.Content, `"kind":"var_handle"`) {
		t.Fatalf("count content = %q", count.Content)
	}
}

func TestHandleReadRejectsSHAMismatch(t *testing.T) {
	store := handle.NewStore()
	ref, err := store.PutText("s", "n", "hello")
	if err != nil {
		t.Fatal(err)
	}
	ref.SHA256 = "deadbeef"
	raw, _ := json.Marshal(map[string]any{"handle": ref, "mode": "summary"})
	tool := &handle.ReadTool{Store: store}
	_, err = tool.Execute(context.Background(), raw)
	if err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("error = %v", err)
	}
}

func TestHandleReadCompactFormAllowsNamespacedName(t *testing.T) {
	store := handle.NewStore()
	if _, err := store.PutText(
		"sess-1",
		"agent-agent-1/transcript",
		"ParallelPolicy",
	); err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry(nil, nil)
	if err := handle.Register(registry, store); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{
		"handle": "sess-1/agent-agent-1/transcript",
		"mode":   "query",
		"query":  "ParallelPolicy",
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := tooltest.Execute(context.Background(), registry, tool.Call{
		Name: "handle_read", Arguments: raw,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "ParallelPolicy" {
		t.Fatalf("namespaced handle content = %q", result.Content)
	}
}
