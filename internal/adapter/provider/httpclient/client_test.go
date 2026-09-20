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
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/observability/tracecontext"
	sessionhistory "github.com/fwtllh-png/QCode/internal/persist/history"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
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

// TestBundledResponsesCatalogEntryEncodesToTheResponsesPath is the T5
// acceptance: a route taken from the bundled catalog, with no custom endpoint
// metadata, still produces a /responses body rather than /chat/completions.
func TestBundledResponsesCatalogEntryEncodesToTheResponsesPath(t *testing.T) {
	resolver, err := model.NewResolver(model.DefaultCatalog())
	if err != nil {
		t.Fatal(err)
	}
	route, err := resolver.Resolve(model.RouteRequest{
		ProviderID: "openai-responses", ModelID: "gpt-4.1",
	})
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	path := mustEncodeRequest(t, provider.ModelRequest{
		Route: route,
		Messages: []provider.Message{
			provider.TextMessage(provider.RoleUser, "hello"),
		},
		MaxOutputTokens: 128,
		PromptCacheKey:  "session-1",
		Idempotent:      true,
	}, &body)
	if path != "/responses" {
		t.Fatalf("path = %q, want /responses", path)
	}
	if body["model"] != "gpt-4.1" {
		t.Fatalf("model = %#v", body["model"])
	}
	if body["prompt_cache_key"] != "session-1" {
		t.Fatalf("prompt_cache_key missing: %#v", body)
	}
	if _, ok := body["messages"]; ok {
		t.Fatalf("Responses body must not use chat messages: %#v", body)
	}
	if _, ok := body["input"]; !ok {
		t.Fatalf("Responses body missing input: %#v", body)
	}
}

func TestEncodeOmitsPromptCacheKeyWithoutCapability(t *testing.T) {
	request := testRequest(t, "https://provider.test", model.ProtocolOpenAIResponses)
	request.PromptCacheKey = "session-1"
	var body map[string]any
	mustEncodeRequest(t, request, &body)
	if _, ok := body["prompt_cache_key"]; ok {
		t.Fatalf("prompt_cache_key should be omitted without capability: %#v", body)
	}
}

func TestEncodeChatPromptCacheKeyWithCapability(t *testing.T) {
	withCache := testRequestWithPromptCache(t, "https://provider.test", model.ProtocolOpenAIChat, true)
	withCache.PromptCacheKey = "session-chat"
	var body map[string]any
	path := mustEncodeRequest(t, withCache, &body)
	if path != "/chat/completions" {
		t.Fatalf("path = %q", path)
	}
	if body["prompt_cache_key"] != "session-chat" {
		t.Fatalf("prompt_cache_key missing: %#v", body)
	}

	without := testRequestWithPromptCache(t, "https://provider.test", model.ProtocolOpenAIChat, false)
	without.PromptCacheKey = "session-chat"
	var withoutBody map[string]any
	mustEncodeRequest(t, without, &withoutBody)
	if _, ok := withoutBody["prompt_cache_key"]; ok {
		t.Fatalf("prompt_cache_key should be omitted without capability: %#v", withoutBody)
	}
}

func TestEncodeToolHistoryByProtocol(t *testing.T) {
	messages := []provider.Message{
		provider.TextMessage(provider.RoleUser, "read"),
		{Role: provider.RoleAssistant, Blocks: []provider.ContentBlock{
			{Type: provider.ContentText, Text: "checking"},
			{Type: provider.ContentToolCall, ToolCall: &provider.ToolCall{
				ID: "call_1", Name: "read", Arguments: `{"path":"a.txt"}`,
			}},
		}},
		{Role: provider.RoleTool, Blocks: []provider.ContentBlock{{
			Type:       provider.ContentToolResult,
			ToolResult: &provider.ToolResult{CallID: "call_1", Content: `{"content":"hello"}`},
		}}},
	}
	t.Run("responses", func(t *testing.T) {
		request := testRequest(t, "https://provider.test", model.ProtocolOpenAIResponses)
		request.Messages = messages
		var body struct {
			Input []map[string]any `json:"input"`
		}
		mustEncodeRequest(t, request, &body)
		if len(body.Input) != 4 ||
			body.Input[1]["role"] != "assistant" ||
			body.Input[2]["type"] != "function_call" ||
			body.Input[2]["call_id"] != "call_1" ||
			body.Input[3]["type"] != "function_call_output" ||
			body.Input[3]["call_id"] != "call_1" {
			t.Fatalf("input = %#v", body.Input)
		}
	})
}

