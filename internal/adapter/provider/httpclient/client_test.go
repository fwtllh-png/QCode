package httpclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"syscall"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/provider/openai"
	providerwire "github.com/fwtllh-png/QCode/internal/adapter/provider/wire"
	"github.com/fwtllh-png/QCode/internal/common/tracecontext"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	"github.com/fwtllh-png/QCode/internal/security/egress"
)

func TestClientOpenAIRequestAndStream(t *testing.T) {
	var requestBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/chat/completions" {
			t.Errorf("path = %q", request.URL.Path)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q", got)
		}
		if err := json.NewDecoder(request.Body).Decode(&requestBody); err != nil {
			t.Error(err)
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"},\"finish_reason\":null}]}\n\n")
		_, _ = io.WriteString(writer, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1}}\n\n")
		_, _ = io.WriteString(writer, "data: [DONE]\n\n")
	}))
	defer server.Close()

	client := testClient()
	client.Credentials = staticCredentials("test-key")
	request := testRequest(t, server.URL, model.ProtocolOpenAIChat)
	request.NativeSearch = true
	request.Tools = []provider.ToolDefinition{{
		Name: "read", Description: "read a file",
		InputSchema: map[string]any{"type": "object"},
	}}
	stream, err := client.Stream(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	events, err := provider.Drain(stream)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 || events[1].Text != "hello" || events[2].Usage.OutputTokens != 1 {
		t.Fatalf("events = %+v", events)
	}
	if requestBody["model"] != "wire-model" || requestBody["stream"] != true {
		t.Fatalf("request body = %+v", requestBody)
	}
	tools, exists := requestBody["tools"].([]any)
	if !exists || len(tools) != 2 {
		t.Fatalf("native search missing from request: %+v", requestBody)
	}
}

func TestClientPropagatesW3CTraceContext(t *testing.T) {
	var propagated tracecontext.Link
	server := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		extracted, err := tracecontext.ExtractHTTP(
			context.Background(),
			request.Header,
		)
		if err != nil {
			t.Errorf("extract trace context: %v", err)
		} else {
			propagated, _ = tracecontext.Current(extracted)
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(
			writer,
			"data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},"+
				"\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n",
		)
	}))
	defer server.Close()
	ctx, err := tracecontext.NewRoot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want, _ := tracecontext.Current(ctx)
	stream, err := testClient().Stream(
		ctx,
		testRequest(t, server.URL, model.ProtocolOpenAIChat),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Drain(stream); err != nil {
		t.Fatal(err)
	}
	if propagated.TraceID != want.TraceID ||
		propagated.SpanID != want.SpanID {
		t.Fatalf("want=%+v propagated=%+v", want, propagated)
	}
}

func TestClientOpenAIResponsesRequest(t *testing.T) {
	var requestBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/responses" {
			t.Errorf("path = %q", request.URL.Path)
		}
		if err := json.NewDecoder(request.Body).Decode(&requestBody); err != nil {
			t.Error(err)
		}
		_, _ = io.WriteString(
			writer,
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n"+
				"data: {\"type\":\"response.completed\",\"response\":{\"usage\":"+
				"{\"input_tokens\":1,\"output_tokens\":1}}}\n\n",
		)
	}))
	defer server.Close()

	client := testClient()
	req := testRequest(t, server.URL, model.ProtocolOpenAIResponses)
	req.Tools = []provider.ToolDefinition{{
		Name: "echo", Description: "echo", InputSchema: map[string]any{"type": "object"},
	}}
	req.NativeSearch = true
	stream, err := client.Stream(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Drain(stream); err != nil {
		t.Fatal(err)
	}
	if requestBody["max_output_tokens"] != float64(128) {
		t.Fatalf("request body = %+v", requestBody)
	}
	tools, _ := requestBody["tools"].([]any)
	if len(tools) < 2 {
		t.Fatalf("tools = %#v", requestBody["tools"])
	}
	fn, _ := tools[0].(map[string]any)
	if fn["type"] != "function" || fn["name"] != "echo" || fn["function"] != nil {
		t.Fatalf("responses function tool must be flat: %#v", fn)
	}
	search, _ := tools[1].(map[string]any)
	if search["type"] != "web_search" {
		t.Fatalf("native search tool = %#v", search)
	}
}

