package openai

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	providerwire "github.com/fwtllh-png/QCode/internal/adapter/provider/wire"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/contextview"
)

func TestCompatibleChatPreservesReasoningAndOmitsExplicitCacheKey(t *testing.T) {
	adapter := compatibleAdapter(t)
	request := compatibleRequest(t)
	request.PromptCacheKey = "internal-session-key"
	request.ReasoningEffort = "off"
	request.Messages = []provider.Message{
		provider.TextMessage(provider.RoleUser, "inspect"),
		{Role: provider.RoleAssistant, Blocks: []provider.ContentBlock{
			{Type: provider.ContentReasoning, Text: "reasoning"},
			{Type: provider.ContentText, Text: "answer"},
		}},
		{Role: provider.RoleTool, Blocks: []provider.ContentBlock{{
			Type: provider.ContentToolResult,
			ToolResult: &provider.ToolResult{
				CallID: "call_1",
			},
		}}},
		{Role: provider.RoleUser, Blocks: []provider.ContentBlock{{
			Type: provider.ContentImage,
			Attachment: &provider.Attachment{
				MediaType: "image/png",
				Data:      []byte("png"),
			},
		}}},
	}
	call, err := adapter.Prepare(request)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Messages       []map[string]any `json:"messages"`
		PromptCacheKey any              `json:"prompt_cache_key"`
		Reasoning      any              `json:"reasoning_effort"`
		Thinking       map[string]any   `json:"thinking"`
	}
	if err := json.Unmarshal(call.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body.PromptCacheKey != nil || body.Reasoning != nil {
		t.Fatalf("unsupported fields leaked: %s", call.Body)
	}
	if body.Thinking["type"] != "disabled" {
		t.Fatalf("thinking = %#v", body.Thinking)
	}
	if body.Messages[1]["reasoning_content"] != "reasoning" {
		t.Fatalf("reasoning missing: %#v", body.Messages[1])
	}
	if body.Messages[2]["content"] != "(empty tool output)" {
		t.Fatalf("empty tool output = %#v", body.Messages[2])
	}
	if !bytes.Contains(call.Body, []byte(`"type":"image_url"`)) {
		t.Fatalf("image input missing: %s", call.Body)
	}
}

