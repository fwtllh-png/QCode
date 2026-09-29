package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/coder/websocket"
	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/provider/httpclient"
	providerwire "github.com/fwtllh-png/QCode/internal/adapter/provider/wire"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	"github.com/fwtllh-png/QCode/internal/security/egress"
)

func TestResponsesSessionSendsStrictDeltaAndSeparatesDigests(t *testing.T) {
	var (
		mu     sync.Mutex
		frames []map[string]any
	)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		conn, err := websocket.Accept(writer, request, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		for index := 0; index < 2; index++ {
			_, data, err := conn.Read(request.Context())
			if err != nil {
				t.Error(err)
				return
			}
			var frame map[string]any
			if err := json.Unmarshal(data, &frame); err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			frames = append(frames, frame)
			mu.Unlock()
			writeResponseEvent(t, request.Context(), conn, map[string]any{
				"type": "response.output_text.delta", "delta": "answer",
			})
			writeResponseEvent(t, request.Context(), conn, completedResponse("resp-"+string(rune('1'+index)), "answer"))
		}
	}))
	defer server.Close()

	client := testClient()
	client.Credentials = staticCredentials("test-key")
	first := incrementalRequest(t, server.URL)
	large := strings.Repeat("stable context ", 1000)
	first.Messages = []provider.Message{provider.TextMessage(provider.RoleUser, large)}
	stream, err := client.Stream(t.Context(), first)
	if err != nil {
		t.Fatal(err)
	}
	firstMetadata := provider.Metadata(stream)
	if _, err := provider.Drain(stream); err != nil {
		t.Fatal(err)
	}

	second := first
	second.Messages = append(second.Messages,
		provider.TextMessage(provider.RoleAssistant, "answer"),
		provider.TextMessage(provider.RoleUser, "next"),
	)
	stream, err = client.Stream(t.Context(), second)
	if err != nil {
		t.Fatal(err)
	}
	secondMetadata := provider.Metadata(stream)
	if _, err := provider.Drain(stream); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(frames) != 2 {
		t.Fatalf("frames = %d", len(frames))
	}
	if frames[0]["previous_response_id"] != nil {
		t.Fatalf("first frame chained: %#v", frames[0])
	}
	if frames[1]["previous_response_id"] != "resp-1" {
		t.Fatalf("second previous_response_id = %#v", frames[1]["previous_response_id"])
	}
	input, _ := frames[1]["input"].([]any)
	if len(input) != 1 {
		t.Fatalf("incremental input = %#v", frames[1]["input"])
	}
	if !secondMetadata.Incremental {
		t.Fatal("second request was not attributed as incremental")
	}
	projection := secondMetadata.Projection
	if projection.Mode != provider.ProjectionModeIncrementalSession ||
		!projection.IncrementalEligible ||
		projection.FallbackReason != "" ||
		projection.StablePrefixDigest == "" ||
		projection.InputDigest == "" ||
		projection.DeltaDigest == "" ||
		projection.LogicalItems != 3 ||
		projection.TransportItems != 1 ||
		!projection.LogicalTransportEquivalent {
		t.Fatalf("incremental projection=%+v", projection)
	}
	if firstMetadata.Projection.FallbackReason !=
		provider.ProjectionFallbackNoCommittedResponse {
		t.Fatalf("initial projection=%+v", firstMetadata.Projection)
	}
	if secondMetadata.RequestBytes*100 >= firstMetadata.RequestBytes*40 {
		t.Fatalf("request bytes did not fall 60%%: first=%d second=%d",
			firstMetadata.RequestBytes, secondMetadata.RequestBytes)
	}
	t.Logf("request bytes: full=%d incremental=%d reduction=%.2f%%",
		firstMetadata.RequestBytes, secondMetadata.RequestBytes,
		100*(1-float64(secondMetadata.RequestBytes)/float64(firstMetadata.RequestBytes)))
	if secondMetadata.LogicalRequestDigest == secondMetadata.TransportPayloadDigest {
		t.Fatal("incremental logical and transport digests must differ")
	}
}

