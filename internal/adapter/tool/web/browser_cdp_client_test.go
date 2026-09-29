package web

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

type cdpTestWriter struct{ bytes.Buffer }

func (*cdpTestWriter) Close() error { return nil }

func TestCDPProxyCredentialsStayOnTheOwnedProxy(t *testing.T) {
	for _, tc := range []struct {
		name, source, origin, realm string
		allow                       bool
	}{
		{"owned proxy", "Proxy", "http://127.0.0.1:43128", "qcode-process-proxy", true},
		{"origin server", "Server", "http://127.0.0.1:43128", "qcode-process-proxy", false},
		{"foreign proxy", "Proxy", "http://127.0.0.1:43129", "qcode-process-proxy", false},
		{"foreign realm", "Proxy", "http://127.0.0.1:43128", "other", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writer := &cdpTestWriter{}
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			client := &cdpClient{writer: writer, ctx: ctx, cancel: cancel,
				pending: make(map[int64]chan cdpMessage), proxyPort: 43128,
				credential: "fixture-proxy-password", challenges: make(map[string]bool)}
			params, err := json.Marshal(map[string]any{"requestId": "request", "authChallenge": map[string]string{
				"source": tc.source, "origin": tc.origin, "scheme": "basic", "realm": tc.realm,
			}})
			if err != nil {
				t.Fatal(err)
			}
			event := cdpMessage{SessionID: "page", Method: "Fetch.authRequired", Params: params}
			if err := client.handleEvent(event); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(writer.String(), "fixture-proxy-password") != tc.allow {
				t.Fatal("proxy credentials were disclosed to the wrong challenge")
			}
			writer.Reset()
			if err := client.handleEvent(event); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(writer.String(), "fixture-proxy-password") || !strings.Contains(writer.String(), "CancelAuth") {
				t.Fatal("repeated challenge was not canceled")
			}
		})
	}
}
