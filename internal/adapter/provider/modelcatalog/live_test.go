package modelcatalog

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
)

func TestProbeReportsWhetherConnectionListsExactModel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(
		response http.ResponseWriter,
		request *http.Request,
	) {
		if request.URL.Path != "/v1/models" {
			http.NotFound(response, request)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(
			`{"data":[{"id":"model-a"},{"id":"model-b"}]}`,
		))
	}))
	defer server.Close()

	for _, test := range []struct {
		model string
		want  bool
	}{
		{model: "model-a", want: true},
		{model: "model-c", want: false},
	} {
		got, err := Probe(
			t.Context(),
			"openai-compatible",
			server.URL+"/v1",
			model.CredentialRef{},
			test.model,
		)
		if err != nil {
			t.Fatal(err)
		}
		if got != test.want {
			t.Fatalf("Probe(%q) = %v, want %v", test.model, got, test.want)
		}
	}
}

func TestDiscoverPreservesOptionalCapacityMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(
		response http.ResponseWriter,
		_ *http.Request,
	) {
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"data":[{
			"id":"model-a",
			"display_name":"Model A",
			"context_length":65536,
			"max_output_tokens":4096
		}]}`))
	}))
	defer server.Close()

	result, err := Discover(
		t.Context(),
		"openai-compatible",
		server.URL,
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	models, ok := result["model_metadata"].([]DiscoveredModel)
	if !ok || len(models) != 1 ||
		models[0].ID != "model-a" ||
		models[0].Name != "Model A" ||
		models[0].ContextTokens != 65_536 ||
		models[0].MaxOutputTokens != 4_096 {
		t.Fatalf("discovered models = %#v", result["model_metadata"])
	}
}

func TestProbeCapabilitiesObservesStreamToolAndReasoning(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(
		response http.ResponseWriter,
		request *http.Request,
	) {
		if request.URL.Path != "/chat/completions" {
			http.NotFound(response, request)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if _, exists := body["max_tokens"]; exists {
			t.Fatal("capability probe imposed a fixed output token limit")
		}
		response.Header().Set("Content-Type", "text/event-stream")
		for _, data := range []string{
			`{"choices":[{"delta":{"reasoning_content":"checking"}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-1","function":{"name":"capability_probe","arguments":"{}"}}]}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		} {
			_, _ = fmt.Fprintf(response, "data: %s\n\n", data)
		}
		_, _ = fmt.Fprint(response, "data: [DONE]\n\n")
	}))
	defer server.Close()

	capabilities, err := ProbeCapabilities(
		t.Context(),
		server.URL,
		"secret",
		"model-a",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !capabilities.Streaming ||
		!capabilities.ToolCalls ||
		!capabilities.Reasoning {
		t.Fatalf("capabilities = %+v", capabilities)
	}
}

func TestProtocolProbeRejectsUnknownProtocol(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		t.Error("invalid probe must not issue a request")
	}))
	defer server.Close()
	for _, protocol := range []model.WireProtocol{"unknown", "anthropic"} {
		_, err := ProbeCapabilitiesForProtocol(t.Context(), server.URL, "", "unknown-model", protocol)
		if err == nil {
			t.Fatalf("accepted protocol=%s", protocol)
		}
	}
	if requests != 0 {
		t.Fatalf("requests=%d", requests)
	}
}
