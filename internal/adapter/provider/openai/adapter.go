package openai

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	providerwire "github.com/fwtllh-png/QCode/internal/adapter/provider/wire"
)

type Adapter struct {
	id        model.AdapterID
	sessionMu sync.Mutex
	sessions  map[string]*responsesSession
}

func NewAdapter(id model.AdapterID) (*Adapter, error) {
	switch id {
	case model.AdapterOpenAI, model.AdapterOpenAICompatible:
		return &Adapter{id: id}, nil
	default:
		return nil, fmt.Errorf("unsupported OpenAI-compatible adapter %q", id)
	}
}

func (a *Adapter) ID() model.AdapterID { return a.id }
func (a *Adapter) Supports(protocol model.WireProtocol) bool {
	return protocol == model.ProtocolOpenAIChat ||
		protocol == model.ProtocolOpenAIResponses
}
func (a *Adapter) Prepare(request provider.ModelRequest) (providerwire.PreparedCall, error) {
	var (
		call providerwire.PreparedCall
		err  error
	)
	switch request.Route.Protocol() {
	case model.ProtocolOpenAIChat:
		policy := ChatPolicy{}
		if a.id == model.AdapterOpenAICompatible {
			policy.EmptyToolOutput = "(empty tool output)"
			policy.ThinkingOff =
				request.Route.Model().Capabilities.ThinkingToggle
			// 统一携带 tool_stream：兼容网关在 stream 模式下默认缓冲
			// 工具参数，逐段输出才能持续续期 idle 超时并尽早采样。
			policy.ToolStream = true
		}
		call, err = PrepareChat(request, a.id, policy)
	case model.ProtocolOpenAIResponses:
		policy := ResponsesPolicy{
			IncludeEncryptedReasoning: true,
		}
		if a.id == model.AdapterOpenAI {
			policy.ReplayAdapter = model.AdapterOpenAI
		}
		call, err = PrepareResponses(request, a.id, policy)
	default:
		return providerwire.PreparedCall{}, fmt.Errorf("adapter %q does not support protocol %q", a.id, request.Route.Protocol())
	}
	if err != nil {
		return providerwire.PreparedCall{}, err
	}
	if a.id == model.AdapterOpenAICompatible {
		call.Projection = providerwire.CompleteStatelessProjection(
			request,
			provider.ProjectionFallbackCapabilityDisabled,
		)
	} else {
		call.Projection = a.prepareProjection(request, call)
	}
	return call, nil
}

type ChatPolicy struct {
	ReasoningWithToolsOnly bool
	RejectImages           bool
	EmptyToolOutput        string
	ThinkingOff            bool
	ToolStream             bool
}

func PrepareChat(
	request provider.ModelRequest,
	adapter model.AdapterID,
	options ChatPolicy,
) (providerwire.PreparedCall, error) {
	messages, err := chatMessages(request.Messages, options)
	if err != nil {
		return providerwire.PreparedCall{}, err
	}
	body := chatBody(request, messages, options)
	return prepareCall(adapter, model.ProtocolOpenAIChat, "/chat/completions", body)
}

type ResponsesPolicy struct {
	ReasoningPlaceholder      string
	IncludeEncryptedReasoning bool
	ReplayAdapter             model.AdapterID
}

