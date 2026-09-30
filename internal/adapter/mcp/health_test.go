package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type healthFixtureTransport struct {
	calls     atomic.Int64
	pings     atomic.Int64
	failCalls atomic.Bool
	rpcError  atomic.Bool
	isError   atomic.Bool
	toolName  string
	blockCall atomic.Bool
}

func (t *healthFixtureTransport) Request(
	ctx context.Context,
	method string,
	_ any,
	target any,
) error {
	switch method {
	case "initialize":
		*target.(*InitializeResult) = InitializeResult{
			ProtocolVersion: ProtocolVersion,
			Capabilities:    map[string]any{"tools": map[string]any{}},
			ServerInfo:      ClientInfo{Name: "fixture", Version: "1"},
		}
	case "tools/list":
		*target.(*ListToolsResult) = ListToolsResult{Tools: []Tool{{
			Name: t.toolName, InputSchema: map[string]any{"type": "object"},
		}}}
	case "tools/call":
		t.calls.Add(1)
		if t.blockCall.Load() {
			<-ctx.Done()
			return ctx.Err()
		}
		if t.failCalls.Load() {
			return errors.New("fixture transport failed")
		}
		if t.rpcError.Load() {
			return &RPCError{Code: -32000, Message: "fixture RPC failure"}
		}
		*target.(*CallToolResult) = CallToolResult{
			Content: []Content{{Type: "text", Text: "ok"}},
			IsError: t.isError.Load(),
		}
	case "ping":
		t.pings.Add(1)
	}
	return nil
}

func TestCircuitBreakerCountsTimeoutAndCanFlap(t *testing.T) {
	transport := &healthFixtureTransport{toolName: "echo"}
	pool := NewPool(func(context.Context, string, ServerConfig) (Transport, error) {
		return transport, nil
	})
	server := fixtureServerConfig("echo")
	server.CallTimeout = 2 * time.Millisecond
	server.CircuitBreaker = CircuitBreakerConfig{
		FailureThreshold: 1, Cooldown: time.Millisecond,
	}
	if _, err := pool.Reload(t.Context(), Config{
		Version: ConfigVersion, Servers: map[string]ServerConfig{"remote": server},
	}); err != nil {
		t.Fatal(err)
	}
	connection, _ := pool.Connection("remote")
	transport.blockCall.Store(true)
	if _, err := connection.CallTool(t.Context(), "echo", json.RawMessage(`{}`)); err == nil {
		t.Fatal("timed out call succeeded")
	}
	if got := healthByServer(pool.HealthSnapshots())["remote"].State; got != HealthOpen {
		t.Fatalf("state = %q, want open", got)
	}
	time.Sleep(2 * time.Millisecond)
	transport.blockCall.Store(false)
	if err := pool.ProbeOpen(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := healthByServer(pool.HealthSnapshots())["remote"].State; got != HealthHealthy {
		t.Fatalf("state = %q, want healthy", got)
	}
	transport.rpcError.Store(true)
	if _, err := connection.CallTool(t.Context(), "echo", json.RawMessage(`{}`)); err == nil {
		t.Fatal("protocol failure succeeded")
	}
	if got := healthByServer(pool.HealthSnapshots())["remote"].State; got != HealthOpen {
		t.Fatalf("state after flap = %q, want open", got)
	}
}

func (*healthFixtureTransport) Notify(context.Context, string, any) error { return nil }
func (*healthFixtureTransport) Close(context.Context) error               { return nil }
func (*healthFixtureTransport) StderrTail() string                        { return "" }

func TestCircuitBreakerProbesWithoutReplayingBusinessCall(t *testing.T) {
	transport := &healthFixtureTransport{toolName: "echo"}
	transport.failCalls.Store(true)
	pool := NewPool(func(context.Context, string, ServerConfig) (Transport, error) {
		return transport, nil
	})
	server := fixtureServerConfig("echo")
	server.CircuitBreaker = CircuitBreakerConfig{
		FailureThreshold: 2, Cooldown: 5 * time.Millisecond,
	}
	config := Config{
		Version: ConfigVersion, Servers: map[string]ServerConfig{"remote": server},
	}
	if _, err := pool.Reload(t.Context(), config); err != nil {
		t.Fatal(err)
	}
	connection, ok := pool.Connection("remote")
	if !ok {
		t.Fatal("connection is missing")
	}
	for range 2 {
		if _, err := connection.CallTool(t.Context(), "echo", json.RawMessage(`{}`)); err == nil {
			t.Fatal("failing business call succeeded")
		}
	}
	if _, err := connection.CallTool(
		t.Context(), "echo", json.RawMessage(`{}`),
	); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("open call error = %v", err)
	}
	if transport.calls.Load() != 2 {
		t.Fatalf("business calls = %d, want exactly 2", transport.calls.Load())
	}
	time.Sleep(10 * time.Millisecond)
	transport.failCalls.Store(false)
	if err := pool.ProbeOpen(t.Context()); err != nil {
		t.Fatal(err)
	}
	if transport.pings.Load() != 1 {
		t.Fatalf("pings = %d, want 1", transport.pings.Load())
	}
	if _, err := connection.CallTool(t.Context(), "echo", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if transport.calls.Load() != 3 {
		t.Fatalf("business calls = %d, want 3", transport.calls.Load())
	}
}

func TestMCPToolIsErrorDoesNotTripBreaker(t *testing.T) {
	transport := &healthFixtureTransport{toolName: "echo"}
	transport.isError.Store(true)
	pool := NewPool(func(context.Context, string, ServerConfig) (Transport, error) {
		return transport, nil
	})
	server := fixtureServerConfig("echo")
	server.CircuitBreaker.FailureThreshold = 1
	if _, err := pool.Reload(t.Context(), Config{
		Version: ConfigVersion, Servers: map[string]ServerConfig{"remote": server},
	}); err != nil {
		t.Fatal(err)
	}
	connection, _ := pool.Connection("remote")
	result, err := connection.CallTool(t.Context(), "echo", json.RawMessage(`{}`))
	if err != nil || !result.IsError {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	health := healthByServer(pool.HealthSnapshots())
	if health["remote"].State != HealthHealthy ||
		health["remote"].ConsecutiveFailures != 0 {
		t.Fatalf("health = %+v", health["remote"])
	}
}

func healthByServer(snapshots []HealthSnapshot) map[string]HealthSnapshot {
	result := make(map[string]HealthSnapshot, len(snapshots))
	for _, snapshot := range snapshots {
		result[snapshot.Server] = snapshot
	}
	return result
}
