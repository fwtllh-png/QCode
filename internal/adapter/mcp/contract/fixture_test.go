package contract_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	mcpruntime "github.com/fwtllh-png/QCode/internal/adapter/mcp"
)

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func TestStdioFixtureContract(t *testing.T) {
	binary := buildFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	command := exec.CommandContext(ctx, binary, "--transport=stdio")
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	encoder := json.NewEncoder(stdin)
	decoder := json.NewDecoder(stdout)

	send := func(value any) {
		t.Helper()
		if err := encoder.Encode(value); err != nil {
			t.Fatal(err)
		}
	}
	receive := func() rpcResponse {
		t.Helper()
		var response rpcResponse
		if err := decoder.Decode(&response); err != nil {
			t.Fatalf("decode response: %v; stderr=%s", err, stderr.String())
		}
		return response
	}

	send(rpcRequest(1, "initialize", map[string]any{}))
	initialize := receive()
	assertRPCSuccess(t, initialize, "1")
	assertCapabilities(t, initialize.Result)
	send(map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	})

	for id, method := range map[int]string{
		2: "tools/list",
		3: "resources/list",
		4: "prompts/list",
	} {
		send(rpcRequest(id, method, map[string]any{}))
		reply := receive()
		assertRPCSuccess(t, reply, fmt.Sprint(id))
		assertCollectionPresent(t, method, reply.Result)
	}

	send(rpcRequest(5, "tools/call", map[string]any{
		"name":      "fixture.wait",
		"arguments": map[string]any{},
	}))
	send(map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/cancelled",
		"params":  map[string]any{"requestId": 5},
	})
	cancelled := receive()
	if string(cancelled.ID) != "5" || cancelled.Error == nil || cancelled.Error.Code != -32800 {
		t.Fatalf("cancelled response = %+v", cancelled)
	}

	send(rpcRequest(6, "shutdown", map[string]any{}))
	assertRPCSuccess(t, receive(), "6")
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("fixture shutdown: %v; stderr=%s", err, stderr.String())
	}
	if ctx.Err() != nil {
		t.Fatalf("fixture exceeded deadline: %v", ctx.Err())
	}
}

