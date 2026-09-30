package httpclient

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
)

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