func TestClientClassifiesTransportErrors(t *testing.T) {
	tests := map[string]struct {
		err       error
		retryable bool
	}{
		"connection reset": {fmt.Errorf("write: %w", syscall.ECONNRESET), true},
		"temporary DNS":    {&net.DNSError{Err: "temporary", Name: "provider", IsTemporary: true}, true},
		"missing DNS":      {&net.DNSError{Err: "not found", Name: "provider", IsNotFound: true}, false},
		"TLS certificate": {
			&tls.CertificateVerificationError{
				UnverifiedCertificates: []*x509.Certificate{{}},
				Err:                    x509.UnknownAuthorityError{Cert: &x509.Certificate{}},
			},
			false,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			wrapped := &url.Error{Op: "Post", URL: "https://provider.test", Err: test.err}
			if got := retryableTransportError(wrapped); got != test.retryable {
				t.Fatalf("retryableTransportError(%v) = %t, want %t", wrapped, got, test.retryable)
			}
		})
	}
}

func TestClientHonorsContextDeadlineBeforeHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		time.Sleep(100 * time.Millisecond)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	client := testClient()

	_, err := client.Stream(ctx, testRequest(t, server.URL, model.ProtocolOpenAIChat))

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stream() error = %v, want deadline exceeded", err)
	}
}

func testClient() *Client {
	client := New()
	client.Credentials = staticCredentials("")
	client.Egress = egress.NewStaticGate()
	client.Egress.SetRuntimeApprover(func(context.Context, egress.Target) error { return nil })
	return client
}

func testRequest(t *testing.T, endpoint string, wireProtocol model.WireProtocol) provider.ModelRequest {
	t.Helper()
	return testRequestWithPromptCache(t, endpoint, wireProtocol, false)
}

func testRequestWithPromptCache(
	t *testing.T, endpoint string, wireProtocol model.WireProtocol, promptCache bool,
) provider.ModelRequest {
	t.Helper()
	adapter := model.AdapterOpenAICompatible
	catalog, err := model.NewCatalog(model.Provider{
		ID:         "fixture",
		Adapter:    adapter,
		Endpoint:   endpoint,
		Protocol:   wireProtocol,
		Credential: model.CredentialRef{Kind: "env", Name: "FIXTURE_API_KEY"},
		Provenance: model.ProvenanceFixture,
		Models: map[string]model.Model{
			"fixture-model": {
				ID:          "fixture-model",
				CanonicalID: "fixture-model",
				WireID:      "wire-model",
				Limits:      model.Limits{ContextTokens: 8192, MaxOutputTokens: 4096},
				Capabilities: model.Capabilities{
					Streaming: true, Reasoning: true, ToolCalls: true, NativeSearch: true,
					PromptCache: promptCache,
				},
				Pricing:    model.Pricing{Currency: "USD", Provenance: model.ProvenanceFixture},
				Provenance: model.ProvenanceFixture,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := model.NewResolver(catalog)
	if err != nil {
		t.Fatal(err)
	}
	route, err := resolver.Resolve(model.RouteRequest{
		ProviderID: "fixture", ModelID: "fixture-model", Provenance: model.ProvenanceFixture,
	})
	if err != nil {
		t.Fatal(err)
	}
	return provider.ModelRequest{
		Route:           route,
		Messages:        []provider.Message{provider.TextMessage(provider.RoleUser, "hello")},
		MaxOutputTokens: 128,
		Idempotent:      true,
	}
}

type staticCredentials string

func (s staticCredentials) Resolve(context.Context, model.CredentialRef) (string, error) {
	return string(s), nil
}

var _ CredentialResolver = staticCredentials("")

func (c *Client) Stream(
	ctx context.Context,
	request provider.ModelRequest,
) (provider.Stream, error) {
	adapter, err := testAdapter(request.Route.Adapter())
	if err != nil {
		return nil, err
	}
	call, err := adapter.Prepare(request)
	if err != nil {
		return nil, protocol.NewProblem(
			protocol.CodeInvalidArgument, err.Error(), false, err,
		)
	}
	if sessionAdapter, ok := adapter.(providerwire.SessionAdapter); ok {
		stream, handled, err := sessionAdapter.TrySession(
			ctx, request, call, c,
		)
		if handled || err != nil {
			return stream, err
		}
	}
	return c.Execute(ctx, request, call, adapter)
}

func testAdapter(id model.AdapterID) (providerwire.Adapter, error) {
	adapter, err := openai.NewAdapter(id)
	if err != nil {
		return nil, fmt.Errorf("test adapter: %w", err)
	}
	return adapter, nil
}