func TestResponsesSessionFallsBackToFullFrameWhenPropertiesChange(t *testing.T) {
	frames := make(chan map[string]any, 2)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		conn, err := websocket.Accept(writer, request, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		for index := 0; index < 2; index++ {
			_, data, err := conn.Read(request.Context())
			if err != nil {
				t.Error(err)
				return
			}
			var frame map[string]any
			if err := json.Unmarshal(data, &frame); err != nil {
				t.Error(err)
				return
			}
			frames <- frame
			writeResponseEvent(t, request.Context(), conn, completedResponse("resp", "answer"))
		}
	}))
	defer server.Close()

	client := testClient()
	first := incrementalRequest(t, server.URL)
	stream, err := client.Stream(t.Context(), first)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Drain(stream); err != nil {
		t.Fatal(err)
	}
	second := first
	second.MaxOutputTokens++
	second.Messages = append(second.Messages,
		provider.TextMessage(provider.RoleAssistant, "answer"),
		provider.TextMessage(provider.RoleUser, "next"),
	)
	stream, err = client.Stream(t.Context(), second)
	if err != nil {
		t.Fatal(err)
	}
	metadata := provider.Metadata(stream)
	if _, err := provider.Drain(stream); err != nil {
		t.Fatal(err)
	}
	<-frames
	frame := <-frames
	if frame["previous_response_id"] != nil || metadata.Incremental {
		t.Fatalf("property change reused continuation: %#v", frame)
	}
	if metadata.Projection.FallbackReason !=
		provider.ProjectionFallbackPropertyChanged {
		t.Fatalf("property fallback=%+v", metadata.Projection)
	}
	input, _ := frame["input"].([]any)
	if len(input) != 3 {
		t.Fatalf("full fallback input = %#v", frame["input"])
	}
}

func TestResponsesSessionCapabilityOffKeepsHTTPBehavior(t *testing.T) {
	var upgraded bool
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		upgraded = request.Header.Get("Upgrade") != ""
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
	}))
	defer server.Close()

	client := testClient()
	request := testRequest(t, server.URL, model.ProtocolOpenAIResponses)
	stream, err := client.Stream(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Drain(stream); err != nil {
		t.Fatal(err)
	}
	if upgraded || provider.Metadata(stream).Incremental {
		t.Fatal("unsupported route changed transport")
	}
}

func TestResponsesSessionConnectionResetDefersFullFallbackToNextCall(t *testing.T) {
	var fullFallback map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Upgrade") == "" {
			if err := json.NewDecoder(request.Body).Decode(&fullFallback); err != nil {
				t.Error(err)
			}
			writer.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(writer, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"http\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
			return
		}
		conn, err := websocket.Accept(writer, request, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		if _, _, err := conn.Read(request.Context()); err != nil {
			t.Error(err)
			return
		}
		writeResponseEvent(t, request.Context(), conn, completedResponse("resp-1", "answer"))
		<-request.Context().Done()
	}))
	defer server.Close()

	client := testClient()
	first := incrementalRequest(t, server.URL)
	stream, err := client.Stream(t.Context(), first)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Drain(stream); err != nil {
		t.Fatal(err)
	}
	client.adapter.sessionMu.Lock()
	session := client.adapter.sessions["session-1"]
	client.adapter.sessionMu.Unlock()
	session.mu.Lock()
	session.conn.Close()
	session.mu.Unlock()

	second := first
	second.Messages = append(second.Messages,
		provider.TextMessage(provider.RoleAssistant, "answer"),
		provider.TextMessage(provider.RoleUser, "next"),
	)
	_, err = client.Stream(t.Context(), second)
	if !protocol.IsCode(err, protocol.CodeUnavailable) {
		t.Fatalf("reset Stream() error = %v", err)
	}
	if fullFallback != nil {
		t.Fatalf("reset call performed an HTTP fallback: %#v", fullFallback)
	}
	stream, err = client.Stream(t.Context(), second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Drain(stream); err != nil {
		t.Fatal(err)
	}
	if provider.Metadata(stream).Incremental {
		t.Fatal("reset connection retained incremental attribution")
	}
	if got := provider.Metadata(stream).Projection.FallbackReason; got != provider.ProjectionFallbackConnectionReset {
		t.Fatalf("connection fallback=%q", got)
	}
	if fullFallback == nil || fullFallback["previous_response_id"] != nil {
		t.Fatalf("full fallback body = %#v", fullFallback)
	}
	input, _ := fullFallback["input"].([]any)
	if len(input) != 3 {
		t.Fatalf("full fallback input = %#v", fullFallback["input"])
	}
}