// TestEncodeImageByProtocol pins the wire shape of an image in all three
// protocols. The shapes differ enough that a single mistake is invisible until a
// provider answers 400 about a field the caller never named.
func TestEncodeImageByProtocol(t *testing.T) {
	// "PNG" as bytes, so the base64 in the assertions is readable rather than a
	// wall of pixels.
	imaged := []provider.Message{{
		Role: provider.RoleUser,
		Blocks: []provider.ContentBlock{
			{Type: provider.ContentText, Text: "what is this"},
			{Type: provider.ContentImage, Attachment: &provider.Attachment{
				MediaType: "image/png", Data: []byte("PNG"), Name: "shot.png",
			}},
		},
	}}
	const dataURL = "data:image/png;base64,UE5H"

	t.Run("chat completions", func(t *testing.T) {
		request := testRequest(t, "https://provider.test", model.ProtocolOpenAIChat)
		request.Messages = imaged
		var body struct {
			Messages []map[string]any `json:"messages"`
		}
		mustEncodeRequest(t, request, &body)
		content, _ := body.Messages[0]["content"].([]any)
		if len(content) != 2 {
			t.Fatalf("content = %#v", body.Messages[0]["content"])
		}
		text, _ := content[0].(map[string]any)
		image, _ := content[1].(map[string]any)
		url, _ := image["image_url"].(map[string]any)
		if text["type"] != "text" || text["text"] != "what is this" {
			t.Fatalf("text part = %#v", text)
		}
		if image["type"] != "image_url" || url["url"] != dataURL {
			t.Fatalf("image part = %#v", image)
		}
	})

	// A text-only message must keep the plain string content it always had:
	// every request body is a prompt cache key, so switching the shape for all
	// traffic would invalidate every cached prefix.
	t.Run("chat completions without an image keeps string content", func(t *testing.T) {
		request := testRequest(t, "https://provider.test", model.ProtocolOpenAIChat)
		request.Messages = []provider.Message{provider.TextMessage(provider.RoleUser, "plain")}
		var body struct {
			Messages []map[string]any `json:"messages"`
		}
		mustEncodeRequest(t, request, &body)
		if body.Messages[0]["content"] != "plain" {
			t.Fatalf("content = %#v", body.Messages[0]["content"])
		}
	})

	t.Run("responses", func(t *testing.T) {
		request := testRequest(t, "https://provider.test", model.ProtocolOpenAIResponses)
		request.Messages = imaged
		var body struct {
			Input []map[string]any `json:"input"`
		}
		mustEncodeRequest(t, request, &body)
		// The image and the question that asks about it have to arrive as one
		// input item, or the model is shown a picture and asked about nothing.
		if len(body.Input) != 1 {
			t.Fatalf("input = %#v", body.Input)
		}
		content, _ := body.Input[0]["content"].([]any)
		text, _ := content[0].(map[string]any)
		image, _ := content[1].(map[string]any)
		if text["type"] != "input_text" || text["text"] != "what is this" {
			t.Fatalf("text part = %#v", text)
		}
		if image["type"] != "input_image" || image["image_url"] != dataURL {
			t.Fatalf("image part = %#v", image)
		}
	})

}