func PrepareResponses(
	request provider.ModelRequest,
	adapter model.AdapterID,
	policy ResponsesPolicy,
) (providerwire.PreparedCall, error) {
	input, err := responsesInput(request.Messages, request.Route, policy)
	if err != nil {
		return providerwire.PreparedCall{}, err
	}
	body := responsesBody(
		request, input, policy.IncludeEncryptedReasoning,
	)
	return prepareCall(adapter, model.ProtocolOpenAIResponses, "/responses", body)
}
func prepareCall(
	adapter model.AdapterID,
	protocol model.WireProtocol,
	path string,
	body map[string]any,
) (providerwire.PreparedCall, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return providerwire.PreparedCall{}, err
	}
	return providerwire.PreparedCall{
		Method: http.MethodPost, Path: path, Body: data,
		Headers: http.Header{
			"Content-Type": []string{"application/json"},
			"Accept":       []string{"text/event-stream"},
		},
		Auth: providerwire.AuthBearer, Adapter: adapter,
		Protocol: protocol,
	}, nil
}
func (a *Adapter) OpenStream(body io.ReadCloser, call providerwire.PreparedCall) (provider.Stream, error) {
	policy := StreamPolicy{CaptureReplay: a.id == model.AdapterOpenAI}
	if a.id == model.AdapterOpenAICompatible {
		policy.NativeCache = true
	}
	stream, err := NewStreamWithOptions(body, call.Protocol, policy)
	if err != nil {
		return nil, err
	}
	if a.id == model.AdapterOpenAICompatible {
		return &meaningfulStream{Stream: stream}, nil
	}
	return stream, nil
}

type meaningfulStream struct {
	provider.Stream
	meaningful bool
}

func (s *meaningfulStream) Recv() (provider.StreamEvent, error) {
	event, err := s.Stream.Recv()
	if err != nil {
		return provider.StreamEvent{}, err
	}
	switch event.Type {
	case provider.EventTextDelta,
		provider.EventReasoningDelta,
		provider.EventToolCallDelta,
		provider.EventSearchResult,
		provider.EventCitation:
		s.meaningful = true
	case provider.EventMessageStop:
		if !s.meaningful {
			return provider.StreamEvent{}, &provider.Failure{
				Code:    provider.FailureEmptyResponse,
				Message: "provider returned an empty response",
			}
		}
	}
	return event, nil
}

func (a *Adapter) ClassifyHTTP(failure providerwire.HTTPFailure) error {
	var payload struct {
		Error struct {
			Message string          `json:"message"`
			Code    json.RawMessage `json:"code"`
			Type    string          `json:"type"`
		} `json:"error"`
	}
	_ = json.Unmarshal([]byte(failure.Body), &payload)
	message := payload.Error.Message
	if message == "" {
		message = fmt.Sprintf("provider returned HTTP %d", failure.Status)
	}
	businessCode := jsonScalarText(payload.Error.Code)
	code, known := structuredHTTPFailure(businessCode, payload.Error.Type)
	if !known && a.id == model.AdapterOpenAICompatible &&
		failure.Status == http.StatusTooManyRequests {
		code = provider.FailureRateLimit
	} else if !known {
		code = classifyHTTPFailure(
			failure.Status,
			businessCode,
			payload.Error.Type,
			message,
		)
	}
	return providerwire.TypedHTTPFailure(
		failure,
		code,
		message,
		providerwire.FirstHeader(
			failure.Header,
			"Openai-Request-Id",
			"X-Request-Id",
		),
	)
}

func structuredHTTPFailure(code, kind string) (provider.FailureCode, bool) {
	for _, value := range []string{code, kind} {
		switch value {
		case "insufficient_quota", "billing_hard_limit_reached":
			return provider.FailureQuota, true
		}
	}
	return "", false
}

func classifyHTTPFailure(
	status int,
	code, kind, message string,
) provider.FailureCode {
	value := strings.ToLower(code + " " + kind + " " + message)
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return provider.FailureAuth
	case providerwire.IsQuotaFailure(value):
		return provider.FailureQuota
	case status == http.StatusTooManyRequests:
		return provider.FailureRateLimit
	case strings.Contains(value, "context_length") ||
		strings.Contains(value, "context length"):
		return provider.FailureContextWindowExceeded
	case status == http.StatusBadRequest:
		return provider.FailureInvalidRequest
	case status >= 500:
		return provider.FailureServer
	default:
		return provider.FailureUnknown
	}
}

func jsonScalarText(raw json.RawMessage) string {
	var value string
	if json.Unmarshal(raw, &value) == nil {
		return value
	}
	return strings.TrimSpace(string(raw))
}

