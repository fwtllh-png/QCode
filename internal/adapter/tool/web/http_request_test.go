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

func TestHTTPRequestSendsStructuredRequestAndAssertsStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.Header.Get("X-Trace") != "fixture" {
			http.Error(writer, "bad request", http.StatusBadRequest)
			return
		}
		writer.Header().Set("Set-Cookie", "secret=value")
		writer.Header().Set("X-Result", "ok")
		writer.WriteHeader(http.StatusCreated)
		_, _ = writer.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	registry := tool.NewRegistry(nil, nil)
	if err := RegisterWithOptions(registry, Options{HTTP: server.Client()}); err != nil {
		t.Fatal(err)
	}
	result, err := tooltest.Execute(t.Context(), registry, tool.Call{
		Name: "http_request",
		Arguments: json.RawMessage(`{
			"url":` + strconvQuote(server.URL) + `,
			"method":"POST",
			"headers":{"Content-Type":"application/json","X-Trace":"fixture"},
			"body":"{\"value\":1}",
			"expect_status":[201]
		}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError || !strings.Contains(result.Content, `"matched_expectation":true`) ||
		!strings.Contains(result.Content, `"X-Result"`) ||
		strings.Contains(result.Content, "secret=value") {
		t.Fatalf("http_request result = %+v", result)
	}
}

func TestHTTPRequestRejectsPersistedCredentialHeaders(t *testing.T) {
	registry := tool.NewRegistry(nil, nil)
	if err := RegisterWithOptions(registry, Options{}); err != nil {
		t.Fatal(err)
	}
	_, err := tooltest.Execute(t.Context(), registry, tool.Call{
		Name: "http_request",
		Arguments: json.RawMessage(`{
			"url":"https://example.com",
			"method":"GET",
			"headers":{"Authorization":"Bearer secret"}
		}`),
	})
	if err == nil || !strings.Contains(err.Error(), "managed connector") {
		t.Fatalf("credential header error = %v", err)
	}
}

func TestInteractiveWebToolsRequireIrreversibleApproval(t *testing.T) {
	for name, binding := range map[string]tool.TrustedBinding{
		"http_request": (&Tool{kind: "http_request"}).TrustedBinding(),
		"web_run":      (&browserTool{runtime: NewFakeBrowser()}).TrustedBinding(),
	} {
		if binding.Effect.Kind != tool.EffectExternalMutation ||
			binding.Effect.Reversibility != tool.Irreversible ||
			binding.Effect.Approval != tool.ApprovalPolicyOnce {
			t.Fatalf("%s binding = %+v", name, binding)
		}
	}
}

func strconvQuote(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}
