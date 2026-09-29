package openai

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
)

func TestResponsesSessionIdleTimeoutRetiresTheInFlightConnection(t *testing.T) {
	var connections atomic.Int32
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		conn, err := websocket.Accept(writer, request, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		ctx := request.Context()
		send := func(event map[string]any) {
			data, _ := json.Marshal(event)
			_ = conn.Write(ctx, websocket.MessageText, data)
		}
		if _, _, err := conn.Read(ctx); err != nil {
			return
		}
		if connections.Add(1) == 1 {
			send(map[string]any{"type": "response.output_text.delta", "delta": "first"})
			select {
			case <-release:
			case <-ctx.Done():
				return
			}
			// The stalled response finishes after the client gave up on it.
			send(map[string]any{"type": "response.output_text.delta", "delta": "late"})
			send(completedResponse("resp-1", "first late"))
			if _, _, err := conn.Read(ctx); err != nil {
				return
			}
		}
		send(map[string]any{"type": "response.output_text.delta", "delta": "second"})
		send(completedResponse("resp-2", "second"))
	}))
	defer server.Close()

	client := testClient()
	client.IdleTimeout = 200 * time.Millisecond
	first := incrementalRequest(t, server.URL)
	stream, err := client.Stream(t.Context(), first)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Drain(stream); err == nil {
		t.Fatal("stalled stream did not hit the idle timeout")
	}
	close(release)

	stream, err = client.Stream(t.Context(), first)
	if err != nil {
		t.Fatal(err)
	}
	events, err := provider.Drain(stream)
	if err != nil {
		t.Fatal(err)
	}
	text := ""
	for _, event := range events {
		if event.Type == provider.EventTextDelta {
			text += event.Text
		}
	}
	if text != "second" {
		t.Fatalf("retry text = %q, want only the retried response", text)
	}
	if got := connections.Load(); got != 2 {
		t.Fatalf("connections = %d, want the timed-out connection retired", got)
	}
}
