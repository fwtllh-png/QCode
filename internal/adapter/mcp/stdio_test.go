package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestStdioConnectionRealFixtureLifecycle(t *testing.T) {
	binary := buildMCPFixture(t)
	transport, err := NewAuthorizedStdioTransport(
		context.Background(),
		"fixture",
		ServerConfig{
			Command: binary,
			Args:    []string{"--transport=stdio", "--stderr-bytes=65536"},
		},
		testRuntimeAuthority(t, t.TempDir()),
	)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := NewConnection("fixture", transport, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := connection.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	tools, err := connection.DiscoverTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 2 || tools[0].Name != "fixture.echo" {
		t.Fatalf("discovered tools = %+v", tools)
	}
	result, err := connection.CallTool(ctx, "fixture.echo", json.RawMessage(`{"value":"ok"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Content) != 1 || result.Content[0].Text != "fixture result" {
		t.Fatalf("tool result = %+v", result)
	}
	if len(transport.StderrTail()) > defaultStderrTailBytes {
		t.Fatalf("stderr tail bytes = %d", len(transport.StderrTail()))
	}

	callCtx, cancelCall := context.WithTimeout(ctx, 50*time.Millisecond)
	_, err = connection.CallTool(callCtx, "fixture.wait", json.RawMessage(`{}`))
	cancelCall()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait error = %v, want deadline exceeded", err)
	}
	if _, err := connection.CallTool(ctx, "fixture.echo", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("connection unusable after cancellation: %v", err)
	}
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer closeCancel()
	if err := connection.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
}

func TestStdioRejectsSecretEnvironment(t *testing.T) {
	_, err := NewAuthorizedStdioTransport(
		context.Background(),
		"fixture",
		ServerConfig{Command: "unused", Env: []string{"MCP_API_KEY=secret"}},
		testRuntimeAuthority(t, t.TempDir()),
	)
	if err == nil {
		t.Fatal("secret environment was accepted")
	}
}

func TestStdioFixtureContract(t *testing.T) {
	binary := buildMCPFixture(t)
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
	receive := func() fixtureRPCResponse {
		t.Helper()
		var response fixtureRPCResponse
		if err := decoder.Decode(&response); err != nil {
			t.Fatalf("decode response: %v; stderr=%s", err, stderr.String())
		}
		return response
	}

	send(fixtureRPCRequest(1, "initialize", map[string]any{}))
	initialize := receive()
	assertFixtureRPCSuccess(t, initialize, "1")
	assertFixtureCapabilities(t, initialize.Result)
	send(map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	})

	for id, method := range map[int]string{
		2: "tools/list",
		3: "resources/list",
		4: "prompts/list",
	} {
		send(fixtureRPCRequest(id, method, map[string]any{}))
		reply := receive()
		assertFixtureRPCSuccess(t, reply, fmt.Sprint(id))
		assertFixtureCollectionPresent(t, method, reply.Result)
	}

	send(fixtureRPCRequest(5, "tools/call", map[string]any{
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

	send(fixtureRPCRequest(6, "shutdown", map[string]any{}))
	assertFixtureRPCSuccess(t, receive(), "6")
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

func buildMCPFixture(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
	name := "mcp-fixture"
	binary := filepath.Join(t.TempDir(), name)
	command := exec.Command("go", "build", "-trimpath", "-o", binary, "./internal/adapter/mcp/testdata/fixture")
	command.Dir = root
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build fixture: %v\n%s", err, output)
	}
	return binary
}

type fixtureRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func fixtureRPCRequest(id int, method string, params any) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	}
}

func assertFixtureRPCSuccess(t *testing.T, response fixtureRPCResponse, id string) {
	t.Helper()
	if response.JSONRPC != "2.0" || string(response.ID) != id || response.Error != nil {
		t.Fatalf("RPC response = %+v, want successful id %s", response, id)
	}
}

func assertFixtureCapabilities(t *testing.T, raw json.RawMessage) {
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

func assertFixtureCollectionPresent(t *testing.T, method string, raw json.RawMessage) {
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