func TestResponsesSessionMidStreamResetForcesNextCallToCompleteHTTP(
	t *testing.T,
) {
	var fullFallback map[string]any
	server := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			if request.Header.Get("Upgrade") == "" {
				if err := json.NewDecoder(request.Body).Decode(
					&fullFallback,
				); err != nil {
					t.Error(err)
				}
				writer.Header().Set(
					"Content-Type",
					"text/event-stream",
				)
				_, _ = io.WriteString(
					writer,
					"data: {\"type\":\"response.completed\","+
						"\"response\":{\"id\":\"http\","+
						"\"usage\":{\"input_tokens\":1,"+
						"\"output_tokens\":1}}}\n\n",
				)
				return
			}
			conn, err := websocket.Accept(writer, request, nil)
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.CloseNow()
			if _, _, err := conn.Read(request.Context()); err != nil {
				t.Error(err)
				return
			}
			writeResponseEvent(
				t,
				request.Context(),
				conn,
				map[string]any{
					"type":            "response.output_text.delta",
					"sequence_number": 0,
					"delta":           "partial",
				},
			)
		},
	))
	defer server.Close()

	client := testClient()
	first := incrementalRequest(t, server.URL)
	stream, err := client.Stream(t.Context(), first)
	if err != nil {
		t.Fatal(err)
	}
	events, err := provider.Drain(stream)
	if err == nil {
		t.Fatalf("mid-stream reset events=%+v error=nil", events)
	}
	if len(events) != 2 ||
		events[1].Type != provider.EventTextDelta ||
		events[1].Text != "partial" {
		t.Fatalf("confirmed events = %+v", events)
	}

	second := first
	second.Messages = append(
		second.Messages,
		provider.TextMessage(provider.RoleAssistant, "partial"),
		provider.TextMessage(provider.RoleUser, "continue"),
	)
	stream, err = client.Stream(t.Context(), second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Drain(stream); err != nil {
		t.Fatal(err)
	}
	metadata := provider.Metadata(stream)
	if metadata.Incremental ||
		metadata.Projection.Mode != provider.ProjectionModeFullHTTP ||
		metadata.Projection.FallbackReason !=
			provider.ProjectionFallbackConnectionReset {
		t.Fatalf("fallback metadata = %+v", metadata)
	}
	if fullFallback == nil ||
		fullFallback["previous_response_id"] != nil {
		t.Fatalf("full fallback body = %#v", fullFallback)
	}
}