func TestEncodeReasoningReplayByProtocol(t *testing.T) {

	t.Run("responses encrypted reasoning becomes neutral plaintext", func(t *testing.T) {
		raw := json.RawMessage(`{"type":"reasoning","id":"rs_1","encrypted_content":"ciphertext","summary":[]}`)
		request := testRequest(t, "https://provider.test", model.ProtocolOpenAIResponses)
		request.Messages = []provider.Message{
			provider.TextMessage(provider.RoleUser, "first"),
			provider.ProducedAssistant(
				request.Route,
				[]provider.ContentBlock{{
					Type: provider.ContentReasoning, ID: "rs_1", Text: "inspect",
				}},
				1,
				responsesReplayState(raw),
			),
			provider.TextMessage(provider.RoleUser, "second"),
		}
		var body struct {
			Input []map[string]any `json:"input"`
		}
		mustEncodeRequest(t, request, &body)
		if len(body.Input) != 3 || body.Input[1]["type"] != "reasoning" ||
			body.Input[2]["role"] != "user" {
			t.Fatalf("reasoning replay = %#v", body.Input)
		}
		if _, exists := body.Input[1]["encrypted_content"]; exists {
			t.Fatalf("encrypted replay leaked: %#v", body.Input[1])
		}
	})

	t.Run("responses drops empty reasoning shells", func(t *testing.T) {
		raw := json.RawMessage(`{"type":"reasoning","id":"rs_empty","content":[],"summary":[]}`)
		request := testRequest(t, "https://provider.test", model.ProtocolOpenAIResponses)
		request.Messages = []provider.Message{
			provider.TextMessage(provider.RoleUser, "q"),
			provider.ProducedAssistant(
				request.Route,
				[]provider.ContentBlock{
					{Type: provider.ContentToolCall, ToolCall: &provider.ToolCall{ID: "c1", Name: "echo", Arguments: `{}`}},
				},
				1,
				responsesReplayState(raw),
			),
		}
		var body struct {
			Input []map[string]any `json:"input"`
		}
		mustEncodeRequest(t, request, &body)
		if len(body.Input) != 2 ||
			body.Input[1]["type"] != "function_call" {
			t.Fatalf("empty reasoning shell was retained: %#v", body.Input)
		}
	})

	t.Run("responses keeps orphan tool call without synthetic reasoning", func(t *testing.T) {
		request := testRequest(t, "https://provider.test", model.ProtocolOpenAIResponses)
		request.Messages = []provider.Message{
			provider.TextMessage(provider.RoleUser, "q"),
			{Role: provider.RoleAssistant, Blocks: []provider.ContentBlock{
				{Type: provider.ContentReasoning, Text: "first"},
				{Type: provider.ContentToolCall, ToolCall: &provider.ToolCall{ID: "c1", Name: "echo", Arguments: `{}`}},
			}},
			{Role: provider.RoleTool, Blocks: []provider.ContentBlock{{
				Type: provider.ContentToolResult, ToolResult: &provider.ToolResult{CallID: "c1", Content: "ok"},
			}}},
			// Tool-only step: no reasoning captured (common when stream only
			// emitted encrypted/empty reasoning that we drop).
			{Role: provider.RoleAssistant, Blocks: []provider.ContentBlock{
				{Type: provider.ContentToolCall, ToolCall: &provider.ToolCall{ID: "c2", Name: "file_read", Arguments: `{}`}},
			}},
		}
		var body struct {
			Input []map[string]any `json:"input"`
		}
		mustEncodeRequest(t, request, &body)
		var sawCall bool
		for i, item := range body.Input {
			if item["type"] != "function_call" || item["call_id"] != "c2" {
				continue
			}
			if i > 0 && body.Input[i-1]["type"] == "reasoning" {
				t.Fatalf("synthetic reasoning inserted before c2: %#v", body.Input)
			}
			sawCall = true
		}
		if !sawCall {
			t.Fatalf("c2 not found in %#v", body.Input)
		}
	})

	t.Run("responses rejects orphan tool output before transport", func(t *testing.T) {
		request := testRequest(t, "https://provider.test", model.ProtocolOpenAIResponses)
		request.Messages = []provider.Message{
			provider.TextMessage(provider.RoleUser, "q"),
			{Role: provider.RoleTool, Blocks: []provider.ContentBlock{{
				Type: provider.ContentToolResult,
				ToolResult: &provider.ToolResult{
					CallID: "orphan", Content: "must not be sent",
				},
			}}},
		}
		if _, _, err := encodeRequest(request); err == nil ||
			!strings.Contains(err.Error(), "no preceding function_call") {
			t.Fatalf("encodeRequest() error = %v", err)
		}
	})

	t.Run("responses encodes paired history reconstructed after restart", func(t *testing.T) {
		events := []protocol.Event{
			{
				Kind: protocol.EventTurnStarted, ThreadID: "thread-1", TurnID: "turn-1", Sequence: 1,
				Data: &protocol.TurnStartedData{Provider: "p", Model: "m", Prompt: "inspect"},
			},
			{
				Kind: protocol.EventToolStart, ThreadID: "thread-1", TurnID: "turn-1", Sequence: 2,
				Data: &protocol.ToolStartData{
					Tool: "exec_command", CallID: "call-1",
					Arguments: []byte(`{"command":"wc -l"}`),
				},
			},
			{
				Kind: protocol.EventToolResult, ThreadID: "thread-1", TurnID: "turn-1", Sequence: 3,
				Data: &protocol.ToolResultData{
					Tool: "exec_command", CallID: "call-1", Output: "42",
				},
			},
			{
				Kind: protocol.EventTurnCompleted, ThreadID: "thread-1", TurnID: "turn-1", Sequence: 4,
				Data: &protocol.TurnCompletedData{Text: "done"},
			},
		}
		reconstructed, err := sessionhistory.ReconstructThread(events, "thread-1")
		if err != nil {
			t.Fatal(err)
		}
		request := testRequest(t, "https://provider.test", model.ProtocolOpenAIResponses)
		request.Messages = append(
			reconstructed.History,
			provider.TextMessage(provider.RoleUser, "continue"),
		)
		var body struct {
			Input []map[string]any `json:"input"`
		}
		mustEncodeRequest(t, request, &body)
		callIndex, outputIndex := -1, -1
		for index, item := range body.Input {
			if item["type"] == "function_call" && item["call_id"] == "call-1" {
				callIndex = index
			}
			if item["type"] == "function_call_output" && item["call_id"] == "call-1" {
				outputIndex = index
			}
		}
		if callIndex < 0 || outputIndex <= callIndex {
			t.Fatalf("reconstructed tool pair is invalid: %#v", body.Input)
		}
	})

	t.Run("responses extracts reasoning_text from replay state", func(t *testing.T) {
		raw := json.RawMessage(`{"type":"reasoning","id":"rs_2","content":[{"type":"reasoning_text","text":"from item"}],"summary":[]}`)
		request := testRequest(t, "https://provider.test", model.ProtocolOpenAIResponses)
		request.Messages = []provider.Message{
			provider.TextMessage(provider.RoleUser, "q"),
			provider.ProducedAssistant(
				request.Route,
				[]provider.ContentBlock{{
					Type: provider.ContentReasoning, ID: "rs_2", Text: "from item",
				}},
				1,
				responsesReplayState(raw),
			),
		}
		var body struct {
			Input []map[string]any `json:"input"`
		}
		mustEncodeRequest(t, request, &body)
		content, _ := body.Input[1]["content"].([]any)
		part, _ := content[0].(map[string]any)
		if body.Input[1]["type"] != "reasoning" || part["text"] != "from item" {
			t.Fatalf("input=%#v", body.Input)
		}
	})

	t.Run("responses plaintext reasoning_text for tool loop", func(t *testing.T) {
		request := testRequest(t, "https://provider.test", model.ProtocolOpenAIResponses)
		request.Messages = []provider.Message{
			provider.TextMessage(provider.RoleUser, "weather?"),
			{Role: provider.RoleAssistant, Blocks: []provider.ContentBlock{
				{Type: provider.ContentReasoning, Text: "need a tool"},
				{Type: provider.ContentToolCall, ToolCall: &provider.ToolCall{
					ID: "call_1", Name: "get_weather", Arguments: `{"city":"HZ"}`,
				}},
			}},
			{Role: provider.RoleTool, Blocks: []provider.ContentBlock{{
				Type:       provider.ContentToolResult,
				ToolResult: &provider.ToolResult{CallID: "call_1", Content: "cloudy"},
			}}},
		}
		var body struct {
			Input []map[string]any `json:"input"`
		}
		mustEncodeRequest(t, request, &body)
		if len(body.Input) < 3 || body.Input[1]["type"] != "reasoning" {
			t.Fatalf("input = %#v", body.Input)
		}
		content, _ := body.Input[1]["content"].([]any)
		if len(content) != 1 {
			t.Fatalf("reasoning content = %#v", body.Input[1]["content"])
		}
		part, _ := content[0].(map[string]any)
		if part["type"] != "reasoning_text" || part["text"] != "need a tool" {
			t.Fatalf("reasoning part = %#v", part)
		}
		if _, hasCipher := body.Input[1]["encrypted_content"]; hasCipher {
			t.Fatalf("plaintext replay must not keep encrypted_content: %#v", body.Input[1])
		}
	})

	t.Run("responses opaque reasoning prefers plaintext content", func(t *testing.T) {
		raw := json.RawMessage(`{"type":"reasoning","id":"rs_2","encrypted_content":"ciphertext","summary":[]}`)
		request := testRequest(t, "https://provider.test", model.ProtocolOpenAIResponses)
		request.Messages = []provider.Message{
			provider.TextMessage(provider.RoleUser, "first"),
			provider.ProducedAssistant(
				request.Route,
				[]provider.ContentBlock{{
					Type: provider.ContentReasoning, ID: "rs_2", Text: "visible chain",
				}},
				1,
				responsesReplayState(raw),
			),
		}
		var body struct {
			Input []map[string]any `json:"input"`
		}
		mustEncodeRequest(t, request, &body)
		item := body.Input[1]
		if item["type"] != "reasoning" || item["id"] != nil {
			t.Fatalf("item = %#v", item)
		}
		if _, hasCipher := item["encrypted_content"]; hasCipher {
			t.Fatalf("expected encrypted_content dropped when plaintext present: %#v", item)
		}
		content, _ := item["content"].([]any)
		part, _ := content[0].(map[string]any)
		if part["type"] != "reasoning_text" || part["text"] != "visible chain" {
			t.Fatalf("content = %#v", item["content"])
		}
	})
}