func TestHTTPSSEFixtureContract(t *testing.T) {
	binary := buildFixture(t)
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
	initializeHTTP, initialize := postRPC(t, client, ready.URL, "", rpcRequest(
		1,
		"initialize",
		map[string]any{},
	))
	session := initializeHTTP.Header.Get("Mcp-Session-Id")
	initializeHTTP.Body.Close()
	if session == "" {
		t.Fatal("initialize response omitted Mcp-Session-Id")
	}
	assertRPCSuccess(t, initialize, "1")
	assertCapabilities(t, initialize.Result)

	for id, method := range map[int]string{
		2: "tools/list",
		3: "resources/list",
		4: "prompts/list",
	} {
		httpResponse, reply := postRPC(
			t,
			client,
			ready.URL,
			session,
			rpcRequest(id, method, map[string]any{}),
		)
		httpResponse.Body.Close()
		assertRPCSuccess(t, reply, fmt.Sprint(id))
		assertCollectionPresent(t, method, reply.Result)
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

	staleHTTP, stale := postRPC(
		t,
		client,
		ready.URL,
		"stale-session",
		rpcRequest(5, "tools/list", map[string]any{}),
	)
	staleHTTP.Body.Close()
	if staleHTTP.StatusCode != http.StatusNotFound ||
		stale.Error == nil ||
		stale.Error.Code != -32001 {
		t.Fatalf("stale session status=%d response=%+v", staleHTTP.StatusCode, stale)
	}

	cancelResult := make(chan rpcResponse, 1)
	cancelError := make(chan error, 1)
	go func() {
		response, reply, err := postRPCResult(
			client,
			ready.URL,
			session,
			rpcRequest(6, "tools/call", map[string]any{
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
	cancelHTTP, _ := postRPC(t, client, ready.URL, session, map[string]any{
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

	shutdownHTTP, shutdown := postRPC(
		t,
		client,
		ready.URL,
		session,
		rpcRequest(7, "shutdown", map[string]any{}),
	)
	shutdownHTTP.Body.Close()
	assertRPCSuccess(t, shutdown, "7")
	sse.Body.Close()
	if err := command.Wait(); err != nil {
		t.Fatalf("fixture shutdown: %v; stderr=%s", err, stderr.String())
	}
	if ctx.Err() != nil {
		t.Fatalf("fixture exceeded deadline: %v", ctx.Err())
	}
}

func TestMCPHTTPImplementationContract(t *testing.T) {
	if os.Getenv("QCODE_REQUIRE_P6_IMPLEMENTATION") != "1" {
		t.Skip("implementation gate is enabled only by P6 Make targets")
	}
	binary := buildFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(
		ctx,
		binary,
		"--transport=http",
		"--listen=127.0.0.1:0",
		"--post-sse",
		"--stale-once-method=tools/call",
	)
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	var ready struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(output).Decode(&ready); err != nil {
		t.Fatal(err)
	}
	transport, err := mcpruntime.NewHTTPTransport(ctx, mcpruntime.ServerConfig{
		Transport: "http",
		URL:       ready.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	connection, err := mcpruntime.NewConnection("contract", transport, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Initialize(ctx); err != nil {
		t.Fatal(err)
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
	if _, err := connection.CallTool(
		ctx,
		"fixture.echo",
		json.RawMessage(`{"contract":true}`),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.ReadResource(ctx, "fixture://readme"); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.GetPrompt(ctx, "fixture.review", nil); err != nil {
		t.Fatal(err)
	}
	if err := connection.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}
}

func buildFixture(t *testing.T) string {
	t.Helper()
	root := repositoryRoot(t)
	name := "mcp-fixture"
	binary := filepath.Join(t.TempDir(), name)
	command := exec.Command(
		"go",
		"build",
		"-trimpath",
		"-o",
		binary,
		"./internal/adapter/mcp/testdata/fixture",
	)
	command.Dir = root
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build fixture: %v\n%s", err, output)
	}
	return binary
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", ".."))
}

func rpcRequest(id int, method string, params any) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	}
}

func assertRPCSuccess(t *testing.T, response rpcResponse, id string) {
	t.Helper()
	if response.JSONRPC != "2.0" || string(response.ID) != id || response.Error != nil {
		t.Fatalf("RPC response = %+v, want successful id %s", response, id)
	}
}

func assertCapabilities(t *testing.T, raw json.RawMessage) {
	t.Helper()
	var result struct {
		Capabilities map[string]json.RawMessage `json:"capabilities"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"tools", "resources", "prompts"} {
		if _, ok := result.Capabilities[name]; !ok {
			t.Errorf("initialize omitted %s capability", name)
		}
	}
}

func assertCollectionPresent(t *testing.T, method string, raw json.RawMessage) {
	t.Helper()
	key := strings.TrimSuffix(method, "/list")
	var result map[string]json.RawMessage
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if _, ok := result[key]; !ok {
		t.Errorf("%s response omitted %q collection", method, key)
	}
}

func postRPC(
	t *testing.T,
	client *http.Client,
	url string,
	session string,
	value any,
) (*http.Response, rpcResponse) {
	t.Helper()
	httpResponse, reply, err := postRPCResult(client, url, session, value)
	if err != nil {
		t.Fatal(err)
	}
	return httpResponse, reply
}

func postRPCResult(
	client *http.Client,
	url string,
	session string,
	value any,
) (*http.Response, rpcResponse, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, rpcResponse{}, err
	}
	request, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, rpcResponse{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	if session != "" {
		request.Header.Set("Mcp-Session-Id", session)
	}
	httpResponse, err := client.Do(request)
	if err != nil {
		return nil, rpcResponse{}, err
	}
	defer func() {
		if httpResponse.StatusCode == http.StatusAccepted {
			io.Copy(io.Discard, httpResponse.Body)
		}
	}()
	if httpResponse.StatusCode == http.StatusAccepted {
		return httpResponse, rpcResponse{}, nil
	}
	var reply rpcResponse
	if err := json.NewDecoder(httpResponse.Body).Decode(&reply); err != nil {
		httpResponse.Body.Close()
		return nil, rpcResponse{}, err
	}
	return httpResponse, reply, nil
}