func chatBody(
	request provider.ModelRequest,
	messages []map[string]any,
	options ChatPolicy,
) map[string]any {
	body := map[string]any{
		"model": request.Route.Model().WireID, "messages": messages,
		"max_tokens": request.MaxOutputTokens, "stream": true,
		"stream_options": map[string]bool{"include_usage": true},
	}
	if options.ToolStream {
		// OpenAI-compatible 网关在 stream 模式下默认缓冲工具参数；
		// tool_stream 让参数逐段到达，而不是等整段拼接完成。
		body["tool_stream"] = true
	}
	if request.Temperature != nil {
		body["temperature"] = *request.Temperature
	}
	if request.ReasoningEffort == "off" && options.ThinkingOff {
		body["thinking"] = map[string]string{"type": "disabled"}
	} else if request.ReasoningEffort != "" {
		body["reasoning_effort"] = request.ReasoningEffort
	}
	applyChatTools(body, request.Tools)
	if request.NativeSearch {
		appendTool(body, map[string]any{"type": "web_search_preview"})
	}
	if !request.Route.Model().Capabilities.AutomaticPromptCache &&
		request.PromptCacheKey != "" &&
		request.Route.Model().Capabilities.PromptCache {
		body["prompt_cache_key"] = request.PromptCacheKey
	}
	return body
}
func responsesBody(
	request provider.ModelRequest,
	input []map[string]any,
	includeEncryptedReasoning bool,
) map[string]any {
	store := false
	if request.Store != nil {
		store = *request.Store
	}
	parallelTools := true
	if request.ParallelTools != nil {
		parallelTools = *request.ParallelTools
	}
	include := request.Include
	if len(include) == 0 && request.ReasoningEffort != "" &&
		includeEncryptedReasoning {
		include = []string{"reasoning.encrypted_content"}
	}
	body := map[string]any{
		"model": request.Route.Model().WireID, "input": input,
		"max_output_tokens": request.MaxOutputTokens, "stream": true,
		"store": store, "parallel_tool_calls": parallelTools,
	}
	if len(include) != 0 {
		body["include"] = include
	}
	if request.Temperature != nil {
		body["temperature"] = *request.Temperature
	}
	if request.ReasoningEffort != "" {
		body["reasoning"] = map[string]any{"effort": request.ReasoningEffort, "summary": "auto"}
	}
	applyResponsesTools(body, request.Tools)
	if request.NativeSearch {
		appendTool(body, map[string]any{"type": "web_search"})
	}
	if !request.Route.Model().Capabilities.AutomaticPromptCache &&
		request.PromptCacheKey != "" &&
		request.Route.Model().Capabilities.PromptCache {
		body["prompt_cache_key"] = request.PromptCacheKey
	}
	return body
}
func chatMessages(
	messages []provider.Message,
	options ChatPolicy,
) ([]map[string]any, error) {
	result := make([]map[string]any, 0, len(messages))
	for _, message := range messages {
		item := map[string]any{"role": message.Role}
		var text, reasoning string
		var calls, images []map[string]any
		for _, block := range message.Blocks {
			switch block.Type {
			case provider.ContentText:
				text += block.Text
			case provider.ContentReasoning:
				reasoning += block.Text
			case provider.ContentImage:
				if options.RejectImages {
					return nil, fmt.Errorf("image input is not supported")
				}
				images = append(images, map[string]any{"type": "image_url", "image_url": map[string]any{"url": block.Attachment.DataURL()}})
			case provider.ContentToolCall:
				call := block.ToolCall
				calls = append(calls, map[string]any{"id": call.ID, "type": "function", "function": map[string]any{"name": call.Name, "arguments": call.Arguments}})
			case provider.ContentToolResult:
				item["tool_call_id"] = block.ToolResult.CallID
				text += block.ToolResult.Content
			}
		}
		if message.Role == provider.RoleTool && text == "" &&
			options.EmptyToolOutput != "" {
			text = options.EmptyToolOutput
		}
		if len(images) == 0 {
			item["content"] = text
		} else {
			content := make([]map[string]any, 0, len(images)+1)
			if text != "" {
				content = append(content, map[string]any{"type": "text", "text": text})
			}
			item["content"] = append(content, images...)
		}
		if len(calls) != 0 {
			item["tool_calls"] = calls
		}
		if reasoning != "" &&
			(!options.ReasoningWithToolsOnly || len(calls) != 0) {
			item["reasoning_content"] = reasoning
		}
		result = append(result, item)
	}
	return result, nil
}
func responsesInput(
	messages []provider.Message,
	route model.ReadyRoute,
	policy ResponsesPolicy,
) ([]map[string]any, error) {
	result := make([]map[string]any, 0, len(messages))
	for _, message := range messages {
		var replayItems []json.RawMessage
		if policy.ReplayAdapter != "" {
			items, err := ParseResponsesReplay(
				message, route, policy.ReplayAdapter,
			)
			if err != nil {
				return nil, err
			}
			replayItems = items
		}
		reasoningBlock := 0
		if grouped, ok := responsesImageItem(message); ok {
			result = append(result, grouped)
			continue
		}
		for _, block := range message.Blocks {
			switch block.Type {
			case provider.ContentText:
				result = append(result, map[string]any{"role": message.Role, "content": block.Text})
			case provider.ContentReasoning:
				replay := replayReasoningItem(
					replayItems, block, reasoningBlock,
				)
				item, err := responsesReasoningItem(
					block,
					replay,
				)
				if err != nil {
					return nil, err
				}
				reasoningBlock++
				if item != nil {
					result = append(result, item)
				}
			case provider.ContentToolCall:
				call := block.ToolCall
				result = ensureReasoningBeforeFunctionCall(
					result, policy.ReasoningPlaceholder,
				)
				result = append(result, map[string]any{"type": "function_call", "call_id": call.ID, "name": call.Name, "arguments": call.Arguments})
			case provider.ContentToolResult:
				result = append(result, map[string]any{"type": "function_call_output", "call_id": block.ToolResult.CallID, "output": block.ToolResult.Content})
			}
		}
	}
	if err := validateResponsesToolPairs(result); err != nil {
		return nil, err
	}
	return result, nil
}
func validateResponsesToolPairs(input []map[string]any) error {
	calls := make(map[string]struct{})
	for index, item := range input {
		switch stringValue(item["type"]) {
		case "function_call":
			callID := stringValue(item["call_id"])
			if callID == "" {
				return fmt.Errorf("OpenAI Responses function_call at input %d has no call_id", index)
			}
			calls[callID] = struct{}{}
		case "function_call_output":
			callID := stringValue(item["call_id"])
			if _, exists := calls[callID]; callID == "" || !exists {
				return fmt.Errorf("OpenAI Responses function_call_output at input %d has no preceding function_call for call_id %q", index, callID)
			}
		}
	}
	return nil
}
func ensureReasoningBeforeFunctionCall(
	result []map[string]any,
	placeholder string,
) []map[string]any {
	if len(result) > 0 {
		switch stringValue(result[len(result)-1]["type"]) {
		case "reasoning", "function_call":
			return result
		}
	}
	if placeholder == "" {
		return result
	}
	return append(result, map[string]any{
		"type": "reasoning",
		"content": []map[string]any{{
			"type": "reasoning_text", "text": placeholder,
		}},
	})
}
func responsesReasoningItem(
	block provider.ContentBlock,
	replay json.RawMessage,
) (map[string]any, error) {
	text := strings.TrimSpace(block.Text)
	var item map[string]any
	if len(replay) != 0 {
		if err := json.Unmarshal(replay, &item); err != nil {
			return nil, fmt.Errorf("decode OpenAI reasoning replay block: %w", err)
		}
		replayText := strings.TrimSpace(reasoningTextFromItem(item))
		if text == "" {
			text = replayText
		} else if !reasoningItemContainsText(item, text) {
			return nil, errors.New(
				"Responses replay reasoning does not match assistant content",
			)
		}
	}
	if text == "" {
		return nil, nil
	}
	out := map[string]any{"type": "reasoning", "content": []map[string]any{{"type": "reasoning_text", "text": text}}}
	if id := stringValue(item["id"]); id != "" {
		out["id"] = id
	}
	return out, nil
}

