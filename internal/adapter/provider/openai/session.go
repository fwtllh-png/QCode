package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	providerwire "github.com/fwtllh-png/QCode/internal/adapter/provider/wire"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type responsesSession struct {
	mu                           sync.Mutex
	conn                         providerwire.Socket
	cancel                       context.CancelFunc
	endpoint, previous, property string
	routeDigest                  string
	windowID                     string
	recoveryID                   string
	prefix                       []json.RawMessage
	lastUsed                     time.Time
	idle                         *time.Timer
	forceHTTP                    bool
}

func (a *Adapter) TrySession(ctx context.Context, request provider.ModelRequest, call providerwire.PreparedCall, transport providerwire.SessionTransport) (provider.Stream, bool, error) {
	if call.Protocol != "openai_responses" || !request.Route.Model().Capabilities.IncrementalResponses || request.PromptCacheKey == "" {
		return nil, false, nil
	}
	if call.Projection.Mode == provider.ProjectionModeFullHTTP {
		return nil, false, nil
	}
	session := a.session(request)
	attempt, err := transport.BeginSession(ctx, request.Route, call)
	if err != nil {
		return nil, true, err
	}
	defer attempt.Close()
	stream, metadata, err := a.openSession(ctx, request, call, attempt, session)
	if err != nil {
		if ctx.Err() != nil {
			return nil, true, ctx.Err()
		}
		session.mu.Lock()
		session.forceHTTP = true
		session.mu.Unlock()
		return nil, true, protocol.NewProblem(protocol.CodeUnavailable, "provider session failed", true, err)
	}
	return attempt.Wrap(stream, metadata), true, nil
}
func (a *Adapter) session(request provider.ModelRequest) *responsesSession {
	key := request.PromptCacheKey
	a.sessionMu.Lock()
	defer a.sessionMu.Unlock()
	if a.sessions == nil {
		a.sessions = make(map[string]*responsesSession)
	}
	session := a.sessions[key]
	if session == nil {
		session = &responsesSession{}
		a.sessions[key] = session
	}
	return session
}
func (a *Adapter) openSession(ctx context.Context, request provider.ModelRequest, call providerwire.PreparedCall, attempt providerwire.SessionAttempt, session *responsesSession) (provider.Stream, provider.TransportMetadata, error) {
	session.mu.Lock()
	body, input, property, err := responsesSocketBody(call.Body)
	if err != nil {
		session.mu.Unlock()
		return nil, provider.TransportMetadata{}, err
	}
	now := time.Now()
	idleTimeout := attempt.IdleTimeout()
	if session.conn != nil && idleTimeout > 0 && now.Sub(session.lastUsed) > idleTimeout {
		session.invalidate()
	}
	projection := evaluateProjection(session, request, call, input, property)
	endpoint, err := websocketEndpoint(request.Route.Endpoint())
	if err != nil {
		session.mu.Unlock()
		return nil, provider.TransportMetadata{}, err
	}
	if session.conn == nil || session.endpoint != endpoint {
		session.invalidate()
		conn, cancel, err := attempt.Dial(endpoint)
		if err != nil {
			session.mu.Unlock()
			return nil, provider.TransportMetadata{}, err
		}
		session.conn, session.cancel, session.endpoint = conn, cancel, endpoint
	}
	incremental := projection.IncrementalEligible
	transportInput, projection, err := projectTransportInput(
		session,
		input,
		projection,
	)
	if err != nil {
		session.mu.Unlock()
		return nil, provider.TransportMetadata{}, err
	}
	if incremental {
		body["previous_response_id"] = session.previous
		body["input"] = transportInput
	}
	session.previous, session.property, session.prefix = "", "", nil
	session.routeDigest, session.windowID, session.recoveryID = "", "", ""
	payload, err := json.Marshal(body)
	if err != nil {
		session.mu.Unlock()
		return nil, provider.TransportMetadata{}, err
	}
	attempt.ProviderRequest()
	if err := session.conn.Write(ctx, payload); err != nil {
		session.invalidate()
		session.mu.Unlock()
		return nil, provider.TransportMetadata{}, err
	}
	session.lastUsed = now
	return newResponsesSocketStream(
			ctx,
			session,
			input,
			property,
			projection,
			request.Projection,
			idleTimeout,
		), sessionMetadata(
			request,
			call,
			payload,
			incremental,
			projection,
		), nil
}
func responsesSocketBody(data []byte) (map[string]any, []json.RawMessage, string, error) {
	var body map[string]any
	if err := json.Unmarshal(data, &body); err != nil {
		return nil, nil, "", err
	}
	delete(body, "stream")
	body["type"] = "response.create"
	rawInput, err := json.Marshal(body["input"])
	if err != nil {
		return nil, nil, "", err
	}
	var input []json.RawMessage
	if err := json.Unmarshal(rawInput, &input); err != nil {
		return nil, nil, "", err
	}
	properties := cloneMap(body)
	delete(properties, "input")
	delete(properties, "type")
	propertyBytes, err := json.Marshal(properties)
	if err != nil {
		return nil, nil, "", err
	}
	return body, input, providerwire.Digest(propertyBytes), nil
}
func cloneMap(input map[string]any) map[string]any {
	out := make(map[string]any, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}
func strictExtension(prefix, current []json.RawMessage) bool {
	if len(current) <= len(prefix) {
		return false
	}
	for index := range prefix {
		if !jsonEqual(prefix[index], current[index]) {
			return false
		}
	}
	return true
}
func jsonEqual(left, right []byte) bool {
	var a, b any
	return json.Unmarshal(left, &a) == nil && json.Unmarshal(right, &b) == nil && deepEqualJSON(a, b)
}
func deepEqualJSON(left, right any) bool {
	a, _ := json.Marshal(left)
	b, _ := json.Marshal(right)
	return string(a) == string(b)
}
func websocketEndpoint(endpoint string) (string, error) {
	value, err := url.Parse(strings.TrimRight(endpoint, "/") + "/responses")
	if err != nil {
		return "", err
	}
	switch value.Scheme {
	case "https":
		value.Scheme = "wss"
	case "http":
		value.Scheme = "ws"
	default:
		return "", errors.New("Responses WebSocket requires an HTTP endpoint")
	}
	return value.String(), nil
}

// responsesSocketStream owns session.mu from openSession until Close. Its own
// mu orders Close against a Recv that another goroutine left blocked in Read
// (the transport idle timeout abandons Recv without waiting for it).
type responsesSocketStream struct {
	mu                       sync.Mutex
	ctx                      context.Context
	session                  *responsesSession
	conn                     providerwire.Socket
	decoder                  ResponsesDecoder
	queue                    []provider.StreamEvent
	started, stopped, closed bool
	input                    []json.RawMessage
	property                 string
	routeDigest              string
	windowID                 string
	recoveryID               string
	pending                  *provider.ResponseState
	idleTimeout              time.Duration
}

func (s *responsesSocketStream) Recv() (provider.StreamEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped || s.closed {
		return provider.StreamEvent{}, io.EOF
	}
	if !s.started {
		s.started = true
		return provider.StreamEvent{Type: provider.EventMessageStart}, nil
	}
	for {
		if s.closed {
			return provider.StreamEvent{}, io.EOF
		}
		if len(s.queue) != 0 {
			event := s.queue[0]
			s.queue = s.queue[1:]
			if event.Type == provider.EventResponseState {
				s.pending = event.Response
				continue
			}
			if event.Type == provider.EventMessageStop && s.pending != nil && !event.StopReason.Incomplete() {
				s.session.previous = s.pending.ID
				s.session.property = s.property
				s.session.prefix = append(append([]json.RawMessage(nil), s.input...), replayOutput(s.pending.Output)...)
				s.session.routeDigest = s.routeDigest
				s.session.windowID = s.windowID
				s.session.recoveryID = s.recoveryID
			}
			if event.Type == provider.EventMessageStop {
				s.stopped = true
			}
			return event, nil
		}
		if err := s.read(); err != nil {
			return provider.StreamEvent{}, err
		}
	}
}
func (s *responsesSession) invalidate() {
	if s.conn != nil {
		_ = s.conn.Close()
	}
	if s.cancel != nil {
		s.cancel()
	}
	if s.idle != nil {
		s.idle.Stop()
	}
	s.conn, s.cancel, s.previous, s.property, s.prefix = nil, nil, "", "", nil
	s.routeDigest, s.windowID, s.recoveryID = "", "", ""
	s.idle = nil
}
func replayOutput(items []json.RawMessage) []json.RawMessage {
	var result []json.RawMessage
	for _, raw := range items {
		var item map[string]any
		if json.Unmarshal(raw, &item) != nil {
			continue
		}
		switch item["type"] {
		case "function_call":
			result = appendJSON(result, map[string]any{"type": "function_call", "call_id": item["call_id"], "name": item["name"], "arguments": item["arguments"]})
		case "reasoning":
			text := responseItemText(item["content"])
			if text == "" {
				text = responseItemText(item["summary"])
			}
			if text != "" {
				value := map[string]any{"type": "reasoning", "content": []map[string]any{{"type": "reasoning_text", "text": text}}}
				if item["id"] != nil {
					value["id"] = item["id"]
				}
				result = appendJSON(result, value)
			}
		case "message":
			if text := responseItemText(item["content"]); text != "" {
				result = appendJSON(result, map[string]any{"role": "assistant", "content": text})
			}
		}
	}
	return result
}
func responseItemText(value any) string {
	parts, _ := value.([]any)
	var text string
	for _, raw := range parts {
		part, _ := raw.(map[string]any)
		if value, ok := part["text"].(string); ok {
			text += value
		}
	}
	return text
}
func appendJSON(items []json.RawMessage, value any) []json.RawMessage {
	raw, err := json.Marshal(value)
	if err == nil {
		items = append(items, raw)
	}
	return items
}

var _ providerwire.SessionAdapter = (*Adapter)(nil)
