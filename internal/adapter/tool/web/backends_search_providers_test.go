package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/testutil/tooltest"
)

func TestWebSearchTavilySearXNGBochaBackends(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/tavily" && request.Method == http.MethodPost:
			var body map[string]any
			_ = json.NewDecoder(request.Body).Decode(&body)
			if body["api_key"] != "tvly-test" || body["query"] != "go modules" {
				http.Error(writer, "bad tavily", http.StatusBadRequest)
				return
			}
			_, _ = writer.Write([]byte(`{"results":[{"title":"Tavily Hit","url":"https://example.com/tavily","content":"tavily snippet"}]}`))
		case request.URL.Path == "/searx/search":
			if request.URL.Query().Get("q") != "go modules" || request.URL.Query().Get("format") != "json" {
				http.Error(writer, "bad searx", http.StatusBadRequest)
				return
			}
			_, _ = writer.Write([]byte(`{"results":[{"title":"SearX Hit","url":"https://example.com/searx","content":"searx snippet"}]}`))
		case request.URL.Path == "/bocha" && request.Method == http.MethodPost:
			if request.Header.Get("Authorization") != "Bearer bocha-key" {
				http.Error(writer, "bad auth", http.StatusUnauthorized)
				return
			}
			_, _ = writer.Write([]byte(`{"data":{"webPages":{"value":[{"name":"Bocha Hit","url":"https://example.com/bocha","snippet":"bocha snippet"}]}}}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	cases := []struct {
		name    string
		tool    *Tool
		backend string
		needle  string
	}{
		{
			name: "tavily",
			tool: &Tool{
				kind: "web_search", searchBackend: "tavily",
				tavilyURL: server.URL + "/tavily", tavilyAPIKey: "tvly-test",
			},
			backend: "tavily", needle: "https://example.com/tavily",
		},
		{
			name: "searxng",
			tool: &Tool{
				kind: "web_search", searchBackend: "searxng",
				searxngURL: server.URL + "/searx",
			},
			backend: "searxng", needle: "https://example.com/searx",
		},
		{
			name: "bocha",
			tool: &Tool{
				kind: "web_search", searchBackend: "bocha",
				bochaURL: server.URL + "/bocha", bochaAPIKey: "bocha-key",
			},
			backend: "bocha", needle: "https://example.com/bocha",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := boundTool(t, tc.tool).
				Execute(t.Context(), json.RawMessage(`{"query":"go modules","limit":3}`))
			if err != nil {
				t.Fatal(err)
			}
			if result.IsError || result.Metadata["backend"] != tc.backend {
				t.Fatalf("result = %+v", result)
			}
			if !strings.Contains(result.Content, tc.needle) {
				t.Fatalf("content = %s", result.Content)
			}
			receipts, _ := result.Metadata["receipts"].([]map[string]any)
			if len(receipts) != 1 || receipts[0]["backend"] != tc.backend {
				t.Fatalf("receipts = %#v", result.Metadata["receipts"])
			}
		})
	}
}

func TestWebRunUnavailableAndFake(t *testing.T) {
	registry := tool.NewRegistry(nil, nil)
	if err := RegisterWithOptions(registry, Options{}); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range registry.Descriptors(tool.VisibleModel) {
		if d.Name == "web_run" {
			found = true
			if d.Availability != tool.AvailabilityUnavailable ||
				d.UnavailableReason != BrowserUnavailableReason {
				t.Fatalf("descriptor = %+v", d)
			}
		}
	}
	if !found {
		t.Fatal("web_run missing")
	}

	fake := NewFakeBrowser()
	registry = tool.NewRegistry(nil, nil)
	if err := RegisterWithOptions(registry, Options{Browser: fake}); err != nil {
		t.Fatal(err)
	}
	nav, err := tooltest.Execute(t.Context(), registry, tool.Call{
		Name:      "web_run",
		Arguments: json.RawMessage(`{"action":"navigate","url":"https://example.com"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if nav.IsError || !strings.Contains(nav.Content, "https://example.com") {
		t.Fatalf("navigate = %+v", nav)
	}
	if _, err := tooltest.Execute(t.Context(), registry, tool.Call{
		Name:      "web_run",
		Arguments: json.RawMessage(`{"action":"click","selector":"#go"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tooltest.Execute(t.Context(), registry, tool.Call{
		Name:      "web_run",
		Arguments: json.RawMessage(`{"action":"fill","selector":"#q","value":"hi"}`),
	}); err != nil {
		t.Fatal(err)
	}
	snap, err := tooltest.Execute(t.Context(), registry, tool.Call{
		Name:      "web_run",
		Arguments: json.RawMessage(`{"action":"snapshot"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if snap.IsError || !strings.Contains(snap.Content, "data-filled") {
		t.Fatalf("snapshot = %+v", snap)
	}
}