func TestClientMakesOneAttemptForRetryableHTTPStatusMatrix(t *testing.T) {
	statuses := []int{
		http.StatusRequestTimeout,
		http.StatusTooEarly,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
	}
	for _, status := range statuses {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var attempts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(
				writer http.ResponseWriter,
				_ *http.Request,
			) {
				attempts.Add(1)
				writer.WriteHeader(status)
			}))
			defer server.Close()
			client := testClient()
			_, err := client.Stream(
				t.Context(),
				testRequest(t, server.URL, model.ProtocolOpenAIChat),
			)
			var problem *protocol.Problem
			if !errors.As(err, &problem) || !problem.Retryable {
				t.Fatalf("error = %v, want retryable problem", err)
			}
			if attempts.Load() != 1 {
				t.Fatalf("attempts = %d, want 1", attempts.Load())
			}
		})
	}
}

func TestClientRetainsRetryAfterMetadataWithoutSleeping(t *testing.T) {
	var attempts atomic.Int32
	var keys []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		attempts.Add(1)
		keys = append(keys, request.Header.Get("Idempotency-Key"))
		writer.Header().Set("Retry-After", "60")
		writer.Header().Set("RateLimit-Limit", "100")
		writer.Header().Set("RateLimit-Remaining", "0")
		writer.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	client := testClient()
	_, err := client.Stream(t.Context(), testRequest(t, server.URL, model.ProtocolOpenAIChat))

	var problem *protocol.Problem
	if !errors.As(err, &problem) {
		t.Fatalf("error = %v", err)
	}
	if problem.HTTPStatus != http.StatusTooManyRequests ||
		problem.RateLimit == nil ||
		problem.RateLimit.Limit != "100" ||
		problem.RateLimit.Remaining != "0" ||
		problem.RateLimit.RetryAfterMS != 60000 {
		t.Fatalf("problem = %+v", problem)
	}
	if attempts.Load() != 1 || len(keys) != 1 || keys[0] == "" {
		t.Fatalf("idempotency keys = %v", keys)
	}
}

