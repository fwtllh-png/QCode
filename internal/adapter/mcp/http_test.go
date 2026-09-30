package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/common/tracecontext"
)

func TestStaleResponseRequiresStructuredCode(t *testing.T) {
	if staleResponse(Response{Error: &RPCError{
		Code: -32000, Message: "session expired",
	}}) {
		t.Fatal("error message must not drive stale-session replay")
	}
	if !staleResponse(Response{Error: &RPCError{
		Code: -32001, Message: "opaque",
	}}) {
		t.Fatal("structured stale-session code was ignored")
	}
}

func TestHTTPConnectionFixtureCapabilitiesAndStaleReconnect(t *testing.T) {
	url, command := startHTTPFixture(
		t,
		"--transport=http",
		"--post-sse",
		"--stale-once-method=tools/call",
	)
	config := fixtureHTTPConfig("http", url)
	pool := NewPool(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if changed, err := pool.Reload(ctx, config); err != nil || !changed {
		t.Fatalf("reload changed=%t err=%v", changed, err)
	}
	if len(pool.Catalog()) != 1 ||
		len(pool.ResourceCatalog()) != 1 ||
		len(pool.PromptCatalog()) != 1 {
		t.Fatalf(
			"catalog tools=%d resources=%d prompts=%d",
			len(pool.Catalog()),
			len(pool.ResourceCatalog()),
			len(pool.PromptCatalog()),
		)
	}
	connection, ok := pool.Connection("fixture")
	if !ok {
		t.Fatal("fixture connection missing")
	}
	discovery, err := connection.DiscoverAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(discovery.Tools) != 2 ||
		len(discovery.Resources) != 1 ||
		len(discovery.Prompts) != 1 {
		t.Fatalf("incomplete MCP discovery: %+v", discovery)
	}
	result, err := connection.CallTool(ctx, "fixture.echo", json.RawMessage(`{"ok":true}`))
	if !errors.Is(err, ErrServerUnavailable) {
		t.Fatalf("stale business call error = %v", err)
	}
	result, err = connection.CallTool(ctx, "fixture.echo", json.RawMessage(`{"ok":true}`))
	if err != nil {
		t.Fatalf("explicit call after reconnect: %v", err)
	}
	if len(result.Content) != 1 || result.Content[0].Text != "fixture result" {
		t.Fatalf("tool result = %+v", result)
	}
	callCtx, cancelCall := context.WithTimeout(ctx, 50*time.Millisecond)
	_, err = connection.CallTool(callCtx, "fixture.wait", json.RawMessage(`{}`))
	cancelCall()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled HTTP call error = %v", err)
	}
	if _, err := connection.CallTool(ctx, "fixture.echo", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("connection unusable after cancellation: %v", err)
	}
	resource, err := connection.ReadResource(ctx, "fixture://readme")
	if err != nil || len(resource.Contents) != 1 ||
		resource.Contents[0].Text != "fixture resource" {
		t.Fatalf("resource=%+v err=%v", resource, err)
	}
	prompt, err := connection.GetPrompt(ctx, "fixture.review", nil)
	if err != nil || len(prompt.Messages) != 1 {
		t.Fatalf("prompt=%+v err=%v", prompt, err)
	}
	if err := pool.ShutdownAll(ctx); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestLegacySSEFixtureCRLFFallback(t *testing.T) {
	url, command := startHTTPFixture(
		t,
		"--transport=legacy-sse",
		"--require-header=X-Fixture-Header=present",
		"--close-stream-once",
	)
	config := fixtureHTTPConfig("http", url)
	server := config.Servers["fixture"]
	server.Headers = map[string]string{"X-Fixture-Header": "present"}
	config.Servers["fixture"] = server
	pool := NewPool(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := pool.Reload(ctx, config); err != nil {
		t.Fatal(err)
	}
	connection, ok := pool.Connection("fixture")
	if !ok {
		t.Fatal("fixture connection missing")
	}
	result, err := connection.CallTool(ctx, "fixture.echo", json.RawMessage(`{}`))
	if err != nil || len(result.Content) != 1 {
		t.Fatalf("legacy result=%+v err=%v", result, err)
	}
	if err := pool.ShutdownAll(ctx); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPTransportPropagatesW3CTraceContext(t *testing.T) {
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
		defer request.Body.Close()
		var rpcRequest Request
		if err := json.NewDecoder(request.Body).Decode(&rpcRequest); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(Response{
			JSONRPC: JSONRPCVersion,
			ID:      rpcRequest.ID,
			Result: mustRawJSON(t, InitializeResult{
				ProtocolVersion: ProtocolVersion,
				Capabilities:    map[string]any{},
				ServerInfo:      ClientInfo{Name: "trace", Version: "1"},
			}),
		})
	}))
	defer server.Close()
	transport, err := NewHTTPTransport(t.Context(), ServerConfig{
		Transport: "http", URL: server.URL,
		ConnectTimeout: time.Second, ReadTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := tracecontext.NewRoot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want, _ := tracecontext.Current(ctx)
	var result InitializeResult
	if err := transport.Request(
		ctx,
		"initialize",
		InitializeParams{},
		&result,
	); err != nil {
		t.Fatal(err)
	}
	if propagated.TraceID != want.TraceID ||
		propagated.SpanID != want.SpanID {
		t.Fatalf("want=%+v propagated=%+v", want, propagated)
	}
}

func TestHTTPAuthPrecedence(t *testing.T) {
	t.Setenv("MCP_HEADER_AUTH", "Env env-token")
	t.Setenv("MCP_BEARER", "bearer-token")
	tests := []struct {
		name       string
		configure  func(*ServerConfig, *countingOAuth)
		want       string
		oauthCalls int64
	}{
		{
			name: "explicit authorization",
			configure: func(config *ServerConfig, _ *countingOAuth) {
				config.Headers = map[string]string{"Authorization": "Explicit direct-token"}
				config.HeaderEnv = map[string]string{"Authorization": "MCP_HEADER_AUTH"}
				config.BearerTokenEnv = "MCP_BEARER"
			},
			want: "Explicit direct-token",
		},
		{
			name: "environment-backed authorization",
			configure: func(config *ServerConfig, _ *countingOAuth) {
				config.HeaderEnv = map[string]string{"Authorization": "MCP_HEADER_AUTH"}
				config.BearerTokenEnv = "MCP_BEARER"
			},
			want: "Env env-token",
		},
		{
			name: "bearer environment",
			configure: func(config *ServerConfig, _ *countingOAuth) {
				config.BearerTokenEnv = "MCP_BEARER"
			},
			want: "Bearer bearer-token",
		},
		{
			name: "oauth",
			configure: func(config *ServerConfig, oauth *countingOAuth) {
				config.OAuthProvider = oauth
			},
			want:       "Bearer oauth-token",
			oauthCalls: 1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var authorization string
			server := httptest.NewServer(http.HandlerFunc(func(
				writer http.ResponseWriter,
				request *http.Request,
			) {
				authorization = request.Header.Get("Authorization")
				writer.Header().Set("Content-Type", "application/json")
				writer.Header().Set("Mcp-Session-Id", "auth-session")
				_ = json.NewEncoder(writer).Encode(Response{
					JSONRPC: JSONRPCVersion,
					ID:      json.RawMessage(`1`),
					Result: mustRawJSON(t, InitializeResult{
						ProtocolVersion: ProtocolVersion,
						Capabilities:    map[string]any{},
						ServerInfo:      ClientInfo{Name: "auth", Version: "1"},
					}),
				})
			}))
			defer server.Close()
			oauth := &countingOAuth{}
			config := ServerConfig{
				Transport:      "http",
				URL:            server.URL,
				ConnectTimeout: time.Second,
				ReadTimeout:    time.Second,
				MaxBodyBytes:   1 << 20,
				MaxChunkBytes:  1 << 16,
				InboundQueue:   8,
			}
			test.configure(&config, oauth)
			transport, err := NewHTTPTransport(context.Background(), config)
			if err != nil {
				t.Fatal(err)
			}
			var result InitializeResult
			if err := transport.Request(
				context.Background(),
				"initialize",
				InitializeParams{},
				&result,
			); err != nil {
				t.Fatal(err)
			}
			if authorization != test.want || oauth.calls.Load() != test.oauthCalls {
				t.Fatalf(
					"authorization=%q oauth calls=%d",
					authorization,
					oauth.calls.Load(),
				)
			}
		})
	}
}

func TestHTTPStaleSessionReconnectsWithoutReplayingBusinessCall(t *testing.T) {
	var mu sync.Mutex
	session := ""
	initializeCalls := 0
	toolCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		defer request.Body.Close()
		var rpcRequest Request
		if err := json.NewDecoder(request.Body).Decode(&rpcRequest); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if rpcRequest.Method != "initialize" &&
			request.Header.Get("Mcp-Session-Id") != session {
			t.Errorf("session header = %q, want %q", request.Header.Get("Mcp-Session-Id"), session)
		}
		reply := Response{JSONRPC: JSONRPCVersion, ID: rpcRequest.ID}
		switch rpcRequest.Method {
		case "initialize":
			initializeCalls++
			session = "session-" + time.Now().Format("150405.000000000")
			writer.Header().Set("Mcp-Session-Id", session)
			reply.Result = mustRawJSON(t, InitializeResult{
				ProtocolVersion: ProtocolVersion,
				Capabilities: map[string]any{
					"tools": map[string]any{},
				},
				ServerInfo: ClientInfo{Name: "stale", Version: "1"},
			})
		case "notifications/initialized":
			writer.WriteHeader(http.StatusAccepted)
			return
		case "tools/list":
			reply.Result = mustRawJSON(t, ListToolsResult{Tools: []Tool{{
				Name: "echo", InputSchema: map[string]any{"type": "object"},
			}}})
		case "tools/call":
			toolCalls++
			if toolCalls == 1 {
				writer.WriteHeader(http.StatusNotFound)
				return
			}
			reply.Result = mustRawJSON(t, CallToolResult{
				Content: []Content{{Type: "text", Text: "ok"}},
			})
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(reply)
	}))
	defer server.Close()
	config := fixtureHTTPConfig("http", server.URL).Servers["fixture"]
	transport, err := NewHTTPTransport(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := NewConnection("stale", transport, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := connection.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.DiscoverTools(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.CallTool(
		ctx, "echo", json.RawMessage(`{}`),
	); !errors.Is(err, ErrServerUnavailable) {
		t.Fatalf("first call error = %v", err)
	}
	mu.Lock()
	if initializeCalls != 2 || toolCalls != 1 {
		t.Fatalf("initialize calls=%d tool calls=%d", initializeCalls, toolCalls)
	}
	mu.Unlock()
	if _, err := connection.CallTool(ctx, "echo", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if toolCalls != 2 {
		t.Fatalf("explicit business calls=%d, want 2", toolCalls)
	}
}

func TestReadSSEResponseDispatchesCatalogNotification(t *testing.T) {
	stream := strings.NewReader(
		"event: message\n" +
			"data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/tools/list_changed\"}\n\n" +
			"event: message\n" +
			"data: {\"jsonrpc\":\"2.0\",\"id\":7,\"result\":{\"tools\":[]}}\n\n",
	)
	var notifications []Notification
	response, err := readSSEResponse(
		stream, json.RawMessage("7"), 4096, 2048,
		func(notification Notification) {
			notifications = append(notifications, notification)
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(notifications) != 1 ||
		notifications[0].Method != "notifications/tools/list_changed" {
		t.Fatalf("notifications = %+v", notifications)
	}
	if string(response.ID) != "7" {
		t.Fatalf("response ID = %s", response.ID)
	}
}

func TestHTTPSSEFixtureContract(t *testing.T) {
	binary := buildMCPFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	command := exec.CommandContext(ctx, binary, "--transport=http", "--listen=127.0.0.1:0")
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	var ready struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(stdout).Decode(&ready); err != nil {
		t.Fatalf("decode readiness: %v; stderr=%s", err, stderr.String())
	}
	if !strings.HasPrefix(ready.URL, "http://127.0.0.1:") {
		t.Fatalf("unexpected fixture URL %q", ready.URL)
	}

	client := &http.Client{Timeout: 3 * time.Second}
	initializeHTTP, initialize := postFixtureRPC(t, client, ready.URL, "", fixtureRPCRequest(
		1,
		"initialize",
		map[string]any{},
	))
	session := initializeHTTP.Header.Get("Mcp-Session-Id")
	initializeHTTP.Body.Close()
	if session == "" {
		t.Fatal("initialize response omitted Mcp-Session-Id")
	}
	assertFixtureRPCSuccess(t, initialize, "1")
	assertFixtureCapabilities(t, initialize.Result)

	for id, method := range map[int]string{
		2: "tools/list",
		3: "resources/list",
		4: "prompts/list",
	} {
		httpResponse, reply := postFixtureRPC(
			t,
			client,
			ready.URL,
			session,
			fixtureRPCRequest(id, method, map[string]any{}),
		)
		httpResponse.Body.Close()
		assertFixtureRPCSuccess(t, reply, fmt.Sprint(id))
		assertFixtureCollectionPresent(t, method, reply.Result)
	}

	sseRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, ready.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	sseRequest.Header.Set("Accept", "text/event-stream")
	sseRequest.Header.Set("Mcp-Session-Id", session)
	sse, err := client.Do(sseRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer sse.Body.Close()
	sseReader := bufio.NewReader(sse.Body)
	eventLine, err := sseReader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	dataLine, err := sseReader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if eventLine != "event: fixture.ready\n" || !strings.Contains(dataLine, session) {
		t.Fatalf("unexpected SSE prelude %q %q", eventLine, dataLine)
	}

	staleHTTP, stale := postFixtureRPC(
		t,
		client,
		ready.URL,
		"stale-session",
		fixtureRPCRequest(5, "tools/list", map[string]any{}),
	)
	staleHTTP.Body.Close()
	if staleHTTP.StatusCode != http.StatusNotFound ||
		stale.Error == nil ||
		stale.Error.Code != -32001 {
		t.Fatalf("stale session status=%d response=%+v", staleHTTP.StatusCode, stale)
	}

	cancelResult := make(chan fixtureRPCResponse, 1)
	cancelError := make(chan error, 1)
	go func() {
		response, reply, err := postFixtureRPCResult(
			client,
			ready.URL,
			session,
			fixtureRPCRequest(6, "tools/call", map[string]any{
				"name":      "fixture.wait",
				"arguments": map[string]any{},
			}),
		)
		if response != nil {
			response.Body.Close()
		}
		if err != nil {
			cancelError <- err
			return
		}
		cancelResult <- reply
	}()
	cancelHTTP, _ := postFixtureRPC(t, client, ready.URL, session, map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/cancelled",
		"params":  map[string]any{"requestId": 6},
	})
	cancelHTTP.Body.Close()

	select {
	case err := <-cancelError:
		t.Fatal(err)
	case cancelled := <-cancelResult:
		if string(cancelled.ID) != "6" || cancelled.Error == nil || cancelled.Error.Code != -32800 {
			t.Fatalf("cancelled response = %+v", cancelled)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}

	shutdownHTTP, shutdown := postFixtureRPC(
		t,
		client,
		ready.URL,
		session,
		fixtureRPCRequest(7, "shutdown", map[string]any{}),
	)
	shutdownHTTP.Body.Close()
	assertFixtureRPCSuccess(t, shutdown, "7")
	sse.Body.Close()
	if err := command.Wait(); err != nil {
		t.Fatalf("fixture shutdown: %v; stderr=%s", err, stderr.String())
	}
	if ctx.Err() != nil {
		t.Fatalf("fixture exceeded deadline: %v", ctx.Err())
	}
}

type countingOAuth struct {
	calls atomic.Int64
}

func (o *countingOAuth) Authorization(context.Context) (string, error) {
	o.calls.Add(1)
	return "Bearer oauth-token", nil
}

func fixtureHTTPConfig(transport, url string) Config {
	return Config{
		Version: ConfigVersion,
		Servers: map[string]ServerConfig{
			"fixture": {
				Transport: transport,
				URL:       url,
				Tools: map[string]ToolBinding{
					"fixture.echo": {
						Capability:         "read",
						AccessMode:         "read",
						ParallelPolicy:     "concurrent",
						SandboxRequirement: "none",
					},
				},
				Resources: []string{"fixture://readme"},
				Prompts:   []string{"fixture.review"},
			},
		},
	}
}

func startHTTPFixture(t *testing.T, arguments ...string) (string, *exec.Cmd) {
	t.Helper()
	binary := buildMCPFixture(t)
	command := exec.Command(binary, append(arguments, "--listen=127.0.0.1:0")...)
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if command.ProcessState == nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	})
	var ready struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(output).Decode(&ready); err != nil {
		t.Fatal(err)
	}
	return ready.URL, command
}

func mustRawJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func postFixtureRPC(
	t *testing.T,
	client *http.Client,
	url string,
	session string,
	value any,
) (*http.Response, fixtureRPCResponse) {
	t.Helper()
	httpResponse, reply, err := postFixtureRPCResult(client, url, session, value)
	if err != nil {
		t.Fatal(err)
	}
	return httpResponse, reply
}

func postFixtureRPCResult(
	client *http.Client,
	url string,
	session string,
	value any,
) (*http.Response, fixtureRPCResponse, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, fixtureRPCResponse{}, err
	}
	request, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fixtureRPCResponse{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	if session != "" {
		request.Header.Set("Mcp-Session-Id", session)
	}
	httpResponse, err := client.Do(request)
	if err != nil {
		return nil, fixtureRPCResponse{}, err
	}
	defer func() {
		if httpResponse.StatusCode == http.StatusAccepted {
			io.Copy(io.Discard, httpResponse.Body)
		}
	}()
	if httpResponse.StatusCode == http.StatusAccepted {
		return httpResponse, fixtureRPCResponse{}, nil
	}
	var reply fixtureRPCResponse
	if err := json.NewDecoder(httpResponse.Body).Decode(&reply); err != nil {
		httpResponse.Body.Close()
		return nil, fixtureRPCResponse{}, err
	}
	return httpResponse, reply, nil
}