func reasoningItemContainsText(item map[string]any, text string) bool {
	var populated bool
	for _, key := range []string{"content", "summary"} {
		candidate := strings.TrimSpace(
			reasoningTextFromContentParts(item[key]),
		)
		if candidate == "" {
			continue
		}
		populated = true
		if candidate == text {
			return true
		}
	}
	return !populated
}

func replayReasoningItem(
	items []json.RawMessage,
	block provider.ContentBlock,
	ordinal int,
) json.RawMessage {
	if block.ID != "" {
		for _, raw := range items {
			var item map[string]any
			if json.Unmarshal(raw, &item) == nil &&
				stringValue(item["id"]) == block.ID &&
				replayItemMatchesBlock(item, block) {
				return raw
			}
		}
	}
	for _, raw := range items {
		var item map[string]any
		if json.Unmarshal(raw, &item) == nil &&
			replayItemMatchesBlock(item, block) {
			return raw
		}
	}
	if block.Text == "" && ordinal >= 0 && ordinal < len(items) {
		return items[ordinal]
	}
	return nil
}

func replayItemMatchesBlock(
	item map[string]any,
	block provider.ContentBlock,
) bool {
	text := strings.TrimSpace(block.Text)
	return text == "" || reasoningItemContainsText(item, text)
}

