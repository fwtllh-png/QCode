package openai

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	providerwire "github.com/fwtllh-png/QCode/internal/adapter/provider/wire"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/contextview"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

// TestBundledResponsesCatalogEntryEncodesToTheResponsesPath is the T5
// acceptance: a route taken from the bundled catalog, with no custom endpoint
// metadata, still produces a /responses body rather than /chat/completions.
func TestBundledResponsesCatalogEntryEncodesToTheResponsesPath(t *testing.T) {
	catalog, err := model.NewCatalog(model.Provider{
		ID: "openai-responses", Adapter: model.AdapterOpenAI,
		Endpoint: "https://api.openai.com/v1", Protocol: model.ProtocolOpenAIResponses,
		Credential: model.CredentialRef{Kind: "env", Name: "OPENAI_API_KEY"},
		Models: map[string]model.Model{
			"gpt-4.1": {
				ID: "gpt-4.1", CanonicalID: "gpt-4.1", WireID: "gpt-4.1",
				Limits: model.Limits{ContextTokens: 1_047_576, MaxOutputTokens: 32_768},
				Capabilities: model.Capabilities{
					Streaming: true, ToolCalls: true, PromptCache: true,
					IncrementalResponses: true,
				},
				Provenance: model.ProvenanceOperatorConfig,
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
	request := encodingRequest(t, "https://provider.test", model.ProtocolOpenAIResponses)
	request.PromptCacheKey = "session-1"
	var body map[string]any
	mustEncodeRequest(t, request, &body)
	if _, ok := body["prompt_cache_key"]; ok {
		t.Fatalf("prompt_cache_key should be omitted without capability: %#v", body)
	}
}

func TestEncodeChatPromptCacheKeyWithCapability(t *testing.T) {
	withCache := encodingRequestWithPromptCache(t, "https://provider.test", model.ProtocolOpenAIChat, true)
	withCache.PromptCacheKey = "session-chat"
	var body map[string]any
	path := mustEncodeRequest(t, withCache, &body)
	if path != "/chat/completions" {
		t.Fatalf("path = %q", path)
	}
	if body["prompt_cache_key"] != "session-chat" {
		t.Fatalf("prompt_cache_key missing: %#v", body)
	}

	without := encodingRequestWithPromptCache(t, "https://provider.test", model.ProtocolOpenAIChat, false)
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
		request := encodingRequest(t, "https://provider.test", model.ProtocolOpenAIResponses)
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
		request := encodingRequest(t, "https://provider.test", model.ProtocolOpenAIChat)
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
		request := encodingRequest(t, "https://provider.test", model.ProtocolOpenAIChat)
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
		request := encodingRequest(t, "https://provider.test", model.ProtocolOpenAIResponses)
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
		request := encodingRequest(t, "https://provider.test", model.ProtocolOpenAIResponses)
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
		request := encodingRequest(t, "https://provider.test", model.ProtocolOpenAIResponses)
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
		request := encodingRequest(t, "https://provider.test", model.ProtocolOpenAIResponses)
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
		request := encodingRequest(t, "https://provider.test", model.ProtocolOpenAIResponses)
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
		reconstructed, err := agentcontext.ReconstructThread(events, "thread-1")
		if err != nil {
			t.Fatal(err)
		}
		request := encodingRequest(t, "https://provider.test", model.ProtocolOpenAIResponses)
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
		request := encodingRequest(t, "https://provider.test", model.ProtocolOpenAIResponses)
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
		request := encodingRequest(t, "https://provider.test", model.ProtocolOpenAIResponses)
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
		request := encodingRequest(t, "https://provider.test", model.ProtocolOpenAIResponses)
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

func TestOpenAIResponsesDoesNotSynthesizeDeepSeekReasoning(t *testing.T) {
	request := testRequest(
		t, "https://api.openai.test", model.ProtocolOpenAIResponses,
	)
	request.Messages = []provider.Message{
		provider.TextMessage(provider.RoleUser, "inspect"),
		{Role: provider.RoleAssistant, Blocks: []provider.ContentBlock{{
			Type: provider.ContentToolCall,
			ToolCall: &provider.ToolCall{
				ID: "call_1", Name: "read", Arguments: "{}",
			},
		}}},
	}
	adapter, err := NewAdapter(model.AdapterOpenAI)
	if err != nil {
		t.Fatal(err)
	}
	call, err := adapter.Prepare(request)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(call.Body), "(continued)") {
		t.Fatalf("DeepSeek placeholder leaked into OpenAI request: %s", call.Body)
	}
}

// TestChatToolStreamIsUniformAcrossCompatibleConnections pins the uniform
// tool_stream contract: every OpenAI-compatible chat connection sends
// tool_stream=true regardless of the provider id, while the Responses
// protocol and the plain OpenAI adapter stay untouched.
func TestChatToolStreamIsUniformAcrossCompatibleConnections(t *testing.T) {
	for _, test := range []struct {
		name       string
		providerID string
		adapterID  model.AdapterID
		protocol   model.WireProtocol
		want       bool
	}{
		{"glm compatible", "glm", model.AdapterOpenAICompatible, model.ProtocolOpenAIChat, true},
		{"deepseek compatible", "deepseek", model.AdapterOpenAICompatible, model.ProtocolOpenAIChat, true},
		{"custom endpoint", "openai-compatible:ab12cd", model.AdapterOpenAICompatible, model.ProtocolOpenAIChat, true},
		{"responses protocol", "glm", model.AdapterOpenAICompatible, model.ProtocolOpenAIResponses, false},
		{"openai adapter", "glm", model.AdapterOpenAI, model.ProtocolOpenAIChat, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := compatibleRequest(t)
			fixtureModel := request.Route.Model()
			catalog, err := model.NewCatalog(model.Provider{
				ID: test.providerID, Adapter: test.adapterID,
				Endpoint: "https://example.invalid", Protocol: test.protocol,
				Provenance: model.ProvenanceFixture,
				Models:     map[string]model.Model{fixtureModel.ID: fixtureModel},
			})
			if err != nil {
				t.Fatal(err)
			}
			resolver, err := model.NewResolver(catalog)
			if err != nil {
				t.Fatal(err)
			}
			request.Route, err = resolver.Resolve(model.RouteRequest{
				ProviderID: test.providerID, ModelID: fixtureModel.ID,
			})
			if err != nil {
				t.Fatal(err)
			}
			request.Tools = []provider.ToolDefinition{{
				Name: "file_write", Description: "Write a file",
				InputSchema: map[string]any{"type": "object"},
			}}
			adapter, err := NewAdapter(test.adapterID)
			if err != nil {
				t.Fatal(err)
			}
			call, err := adapter.Prepare(request)
			if err != nil {
				t.Fatal(err)
			}
			var body struct {
				Stream     bool  `json:"stream"`
				ToolStream *bool `json:"tool_stream"`
			}
			if err := json.Unmarshal(call.Body, &body); err != nil {
				t.Fatal(err)
			}
			if !body.Stream {
				t.Fatal("streaming must remain enabled")
			}
			if test.want {
				if body.ToolStream == nil || !*body.ToolStream {
					t.Fatalf("tool_stream must be true: %s", call.Body)
				}
			} else if body.ToolStream != nil {
				t.Fatalf("tool_stream leaked outside the compatible chat path: %s", call.Body)
			}
		})
	}
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

func mustEncodeRequest(
	t testing.TB,
	request provider.ModelRequest,
	target any,
) string {
	t.Helper()
	data, path, err := encodeRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, target); err != nil {
		t.Fatal(err)
	}
	return path
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

func testRequest(
	t *testing.T,
	endpoint string,
	protocol model.WireProtocol,
) provider.ModelRequest {
	t.Helper()
	catalog, err := model.NewCatalog(model.Provider{
		ID: "fixture", Adapter: model.AdapterOpenAI, Endpoint: endpoint,
		Protocol: protocol, Provenance: model.ProvenanceFixture,
		Models: map[string]model.Model{"fixture-model": {
			ID: "fixture-model", CanonicalID: "fixture-model", WireID: "wire-model",
			Limits: model.Limits{ContextTokens: 8192, MaxOutputTokens: 4096},
			Capabilities: model.Capabilities{
				Streaming: true, Reasoning: true, ToolCalls: true,
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
		ProviderID: "fixture", ModelID: "fixture-model",
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
		Idempotent:      true,
	}
}

func encodingRequest(t *testing.T, endpoint string, wireProtocol model.WireProtocol) provider.ModelRequest {
	t.Helper()
	return encodingRequestWithPromptCache(t, endpoint, wireProtocol, false)
}

func encodingRequestWithPromptCache(
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

func encodeRequest(
	request provider.ModelRequest,
) ([]byte, string, error) {
	adapter, err := testAdapter(request.Route.Adapter())
	if err != nil {
		return nil, "", err
	}
	call, err := adapter.Prepare(request)
	return call.Body, call.Path, err
}

func testAdapter(id model.AdapterID) (providerwire.Adapter, error) {
	adapter, err := NewAdapter(id)
	if err != nil {
		return nil, fmt.Errorf("test adapter: %w", err)
	}
	return adapter, nil
}