func TestClientDerivesSharedCooldownFromRateLimitedRequest(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		_ *http.Request,
	) {
		attempts.Add(1)
		time.Sleep(20 * time.Millisecond)
		writer.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	client := testClient()
	request := testRequest(t, server.URL, model.ProtocolOpenAIChat)
	_, err := client.Stream(t.Context(), request)
	var problem *protocol.Problem
	var failure *provider.Failure
	if !errors.As(err, &problem) ||
		!errors.As(err, &failure) ||
		problem.RateLimit != nil ||
		failure.RetryAfterMS != 0 {
		t.Fatalf("rate limit error = %#v, failure = %#v", problem, failure)
	}

	ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
	defer cancel()
	if _, err := client.Stream(ctx, request); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cooldown error = %v, want deadline exceeded", err)
	}
	if attempts.Load() != 1 {
		t.Fatalf("provider attempts = %d, want shared cooldown before retry", attempts.Load())
	}
	if cooldown := client.RouteCooldown(request.Route); cooldown <= 0 {
		t.Fatalf("route cooldown = %s, want remaining governor wait", cooldown)
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
			var attempts atomic.Int32
			client := testClient()
			client.HTTP = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				attempts.Add(1)
				return nil, test.err
			})}

			_, err := client.Stream(t.Context(), testRequest(t, "https://provider.test", model.ProtocolOpenAIChat))
			var problem *protocol.Problem
			if !errors.As(err, &problem) ||
				problem.Retryable != test.retryable ||
				attempts.Load() != 1 {
				t.Fatalf("error=%v attempts=%d", err, attempts.Load())
			}
		})
	}
}