func reasoningTextFromItem(item map[string]any) string {
	if item == nil {
		return ""
	}
	if text := reasoningTextFromContentParts(item["content"]); text != "" {
		return text
	}
	return reasoningTextFromContentParts(item["summary"])
}
func reasoningTextFromContentParts(value any) string {
	switch content := value.(type) {
	case string:
		return content
	case []any:
		var parts []string
		for _, raw := range content {
			part, _ := raw.(map[string]any)
			if part == nil {
				continue
			}
			switch stringValue(part["type"]) {
			case "reasoning_text", "output_text", "summary_text", "text", "":
				if text := stringValue(part["text"]); text != "" {
					parts = append(parts, text)
				} else if text := stringValue(part["reasoning_text"]); text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "")
	default:
		return ""
	}
}
func responsesImageItem(message provider.Message) (map[string]any, bool) {
	var content []map[string]any
	images := false
	for _, block := range message.Blocks {
		switch block.Type {
		case provider.ContentText:
			content = append(content, map[string]any{"type": "input_text", "text": block.Text})
		case provider.ContentImage:
			images = true
			content = append(content, map[string]any{"type": "input_image", "image_url": block.Attachment.DataURL()})
		}
	}
	if !images {
		return nil, false
	}
	return map[string]any{"role": message.Role, "content": content}, true
}
func applyChatTools(body map[string]any, definitions []provider.ToolDefinition) {
	for _, definition := range definitions {
		function := map[string]any{
			"name": definition.Name, "description": definition.Description,
			"parameters": definition.InputSchema,
		}
		appendTool(body, map[string]any{"type": "function", "function": function})
	}
}
func applyResponsesTools(body map[string]any, definitions []provider.ToolDefinition) {
	for _, definition := range definitions {
		appendTool(body, map[string]any{"type": "function", "name": definition.Name, "description": definition.Description, "parameters": definition.InputSchema})
	}
}
func appendTool(body map[string]any, definition map[string]any) {
	tools, _ := body["tools"].([]map[string]any)
	body["tools"] = append(tools, definition)
}
