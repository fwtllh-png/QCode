package modelcatalog

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider/httpclient"
)

func TestLiveRequestsReportSafeProviderErrors(t *testing.T) {
	const apiKey = "test-credential-sensitive"
	messageBody := func(message string) string {
		t.Helper()
		body, err := json.Marshal(map[string]any{"error": map[string]string{"message": message}})
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	shortBody := messageBody("request rejected")
	atLimit := shortBody + strings.Repeat(" ", httpclient.MaxErrorBodyBytes-len(shortBody))
	for _, test := range []struct {
		name   string
		status int
		body   string
		detail string
	}{
		{
			name: "invalid parameter", status: http.StatusBadRequest,
			body:   messageBody("Parameter 'tool_choice' is not supported for this request."),
			detail: "Parameter 'tool_choice' is not supported for this request.",
		},
		{
			name: "credential reflection", status: http.StatusUnauthorized,
			body:   messageBody("Incorrect API key provided: " + apiKey),
			detail: "Incorrect API key provided: [REDACTED]",
		},
		{
			name: "escaped credential", status: http.StatusUnauthorized,
			body:   `{"error":{"message":"Rejected test\u002dcredential-sensitive"}}`,
			detail: "Rejected [REDACTED]",
		},
		{
			name: "other credentials", status: http.StatusForbidden,
			body:   messageBody("Rejected Bearer other-token; api_key=other-key"),
			detail: "Rejected Bearer [REDACTED]; api_key=[REDACTED]",
		},
		{
			name: "multiline message", status: http.StatusBadRequest,
			body:   messageBody("  Invalid request.\n\tCheck model ID.  "),
			detail: "Invalid request. Check model ID.",
		},
		{
			name: "ignored fields", status: http.StatusBadRequest,
			body:   `{"error":{"message":"request rejected","debug":"` + apiKey + `"}}`,
			detail: "request rejected",
		},
		{name: "empty body", status: http.StatusBadGateway},
		{name: "missing message", status: http.StatusBadRequest, body: `{"error":{"code":"invalid_request"}}`},
		{name: "blank message", status: http.StatusBadRequest, body: messageBody(" \n\t")},
		{name: "malformed JSON", status: http.StatusBadRequest, body: `{"error":{"message":"` + apiKey},
		{name: "HTML body", status: http.StatusNotFound, body: "<html>" + apiKey + "</html>"},
		{name: "plain text body", status: http.StatusBadGateway, body: "Error: " + apiKey},
		{name: "at diagnostic limit", status: http.StatusBadRequest, body: atLimit, detail: "request rejected"},
		{name: "over diagnostic limit", status: http.StatusBadRequest, body: atLimit + " "},
		{
			name: "credential at truncation boundary", status: http.StatusBadRequest,
			body: messageBody(strings.Repeat("x", httpclient.MaxErrorBodyBytes-len(`{"error":{"message":"`)-5) + apiKey),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			for _, operation := range []struct {
				name   string
				prefix string
				call   func() error
			}{
				{"chat", "model capability probe", func() error {
					_, err := ProbeCapabilitiesForProtocol(t.Context(), server.URL, " "+apiKey+" ", "model-a", model.ProtocolOpenAIChat)
					return err
				}},
				{"responses", "model capability probe", func() error {
					_, err := ProbeCapabilitiesForProtocol(t.Context(), server.URL, apiKey, "model-a", model.ProtocolOpenAIResponses)
					return err
				}},
				{"list", "live list", func() error {
					_, err := Discover(t.Context(), "openai-compatible", server.URL, apiKey)
					return err
				}},
			} {
				t.Run(operation.name, func(t *testing.T) {
					want := fmt.Sprintf("%s HTTP %d", operation.prefix, test.status)
					if test.detail != "" {
						want += ": " + test.detail
					}
					err := operation.call()
					if err == nil || err.Error() != want {
						t.Fatalf("error = %v, want %q", err, want)
					}
				})
			}
		})
	}
}

type failingErrorBody struct{}

func (failingErrorBody) Read([]byte) (int, error) { return 0, errors.New("body read failed") }
func (failingErrorBody) Close() error             { return nil }

func TestResponseErrorPreservesStatusWhenBodyReadFails(t *testing.T) {
	err := responseError("model capability probe", &http.Response{
		StatusCode: http.StatusBadRequest,
		Body:       failingErrorBody{},
	}, "")
	if err.Error() != "model capability probe HTTP 400" {
		t.Fatalf("error = %v", err)
	}
}