func TestClientDoesNotRetryPermanentClientError(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		writer.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(writer, "invalid model")
	}))
	defer server.Close()

	client := testClient()
	_, err := client.Stream(t.Context(), testRequest(t, server.URL, model.ProtocolOpenAIChat))
	if !protocol.IsCode(err, protocol.CodeInvalidArgument) {
		t.Fatalf("error = %v, code = %q", err, protocol.CodeOf(err))
	}
	if attempts.Load() != 1 {
		t.Fatalf("attempts = %d, want 1", attempts.Load())
	}
}

func TestClientCancellationClosesResponse(t *testing.T) {
	requestCanceled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		if flusher, ok := writer.(http.Flusher); ok {
			flusher.Flush()
		}
		<-request.Context().Done()
		close(requestCanceled)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	client := testClient()
	stream, err := client.Stream(ctx, testRequest(t, server.URL, model.ProtocolOpenAIChat))
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	_, err = stream.Recv()
	if err != nil {
		t.Fatalf("first event error = %v", err)
	}
	_, err = stream.Recv()
	if err == nil {
		t.Fatal("Recv() error = nil after cancellation")
	}
	select {
	case <-requestCanceled:
	case <-time.After(time.Second):
		t.Fatal("server request context was not canceled")
	}
	_ = stream.Close()
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

func TestConnectionTimeoutDoesNotCapProgressingStreamLifetime(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		_ *http.Request,
	) {
		writer.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := writer.(http.Flusher)
		for _, text := range []string{"one", "two", "three"} {
			_, _ = fmt.Fprintf(
				writer,
				"data: {\"choices\":[{\"delta\":{\"content\":%q},\"finish_reason\":null}]}\n\n",
				text,
			)
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(20 * time.Millisecond)
		}
		_, _ = io.WriteString(writer, "data: [DONE]\n\n")
	}))
	defer server.Close()

	client := testClient()
	client.SetConnectionTimeout(10 * time.Millisecond)
	client.IdleTimeout = 100 * time.Millisecond
	stream, err := client.Stream(
		t.Context(),
		testRequest(t, server.URL, model.ProtocolOpenAIChat),
	)
	if err != nil {
		t.Fatal(err)
	}
	events, err := provider.Drain(stream)
	if err != nil {
		t.Fatal(err)
	}
	var text string
	for _, event := range events {
		text += event.Text
	}
	if text != "onetwothree" {
		t.Fatalf("stream text = %q", text)
	}
}

func TestProviderRequestDeadlineReportsResponseHeaderScope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(
		http.ResponseWriter,
		*http.Request,
	) {
		time.Sleep(100 * time.Millisecond)
	}))
	defer server.Close()

	client := testClient()
	client.SetDeadlineConfig(DeadlineConfig{
		Connection:      time.Second,
		TLSHandshake:    time.Second,
		ResponseHeaders: 10 * time.Millisecond,
	})
	request := testRequest(t, server.URL, model.ProtocolOpenAIChat)
	request.LogicalRequestID = "sample-header-timeout"
	_, err := client.Stream(t.Context(), request)
	if !protocol.IsCode(err, protocol.CodeDeadlineExceeded) {
		t.Fatalf("Stream() error = %v", err)
	}
	problem := protocol.ProblemOf(err)
	if problem.Fault == nil ||
		problem.Fault.Stage != protocol.FaultStageResponseHeaders ||
		problem.Fault.OperationID != "sample-header-timeout" ||
		problem.Fault.RetryOwner != protocol.FaultRetryOwnerEngine ||
		problem.Fault.Deadline == nil ||
		problem.Fault.Deadline.Scope !=
			protocol.DeadlineProviderResponseHeaders ||
		problem.Fault.Deadline.TimeoutMS != 10 {
		t.Fatalf("response header fault = %+v", problem.Fault)
	}
}