func TestCompatibleChatStatelessProjectionPreservesToolCallReasoning(t *testing.T) {
	adapter := compatibleAdapter(t)
	request := compatibleRequest(t)
	request.ReasoningEffort = "high"
	request.Messages = contextview.ProjectStatelessHistory([]provider.Message{
		provider.TextMessage(provider.RoleUser, "inspect"),
		{
			Role: provider.RoleAssistant,
			Blocks: []provider.ContentBlock{
				{Type: provider.ContentReasoning, Text: "reasoning"},
				{Type: provider.ContentToolCall, ToolCall: &provider.ToolCall{
					ID: "call_1", Name: "read", Arguments: `{}`,
				}},
			},
		},
		{
			Role: provider.RoleTool,
			Blocks: []provider.ContentBlock{{
				Type: provider.ContentToolResult,
				ToolResult: &provider.ToolResult{
					CallID: "call_1", Content: "ok",
				},
			}},
		},
	})

	call, err := adapter.Prepare(request)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(call.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body.Messages[1]["reasoning_content"] != "reasoning" {
		t.Fatalf("tool-call reasoning missing after stateless projection: %#v", body.Messages[1])
	}
}

func TestCompatibleChatThinkingToggleIsCapabilityGated(t *testing.T) {
	adapter := compatibleAdapter(t)
	request := compatibleRequest(t)
	capabilities := request.Route.Model().Capabilities
	capabilities.ThinkingToggle = false
	request.Route = request.Route.WithCapabilities(capabilities)
	request.ReasoningEffort = "off"
	call, err := adapter.Prepare(request)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(call.Body, &body); err != nil {
		t.Fatal(err)
	}
	if _, exists := body["thinking"]; exists {
		t.Fatalf("provider-specific thinking field leaked: %s", call.Body)
	}
	var effort string
	if err := json.Unmarshal(body["reasoning_effort"], &effort); err != nil {
		t.Fatal(err)
	}
	if effort != "off" {
		t.Fatalf("reasoning_effort = %q", effort)
	}
}

func TestCompatibleChatWireRequestIsStrictAppendOnly(t *testing.T) {
	adapter := compatibleAdapter(t)
	request := compatibleRequest(t)
	request.PromptCacheKey = "session-append-only"
	request.ReasoningEffort = "high"
	request.Tools = []provider.ToolDefinition{{
		Name: "read", Description: "Read a file",
		InputSchema: map[string]any{"type": "object"},
	}}
	request.Messages = []provider.Message{
		provider.TextMessage(provider.RoleSystem, "stable system"),
		provider.TextMessage(provider.RoleUser, "first"),
	}
	first, err := adapter.Prepare(request)
	if err != nil {
		t.Fatal(err)
	}
	request.Messages = append(
		request.Messages,
		provider.Message{
			Role: provider.RoleAssistant,
			Blocks: []provider.ContentBlock{
				{Type: provider.ContentReasoning, Text: "think"},
				{Type: provider.ContentText, Text: "first answer"},
			},
		},
		provider.TextMessage(provider.RoleUser, "second"),
	)
	second, err := adapter.Prepare(request)
	if err != nil {
		t.Fatal(err)
	}
	firstMessages, firstHeader := chatWireParts(t, first.Body)
	secondMessages, secondHeader := chatWireParts(t, second.Body)
	if !bytes.Equal(firstHeader, secondHeader) {
		t.Fatalf(
			"request header changed across Turns:\nfirst=%s\nsecond=%s",
			firstHeader,
			secondHeader,
		)
	}
	if len(secondMessages) <= len(firstMessages) {
		t.Fatalf(
			"messages did not grow: first=%d second=%d",
			len(firstMessages),
			len(secondMessages),
		)
	}
	for index := range firstMessages {
		if !bytes.Equal(firstMessages[index], secondMessages[index]) {
			t.Fatalf(
				"message %d was rewritten:\nfirst=%s\nsecond=%s",
				index,
				firstMessages[index],
				secondMessages[index],
			)
		}
	}
}

func TestCompatibleChatWirePrefixReportsFirstSerializedDivergence(t *testing.T) {
	adapter := compatibleAdapter(t)
	request := compatibleRequest(t)
	request.PromptCacheKey = "session-divergence"
	request.Messages = []provider.Message{
		provider.TextMessage(provider.RoleSystem, "stable"),
		provider.TextMessage(provider.RoleUser, "first"),
	}
	first, err := adapter.Prepare(request)
	if err != nil {
		t.Fatal(err)
	}
	request.Messages[1] = provider.TextMessage(provider.RoleUser, "changed")
	call, err := adapter.Prepare(request)
	if err != nil {
		t.Fatal(err)
	}
	firstMessages, _ := chatWireParts(t, first.Body)
	secondMessages, _ := chatWireParts(t, call.Body)
	commonBytes := 0
	for index := range firstMessages {
		if !bytes.Equal(firstMessages[index], secondMessages[index]) {
			if index != 1 || commonBytes == 0 {
				t.Fatalf("wire divergence index=%d common_bytes=%d", index, commonBytes)
			}
			return
		}
		commonBytes += len(firstMessages[index])
	}
	t.Fatal("serialized message divergence was not detected")
}

func TestCompatibleChatStreamUsesNativeCacheAndAcceptsStandardEOF(t *testing.T) {
	adapter := compatibleAdapter(t)
	stream, err := adapter.OpenStream(
		io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`,
			"",
			`data: {"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":1,` +
				`"prompt_cache_hit_tokens":10,"prompt_cache_miss_tokens":2}}`,
			"",
			`data: [DONE]`,
			"",
			"",
		}, "\n"))),
		providerwire.PreparedCall{Protocol: model.ProtocolOpenAIChat},
	)
	if err != nil {
		t.Fatal(err)
	}
	events, err := provider.Drain(stream)
	if err != nil {
		t.Fatal(err)
	}
	usage := events[len(events)-2].Usage
	if usage == nil || usage.InputTokens != 12 || usage.CachedTokens != 10 {
		t.Fatalf("usage = %#v", usage)
	}

	incomplete, err := adapter.OpenStream(
		io.NopCloser(strings.NewReader(
			"data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},"+
				"\"finish_reason\":\"stop\"}]}\n\n",
		)),
		providerwire.PreparedCall{Protocol: model.ProtocolOpenAIChat},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Drain(incomplete); err != nil {
		t.Fatalf("standard finish_reason followed by EOF failed: %v", err)
	}
}

func TestCompatibleChatRejectsEmptyCompletion(t *testing.T) {
	adapter := compatibleAdapter(t)
	stream, err := adapter.OpenStream(
		io.NopCloser(strings.NewReader(
			"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n",
		)),
		providerwire.PreparedCall{Protocol: model.ProtocolOpenAIChat},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.Drain(stream)
	var failure *provider.Failure
	if !errors.As(err, &failure) ||
		failure.Code != provider.FailureEmptyResponse {
		t.Fatalf("empty completion failure = %T %v", err, err)
	}
}

func TestCompatibleHTTPFailurePreservesTypedContextError(t *testing.T) {
	adapter := compatibleAdapter(t)
	err := adapter.ClassifyHTTP(providerwire.HTTPFailure{
		Status: http.StatusBadRequest,
		Body: `{"error":{"message":"context length exceeded",` +
			`"code":"context_length_exceeded","type":"invalid_request_error"}}`,
		Header: http.Header{
			"X-Request-Id": []string{"request-1"},
		},
	})
	var failure *provider.Failure
	if !errors.As(err, &failure) {
		t.Fatalf("failure = %T %v", err, err)
	}
	if failure.Code != provider.FailureContextWindowExceeded ||
		failure.RequestID != "request-1" {
		t.Fatalf("failure = %+v", failure)
	}
}

func TestCompatibleHTTPFailureTreatsAmbiguousQuota429AsRateLimit(t *testing.T) {
	adapter := compatibleAdapter(t)
	err := adapter.ClassifyHTTP(providerwire.HTTPFailure{
		Status: http.StatusTooManyRequests,
		Body: `{"error":{"message":"account balance is insufficient",` +
			`"code":10003,"type":"rate_limit_error"}}`,
	})
	var failure *provider.Failure
	if !errors.As(err, &failure) {
		t.Fatalf("failure = %T %v", err, err)
	}
	if failure.Code != provider.FailureRateLimit {
		t.Fatalf("failure = %+v", failure)
	}
}

func TestCompatibleHTTPFailureRecognizesInsufficientQuota429(t *testing.T) {
	adapter := compatibleAdapter(t)
	err := adapter.ClassifyHTTP(providerwire.HTTPFailure{
		Status: http.StatusTooManyRequests,
		Body: `{"error":{"message":"Allocated quota exceeded, please increase your quota limit.",` +
			`"code":"insufficient_quota","type":"invalid_request_error"}}`,
	})
	var failure *provider.Failure
	if !errors.As(err, &failure) ||
		failure.Code != provider.FailureQuota {
		t.Fatalf("failure = %+v", failure)
	}
}

func chatWireParts(
	t *testing.T,
	body []byte,
) ([]json.RawMessage, []byte) {
	t.Helper()
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatal(err)
	}
	var messages []json.RawMessage
	if err := json.Unmarshal(envelope["messages"], &messages); err != nil {
		t.Fatal(err)
	}
	delete(envelope, "messages")
	header, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return messages, header
}

func compatibleAdapter(t *testing.T) *Adapter {
	t.Helper()
	adapter, err := NewAdapter(model.AdapterOpenAICompatible)
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

func compatibleRequest(t *testing.T) provider.ModelRequest {
	t.Helper()
	catalog, err := model.NewCatalog(model.Provider{
		ID: "compatible", Adapter: model.AdapterOpenAICompatible,
		Endpoint: "https://example.invalid",
		Protocol: model.ProtocolOpenAIChat,
		Credential: model.CredentialRef{
			Kind: "env", Name: "TEST_API_KEY",
		},
		Provenance: model.ProvenanceFixture,
		Models: map[string]model.Model{"model": {
			ID: "model", CanonicalID: "model", WireID: "model",
			Limits: model.Limits{
				ContextTokens: 8192, MaxOutputTokens: 4096,
			},
			Capabilities: model.Capabilities{
				Streaming: true, Reasoning: true, ToolCalls: true,
				Vision: true, ImageInput: true, PromptCache: true,
				AutomaticPromptCache: true, ThinkingToggle: true,
			},
			Pricing:    model.Pricing{Provenance: model.ProvenanceFixture},
			Provenance: model.ProvenanceFixture,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := model.NewResolver(catalog)
	if err != nil {
		t.Fatal(err)
	}
	route, err := resolver.Resolve(model.RouteRequest{
		ProviderID: "compatible",
		ModelID:    "model",
	})
	if err != nil {
		t.Fatal(err)
	}
	return provider.ModelRequest{
		Route: route,
		Messages: []provider.Message{
			provider.TextMessage(provider.RoleUser, "hello"),
		},
		MaxOutputTokens: 128,
	}
}
