package wire

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
)

func TestProbeModelConnectionUsesSelectedProtocol(t *testing.T) {
	for _, test := range []struct {
		protocol model.WireProtocol
		path     string
		events   []string
	}{
		{model.ProtocolOpenAIChat, "/chat/completions", []string{
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call","function":{"name":"capability_probe","arguments":"{}"}}]}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			`[DONE]`,
		}},
		{model.ProtocolOpenAIResponses, "/responses", []string{
			`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call","name":"capability_probe"}}`,
			`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{}"}`,
			`{"type":"response.completed","response":{"status":"completed"}}`,
		}},
		{model.ProtocolAnthropic, "/messages", []string{
			`{"type":"message_start","message":{"usage":{"input_tokens":1}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call","name":"capability_probe","input":{}}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{}"}}`,
			`{"type":"message_stop"}`,
		}},
	} {
		t.Run(string(test.protocol), func(t *testing.T) {
			providerID := "openai-compatible"
			if test.protocol == model.ProtocolAnthropic {
				providerID = "anthropic"
			}
			for _, saved := range []bool{false, true} {
				t.Run(fmt.Sprint("saved=", saved), func(t *testing.T) {
					t.Setenv("QCODE_TEST_PROBE_KEY", "fixture-key")
					calls := 0
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						calls++
						if test.protocol == model.ProtocolAnthropic {
							if r.Header.Get("x-api-key") != "fixture-key" || r.Header.Get("anthropic-version") == "" {
								t.Error("missing Messages authentication")
							}
						} else if r.Header.Get("Authorization") != "Bearer fixture-key" {
							t.Error("missing bearer authentication")
						}
						if r.URL.Path == "/models" {
							fmt.Fprint(w, `{"data":[{"id":"unknown-model","context_window":65536,"max_output_tokens":8192}]}`)
							return
						}
						if r.URL.Path != test.path {
							t.Errorf("path=%s want=%s", r.URL.Path, test.path)
							http.NotFound(w, r)
							return
						}
						var body map[string]any
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
							t.Error(err)
						}
						if body["model"] != "unknown-model" || body["stream"] != true {
							t.Errorf("body=%v", body)
						}
						if test.protocol == model.ProtocolOpenAIResponses && (body["input"] == nil || body["messages"] != nil) {
							t.Errorf("Responses body=%v", body)
						}
						if test.protocol == model.ProtocolAnthropic && body["max_tokens"] != float64(8192) {
							t.Errorf("did not use advertised output limit: %v", body)
						}
						w.Header().Set("Content-Type", "text/event-stream")
						for _, event := range test.events {
							fmt.Fprintf(w, "data: %s\n\n", event)
						}
					}))
					defer server.Close()
					key := "fixture-key"
					var reference model.CredentialRef
					if saved {
						key = ""
						reference = model.CredentialRef{Kind: "env", Name: "QCODE_TEST_PROBE_KEY"}
					}
					got, err := ProbeModelConnection(t.Context(), providerID, server.URL, "unknown-model", key, reference, test.protocol)
					if err != nil || !got.Capabilities.ToolCalls || !got.Capabilities.Streaming || len(got.Models) != 1 || calls != 2 {
						t.Fatalf("got=%+v calls=%d err=%v", got, calls, err)
					}
				})
			}
		})
	}
}