func TestClientDoesNotReplayAfterStreamStarts(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: {\n\n")
	}))
	defer server.Close()

	client := testClient()
	stream, err := client.Stream(t.Context(), testRequest(t, server.URL, model.ProtocolOpenAIChat))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Drain(stream); err == nil {
		t.Fatal("Drain() error = nil, want malformed stream error")
	}
	if attempts.Load() != 1 {
		t.Fatalf("attempts = %d, want no replay after stream start", attempts.Load())
	}
}

func TestNormalizeStreamErrorClassifiesConnectionReset(t *testing.T) {
	err := normalizeStreamError(
		fmt.Errorf("read stream: %w", syscall.ECONNRESET),
		provider.TransportMetadata{LogicalRequestID: "sample-1"},
	)
	if !protocol.IsCode(err, protocol.CodeUnavailable) ||
		!protocol.IsRetryable(err) ||
		!errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("normalizeStreamError() = %#v", err)
	}
	problem := protocol.ProblemOf(err)
	if problem.Fault == nil ||
		problem.Fault.Stage != protocol.FaultStageModelSample ||
		problem.Fault.OperationID != "sample-1" ||
		problem.Fault.RetryOwner != protocol.FaultRetryOwnerEngine {
		t.Fatalf("fault = %+v", problem.Fault)
	}
}

func TestClientStreamIdleTimeoutAndNonIdempotentRetry(t *testing.T) {
	t.Run("idle timeout", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			writer.Header().Set("Content-Type", "text/event-stream")
			writer.WriteHeader(http.StatusOK)
			if flusher, ok := writer.(http.Flusher); ok {
				flusher.Flush()
			}
			<-request.Context().Done()
		}))
		defer server.Close()
		client := testClient()
		client.IdleTimeout = 10 * time.Millisecond
		stream, err := client.Stream(t.Context(), testRequest(t, server.URL, model.ProtocolOpenAIChat))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := stream.Recv(); err != nil {
			t.Fatal(err)
		}
		if _, err := stream.Recv(); !protocol.IsCode(err, protocol.CodeDeadlineExceeded) {
			t.Fatalf("idle Recv() error = %v", err)
		} else {
			problem := protocol.ProblemOf(err)
			if problem.Fault == nil ||
				problem.Fault.Stage != protocol.FaultStageStreamIdle ||
				problem.Fault.Deadline == nil ||
				problem.Fault.Deadline.Scope != protocol.DeadlineProviderStreamIdle ||
				!problem.Fault.Deadline.Renewable {
				t.Fatalf("idle fault = %+v", problem.Fault)
			}
		}
	})

	t.Run("non idempotent", func(t *testing.T) {
		var attempts atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			attempts.Add(1)
			writer.WriteHeader(http.StatusBadGateway)
		}))
		defer server.Close()
		client := testClient()
		request := testRequest(t, server.URL, model.ProtocolOpenAIChat)
		request.Idempotent = false
		if _, err := client.Stream(t.Context(), request); err == nil {
			t.Fatal("Stream() error = nil")
		}
		if attempts.Load() != 1 {
			t.Fatalf("attempts = %d", attempts.Load())
		}
	})
}

func testClient() *Client {
	client := New()
	client.Credentials = staticCredentials("")
	return client
}

func responsesReplayState(items ...json.RawMessage) *provider.ReplayState {
	data, _ := json.Marshal(struct {
		Items []json.RawMessage `json:"items"`
	}{Items: items})
	return &provider.ReplayState{
		Version: provider.ReplayVersion,
		Data:    data,
	}
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

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}