func TestResponsesSessionReplayOutputMatchesLogicalInputEncoding(t *testing.T) {
	route := testRequest(
		t, "https://api.openai.test", model.ProtocolOpenAIResponses,
	).Route
	reasoning := json.RawMessage(`{"id":"rs_1","type":"reasoning","content":[{"type":"reasoning_text","text":"inspect"}]}`)
	output := []json.RawMessage{
		reasoning,
		json.RawMessage(`{"id":"fc_1","type":"function_call","call_id":"call_1","name":"read","arguments":"{\"path\":\"a.go\"}"}`),
		json.RawMessage(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}`),
	}
	logical, err := responsesInput([]provider.Message{
		provider.ProducedAssistant(
			route,
			[]provider.ContentBlock{
				{Type: provider.ContentReasoning, ID: "rs_1", Text: "inspect"},
				{Type: provider.ContentToolCall, ToolCall: &provider.ToolCall{
					ID: "call_1", Name: "read", Arguments: `{"path":"a.go"}`,
				}},
				{Type: provider.ContentText, Text: "done"},
			},
			1,
			mustReplayState(t, []json.RawMessage{reasoning}),
		),
	}, route, ResponsesPolicy{ReplayAdapter: model.AdapterOpenAI})
	if err != nil {
		t.Fatal(err)
	}
	replayed := replayOutput(output)
	if len(replayed) != len(logical) {
		t.Fatalf("replayed=%s logical=%#v", replayed, logical)
	}
	for index := range logical {
		want, err := json.Marshal(logical[index])
		if err != nil {
			t.Fatal(err)
		}
		if !jsonEqual(replayed[index], want) {
			t.Fatalf("item %d replayed=%s logical=%s", index, replayed[index], want)
		}
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

func TestResponsesSessionRejectsCompactionOrResumeRebase(t *testing.T) {
	prefix := []json.RawMessage{
		json.RawMessage(`{"role":"user","content":"old"}`),
		json.RawMessage(`{"role":"assistant","content":"answer"}`),
	}
	rebased := []json.RawMessage{
		json.RawMessage(`{"role":"system","content":"compacted summary"}`),
		json.RawMessage(`{"role":"user","content":"next"}`),
	}
	if strictExtension(prefix, rebased) {
		t.Fatal("compaction/resume rebase must force a complete request")
	}
}

func TestResponsesProjectionComparesEveryWireRequestProperty(t *testing.T) {
	adapter, err := NewAdapter(model.AdapterOpenAI)
	if err != nil {
		t.Fatal(err)
	}
	base := incrementalRequest(t, "https://api.openai.test")
	base.Projection = provider.ProjectionContext{
		ContextRevision: 3, WindowID: "window_1", WindowNumber: 1,
	}
	call, err := adapter.Prepare(base)
	if err != nil {
		t.Fatal(err)
	}
	baseline := call.Projection
	boolPointer := func(value bool) *bool { return &value }
	floatPointer := func(value float64) *float64 { return &value }
	tests := []struct {
		name   string
		mutate func(*provider.ModelRequest)
	}{
		{"max_output_tokens", func(value *provider.ModelRequest) { value.MaxOutputTokens++ }},
		{"temperature", func(value *provider.ModelRequest) { value.Temperature = floatPointer(0.2) }},
		{"reasoning_effort", func(value *provider.ModelRequest) { value.ReasoningEffort = "high" }},
		{"native_search", func(value *provider.ModelRequest) { value.NativeSearch = true }},
		{"tools", func(value *provider.ModelRequest) {
			value.Tools = []provider.ToolDefinition{{
				Name: "read", Description: "Read a file",
				InputSchema: map[string]any{"type": "object"},
			}}
		}},
		{"prompt_cache_key", func(value *provider.ModelRequest) { value.PromptCacheKey = "other" }},
		{"store", func(value *provider.ModelRequest) { value.Store = boolPointer(true) }},
		{"parallel_tools", func(value *provider.ModelRequest) { value.ParallelTools = boolPointer(false) }},
		{"include", func(value *provider.ModelRequest) { value.Include = []string{"reasoning.encrypted_content"} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := base
			test.mutate(&request)
			changed, err := adapter.Prepare(request)
			if err != nil {
				t.Fatal(err)
			}
			if changed.Projection.PropertyDigest == baseline.PropertyDigest {
				t.Fatalf(
					"property digest unchanged: baseline=%+v changed=%+v",
					baseline,
					changed.Projection,
				)
			}
		})
	}
	history := base
	history.Messages = append(
		history.Messages,
		provider.TextMessage(provider.RoleUser, "next"),
	)
	changed, err := adapter.Prepare(history)
	if err != nil {
		t.Fatal(err)
	}
	if changed.Projection.PropertyDigest != baseline.PropertyDigest ||
		changed.Projection.InputDigest == baseline.InputDigest {
		t.Fatalf(
			"history/property partition drift: baseline=%+v changed=%+v",
			baseline,
			changed.Projection,
		)
	}
}

func TestResponsesProjectionReportsEveryContinuityFallback(t *testing.T) {
	request := incrementalRequest(t, "https://api.openai.test")
	request.Projection = provider.ProjectionContext{
		ContextRevision: 4, WindowID: "window_1", WindowNumber: 1,
	}
	adapter, err := NewAdapter(model.AdapterOpenAI)
	if err != nil {
		t.Fatal(err)
	}
	call, err := adapter.Prepare(request)
	if err != nil {
		t.Fatal(err)
	}
	prefix := []json.RawMessage{json.RawMessage(`{"role":"user","content":"hello"}`)}
	extended := append(
		append([]json.RawMessage(nil), prefix...),
		json.RawMessage(`{"role":"user","content":"next"}`),
	)
	base := responsesSession{
		previous: "resp-1", property: call.Projection.PropertyDigest,
		routeDigest: call.Projection.RouteDigest,
		windowID:    request.Projection.WindowID,
		prefix:      prefix,
	}
	tests := []struct {
		name     string
		reason   provider.ProjectionFallbackReason
		mutate   func(*responsesSession, *provider.ModelRequest, *providerwire.PreparedCall)
		input    []json.RawMessage
		property string
	}{
		{
			name: "retry", reason: provider.ProjectionFallbackRetry,
			mutate: func(_ *responsesSession, request *provider.ModelRequest, _ *providerwire.PreparedCall) {
				request.Projection.Retry = true
			},
		},
		{
			name: "resume", reason: provider.ProjectionFallbackResume,
			mutate: func(_ *responsesSession, request *provider.ModelRequest, _ *providerwire.PreparedCall) {
				request.Projection.RecoveryID = "continue\x00turn_1"
			},
		},
		{
			name: "route", reason: provider.ProjectionFallbackRouteChanged,
			mutate: func(_ *responsesSession, _ *provider.ModelRequest, call *providerwire.PreparedCall) {
				call.Projection.RouteDigest = "changed"
			},
		},
		{
			name: "compaction", reason: provider.ProjectionFallbackCompaction,
			mutate: func(_ *responsesSession, request *provider.ModelRequest, _ *providerwire.PreparedCall) {
				request.Projection.WindowID = "window_2"
				request.Projection.WindowNumber = 2
			},
		},
		{
			name: "properties", reason: provider.ProjectionFallbackPropertyChanged,
			property: "changed",
		},
		{
			name: "history", reason: provider.ProjectionFallbackHistoryRebased,
			input: []json.RawMessage{json.RawMessage(`{"role":"user","content":"rebased"}`)},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session := responsesSession{
				previous:    base.previous,
				property:    base.property,
				routeDigest: base.routeDigest,
				windowID:    base.windowID,
				recoveryID:  base.recoveryID,
				prefix:      append([]json.RawMessage(nil), base.prefix...),
			}
			currentRequest, currentCall := request, call
			if test.mutate != nil {
				test.mutate(&session, &currentRequest, &currentCall)
			}
			input := extended
			if test.input != nil {
				input = test.input
			}
			property := call.Projection.PropertyDigest
			if test.property != "" {
				property = test.property
			}
			receipt := evaluateProjection(
				&session,
				currentRequest,
				currentCall,
				input,
				property,
			)
			if receipt.IncrementalEligible ||
				receipt.FallbackReason != test.reason {
				t.Fatalf("receipt=%+v", receipt)
			}
		})
	}
	eligible := evaluateProjection(
		&base,
		request,
		call,
		extended,
		call.Projection.PropertyDigest,
	)
	if !eligible.IncrementalEligible ||
		eligible.FallbackReason != "" ||
		eligible.StablePrefixDigest == "" {
		t.Fatalf("eligible receipt=%+v", eligible)
	}
}

func TestResponsesProjectionRetryOwnsForcedHTTPFallback(t *testing.T) {
	request := incrementalRequest(t, "https://api.openai.test")
	request.Projection.Retry = true
	adapter, err := NewAdapter(model.AdapterOpenAI)
	if err != nil {
		t.Fatal(err)
	}
	session := adapter.session(request)
	session.forceHTTP = true
	call, err := adapter.Prepare(request)
	if err != nil {
		t.Fatal(err)
	}
	if call.Projection.Mode != provider.ProjectionModeFullHTTP ||
		call.Projection.FallbackReason != provider.ProjectionFallbackRetry ||
		session.forceHTTP {
		t.Fatalf(
			"projection=%+v force_http=%t",
			call.Projection,
			session.forceHTTP,
		)
	}
}

func TestResponsesStablePrefixReuseAfterSampleThreeExceeds95Percent(t *testing.T) {
	request := incrementalRequest(t, "https://api.openai.test")
	request.Projection.WindowID = "window_1"
	adapter, err := NewAdapter(model.AdapterOpenAI)
	if err != nil {
		t.Fatal(err)
	}
	call, err := adapter.Prepare(request)
	if err != nil {
		t.Fatal(err)
	}
	prefix := make([]json.RawMessage, 100)
	for index := range prefix {
		encoded, err := json.Marshal(map[string]any{
			"type": "message", "role": "user", "content": index,
		})
		if err != nil {
			t.Fatal(err)
		}
		prefix[index] = encoded
	}
	logical := append(
		append([]json.RawMessage(nil), prefix...),
		json.RawMessage(`{"type":"message","role":"user","content":"delta"}`),
	)
	session := responsesSession{
		previous: "resp-3", property: call.Projection.PropertyDigest,
		routeDigest: call.Projection.RouteDigest,
		windowID:    request.Projection.WindowID,
		prefix:      prefix,
	}
	receipt := evaluateProjection(
		&session,
		request,
		call,
		logical,
		call.Projection.PropertyDigest,
	)
	transport, receipt, err := projectTransportInput(
		&session,
		logical,
		receipt,
	)
	if err != nil {
		t.Fatal(err)
	}
	reuseBasisPoints := (len(logical) - len(transport)) * 10_000 /
		len(logical)
	if !receipt.IncrementalEligible ||
		!receipt.LogicalTransportEquivalent ||
		reuseBasisPoints < 9_500 {
		t.Fatalf(
			"receipt=%+v logical=%d transport=%d reuse_bps=%d",
			receipt,
			len(logical),
			len(transport),
			reuseBasisPoints,
		)
	}
}

func incrementalRequest(t *testing.T, endpoint string) provider.ModelRequest {
	request := testRequest(t, endpoint, model.ProtocolOpenAIResponses)
	capabilities := request.Route.Model().Capabilities
	capabilities.IncrementalResponses = true
	capabilities.PromptCache = true
	request.Route = request.Route.WithCapabilities(capabilities)
	request.PromptCacheKey = "session-1"
	return request
}

func completedResponse(id, text string) map[string]any {
	return map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id": id,
			"output": []any{map[string]any{
				"type": "message", "role": "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": text}},
			}},
			"usage": map[string]any{"input_tokens": 1, "output_tokens": 1},
		},
	}
}

func writeResponseEvent(
	t *testing.T,
	ctx context.Context,
	conn *websocket.Conn,
	event map[string]any,
) {
	t.Helper()
	data, err := json.Marshal(event)
	if err != nil {
		t.Error(err)
		return
	}
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		t.Error(err)
	}
}

type adapterClient struct {
	*httpclient.Client
	adapter *Adapter
}

func testClient() *adapterClient {
	adapter, err := NewAdapter(model.AdapterOpenAI)
	if err != nil {
		panic(err)
	}
	client := httpclient.New()
	client.Credentials = staticCredentials("")
	client.Egress = egress.NewStaticGate()
	client.Egress.SetRuntimeApprover(func(context.Context, egress.Target) error { return nil })
	return &adapterClient{Client: client, adapter: adapter}
}

func (c *adapterClient) Stream(
	ctx context.Context,
	request provider.ModelRequest,
) (provider.Stream, error) {
	call, err := c.adapter.Prepare(request)
	if err != nil {
		return nil, err
	}
	stream, handled, err := c.adapter.TrySession(ctx, request, call, c.Client)
	if handled || err != nil {
		return stream, err
	}
	return c.Execute(ctx, request, call, c.adapter)
}

type staticCredentials string

func (c staticCredentials) Resolve(
	context.Context,
	model.CredentialRef,
) (string, error) {
	return string(c), nil
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
