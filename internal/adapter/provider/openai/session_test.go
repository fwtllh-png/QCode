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
